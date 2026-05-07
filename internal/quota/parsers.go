package quota

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseClaude extracts samples from Anthropic Messages API rate-limit headers.
// Anthropic emits triples per scheme on every successful response:
//
//	anthropic-ratelimit-requests-{limit,remaining,reset}
//	anthropic-ratelimit-tokens-{limit,remaining,reset}
//	anthropic-ratelimit-input-tokens-{limit,remaining,reset}
//	anthropic-ratelimit-output-tokens-{limit,remaining,reset}
//
// Reset values are RFC3339 timestamps. We additionally surface
// `anthropic-ratelimit-priority-tier` and the OAuth 5h rolling-window message
// counter when present.
func parseClaude(_ int, h http.Header) []Sample {
	if h == nil {
		return nil
	}
	schemes := []struct{ key, scheme, unit string }{
		{"anthropic-ratelimit-requests", "anthropic_requests", "requests"},
		{"anthropic-ratelimit-tokens", "anthropic_tokens", "tokens"},
		{"anthropic-ratelimit-input-tokens", "anthropic_input_tokens", "tokens"},
		{"anthropic-ratelimit-output-tokens", "anthropic_output_tokens", "tokens"},
	}
	out := make([]Sample, 0, len(schemes))
	for _, s := range schemes {
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

// readTime parses an RFC3339 timestamp. Returns zero time if missing/invalid.
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
