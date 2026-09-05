package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const acmeIssueLeaseTTL = 10 * time.Minute

// acquireACMELock obtains the process-shared ACME lease.  The lease lives in
// SQLite so web requests, the renewal scheduler, and the administrative CLI
// all serialize issuance even when they run in different processes.
func (d *DB) acquireACMELock(ctx context.Context, owner string, ttl time.Duration) error {
	if d == nil || d.db == nil {
		return errors.New("ACME lock database is unavailable")
	}
	if strings.TrimSpace(owner) == "" {
		return errors.New("ACME lock owner is empty")
	}
	if ttl <= 0 {
		ttl = acmeIssueLeaseTTL
	}
	for {
		now := time.Now()
		result, err := d.db.ExecContext(ctx, `UPDATE acme_issue_lock
			SET owner=?, expires_at_ms=?
			WHERE id=1 AND (expires_at_ms < ? OR owner=?)`, owner,
			now.Add(ttl).UnixMilli(), now.UnixMilli(), owner)
		if err == nil {
			if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
				return rowsErr
			} else if affected == 1 {
				return nil
			}
		} else if !isSQLiteBusyError(err) {
			return fmt.Errorf("acquire ACME lock: %w", err)
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (d *DB) releaseACMELock(owner string) error {
	if d == nil || d.db == nil || strings.TrimSpace(owner) == "" {
		return nil
	}
	_, err := d.db.Exec(`UPDATE acme_issue_lock SET owner='', expires_at_ms=0 WHERE id=1 AND owner=?`, owner)
	return err
}

func newACMELockOwner() string {
	return fmt.Sprintf("meridian-%d-%d", os.Getpid(), time.Now().UnixNano())
}
