package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPanelExternalProxyConfigurationSeparatesHeaderTrustFromDeployment(t *testing.T) {
	for _, test := range []struct {
		name    string
		flag    string
		cidrs   string
		want    bool
		wantErr bool
	}{
		{name: "implicit private defaults"},
		{name: "explicit private defaults", cidrs: defaultTrustedProxyCIDRs},
		{name: "private range is only header trust", cidrs: "172.17.0.0/16"},
		{name: "legacy loopback peers", cidrs: "127.0.0.1/32,::1/128", want: true},
		{name: "legacy exact private peer", cidrs: "172.17.0.5/32", want: true},
		{name: "legacy exact IPv6 peer", cidrs: "fd12:3456::5/128", want: true},
		{name: "mixed peers and subnet", cidrs: "127.0.0.1/32,172.17.0.0/16"},
		{name: "explicit deployment with defaults", flag: "true", want: true},
		{name: "explicit deployment with subnet", flag: " TRUE ", cidrs: "172.17.0.0/16", want: true},
		{name: "disabled legacy exact peer", flag: "false", cidrs: "172.17.0.5/32"},
		{name: "invalid deployment flag", flag: "yes", wantErr: true},
		{name: "empty allowlist", flag: "true", cidrs: ",", wantErr: true},
		{name: "empty legacy allowlist", cidrs: ","},
	} {
		t.Run(test.name, func(t *testing.T) {
			trusted, err := parseTrustedProxyCIDRs(test.cidrs)
			if err != nil {
				t.Fatal(err)
			}
			enabled, err := parsePanelExternalProxyEnabled(test.flag, trusted)
			if (err != nil) != test.wantErr || enabled != test.want {
				t.Fatalf("external proxy enabled=%v err=%v, want %v error=%v", enabled, err, test.want, test.wantErr)
			}
		})
	}
}

func TestHostOnlyIngressDeploymentPolicy(t *testing.T) {
	trusted, err := parseTrustedProxyCIDRs("")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		loopback bool
		tls      bool
		external bool
		trusted  []*net.IPNet
		want     bool
	}{
		{name: "public HTTP with default header trust", trusted: trusted},
		{name: "loopback", loopback: true, want: true},
		{name: "built-in TLS", tls: true, want: true},
		{name: "external proxy", external: true, trusted: trusted, want: true},
		{name: "external proxy without source allowlist", external: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := hostOnlyIngressAvailable(test.loopback, test.tls, test.external, test.trusted); got != test.want {
				t.Fatalf("host ingress available=%v, want %v", got, test.want)
			}
			app := newTestApp(t)
			app.panelBindLoopback = test.loopback
			app.panelTLSEnabled = test.tls
			app.panelExternalProxyEnabled = test.external
			app.trustedProxies = test.trusted
			app.pm.SetHostOnlyIngressSafe(test.want)
			response := httptest.NewRecorder()
			app.handleIngressCapabilities(response, httptest.NewRequest(http.MethodGet, "/api/ingress-capabilities", nil))
			body := decodeBody(t, response)
			if got := body["host_only_available"]; got != test.want {
				t.Fatalf("reported host ingress=%v, want %v", got, test.want)
			}
			if got := body["trusted_proxy_configured"]; got != (test.external && len(test.trusted) > 0) {
				t.Fatalf("reported external proxy=%v", got)
			}
		})
	}
}

func TestHandleBackupRestoreRequiresExternalProxyDeploymentToPreserveHostIngress(t *testing.T) {
	for _, externalProxy := range []bool{false, true} {
		name := "default header trust without proxy"
		if externalProxy {
			name = "external proxy enabled"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			targetPath := filepath.Join(dir, "target.db")
			targetDB, err := openDB(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			defer targetDB.Close()
			if _, err := targetDB.db.Exec(`UPDATE panel_settings SET panel_domain='panel.target.example.com', route_domain='target.example.com', listen_port=9090, tls_enabled=0, configured=1 WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			oldJWT := bytes.Repeat([]byte("j"), 32)
			oldHeader := bytes.Repeat([]byte("h"), 32)
			backupPath := filepath.Join(dir, "source.db")
			makeBackupDatabase(t, backupPath, oldJWT, oldHeader)
			addBackupIngressSites(t, backupPath)
			backupData, err := os.ReadFile(backupPath)
			if err != nil {
				t.Fatal(err)
			}
			includeTLS := false
			manifest := backupManifest{
				Format:            "meridian-backup",
				FormatVersion:     backupFormatVersion,
				Files:             []string{backupDatabaseEntry},
				IncludeTLS:        &includeTLS,
				JWTSecret:         base64.RawStdEncoding.EncodeToString(oldJWT),
				UpstreamHeaderKey: base64.RawStdEncoding.EncodeToString(oldHeader),
			}
			const password = "correct horse battery staple"
			payload, err := sealBackupV2(testBackupArchive(t, manifest, map[string][]byte{backupDatabaseEntry: backupData}), password)
			if err != nil {
				t.Fatal(err)
			}
			var form bytes.Buffer
			writer := multipart.NewWriter(&form)
			for field, value := range map[string]string{"password": password, "confirm": "恢复"} {
				if err := writer.WriteField(field, value); err != nil {
					t.Fatal(err)
				}
			}
			file, err := writer.CreateFormFile("backup", "backup.mrbak")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			trusted, err := parseTrustedProxyCIDRs("")
			if err != nil {
				t.Fatal(err)
			}
			app := &App{
				db:                        targetDB,
				dbPath:                    targetPath,
				pm:                        NewProxyManager(targetDB, bytes.Repeat([]byte("k"), 32)),
				trustedProxies:            trusted,
				panelExternalProxyEnabled: externalProxy,
			}
			request := httptest.NewRequest(http.MethodPost, "/api/backup/restore", &form)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			response := httptest.NewRecorder()
			app.handleBackupRestore(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("restore status=%d body=%s", response.Code, response.Body.String())
			}
			wantReset := 2
			if externalProxy {
				wantReset = 0
			}
			if got := decodeBody(t, response)["ingress_reset_count"]; got != float64(wantReset) {
				t.Fatalf("reset ingress count=%v, want %d", got, wantReset)
			}
			stagedDB, err := sql.Open("sqlite", filepath.Join(targetPath+backupPendingSuffix, backupDatabaseEntry)+"?_pragma=query_only(1)")
			if err != nil {
				t.Fatal(err)
			}
			defer stagedDB.Close()
			var disabledHosts int
			if err := stagedDB.QueryRow(`SELECT COUNT(*) FROM sites WHERE name IN ('shared-host', 'shared-both') AND ingress_mode=? AND public_host='' AND enabled=0`, ingressModeUnset).Scan(&disabledHosts); err != nil {
				t.Fatal(err)
			}
			if disabledHosts != wantReset {
				t.Fatalf("staged disabled host sites=%d, want %d", disabledHosts, wantReset)
			}
		})
	}
}
