package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestWatchHistoryNarrowPagePreservesCompleteFieldsFiltersAndRepresentatives(t *testing.T) {
	db := openWatchHistoryTestDB(t)
	first := createWatchHistoryTestSite(t, db, true)
	second := createWatchHistoryTestSite(t, db, true)
	const base = int64(1800000000000)
	type seed struct {
		site                          *Site
		item, title, kind, series     string
		at, position, runtime, tmdbID int64
	}
	seeds := []seed{
		{first, "movie-a", "Movie A", "movie", "", 10, 100, 1000, 1},
		{first, "movie-a", "Movie A", "movie", "", 20, 200, 1000, 1},
		{first, "series-a-1", "Episode One", "episode", " Example Series ", 12, 100, 1000, 2},
		{first, "series-a-2", "Episode Two", "episode", "example series", 30, 950, 1000, 2},
		{second, "series-a-3", "Episode Three", "episode", "example series", 30, 300, 1000, 2},
		{first, "tmdb-a-1", "Episode Four", "episode", "", 14, 100, 1000, 42},
		{first, "tmdb-a-2", "Episode Five", "episode", "", 35, 950, 1000, 42},
		{first, "movie-b", "Movie B", "movie", "", 40, 500, 1000, 3},
		{first, "movie-c", "Movie C", "movie", "", 40, 500, 1000, 4},
		{second, "movie-d", "Movie D", "movie", "", 45, 600, 1000, 5},
		{first, "series-b-1", "Episode Six", "episode", "Completed Series", 50, 100, 0, 6},
		{first, "series-b-2", "Episode Seven", "episode", "Completed Series", 55, 1000, 1000, 6},
	}
	cast := []WatchHistoryCastMember{{Name: "Actor", Character: "Lead", ProfilePath: "/actor.jpg"}}
	var candidates []WatchHistoryEntry
	for i, row := range seeds {
		event := watchHistoryTestEvent(row.site.ID, fmt.Sprintf("page-session-%d", i), base+row.at, row.position)
		event.UpstreamItemID, event.Title, event.OriginalTitle = row.item, row.title, "Original "+row.title
		event.MediaType, event.SeriesName, event.RunTimeTicks = row.kind, row.series, row.runtime
		event.ProductionYear, event.SeasonNumber, event.EpisodeNumber = 2026, 1, i+1
		// Movie sessions share one media row, including its media-level fields.
		if row.item == "movie-a" {
			event.EpisodeNumber = 1
		}
		event.TMDBID = row.tmdbID
		event.TMDBType = "movie"
		if row.kind == "episode" {
			event.TMDBType = "tv"
		}
		event.UserName, event.UserID, event.DeviceID, event.DeviceName, event.ClientName, event.PlaySessionID = "Viewer", "user", "device", "Phone", "Client", fmt.Sprintf("session-%d", i)
		if _, err := db.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil {
			t.Fatal(err)
		}
		var id, mediaID int64
		if err := db.db.QueryRow("SELECT id,media_item_id FROM watch_sessions WHERE site_id=? AND session_hash=?", row.site.ID, event.SessionHash).Scan(&id, &mediaID); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, WatchHistoryEntry{
			ID: id, SiteID: row.site.ID, SiteName: row.site.Name, MediaItemID: mediaID, UpstreamItemID: row.item,
			MediaType: row.kind, Title: row.title, OriginalTitle: event.OriginalTitle, ProductionYear: 2026,
			SeriesName: row.series, SeasonNumber: 1, EpisodeNumber: event.EpisodeNumber, TMDBType: event.TMDBType, TMDBID: row.tmdbID,
			Overview: "A complete overview", PosterPath: "/poster.jpg", BackdropPath: "/backdrop.jpg", ReleaseDate: "2026-01-02", VoteAverage: 8.5,
			Genres: []string{"Drama"}, Status: "Returning Series", LastAirDate: "2026-02-01", NextAirDate: "2026-02-08", NextSeasonNumber: 2, NextEpisodeNumber: 3,
			NextEpisodeName: "Next", SeasonCount: 2, EpisodeCount: 20, Stills: []string{"/still.jpg"}, Cast: cast, MatchStatus: "tmdb_id",
			StartedAtMS: event.ObservedAtMS, LastSeenAtMS: event.ObservedAtMS, PositionTicks: row.position, RunTimeTicks: row.runtime,
			PlayMethod: event.PlayMethod, Completed: watchHistoryCompleted(row.position, row.runtime), sessionHash: event.SessionHash,
			UserName: event.UserName, UserID: event.UserID, DeviceID: event.DeviceID, DeviceName: event.DeviceName, ClientName: event.ClientName, PlaySessionID: event.PlaySessionID,
		})
	}
	if _, err := db.db.Exec(`UPDATE media_items SET overview='A complete overview',poster_path='/poster.jpg',backdrop_path='/backdrop.jpg',release_date='2026-01-02',vote_average=8.5,
		genres_json='["Drama"]',status='Returning Series',last_air_date='2026-02-01',next_air_date='2026-02-08',next_season_number=2,next_episode_number=3,next_episode_name='Next',
		season_count=2,episode_count=20,stills_json='["/still.jpg"]',cast_json='[{"name":"Actor","character":"Lead","profile_path":"/actor.jpg"}]',match_status='tmdb_id'`); err != nil {
		t.Fatal(err)
	}
	filters := []WatchHistoryFilter{
		{Limit: 2}, {SiteID: first.ID, Limit: 2}, {SiteID: second.ID, Limit: 2},
		{MediaType: "movie", Limit: 2}, {MediaType: "episode", Limit: 2},
		{Query: "Episode", Limit: 2}, {Query: "Example", Limit: 2}, {Query: "Original", Limit: 2},
		{FromMS: base + 15, ToMS: base + 50, Limit: 2}, {SiteID: first.ID, MediaType: "episode", FromMS: base + 15, Limit: 2},
	}
	for _, filter := range filters {
		t.Run(fmt.Sprintf("%+v", filter), func(t *testing.T) {
			for page := 0; page < 2; page++ {
				want := watchHistoryExpectedPage(candidates, filter)
				got, err := db.ListWatchHistory(filter)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("page%d complete entries differ\ngot=%+v\nwant=%+v", page+1, got, want)
				}
				if len(got) == 0 {
					break
				}
				last := got[len(got)-1]
				filter.BeforeMS, filter.BeforeID = last.LastSeenAtMS, last.ID
			}
		})
	}
}

// An independent Go model makes the representative-before-cursor contract
// explicit without copying the window query or its column/CTE implementation.
func watchHistoryExpectedPage(candidates []WatchHistoryEntry, filter WatchHistoryFilter) []WatchHistoryEntry {
	groups := make(map[string]WatchHistoryEntry)
	preferred := func(a, b WatchHistoryEntry) bool {
		ap := a.MediaType == "episode" && !a.Completed && a.RunTimeTicks > 0
		bp := b.MediaType == "episode" && !b.Completed && b.RunTimeTicks > 0
		if ap != bp {
			return ap
		}
		return a.LastSeenAtMS > b.LastSeenAtMS || a.LastSeenAtMS == b.LastSeenAtMS && a.ID > b.ID
	}
	for _, entry := range candidates {
		if filter.SiteID > 0 && entry.SiteID != filter.SiteID || filter.MediaType != "" && entry.MediaType != filter.MediaType || filter.FromMS > 0 && entry.LastSeenAtMS < filter.FromMS || filter.ToMS > 0 && entry.LastSeenAtMS > filter.ToMS {
			continue
		}
		q := strings.ToLower(filter.Query)
		if q != "" && !strings.Contains(strings.ToLower(entry.Title), q) && !strings.Contains(strings.ToLower(entry.OriginalTitle), q) && !strings.Contains(strings.ToLower(entry.SeriesName), q) {
			continue
		}
		key := fmt.Sprintf("%d/media/%d", entry.SiteID, entry.MediaItemID)
		if entry.MediaType == "episode" {
			if series := strings.ToLower(strings.TrimSpace(entry.SeriesName)); series != "" {
				key = fmt.Sprintf("%d/name/%s", entry.SiteID, series)
			} else if entry.TMDBType == "tv" && entry.TMDBID > 0 {
				key = fmt.Sprintf("%d/tmdb/%d", entry.SiteID, entry.TMDBID)
			}
		}
		if old, ok := groups[key]; !ok || preferred(entry, old) {
			groups[key] = entry
		}
	}
	result := make([]WatchHistoryEntry, 0, len(groups))
	for _, entry := range groups {
		if filter.BeforeMS > 0 && (entry.LastSeenAtMS > filter.BeforeMS || entry.LastSeenAtMS == filter.BeforeMS && entry.ID >= filter.BeforeID) {
			continue
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].LastSeenAtMS > result[j].LastSeenAtMS || result[i].LastSeenAtMS == result[j].LastSeenAtMS && result[i].ID > result[j].ID
	})
	if len(result) > filter.Limit {
		result = result[:filter.Limit]
	}
	return result
}

func BenchmarkListWatchHistoryMetadataPage(b *testing.B) {
	db, err := openDB(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.Close)
	trafficCacheTestSite(b, db, 1)
	tx, err := db.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i := 1; i <= 2500; i++ {
		kind, series := "movie", ""
		if i%2 == 0 {
			kind, series = "episode", fmt.Sprintf("Series %d", i/20)
		}
		if _, err := tx.Exec(`INSERT INTO media_items(id,site_id,upstream_item_id,media_type,title,series_name,overview,cast_json,created_at_ms,updated_at_ms)
			VALUES(?,1,?,?,?,?,?,'[{"name":"Actor","character":"Lead","profile_path":"/actor.jpg"}]',?,?)`, i, fmt.Sprintf("item-%d", i), kind, fmt.Sprintf("Title %d", i), series, strings.Repeat("metadata ", 228), now, now); err != nil {
			b.Fatal(err)
		}
	}
	for i := 1; i <= 5000; i++ {
		if _, err := tx.Exec("INSERT INTO watch_sessions(site_id,media_item_id,session_hash,started_at_ms,last_seen_at_ms,position_ticks,runtime_ticks,completed) VALUES(1,?,?,?,?,100,1000,?)", (i-1)%2500+1, fmt.Sprintf("session-%d", i), now-int64(i)*60000, now-int64(i)*60000, sqliteBool(i%3 == 0)); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	filter := WatchHistoryFilter{Limit: 25}
	if _, err := db.ListWatchHistory(filter); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.ListWatchHistory(filter); err != nil {
			b.Fatal(err)
		}
	}
}
