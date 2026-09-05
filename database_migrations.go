package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	migrationRetryDelay    = 25 * time.Millisecond
	migrationRetryDeadline = 5 * time.Second
)

func (d *DB) migrate() error {
	deadline := time.Now().Add(migrationRetryDeadline)
	for {
		err := d.migrateOnce()
		if err == nil || !isSQLiteBusyError(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(migrationRetryDelay)
	}
}

func isSQLiteBusyError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	// SQLite encodes the primary result code in the low byte of extended errors.
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	default:
		return false
	}
}

func isSQLiteUniqueConstraintError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return true
	default:
		return false
	}
}

func (d *DB) migrateOnce() error {
	ctx := context.Background()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	if _, err := conn.ExecContext(ctx, `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		session_version INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
		CREATE TABLE IF NOT EXISTS sites (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sort_order INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL,
			listen_port INTEGER NOT NULL UNIQUE,
			public_host TEXT NOT NULL DEFAULT '',
			path_prefix TEXT NOT NULL DEFAULT '',
			ingress_mode TEXT NOT NULL DEFAULT 'port',
			target_url TEXT NOT NULL,
		primary_line_name TEXT NOT NULL DEFAULT '主线路',
		playback_target_url TEXT NOT NULL DEFAULT '',
		playback_mode TEXT NOT NULL DEFAULT 'direct',
		main_video_stream_mode TEXT NOT NULL DEFAULT 'proxy',
		failover_targets TEXT NOT NULL DEFAULT '[]',
		failover_lines TEXT NOT NULL DEFAULT '[]',
		stream_hosts TEXT NOT NULL DEFAULT '[]',
		ua_mode TEXT DEFAULT 'passthrough',
		custom_user_agent TEXT NOT NULL DEFAULT '',
		custom_client TEXT NOT NULL DEFAULT '',
		custom_version TEXT NOT NULL DEFAULT '',
		client_ip_mode TEXT NOT NULL DEFAULT 'both',
		upstream_headers TEXT NOT NULL DEFAULT '[]',
		dynamic_discovery_enabled INTEGER NOT NULL DEFAULT 1,
		dynamic_profile TEXT NOT NULL DEFAULT 'compatible',
		dynamic_discovery_sources TEXT NOT NULL DEFAULT '["redirect","playback_info"]',
		dynamic_domain_rules TEXT NOT NULL DEFAULT '[]',
		dynamic_allow_https_downgrade INTEGER NOT NULL DEFAULT 1,
		dynamic_policy_revision INTEGER NOT NULL DEFAULT 1,
		asset_cache_enabled INTEGER NOT NULL DEFAULT 0,
		asset_cache_ttl_sec INTEGER NOT NULL DEFAULT 86400,
		asset_cache_max_bytes BIGINT NOT NULL DEFAULT 536870912,
		asset_cache_rules TEXT NOT NULL DEFAULT '*/file/*\n*/emby/Items/*/Images/*',
		watch_history_enabled INTEGER NOT NULL DEFAULT 0,
		account_retention_days INTEGER NOT NULL DEFAULT 0,
		account_retention_started_at_ms INTEGER NOT NULL DEFAULT 0,
		account_retention_last_completed_at_ms INTEGER NOT NULL DEFAULT 0,
		media_movie_count INTEGER NOT NULL DEFAULT -1,
		media_series_count INTEGER NOT NULL DEFAULT -1,
		media_episode_count INTEGER NOT NULL DEFAULT -1,
		media_count_updated_at_ms INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER DEFAULT 1,
		traffic_quota BIGINT DEFAULT 0,
		traffic_used BIGINT DEFAULT 0,
		traffic_used_in BIGINT NOT NULL DEFAULT 0,
		traffic_used_out BIGINT NOT NULL DEFAULT 0,
		speed_limit INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS traffic_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		bytes_in BIGINT DEFAULT 0,
		bytes_out BIGINT DEFAULT 0,
		requests BIGINT NOT NULL DEFAULT 0,
		recorded_at DATETIME NOT NULL,
		recorded_at_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_traffic_site_time ON traffic_logs(site_id, recorded_at);
	CREATE TABLE IF NOT EXISTS request_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		site_name TEXT NOT NULL,
		resource_category TEXT NOT NULL,
		status_code INTEGER NOT NULL,
		client_ip TEXT NOT NULL,
		user_agent TEXT NOT NULL,
		upstream_user_agent TEXT NOT NULL DEFAULT '',
		backend_address TEXT NOT NULL DEFAULT '',
		inbound_colo TEXT NOT NULL DEFAULT '',
		outbound_colo TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL,
		path TEXT NOT NULL,
		recorded_at_ms INTEGER NOT NULL,
		timeline_at_ms INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_request_logs_time ON request_logs(recorded_at_ms DESC, id DESC);
	CREATE INDEX IF NOT EXISTS idx_request_logs_category_status ON request_logs(resource_category, status_code, recorded_at_ms DESC);
	CREATE INDEX IF NOT EXISTS idx_request_logs_site_time ON request_logs(site_id, recorded_at_ms DESC, id DESC);
	CREATE TABLE IF NOT EXISTS media_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		upstream_item_id TEXT NOT NULL,
		media_type TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		original_title TEXT NOT NULL DEFAULT '',
		production_year INTEGER NOT NULL DEFAULT 0,
		series_name TEXT NOT NULL DEFAULT '',
		season_number INTEGER NOT NULL DEFAULT -1,
		episode_number INTEGER NOT NULL DEFAULT -1,
		tmdb_type TEXT NOT NULL DEFAULT '',
		tmdb_id INTEGER NOT NULL DEFAULT 0,
		imdb_id TEXT NOT NULL DEFAULT '',
		tvdb_id TEXT NOT NULL DEFAULT '',
		overview TEXT NOT NULL DEFAULT '',
		poster_path TEXT NOT NULL DEFAULT '',
		cast_json TEXT NOT NULL DEFAULT '[]',
		details_version INTEGER NOT NULL DEFAULT 0,
		backdrop_path TEXT NOT NULL DEFAULT '',
		release_date TEXT NOT NULL DEFAULT '',
		vote_average REAL NOT NULL DEFAULT 0,
		genres_json TEXT NOT NULL DEFAULT '[]',
		status TEXT NOT NULL DEFAULT '',
		last_air_date TEXT NOT NULL DEFAULT '',
		next_air_date TEXT NOT NULL DEFAULT '',
		next_season_number INTEGER NOT NULL DEFAULT -1,
		next_episode_number INTEGER NOT NULL DEFAULT -1,
		next_episode_name TEXT NOT NULL DEFAULT '',
		season_count INTEGER NOT NULL DEFAULT 0,
		episode_count INTEGER NOT NULL DEFAULT 0,
		stills_json TEXT NOT NULL DEFAULT '[]',
		match_status TEXT NOT NULL DEFAULT 'pending',
		metadata_updated_at_ms INTEGER NOT NULL DEFAULT 0,
		created_at_ms INTEGER NOT NULL,
		updated_at_ms INTEGER NOT NULL,
		UNIQUE(site_id, upstream_item_id)
	);
	CREATE INDEX IF NOT EXISTS idx_media_items_site_updated ON media_items(site_id, updated_at_ms DESC, id DESC);
	CREATE TABLE IF NOT EXISTS watch_sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
		session_hash TEXT NOT NULL,
		started_at_ms INTEGER NOT NULL,
		last_seen_at_ms INTEGER NOT NULL,
		stopped_at_ms INTEGER NOT NULL DEFAULT 0,
		position_ticks INTEGER NOT NULL DEFAULT 0,
		runtime_ticks INTEGER NOT NULL DEFAULT 0,
		play_method TEXT NOT NULL DEFAULT '',
		completed INTEGER NOT NULL DEFAULT 0,
		user_name TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL DEFAULT '',
		device_id TEXT NOT NULL DEFAULT '',
		device_name TEXT NOT NULL DEFAULT '',
		client_name TEXT NOT NULL DEFAULT '',
		play_session_id TEXT NOT NULL DEFAULT '',
		token_ciphertext TEXT NOT NULL DEFAULT '',
		UNIQUE(site_id, session_hash)
	);
	CREATE INDEX IF NOT EXISTS idx_watch_sessions_time ON watch_sessions(last_seen_at_ms DESC, id DESC);
	CREATE INDEX IF NOT EXISTS idx_watch_sessions_site_time ON watch_sessions(site_id, last_seen_at_ms DESC, id DESC);
	CREATE INDEX IF NOT EXISTS idx_watch_sessions_media_time ON watch_sessions(media_item_id, last_seen_at_ms DESC);
	CREATE TABLE IF NOT EXISTS tmdb_jobs (
		media_item_id INTEGER PRIMARY KEY REFERENCES media_items(id) ON DELETE CASCADE,
		state TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at_ms INTEGER NOT NULL DEFAULT 0,
		lease_until_ms INTEGER NOT NULL DEFAULT 0,
		last_error_code TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL DEFAULT 0,
		updated_at_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_tmdb_jobs_due ON tmdb_jobs(state, next_attempt_at_ms, lease_until_ms);
	CREATE TABLE IF NOT EXISTS tmdb_cache (
		tmdb_type TEXT NOT NULL,
		tmdb_id INTEGER NOT NULL,
		language TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		original_title TEXT NOT NULL DEFAULT '',
		overview TEXT NOT NULL DEFAULT '',
		release_year INTEGER NOT NULL DEFAULT 0,
		poster_path TEXT NOT NULL DEFAULT '',
		cast_json TEXT NOT NULL DEFAULT '[]',
		details_version INTEGER NOT NULL DEFAULT 0,
		backdrop_path TEXT NOT NULL DEFAULT '',
		release_date TEXT NOT NULL DEFAULT '',
		vote_average REAL NOT NULL DEFAULT 0,
		genres_json TEXT NOT NULL DEFAULT '[]',
		status TEXT NOT NULL DEFAULT '',
		last_air_date TEXT NOT NULL DEFAULT '',
		next_air_date TEXT NOT NULL DEFAULT '',
		next_season_number INTEGER NOT NULL DEFAULT -1,
		next_episode_number INTEGER NOT NULL DEFAULT -1,
		next_episode_name TEXT NOT NULL DEFAULT '',
		season_count INTEGER NOT NULL DEFAULT 0,
		episode_count INTEGER NOT NULL DEFAULT 0,
		stills_json TEXT NOT NULL DEFAULT '[]',
		updated_at_ms INTEGER NOT NULL,
		expires_at_ms INTEGER NOT NULL,
		PRIMARY KEY(tmdb_type, tmdb_id, language)
	) WITHOUT ROWID;
	CREATE INDEX IF NOT EXISTS idx_tmdb_cache_expiry ON tmdb_cache(expires_at_ms);
	CREATE TABLE IF NOT EXISTS tmdb_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		enabled INTEGER NOT NULL DEFAULT 0,
		token_ciphertext TEXT NOT NULL DEFAULT '',
		language TEXT NOT NULL DEFAULT 'zh-CN',
		history_retention_days INTEGER NOT NULL DEFAULT 90,
		credential_state TEXT NOT NULL DEFAULT 'unconfigured',
		last_error_code TEXT NOT NULL DEFAULT '',
		last_tested_at_ms INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	INSERT OR IGNORE INTO tmdb_settings (id) VALUES (1);
	CREATE TABLE IF NOT EXISTS panel_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		panel_domain TEXT NOT NULL DEFAULT '',
		route_domain TEXT NOT NULL DEFAULT '',
		listen_port INTEGER NOT NULL DEFAULT 0,
		tls_enabled INTEGER NOT NULL DEFAULT 0,
		configured INTEGER NOT NULL DEFAULT 0,
		acme_email TEXT NOT NULL DEFAULT '',
		acme_dns_provider TEXT NOT NULL DEFAULT 'cloudflare',
		acme_token_ciphertext TEXT NOT NULL DEFAULT '',
		acme_staging INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	INSERT OR IGNORE INTO panel_settings (id) VALUES (1);
	CREATE TABLE IF NOT EXISTS telegram_report_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		enabled INTEGER NOT NULL DEFAULT 0,
		bot_token_ciphertext TEXT NOT NULL DEFAULT '',
		chat_id TEXT NOT NULL DEFAULT '',
		schedule_time TEXT NOT NULL DEFAULT '20:00',
		frequency TEXT NOT NULL DEFAULT 'daily',
		weekday INTEGER NOT NULL DEFAULT 1,
		last_sent_key TEXT NOT NULL DEFAULT '',
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	INSERT OR IGNORE INTO telegram_report_settings (id) VALUES (1);
	CREATE TABLE IF NOT EXISTS system_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		ui_mode TEXT NOT NULL DEFAULT 'novice', ui_radius INTEGER NOT NULL DEFAULT 10,
		traffic_billing_mode TEXT NOT NULL DEFAULT 'bidirectional', traffic_reset_day INTEGER NOT NULL DEFAULT 1,
		probe_timeout_ms INTEGER NOT NULL DEFAULT 5000, ping_cache_minutes INTEGER NOT NULL DEFAULT 10,
		schedule_timezone_offset INTEGER NOT NULL DEFAULT 480,
		log_enabled INTEGER NOT NULL DEFAULT 1, log_level TEXT NOT NULL DEFAULT 'info',
		log_retention_days INTEGER NOT NULL DEFAULT 30, log_write_delay_minutes INTEGER NOT NULL DEFAULT 0,
		log_flush_threshold INTEGER NOT NULL DEFAULT 1, log_batch_size INTEGER NOT NULL DEFAULT 50,
		log_retry_count INTEGER NOT NULL DEFAULT 2, log_retry_backoff_ms INTEGER NOT NULL DEFAULT 75,
		log_task_lease_ms INTEGER NOT NULL DEFAULT 300000,
		log_write_image INTEGER NOT NULL DEFAULT 0, log_write_playback INTEGER NOT NULL DEFAULT 1, log_write_metadata INTEGER NOT NULL DEFAULT 0,
		log_write_video INTEGER NOT NULL DEFAULT 1, log_write_api INTEGER NOT NULL DEFAULT 1, log_write_auth INTEGER NOT NULL DEFAULT 1,
		log_write_subtitle INTEGER NOT NULL DEFAULT 1, log_write_asset INTEGER NOT NULL DEFAULT 1, log_write_websocket INTEGER NOT NULL DEFAULT 1,
		log_resource_taxonomy_version INTEGER NOT NULL DEFAULT 1,
		log_write_node INTEGER NOT NULL DEFAULT 1, log_write_category INTEGER NOT NULL DEFAULT 1, log_write_status INTEGER NOT NULL DEFAULT 1,
		log_write_client_ip INTEGER NOT NULL DEFAULT 1, log_write_colo INTEGER NOT NULL DEFAULT 0,
		log_write_ua INTEGER NOT NULL DEFAULT 1, log_write_upstream_ua INTEGER NOT NULL DEFAULT 1, log_write_backend_address INTEGER NOT NULL DEFAULT 1, log_write_timeline INTEGER NOT NULL DEFAULT 1, log_display_client_ip INTEGER NOT NULL DEFAULT 1,
		log_display_colo INTEGER NOT NULL DEFAULT 0, log_display_ua INTEGER NOT NULL DEFAULT 1, log_display_upstream_ua INTEGER NOT NULL DEFAULT 1, log_display_backend_address INTEGER NOT NULL DEFAULT 1,
		log_display_node INTEGER NOT NULL DEFAULT 1, log_display_category INTEGER NOT NULL DEFAULT 1,
		log_display_status INTEGER NOT NULL DEFAULT 1, log_display_timeline INTEGER NOT NULL DEFAULT 1,
		log_search_mode TEXT NOT NULL DEFAULT 'like', updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	INSERT OR IGNORE INTO system_settings (id) VALUES (1);
	CREATE TABLE IF NOT EXISTS control_nodes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		guid TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL COLLATE NOCASE UNIQUE,
		address TEXT NOT NULL DEFAULT '',
		entry_mode TEXT NOT NULL DEFAULT 'direct' CHECK(entry_mode IN ('direct','shared')),
		http_port INTEGER NOT NULL DEFAULT 0,
		https_port INTEGER NOT NULL DEFAULT 443,
		enabled INTEGER NOT NULL DEFAULT 1,
		priority INTEGER NOT NULL DEFAULT 100,
		traffic_quota BIGINT NOT NULL DEFAULT 0,
		billing_mode TEXT NOT NULL DEFAULT 'outbound' CHECK(billing_mode IN ('outbound','bidirectional')),
		reset_day INTEGER NOT NULL DEFAULT 1 CHECK(reset_day BETWEEN 0 AND 31),
		cycle_started_at_ms INTEGER NOT NULL DEFAULT 0,
		period_rx_bytes BIGINT NOT NULL DEFAULT 0,
		period_tx_bytes BIGINT NOT NULL DEFAULT 0,
		lifetime_rx_bytes BIGINT NOT NULL DEFAULT 0,
		lifetime_tx_bytes BIGINT NOT NULL DEFAULT 0,
		last_raw_rx_bytes BIGINT NOT NULL DEFAULT 0,
		last_raw_tx_bytes BIGINT NOT NULL DEFAULT 0,
		last_boot_id TEXT NOT NULL DEFAULT '',
		last_report_session_id TEXT NOT NULL DEFAULT '',
		last_sequence BIGINT NOT NULL DEFAULT 0,
		interface_name TEXT NOT NULL DEFAULT '',
		agent_version TEXT NOT NULL DEFAULT '',
		desired_config_hash TEXT NOT NULL DEFAULT '',
		applied_config_hash TEXT NOT NULL DEFAULT '',
		agent_listener_error TEXT NOT NULL DEFAULT '',
		event_spool_error TEXT NOT NULL DEFAULT '',
		event_queue_depth INTEGER NOT NULL DEFAULT 0,
		event_dropped BIGINT NOT NULL DEFAULT 0,
		enrollment_token_hash TEXT NOT NULL DEFAULT '',
		enrollment_expires_at_ms INTEGER NOT NULL DEFAULT 0,
		agent_token_hash TEXT NOT NULL DEFAULT '',
		enrolled_at_ms INTEGER NOT NULL DEFAULT 0,
		last_seen_at_ms INTEGER NOT NULL DEFAULT 0,
		created_at_ms INTEGER NOT NULL,
		updated_at_ms INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_control_nodes_last_seen ON control_nodes(last_seen_at_ms DESC);
	CREATE INDEX IF NOT EXISTS idx_control_nodes_enrollment_token ON control_nodes(enrollment_token_hash);
	CREATE INDEX IF NOT EXISTS idx_control_nodes_agent_token ON control_nodes(agent_token_hash);
	CREATE TABLE IF NOT EXISTS node_scheduler_settings (
		id INTEGER PRIMARY KEY CHECK(id = 1),
		mode TEXT NOT NULL DEFAULT 'auto' CHECK(mode IN ('auto','manual')),
		manual_node_id INTEGER,
		active_node_id INTEGER,
		updated_at_ms INTEGER NOT NULL DEFAULT 0
	);
	INSERT OR IGNORE INTO node_scheduler_settings (id) VALUES (1);
	CREATE TABLE IF NOT EXISTS site_node_schedules (
		site_id INTEGER PRIMARY KEY,
		enabled INTEGER NOT NULL DEFAULT 0,
		mode TEXT NOT NULL DEFAULT 'global' CHECK(mode IN ('global','fixed')),
		fixed_node_id INTEGER,
		desired_node_id INTEGER,
		applied_node_id INTEGER,
		cf_zone_id TEXT NOT NULL DEFAULT '',
		cf_record_id TEXT NOT NULL DEFAULT '',
		cf_record_type TEXT NOT NULL DEFAULT '',
		applied_address TEXT NOT NULL DEFAULT '',
		dns_status TEXT NOT NULL DEFAULT 'disabled',
		config_hash TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		created_at_ms INTEGER NOT NULL DEFAULT 0,
		updated_at_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_site_node_schedules_desired ON site_node_schedules(desired_node_id,enabled);
	CREATE TABLE IF NOT EXISTS site_node_probe_failures (
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		node_id INTEGER NOT NULL REFERENCES control_nodes(id) ON DELETE CASCADE,
		failed_until_ms INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		updated_at_ms INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(site_id,node_id)
	);
	CREATE INDEX IF NOT EXISTS idx_site_node_probe_failures_until ON site_node_probe_failures(site_id,failed_until_ms);
	`); err != nil {
		return err
	}
	if err := ensureDynamicObservationSchema(ctx, conn); err != nil {
		return err
	}
	if err := validateDynamicObservationSchema(ctx, conn); err != nil {
		return err
	}
	var hasTMDBJobRevision int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('tmdb_jobs') WHERE name='revision'").Scan(&hasTMDBJobRevision); err != nil {
		return err
	}
	if hasTMDBJobRevision == 0 {
		if _, err := conn.ExecContext(ctx, "ALTER TABLE tmdb_jobs ADD COLUMN revision INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	var hasSessionVersion int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('users') WHERE name='session_version'").Scan(&hasSessionVersion); err != nil {
		return err
	}
	if hasSessionVersion == 0 {
		if _, err := conn.ExecContext(ctx, "ALTER TABLE users ADD COLUMN session_version INTEGER NOT NULL DEFAULT 1"); err != nil {
			return err
		}
	}
	for _, migration := range []struct{ column, sql string }{
		{"entry_mode", "ALTER TABLE control_nodes ADD COLUMN entry_mode TEXT NOT NULL DEFAULT 'direct'"},
		{"http_port", "ALTER TABLE control_nodes ADD COLUMN http_port INTEGER NOT NULL DEFAULT 0"},
		{"https_port", "ALTER TABLE control_nodes ADD COLUMN https_port INTEGER NOT NULL DEFAULT 443"},
		{"desired_config_hash", "ALTER TABLE control_nodes ADD COLUMN desired_config_hash TEXT NOT NULL DEFAULT ''"},
		{"applied_config_hash", "ALTER TABLE control_nodes ADD COLUMN applied_config_hash TEXT NOT NULL DEFAULT ''"},
		{"agent_listener_error", "ALTER TABLE control_nodes ADD COLUMN agent_listener_error TEXT NOT NULL DEFAULT ''"},
	} {
		var found int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('control_nodes') WHERE name=?", migration.column).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
		}
	}

	for _, migration := range []struct {
		column string
		sql    string
	}{
		{"playback_target_url", "ALTER TABLE sites ADD COLUMN playback_target_url TEXT NOT NULL DEFAULT ''"},
		{"playback_mode", "ALTER TABLE sites ADD COLUMN playback_mode TEXT NOT NULL DEFAULT 'direct'"},
		{"main_video_stream_mode", "ALTER TABLE sites ADD COLUMN main_video_stream_mode TEXT NOT NULL DEFAULT 'proxy'"},
		{"primary_line_name", "ALTER TABLE sites ADD COLUMN primary_line_name TEXT NOT NULL DEFAULT '主线路'"},
		{"failover_targets", "ALTER TABLE sites ADD COLUMN failover_targets TEXT NOT NULL DEFAULT '[]'"},
		{"failover_lines", "ALTER TABLE sites ADD COLUMN failover_lines TEXT NOT NULL DEFAULT '[]'"},
		{"stream_hosts", "ALTER TABLE sites ADD COLUMN stream_hosts TEXT NOT NULL DEFAULT '[]'"},
		{"custom_user_agent", "ALTER TABLE sites ADD COLUMN custom_user_agent TEXT NOT NULL DEFAULT ''"},
		{"custom_client", "ALTER TABLE sites ADD COLUMN custom_client TEXT NOT NULL DEFAULT ''"},
		{"custom_version", "ALTER TABLE sites ADD COLUMN custom_version TEXT NOT NULL DEFAULT ''"},
		{"client_ip_mode", "ALTER TABLE sites ADD COLUMN client_ip_mode TEXT NOT NULL DEFAULT 'both'"},
		{"public_host", "ALTER TABLE sites ADD COLUMN public_host TEXT NOT NULL DEFAULT ''"},
		{"path_prefix", "ALTER TABLE sites ADD COLUMN path_prefix TEXT NOT NULL DEFAULT ''"},
		{"ingress_mode", "ALTER TABLE sites ADD COLUMN ingress_mode TEXT NOT NULL DEFAULT 'port'"},
		{"upstream_headers", "ALTER TABLE sites ADD COLUMN upstream_headers TEXT NOT NULL DEFAULT '[]'"},
		{"dynamic_discovery_enabled", "ALTER TABLE sites ADD COLUMN dynamic_discovery_enabled INTEGER NOT NULL DEFAULT 0"},
		{"dynamic_profile", "ALTER TABLE sites ADD COLUMN dynamic_profile TEXT NOT NULL DEFAULT 'safe'"},
		{"dynamic_discovery_sources", "ALTER TABLE sites ADD COLUMN dynamic_discovery_sources TEXT NOT NULL DEFAULT '[\"redirect\"]'"},
		{"dynamic_domain_rules", "ALTER TABLE sites ADD COLUMN dynamic_domain_rules TEXT NOT NULL DEFAULT '[]'"},
		{"dynamic_allow_https_downgrade", "ALTER TABLE sites ADD COLUMN dynamic_allow_https_downgrade INTEGER NOT NULL DEFAULT 0"},
		{"dynamic_policy_revision", "ALTER TABLE sites ADD COLUMN dynamic_policy_revision INTEGER NOT NULL DEFAULT 1"},
		{"asset_cache_enabled", "ALTER TABLE sites ADD COLUMN asset_cache_enabled INTEGER NOT NULL DEFAULT 0"},
		{"asset_cache_ttl_sec", "ALTER TABLE sites ADD COLUMN asset_cache_ttl_sec INTEGER NOT NULL DEFAULT 86400"},
		{"asset_cache_max_bytes", "ALTER TABLE sites ADD COLUMN asset_cache_max_bytes BIGINT NOT NULL DEFAULT 536870912"},
		{"asset_cache_rules", "ALTER TABLE sites ADD COLUMN asset_cache_rules TEXT NOT NULL DEFAULT '*/file/*\n*/emby/Items/*/Images/*'"},
		{"watch_history_enabled", "ALTER TABLE sites ADD COLUMN watch_history_enabled INTEGER NOT NULL DEFAULT 0"},
		{"account_retention_days", "ALTER TABLE sites ADD COLUMN account_retention_days INTEGER NOT NULL DEFAULT 0"},
		{"account_retention_started_at_ms", "ALTER TABLE sites ADD COLUMN account_retention_started_at_ms INTEGER NOT NULL DEFAULT 0"},
		{"account_retention_last_completed_at_ms", "ALTER TABLE sites ADD COLUMN account_retention_last_completed_at_ms INTEGER NOT NULL DEFAULT 0"},
		{"media_movie_count", "ALTER TABLE sites ADD COLUMN media_movie_count INTEGER NOT NULL DEFAULT -1"},
		{"media_series_count", "ALTER TABLE sites ADD COLUMN media_series_count INTEGER NOT NULL DEFAULT -1"},
		{"media_episode_count", "ALTER TABLE sites ADD COLUMN media_episode_count INTEGER NOT NULL DEFAULT -1"},
		{"media_count_updated_at_ms", "ALTER TABLE sites ADD COLUMN media_count_updated_at_ms INTEGER NOT NULL DEFAULT 0"},
		{"sort_order", "ALTER TABLE sites ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0"},
	} {
		exists, err := sqliteColumnExists(ctx, conn, migration.column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
			if migration.column == "sort_order" {
				if _, err := conn.ExecContext(ctx, "UPDATE sites SET sort_order=id"); err != nil {
					return err
				}
			}
		}
	}
	for _, migration := range []struct {
		table, column, sql string
	}{
		{"media_items", "cast_json", "ALTER TABLE media_items ADD COLUMN cast_json TEXT NOT NULL DEFAULT '[]'"},
		{"tmdb_cache", "cast_json", "ALTER TABLE tmdb_cache ADD COLUMN cast_json TEXT NOT NULL DEFAULT '[]'"},
		{"media_items", "details_version", "ALTER TABLE media_items ADD COLUMN details_version INTEGER NOT NULL DEFAULT 0"},
		{"tmdb_cache", "details_version", "ALTER TABLE tmdb_cache ADD COLUMN details_version INTEGER NOT NULL DEFAULT 0"},
		{"media_items", "backdrop_path", "ALTER TABLE media_items ADD COLUMN backdrop_path TEXT NOT NULL DEFAULT ''"},
		{"media_items", "release_date", "ALTER TABLE media_items ADD COLUMN release_date TEXT NOT NULL DEFAULT ''"},
		{"media_items", "vote_average", "ALTER TABLE media_items ADD COLUMN vote_average REAL NOT NULL DEFAULT 0"},
		{"media_items", "genres_json", "ALTER TABLE media_items ADD COLUMN genres_json TEXT NOT NULL DEFAULT '[]'"},
		{"media_items", "status", "ALTER TABLE media_items ADD COLUMN status TEXT NOT NULL DEFAULT ''"},
		{"media_items", "last_air_date", "ALTER TABLE media_items ADD COLUMN last_air_date TEXT NOT NULL DEFAULT ''"},
		{"media_items", "next_air_date", "ALTER TABLE media_items ADD COLUMN next_air_date TEXT NOT NULL DEFAULT ''"},
		{"media_items", "next_season_number", "ALTER TABLE media_items ADD COLUMN next_season_number INTEGER NOT NULL DEFAULT -1"},
		{"media_items", "next_episode_number", "ALTER TABLE media_items ADD COLUMN next_episode_number INTEGER NOT NULL DEFAULT -1"},
		{"media_items", "next_episode_name", "ALTER TABLE media_items ADD COLUMN next_episode_name TEXT NOT NULL DEFAULT ''"},
		{"media_items", "season_count", "ALTER TABLE media_items ADD COLUMN season_count INTEGER NOT NULL DEFAULT 0"},
		{"media_items", "episode_count", "ALTER TABLE media_items ADD COLUMN episode_count INTEGER NOT NULL DEFAULT 0"},
		{"media_items", "stills_json", "ALTER TABLE media_items ADD COLUMN stills_json TEXT NOT NULL DEFAULT '[]'"},
		{"tmdb_cache", "backdrop_path", "ALTER TABLE tmdb_cache ADD COLUMN backdrop_path TEXT NOT NULL DEFAULT ''"},
		{"tmdb_cache", "release_date", "ALTER TABLE tmdb_cache ADD COLUMN release_date TEXT NOT NULL DEFAULT ''"},
		{"tmdb_cache", "vote_average", "ALTER TABLE tmdb_cache ADD COLUMN vote_average REAL NOT NULL DEFAULT 0"},
		{"tmdb_cache", "genres_json", "ALTER TABLE tmdb_cache ADD COLUMN genres_json TEXT NOT NULL DEFAULT '[]'"},
		{"tmdb_cache", "status", "ALTER TABLE tmdb_cache ADD COLUMN status TEXT NOT NULL DEFAULT ''"},
		{"tmdb_cache", "last_air_date", "ALTER TABLE tmdb_cache ADD COLUMN last_air_date TEXT NOT NULL DEFAULT ''"},
		{"tmdb_cache", "next_air_date", "ALTER TABLE tmdb_cache ADD COLUMN next_air_date TEXT NOT NULL DEFAULT ''"},
		{"tmdb_cache", "next_season_number", "ALTER TABLE tmdb_cache ADD COLUMN next_season_number INTEGER NOT NULL DEFAULT -1"},
		{"tmdb_cache", "next_episode_number", "ALTER TABLE tmdb_cache ADD COLUMN next_episode_number INTEGER NOT NULL DEFAULT -1"},
		{"tmdb_cache", "next_episode_name", "ALTER TABLE tmdb_cache ADD COLUMN next_episode_name TEXT NOT NULL DEFAULT ''"},
		{"tmdb_cache", "season_count", "ALTER TABLE tmdb_cache ADD COLUMN season_count INTEGER NOT NULL DEFAULT 0"},
		{"tmdb_cache", "episode_count", "ALTER TABLE tmdb_cache ADD COLUMN episode_count INTEGER NOT NULL DEFAULT 0"},
		{"tmdb_cache", "stills_json", "ALTER TABLE tmdb_cache ADD COLUMN stills_json TEXT NOT NULL DEFAULT '[]'"},
		{"watch_sessions", "user_name", "ALTER TABLE watch_sessions ADD COLUMN user_name TEXT NOT NULL DEFAULT ''"},
		{"watch_sessions", "user_id", "ALTER TABLE watch_sessions ADD COLUMN user_id TEXT NOT NULL DEFAULT ''"},
		{"watch_sessions", "device_id", "ALTER TABLE watch_sessions ADD COLUMN device_id TEXT NOT NULL DEFAULT ''"},
		{"watch_sessions", "device_name", "ALTER TABLE watch_sessions ADD COLUMN device_name TEXT NOT NULL DEFAULT ''"},
		{"watch_sessions", "client_name", "ALTER TABLE watch_sessions ADD COLUMN client_name TEXT NOT NULL DEFAULT ''"},
		{"watch_sessions", "play_session_id", "ALTER TABLE watch_sessions ADD COLUMN play_session_id TEXT NOT NULL DEFAULT ''"},
		{"watch_sessions", "token_ciphertext", "ALTER TABLE watch_sessions ADD COLUMN token_ciphertext TEXT NOT NULL DEFAULT ''"},
	} {
		var found int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?", migration.table, migration.column).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
		}
	}
	// Detail payloads are copied into media_items for fast history rendering.
	// Keep a schema marker so a richer TMDB payload (cast avatars, stills, etc.)
	// can invalidate old copies exactly once instead of serving them forever.
	var tmdbDetailsSchemaVersion int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('tmdb_settings') WHERE name='details_schema_version'").Scan(&tmdbDetailsSchemaVersion); err != nil {
		return err
	}
	if tmdbDetailsSchemaVersion == 0 {
		if _, err := conn.ExecContext(ctx, "ALTER TABLE tmdb_settings ADD COLUMN details_schema_version INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	if err := conn.QueryRowContext(ctx, "SELECT details_schema_version FROM tmdb_settings WHERE id=1").Scan(&tmdbDetailsSchemaVersion); err != nil {
		return err
	}
	if tmdbDetailsSchemaVersion < tmdbDetailsVersion {
		if _, err := conn.ExecContext(ctx, `UPDATE media_items SET overview='', poster_path='', backdrop_path='', release_date='',
			vote_average=0, genres_json='[]', status='', last_air_date='', next_air_date='', next_season_number=-1,
			next_episode_number=-1, next_episode_name='', season_count=0, episode_count=0, stills_json='[]', cast_json='[]',
			details_version=0, match_status='pending', metadata_updated_at_ms=0,
			updated_at_ms=MAX(updated_at_ms,strftime('%s','now')*1000)`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO tmdb_jobs
			(media_item_id, state, attempts, next_attempt_at_ms, lease_until_ms, last_error_code, revision, updated_at_ms)
			SELECT id, 'pending', 0, 0, 0, '', 0, strftime('%s','now')*1000 FROM media_items`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE tmdb_jobs SET state='pending', attempts=0,
			next_attempt_at_ms=0, lease_until_ms=0, last_error_code='', revision=revision+1,
			updated_at_ms=strftime('%s','now')*1000`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM tmdb_cache"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE tmdb_settings SET details_schema_version=? WHERE id=1", tmdbDetailsVersion); err != nil {
			return err
		}
	}
	// Existing records were enriched before the extended detail fields existed.
	// Queue them once for a fresh TMDB detail fetch while keeping the marker
	// stable across subsequent startups.
	if _, err := conn.ExecContext(ctx, `UPDATE tmdb_jobs SET state='pending', attempts=0,
		next_attempt_at_ms=0, lease_until_ms=0, last_error_code='', updated_at_ms=strftime('%s','now')*1000
		WHERE media_item_id IN (SELECT id FROM media_items WHERE metadata_updated_at_ms>0 AND details_version=0)`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE media_items SET details_version=1 WHERE metadata_updated_at_ms>0 AND details_version=0`); err != nil {
		return err
	}
	for _, migration := range []struct{ column, sql string }{
		{"traffic_billing_mode", "ALTER TABLE system_settings ADD COLUMN traffic_billing_mode TEXT NOT NULL DEFAULT 'bidirectional'"},
		{"traffic_reset_day", "ALTER TABLE system_settings ADD COLUMN traffic_reset_day INTEGER NOT NULL DEFAULT 1"},
		{"log_write_playback", "ALTER TABLE system_settings ADD COLUMN log_write_playback INTEGER NOT NULL DEFAULT 1"},
		{"log_write_video", "ALTER TABLE system_settings ADD COLUMN log_write_video INTEGER NOT NULL DEFAULT 1"},
		{"log_write_api", "ALTER TABLE system_settings ADD COLUMN log_write_api INTEGER NOT NULL DEFAULT 1"},
		{"log_write_auth", "ALTER TABLE system_settings ADD COLUMN log_write_auth INTEGER NOT NULL DEFAULT 1"},
		{"log_write_subtitle", "ALTER TABLE system_settings ADD COLUMN log_write_subtitle INTEGER NOT NULL DEFAULT 1"},
		{"log_write_asset", "ALTER TABLE system_settings ADD COLUMN log_write_asset INTEGER NOT NULL DEFAULT 1"},
		{"log_write_websocket", "ALTER TABLE system_settings ADD COLUMN log_write_websocket INTEGER NOT NULL DEFAULT 1"},
		{"log_resource_taxonomy_version", "ALTER TABLE system_settings ADD COLUMN log_resource_taxonomy_version INTEGER NOT NULL DEFAULT 0"},
		{"log_write_node", "ALTER TABLE system_settings ADD COLUMN log_write_node INTEGER NOT NULL DEFAULT 1"},
		{"log_write_category", "ALTER TABLE system_settings ADD COLUMN log_write_category INTEGER NOT NULL DEFAULT 1"},
		{"log_write_status", "ALTER TABLE system_settings ADD COLUMN log_write_status INTEGER NOT NULL DEFAULT 1"},
		{"log_write_timeline", "ALTER TABLE system_settings ADD COLUMN log_write_timeline INTEGER NOT NULL DEFAULT 1"},
		{"log_write_upstream_ua", "ALTER TABLE system_settings ADD COLUMN log_write_upstream_ua INTEGER NOT NULL DEFAULT 1"},
		{"log_write_backend_address", "ALTER TABLE system_settings ADD COLUMN log_write_backend_address INTEGER NOT NULL DEFAULT 1"},
		{"log_display_node", "ALTER TABLE system_settings ADD COLUMN log_display_node INTEGER NOT NULL DEFAULT 1"},
		{"log_display_category", "ALTER TABLE system_settings ADD COLUMN log_display_category INTEGER NOT NULL DEFAULT 1"},
		{"log_display_status", "ALTER TABLE system_settings ADD COLUMN log_display_status INTEGER NOT NULL DEFAULT 1"},
		{"log_display_timeline", "ALTER TABLE system_settings ADD COLUMN log_display_timeline INTEGER NOT NULL DEFAULT 1"},
		{"log_display_upstream_ua", "ALTER TABLE system_settings ADD COLUMN log_display_upstream_ua INTEGER NOT NULL DEFAULT 1"},
		{"log_display_backend_address", "ALTER TABLE system_settings ADD COLUMN log_display_backend_address INTEGER NOT NULL DEFAULT 1"},
	} {
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('system_settings') WHERE name=?", migration.column).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
		}
	}
	for _, migration := range []struct{ column, sql string }{
		{"traffic_used_in", "ALTER TABLE sites ADD COLUMN traffic_used_in BIGINT NOT NULL DEFAULT -1"},
		{"traffic_used_out", "ALTER TABLE sites ADD COLUMN traffic_used_out BIGINT NOT NULL DEFAULT -1"},
	} {
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('sites') WHERE name=?", migration.column).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
		}
	}
	for _, migration := range []struct{ table, column, sql string }{
		{"control_nodes", "traffic_manual_offset_bytes", "ALTER TABLE control_nodes ADD COLUMN traffic_manual_offset_bytes BIGINT NOT NULL DEFAULT 0"},
		{"control_nodes", "last_report_session_id", "ALTER TABLE control_nodes ADD COLUMN last_report_session_id TEXT NOT NULL DEFAULT ''"},
		{"control_nodes", "event_spool_error", "ALTER TABLE control_nodes ADD COLUMN event_spool_error TEXT NOT NULL DEFAULT ''"},
		{"control_nodes", "event_queue_depth", "ALTER TABLE control_nodes ADD COLUMN event_queue_depth INTEGER NOT NULL DEFAULT 0"},
		{"control_nodes", "event_dropped", "ALTER TABLE control_nodes ADD COLUMN event_dropped BIGINT NOT NULL DEFAULT 0"},
		{"site_node_schedules", "agent_boot_id", "ALTER TABLE site_node_schedules ADD COLUMN agent_boot_id TEXT NOT NULL DEFAULT ''"},
		{"site_node_schedules", "agent_request_count", "ALTER TABLE site_node_schedules ADD COLUMN agent_request_count BIGINT NOT NULL DEFAULT 0"},
		{"site_node_schedules", "agent_last_request_at_ms", "ALTER TABLE site_node_schedules ADD COLUMN agent_last_request_at_ms INTEGER NOT NULL DEFAULT 0"},
		{"site_node_schedules", "agent_last_status", "ALTER TABLE site_node_schedules ADD COLUMN agent_last_status INTEGER NOT NULL DEFAULT 0"},
	} {
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?", migration.table, migration.column).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS node_request_events (
		node_id INTEGER NOT NULL REFERENCES control_nodes(id) ON DELETE CASCADE,
		agent_boot_id TEXT NOT NULL,
		event_id BIGINT NOT NULL,
		event_uid TEXT NOT NULL DEFAULT '',
		received_at_ms INTEGER NOT NULL,
		PRIMARY KEY(node_id, agent_boot_id, event_id)
	) WITHOUT ROWID; CREATE INDEX IF NOT EXISTS idx_node_request_events_received ON node_request_events(received_at_ms);`); err != nil {
		return err
	}
	var eventUIDColumnCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('node_request_events') WHERE name=?", "event_uid").Scan(&eventUIDColumnCount); err != nil {
		return err
	} else if eventUIDColumnCount == 0 {
		// The table may have been created by an older release. Add the stable
		// event identity before creating the partial unique index.
		if _, err := conn.ExecContext(ctx, "ALTER TABLE node_request_events ADD COLUMN event_uid TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_node_request_events_uid ON node_request_events(node_id,event_uid) WHERE event_uid <> ''"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS node_site_counters (
		node_id INTEGER NOT NULL REFERENCES control_nodes(id) ON DELETE CASCADE,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		boot_id TEXT NOT NULL,
		last_bytes_in BIGINT NOT NULL DEFAULT 0,
		last_bytes_out BIGINT NOT NULL DEFAULT 0,
		last_request_count BIGINT NOT NULL DEFAULT 0,
		updated_at_ms INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(node_id, site_id)
	) WITHOUT ROWID;
	CREATE TABLE IF NOT EXISTS node_site_traffic_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id INTEGER NOT NULL REFERENCES control_nodes(id) ON DELETE CASCADE,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		bytes_in BIGINT NOT NULL DEFAULT 0,
		bytes_out BIGINT NOT NULL DEFAULT 0,
		requests BIGINT NOT NULL DEFAULT 0,
		recorded_at_ms INTEGER NOT NULL,
		UNIQUE(node_id, site_id, recorded_at_ms)
	);
	CREATE INDEX IF NOT EXISTS idx_node_site_traffic_site_time ON node_site_traffic_logs(site_id, recorded_at_ms);
	CREATE INDEX IF NOT EXISTS idx_node_site_traffic_node_time ON node_site_traffic_logs(node_id, recorded_at_ms);`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sites SET traffic_used_in=traffic_used/2, traffic_used_out=traffic_used-(traffic_used/2) WHERE traffic_used_in<0 OR traffic_used_out<0"); err != nil {
		return err
	}
	var logResourceTaxonomyVersion int
	if err := conn.QueryRowContext(ctx, "SELECT log_resource_taxonomy_version FROM system_settings WHERE id=1").Scan(&logResourceTaxonomyVersion); err != nil {
		return err
	}
	if logResourceTaxonomyVersion < 1 {
		if _, err := conn.ExecContext(ctx, `UPDATE system_settings SET
			log_write_playback=log_write_metadata,
			log_write_metadata=0,
			log_resource_taxonomy_version=1
			WHERE id=1`); err != nil {
			return err
		}
	}
	if err := ensurePanelSettingsListenPortSchema(ctx, conn); err != nil {
		return err
	}
	for _, migration := range []struct{ column, sql string }{
		{"inbound_colo", "ALTER TABLE request_logs ADD COLUMN inbound_colo TEXT NOT NULL DEFAULT ''"},
		{"outbound_colo", "ALTER TABLE request_logs ADD COLUMN outbound_colo TEXT NOT NULL DEFAULT ''"},
		{"upstream_user_agent", "ALTER TABLE request_logs ADD COLUMN upstream_user_agent TEXT NOT NULL DEFAULT ''"},
		{"backend_address", "ALTER TABLE request_logs ADD COLUMN backend_address TEXT NOT NULL DEFAULT ''"},
		{"final_node", "ALTER TABLE request_logs ADD COLUMN final_node TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('request_logs') WHERE name=?", migration.column).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
		}
	}
	var hasTimelineColumn int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('request_logs') WHERE name='timeline_at_ms'").Scan(&hasTimelineColumn); err != nil {
		return err
	}
	if hasTimelineColumn == 0 {
		if _, err := conn.ExecContext(ctx, "ALTER TABLE request_logs ADD COLUMN timeline_at_ms INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE request_logs SET timeline_at_ms=recorded_at_ms"); err != nil {
			return err
		}
	}
	// public_host was introduced before ingress_mode on the unreleased Issue #28
	// branch. Migrate those rows to the secure host-only behavior instead of
	// silently retaining a public high-port listener.
	if _, err := conn.ExecContext(ctx, "UPDATE sites SET ingress_mode='host' WHERE public_host <> '' AND ingress_mode='port'"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_sites_public_host ON sites(public_host COLLATE NOCASE) WHERE public_host <> ''"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_sites_path_prefix ON sites(path_prefix COLLATE NOCASE) WHERE path_prefix <> ''"); err != nil {
		return err
	}

	var hasRequestsColumn int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('traffic_logs') WHERE name='requests'").Scan(&hasRequestsColumn); err != nil {
		return err
	}
	if hasRequestsColumn == 0 {
		if _, err := conn.ExecContext(ctx, "ALTER TABLE traffic_logs ADD COLUMN requests BIGINT NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}

	// idx_traffic_site_hour and idx_traffic_site_minute enforce the same
	// physical uniqueness (site_id, recorded_at); only the bucket timestamp
	// written by addTrafficWithRequests changes from HH:00 to HH:MM. Preserve
	// every legacy hourly row, and only collapse exact duplicate timestamps on
	// very old databases that predate the unique index.
	var hasTrafficBucketIndex int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_traffic_site_hour','idx_traffic_site_minute')").Scan(&hasTrafficBucketIndex); err != nil {
		return err
	}
	if hasTrafficBucketIndex == 0 {
		if _, err := conn.ExecContext(ctx, `
			CREATE TABLE traffic_logs_dedup (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
				bytes_in BIGINT DEFAULT 0,
				bytes_out BIGINT DEFAULT 0,
				requests BIGINT NOT NULL DEFAULT 0,
				recorded_at DATETIME NOT NULL
			);
			INSERT INTO traffic_logs_dedup (site_id, bytes_in, bytes_out, requests, recorded_at)
			SELECT site_id, SUM(bytes_in), SUM(bytes_out), SUM(requests), recorded_at
			FROM traffic_logs
			GROUP BY site_id, recorded_at;
			DELETE FROM traffic_logs;
			INSERT INTO traffic_logs (site_id, bytes_in, bytes_out, requests, recorded_at)
			SELECT site_id, bytes_in, bytes_out, requests, recorded_at
			FROM traffic_logs_dedup;
			DROP TABLE traffic_logs_dedup;
		`); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_traffic_site_minute ON traffic_logs(site_id, recorded_at)"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "DROP INDEX IF EXISTS idx_traffic_site_hour"); err != nil {
		return err
	}
	var hasTrafficRecordedMS int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('traffic_logs') WHERE name='recorded_at_ms'").Scan(&hasTrafficRecordedMS); err != nil {
		return err
	}
	if hasTrafficRecordedMS == 0 {
		if _, err := conn.ExecContext(ctx, "ALTER TABLE traffic_logs ADD COLUMN recorded_at_ms INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, "SELECT id, recorded_at FROM traffic_logs WHERE recorded_at_ms=0")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var recordedAt string
			if err := rows.Scan(&id, &recordedAt); err != nil {
				rows.Close()
				return err
			}
			if recordedAtMS := trafficWallClockMillis(recordedAt); recordedAtMS > 0 {
				if _, err := conn.ExecContext(ctx, "UPDATE traffic_logs SET recorded_at_ms=? WHERE id=?", recordedAtMS, id); err != nil {
					rows.Close()
					return err
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	if _, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_traffic_site_time_ms ON traffic_logs(site_id, recorded_at_ms)"); err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func ensurePanelSettingsListenPortSchema(ctx context.Context, conn *sql.Conn) error {
	for _, migration := range []struct{ column, sql string }{
		{"listen_port", "ALTER TABLE panel_settings ADD COLUMN listen_port INTEGER NOT NULL DEFAULT 0"},
		{"acme_email", "ALTER TABLE panel_settings ADD COLUMN acme_email TEXT NOT NULL DEFAULT ''"},
		{"acme_dns_provider", "ALTER TABLE panel_settings ADD COLUMN acme_dns_provider TEXT NOT NULL DEFAULT 'cloudflare'"},
		{"acme_token_ciphertext", "ALTER TABLE panel_settings ADD COLUMN acme_token_ciphertext TEXT NOT NULL DEFAULT ''"},
		{"acme_staging", "ALTER TABLE panel_settings ADD COLUMN acme_staging INTEGER NOT NULL DEFAULT 0"},
	} {
		var found int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('panel_settings') WHERE name=?", migration.column).Scan(&found); err != nil {
			return err
		}
		if found != 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
			return err
		}
	}
	return nil
}

func sqliteColumnExists(ctx context.Context, conn *sql.Conn, column string) (bool, error) {
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('sites') WHERE name=?", column).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

const dynamicObservationTableDDL = `
CREATE TABLE dynamic_observations (
	site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
	canonical_authority TEXT NOT NULL,
	source TEXT NOT NULL CHECK(source IN ('redirect', 'playback_info', 'hls', 'dash')),
	decision TEXT NOT NULL CHECK(decision IN ('allowed', 'denied')),
	reason_code TEXT NOT NULL CHECK(reason_code IN (
		'redirect_allowed',
		'candidate_allowed',
		'invalid_location',
		'unsupported_status',
		'redirect_loop',
		'hop_limit',
		'scheme_denied',
		'port_denied',
		'domain_denied',
		'https_downgrade_denied',
		'self_target',
		'dns_failure',
		'address_denied',
		'dial_failure',
		'tls_failure',
		'capacity_limit',
		'rate_limit',
		'parse_failure',
		'request_unclassified',
		'structured_body_limit',
		'playback_info_denied',
		'hls_feature_denied',
		'dash_feature_denied',
		'redirect_body_replay_denied',
		'capability_invalid',
		'capability_expired',
		'response_failure',
		'runtime_unavailable'
	)),
	first_seen_ms INTEGER NOT NULL CHECK(first_seen_ms >= 0),
	last_seen_ms INTEGER NOT NULL CHECK(last_seen_ms >= first_seen_ms),
	count INTEGER NOT NULL CHECK(count > 0),
	PRIMARY KEY(site_id, canonical_authority, source, decision, reason_code)
) WITHOUT ROWID;`

const dynamicObservationIndexesDDL = `
CREATE INDEX IF NOT EXISTS idx_dynamic_observations_site_last_seen
	ON dynamic_observations(site_id, last_seen_ms DESC);
CREATE INDEX IF NOT EXISTS idx_dynamic_observations_last_seen
	ON dynamic_observations(last_seen_ms);`

const (
	dynamicObservationSchemaCurrent  = "current"
	dynamicObservationSchemaPrevious = "previous"
	dynamicObservationSchemaLegacy   = "legacy"
	dynamicObservationSchemaInvalid  = "invalid"
)

func compactSQLiteDDL(value string) string {
	var compact strings.Builder
	compact.Grow(len(value))
	for _, character := range strings.ToLower(value) {
		if !unicode.IsSpace(character) {
			compact.WriteRune(character)
		}
	}
	return compact.String()
}

func dynamicObservationSchemaState(tableSQL string) string {
	compact := compactSQLiteDDL(tableSQL)
	common := []string{
		"check(decisionin('allowed','denied'))",
		"check(first_seen_ms>=0)",
		"check(last_seen_ms>=first_seen_ms)",
		"check(count>0)",
		"site_idintegernotnullreferencessites(id)ondeletecascade",
		")withoutrowid",
	}
	for _, fragment := range common {
		if !strings.Contains(compact, fragment) {
			return dynamicObservationSchemaInvalid
		}
	}
	currentSource := "check(sourcein('redirect','playback_info','hls','dash'))"
	currentReasons := "check(reason_codein('redirect_allowed','candidate_allowed','invalid_location','unsupported_status','redirect_loop','hop_limit','scheme_denied','port_denied','domain_denied','https_downgrade_denied','self_target','dns_failure','address_denied','dial_failure','tls_failure','capacity_limit','rate_limit','parse_failure','request_unclassified','structured_body_limit','playback_info_denied','hls_feature_denied','dash_feature_denied','redirect_body_replay_denied','capability_invalid','capability_expired','response_failure','runtime_unavailable'))"
	if strings.Contains(compact, currentSource) && strings.Contains(compact, currentReasons) {
		return dynamicObservationSchemaCurrent
	}
	previousReasons := "check(reason_codein('redirect_allowed','candidate_allowed','invalid_location','unsupported_status','redirect_loop','hop_limit','scheme_denied','port_denied','domain_denied','https_downgrade_denied','self_target','dns_failure','address_denied','dial_failure','tls_failure','capacity_limit','rate_limit','parse_failure','capability_invalid','capability_expired','response_failure','runtime_unavailable'))"
	if strings.Contains(compact, currentSource) && strings.Contains(compact, previousReasons) {
		return dynamicObservationSchemaPrevious
	}
	legacySource := "check(source='redirect')"
	legacyReasons := "check(reason_codein('redirect_allowed','invalid_location','unsupported_status','redirect_loop','hop_limit','scheme_denied','port_denied','domain_denied','https_downgrade_denied','self_target','dns_failure','address_denied','dial_failure','tls_failure','capacity_limit','rate_limit','response_failure','runtime_unavailable'))"
	if strings.Contains(compact, legacySource) && strings.Contains(compact, legacyReasons) {
		return dynamicObservationSchemaLegacy
	}
	return dynamicObservationSchemaInvalid
}

func ensureDynamicObservationSchema(ctx context.Context, conn *sql.Conn) error {
	var tableSQL string
	err := conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='dynamic_observations'").Scan(&tableSQL)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, dynamicObservationTableDDL+dynamicObservationIndexesDDL); err != nil {
			return fmt.Errorf("create dynamic_observations schema: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect dynamic_observations table SQL: %w", err)
	}
	switch dynamicObservationSchemaState(tableSQL) {
	case dynamicObservationSchemaCurrent:
		if _, err := conn.ExecContext(ctx, dynamicObservationIndexesDDL); err != nil {
			return fmt.Errorf("create dynamic observation indexes: %w", err)
		}
		return nil
	case dynamicObservationSchemaPrevious, dynamicObservationSchemaLegacy:
		var staleTable int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='dynamic_observations_legacy'").Scan(&staleTable); err != nil {
			return fmt.Errorf("inspect legacy dynamic observation table: %w", err)
		}
		if staleTable != 0 {
			return fmt.Errorf("dynamic_observations_legacy already exists")
		}
		migrationSQL := `
ALTER TABLE dynamic_observations RENAME TO dynamic_observations_legacy;
` + dynamicObservationTableDDL + `
INSERT INTO dynamic_observations
	(site_id, canonical_authority, source, decision, reason_code, first_seen_ms, last_seen_ms, count)
SELECT site_id, canonical_authority, source, decision, reason_code, first_seen_ms, last_seen_ms, count
FROM dynamic_observations_legacy;
DROP TABLE dynamic_observations_legacy;
` + dynamicObservationIndexesDDL
		if _, err := conn.ExecContext(ctx, migrationSQL); err != nil {
			return fmt.Errorf("migrate dynamic_observations enum constraints: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("dynamic_observations contains unrecognized or unsafe constraints")
	}
}

func validateDynamicObservationSchema(ctx context.Context, conn *sql.Conn) error {
	type columnSpec struct {
		name               string
		typeName           string
		primaryKeyPosition int
	}
	expected := []columnSpec{
		{name: "site_id", typeName: "INTEGER", primaryKeyPosition: 1},
		{name: "canonical_authority", typeName: "TEXT", primaryKeyPosition: 2},
		{name: "source", typeName: "TEXT", primaryKeyPosition: 3},
		{name: "decision", typeName: "TEXT", primaryKeyPosition: 4},
		{name: "reason_code", typeName: "TEXT", primaryKeyPosition: 5},
		{name: "first_seen_ms", typeName: "INTEGER"},
		{name: "last_seen_ms", typeName: "INTEGER"},
		{name: "count", typeName: "INTEGER"},
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT name, upper(type), "notnull", dflt_value, pk
		FROM pragma_table_info('dynamic_observations')
		ORDER BY cid`)
	if err != nil {
		return fmt.Errorf("inspect dynamic_observations schema: %w", err)
	}
	position := 0
	for rows.Next() {
		if position >= len(expected) {
			_ = rows.Close()
			return fmt.Errorf("dynamic_observations contains unexpected columns")
		}
		var name, typeName string
		var notNull, primaryKeyPosition int
		var defaultValue sql.NullString
		if err := rows.Scan(&name, &typeName, &notNull, &defaultValue, &primaryKeyPosition); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect dynamic_observations column: %w", err)
		}
		want := expected[position]
		if name != want.name || typeName != want.typeName || notNull != 1 || defaultValue.Valid || primaryKeyPosition != want.primaryKeyPosition {
			_ = rows.Close()
			return fmt.Errorf("dynamic_observations column %d has an invalid definition", position)
		}
		position++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("inspect dynamic_observations schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close dynamic_observations schema rows: %w", err)
	}
	if position != len(expected) {
		return fmt.Errorf("dynamic_observations is missing required columns")
	}

	var tableSQL string
	if err := conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='dynamic_observations'").Scan(&tableSQL); err != nil {
		return fmt.Errorf("inspect dynamic_observations table SQL: %w", err)
	}
	if dynamicObservationSchemaState(tableSQL) != dynamicObservationSchemaCurrent {
		return fmt.Errorf("dynamic_observations enum constraints are invalid")
	}

	for _, index := range []struct {
		name    string
		columns []string
	}{
		{name: "idx_dynamic_observations_site_last_seen", columns: []string{"site_id", "last_seen_ms"}},
		{name: "idx_dynamic_observations_last_seen", columns: []string{"last_seen_ms"}},
	} {
		indexRows, err := conn.QueryContext(ctx, "SELECT name FROM pragma_index_info(?) ORDER BY seqno", index.name)
		if err != nil {
			return fmt.Errorf("inspect dynamic observation index %s: %w", index.name, err)
		}
		columns := make([]string, 0, len(index.columns))
		for indexRows.Next() {
			var column string
			if err := indexRows.Scan(&column); err != nil {
				_ = indexRows.Close()
				return fmt.Errorf("inspect dynamic observation index %s: %w", index.name, err)
			}
			columns = append(columns, column)
		}
		if err := indexRows.Err(); err != nil {
			_ = indexRows.Close()
			return fmt.Errorf("inspect dynamic observation index %s: %w", index.name, err)
		}
		if err := indexRows.Close(); err != nil {
			return fmt.Errorf("close dynamic observation index %s rows: %w", index.name, err)
		}
		if len(columns) != len(index.columns) {
			return fmt.Errorf("dynamic observation index %s has an invalid definition", index.name)
		}
		for i := range columns {
			if columns[i] != index.columns[i] {
				return fmt.Errorf("dynamic observation index %s has an invalid definition", index.name)
			}
		}
	}
	return nil
}
