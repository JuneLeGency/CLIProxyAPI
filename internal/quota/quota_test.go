package quota

import (
	"net/http"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestParseClaude_AnthropicHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "987")
	h.Set("anthropic-ratelimit-requests-reset", "2026-05-07T18:30:00Z")
	h.Set("anthropic-ratelimit-tokens-limit", "200000")
	h.Set("anthropic-ratelimit-tokens-remaining", "187543")
	h.Set("anthropic-ratelimit-tokens-reset", "2026-05-07T18:30:00Z")

	got := parseClaude(200, h)
	if len(got) != 2 {
		t.Fatalf("expected 2 samples, got %d (%+v)", len(got), got)
	}

	if got[0].Scheme != "anthropic_requests" || got[0].Remaining != 987 || got[0].Limit != 1000 {
		t.Errorf("requests sample wrong: %+v", got[0])
	}
	if got[1].Scheme != "anthropic_tokens" || got[1].Remaining != 187543 {
		t.Errorf("tokens sample wrong: %+v", got[1])
	}
	wantReset := time.Date(2026, 5, 7, 18, 30, 0, 0, time.UTC)
	if !got[0].ResetsAt.Equal(wantReset) {
		t.Errorf("reset mismatch: got %v want %v", got[0].ResetsAt, wantReset)
	}
	if got[0].Source != SourceHeader {
		t.Errorf("expected SourceHeader, got %q", got[0].Source)
	}
}

func TestParseClaude_PartialHeaders(t *testing.T) {
	// Only remaining is set; limit missing. Should still emit a sample.
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-remaining", "42")

	got := parseClaude(200, h)
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
	if got[0].Remaining != 42 || got[0].Limit != 0 {
		t.Errorf("partial sample wrong: %+v", got[0])
	}
}

func TestParseClaude_NoHeaders(t *testing.T) {
	if got := parseClaude(200, http.Header{}); len(got) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
	if got := parseClaude(200, nil); got != nil {
		t.Errorf("expected nil for nil headers, got %+v", got)
	}
}

func TestParseClaude_OAuthUnifiedHeaders(t *testing.T) {
	// Real header shape observed from Anthropic OAuth-beta /v1/messages —
	// Reset is Unix epoch seconds (integer string), Status is
	// "allowed" / "warning" / "exceeded".
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778143200") // 2026-05-08T18:00:00Z
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "warning")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1778565600") // 2026-05-13T13:00:00Z
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.78")
	h.Set("Anthropic-Ratelimit-Unified-7d_sonnet-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d_sonnet-Reset", "1778565600")
	h.Set("Anthropic-Ratelimit-Unified-7d_sonnet-Utilization", "0.61")

	got := parseClaude(200, h)
	if len(got) != 3 {
		t.Fatalf("expected 3 unified samples, got %d (%+v)", len(got), got)
	}

	wantBy := map[string]int64{
		"anthropic_unified_5h":        58, // (1-0.42)*100
		"anthropic_unified_7d":        22, // (1-0.78)*100
		"anthropic_unified_7d_sonnet": 39, // (1-0.61)*100
	}
	for _, s := range got {
		want, ok := wantBy[s.Scheme]
		if !ok {
			t.Errorf("unexpected scheme %q", s.Scheme)
			continue
		}
		if s.Remaining != want || s.Limit != 100 || s.Unit != "percent" {
			t.Errorf("%s: got remaining=%d limit=%d unit=%q, want remaining=%d limit=100 unit=%q",
				s.Scheme, s.Remaining, s.Limit, s.Unit, want, "percent")
		}
		if s.ResetsAt.IsZero() {
			t.Errorf("%s: reset should parse", s.Scheme)
		}
	}
}

func TestParseClaude_OAuthExceededForcesZero(t *testing.T) {
	// "exceeded" status with no utilization should still force remaining=0.
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "exceeded")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778143200")

	got := parseClaude(429, h)
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
	if got[0].Remaining != 0 {
		t.Errorf("exceeded status should force remaining=0, got %d", got[0].Remaining)
	}
}

func TestParseOpenAI_StandardHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "60")
	h.Set("x-ratelimit-remaining-requests", "58")
	h.Set("x-ratelimit-reset-requests", "1s")
	h.Set("x-ratelimit-limit-tokens", "150000")
	h.Set("x-ratelimit-remaining-tokens", "149800")

	got := parseOpenAI(200, h)
	if len(got) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(got))
	}
	if got[0].Scheme != "openai_requests" || got[0].Remaining != 58 {
		t.Errorf("requests sample wrong: %+v", got[0])
	}
	if got[0].ResetsAt.IsZero() {
		t.Errorf("expected duration reset to compute non-zero time")
	}
}

func TestParseOpenAI_FloatLimit(t *testing.T) {
	// Some openai-compat servers (e.g. Together.ai) emit floats.
	h := http.Header{}
	h.Set("x-ratelimit-limit-tokens", "9999.0")
	h.Set("x-ratelimit-remaining-tokens", "9000.5")

	got := parseOpenAI(200, h)
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
	if got[0].Limit != 9999 || got[0].Remaining != 9000 {
		t.Errorf("float fallback wrong: %+v", got[0])
	}
}

func TestParseGemini_OnlyOn429(t *testing.T) {
	if got := parseGemini(200, http.Header{}); got != nil {
		t.Errorf("expected nil on success, got %+v", got)
	}
	if got := parseGemini(500, http.Header{}); got != nil {
		t.Errorf("expected nil on 500, got %+v", got)
	}
}

func TestParseGemini_RetryAfterSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")

	got := parseGemini(429, h)
	if len(got) != 1 {
		t.Fatalf("expected 1 sample on 429, got %d", len(got))
	}
	if got[0].Remaining != 0 || got[0].Source != SourceInferred {
		t.Errorf("inferred sample wrong: %+v", got[0])
	}
	// ResetsAt should be ~30s in future
	if d := time.Until(got[0].ResetsAt); d < 25*time.Second || d > 35*time.Second {
		t.Errorf("expected ~30s reset, got %v", d)
	}
}

func TestParseGemini_MissingRetryAfter_Default60s(t *testing.T) {
	got := parseGemini(429, http.Header{})
	if len(got) != 1 {
		t.Fatalf("expected fallback sample, got %d", len(got))
	}
	if d := time.Until(got[0].ResetsAt); d < 55*time.Second || d > 65*time.Second {
		t.Errorf("expected ~60s default reset, got %v", d)
	}
}

func TestStore_PutGetAll(t *testing.T) {
	s := &Store{}

	if _, ok := s.Get("missing"); ok {
		t.Errorf("expected missing key to return ok=false")
	}

	samples := []Sample{
		{Scheme: "anthropic_tokens", Remaining: 100, Limit: 200, Source: SourceHeader},
	}
	s.Put("auth-1", samples)
	s.Put("auth-2", samples)

	snap, ok := s.Get("auth-1")
	if !ok {
		t.Fatalf("expected snapshot for auth-1")
	}
	if len(snap.Samples) != 1 || snap.Samples[0].Remaining != 100 {
		t.Errorf("snapshot wrong: %+v", snap)
	}
	if snap.AuthID != "auth-1" {
		t.Errorf("AuthID not propagated: %s", snap.AuthID)
	}
	if snap.ObservedAt.IsZero() {
		t.Errorf("ObservedAt should be set")
	}

	all := s.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(all))
	}
	if all[0].AuthID != "auth-1" || all[1].AuthID != "auth-2" {
		t.Errorf("All() should sort by AuthID, got %+v", all)
	}
}

func TestStore_PutEmptyClears(t *testing.T) {
	s := &Store{}
	s.Put("auth-1", []Sample{{Scheme: "x", Remaining: 1}})
	s.Put("auth-1", nil)
	if _, ok := s.Get("auth-1"); ok {
		t.Errorf("expected nil samples to clear snapshot")
	}
}

func TestStore_DefensiveCopy(t *testing.T) {
	s := &Store{}
	original := []Sample{{Scheme: "anthropic_tokens", Remaining: 100}}
	s.Put("auth-1", original)

	// Mutating the original slice must not affect what's stored.
	original[0].Remaining = 999

	snap, _ := s.Get("auth-1")
	if snap.Samples[0].Remaining != 100 {
		t.Errorf("store leaked original slice; got Remaining=%d", snap.Samples[0].Remaining)
	}

	// Mutating the returned slice must not affect what's stored either.
	snap.Samples[0].Remaining = 555
	again, _ := s.Get("auth-1")
	if again.Samples[0].Remaining != 100 {
		t.Errorf("store leaked returned slice; got Remaining=%d", again.Samples[0].Remaining)
	}
}

func TestRecordResponse_DispatchesByProvider(t *testing.T) {
	store := Default
	defer store.Put("test-claude", nil) // cleanup

	auth := &cliproxyauth.Auth{ID: "test-claude", Provider: "claude"}
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "999")

	RecordResponse(auth, 200, h)

	snap, ok := store.Get("test-claude")
	if !ok {
		t.Fatalf("expected snapshot recorded for claude auth")
	}
	if len(snap.Samples) != 1 || snap.Samples[0].Scheme != "anthropic_requests" {
		t.Errorf("dispatch wrong: %+v", snap)
	}
}

func TestRecordResponse_NilAuthIgnored(t *testing.T) {
	// Must not panic.
	RecordResponse(nil, 200, http.Header{})
	RecordResponse(&cliproxyauth.Auth{ID: ""}, 200, http.Header{})
}

func TestRecordResponse_UnknownProviderNoOp(t *testing.T) {
	store := Default
	defer store.Put("vertex-1", nil)

	// Seed an existing snapshot.
	store.Put("vertex-1", []Sample{{Scheme: "anthropic_tokens", Remaining: 5}})

	auth := &cliproxyauth.Auth{ID: "vertex-1", Provider: "vertex"}
	RecordResponse(auth, 200, http.Header{})

	// Unknown provider should leave the existing snapshot alone.
	snap, ok := store.Get("vertex-1")
	if !ok || len(snap.Samples) != 1 || snap.Samples[0].Remaining != 5 {
		t.Errorf("unknown provider should not clobber, got %+v", snap)
	}
}
