package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

const (
	maxSiteIconPackBytes = 4 << 20
	maxSiteIconCount     = 16384
	maxSiteIconNameLen   = 160
	maxSiteIconURLLen    = 4096
)

// SiteIcon is the small, safe subset of an uploaded icon package that the UI
// needs. We intentionally store only names and HTTPS image URLs; arbitrary
// HTML, SVG markup, scripts, and data URLs never enter the database or page.
type SiteIcon struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type SiteIconPack struct {
	ID          int64      `json:"id,omitempty"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	SourceURL   string     `json:"source_url,omitempty"`
	Icons       []SiteIcon `json:"icons"`
	UpdatedAt   string     `json:"updated_at,omitempty"`
	Active      bool       `json:"active,omitempty"`
	BuiltIn     bool       `json:"built_in"`
}

// SiteIconPackSummary is the lightweight list representation of an icon pack.
// The icon array is intentionally omitted so a long list never forces the
// controller to marshal megabytes of icon URLs into the management UI.
type SiteIconPackSummary struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	SourceURL   string `json:"source_url,omitempty"`
	IconCount   int    `json:"icon_count"`
	Active      bool   `json:"active"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	BuiltIn     bool   `json:"built_in"`
}

var errInvalidSiteIconPack = errors.New("invalid site icon pack")
var errSiteIconPackNotFound = errors.New("site icon pack not found")
var errSiteIconPackSourceMissing = errors.New("site icon pack has no source URL")

func normalizeSiteIconURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > maxSiteIconURLLen {
		return "", fmt.Errorf("icon url must be between 1 and %d bytes", maxSiteIconURLLen)
	}
	u, err := url.ParseRequestURI(value)
	if err != nil || u == nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("icon url must be an absolute HTTPS URL without credentials or fragments")
	}
	return u.String(), nil
}

func normalizeSiteIconName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maxSiteIconNameLen {
		return "", fmt.Errorf("icon name must be at most %d bytes", maxSiteIconNameLen)
	}
	for _, r := range value {
		if r == '\u0000' || r == '\n' || r == '\r' {
			return "", errors.New("icon name contains an invalid control character")
		}
	}
	return value, nil
}

func normalizeSiteIconSelection(name, imageURL string) (string, string, error) {
	name, err := normalizeSiteIconName(name)
	if err != nil {
		return "", "", err
	}
	imageURL = strings.TrimSpace(imageURL)
	if name == "" && imageURL == "" {
		return "", "", nil
	}
	if name == "" || imageURL == "" {
		return "", "", errors.New("icon name and icon url must be provided together")
	}
	imageURL, err = normalizeSiteIconURL(imageURL)
	if err != nil {
		return "", "", err
	}
	return name, imageURL, nil
}

func parseSiteIconPack(raw []byte) (SiteIconPack, error) {
	if len(raw) == 0 || len(raw) > maxSiteIconPackBytes {
		return SiteIconPack{}, fmt.Errorf("icon pack must be between 1 byte and %d MiB", maxSiteIconPackBytes>>20)
	}
	var input struct {
		Name        string     `json:"name"`
		Description string     `json:"description"`
		Icons       []SiteIcon `json:"icons"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return SiteIconPack{}, fmt.Errorf("%w: %v", errInvalidSiteIconPack, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return SiteIconPack{}, fmt.Errorf("%w: pack must contain one JSON object", errInvalidSiteIconPack)
	}
	if len(input.Icons) == 0 || len(input.Icons) > maxSiteIconCount {
		return SiteIconPack{}, fmt.Errorf("%w: icons must contain between 1 and %d entries", errInvalidSiteIconPack, maxSiteIconCount)
	}
	pack := SiteIconPack{Name: strings.TrimSpace(input.Name), Description: strings.TrimSpace(input.Description), Icons: make([]SiteIcon, 0, len(input.Icons))}
	if len(pack.Name) > 200 || len(pack.Description) > 1000 {
		return SiteIconPack{}, fmt.Errorf("%w: pack metadata is too long", errInvalidSiteIconPack)
	}
	seen := make(map[string]struct{}, len(input.Icons))
	for _, icon := range input.Icons {
		name, err := normalizeSiteIconName(icon.Name)
		if err != nil || name == "" {
			return SiteIconPack{}, fmt.Errorf("%w: invalid icon name", errInvalidSiteIconPack)
		}
		imageURL, err := normalizeSiteIconURL(icon.URL)
		if err != nil {
			return SiteIconPack{}, fmt.Errorf("%w: icon %q has an invalid url", errInvalidSiteIconPack, name)
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		pack.Icons = append(pack.Icons, SiteIcon{Name: name, URL: imageURL})
	}
	if len(pack.Icons) == 0 {
		return SiteIconPack{}, fmt.Errorf("%w: no usable icons", errInvalidSiteIconPack)
	}
	return pack, nil
}

func (d *DB) SiteIconPack() (SiteIconPack, error) {
	return d.ActiveSiteIconPack()
}

func (d *DB) ActiveSiteIconPack() (SiteIconPack, error) {
	pack, err := scanSiteIconPack(d.db.QueryRow("SELECT id, pack_name, description, source_url, icons_json, is_active, updated_at FROM site_icon_packs WHERE is_active=1 ORDER BY id LIMIT 1"))
	if errors.Is(err, errSiteIconPackNotFound) {
		return SiteIconPack{Icons: []SiteIcon{}}, nil
	}
	return pack, err
}

func (d *DB) SiteIconPackByID(id int64) (SiteIconPack, error) {
	return scanSiteIconPack(d.db.QueryRow("SELECT id, pack_name, description, source_url, icons_json, is_active, updated_at FROM site_icon_packs WHERE id=?", id))
}

func scanSiteIconPack(row *sql.Row) (SiteIconPack, error) {
	var pack SiteIconPack
	var iconsJSON string
	var isActive int
	err := row.Scan(&pack.ID, &pack.Name, &pack.Description, &pack.SourceURL, &iconsJSON, &isActive, &pack.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteIconPack{}, errSiteIconPackNotFound
	}
	if err != nil {
		return SiteIconPack{}, err
	}
	pack.Active = isActive != 0
	pack.BuiltIn = isBuiltinSiteIconPack(pack.SourceURL)
	if err := json.Unmarshal([]byte(iconsJSON), &pack.Icons); err != nil {
		return SiteIconPack{}, fmt.Errorf("decode stored site icon pack: %w", err)
	}
	if pack.Icons == nil {
		pack.Icons = []SiteIcon{}
	}
	return pack, nil
}

func (d *DB) SiteIconPacks() ([]SiteIconPackSummary, error) {
	rows, err := d.db.Query("SELECT id, pack_name, description, source_url, icons_json, is_active, updated_at FROM site_icon_packs ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	packs := make([]SiteIconPackSummary, 0)
	for rows.Next() {
		var summary SiteIconPackSummary
		var iconsJSON string
		var isActive int
		if err := rows.Scan(&summary.ID, &summary.Name, &summary.Description, &summary.SourceURL, &iconsJSON, &isActive, &summary.UpdatedAt); err != nil {
			return nil, err
		}
		var icons []SiteIcon
		if err := json.Unmarshal([]byte(iconsJSON), &icons); err != nil {
			return nil, fmt.Errorf("decode stored site icon pack: %w", err)
		}
		summary.IconCount = len(icons)
		summary.Active = isActive != 0
		summary.BuiltIn = isBuiltinSiteIconPack(summary.SourceURL)
		packs = append(packs, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return packs, nil
}

func normalizeSiteIconPackForStorage(pack SiteIconPack) (SiteIconPack, error) {
	if len(pack.Icons) == 0 || len(pack.Icons) > maxSiteIconCount {
		return SiteIconPack{}, errInvalidSiteIconPack
	}
	pack.Name = strings.TrimSpace(pack.Name)
	pack.Description = strings.TrimSpace(pack.Description)
	pack.SourceURL = strings.TrimSpace(pack.SourceURL)
	if len(pack.Name) > 200 || len(pack.Description) > 1000 {
		return SiteIconPack{}, errInvalidSiteIconPack
	}
	if pack.SourceURL != "" {
		var err error
		pack.SourceURL, err = normalizeSiteIconURL(pack.SourceURL)
		if err != nil {
			return SiteIconPack{}, err
		}
	}
	normalizedIcons := make([]SiteIcon, 0, len(pack.Icons))
	seen := make(map[string]struct{}, len(pack.Icons))
	for _, icon := range pack.Icons {
		name, err := normalizeSiteIconName(icon.Name)
		if err != nil || name == "" {
			return SiteIconPack{}, errInvalidSiteIconPack
		}
		imageURL, err := normalizeSiteIconURL(icon.URL)
		if err != nil {
			return SiteIconPack{}, errInvalidSiteIconPack
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		normalizedIcons = append(normalizedIcons, SiteIcon{Name: name, URL: imageURL})
	}
	if len(normalizedIcons) == 0 {
		return SiteIconPack{}, errInvalidSiteIconPack
	}
	pack.Icons = normalizedIcons
	return pack, nil
}

func (d *DB) SaveSiteIconPack(pack SiteIconPack) error {
	pack, err := normalizeSiteIconPackForStorage(pack)
	if err != nil {
		return err
	}
	iconsJSON, err := json.Marshal(pack.Icons)
	if err != nil {
		return err
	}
	if len(iconsJSON) > maxSiteIconPackBytes {
		return fmt.Errorf("icon pack is too large")
	}
	var activeID int64
	err = d.db.QueryRow("SELECT id FROM site_icon_packs WHERE is_active=1 ORDER BY id LIMIT 1").Scan(&activeID)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = d.db.Exec("INSERT INTO site_icon_packs (pack_name, description, source_url, icons_json, is_active) VALUES (?,?,?,?,1)", pack.Name, pack.Description, pack.SourceURL, string(iconsJSON))
		return err
	}
	if err != nil {
		return err
	}
	_, err = d.db.Exec("UPDATE site_icon_packs SET pack_name=?, description=?, source_url=?, icons_json=?, is_active=1, updated_at=CURRENT_TIMESTAMP WHERE id=?", pack.Name, pack.Description, pack.SourceURL, string(iconsJSON), activeID)
	return err
}

func (d *DB) CreateSiteIconPack(pack SiteIconPack) (SiteIconPack, error) {
	pack, err := normalizeSiteIconPackForStorage(pack)
	if err != nil {
		return SiteIconPack{}, err
	}
	iconsJSON, err := json.Marshal(pack.Icons)
	if err != nil {
		return SiteIconPack{}, err
	}
	if len(iconsJSON) > maxSiteIconPackBytes {
		return SiteIconPack{}, fmt.Errorf("icon pack is too large")
	}
	var activeCount int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM site_icon_packs WHERE is_active=1").Scan(&activeCount); err != nil {
		return SiteIconPack{}, err
	}
	isActive := 0
	if activeCount == 0 {
		isActive = 1
	}
	result, err := d.db.Exec("INSERT INTO site_icon_packs (pack_name, description, source_url, icons_json, is_active) VALUES (?,?,?,?,?)", pack.Name, pack.Description, pack.SourceURL, string(iconsJSON), isActive)
	if err != nil {
		return SiteIconPack{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return SiteIconPack{}, err
	}
	pack.ID = id
	pack.Active = isActive != 0
	pack.BuiltIn = isBuiltinSiteIconPack(pack.SourceURL)
	pack.UpdatedAt = iconPackUpdatedAt()
	return pack, nil
}

func (d *DB) UpdateSiteIconPack(id int64, pack SiteIconPack) (SiteIconPack, error) {
	stored, err := d.SiteIconPackByID(id)
	if err != nil {
		return SiteIconPack{}, err
	}
	if stored.SourceURL == "" {
		return SiteIconPack{}, errSiteIconPackSourceMissing
	}
	pack.SourceURL = stored.SourceURL
	pack, err = normalizeSiteIconPackForStorage(pack)
	if err != nil {
		return SiteIconPack{}, fmt.Errorf("%w: %v", errInvalidSiteIconPack, err)
	}
	iconsJSON, err := json.Marshal(pack.Icons)
	if err != nil {
		return SiteIconPack{}, err
	}
	if len(iconsJSON) > maxSiteIconPackBytes {
		return SiteIconPack{}, fmt.Errorf("%w: icon pack is too large", errInvalidSiteIconPack)
	}
	result, err := d.db.Exec("UPDATE site_icon_packs SET pack_name=?, description=?, icons_json=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", pack.Name, pack.Description, string(iconsJSON), id)
	if err != nil {
		return SiteIconPack{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return SiteIconPack{}, err
	}
	if affected == 0 {
		return SiteIconPack{}, errSiteIconPackNotFound
	}
	return d.SiteIconPackByID(id)
}

func (d *DB) SetActiveSiteIconPack(id int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE site_icon_packs SET is_active=0 WHERE is_active=1"); err != nil {
		return err
	}
	result, err := tx.Exec("UPDATE site_icon_packs SET is_active=1 WHERE id=?", id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errSiteIconPackNotFound
	}
	return tx.Commit()
}

func (d *DB) DeleteSiteIconPack(id int64) error {
	result, err := d.db.Exec("DELETE FROM site_icon_packs WHERE id=?", id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errSiteIconPackNotFound
	}
	return nil
}

func (d *DB) ClearSiteIconPack() error {
	_, err := d.db.Exec("DELETE FROM site_icon_packs")
	return err
}

func (p SiteIconPack) icon(name string) (SiteIcon, bool) {
	for _, icon := range p.Icons {
		if strings.EqualFold(strings.TrimSpace(icon.Name), strings.TrimSpace(name)) {
			return icon, true
		}
	}
	return SiteIcon{}, false
}

// validateNewSelection validates a selection the operator is actively making
// against the currently loaded icon pack. Existing persisted selections are
// intentionally handled by the site update path so replacing a pack cannot
// invalidate unrelated edits.
func (p SiteIconPack) validateNewSelection(name, imageURL string) (string, string, error) {
	name, imageURL, err := normalizeSiteIconSelection(name, imageURL)
	if err != nil || name == "" {
		return name, imageURL, err
	}
	icon, ok := p.icon(name)
	if !ok {
		return "", "", errors.New("selected icon does not exist in the current icon pack")
	}
	if imageURL != icon.URL {
		return "", "", errors.New("icon url does not match the selected icon pack entry")
	}
	return icon.Name, icon.URL, nil
}

// normalizeSelection is kept as a compatibility name for callers that mean
// an explicit picker selection. It is deliberately strict for new values.
func (p SiteIconPack) normalizeSelection(name, imageURL string) (string, string, error) {
	return p.validateNewSelection(name, imageURL)
}

func iconPackUpdatedAt() string { return time.Now().UTC().Format(time.RFC3339) }
