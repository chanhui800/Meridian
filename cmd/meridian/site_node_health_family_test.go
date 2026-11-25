package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSiteDNSCandidateHealthProbeDoesNotBorrowAnotherFamily(t *testing.T) {
	for _, failingFamily := range []string{"v4", "v6"} {
		t.Run("unhealthy="+failingFamily, func(t *testing.T) {
			f := newSchedulerDNSFixture(t, true)
			listener4, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener4.Close() })
			_, portText, err := net.SplitHostPort(listener4.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			listener6, err := net.Listen("tcp6", net.JoinHostPort("::1", portText))
			if err != nil {
				t.Skipf("IPv6 loopback listener is unavailable: %v", err)
			}
			t.Cleanup(func() { _ = listener6.Close() })
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			certificateTemplate := &x509.Certificate{
				SerialNumber: big.NewInt(1),
				NotBefore:    time.Now().Add(-time.Minute),
				NotAfter:     time.Now().Add(time.Hour),
				DNSNames:     []string{f.host},
				KeyUsage:     x509.KeyUsageDigitalSignature,
				ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}
			certificateDER, err := x509.CreateCertificate(rand.Reader, certificateTemplate, certificateTemplate, &privateKey.PublicKey, privateKey)
			if err != nil {
				t.Fatal(err)
			}
			certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
			privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
			if err != nil {
				t.Fatal(err)
			}
			privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
			serverCertificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := x509.ParseCertificate(certificateDER)
			if err != nil {
				t.Fatal(err)
			}
			var failV4, failV6 atomic.Bool
			var hitsV4, hitsV6 atomic.Int64
			startServer := func(listener net.Listener, failure *atomic.Bool, hits *atomic.Int64) *http.Server {
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if r.URL.Path != "/.well-known/meridian-agent-health" || r.Host != f.host || r.Header.Get("X-Meridian-Probe") != encodeRuntimeKey(f.probeSecret) {
						t.Error("health probe lost its path, hostname, or authentication binding")
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					if failure.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("X-Meridian-Node", f.node.GUID)
					w.WriteHeader(http.StatusOK)
				})}
				go func() {
					_ = server.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{serverCertificate}, MinVersion: tls.VersionTLS12}))
				}()
				t.Cleanup(func() { _ = server.Close() })
				return server
			}
			startServer(listener4, &failV4, &hitsV4)
			startServer(listener6, &failV6, &hitsV6)
			roots := x509.NewCertPool()
			roots.AddCert(leaf)
			probe := func(ctx context.Context, node ControlNode, host string, secret []byte) error {
				addresses, err := scheduledNodeHealthAddresses(node)
				if err != nil || len(addresses) != 1 || addresses[0] != node.Address {
					t.Fatalf("one DNS candidate must probe only its own address: candidate=%q DNS mode=%q probes=%v err=%v", node.Address, node.DNSPublish, addresses, err)
				}
				// Route the selected public candidate to loopback, then exercise the
				// production TLS/authenticated HTTP probe without re-resolving families.
				var loopback string
				switch node.Address {
				case f.node.AddressV4:
					loopback = "127.0.0.1"
				case f.node.AddressV6:
					loopback = "::1"
				default:
					return fmt.Errorf("unexpected health candidate %q", node.Address)
				}
				node.Port = port
				return probeScheduledNodeAtAddress(ctx, node, host, secret, roots, loopback)
			}
			reconcile := func(at time.Time) error {
				unlock := f.app.lockSiteSchedule(f.site.ID)
				defer unlock()
				schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
				if err != nil {
					return err
				}
				return f.app.reconcileOneSiteScheduleWithProbeLocked(context.Background(), schedule, at, probe)
			}
			if err := reconcile(f.now); err != nil {
				t.Fatalf("both healthy families must pass readiness: %v", err)
			}
			if hitsV4.Load() != 1 || hitsV6.Load() != 1 {
				t.Fatalf("healthy candidate requests IPv4/IPv6=%d/%d, want 1/1", hitsV4.Load(), hitsV6.Load())
			}
			getsBefore := f.gets.Load()
			wantRecordType := "A"
			if failingFamily == "v4" {
				failV4.Store(true)
			} else {
				failV6.Store(true)
				wantRecordType = "AAAA"
			}
			if err := reconcile(f.now.Add(15 * time.Second)); err == nil || !strings.Contains(err.Error(), wantRecordType+" entry health check") {
				t.Fatalf("unhealthy %s must block DNS readiness despite its healthy sibling: %v", failingFamily, err)
			}
			if f.gets.Load() != getsBefore || f.posts.Load() != 0 || f.puts.Load() != 0 || f.deletes.Load() != 0 {
				t.Fatal("unhealthy address family reached Cloudflare DNS maintenance")
			}
		})
	}
}
