package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"meridian/web"
	"strings"
	"sync"
)

type builtinSiteIconPackSource struct {
	File      string
	SourceURL string
}

var builtinSiteIconPackSources = [...]builtinSiteIconPackSource{
	{File: "lige.json", SourceURL: "https://raw.githubusercontent.com/lige47/lige_icon/main/lige-emby-icon.json"},
	{File: "sakura.json", SourceURL: "https://raw.githubusercontent.com/baiitang/Sakura/main/Fileball/Yuan/tubiao.json"},
	{File: "liangyou.json", SourceURL: "https://raw.githubusercontent.com/MaddestAlistar/LiangYou-IconLibrary/main/LiangYou-Emby-Icons.json"},
}

type builtinSiteIconPackSeed struct {
	Name        string
	Description string
	SourceURL   string
	IconsJSON   string
}

var loadBuiltinSiteIconPackSeeds = sync.OnceValues(func() ([]builtinSiteIconPackSeed, error) {
	seeds := make([]builtinSiteIconPackSeed, 0, len(builtinSiteIconPackSources))
	for _, source := range builtinSiteIconPackSources {
		raw, err := web.StaticFiles.ReadFile("static/icon-packs/" + source.File)
		if err != nil {
			return nil, fmt.Errorf("read builtin icon pack %s: %w", source.File, err)
		}
		pack, err := parseSiteIconPack(raw)
		if err != nil {
			return nil, fmt.Errorf("parse builtin icon pack %s: %w", source.File, err)
		}
		iconsJSON, err := json.Marshal(pack.Icons)
		if err != nil {
			return nil, fmt.Errorf("encode builtin icon pack %s: %w", source.File, err)
		}
		seeds = append(seeds, builtinSiteIconPackSeed{
			Name: pack.Name, Description: pack.Description, SourceURL: source.SourceURL, IconsJSON: string(iconsJSON),
		})
	}
	return seeds, nil
})

func isBuiltinSiteIconPack(sourceURL string) bool {
	for _, source := range builtinSiteIconPackSources {
		if strings.TrimSpace(sourceURL) == source.SourceURL {
			return true
		}
	}
	return false
}

func seedBuiltinSiteIconPacks(ctx context.Context, conn *sql.Conn) error {
	seeds, err := loadBuiltinSiteIconPackSeeds()
	if err != nil {
		return err
	}
	var activeCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM site_icon_packs WHERE is_active=1").Scan(&activeCount); err != nil {
		return err
	}
	for _, seed := range seeds {
		var existingCount int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM site_icon_packs WHERE trim(source_url)=?", seed.SourceURL).Scan(&existingCount); err != nil {
			return err
		}
		if existingCount != 0 {
			continue
		}
		isActive := 0
		if activeCount == 0 {
			isActive = 1
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO site_icon_packs(pack_name,description,source_url,icons_json,is_active) VALUES(?,?,?,?,?)", seed.Name, seed.Description, seed.SourceURL, seed.IconsJSON, isActive); err != nil {
			return fmt.Errorf("seed builtin icon pack: %w", err)
		}
		activeCount += isActive
	}
	return nil
}
