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
	maxSiteIconCount     = 4096
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
	Name        string     `json:"name"`
	Description string     `json:"description"`
	SourceURL   string     `json:"source_url,omitempty"`
	Icons       []SiteIcon `json:"icons"`
	UpdatedAt   string     `json:"updated_at,omitempty"`
}

var errInvalidSiteIconPack = errors.New("invalid site icon pack")

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
	var pack SiteIconPack
	var iconsJSON string
	err := d.db.QueryRow("SELECT pack_name, description, source_url, icons_json, updated_at FROM site_icon_pack WHERE id=1").Scan(&pack.Name, &pack.Description, &pack.SourceURL, &iconsJSON, &pack.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteIconPack{Icons: []SiteIcon{}}, nil
	}
	if err != nil {
		return SiteIconPack{}, err
	}
	if err := json.Unmarshal([]byte(iconsJSON), &pack.Icons); err != nil {
		return SiteIconPack{}, fmt.Errorf("decode stored site icon pack: %w", err)
	}
	if pack.Icons == nil {
		pack.Icons = []SiteIcon{}
	}
	return pack, nil
}

func (d *DB) SaveSiteIconPack(pack SiteIconPack) error {
	if len(pack.Icons) == 0 || len(pack.Icons) > maxSiteIconCount {
		return errInvalidSiteIconPack
	}
	pack.Name = strings.TrimSpace(pack.Name)
	pack.Description = strings.TrimSpace(pack.Description)
	pack.SourceURL = strings.TrimSpace(pack.SourceURL)
	if len(pack.Name) > 200 || len(pack.Description) > 1000 {
		return errInvalidSiteIconPack
	}
	if pack.SourceURL != "" {
		var err error
		pack.SourceURL, err = normalizeSiteIconURL(pack.SourceURL)
		if err != nil {
			return err
		}
	}
	normalizedIcons := make([]SiteIcon, 0, len(pack.Icons))
	seen := make(map[string]struct{}, len(pack.Icons))
	for _, icon := range pack.Icons {
		name, err := normalizeSiteIconName(icon.Name)
		if err != nil || name == "" {
			return errInvalidSiteIconPack
		}
		imageURL, err := normalizeSiteIconURL(icon.URL)
		if err != nil {
			return errInvalidSiteIconPack
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		normalizedIcons = append(normalizedIcons, SiteIcon{Name: name, URL: imageURL})
	}
	if len(normalizedIcons) == 0 {
		return errInvalidSiteIconPack
	}
	pack.Icons = normalizedIcons
	iconsJSON, err := json.Marshal(pack.Icons)
	if err != nil {
		return err
	}
	if len(iconsJSON) > maxSiteIconPackBytes {
		return fmt.Errorf("icon pack is too large")
	}
	_, err = d.db.Exec("INSERT INTO site_icon_pack (id, pack_name, description, source_url, icons_json, updated_at) VALUES (1,?,?,?,?,CURRENT_TIMESTAMP) ON CONFLICT(id) DO UPDATE SET pack_name=excluded.pack_name, description=excluded.description, source_url=excluded.source_url, icons_json=excluded.icons_json, updated_at=CURRENT_TIMESTAMP", pack.Name, pack.Description, pack.SourceURL, string(iconsJSON))
	return err
}

func (d *DB) ClearSiteIconPack() error {
	_, err := d.db.Exec("UPDATE site_icon_pack SET pack_name='', description='', source_url='', icons_json='[]', updated_at=CURRENT_TIMESTAMP WHERE id=1")
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

func (p SiteIconPack) normalizeSelection(name, imageURL string) (string, string, error) {
	name, imageURL, err := normalizeSiteIconSelection(name, imageURL)
	if err != nil || name == "" {
		return name, imageURL, err
	}
	if icon, ok := p.icon(name); ok {
		if imageURL != icon.URL {
			return "", "", errors.New("icon url does not match the selected icon pack entry")
		}
		return icon.Name, icon.URL, nil
	}
	// Keep an existing selection usable after an icon pack is replaced. The URL
	// has already passed the strict HTTPS validation above.
	return name, imageURL, nil
}

func iconPackUpdatedAt() string { return time.Now().UTC().Format(time.RFC3339) }
