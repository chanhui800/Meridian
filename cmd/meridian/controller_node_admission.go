package main

import (
	"database/sql"
	"fmt"
	"net/http"
)

// controllerNodeAdmissionSnapshot is immutable after publication. Ownership is
// the last committed applied node, including disabled schedules whose remote
// cleanup is still pending; desired-node/readiness changes must not reopen the
// Controller once a site has been handed off.
type controllerNodeAdmissionSnapshot struct {
	owners map[int64]int64
}

type controllerNodeAdmissionQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

// siteAppliedEntry is presentation metadata for the committed entry. Desired
// assignments never appear here, and cleanup keeps the old entry until its
// applied ownership has been cleared.
type siteAppliedEntry struct {
	NodeID int64
	Port   int
}

func (d *DB) siteAppliedEntries() (map[int64]siteAppliedEntry, error) {
	rows, err := d.db.Query(`SELECT site_id,applied_node_id,applied_node_port
		FROM site_node_schedules WHERE applied_node_id>0`)
	if err != nil {
		return nil, fmt.Errorf("load applied site entries: %w", err)
	}
	defer rows.Close()
	entries := make(map[int64]siteAppliedEntry)
	for rows.Next() {
		var siteID int64
		var entry siteAppliedEntry
		if err := rows.Scan(&siteID, &entry.NodeID, &entry.Port); err != nil {
			return nil, err
		}
		entries[siteID] = entry
	}
	return entries, rows.Err()
}

func readControllerNodeAdmission(queryer controllerNodeAdmissionQueryer) (*controllerNodeAdmissionSnapshot, error) {
	rows, err := queryer.Query("SELECT site_id,applied_node_id FROM site_node_schedules WHERE applied_node_id>0")
	if err != nil {
		return nil, fmt.Errorf("load controller node admission: %w", err)
	}
	defer rows.Close()
	next := &controllerNodeAdmissionSnapshot{owners: make(map[int64]int64)}
	for rows.Next() {
		var siteID, nodeID int64
		if err := rows.Scan(&siteID, &nodeID); err != nil {
			return nil, fmt.Errorf("load controller node admission: %w", err)
		}
		next.owners[siteID] = nodeID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load controller node admission: %w", err)
	}
	return next, nil
}

// commitControllerNodeAdmission reads the replacement ownership state from the
// same transaction that changes applied_node_id. It publishes only after a
// successful commit, before the mutation returns. Serializing publication keeps
// an older commit from overwriting a newer snapshot after SQLite releases its
// connection. The transaction already owns that connection before taking this
// lock; no writer takes this lock while waiting to open its transaction.
func (d *DB) commitControllerNodeAdmission(tx *sql.Tx) error {
	d.controllerNodeAdmissionMu.Lock()
	defer d.controllerNodeAdmissionMu.Unlock()
	next, err := readControllerNodeAdmission(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.controllerNodeAdmission.Store(next)
	return nil
}

func (d *DB) controllerNodeOwnsSite(siteID int64) bool {
	// Agent bundles use ephemeral databases and the same proxy handler. Their
	// local routes must remain admissible regardless of Controller ownership.
	if d == nil || d.edgeEphemeral || siteID <= 0 {
		return false
	}
	snapshot := d.controllerNodeAdmission.Load()
	return snapshot != nil && snapshot.owners[siteID] > 0
}

func (d *DB) rejectControllerNodeAdmission(w http.ResponseWriter, siteID int64) bool {
	if !d.controllerNodeOwnsSite(siteID) {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMisdirectedRequest)
	_, _ = w.Write([]byte(`{"error":"site is served by a scheduled node","code":"site_node_owned"}`))
	return true
}
