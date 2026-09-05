package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/net/publicsuffix"
)

const (
	letsEncryptProductionDirectory  = "https://acme-v02.api.letsencrypt.org/directory"
	letsEncryptStagingDirectory     = "https://acme-staging-v02.api.letsencrypt.org/directory"
	panelCertificateRenewalWindow   = 30 * 24 * time.Hour
	panelCertificateRenewalInterval = 12 * time.Hour
	panelACMETokenCipherPrefix      = "v1:"
)

var errCertificateIssuanceBusy = errors.New("a certificate request is already running")

type panelCertificateStatus struct {
	Available                 bool   `json:"available"`
	TLSEnabled                bool   `json:"tls_enabled"`
	PanelDomain               string `json:"panel_domain"`
	PanelPrefix               string `json:"panel_prefix"`
	RouteDomain               string `json:"route_domain"`
	WildcardDomain            string `json:"wildcard_domain"`
	CertificateWildcardDomain string `json:"certificate_wildcard_domain,omitempty"`
	CertificateCurrent        bool   `json:"certificate_current"`
	CertificateValid          bool   `json:"certificate_valid"`
	CertificateReused         bool   `json:"certificate_reused,omitempty"`
	ListenPort                int    `json:"listen_port"`
	ActiveListenPort          int    `json:"active_listen_port"`
	Configured                bool   `json:"configured"`
	SettingsConfigured        bool   `json:"settings_configured"`
	Subject                   string `json:"subject,omitempty"`
	ExpiresAt                 string `json:"expires_at,omitempty"`
	DaysRemaining             int    `json:"days_remaining,omitempty"`
	AutoRenewEnabled          bool   `json:"auto_renew_enabled"`
	ACMEEmail                 string `json:"acme_email,omitempty"`
	ACMEDNSProvider           string `json:"dns_provider,omitempty"`
	DNSAPIToken               string `json:"dns_api_token,omitempty"`
	ACMEStaging               bool   `json:"acme_staging"`
	RestartRequired           bool   `json:"restart_required"`
	Issuing                   bool   `json:"issuing"`
}

type panelCertificateManager struct {
	certFile     string
	keyFile      string
	edgeCertFile string
	edgeKeyFile  string
	accountDir   string
	httpClient   *http.Client

	mu                 sync.Mutex
	issuing            bool
	issueGate          chan struct{}
	currentCertificate *tls.Certificate
}

// panelPairPaths returns the currently active panel certificate pair.  The
// legacy fullchain.pem/privkey.pem files remain readable for upgrades, while
// renewals switch a sibling pointer atomically so cert and key always match.
func (m *panelCertificateManager) panelPairPaths() (string, string) {
	if m == nil || strings.TrimSpace(m.certFile) == "" || strings.TrimSpace(m.keyFile) == "" {
		return "", ""
	}
	currentDir := filepath.Join(filepath.Dir(m.certFile), ".panel-current")
	if _, err := os.Stat(filepath.Join(currentDir, "fullchain.pem")); err == nil { // #nosec G703 -- currentDir is derived from the administrator-configured TLS path.
		return filepath.Join(currentDir, "fullchain.pem"), filepath.Join(currentDir, "privkey.pem")
	}
	return m.certFile, m.keyFile
}

func (m *panelCertificateManager) panelAtomicPairPaths() (string, string) {
	if m == nil || strings.TrimSpace(m.certFile) == "" {
		return "", ""
	}
	currentDir := filepath.Join(filepath.Dir(m.certFile), ".panel-current")
	return filepath.Join(currentDir, "fullchain.pem"), filepath.Join(currentDir, "privkey.pem")
}

func (m *panelCertificateManager) acquireIssue(ctx context.Context) error {
	if m == nil {
		return errors.New("certificate manager is unavailable")
	}
	m.mu.Lock()
	if m.issueGate == nil {
		m.issueGate = make(chan struct{}, 1)
	}
	gate := m.issueGate
	m.mu.Unlock()
	select {
	case gate <- struct{}{}:
		m.mu.Lock()
		m.issuing = true
		m.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *panelCertificateManager) releaseIssue() {
	if m == nil {
		return
	}
	m.mu.Lock()
	gate := m.issueGate
	m.issuing = false
	m.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

// nodeEdgeTLSPaths returns an isolated certificate pair for one Agent.  The
// legacy edgeCertFile/edgeKeyFile pair is retained for migration and CLI
// compatibility, but runtime Agent configurations must use this per-node
// directory so compromising one node cannot expose another node's key.
func (m *panelCertificateManager) nodeEdgeTLSPaths(nodeGUID string) (string, string, error) {
	if m == nil || strings.TrimSpace(m.edgeCertFile) == "" || strings.TrimSpace(m.edgeKeyFile) == "" {
		return "", "", errors.New("edge TLS certificate paths are unavailable")
	}
	guid := strings.TrimSpace(nodeGUID)
	if guid == "" || strings.ContainsAny(guid, `/\\`) || guid == "." || guid == ".." {
		return "", "", errors.New("node GUID is invalid")
	}
	for _, r := range guid {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return "", "", errors.New("node GUID is invalid")
		}
	}
	root := filepath.Join(filepath.Dir(m.edgeCertFile), "edge-nodes", guid)
	current := filepath.Join(root, "current")
	return filepath.Join(current, "fullchain.pem"), filepath.Join(current, "privkey.pem"), nil
}

// installCertificatePairAtomic writes both TLS files into an immutable
// generation and atomically switches one current-directory pointer.  Readers
// therefore observe a matching certificate/key pair even during renewal.
func installCertificatePairAtomic(certFile, keyFile string, certPEM, keyPEM []byte) error {
	if strings.TrimSpace(certFile) == "" || strings.TrimSpace(keyFile) == "" || filepath.Dir(certFile) != filepath.Dir(keyFile) {
		return errors.New("certificate and key paths must share a directory")
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("certificate/key pair is invalid: %w", err)
	}
	currentDir := filepath.Dir(certFile)
	root := filepath.Dir(currentDir)
	generations := filepath.Join(root, "generations")
	if err := os.MkdirAll(generations, 0o700); err != nil { // #nosec G703 -- paths are derived from validated administrator-controlled TLS locations.
		return err
	}
	generation, err := os.MkdirTemp(generations, "generation-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(generation) // #nosec G703 -- generation is created by MkdirTemp beneath the private TLS directory.
		}
	}()
	if err := writePrivateFileAtomic(filepath.Join(generation, "fullchain.pem"), certPEM); err != nil {
		return err
	}
	if err := writePrivateFileAtomic(filepath.Join(generation, "privkey.pem"), keyPEM); err != nil {
		return err
	}
	// Linux production uses an atomic symlink replacement. Windows test/dev
	// environments may not permit symlinks, so retain a safe pair-wise fallback.
	tmpLink := fmt.Sprintf("%s.next.%d.%d", currentDir, os.Getpid(), time.Now().UnixNano())
	_ = os.Remove(tmpLink)                                                                               // #nosec G703 -- tmpLink is derived from the validated current directory.
	if err := os.Symlink(filepath.Join("generations", filepath.Base(generation)), tmpLink); err == nil { // #nosec G703 -- link target is generated beneath the private TLS directory.
		if err := os.Rename(tmpLink, currentDir); err != nil { // #nosec G703 -- both paths are generated within the private TLS directory.
			// Backups from v1.9.13 may have restored current as a real
			// directory. Move it aside, switch the pointer, then remove the
			// legacy directory; readers never observe a partial new pair.
			legacyDir := fmt.Sprintf("%s.legacy.%d", currentDir, time.Now().UnixNano())
			if info, statErr := os.Stat(currentDir); statErr == nil && info.IsDir() && os.Rename(currentDir, legacyDir) == nil {
				if switchErr := os.Rename(tmpLink, currentDir); switchErr == nil {
					_ = os.RemoveAll(legacyDir) // #nosec G703 -- legacyDir is a private, uniquely named sibling of currentDir.
					keep = true
					if cleanupErr := cleanupOldCertificateGenerations(generations, currentDir, 2); cleanupErr != nil {
						return fmt.Errorf("cleanup old certificate generations: %w", cleanupErr)
					}
					return nil
				}
				_ = os.Rename(legacyDir, currentDir)
			}
			_ = os.Remove(tmpLink) // #nosec G703 -- tmpLink is generated within the private TLS directory.
			return err
		}
		keep = true
		if err := cleanupOldCertificateGenerations(generations, currentDir, 2); err != nil {
			return fmt.Errorf("cleanup old certificate generations: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(currentDir, 0o700); err != nil { // #nosec G703 -- currentDir is derived from validated TLS paths.
		return err
	}
	if err := writePrivateFileAtomic(certFile, certPEM); err != nil {
		return err
	}
	if err := writePrivateFileAtomic(keyFile, keyPEM); err != nil {
		return err
	}
	keep = true
	if err := cleanupOldCertificateGenerations(generations, currentDir, 2); err != nil {
		return fmt.Errorf("cleanup old certificate generations: %w", err)
	}
	return nil
}

// cleanupOldCertificateGenerations bounds long-running renewal storage while
// retaining the active generation and one rollback candidate.  It never
// follows or removes the current pointer itself.
func cleanupOldCertificateGenerations(generationsDir, currentDir string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	entries, err := os.ReadDir(generationsDir)
	if err != nil {
		return err
	}
	currentName := ""
	if target, readErr := os.Readlink(currentDir); readErr == nil {
		currentName = filepath.Base(filepath.Clean(target))
	}
	type generationEntry struct {
		name string
		info os.FileInfo
	}
	values := make([]generationEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "generation-") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		values = append(values, generationEntry{name: entry.Name(), info: info})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].info.ModTime().After(values[j].info.ModTime()) })
	retained := make(map[string]bool, keep)
	if currentName != "" {
		retained[currentName] = true
	}
	for _, entry := range values {
		if len(retained) >= keep {
			break
		}
		retained[entry.name] = true
	}
	for _, entry := range values {
		if retained[entry.name] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(generationsDir, entry.name)); err != nil { // #nosec G703 -- entry names are returned by ReadDir from the private generations directory.
			return err
		}
	}
	return nil
}

// validatePanelEdgeKeySeparation prevents an accidentally reused private key
// from being accepted at startup. Missing edge files are allowed so automatic
// provisioning can create the first per-node pair.
func (m *panelCertificateManager) validatePanelEdgeKeySeparation() error {
	if m == nil || m.certFile == "" || m.keyFile == "" || m.edgeCertFile == "" || m.edgeKeyFile == "" {
		return nil
	}
	panelCertFile, panelKeyFile := m.panelPairPaths()
	panelPair, panelErr := tls.LoadX509KeyPair(panelCertFile, panelKeyFile)
	if errors.Is(panelErr, os.ErrNotExist) {
		return nil
	}
	if panelErr != nil || len(panelPair.Certificate) == 0 {
		return nil
	}
	panelCert, err := x509.ParseCertificate(panelPair.Certificate[0])
	if err != nil {
		return nil
	}
	panelSPKI, err := x509.MarshalPKIXPublicKey(panelCert.PublicKey)
	if err != nil {
		return nil
	}
	comparePair := func(certFile, keyFile string) error {
		edgePair, edgeErr := tls.LoadX509KeyPair(certFile, keyFile)
		if errors.Is(edgeErr, os.ErrNotExist) || edgeErr != nil || len(edgePair.Certificate) == 0 {
			return nil
		}
		edgeCert, parseErr := x509.ParseCertificate(edgePair.Certificate[0])
		if parseErr != nil {
			return nil
		}
		edgeSPKI, marshalErr := x509.MarshalPKIXPublicKey(edgeCert.PublicKey)
		if marshalErr == nil && bytes.Equal(panelSPKI, edgeSPKI) {
			return errors.New("panel TLS and edge TLS use the same private key; configure a dedicated edge certificate")
		}
		return nil
	}
	if err := comparePair(m.edgeCertFile, m.edgeKeyFile); err != nil {
		return err
	}
	edgeRoot := filepath.Join(filepath.Dir(m.edgeCertFile), "edge-nodes")
	entries, readErr := os.ReadDir(edgeRoot)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		certFile := filepath.Join(edgeRoot, entry.Name(), "current", "fullchain.pem")
		keyFile := filepath.Join(edgeRoot, entry.Name(), "current", "privkey.pem")
		if err := comparePair(certFile, keyFile); err != nil {
			return err
		}
	}
	return nil
}

type issuedPanelCertificate struct {
	certPEM     []byte
	keyPEM      []byte
	certificate tls.Certificate
}

type installedPanelCertificateBackup struct {
	certPEM      []byte
	keyPEM       []byte
	marker       []byte
	certExists   bool
	keyExists    bool
	markerExists bool
}

type cloudflareClient struct {
	token      string
	httpClient *http.Client
	apiBase    string
}

type cloudflareResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func panelTLSPaths(dbPath string) (certFile, keyFile string) {
	certFile = strings.TrimSpace(os.Getenv("PANEL_TLS_CERT_FILE"))
	keyFile = strings.TrimSpace(os.Getenv("PANEL_TLS_KEY_FILE"))
	if certFile != "" || keyFile != "" {
		return certFile, keyFile
	}
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return "", ""
	}
	base := filepath.Join(filepath.Dir(dbPath), "tls")
	return filepath.Join(base, "fullchain.pem"), filepath.Join(base, "privkey.pem")
}

func panelTLSBackupPaths(dbPath string) (certFile, keyFile string) {
	certFile, keyFile = panelTLSPaths(dbPath)
	if certFile == "" {
		return certFile, keyFile
	}
	currentDir := filepath.Join(filepath.Dir(certFile), ".panel-current")
	if _, err := os.Stat(filepath.Join(currentDir, "fullchain.pem")); err == nil {
		return filepath.Join(currentDir, "fullchain.pem"), filepath.Join(currentDir, "privkey.pem")
	}
	return certFile, keyFile
}

func edgeNodeTLSRoot(dbPath string) string {
	certFile, _ := edgeTLSPaths(dbPath)
	if strings.TrimSpace(certFile) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(certFile), "edge-nodes")
}

// edgeTLSPaths deliberately has its own certificate and key.  Edge nodes are
// untrusted execution environments relative to the panel; the panel key must
// never be included in an Agent runtime configuration.
func edgeTLSPaths(dbPath string) (certFile, keyFile string) {
	certFile = strings.TrimSpace(os.Getenv("EDGE_TLS_CERT_FILE"))
	keyFile = strings.TrimSpace(os.Getenv("EDGE_TLS_KEY_FILE"))
	if certFile != "" || keyFile != "" {
		return certFile, keyFile
	}
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return "", ""
	}
	base := filepath.Join(filepath.Dir(dbPath), "tls")
	return filepath.Join(base, "edge-fullchain.pem"), filepath.Join(base, "edge-privkey.pem")
}

func newPanelCertificateManager(dbPath string, httpClient *http.Client) *panelCertificateManager {
	certFile, keyFile := panelTLSPaths(dbPath)
	edgeCertFile, edgeKeyFile := edgeTLSPaths(dbPath)
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	accountDir := ""
	if certFile != "" {
		accountDir = filepath.Dir(certFile)
	}
	return &panelCertificateManager{
		certFile:     certFile,
		keyFile:      keyFile,
		edgeCertFile: edgeCertFile,
		edgeKeyFile:  edgeKeyFile,
		accountDir:   accountDir,
		httpClient:   httpClient,
		issueGate:    make(chan struct{}, 1),
	}
}

func (m *panelCertificateManager) status(settings PanelSettings, activePanelDomain, activeRouteDomain string, activeListenPort int, tlsEnabled bool) panelCertificateStatus {
	status := panelCertificateStatus{
		Available:          m != nil && m.certFile != "" && m.keyFile != "",
		TLSEnabled:         tlsEnabled,
		PanelDomain:        settings.PanelDomain,
		PanelPrefix:        panelPrefixForSettings(settings),
		RouteDomain:        settings.RouteDomain,
		WildcardDomain:     wildcardDomainForSettings(settings),
		ListenPort:         settings.ListenPort,
		ActiveListenPort:   activeListenPort,
		RestartRequired:    settings.TLSEnabled != tlsEnabled || settings.PanelDomain != activePanelDomain || settings.RouteDomain != activeRouteDomain || settings.ListenPort != activeListenPort,
		SettingsConfigured: settings.Configured && settings.PanelDomain != "" && settings.RouteDomain != "" && settings.ListenPort != 0,
	}
	if m == nil {
		return status
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	status.Issuing = m.issuing
	certFile, _ := m.panelPairPaths()
	data, err := os.ReadFile(certFile) // #nosec G304 G703 -- certFile is an administrator-configured TLS path captured when the certificate manager is created, never an HTTP request value.
	if err != nil {
		return status
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return status
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return status
	}
	status.Configured = true
	for _, name := range certificate.DNSNames {
		if strings.HasPrefix(name, "*.") {
			status.CertificateWildcardDomain = strings.ToLower(name)
			break
		}
	}
	status.CertificateCurrent = status.CertificateWildcardDomain != "" && status.CertificateWildcardDomain == status.WildcardDomain
	now := time.Now()
	status.CertificateValid = !now.Before(certificate.NotBefore) && now.Before(certificate.NotAfter)
	status.Subject = certificate.Subject.CommonName
	status.ExpiresAt = certificate.NotAfter.UTC().Format(time.RFC3339)
	days := int(time.Until(certificate.NotAfter).Hours() / 24)
	if days < 0 {
		days = 0
	}
	status.DaysRemaining = days
	return status
}

func certificateNeedsRenewal(status panelCertificateStatus) bool {
	return !status.Configured || !status.CertificateCurrent || !status.CertificateValid || status.DaysRemaining <= int(panelCertificateRenewalWindow.Hours()/24)
}

func certificateCanBeReused(status panelCertificateStatus) bool {
	return status.CertificateCurrent && status.CertificateValid && !certificateNeedsRenewal(status)
}

func certificateStatusForFile(certFile string, expectedWildcard string) panelCertificateStatus {
	status := panelCertificateStatus{WildcardDomain: expectedWildcard}
	data, err := os.ReadFile(certFile) // #nosec G304 G703 -- path is derived from the administrator-controlled TLS directory.
	if err != nil {
		return status
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return status
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return status
	}
	status.Configured = true
	now := time.Now()
	status.CertificateValid = !now.Before(certificate.NotBefore) && now.Before(certificate.NotAfter)
	for _, name := range certificate.DNSNames {
		if strings.HasPrefix(name, "*.") {
			status.CertificateWildcardDomain = strings.ToLower(name)
			break
		}
	}
	status.CertificateCurrent = expectedWildcard == "" || strings.EqualFold(status.CertificateWildcardDomain, expectedWildcard)
	status.ExpiresAt = certificate.NotAfter.UTC().Format(time.RFC3339)
	status.DaysRemaining = int(time.Until(certificate.NotAfter).Hours() / 24)
	if status.DaysRemaining < 0 {
		status.DaysRemaining = 0
	}
	return status
}

func certificateCoversHost(certFile, host string) error {
	data, err := os.ReadFile(certFile) // #nosec G703 G304 -- path is derived from the administrator-controlled TLS directory.
	if err != nil {
		return fmt.Errorf("read edge TLS certificate: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("edge TLS certificate is invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse edge TLS certificate: %w", err)
	}
	if err := certificate.VerifyHostname(strings.TrimSpace(host)); err != nil {
		return fmt.Errorf("edge TLS certificate does not cover host %q", host)
	}
	return nil
}

// verifyCertificateChainForHost validates the complete PEM chain against the
// host's system trust store.  Hostname-only checks are insufficient for edge
// reuse because a self-signed or not-yet-valid certificate can still contain
// the expected SAN.
func verifyCertificateChainForHost(certFile, host string) error {
	data, err := os.ReadFile(certFile) // #nosec G304 G703 -- path is derived from administrator-controlled TLS configuration.
	if err != nil {
		return fmt.Errorf("read edge TLS certificate: %w", err)
	}
	var chain []*x509.Certificate
	for rest := data; len(rest) > 0; {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr != nil {
			return fmt.Errorf("parse edge TLS certificate: %w", parseErr)
		}
		chain = append(chain, cert)
	}
	if len(chain) == 0 {
		return errors.New("edge TLS certificate chain is empty")
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		return errors.New("system trust store is unavailable")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{DNSName: strings.TrimSpace(host), CurrentTime: time.Now(), Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("edge TLS certificate chain is not trusted: %w", err)
	}
	return nil
}

func wildcardDomainCoversHost(routeDomain, host string) bool {
	route := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(routeDomain)), ".")
	value := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if route == "" || value == "" {
		return true
	}
	suffix := "." + route
	if !strings.HasSuffix(value, suffix) {
		return false
	}
	label := strings.TrimSuffix(value, suffix)
	return label != "" && !strings.Contains(label, ".")
}

func panelACMETokenKeyForSecret(secret []byte) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte("meridian panel acme dns token v1\x00"))
	_, _ = h.Write(secret)
	return h.Sum(nil)
}

func encryptPanelACMEToken(token string) (string, error) {
	return encryptPanelACMETokenWithSecret(token, jwtSecret)
}

func encryptPanelACMETokenWithSecret(token string, secret []byte) (string, error) {
	token = strings.TrimSpace(token)
	if len(token) < 20 || len(token) > 512 || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("a valid Cloudflare DNS API token is required")
	}
	block, err := aes.NewCipher(panelACMETokenKeyForSecret(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(token), []byte("meridian-panel-acme"))
	return panelACMETokenCipherPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptPanelACMEToken(ciphertext string) (string, error) {
	return decryptPanelACMETokenWithSecret(ciphertext, jwtSecret)
}

func decryptPanelACMETokenWithSecret(ciphertext string, secret []byte) (string, error) {
	if !strings.HasPrefix(ciphertext, panelACMETokenCipherPrefix) {
		return "", errors.New("invalid Cloudflare DNS API token ciphertext")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(ciphertext, panelACMETokenCipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decode Cloudflare DNS API token: %w", err)
	}
	block, err := aes.NewCipher(panelACMETokenKeyForSecret(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(payload) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("invalid Cloudflare DNS API token ciphertext")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], []byte("meridian-panel-acme"))
	if err != nil {
		return "", fmt.Errorf("decrypt Cloudflare DNS API token: %w", err)
	}
	return string(plain), nil
}

func (m *panelCertificateManager) tlsConfig(enabled bool) (*tls.Config, bool, error) {
	if !enabled {
		return nil, false, nil
	}
	if m == nil || m.certFile == "" || m.keyFile == "" || m.accountDir == "" {
		return nil, false, errors.New("panel TLS certificate storage is unavailable")
	}
	certFile, keyFile := m.panelPairPaths()
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, false, fmt.Errorf("load panel TLS certificate: %w", err)
	}
	m.mu.Lock()
	m.currentCertificate = &certificate
	m.mu.Unlock()
	if err := writePrivateFileAtomic(filepath.Join(m.accountDir, "enabled"), []byte("enabled\n")); err != nil {
		return nil, false, fmt.Errorf("write panel TLS enabled marker: %w", err)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.currentCertificate == nil {
				return nil, errors.New("panel TLS certificate is unavailable")
			}
			return m.currentCertificate, nil
		},
	}, true, nil
}

func validatePanelCertificateRequest(email, token string) error {
	email = strings.TrimSpace(email)
	address, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(address.Address, email) || len(email) > 254 {
		return errors.New("a valid ACME email address is required")
	}
	token = strings.TrimSpace(token)
	if len(token) < 20 || len(token) > 512 || strings.ContainsAny(token, "\r\n") {
		return errors.New("a valid Cloudflare DNS API token is required")
	}
	return nil
}

func (m *panelCertificateManager) issueCloudflare(ctx context.Context, email, token, panelDomain, routeDomain string, staging bool) (*issuedPanelCertificate, error) {
	settings, err := normalizeManagedPanelSettings(panelDomain, routeDomain)
	if err != nil {
		return nil, err
	}
	return m.issueCloudflareForIdentifiers(ctx, email, token, settings.RouteDomain, []string{wildcardDomainForSettings(settings)}, staging)
}

// issueCloudflareForIdentifiers issues one certificate for an explicit SAN
// set. Edge certificates include a node-unique SAN alongside the shared
// wildcard, so each node uses a distinct ACME identifier set and does not
// consume the exact-set rate limit shared by every other node.
func (m *panelCertificateManager) issueCloudflareForIdentifiers(ctx context.Context, email, token, routeDomain string, identifiers []string, staging bool) (*issuedPanelCertificate, error) {
	normalizedRoute, err := normalizeRouteDomain(routeDomain)
	if err != nil {
		return nil, fmt.Errorf("invalid route domain: %w", err)
	}
	cleanIdentifiers := make([]string, 0, len(identifiers))
	seen := make(map[string]bool, len(identifiers))
	for _, identifier := range identifiers {
		value := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(identifier), "."))
		if value == "" || strings.ContainsAny(value, "\r\n") || len(value) > 253 {
			return nil, errors.New("invalid ACME certificate identifier")
		}
		if _, err := normalizePublicHost(strings.TrimPrefix(value, "*.")); err != nil {
			return nil, fmt.Errorf("invalid ACME certificate identifier %q: %w", value, err)
		}
		if strings.HasPrefix(value, "*.") && strings.Count(value, "*") != 1 {
			return nil, errors.New("invalid wildcard certificate identifier")
		}
		if !seen[value] {
			seen[value] = true
			cleanIdentifiers = append(cleanIdentifiers, value)
		}
	}
	if len(cleanIdentifiers) == 0 {
		return nil, errors.New("at least one ACME certificate identifier is required")
	}
	if m == nil || m.certFile == "" || m.keyFile == "" || m.accountDir == "" {
		return nil, errors.New("可写的 TLS 数据目录不可用")
	}
	if err := validatePanelCertificateRequest(email, token); err != nil {
		return nil, err
	}
	if err := m.acquireIssue(ctx); err != nil {
		return nil, err
	}
	defer m.releaseIssue()

	if err := os.MkdirAll(m.accountDir, 0o700); err != nil { // #nosec G703 -- accountDir is captured from administrator-configured TLS paths.
		return nil, fmt.Errorf("create TLS directory: %w", err)
	}
	directoryURL := letsEncryptProductionDirectory
	accountName := "acme-account.pem"
	if staging {
		directoryURL = letsEncryptStagingDirectory
		accountName = "acme-account-staging.pem"
	}
	accountKey, err := loadOrCreateACMEAccountKey(m.accountDir, accountName)
	if err != nil {
		return nil, err
	}
	client := &acme.Client{Key: accountKey, DirectoryURL: directoryURL, UserAgent: "Meridian/" + appVersion}
	_, err = client.Register(ctx, &acme.Account{Contact: []string{"mailto:" + strings.TrimSpace(email)}}, acme.AcceptTOS)
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("register ACME account: %w", err)
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(cleanIdentifiers...))
	if err != nil {
		return nil, fmt.Errorf("create ACME order: %w", err)
	}
	cf := &cloudflareClient{token: strings.TrimSpace(token), httpClient: m.httpClient, apiBase: "https://api.cloudflare.com/client/v4"}
	for _, authorizationURL := range order.AuthzURLs {
		if err := fulfillCloudflareDNSAuthorization(ctx, client, cf, authorizationURL, normalizedRoute); err != nil {
			return nil, err
		}
	}
	readyOrder, err := client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("wait for ACME order: %w", err)
	}
	certificateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate certificate key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: cleanIdentifiers}, certificateKey)
	if err != nil {
		return nil, fmt.Errorf("create certificate request: %w", err)
	}
	chain, _, err := client.CreateOrderCert(ctx, readyOrder.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("issue certificate: %w", err)
	}
	certPEM := make([]byte, 0, len(chain)*1024)
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificateKey)
	if err != nil {
		return nil, fmt.Errorf("encode certificate key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("validate issued certificate pair: %w", err)
	}
	return &issuedPanelCertificate{certPEM: certPEM, keyPEM: keyPEM, certificate: certificate}, nil
}

func (m *panelCertificateManager) install(issued *issuedPanelCertificate, activate bool) error {
	if m == nil || issued == nil || m.certFile == "" || m.keyFile == "" || m.accountDir == "" {
		return errors.New("issued panel certificate is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	certFile, keyFile := m.panelAtomicPairPaths()
	if certFile == "" || keyFile == "" {
		return errors.New("panel TLS certificate storage is unavailable")
	}
	if err := installCertificatePairAtomic(certFile, keyFile, issued.certPEM, issued.keyPEM); err != nil {
		return fmt.Errorf("install certificate pair: %w", err)
	}
	if err := writePrivateFileAtomic(filepath.Join(m.accountDir, "enabled"), []byte("enabled\n")); err != nil {
		return fmt.Errorf("write panel TLS enabled marker: %w", err)
	}
	if activate {
		certificate := issued.certificate
		m.currentCertificate = &certificate
	}
	return nil
}

func (m *panelCertificateManager) activate(issued *issuedPanelCertificate) {
	if m == nil || issued == nil {
		return
	}
	m.mu.Lock()
	certificate := issued.certificate
	m.currentCertificate = &certificate
	m.mu.Unlock()
}

// disable clears the active in-memory certificate and marker without deleting
// the certificate files. Keeping the files allows a later manual issuance or
// renewal to reuse the existing storage after the panel has fallen back to
// HTTP.
func (m *panelCertificateManager) disable() error {
	if m == nil || m.accountDir == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentCertificate = nil
	err := os.Remove(filepath.Join(m.accountDir, "enabled")) // #nosec G703 -- accountDir is the managed TLS directory and enabled is a fixed marker basename.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func readOptionalFile(filename string) ([]byte, bool, error) {
	if filename == "" || filepath.Base(filename) == "." || filepath.Base(filename) == string(filepath.Separator) {
		return nil, false, errors.New("invalid TLS file path")
	}
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return nil, false, err
	}
	file, err := root.Open(filepath.Base(filename))
	if err != nil {
		_ = root.Close()
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 1<<20))
	closeErr := file.Close()
	rootErr := root.Close()
	if readErr != nil {
		return nil, false, readErr
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	if rootErr != nil {
		return nil, false, rootErr
	}
	return data, true, nil
}

func (m *panelCertificateManager) backupInstalledFiles() (installedPanelCertificateBackup, error) {
	if m == nil || m.certFile == "" || m.keyFile == "" || m.accountDir == "" {
		return installedPanelCertificateBackup{}, errors.New("panel TLS certificate storage is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var backup installedPanelCertificateBackup
	var err error
	certFile, keyFile := m.panelPairPaths()
	if backup.certPEM, backup.certExists, err = readOptionalFile(certFile); err != nil {
		return installedPanelCertificateBackup{}, fmt.Errorf("back up certificate chain: %w", err)
	}
	if backup.keyPEM, backup.keyExists, err = readOptionalFile(keyFile); err != nil {
		return installedPanelCertificateBackup{}, fmt.Errorf("back up certificate key: %w", err)
	}
	markerFile := filepath.Join(m.accountDir, "enabled")
	if backup.marker, backup.markerExists, err = readOptionalFile(markerFile); err != nil {
		return installedPanelCertificateBackup{}, fmt.Errorf("back up TLS marker: %w", err)
	}
	return backup, nil
}

func restoreOptionalFile(filename string, data []byte, existed bool) error {
	if existed {
		return writePrivateFileAtomic(filename, data)
	}
	err := os.Remove(filename) // #nosec G703 -- callers pass only the manager's certificate, private-key, or fixed enabled-marker path.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (m *panelCertificateManager) restoreInstalledFiles(backup installedPanelCertificateBackup) error {
	if m == nil || m.certFile == "" || m.keyFile == "" || m.accountDir == "" {
		return errors.New("panel TLS certificate storage is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	certFile, keyFile := m.panelPairPaths()
	if backup.certExists && backup.keyExists {
		atomicCert, atomicKey := m.panelAtomicPairPaths()
		if err := installCertificatePairAtomic(atomicCert, atomicKey, backup.certPEM, backup.keyPEM); err != nil {
			return fmt.Errorf("restore certificate pair: %w", err)
		}
	} else {
		// No previous pair means this was the first installation. Remove the
		// newly-created atomic pointer before restoring the legacy paths.
		currentDir := filepath.Join(filepath.Dir(m.certFile), ".panel-current")
		if err := os.Remove(currentDir); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- currentDir is derived from the manager's validated TLS path.
			if removeAllErr := os.RemoveAll(currentDir); removeAllErr != nil { // #nosec G703 -- currentDir is derived from the manager's validated TLS path.
				return fmt.Errorf("remove new certificate pointer: %w", err)
			}
		}
		if err := restoreOptionalFile(keyFile, backup.keyPEM, backup.keyExists); err != nil {
			return fmt.Errorf("restore certificate key: %w", err)
		}
		if err := restoreOptionalFile(certFile, backup.certPEM, backup.certExists); err != nil {
			return fmt.Errorf("restore certificate chain: %w", err)
		}
	}
	if err := restoreOptionalFile(filepath.Join(m.accountDir, "enabled"), backup.marker, backup.markerExists); err != nil {
		return fmt.Errorf("restore TLS marker: %w", err)
	}
	m.currentCertificate = nil
	return nil
}

func loadOrCreateACMEAccountKey(directory, filename string) (crypto.Signer, error) {
	if filename == "" || filename != filepath.Base(filename) {
		return nil, errors.New("ACME account key filename must be a base name")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open ACME account directory: %w", err)
	}
	file, err := root.Open(filename)
	if err == nil {
		const maxAccountKeyBytes = 64 << 10
		data, readErr := io.ReadAll(io.LimitReader(file, maxAccountKeyBytes+1))
		closeErr := file.Close()
		rootCloseErr := root.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read ACME account key: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close ACME account key: %w", closeErr)
		}
		if rootCloseErr != nil {
			return nil, fmt.Errorf("close ACME account directory: %w", rootCloseErr)
		}
		if len(data) > maxAccountKeyBytes {
			return nil, errors.New("stored ACME account key is too large")
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, errors.New("stored ACME account key is invalid")
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse ACME account key: %w", err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, errors.New("stored ACME account key is not a signer")
		}
		return signer, nil
	}
	if closeErr := root.Close(); closeErr != nil {
		return nil, fmt.Errorf("close ACME account directory: %w", closeErr)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read ACME account key: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ACME account key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode ACME account key: %w", err)
	}
	if err := writePrivateFileAtomic(filepath.Join(directory, filename), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, fmt.Errorf("write ACME account key: %w", err)
	}
	return key, nil
}

func writePrivateFileAtomic(filename string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil { // #nosec G703 G304 -- filename is generated from the configured private TLS directory.
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".meridian-tls-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 G304 -- filename is generated from the configured private TLS directory.
			return err
		}
	}
	return os.Rename(tmpName, filename) // #nosec G703 G304 -- both paths are generated within the private TLS directory.
}

func fulfillCloudflareDNSAuthorization(ctx context.Context, acmeClient *acme.Client, cf *cloudflareClient, authorizationURL, routeDomain string) error {
	authorization, err := acmeClient.GetAuthorization(ctx, authorizationURL)
	if err != nil {
		return fmt.Errorf("get ACME authorization: %w", err)
	}
	if authorization.Status == acme.StatusValid {
		return nil
	}
	var challenge *acme.Challenge
	for _, candidate := range authorization.Challenges {
		if candidate.Type == "dns-01" {
			challenge = candidate
			break
		}
	}
	if challenge == nil {
		return errors.New("ACME server did not offer a DNS-01 challenge")
	}
	value, err := acmeClient.DNS01ChallengeRecord(challenge.Token)
	if err != nil {
		return fmt.Errorf("prepare DNS-01 challenge: %w", err)
	}
	zoneName, err := publicsuffix.EffectiveTLDPlusOne(routeDomain)
	if err != nil {
		return fmt.Errorf("resolve DNS zone: %w", err)
	}
	zoneID, err := cf.findZone(ctx, zoneName)
	if err != nil {
		return err
	}
	recordName := "_acme-challenge." + strings.TrimPrefix(authorization.Identifier.Value, "*.")
	recordID, err := cf.createTXTRecord(ctx, zoneID, recordName, value)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = cf.deleteRecord(cleanupCtx, zoneID, recordID)
	}()
	if err := waitForTXTRecord(ctx, recordName, value); err != nil {
		return err
	}
	if _, err := acmeClient.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("accept DNS-01 challenge: %w", err)
	}
	if _, err := acmeClient.WaitAuthorization(ctx, authorization.URI); err != nil {
		return fmt.Errorf("validate DNS-01 challenge: %w", err)
	}
	return nil
}

func dnsPropagationResolvers() []*net.Resolver {
	servers := strings.Split(strings.TrimSpace(os.Getenv("DNS_PROPAGATION_RESOLVERS")), ",")
	if len(servers) == 1 && strings.TrimSpace(servers[0]) == "" {
		servers = []string{"1.1.1.1", "8.8.8.8"}
	}
	resolvers := make([]*net.Resolver, 0, len(servers)+1)
	for _, server := range servers {
		server = strings.TrimSpace(server)
		if net.ParseIP(server) == nil {
			continue
		}
		resolverIP := server
		resolvers = append(resolvers, &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "udp", net.JoinHostPort(resolverIP, "53"))
		}})
	}
	// Keep the host resolver as a final fallback for split-horizon DNS and
	// environments that intentionally block public recursive resolvers.
	resolvers = append(resolvers, net.DefaultResolver)
	return resolvers
}

func dnsPropagationTimeout() time.Duration {
	value := strings.TrimSpace(os.Getenv("DNS_PROPAGATION_TIMEOUT"))
	if value == "" {
		return 2 * time.Minute
	}
	if duration, err := time.ParseDuration(value); err == nil && duration >= 10*time.Second && duration <= 15*time.Minute {
		return duration
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 10 && seconds <= 900 {
		return time.Duration(seconds) * time.Second
	}
	return 2 * time.Minute
}

func waitForTXTRecord(ctx context.Context, name, value string) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timeout := time.NewTimer(dnsPropagationTimeout())
	defer timeout.Stop()
	resolvers := dnsPropagationResolvers()
	for {
		for _, resolver := range resolvers {
			records, err := resolver.LookupTXT(ctx, name)
			if err == nil {
				for _, record := range records {
					if record == value {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("DNS-01 record %s did not propagate within %s", name, dnsPropagationTimeout())
		case <-ticker.C:
		}
	}
}

func (c *cloudflareClient) request(ctx context.Context, method, requestPath string, body io.Reader) (json.RawMessage, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.apiBase, "/")+requestPath, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("cloudflare DNS request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read Cloudflare DNS response: %w", err)
	}
	var envelope cloudflareResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, errors.New("cloudflare DNS returned an invalid response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		message := "cloudflare DNS request was rejected"
		if len(envelope.Errors) > 0 && strings.TrimSpace(envelope.Errors[0].Message) != "" {
			message += ": " + strings.TrimSpace(envelope.Errors[0].Message)
		}
		return nil, errors.New(message)
	}
	return envelope.Result, nil
}

func (c *cloudflareClient) findZone(ctx context.Context, zoneName string) (string, error) {
	result, err := c.request(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(zoneName)+"&status=active&per_page=1", nil)
	if err != nil {
		return "", err
	}
	var zones []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &zones); err != nil || len(zones) == 0 || zones[0].ID == "" {
		return "", fmt.Errorf("cloudflare DNS zone %s was not found", zoneName)
	}
	return zones[0].ID, nil
}

func (c *cloudflareClient) createTXTRecord(ctx context.Context, zoneID, name, value string) (string, error) {
	body, err := json.Marshal(map[string]interface{}{"type": "TXT", "name": name, "content": value, "ttl": 60})
	if err != nil {
		return "", err
	}
	result, err := c.request(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	var record struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &record); err != nil || record.ID == "" {
		return "", errors.New("cloudflare DNS did not return the challenge record ID")
	}
	return record.ID, nil
}

func (c *cloudflareClient) deleteRecord(ctx context.Context, zoneID, recordID string) error {
	_, err := c.request(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), nil)
	return err
}
