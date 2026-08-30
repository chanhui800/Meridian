package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

const maxAgentEventBodyBytes = 32 << 10

// Keep the complete report comfortably below the controller request limit
// when several metadata events are acknowledged in one heartbeat.
const maxAgentEventResponseBodyBytes = 16 << 10
const maxAgentEventsPerReport = 16

type state struct {
	NodeGUID string `json:"node_guid"`
	Token    string `json:"agent_token"`
}

type report struct {
	BootID            string         `json:"boot_id"`
	Sequence          int64          `json:"sequence"`
	InterfaceName     string         `json:"interface_name"`
	RXBytes           int64          `json:"rx_bytes"`
	TXBytes           int64          `json:"tx_bytes"`
	AgentVersion      string         `json:"agent_version"`
	AppliedConfigHash string         `json:"applied_config_hash"`
	ListenerError     string         `json:"listener_error"`
	SiteStats         []siteStat     `json:"site_stats,omitempty"`
	Events            []requestEvent `json:"events,omitempty"`
}

type requestEvent struct {
	EventID                 int64  `json:"event_id"`
	SiteID                  int64  `json:"site_id"`
	Host                    string `json:"host"`
	Method                  string `json:"method"`
	Path                    string `json:"path"`
	Query                   string `json:"query,omitempty"`
	StatusCode              int    `json:"status_code"`
	ClientIP                string `json:"client_ip,omitempty"`
	UserAgent               string `json:"user_agent,omitempty"`
	Authorization           string `json:"authorization,omitempty"`
	Body                    string `json:"body,omitempty"`
	ContentType             string `json:"content_type,omitempty"`
	ContentEncoding         string `json:"content_encoding,omitempty"`
	ResponseBody            string `json:"response_body,omitempty"`
	ResponseContentType     string `json:"response_content_type,omitempty"`
	ResponseContentEncoding string `json:"response_content_encoding,omitempty"`
	RecordedAtMS            int64  `json:"recorded_at_ms"`
}

type siteStat struct {
	Host            string `json:"host"`
	RequestCount    int64  `json:"request_count"`
	LastRequestAtMS int64  `json:"last_request_at_ms"`
	LastStatus      int    `json:"last_status"`
}

type siteStatValue struct{ siteStat }

type siteStatsStore struct {
	mu     sync.Mutex
	items  map[string]siteStatValue
	events *requestEventStore
}

type requestEventStore struct {
	mu    sync.Mutex
	next  int64
	items []requestEvent
}

func (s *requestEventStore) add(event requestEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	event.EventID = s.next
	s.items = append(s.items, event)
	if len(s.items) > 128 {
		s.items = s.items[len(s.items)-128:]
	}
}
func (s *requestEventStore) snapshot() []requestEvent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if len(s.items) > maxAgentEventsPerReport {
		start = len(s.items) - maxAgentEventsPerReport
	}
	return append([]requestEvent(nil), s.items[start:]...)
}
func (s *requestEventStore) ack(events []requestEvent) {
	if s == nil || len(events) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	max := events[len(events)-1].EventID
	kept := s.items[:0]
	for _, item := range s.items {
		if item.EventID > max {
			kept = append(kept, item)
		}
	}
	s.items = kept
}

func (s *siteStatsStore) record(host string, status int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]siteStatValue)
	}
	value := s.items[host]
	value.Host = host
	value.RequestCount++
	value.LastRequestAtMS = time.Now().UnixMilli()
	value.LastStatus = status
	s.items[host] = value
}

func (s *siteStatsStore) snapshot() []siteStat {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]siteStat, 0, len(s.items))
	for _, value := range s.items {
		values = append(values, value.siteStat)
	}
	return values
}

type siteRoute struct {
	SiteID            int64               `json:"site_id"`
	Host              string              `json:"host"`
	TargetURL         string              `json:"target_url"`
	PlaybackTargetURL string              `json:"playback_target_url,omitempty"`
	StreamHosts       []string            `json:"stream_hosts,omitempty"`
	PlaybackMode      string              `json:"playback_mode,omitempty"`
	Headers           map[string][]string `json:"headers,omitempty"`
}

type runtimeConfig struct {
	SchemaVersion  int         `json:"schema_version"`
	ConfigHash     string      `json:"config_hash"`
	NodeGUID       string      `json:"node_guid"`
	EntryMode      string      `json:"entry_mode"`
	HTTPPort       int         `json:"http_port"`
	HTTPSPort      int         `json:"https_port"`
	CertificatePEM string      `json:"certificate_pem,omitempty"`
	PrivateKeyPEM  string      `json:"private_key_pem,omitempty"`
	Routes         []siteRoute `json:"routes"`
}

type agentRuntime struct {
	mu            sync.RWMutex
	stateDir      string
	nodeGUID      string
	port          int
	handler       http.Handler
	certificate   *tls.Certificate
	servers       []*http.Server
	appliedHash   string
	listenerError string
	stats         *siteStatsStore
	events        *requestEventStore
}

func normalizeController(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("controller must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	hostIP := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" && (hostIP == nil || !hostIP.IsLoopback()) {
		return "", errors.New("public controller must use HTTPS")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func loadState(path string) (state, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- administrator-selected local Agent state path.
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	var value state
	if err := json.Unmarshal(data, &value); err != nil {
		return state{}, fmt.Errorf("decode state: %w", err)
	}
	if value.NodeGUID == "" || value.Token == "" {
		return state{}, errors.New("agent state is incomplete")
	}
	return value, nil
}

func saveState(path string, value state) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".meridian-agent-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func request(ctx context.Context, client *http.Client, method, endpoint, token string, body, output any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 1<<20)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(limited).Decode(&failure)
		if failure.Error == "" {
			failure.Error = response.Status
		}
		return errors.New(failure.Error)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, limited)
		return err
	}
	return json.NewDecoder(limited).Decode(output)
}

func enroll(ctx context.Context, client *http.Client, controller, tokenFile, statePath string) (state, error) {
	data, err := os.ReadFile(tokenFile) // #nosec G304 -- administrator-selected enrollment file.
	if err != nil {
		return state{}, fmt.Errorf("read enrollment token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(token) > 256 {
		return state{}, errors.New("invalid enrollment token")
	}
	var response struct {
		NodeGUID string `json:"node_guid"`
		Token    string `json:"agent_token"`
	}
	if err := request(ctx, client, http.MethodPost, controller+"/api/agent/enroll", token, nil, &response); err != nil {
		return state{}, fmt.Errorf("enroll: %w", err)
	}
	value := state{NodeGUID: response.NodeGUID, Token: response.Token}
	if err := saveState(statePath, value); err != nil {
		return state{}, err
	}
	if err := os.Remove(tokenFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return state{}, err
	}
	return value, nil
}

func defaultInterface() string {
	if file, err := os.Open("/proc/net/route"); err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		_ = scanner.Scan()
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 4 && fields[1] == "00000000" {
				flags, err := strconv.ParseUint(fields[3], 16, 32)
				if err == nil && flags&2 != 0 {
					return fields[0]
				}
			}
		}
	}
	interfaces, _ := net.Interfaces()
	for _, candidate := range interfaces {
		if candidate.Flags&net.FlagUp != 0 && candidate.Flags&net.FlagLoopback == 0 {
			return candidate.Name
		}
	}
	return ""
}

func counter(interfaceName, name string) (int64, error) {
	if _, err := net.InterfaceByName(interfaceName); err != nil {
		return 0, err
	}
	if name != "rx_bytes" && name != "tx_bytes" {
		return 0, errors.New("invalid counter")
	}
	data, err := os.ReadFile(filepath.Join("/sys/class/net", interfaceName, "statistics", name)) // #nosec G304 -- interface is discovered from the kernel route table.
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("invalid interface counter")
	}
	return value, nil
}

func runID() string {
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	suffix := hex.EncodeToString(random)
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if boot := strings.TrimSpace(string(data)); boot != "" && len(boot) <= 128 {
			return boot + ":" + suffix
		}
	}
	return suffix
}

func collect(id string, sequence int64) (report, error) {
	interfaceName := defaultInterface()
	if interfaceName == "" {
		return report{}, errors.New("no active network interface")
	}
	rx, err := counter(interfaceName, "rx_bytes")
	if err != nil {
		return report{}, err
	}
	tx, err := counter(interfaceName, "tx_bytes")
	if err != nil {
		return report{}, err
	}
	return report{BootID: id, Sequence: sequence, InterfaceName: interfaceName, RXBytes: rx, TXBytes: tx, AgentVersion: version}, nil
}

func configHash(config runtimeConfig) (string, error) {
	config.ConfigHash = ""
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func requestHost(value string) string {
	host := strings.TrimSpace(strings.ToLower(value))
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(parsed, "[]")
	}
	return strings.Trim(host, "[]")
}

func normalizeAgentTargetURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("target is empty")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" {
		parsed, err = url.Parse("http://" + value)
	}
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("target must be an absolute HTTP(S) URL")
	}
	return parsed, nil
}

func agentPlaybackRequest(path string) bool {
	path = strings.ToLower(path)
	if strings.HasPrefix(path, "/videos/") || strings.HasPrefix(path, "/emby/videos/") ||
		strings.HasPrefix(path, "/audio/") || strings.HasPrefix(path, "/emby/audio/") ||
		strings.HasPrefix(path, "/livetv/") || strings.HasPrefix(path, "/emby/livetv/") {
		return true
	}
	if strings.HasPrefix(path, "/items/") || strings.HasPrefix(path, "/emby/items/") {
		return strings.Contains(path, "/download") || strings.Contains(path, "/file")
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status       int
	capture      *bytes.Buffer
	captureLimit int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	original := p
	if w.capture != nil && w.capture.Len() < w.captureLimit {
		remaining := w.captureLimit - w.capture.Len()
		capture := p
		if len(capture) > remaining {
			capture = capture[:remaining]
		}
		_, _ = w.capture.Write(capture)
	}
	return w.ResponseWriter.Write(original)
}
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		f.Flush()
	}
}
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking is not supported")
	}
	return h.Hijack()
}
func agentMetadataResponsePath(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	parts := strings.Split(strings.Trim(strings.ToLower(strings.TrimSpace(path)), "/"), "/")
	for index, part := range parts {
		if part == "items" && index+1 < len(parts) && parts[index+1] != "" && index+2 == len(parts) {
			return parts[index+1] != "counts" && parts[index+1] != "latest" && parts[index+1] != "filters" && parts[index+1] != "images" && parts[index+1] != "resume"
		}
	}
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if last == "items" {
		return true
	}
	if last == "resume" || last == "latest" || last == "episodes" || last == "seasons" || last == "nextup" {
		for _, part := range parts[:len(parts)-1] {
			if part == "items" || part == "shows" {
				return true
			}
		}
	}
	return false
}

func buildRouteHandler(config runtimeConfig, stores ...*siteStatsStore) (http.Handler, error) {
	var stats *siteStatsStore
	if len(stores) > 0 {
		stats = stores[0]
	}
	if stats != nil && stats.events == nil {
		stats.events = &requestEventStore{}
	}
	type routeHandler struct {
		primary  http.Handler
		playback http.Handler
	}
	routes := make(map[string]routeHandler, len(config.Routes))
	routeIDs := make(map[string]int64, len(config.Routes))
	for _, route := range config.Routes {
		route := route
		host := requestHost(route.Host)
		if host == "" {
			return nil, errors.New("agent config contains an invalid or duplicate host")
		}
		if _, exists := routes[host]; exists {
			return nil, errors.New("agent config contains an invalid or duplicate host")
		}
		target, err := normalizeAgentTargetURL(route.TargetURL)
		if err != nil {
			return nil, fmt.Errorf("invalid target for site %d", route.SiteID)
		}
		makeProxy := func(proxyTarget *url.URL) http.Handler {
			proxy := httputil.NewSingleHostReverseProxy(proxyTarget)
			baseDirector := proxy.Director
			headers := route.Headers
			proxy.Director = func(req *http.Request) {
				baseDirector(req)
				req.Host = proxyTarget.Host
				for name, values := range headers {
					req.Header[name] = append([]string(nil), values...)
				}
			}
			proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				fmt.Fprintf(os.Stderr, "Meridian Agent proxy failed for %s: %v\n", host, err)
			}
			return proxy
		}
		playback := target
		if strings.TrimSpace(route.PlaybackTargetURL) != "" {
			playback, err = normalizeAgentTargetURL(route.PlaybackTargetURL)
			if err != nil {
				return nil, fmt.Errorf("invalid playback target for site %d", route.SiteID)
			}
		}
		routes[host] = routeHandler{primary: makeProxy(target), playback: makeProxy(playback)}
		routeIDs[host] = route.SiteID
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := requestHost(r.Host)
		route, ok := routes[host]
		if !ok {
			http.Error(w, "site not assigned", http.StatusMisdirectedRequest)
			return
		}
		if r.URL.Path == "/.well-known/meridian-agent-health" {
			w.Header().Set("X-Meridian-Node", config.NodeGUID)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		handler := route.primary
		if agentPlaybackRequest(r.URL.Path) {
			handler = route.playback
		}
		var responseCapture bytes.Buffer
		captureResponse := agentMetadataResponsePath(r.Method, r.URL.Path)
		if captureResponse {
			// Keep the bounded event body JSON and avoid forwarding compressed
			// bytes through a JSON string to the controller.
			r.Header.Set("Accept-Encoding", "identity")
		}
		recorder := &statusRecorder{ResponseWriter: w}
		if captureResponse {
			recorder.capture = &responseCapture
			recorder.captureLimit = maxAgentEventResponseBodyBytes
		}
		var eventBody string
		if strings.Contains(strings.ToLower(r.URL.Path), "/sessions/playing") && r.Body != nil &&
			(r.ContentLength < 0 || r.ContentLength <= maxAgentEventBodyBytes) {
			data, err := io.ReadAll(io.LimitReader(r.Body, maxAgentEventBodyBytes+1))
			if err == nil && len(data) <= maxAgentEventBodyBytes {
				eventBody = string(data)
				r.Body = io.NopCloser(bytes.NewReader(data))
			}
		}
		handler.ServeHTTP(recorder, r)
		if stats != nil {
			stats.record(host, recorder.status)
			if stats.events != nil {
				clientIP := r.RemoteAddr
				if parsed, _, err := net.SplitHostPort(clientIP); err == nil {
					clientIP = parsed
				}
				status := recorder.status
				if status == 0 {
					status = http.StatusInternalServerError
				}
				authorization := ""
				if strings.Contains(strings.ToLower(r.URL.Path), "/sessions/playing") {
					authorization = r.Header.Get("Authorization")
				}
				stats.events.add(requestEvent{SiteID: routeIDs[host], Host: host, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, StatusCode: status, ClientIP: clientIP, UserAgent: r.UserAgent(), Authorization: authorization, Body: eventBody, ContentType: r.Header.Get("Content-Type"), ContentEncoding: r.Header.Get("Content-Encoding"), ResponseBody: responseCapture.String(), ResponseContentType: w.Header().Get("Content-Type"), ResponseContentEncoding: w.Header().Get("Content-Encoding"), RecordedAtMS: time.Now().UnixMilli()})
			}
		}
	}), nil
}

func writeRuntimeFile(path string, data []byte) error {
	if len(data) == 0 {
		return errors.New("runtime TLS data is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".meridian-runtime-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (runtime *agentRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	runtime.mu.RLock()
	handler := runtime.handler
	runtime.mu.RUnlock()
	if handler == nil {
		http.Error(w, "configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}

func (runtime *agentRuntime) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	runtime.mu.RLock()
	certificate := runtime.certificate
	runtime.mu.RUnlock()
	if certificate == nil {
		return nil, errors.New("TLS certificate is unavailable")
	}
	return certificate, nil
}

func shutdownServers(servers []*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(ctx)
	}
}

func listenersMatchConfig(currentPort, serverCount int, config runtimeConfig) bool {
	needsListener := len(config.Routes) > 0
	hasListener := serverCount > 0
	return currentPort == config.HTTPSPort && hasListener == needsListener
}

func (runtime *agentRuntime) startListeners(config runtimeConfig) ([]*http.Server, error) {
	if len(config.Routes) == 0 {
		return nil, nil
	}
	if config.EntryMode != "direct" {
		return nil, errors.New("Agent configuration must use HTTPS direct mode")
	}
	if config.HTTPPort != 0 {
		return nil, errors.New("Agent HTTP listener is no longer supported")
	}
	if config.HTTPSPort < 1 || config.HTTPSPort > 65535 {
		return nil, errors.New("Agent port must be between 1 and 65535")
	}
	httpsListener, err := net.Listen("tcp", ":"+strconv.Itoa(config.HTTPSPort))
	if err != nil {
		return nil, fmt.Errorf("HTTPS listener %d: %w", config.HTTPSPort, err)
	}
	proxy := &http.Server{Handler: runtime, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: runtime.getCertificate}}
	tlsListener := tls.NewListener(httpsListener, proxy.TLSConfig)
	servers := []*http.Server{proxy}
	go func() {
		if err := proxy.Serve(tlsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "Meridian Agent HTTPS listener failed: %v\n", err)
		}
	}()
	return servers, nil
}

func (runtime *agentRuntime) apply(config runtimeConfig) error {
	if config.SchemaVersion != 1 {
		return fmt.Errorf("unsupported config schema %d", config.SchemaVersion)
	}
	hash, err := configHash(config)
	if err != nil {
		return err
	}
	if hash != config.ConfigHash {
		return errors.New("agent config hash mismatch")
	}
	if runtime.stats == nil {
		runtime.stats = &siteStatsStore{}
	}
	if runtime.events == nil {
		runtime.events = &requestEventStore{}
	}
	handler, err := buildRouteHandler(config, runtime.stats)
	if err != nil {
		return err
	}
	var certificate *tls.Certificate
	if len(config.Routes) > 0 {
		parsed, err := tls.X509KeyPair([]byte(config.CertificatePEM), []byte(config.PrivateKeyPEM))
		if err != nil {
			return fmt.Errorf("load runtime TLS certificate: %w", err)
		}
		certificate = &parsed
		tlsDir := filepath.Join(runtime.stateDir, "tls")
		if err := writeRuntimeFile(filepath.Join(tlsDir, "fullchain.pem"), []byte(config.CertificatePEM)); err != nil {
			return err
		}
		if err := writeRuntimeFile(filepath.Join(tlsDir, "privkey.pem"), []byte(config.PrivateKeyPEM)); err != nil {
			return err
		}
	}
	runtime.mu.RLock()
	sameListeners := listenersMatchConfig(runtime.port, len(runtime.servers), config)
	oldServers := append([]*http.Server(nil), runtime.servers...)
	runtime.mu.RUnlock()
	if !sameListeners {
		shutdownServers(oldServers)
	}
	runtime.mu.Lock()
	runtime.handler, runtime.certificate, runtime.nodeGUID = handler, certificate, config.NodeGUID
	runtime.mu.Unlock()
	servers := oldServers
	if !sameListeners {
		servers, err = runtime.startListeners(config)
		if err != nil {
			runtime.mu.Lock()
			runtime.listenerError = err.Error()
			runtime.servers = nil
			runtime.mu.Unlock()
			return err
		}
	}
	runtime.mu.Lock()
	runtime.port = config.HTTPSPort
	runtime.servers, runtime.appliedHash, runtime.listenerError = servers, config.ConfigHash, ""
	runtime.mu.Unlock()
	return nil
}

func (runtime *agentRuntime) status() (string, string) {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.appliedHash, runtime.listenerError
}

func (runtime *agentRuntime) stop() {
	runtime.mu.Lock()
	servers := append([]*http.Server(nil), runtime.servers...)
	runtime.servers = nil
	runtime.mu.Unlock()
	shutdownServers(servers)
}

func fetchConfig(ctx context.Context, client *http.Client, controller, token string) (runtimeConfig, error) {
	var config runtimeConfig
	err := request(ctx, client, http.MethodGet, controller+"/api/agent/config", token, nil, &config)
	return config, err
}

func run() error {
	controllerFlag := flag.String("controller", "", "controller URL")
	statePath := flag.String("state", "/var/lib/meridian-agent/state.json", "state path")
	tokenFile := flag.String("enroll-token-file", "", "one-time enrollment token file")
	once := flag.Bool("once", false, "report once and exit")
	flag.Parse()
	controller, err := normalizeController(*controllerFlag)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	current, err := loadState(*statePath)
	if err != nil {
		return err
	}
	if current.Token == "" {
		if *tokenFile == "" {
			return errors.New("agent is not enrolled")
		}
		current, err = enroll(ctx, client, controller, *tokenFile, *statePath)
		if err != nil {
			return err
		}
		fmt.Printf("Meridian Agent enrolled as %s\n", current.NodeGUID)
	}
	runtime := &agentRuntime{stateDir: filepath.Dir(*statePath)}
	runtime.stats = &siteStatsStore{events: &requestEventStore{}}
	defer runtime.stop()
	id, sequence := runID(), int64(1)
	applyConfig := func() {
		config, err := fetchConfig(ctx, client, controller, current.Token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Meridian Agent config fetch failed: %v\n", err)
			return
		}
		if err = runtime.apply(config); err != nil {
			runtime.mu.Lock()
			runtime.listenerError = err.Error()
			runtime.mu.Unlock()
			fmt.Fprintf(os.Stderr, "Meridian Agent config apply failed: %v\n", err)
		}
	}
	reportOnce := func() error {
		payload, err := collect(id, sequence)
		if err != nil {
			return err
		}
		payload.AppliedConfigHash, payload.ListenerError = runtime.status()
		payload.SiteStats = runtime.stats.snapshot()
		payload.Events = runtime.stats.events.snapshot()
		if err := request(ctx, client, http.MethodPost, controller+"/api/agent/report", current.Token, payload, nil); err != nil {
			return err
		}
		runtime.stats.events.ack(payload.Events)
		sequence++
		return nil
	}
	applyConfig()
	if err := reportOnce(); err != nil {
		return err
	}
	if *once {
		return nil
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			applyConfig()
			if err := reportOnce(); err != nil {
				fmt.Fprintf(os.Stderr, "Meridian Agent report failed: %v\n", err)
			}
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "meridian-agent: %v\n", err)
		os.Exit(1)
	}
}
