package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTLSBackupPinsSnapshotNodeCertificatesBeforeConcurrentDeletion(t *testing.T) {
	for _, deleteDuringExport := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable inventory", true: "delete waits until TLS bytes are pinned"}[deleteDuringExport], func(t *testing.T) {
			for _, name := range []string{"TLS_STATE_DIR", "PANEL_TLS_CERT_FILE", "PANEL_TLS_KEY_FILE", "EDGE_TLS_CERT_FILE", "EDGE_TLS_KEY_FILE"} {
				t.Setenv(name, "")
			}
			app := newTestApp(t)
			app.dbPath = app.db.dbPath
			app.panelCertificates = newPanelCertificateManager(app.dbPath, nil)
			if _, err := app.db.CreateInitialUser("backup-inventory-admin", "synthetic-backup-inventory-password"); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "backup-inventory-node", Address: "203.0.113.61", Port: 19443}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
				t.Fatal(err)
			}
			site, err := app.db.CreateSiteRecord(Site{Name: "backup-inventory-site", ListenPort: 18443, PublicHost: "backup-inventory.example.test",
				IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", StreamHosts: "[]", UAMode: passthroughUAMode})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.db.Exec(`INSERT INTO site_node_schedules(site_id,enabled,mode,fixed_node_id,desired_node_id,applied_node_id,applied_node_port,dns_status,created_at_ms,updated_at_ms)
				VALUES(?,1,'fixed',?,?,?,?, 'active',?,?)`, site.ID, node.ID, node.ID, node.ID, node.Port, now.UnixMilli(), now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			certificate, privateKey := reviewCertificatePEM(t)
			certPath, keyPath, err := app.panelCertificates.nodeEdgeTLSPaths(node.GUID)
			if err != nil {
				t.Fatal(err)
			}
			if err := installCertificatePairAtomic(certPath, keyPath, []byte(certificate), []byte(privateKey)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := app.readNodeEdgeCertificatePair(node.GUID); err != nil {
				t.Fatal(err)
			}

			const password = "snapshot-node-inventory-test-password"
			type backupResult struct {
				artifact builtBackupFile
				err      error
			}
			backupDone := make(chan backupResult, 1)
			certificatePairFilesMu.Lock()
			fileLockHeld := true
			defer func() {
				if fileLockHeld {
					certificatePairFilesMu.Unlock()
				}
			}()
			go func() {
				artifact, err := app.buildBackupToFile(password, true)
				backupDone <- backupResult{artifact: artifact, err: err}
			}()

			deadline := time.Now().Add(5 * time.Second)
			snapshotFound := false
			for time.Now().Before(deadline) {
				candidates, err := filepath.Glob(filepath.Join(filepath.Dir(app.dbPath), ".meridian-backup-*", backupDatabaseEntry))
				if err != nil {
					t.Fatal(err)
				}
				for _, candidate := range candidates {
					snapshotDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(candidate)+"?mode=ro&_pragma=query_only(1)")
					if err != nil {
						continue
					}
					var count int
					err = snapshotDB.QueryRow("SELECT COUNT(*) FROM control_nodes WHERE guid=?", node.GUID).Scan(&count)
					_ = snapshotDB.Close()
					if err == nil && count == 1 {
						snapshotFound = true
						break
					}
				}
				if snapshotFound {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !snapshotFound {
				t.Fatal("TLS export did not create a snapshot containing the node")
			}

			deleted := make(chan error, 1)
			if deleteDuringExport {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := app.prepareNodeDeletion(ctx, node.ID); err != nil {
						deleted <- err
						return
					}
					deleted <- app.db.DeleteControlNode(node.ID)
				}()
				// Confirm the scheduler has prepared deletion but the TLS mutation
				// barrier keeps the node row/certificate alive while backup pins it.
				prepareDeadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(prepareDeadline) {
					var enabled int
					queryErr := app.db.db.QueryRow("SELECT enabled FROM site_node_schedules WHERE site_id=?", site.ID).Scan(&enabled)
					if queryErr == nil && enabled == 0 {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				select {
				case err := <-deleted:
					t.Fatalf("node delete crossed backup snapshot barrier early: %v", err)
				case <-time.After(25 * time.Millisecond):
				}
				if _, err := app.db.controlNodeByID(node.ID, time.Now()); err != nil {
					t.Fatalf("node identity was deleted before TLS bytes were pinned: %v", err)
				}
			}
			certificatePairFilesMu.Unlock()
			fileLockHeld = false
			var result backupResult
			select {
			case result = <-backupDone:
			case <-time.After(15 * time.Second):
				t.Fatal("TLS backup did not finish after pair lock release")
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.artifact.Cleanup != nil {
				defer result.artifact.Cleanup()
			}
			if deleteDuringExport {
				select {
				case err := <-deleted:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("node deletion did not resume after certificate bytes were pinned")
				}
			}
			if _, err := os.Stat(certPath); deleteDuringExport && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("concurrent node deletion left TLS path error=%v, want file removal", err)
			}
			sealed, err := os.ReadFile(result.artifact.Path)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := openBackup(sealed, password)
			if err != nil {
				t.Fatal(err)
			}
			manifest, entries, err := parseBackupArchive(plain)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := entries[backupTLSEdgeNodesPrefix+node.GUID+"/fullchain.pem"]; !ok {
				t.Fatal("snapshot node certificate is missing from the successful TLS archive")
			}
			if _, ok := entries[backupTLSEdgeNodesPrefix+node.GUID+"/privkey.pem"]; !ok {
				t.Fatal("snapshot node private key is missing from the successful TLS archive")
			}
			restoredPath := filepath.Join(t.TempDir(), "restored.db")
			restoredDB, err := openDB(restoredPath)
			if err != nil {
				t.Fatal(err)
			}
			restoredDB.Close()
			if _, err := writeRestorePending(restoredPath, manifest, entries, jwtSecret, app.pm.upstreamHeaderKey, nil, true); err != nil {
				t.Fatal(err)
			}
			if _, err := applyPendingRestore(restoredPath); err != nil {
				t.Fatal(err)
			}
			restoredDB, err = openDB(restoredPath)
			if err != nil {
				t.Fatal(err)
			}
			defer restoredDB.Close()
			restoredApp := &App{db: restoredDB, dbPath: restoredPath, panelCertificates: newPanelCertificateManager(restoredPath, nil)}
			if _, err := restoredDB.controlNodeByID(node.ID, time.Now()); err != nil {
				t.Fatalf("restored TLS backup lost snapshot node: %v", err)
			}
			if _, _, err := restoredApp.readNodeEdgeCertificatePair(node.GUID); err != nil {
				t.Fatalf("restored TLS backup cannot configure the snapshot node: %v", err)
			}
			if err := finalizeAppliedRestore(restoredPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}
