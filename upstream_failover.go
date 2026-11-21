package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	maxFailoverTargets        = 8
	failoverCooldown          = 30 * time.Second
	failoverPreferredLifetime = 5 * time.Minute
)

// upstreamFailoverState is deliberately process-local. A transient network
// failure must not rewrite a site's configured priority in the database.
type upstreamFailoverState struct {
	mu             sync.Mutex
	preferred      string
	preferredUntil time.Time
	failedUntil    map[string]time.Time
	primaryProbeAt time.Time
	primaryProbe   bool
	generation     uint64
}

type failoverSelection struct {
	targets    []*url.URL
	generation uint64
	primaryKey string
	probe      bool
}

func newUpstreamFailoverState() *upstreamFailoverState {
	return &upstreamFailoverState{failedUntil: make(map[string]time.Time)}
}

func upstreamTargetKey(target *url.URL) string {
	if target == nil {
		return ""
	}
	return canonicalTargetKey(target) + target.EscapedPath()
}

func (s *upstreamFailoverState) ordered(targets []*url.URL, now time.Time) failoverSelection {
	if len(targets) < 2 || s == nil {
		return failoverSelection{targets: append([]*url.URL(nil), targets...)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, until := range s.failedUntil {
		if !until.After(now) {
			delete(s.failedUntil, key)
		}
	}
	s.expirePreference(now)
	primaryKey := upstreamTargetKey(targets[0])
	selection := failoverSelection{generation: s.generation, primaryKey: primaryKey}
	if s.preferred != "" && s.preferred != primaryKey && !s.primaryProbe && !s.failedUntil[primaryKey].After(now) && !s.primaryProbeAt.After(now) {
		// Reserve one request as the recovery probe. Concurrent requests keep
		// using the active backup until this attempt actually finishes, even if
		// the cooldown elapses or a backup response succeeds in the meantime.
		s.primaryProbe = true
		selection.probe = true
		s.primaryProbeAt = now.Add(failoverCooldown)
	}
	preferred, healthy, cooling := make([]*url.URL, 0, 1), make([]*url.URL, 0, len(targets)), make([]*url.URL, 0, len(targets))
	for _, target := range targets {
		key := upstreamTargetKey(target)
		if key == primaryKey && s.primaryProbe && !selection.probe && s.preferred != primaryKey {
			continue
		}
		if selection.probe && key == primaryKey {
			preferred = append(preferred, target)
		} else if key == s.preferred && !s.failedUntil[key].After(now) {
			preferred = append(preferred, target)
		} else if s.failedUntil[key].After(now) {
			cooling = append(cooling, target)
		} else {
			healthy = append(healthy, target)
		}
	}
	selection.targets = append(append(preferred, healthy...), cooling...)
	return selection
}

func (s *upstreamFailoverState) releaseProbe(selection failoverSelection) {
	if !selection.probe {
		return
	}
	s.mu.Lock()
	s.primaryProbe = false
	s.mu.Unlock()
}

// expirePreference must be called with the state locked.
func (s *upstreamFailoverState) expirePreference(now time.Time) {
	if s.preferred != "" && !s.preferredUntil.After(now) {
		s.preferred = ""
		s.preferredUntil = time.Time{}
		s.generation++
	}
}

// A primary probe can finish while another request reports a backup failure.
// Other results must belong to the current preference to change it.
func (s *upstreamFailoverState) acceptsResult(key string, selection failoverSelection) bool {
	return selection.generation == s.generation || (selection.probe && key == selection.primaryKey)
}

func (s *upstreamFailoverState) markFailure(target *url.URL, now time.Time, selection *failoverSelection) {
	if s == nil || target == nil {
		return
	}
	key := upstreamTargetKey(target)
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.acceptsResult(key, *selection) {
		return
	}
	s.failedUntil[key] = now.Add(failoverCooldown)
	if key == selection.primaryKey {
		s.primaryProbeAt = now.Add(failoverCooldown)
	}
	if s.preferred == key {
		s.preferred = ""
		s.preferredUntil = time.Time{}
		s.generation++
		selection.generation = s.generation
	}
}

func (s *upstreamFailoverState) markSuccess(target *url.URL, now time.Time, selection failoverSelection) {
	if s == nil || target == nil {
		return
	}
	key := upstreamTargetKey(target)
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A request which started before a successful switch cannot undo that
	// choice merely because its old line returned later.
	if !s.acceptsResult(key, selection) {
		return
	}
	delete(s.failedUntil, key)
	if s.preferred != key {
		s.generation++
	}
	s.preferred = key
	s.preferredUntil = now.Add(failoverPreferredLifetime)
	if key == selection.primaryKey {
		s.primaryProbeAt = time.Time{}
	}
}

func (s *upstreamFailoverState) preferredTarget(targets []*url.URL, now time.Time) (*url.URL, failoverSelection) {
	if s == nil || len(targets) < 2 {
		return nil, failoverSelection{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expirePreference(now)
	selection := failoverSelection{generation: s.generation, primaryKey: upstreamTargetKey(targets[0])}
	if s.preferred != "" && !s.failedUntil[s.preferred].After(now) {
		for _, target := range targets {
			if upstreamTargetKey(target) == s.preferred {
				return target, selection
			}
		}
	}
	return nil, selection
}

func isRetryableUpstreamStatus(status int) bool {
	return status == http.StatusInternalServerError || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

type failoverRequestContextKey struct{}
type configuredUpstreamContextKey struct{}

type failoverRequestPlan struct {
	originalURL      *url.URL
	targets          []*url.URL
	active           *url.URL
	maxBodyBytes     int64
	playbackInfoPost bool
}

func withFailoverRequestPlan(req *http.Request, originalURL *url.URL, targets []*url.URL, active *url.URL, maxBodyBytes int64) *http.Request {
	if req == nil || originalURL == nil || len(targets) < 2 {
		return req
	}
	copyURL := *originalURL
	plan := failoverRequestPlan{
		originalURL:      &copyURL,
		targets:          append([]*url.URL(nil), targets...),
		active:           active,
		maxBodyBytes:     maxBodyBytes,
		playbackInfoPost: req.Method == http.MethodPost && isPlaybackInfoRequest(copyURL.Path),
	}
	return req.WithContext(context.WithValue(req.Context(), failoverRequestContextKey{}, plan))
}

func isFailoverRequestEligible(req *http.Request, plan failoverRequestPlan) bool {
	if req == nil || hasUpgradeIntent(req) {
		return false
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead && (req.Method != http.MethodPost || !plan.playbackInfoPost) {
		return false
	}
	hasBody := req.Body != nil && req.Body != http.NoBody || req.ContentLength != 0 || len(req.TransferEncoding) != 0 || len(req.Trailer) != 0
	if !hasBody {
		return true
	}
	return req.GetBody != nil && req.ContentLength > 0 && plan.maxBodyBytes > 0 && req.ContentLength <= plan.maxBodyBytes &&
		len(req.TransferEncoding) == 0 && len(req.Trailer) == 0
}

func failoverPlanFromRequest(req *http.Request) (failoverRequestPlan, bool) {
	if req == nil {
		return failoverRequestPlan{}, false
	}
	plan, ok := req.Context().Value(failoverRequestContextKey{}).(failoverRequestPlan)
	return plan, ok && plan.originalURL != nil && len(plan.targets) > 1
}

type failoverTransport struct {
	base  http.RoundTripper
	state *upstreamFailoverState
}

func (*failoverTransport) requestDiagnosticsOwnsRoundTrip() {}

func (t *failoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	plan, enabled := failoverPlanFromRequest(req)
	if !enabled {
		return roundTripWithRequestDiagnostics(t.base, req, "configured")
	}
	if !isFailoverRequestEligible(req, plan) {
		target, selection := t.state.preferredTarget(plan.targets, time.Now())
		if target == nil || upstreamTargetKey(target) == upstreamTargetKey(plan.active) {
			return roundTripWithRequestDiagnostics(t.base, req, "configured")
		}
		attempt := cloneFailoverInitialAttempt(req, plan, target)
		if tracker := backendAddressTrackerFromContext(attempt.Context()); tracker != nil {
			tracker.SetURL(target)
		}
		resp, err := roundTripWithRequestDiagnostics(t.base, attempt, "configured")
		if err != nil {
			if !isClientRequestCancellation(originalRequestContext(req.Context()), err) {
				t.state.markFailure(target, time.Now(), &selection)
			}
			return nil, err
		}
		if isRetryableUpstreamStatus(resp.StatusCode) {
			t.state.markFailure(target, time.Now(), &selection)
		} else if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest {
			t.state.markSuccess(target, time.Now(), selection)
		}
		return resp, nil
	}
	selection := t.state.ordered(plan.targets, time.Now())
	defer t.state.releaseProbe(selection)
	targets := selection.targets
	var lastErr error
	for index, target := range targets {
		attempt := req
		if index > 0 || upstreamTargetKey(target) != upstreamTargetKey(plan.active) {
			var cloneErr error
			attempt, cloneErr = cloneFailoverAttempt(req, plan, target)
			if cloneErr != nil {
				return nil, cloneErr
			}
		}
		if tracker := backendAddressTrackerFromContext(attempt.Context()); tracker != nil {
			tracker.SetURL(target)
		}
		if index > 0 {
			recordRequestDiagnosticsRetry(attempt.Context())
		}
		resp, err := roundTripWithRequestDiagnostics(t.base, attempt, "configured")
		if err != nil {
			if isClientRequestCancellation(originalRequestContext(req.Context()), err) {
				return nil, err
			}
			t.state.markFailure(target, time.Now(), &selection)
			lastErr = err
			continue
		}
		if isRetryableUpstreamStatus(resp.StatusCode) {
			t.state.markFailure(target, time.Now(), &selection)
			if index+1 < len(targets) {
				resp.Body.Close()
				continue
			}
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest {
			t.state.markSuccess(target, time.Now(), selection)
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("all upstream failover attempts failed")
}

func cloneFailoverInitialAttempt(req *http.Request, plan failoverRequestPlan, target *url.URL) *http.Request {
	clone := req.Clone(context.WithValue(req.Context(), configuredUpstreamContextKey{}, target))
	clone.Header = req.Header.Clone()
	// This request has not been sent yet, so its original body can move to the
	// selected active line without requiring a replay contract.
	clone.Body = req.Body
	cloneURL := *plan.originalURL
	applyUpstreamURL(&cloneURL, target)
	clone.URL = &cloneURL
	clone.Host = target.Host
	return clone
}

func cloneFailoverAttempt(req *http.Request, plan failoverRequestPlan, target *url.URL) (*http.Request, error) {
	clone := req.Clone(context.WithValue(req.Context(), configuredUpstreamContextKey{}, target))
	clone.Header = req.Header.Clone()
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return nil, errors.New("failover request body is not replayable")
		}
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("replay failover request body: %w", err)
		}
		clone.Body = body
	}
	cloneURL := *plan.originalURL
	applyUpstreamURL(&cloneURL, target)
	clone.URL = &cloneURL
	clone.Host = target.Host
	return clone, nil
}

func prepareFailoverPlaybackInfoBody(r *http.Request, maxBodyBytes int64) error {
	if r == nil || r.Method != http.MethodPost || r.URL == nil || !isPlaybackInfoRequest(r.URL.Path) ||
		r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 || r.GetBody != nil {
		return nil
	}
	if r.ContentLength < 0 || maxBodyBytes <= 0 || r.ContentLength > maxBodyBytes ||
		len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
		return nil
	}
	body := make([]byte, int(r.ContentLength))
	if _, err := io.ReadFull(r.Body, body); err != nil {
		_ = r.Body.Close()
		return fmt.Errorf("read replayable PlaybackInfo body: %w", err)
	}
	var extra [1]byte
	if count, err := r.Body.Read(extra[:]); count != 0 || !errors.Is(err, io.EOF) {
		_ = r.Body.Close()
		if err == nil {
			err = errors.New("request body exceeds its declared length")
		}
		return fmt.Errorf("validate replayable PlaybackInfo body: %w", err)
	}
	if err := r.Body.Close(); err != nil {
		return fmt.Errorf("close replayable PlaybackInfo body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return nil
}

func parseFailoverTargets(primary string, raw []string) ([]*url.URL, error) {
	primaryTarget, err := normalizeTargetURL(primary)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFailoverTargets-1 {
		return nil, errors.New("failover_targets must contain at most 7 backup targets")
	}
	targets := []*url.URL{primaryTarget}
	seen := map[string]bool{upstreamTargetKey(primaryTarget): true}
	for _, value := range raw {
		target, targetErr := normalizeTargetURL(value)
		if targetErr != nil {
			return nil, targetErr
		}
		key := upstreamTargetKey(target)
		if seen[key] {
			return nil, errors.New("failover_targets must not repeat the primary or another backup target")
		}
		seen[key] = true
		targets = append(targets, target)
	}
	return targets, nil
}
