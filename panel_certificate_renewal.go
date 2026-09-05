package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const edgeCertificateRenewalTimeout = 3 * time.Minute

func edgeCertificateHost(routeDomain, nodeGUID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(nodeGUID)))
	return "edge-" + hex.EncodeToString(digest[:6]) + "." + strings.TrimSuffix(strings.ToLower(strings.TrimSpace(routeDomain)), ".")
}

func edgeCertificateIdentifiers(routeDomain, nodeGUID string) []string {
	wildcard := "*." + strings.TrimSuffix(strings.ToLower(strings.TrimSpace(routeDomain)), ".")
	return []string{wildcard, edgeCertificateHost(routeDomain, nodeGUID)}
}

func edgeCertificateRequired(node ControlNode) bool {
	return node.EnrolledAtMS > 0 && node.Enabled
}

func runPanelCertificateRenewalScheduler(ctx context.Context, db *DB, manager *panelCertificateManager, restart ...func()) {
	if db == nil || manager == nil {
		return
	}
	check := func() {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		disabled, err := disableExpiredPanelTLSIfNeeded(db, manager)
		if err != nil {
			log.Printf("[panel-certificate] automatic HTTPS fallback failed: %v", err)
			return
		}
		if disabled {
			if len(restart) > 0 && restart[0] != nil {
				restart[0]()
			}
		}
		if err := renewPanelCertificateIfDue(checkCtx, db, manager); err != nil && !errors.Is(err, errCertificateIssuanceBusy) && !errors.Is(err, context.Canceled) {
			log.Printf("[panel-certificate] automatic renewal failed: %v", err)
		}
		if err := renewEdgeCertificatesIfDue(checkCtx, db, manager); err != nil && !errors.Is(err, errCertificateIssuanceBusy) && !errors.Is(err, context.Canceled) {
			log.Printf("[edge-certificate] automatic renewal failed: %v", err)
		}
	}
	go check()
	ticker := time.NewTicker(panelCertificateRenewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			check()
		case <-ctx.Done():
			return
		}
	}
}

// renewEdgeCertificatesIfDue provisions an independent certificate/key pair
// for every enrolled node. A missing, expiring, invalid, or hostname-mismatched
// pair is replaced in its own generation directory without touching panel TLS.
func renewEdgeCertificatesIfDue(ctx context.Context, db *DB, manager *panelCertificateManager) error {
	if db == nil || manager == nil {
		return nil
	}
	settings, err := db.PanelSettings()
	if err != nil {
		return err
	}
	if !settings.Configured || strings.TrimSpace(settings.RouteDomain) == "" || jwtSecretEphemeral || strings.TrimSpace(settings.ACMEEmail) == "" || strings.TrimSpace(settings.ACMETokenCiphertext) == "" {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(settings.ACMEDNSProvider))
	if provider == "" {
		provider = "cloudflare"
	}
	if provider != "cloudflare" {
		return errors.New("automatic edge renewal supports Cloudflare DNS only")
	}
	token, err := decryptPanelACMEToken(settings.ACMETokenCiphertext)
	if err != nil {
		return errors.New("无法解密已保存的 DNS API Token")
	}
	nodes, err := db.listControlNodes(time.Now())
	if err != nil {
		return err
	}
	var failures []error
	for _, node := range nodes {
		if !edgeCertificateRequired(node) || strings.TrimSpace(node.GUID) == "" {
			continue
		}
		nodeCtx, cancel := context.WithTimeout(ctx, edgeCertificateRenewalTimeout)
		_, ensureErr := ensureEdgeCertificateForNode(nodeCtx, settings, token, manager, node)
		cancel()
		if ensureErr != nil {
			failures = append(failures, fmt.Errorf("node %s: %w", node.Name, ensureErr))
			log.Printf("[edge-certificate] node %s renewal failed: %v", node.Name, ensureErr)
		}
	}
	return errors.Join(failures...)
}

// ensureEdgeCertificateForNode provisions or renews exactly one enrolled
// node's certificate. A valid pair is reused, preventing an administrator
// retry or a scheduler tick from needlessly consuming ACME issuance quota.
func ensureEdgeCertificateForNode(ctx context.Context, settings PanelSettings, token string, manager *panelCertificateManager, node ControlNode) (bool, error) {
	if manager == nil || !edgeCertificateRequired(node) || strings.TrimSpace(node.GUID) == "" {
		return false, nil
	}
	certFile, keyFile, err := manager.nodeEdgeTLSPaths(node.GUID)
	if err != nil {
		return false, err
	}
	wildcard := wildcardDomainForSettings(settings)
	uniqueHost := edgeCertificateHost(settings.RouteDomain, node.GUID)
	status := certificateStatusForFile(certFile, wildcard)
	_, pairErr := tls.LoadX509KeyPair(certFile, keyFile)
	hostErr := certificateCoversHost(certFile, uniqueHost)
	if status.Configured && pairErr == nil && status.CertificateValid && status.CertificateCurrent && hostErr == nil && status.DaysRemaining > int(panelCertificateRenewalWindow.Hours()/24) {
		return false, nil
	}
	issued, issueErr := manager.issueCloudflareForIdentifiers(ctx, settings.ACMEEmail, token, settings.RouteDomain, edgeCertificateIdentifiers(settings.RouteDomain, node.GUID), settings.ACMEStaging)
	if issueErr != nil {
		return false, issueErr
	}
	if installErr := installCertificatePairAtomic(certFile, keyFile, issued.certPEM, issued.keyPEM); installErr != nil {
		return false, installErr
	}
	log.Printf("[edge-certificate] certificate provisioned for node %s (%s + %s)", node.Name, wildcard, uniqueHost)
	return true, nil
}

func provisionEdgeCertificateForNode(ctx context.Context, db *DB, manager *panelCertificateManager, node ControlNode) error {
	if db == nil || manager == nil || !edgeCertificateRequired(node) {
		return nil
	}
	settings, err := db.PanelSettings()
	if err != nil {
		return err
	}
	if !settings.Configured || strings.TrimSpace(settings.RouteDomain) == "" || jwtSecretEphemeral || strings.TrimSpace(settings.ACMEEmail) == "" || strings.TrimSpace(settings.ACMETokenCiphertext) == "" {
		return nil
	}
	token, err := decryptPanelACMEToken(settings.ACMETokenCiphertext)
	if err != nil {
		return errors.New("无法解密已保存的 DNS API Token")
	}
	_, err = ensureEdgeCertificateForNode(ctx, settings, token, manager, node)
	return err
}

// disableExpiredPanelTLSIfNeeded makes an expired certificate a recoverable
// configuration state. The certificate files are intentionally preserved;
// only the active database flag and marker are cleared before a restart.
func disableExpiredPanelTLSIfNeeded(db *DB, manager *panelCertificateManager) (bool, error) {
	if db == nil || manager == nil {
		return false, nil
	}
	settings, err := db.PanelSettings()
	if err != nil {
		return false, err
	}
	if !settings.TLSEnabled || !settings.Configured || settings.PanelDomain == "" || settings.RouteDomain == "" {
		return false, nil
	}
	status := manager.status(settings, settings.PanelDomain, settings.RouteDomain, settings.ListenPort, settings.TLSEnabled)
	if status.Issuing || !status.Configured || status.CertificateValid {
		return false, nil
	}
	if err := db.SetPanelTLSEnabled(false); err != nil {
		return false, fmt.Errorf("disable expired panel TLS in settings: %w", err)
	}
	if err := manager.disable(); err != nil {
		return false, fmt.Errorf("disable expired panel TLS marker: %w", err)
	}
	log.Printf("[panel-certificate] certificate for *.%s expired; HTTPS disabled and HTTP fallback requested", settings.RouteDomain)
	return true, nil
}

func renewPanelCertificateIfDue(ctx context.Context, db *DB, manager *panelCertificateManager) error {
	settings, err := db.PanelSettings()
	if err != nil {
		return err
	}
	if !settings.TLSEnabled || !settings.Configured || settings.PanelDomain == "" || settings.RouteDomain == "" {
		return nil
	}
	if jwtSecretEphemeral || strings.TrimSpace(settings.ACMEEmail) == "" || strings.TrimSpace(settings.ACMETokenCiphertext) == "" {
		return nil
	}
	status := manager.status(settings, settings.PanelDomain, settings.RouteDomain, settings.ListenPort, settings.TLSEnabled)
	if !certificateNeedsRenewal(status) {
		return nil
	}
	token, err := decryptPanelACMEToken(settings.ACMETokenCiphertext)
	if err != nil {
		return errors.New("无法解密已保存的 DNS API Token")
	}
	provider := strings.ToLower(strings.TrimSpace(settings.ACMEDNSProvider))
	if provider == "" {
		provider = "cloudflare"
	}
	if provider != "cloudflare" {
		return errors.New("自动续签暂仅支持 Cloudflare DNS")
	}
	issued, err := manager.issueCloudflare(ctx, settings.ACMEEmail, token, settings.PanelDomain, settings.RouteDomain, settings.ACMEStaging)
	if err != nil {
		return err
	}
	backup, err := manager.backupInstalledFiles()
	if err != nil {
		return err
	}
	if err := manager.install(issued, true); err != nil {
		_ = manager.restoreInstalledFiles(backup)
		return err
	}
	log.Printf("[panel-certificate] wildcard certificate renewed for *.%s", settings.RouteDomain)
	return nil
}
