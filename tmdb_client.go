package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	tmdbAPIBaseURL     = "https://api.themoviedb.org/3"
	tmdbImageBaseURL   = "https://image.tmdb.org/t/p/w500"
	tmdbHTTPTimeout    = 10 * time.Second
	tmdbJSONMaxBytes   = 1 << 20
	tmdbPosterMaxBytes = 8 << 20
	tmdbCacheLifetime  = 180 * 24 * time.Hour
)

type tmdbAPIError struct {
	Code       string
	StatusCode int
	RetryAfter time.Duration
}

func (e *tmdbAPIError) Error() string {
	if e == nil {
		return "TMDB request failed"
	}
	return "TMDB request failed: " + e.Code
}

type tmdbClient struct {
	httpClient *http.Client
	apiBase    string
	imageBase  string
}

func newTMDBClient(client *http.Client) *tmdbClient {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		client = &http.Client{
			Timeout:   tmdbHTTPTimeout,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &tmdbClient{httpClient: client, apiBase: tmdbAPIBaseURL, imageBase: tmdbImageBaseURL}
}

func newTMDBClientForTest(client *http.Client, apiBase, imageBase string) *tmdbClient {
	c := newTMDBClient(client)
	c.apiBase = strings.TrimRight(apiBase, "/")
	c.imageBase = strings.TrimRight(imageBase, "/")
	return c
}

func boundedRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var wait time.Duration
	if seconds, err := strconv.Atoi(value); err == nil {
		wait = time.Duration(seconds) * time.Second
	} else if when, err := http.ParseTime(value); err == nil {
		wait = when.Sub(now)
	}
	if wait <= 0 {
		return 0
	}
	if wait < time.Minute {
		return time.Minute
	}
	if wait > 6*time.Hour {
		return 6 * time.Hour
	}
	return wait
}

func (c *tmdbClient) doJSON(ctx context.Context, token, endpoint string, query url.Values, dst any) error {
	if c == nil || c.httpClient == nil {
		return &tmdbAPIError{Code: "client_unavailable"}
	}
	parsed, err := url.Parse(strings.TrimRight(c.apiBase, "/") + "/" + strings.TrimLeft(endpoint, "/"))
	if err != nil {
		return &tmdbAPIError{Code: "invalid_endpoint"}
	}
	parsed.RawQuery = query.Encode()
	// #nosec G704 -- production clients use the compile-time TMDB HTTPS base URL;
	// the only alternate base is supplied by the package-private test constructor.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return &tmdbAPIError{Code: "invalid_request"}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Meridian/"+strings.TrimPrefix(appVersion, "v"))
	// #nosec G704 -- the request URL is assembled from the fixed production
	// TMDB base plus internally selected endpoints; redirects are rejected.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &tmdbAPIError{Code: "network"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "http_error"
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			code = "auth"
		case resp.StatusCode == http.StatusTooManyRequests:
			code = "rate_limited"
		case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooEarly:
			code = "network"
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			code = "client_error"
		case resp.StatusCode >= 500:
			code = "server"
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			code = "redirect"
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return &tmdbAPIError{Code: code, StatusCode: resp.StatusCode, RetryAfter: boundedRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &tmdbAPIError{Code: "invalid_content_type", StatusCode: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, tmdbJSONMaxBytes+1))
	if err != nil {
		return &tmdbAPIError{Code: "read"}
	}
	if len(data) > tmdbJSONMaxBytes {
		return &tmdbAPIError{Code: "response_too_large"}
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return &tmdbAPIError{Code: "invalid_json"}
	}
	return nil
}

func (c *tmdbClient) testCredentials(ctx context.Context, token string) error {
	var response struct {
		Images struct {
			SecureBaseURL string `json:"secure_base_url"`
		} `json:"images"`
	}
	if err := c.doJSON(ctx, token, "/configuration", nil, &response); err != nil {
		return err
	}
	if !strings.HasPrefix(response.Images.SecureBaseURL, "https://") {
		return &tmdbAPIError{Code: "invalid_configuration"}
	}
	return nil
}

type tmdbLookupMedia struct {
	ID             int64
	JobRevision    int64
	MediaType      string
	Title          string
	OriginalTitle  string
	ProductionYear int
	SeriesName     string
	SeasonNumber   int
	EpisodeNumber  int
	TMDBType       string
	TMDBID         int64
	IMDBID         string
	TVDBID         string
}

type tmdbMetadata struct {
	TMDBType          string
	TMDBID            int64
	Title             string
	OriginalTitle     string
	Overview          string
	ReleaseYear       int
	ReleaseDate       string
	PosterPath        string
	BackdropPath      string
	VoteAverage       float64
	Genres            []string
	Status            string
	LastAirDate       string
	NextAirDate       string
	NextSeasonNumber  int
	NextEpisodeNumber int
	NextEpisodeName   string
	SeasonCount       int
	EpisodeCount      int
	Stills            []string
	Cast              []tmdbCastMember
	MatchMethod       string
}

type tmdbCastMember struct {
	Name        string `json:"name"`
	Character   string `json:"character"`
	ProfilePath string `json:"profile_path,omitempty"`
}

type tmdbCredits struct {
	Cast []tmdbCastMember `json:"cast"`
}

type tmdbGenre struct {
	Name string `json:"name"`
}

type tmdbImage struct {
	FilePath string `json:"file_path"`
}

type tmdbNextEpisode struct {
	AirDate       string `json:"air_date"`
	SeasonNumber  int    `json:"season_number"`
	EpisodeNumber int    `json:"episode_number"`
	Name          string `json:"name"`
}

type tmdbImages struct {
	Backdrops []tmdbImage `json:"backdrops"`
}

type tmdbSearchItem struct {
	ID               int64            `json:"id"`
	Title            string           `json:"title"`
	OriginalTitle    string           `json:"original_title"`
	Name             string           `json:"name"`
	OriginalName     string           `json:"original_name"`
	Overview         string           `json:"overview"`
	PosterPath       string           `json:"poster_path"`
	BackdropPath     string           `json:"backdrop_path"`
	ReleaseDate      string           `json:"release_date"`
	FirstAirDate     string           `json:"first_air_date"`
	LastAirDate      string           `json:"last_air_date"`
	VoteAverage      float64          `json:"vote_average"`
	Genres           []tmdbGenre      `json:"genres"`
	Status           string           `json:"status"`
	NumberOfSeasons  int              `json:"number_of_seasons"`
	NumberOfEpisodes int              `json:"number_of_episodes"`
	NextEpisodeToAir *tmdbNextEpisode `json:"next_episode_to_air,omitempty"`
	Images           *tmdbImages      `json:"images,omitempty"`
	Credits          *tmdbCredits     `json:"credits,omitempty"`
}

func normalizeTMDBCast(value []tmdbCastMember) []tmdbCastMember {
	cast := make([]tmdbCastMember, 0, 20)
	for _, member := range value {
		name := requestLogSafeText(member.Name, 256)
		if name == "" {
			continue
		}
		profilePath := strings.TrimSpace(member.ProfilePath)
		if !validTMDBImagePath(profilePath) {
			profilePath = ""
		}
		cast = append(cast, tmdbCastMember{
			Name:        name,
			Character:   requestLogSafeText(member.Character, 256),
			ProfilePath: profilePath,
		})
		if len(cast) == 20 {
			break
		}
	}
	return cast
}

func normalizeTMDBDate(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 32 {
		return ""
	}
	if value == "" {
		return ""
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return ""
	}
	return value
}

func normalizeTMDBGenres(value []tmdbGenre) []string {
	genres := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	for _, genre := range value {
		name := requestLogSafeText(genre.Name, 128)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		genres = append(genres, name)
		if len(genres) == 8 {
			break
		}
	}
	return genres
}

func validTMDBImagePath(value string) bool {
	if len(value) < 2 || len(value) > 256 || value[0] != '/' || strings.Contains(value, "\\") || strings.Contains(value, "..") || strings.ContainsAny(value, "?#\x00") {
		return false
	}
	switch strings.ToLower(path.Ext(value)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	default:
		return false
	}
}

func normalizeTMDBImages(value *tmdbImages) []string {
	if value == nil {
		return []string{}
	}
	paths := make([]string, 0, 12)
	seen := make(map[string]struct{}, 12)
	for _, image := range value.Backdrops {
		path := strings.TrimSpace(image.FilePath)
		if !validTMDBImagePath(path) {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
		if len(paths) == 12 {
			break
		}
	}
	return paths
}

func (item tmdbSearchItem) metadata(tmdbType, method string) tmdbMetadata {
	title, original, date := item.Title, item.OriginalTitle, item.ReleaseDate
	if tmdbType == "tv" {
		title, original, date = item.Name, item.OriginalName, item.FirstAirDate
	}
	stills := normalizeTMDBImages(item.Images)
	backdropPath := strings.TrimSpace(item.BackdropPath)
	if !validTMDBImagePath(backdropPath) && len(stills) > 0 {
		backdropPath = stills[0]
	}
	metadata := tmdbMetadata{
		TMDBType: tmdbType, TMDBID: item.ID, Title: strings.TrimSpace(title), OriginalTitle: strings.TrimSpace(original),
		Overview: strings.TrimSpace(item.Overview), ReleaseYear: yearFromTMDBDate(date), ReleaseDate: normalizeTMDBDate(date),
		PosterPath: strings.TrimSpace(item.PosterPath), BackdropPath: backdropPath, VoteAverage: item.VoteAverage,
		Genres: normalizeTMDBGenres(item.Genres), Status: requestLogSafeText(item.Status, 128), LastAirDate: normalizeTMDBDate(item.LastAirDate),
		SeasonCount: item.NumberOfSeasons, EpisodeCount: item.NumberOfEpisodes, Stills: stills, MatchMethod: method,
		Cast: func() []tmdbCastMember {
			if item.Credits == nil {
				return []tmdbCastMember{}
			}
			return normalizeTMDBCast(item.Credits.Cast)
		}(),
	}
	if item.NextEpisodeToAir != nil {
		metadata.NextAirDate = normalizeTMDBDate(item.NextEpisodeToAir.AirDate)
		metadata.NextSeasonNumber = item.NextEpisodeToAir.SeasonNumber
		metadata.NextEpisodeNumber = item.NextEpisodeToAir.EpisodeNumber
		metadata.NextEpisodeName = requestLogSafeText(item.NextEpisodeToAir.Name, 256)
	}
	if metadata.VoteAverage < 0 || metadata.VoteAverage > 10 {
		metadata.VoteAverage = 0
	}
	return metadata
}

func yearFromTMDBDate(value string) int {
	if len(value) < 4 {
		return 0
	}
	year, _ := strconv.Atoi(value[:4])
	if year < 1800 || year > 3000 {
		return 0
	}
	return year
}

func tmdbTypeForMedia(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "movie", "video":
		return "movie"
	case "series", "season", "episode", "tv":
		return "tv"
	default:
		return ""
	}
}

func normalizeTMDBTitle(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func safeExternalID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func (c *tmdbClient) lookupMedia(ctx context.Context, token, language string, media tmdbLookupMedia) (tmdbMetadata, error) {
	tmdbType := tmdbTypeForMedia(media.MediaType)
	if tmdbType == "" {
		return tmdbMetadata{}, &tmdbAPIError{Code: "unsupported_media_type"}
	}
	if media.TMDBType == "movie" || media.TMDBType == "tv" {
		tmdbType = media.TMDBType
	}
	// Emby/Jellyfin commonly put the TMDB episode ID in an Episode item. The
	// TMDB v3 detail route for episodes needs series/season/episode coordinates,
	// so never misroute that ID to /tv/{series_id}; fall back to the series title.
	if media.TMDBID > 0 && strings.ToLower(strings.TrimSpace(media.MediaType)) != "episode" {
		var item tmdbSearchItem
		if err := c.doJSON(ctx, token, fmt.Sprintf("/%s/%d", tmdbType, media.TMDBID), url.Values{"language": {language}, "append_to_response": {"credits,images"}}, &item); err != nil {
			return tmdbMetadata{}, err
		}
		// This response is already the detail document, including the requested
		// credits/images expansion; do not issue the same detail request twice.
		return item.metadata(tmdbType, "tmdb_id"), nil
	}
	for _, external := range []struct {
		id     string
		source string
	}{
		{media.IMDBID, "imdb_id"},
		{media.TVDBID, "tvdb_id"},
	} {
		if !safeExternalID(external.id) {
			continue
		}
		var result struct {
			MovieResults []tmdbSearchItem `json:"movie_results"`
			TVResults    []tmdbSearchItem `json:"tv_results"`
		}
		query := url.Values{"external_source": {external.source}, "language": {language}}
		if err := c.doJSON(ctx, token, "/find/"+url.PathEscape(external.id), query, &result); err != nil {
			return tmdbMetadata{}, err
		}
		matches := result.MovieResults
		if tmdbType == "tv" {
			matches = result.TVResults
		}
		if len(matches) == 1 {
			return c.enrichTMDBMetadata(ctx, token, language, matches[0].metadata(tmdbType, external.source)), nil
		}
	}
	queryTitle := strings.TrimSpace(media.Title)
	if tmdbType == "tv" && strings.TrimSpace(media.SeriesName) != "" {
		queryTitle = strings.TrimSpace(media.SeriesName)
	}
	if queryTitle == "" || len(queryTitle) > 256 {
		return tmdbMetadata{}, &tmdbAPIError{Code: "insufficient_metadata"}
	}
	query := url.Values{"query": {queryTitle}, "language": {language}, "include_adult": {"false"}, "page": {"1"}}
	// An Episode's ProductionYear is its air year, not necessarily the series'
	// first-air year. Applying it to /search/tv would reject long-running shows.
	if media.ProductionYear > 0 && strings.ToLower(strings.TrimSpace(media.MediaType)) != "episode" {
		if tmdbType == "movie" {
			query.Set("year", strconv.Itoa(media.ProductionYear))
		} else {
			query.Set("first_air_date_year", strconv.Itoa(media.ProductionYear))
		}
	}
	var result struct {
		Results []tmdbSearchItem `json:"results"`
	}
	if err := c.doJSON(ctx, token, "/search/"+tmdbType, query, &result); err != nil {
		return tmdbMetadata{}, err
	}
	wanted := normalizeTMDBTitle(queryTitle)
	exact := make([]tmdbSearchItem, 0, 2)
	for _, candidate := range result.Results {
		meta := candidate.metadata(tmdbType, "title")
		if normalizeTMDBTitle(meta.Title) != wanted && normalizeTMDBTitle(meta.OriginalTitle) != wanted {
			continue
		}
		if media.ProductionYear > 0 && meta.ReleaseYear > 0 && strings.ToLower(strings.TrimSpace(media.MediaType)) != "episode" && media.ProductionYear != meta.ReleaseYear {
			continue
		}
		exact = append(exact, candidate)
	}
	if len(exact) != 1 {
		return tmdbMetadata{}, &tmdbAPIError{Code: "no_confident_match"}
	}
	metadata := exact[0].metadata(tmdbType, "title")
	// Search results do not include credits. Fetch the selected title's detail
	// document once so the history dialog can show a bounded cast list. If the
	// optional detail call is unavailable, retain the already useful search
	// metadata and let the normal cache lifetime retry it later.
	return c.enrichTMDBMetadata(ctx, token, language, metadata), nil
}

// enrichTMDBMetadata follows a successful search/find match with the detail
// document. Search and /find responses intentionally omit credits and images,
// which otherwise makes valid TMDB matches look like they have no actors or
// stills in the watch-history dialog.
func (c *tmdbClient) enrichTMDBMetadata(ctx context.Context, token, language string, metadata tmdbMetadata) tmdbMetadata {
	if metadata.TMDBID <= 0 || (metadata.TMDBType != "movie" && metadata.TMDBType != "tv") {
		return metadata
	}
	var detail tmdbSearchItem
	if err := c.doJSON(ctx, token, fmt.Sprintf("/%s/%d", metadata.TMDBType, metadata.TMDBID), url.Values{"language": {language}, "append_to_response": {"credits,images"}}, &detail); err != nil {
		return metadata
	}
	detailed := detail.metadata(metadata.TMDBType, metadata.MatchMethod)
	if detailed.Title != "" {
		metadata.Title = detailed.Title
	}
	if detailed.OriginalTitle != "" {
		metadata.OriginalTitle = detailed.OriginalTitle
	}
	if detailed.Overview != "" {
		metadata.Overview = detailed.Overview
	}
	if detailed.ReleaseYear > 0 {
		metadata.ReleaseYear = detailed.ReleaseYear
	}
	if detailed.ReleaseDate != "" {
		metadata.ReleaseDate = detailed.ReleaseDate
	}
	if detailed.PosterPath != "" {
		metadata.PosterPath = detailed.PosterPath
	}
	if detailed.BackdropPath != "" {
		metadata.BackdropPath = detailed.BackdropPath
	}
	if detailed.VoteAverage > 0 {
		metadata.VoteAverage = detailed.VoteAverage
	}
	if len(detailed.Genres) > 0 {
		metadata.Genres = detailed.Genres
	}
	if detailed.Status != "" {
		metadata.Status = detailed.Status
	}
	if detailed.LastAirDate != "" {
		metadata.LastAirDate = detailed.LastAirDate
	}
	if detailed.NextAirDate != "" {
		metadata.NextAirDate = detailed.NextAirDate
		metadata.NextSeasonNumber = detailed.NextSeasonNumber
		metadata.NextEpisodeNumber = detailed.NextEpisodeNumber
		metadata.NextEpisodeName = detailed.NextEpisodeName
	}
	if detailed.SeasonCount > 0 {
		metadata.SeasonCount = detailed.SeasonCount
	}
	if detailed.EpisodeCount > 0 {
		metadata.EpisodeCount = detailed.EpisodeCount
	}
	// A successful detail response is authoritative for bounded collections,
	// including an empty cast/stills list.
	metadata.Cast = detailed.Cast
	metadata.Stills = detailed.Stills
	return metadata
}
