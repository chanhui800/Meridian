package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func runCommandLine(args []string, input io.Reader, output io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "--version", "-v":
		if len(args) != 1 {
			return true, errors.New("version command does not accept arguments")
		}
		_, err := fmt.Fprintln(output, appVersion)
		return true, err
	case "--healthcheck":
		if len(args) != 1 {
			return true, errors.New("healthcheck command does not accept arguments")
		}
		return true, runHealthcheckCommand()
	case "admin":
		return true, runAdminCommand(args[1:], input, output)
	default:
		return false, nil
	}
}

func runHealthcheckCommand() error {
	dbPath := strings.TrimSpace(os.Getenv("DB_PATH"))
	if dbPath == "" {
		dbPath = "/app/data/meridian.db"
	}
	markerPath := filepath.Join(filepath.Dir(dbPath), "panel-port")
	// #nosec G703 G304 -- DB_PATH is an administrator-controlled local database location; this only reads the sibling panel-port marker written by Meridian itself.
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read panel port: %w", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(marker)))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid panel port %q", strings.TrimSpace(string(marker)))
	}

	transport := &http.Transport{
		// #nosec G402 -- the healthcheck connects only to the fixed 127.0.0.1 host; the configured panel certificate normally names its public domain rather than loopback.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   4 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var lastErr error
	hosts := []string{"127.0.0.1", "::1"}
	for _, scheme := range []string{"https", "http"} {
		for _, host := range hosts {
			endpoint := fmt.Sprintf("%s://%s/api/auth/check", scheme, net.JoinHostPort(host, strconv.Itoa(port)))
			// #nosec G704 -- scheme and host are selected from fixed loopback lists, path is constant, and port is range-validated.
			resp, requestErr := client.Get(endpoint)
			if requestErr != nil {
				lastErr = requestErr
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			closeErr := resp.Body.Close()
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && closeErr == nil {
				return nil
			}
			lastErr = fmt.Errorf("%s returned HTTP %d", endpoint, resp.StatusCode)
		}
	}
	return fmt.Errorf("panel healthcheck failed: %w", lastErr)
}

func runAdminCommand(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: meridian admin reset-password --db <path> --password-stdin | issue-edge-certificate")
	}
	if args[0] == "issue-edge-certificate" {
		if len(args) != 1 {
			return errors.New("issue-edge-certificate does not accept arguments")
		}
		return runIssueEdgeCertificateCommand(output)
	}
	if args[0] != "reset-password" {
		return errors.New("usage: meridian admin reset-password --db <path> --password-stdin | issue-edge-certificate")
	}
	var dbPath string
	passwordStdin := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--db":
			if dbPath != "" || i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return errors.New("--db requires exactly one non-empty path")
			}
			dbPath = args[i+1]
			i++
		case "--password-stdin":
			if passwordStdin {
				return errors.New("--password-stdin may only be specified once")
			}
			passwordStdin = true
		default:
			return errors.New("unknown reset-password argument")
		}
	}
	if dbPath == "" || !passwordStdin {
		return errors.New("usage: meridian admin reset-password --db <path> --password-stdin")
	}

	password, err := readPasswordLine(input)
	if err != nil {
		return err
	}
	db, err := openDB(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := db.ResetAdminPassword(password); err != nil {
		return fmt.Errorf("reset administrator password: %w", err)
	}
	_, err = fmt.Fprintln(output, "administrator password updated")
	return err
}

// runIssueEdgeCertificateCommand provisions a second wildcard certificate for
// Agent listeners without touching the panel certificate/key pair. It reads
// the already encrypted Cloudflare credential from the local database and
// only prints non-sensitive status to the caller.
func runIssueEdgeCertificateCommand(output io.Writer) error {
	if jwtSecretEphemeral {
		return errors.New("JWT_SECRET is ephemeral; configure a persistent secret before issuing an edge certificate")
	}
	dbPath := strings.TrimSpace(os.Getenv("DB_PATH"))
	if dbPath == "" {
		dbPath = "/app/data/meridian.db"
	}
	db, err := openDB(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	settings, err := db.PanelSettings()
	if err != nil {
		return fmt.Errorf("read panel settings: %w", err)
	}
	if !settings.Configured || strings.TrimSpace(settings.ACMEEmail) == "" || strings.TrimSpace(settings.ACMETokenCiphertext) == "" {
		return errors.New("Cloudflare ACME credentials and panel settings are not configured")
	}
	token, err := decryptPanelACMEToken(settings.ACMETokenCiphertext)
	if err != nil {
		return fmt.Errorf("decrypt Cloudflare DNS token: %w", err)
	}
	manager := newPanelCertificateManager(dbPath, nil)
	if manager == nil || strings.TrimSpace(manager.edgeCertFile) == "" || strings.TrimSpace(manager.edgeKeyFile) == "" {
		return errors.New("edge TLS certificate paths are unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	issued, err := manager.issueCloudflare(ctx, settings.ACMEEmail, token, settings.PanelDomain, settings.RouteDomain, settings.ACMEStaging)
	if err != nil {
		return fmt.Errorf("issue edge wildcard certificate: %w", err)
	}
	// #nosec G703 -- the manager path is derived from administrator-controlled
	// DB_PATH/EDGE_TLS_CERT_FILE and is never supplied by an HTTP request.
	if err := os.MkdirAll(filepath.Dir(manager.edgeCertFile), 0o700); err != nil {
		return fmt.Errorf("create edge TLS directory: %w", err)
	}
	if err := writePrivateFileAtomic(manager.edgeKeyFile, issued.keyPEM); err != nil {
		return fmt.Errorf("write edge TLS private key: %w", err)
	}
	if err := writePrivateFileAtomic(manager.edgeCertFile, issued.certPEM); err != nil {
		return fmt.Errorf("write edge TLS certificate: %w", err)
	}
	_, err = fmt.Fprintf(output, "edge wildcard certificate installed for *.%s\n", settings.RouteDomain)
	return err
}

func readPasswordLine(input io.Reader) (string, error) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64), 74)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return "", errors.New("password input is empty")
	}
	password := strings.TrimSuffix(scanner.Text(), "\r")
	if scanner.Scan() {
		return "", errors.New("password input must contain exactly one line")
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if err := validateAdminPassword(password); err != nil {
		return "", err
	}
	return password, nil
}

func panelListenAddress(bindAddress string, port int) (string, error) {
	bindAddress = strings.TrimSpace(bindAddress)
	if bindAddress == "" {
		bindAddress = "0.0.0.0"
	}
	if net.ParseIP(bindAddress) == nil {
		return "", fmt.Errorf("PANEL_BIND_ADDR must be an IP address, got %q", bindAddress)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("panel port must be between 1 and 65535, got %d", port)
	}
	return net.JoinHostPort(bindAddress, strconv.Itoa(port)), nil
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func panelTLSConfigFromEnv(dbPath string) (*tls.Config, bool, error) {
	return newPanelCertificateManager(dbPath, nil).tlsConfig(envBool("PANEL_TLS_ENABLED"))
}

func panelPortMarkerPath(dbPath string) string {
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), "panel-port")
}
