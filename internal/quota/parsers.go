package quota

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseCodexUsage extracts ChatGPT/Codex subscription windows returned by
// GET /backend-api/wham/usage. Unlike API-key rate limits, Codex OAuth quota
// is reported in the response body as a used percentage for rolling windows.
func ParseCodexUsage(body []byte) ([]Sample, error) {
	type window struct {
		UsedPercent        float64 `json:"used_percent"`
		WindowMinutes      int64   `json:"window_minutes"`
		LimitWindowSeconds int64   `json:"limit_window_seconds"`
		ResetsAt           int64   `json:"resets_at"`
		ResetAt            int64   `json:"reset_at"`
	}
	type rateLimit struct {
		Primary   *window `json:"primary_window"`
		Secondary *window `json:"secondary_window"`
	}
	var payload struct {
		RateLimit *rateLimit `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode codex usage: %w", err)
	}
	if payload.RateLimit == nil {
		return nil, fmt.Errorf("codex usage response has no rate_limit")
	}
	out := make([]Sample, 0, 2)
	appendWindow := func(position string, w *window) {
		if w == nil {
			return
		}
		minutes := w.WindowMinutes
		if minutes <= 0 && w.LimitWindowSeconds > 0 {
			minutes = w.LimitWindowSeconds / 60
		}
		if minutes <= 0 {
			return
		}
		used := math.Max(0, math.Min(100, w.UsedPercent))
		remaining := int64(math.Round(100 - used))
		scheme := fmt.Sprintf("codex_%s_%dm", position, minutes)
		switch minutes {
		case 300:
			scheme = "codex_5h"
		case 10080:
			scheme = "codex_7d"
		}
		var reset time.Time
		resetAt := w.ResetsAt
		if resetAt <= 0 {
			resetAt = w.ResetAt
		}
		if resetAt > 0 {
			reset = time.Unix(resetAt, 0).UTC()
		}
		out = append(out, Sample{Scheme: scheme, Limit: 100, Remaining: remaining, Unit: "percent", ResetsAt: reset, Source: SourceHeader})
	}
	appendWindow("primary", payload.RateLimit.Primary)
	appendWindow("secondary", payload.RateLimit.Secondary)
	if len(out) == 0 {
		return nil, fmt.Errorf("codex usage response has no quota windows")
	}
	return out, nil
}

// parseClaude extracts samples from Anthropic Messages API rate-limit headers.
//
// Two header families are produced by different Anthropic endpoints:
//
//  1. API-key path (documented at platform.claude.com):
//     anthropic-ratelimit-{requests,tokens,input-tokens,output-tokens}-{limit,remaining,reset}
//
//  2. OAuth-beta path (Pro / Team subscriptions, undocumented but emitted on
//     every successful claude.ai-style /v1/messages call):
//     Anthropic-Ratelimit-Unified-{5h,7d,7d_sonnet}-{Status,Reset,Utilization}
//
// We parse both — most successful OAuth requests will yield only the
// "unified" family, while API-key requests yield only the legacy family.
// Mixed responses just produce more samples.
//
// Utilization is 0.0–1.0. We translate it into Limit=100 / Remaining=
// floor((1-util)*100) so callers get a percent-based view (the absolute
// token / message numbers are not exposed by Anthropic on the OAuth path).
func parseClaude(_ int, h http.Header) []Sample {
	if h == nil {
		return nil
	}
	out := make([]Sample, 0, 6)

	// (1) API-key family
	apiKeyFamily := []struct{ key, scheme, unit string }{
		{"anthropic-ratelimit-requests", "anthropic_requests", "requests"},
		{"anthropic-ratelimit-tokens", "anthropic_tokens", "tokens"},
		{"anthropic-ratelimit-input-tokens", "anthropic_input_tokens", "tokens"},
		{"anthropic-ratelimit-output-tokens", "anthropic_output_tokens", "tokens"},
	}
	for _, s := range apiKeyFamily {
		limit, lOK := readInt(h, s.key+"-limit")
		remaining, rOK := readInt(h, s.key+"-remaining")
		if !lOK && !rOK {
			continue
		}
		out = append(out, Sample{
			Scheme:    s.scheme,
			Limit:     limit,
			Remaining: remaining,
			Unit:      s.unit,
			ResetsAt:  readTime(h, s.key+"-reset"),
			Source:    SourceHeader,
		})
	}

	// (2) OAuth-beta unified family — windows: 5h (rolling), 7d (rolling),
	// 7d_sonnet (per-model 7d rolling for Sonnet).
	for _, window := range []string{"5h", "7d", "7d_sonnet"} {
		prefix := "Anthropic-Ratelimit-Unified-" + window
		util := strings.TrimSpace(h.Get(prefix + "-Utilization"))
		reset := h.Get(prefix + "-Reset")
		status := h.Get(prefix + "-Status")
		if util == "" && reset == "" && status == "" {
			continue
		}
		remaining := int64(0)
		limit := int64(100)
		if util != "" {
			if u, err := strconv.ParseFloat(util, 64); err == nil {
				if u < 0 {
					u = 0
				}
				if u > 1 {
					u = 1
				}
				remaining = int64(math.Round((1 - u) * 100))
			}
		}
		// If status reports exceeded but utilization wasn't sent, force 0.
		if strings.EqualFold(strings.TrimSpace(status), "exceeded") {
			remaining = 0
		}
		out = append(out, Sample{
			Scheme:    "anthropic_unified_" + window,
			Limit:     limit,
			Remaining: remaining,
			Unit:      "percent",
			ResetsAt:  readTime(h, prefix+"-Reset"),
			Source:    SourceHeader,
		})
	}

	return out
}

// parseOpenAI handles `x-ratelimit-{limit,remaining,reset}-{requests,tokens}`
// emitted by Codex/ChatGPT and OpenAI-compat upstreams.
//
// The reset header is RFC3339 from genuine OpenAI but a duration string ("12s",
// "1m30s", "200ms") on some compat servers — we accept both.
func parseOpenAI(_ int, h http.Header) []Sample {
	if h == nil {
		return nil
	}
	schemes := []struct{ suffix, scheme, unit string }{
		{"requests", "openai_requests", "requests"},
		{"tokens", "openai_tokens", "tokens"},
	}
	out := make([]Sample, 0, len(schemes))
	for _, s := range schemes {
		limit, lOK := readInt(h, "x-ratelimit-limit-"+s.suffix)
		remaining, rOK := readInt(h, "x-ratelimit-remaining-"+s.suffix)
		if !lOK && !rOK {
			continue
		}
		out = append(out, Sample{
			Scheme:    s.scheme,
			Limit:     limit,
			Remaining: remaining,
			Unit:      s.unit,
			ResetsAt:  readDurationOrTime(h, "x-ratelimit-reset-"+s.suffix),
			Source:    SourceHeader,
		})
	}
	return out
}

// parseGemini infers a sample from a 429 response. Gemini AI Studio (the
// canonical free-tier path) does not advertise rate-limit headers on success,
// so the fastest signal is `Retry-After` on a quota failure.
//
// The provider documents per-minute and per-day caps; when we see a 429 we
// emit a `gemini_rpm` sample with Remaining=0 and ResetsAt computed from
// Retry-After. Successful calls clear the sample by returning nil so the
// store deletes any stale entry.
func parseGemini(status int, h http.Header) []Sample {
	if status != http.StatusTooManyRequests {
		return nil
	}
	resets := readDurationOrTime(h, "Retry-After")
	if resets.IsZero() {
		// Default to 60s if Retry-After is missing.
		resets = time.Now().Add(60 * time.Second).UTC()
	}
	return []Sample{{
		Scheme:    "gemini_rpm",
		Remaining: 0,
		Limit:     0,
		Unit:      "requests",
		ResetsAt:  resets,
		Source:    SourceInferred,
	}}
}

// readInt returns the parsed int64 value of header key, or (0, false) when
// the header is missing or unparseable.
func readInt(h http.Header, key string) (int64, bool) {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// Some upstreams emit floats ("9999.0"); try float fallback.
		f, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return 0, false
		}
		return int64(f), true
	}
	return n, true
}

// readTime parses a timestamp from a header. Accepts RFC3339, integer Unix
// seconds (Anthropic's OAuth-beta unified family uses this), or floating
// Unix seconds. Returns zero time when missing or unparseable.
func readTime(h http.Header, key string) time.Time {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC()
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		// Anthropic OAuth uses Unix seconds. Heuristic: a value that
		// looks like a Unix epoch (>= 2001-01-01) is treated as such.
		if secs > 1_000_000_000 {
			return time.Unix(secs, 0).UTC()
		}
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f > 1_000_000_000 {
		return time.Unix(int64(f), 0).UTC()
	}
	return time.Time{}
}

// readDurationOrTime accepts either an RFC3339 timestamp or a Go-style
// duration string (e.g. "30s", "1m"), returning the absolute reset time.
// Plain integer seconds (Retry-After) are also handled.
func readDurationOrTime(h http.Header, key string) time.Time {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC()
	}
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(d).UTC()
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Now().Add(time.Duration(secs) * time.Second).UTC()
	}
	return time.Time{}
}
