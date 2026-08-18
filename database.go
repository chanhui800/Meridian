package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type DB struct {
	db *sql.DB

	dynamicObservationQueue     chan dynamicObservationCommand
	dynamicObservationDone      chan struct{}
	dynamicObservationGate      sync.RWMutex
	dynamicObservationCloseOnce sync.Once
	dynamicObservationClosed    atomic.Bool
	droppedDynamicObservations  atomic.Uint64
	droppedRequestLogs          atomic.Uint64
	systemSettings              atomic.Pointer[SystemSettings]
}

func openDB(path string) (*DB, error) {
	setSecureFileCreationMask()
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	d := &DB{db: sqlDB}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	settings, err := d.loadSystemSettings()
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("load system settings: %w", err)
	}
	d.systemSettings.Store(&settings)
	configureProbeClient(time.Duration(settings.ProbeTimeoutMS) * time.Millisecond)
	if err := hardenDatabaseFilePermissions(path); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := d.validateStoredDynamicPolicies(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("validate stored dynamic policies: %w", err)
	}
	d.dynamicObservationQueue = make(chan dynamicObservationCommand, dynamicObservationQueueCapacity+requestLogQueueCapacity)
	d.dynamicObservationDone = make(chan struct{})
	go d.runDynamicObservationWriter()
	return d, nil
}

// warnUnenforcedFileModes keeps the platform warning to one line per process
// instead of one per openDB call.
var warnUnenforcedFileModes sync.Once

func hardenDatabaseFilePermissions(path string) error {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	if !fileModesEnforced() {
		// Chmod would report success and change nothing, which is worse than not
		// trying: it would let the operator believe the database is protected.
		warnUnenforcedFileModes.Do(func() {
			log.Printf("This platform does not enforce POSIX file modes, so %s keeps whatever permissions it inherits from its directory. That file holds the administrator password hash and every configured upstream URL: restrict the directory yourself and do not leave it somewhere other local users can read.", path)
		})
		return nil
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		// #nosec G703 -- the database path is operator-controlled and never derived from a request.
		if err := os.Chmod(candidate, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure database file %s: %w", candidate, err)
		}
	}
	return nil
}

func (d *DB) Close() {
	if d == nil {
		return
	}
	d.dynamicObservationCloseOnce.Do(func() {
		if d.dynamicObservationQueue != nil {
			result := make(chan error, 1)
			d.dynamicObservationGate.Lock()
			d.dynamicObservationClosed.Store(true)
			d.dynamicObservationGate.Unlock()
			d.dynamicObservationQueue <- dynamicObservationCommand{kind: dynamicObservationCommandStop, result: result}
			<-result
			<-d.dynamicObservationDone
		}
		_ = d.db.Close()
	})
}

func sqliteBool(value bool) int {
	if value {
		return 1
	}
	return 0
}
