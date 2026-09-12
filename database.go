package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	db                 *sql.DB
	dbPath             string
	edgeEphemeral      bool
	edgeRequestLogSink func(requestLogEvent)
	edgeTelemetrySink  func(edgeTelemetryEvent)
	// edgeWatchHistorySink forwards watch-history events from an Agent's
	// ephemeral proxy database into the durable Agent event spool.  Controller
	// databases leave this nil and use the normal asynchronous writer below.
	edgeWatchHistorySink func(watchHistoryEvent) bool

	dynamicObservationQueue     chan dynamicObservationCommand
	dynamicObservationDone      chan struct{}
	watchHistoryInboxWake       chan struct{}
	dynamicObservationGate      sync.RWMutex
	dynamicObservationCloseOnce sync.Once
	dynamicObservationClosed    atomic.Bool
	droppedDynamicObservations  atomic.Uint64
	droppedRequestLogs          atomic.Uint64
	droppedWatchHistory         atomic.Uint64
	agentSecurityRejected       atomic.Uint64
	agentSecurityRequestEvent   atomic.Uint64
	agentSecuritySiteStat       atomic.Uint64
	agentSecurityMediaCount     atomic.Uint64
	agentSecurityRetention      atomic.Uint64
	agentSecurityObservation    atomic.Uint64
	agentSecurityLastRejectedMS atomic.Int64
	agentSecurityMu             sync.Mutex
	agentSecurityLastLog        map[string]time.Time
	lastTrafficPruneMS          atomic.Int64
	installUUID                 string
	systemSettings              atomic.Pointer[SystemSettings]
	nodeTLSMutationMu           sync.Mutex
	watchHistoryMetadataMu      sync.Mutex
	watchHistoryMetadata        map[watchHistoryMetadataKey]watchHistoryMetadataEntry
	agentLiveMu                 sync.RWMutex
	agentLive                   map[nodeLiveTrafficKey]nodeLiveTrafficState
}

func openDB(path string) (*DB, error) {
	setSecureFileCreationMask()
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	d := &DB{db: sqlDB, dbPath: path, agentSecurityLastLog: make(map[string]time.Time), agentLive: make(map[nodeLiveTrafficKey]nodeLiveTrafficState)}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := d.ensureInstallationUUID(); err != nil {
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
	d.dynamicObservationQueue = make(chan dynamicObservationCommand, dynamicObservationQueueCapacity+requestLogQueueCapacity+watchHistoryQueueCapacity)
	d.dynamicObservationDone = make(chan struct{})
	d.watchHistoryInboxWake = make(chan struct{}, 1)
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
		d.agentLiveMu.Lock()
		d.agentLive = nil
		d.agentLiveMu.Unlock()
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

// ensureInstallationUUID loads or creates the random identity that scopes
// Cloudflare DNS ownership markers to this Meridian installation, so two
// controllers sharing one zone can never treat each other's records as their
// own.
func (d *DB) ensureInstallationUUID() error {
	if d == nil || d.db == nil {
		return errors.New("database is unavailable")
	}
	var value string
	err := d.db.QueryRow("SELECT install_uuid FROM installation_meta WHERE id=1").Scan(&value)
	if err == nil && strings.TrimSpace(value) != "" {
		d.installUUID = strings.TrimSpace(value)
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	value = hex.EncodeToString(raw)
	if _, err := d.db.Exec("INSERT OR IGNORE INTO installation_meta(id, install_uuid) VALUES(1,?)", value); err != nil {
		return err
	}
	if err := d.db.QueryRow("SELECT install_uuid FROM installation_meta WHERE id=1").Scan(&value); err != nil {
		return err
	}
	d.installUUID = strings.TrimSpace(value)
	return nil
}

// rotateInstallationUUID replaces the installation identity and returns the
// new value. It exists because restoring a backup copies installation_meta
// along with every other table: a disaster-recovery takeover must keep the
// original identity so it can keep managing the DNS records that controller
// created, while a controller cloned from a backup must adopt a new one or the
// two installations would present the same Cloudflare ownership marker and
// each would treat the other's records as its own.
//
// Note that this only updates the cached value for the calling process. A
// running panel reads installUUID once at startup, so an operator who rotates
// the identity through the CLI must restart the service.
func (d *DB) rotateInstallationUUID() (string, error) {
	if d == nil || d.db == nil {
		return "", errors.New("database is unavailable")
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	value := hex.EncodeToString(raw)
	if _, err := d.db.Exec("INSERT INTO installation_meta(id, install_uuid) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET install_uuid=excluded.install_uuid", value); err != nil {
		return "", err
	}
	var stored string
	if err := d.db.QueryRow("SELECT install_uuid FROM installation_meta WHERE id=1").Scan(&stored); err != nil {
		return "", err
	}
	stored = strings.TrimSpace(stored)
	if stored != value {
		return "", errors.New("installation identity was not updated")
	}
	d.installUUID = stored
	return stored, nil
}
