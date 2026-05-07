// Package quota provides passive scraping of upstream provider rate-limit
// headers. Samples are stored per-auth-id in an in-memory store and exposed
// through the management API for downstream load-balancing / status displays.
package quota

import "time"

// Source describes how a Sample was obtained.
type Source string

const (
	// SourceHeader marks a sample parsed directly from a provider response header.
	SourceHeader Source = "header_scrape"
	// SourceInferred marks a sample derived from a 429 response or static
	// provider documentation when no machine-readable header was sent.
	SourceInferred Source = "inferred"
)

// Sample captures one slice of remaining capacity reported by a provider.
type Sample struct {
	// Scheme identifies the limit category, e.g. "anthropic_requests",
	// "anthropic_tokens", "openai_requests", "gemini_rpd".
	Scheme string `json:"scheme"`
	// Remaining is the count remaining before the limit resets.
	Remaining int64 `json:"remaining"`
	// Limit is the upper bound for this scheme during the current window.
	Limit int64 `json:"limit"`
	// Unit describes what is being counted ("requests" / "tokens").
	Unit string `json:"unit,omitempty"`
	// ResetsAt is when Remaining returns to Limit. Zero if unknown.
	ResetsAt time.Time `json:"resets_at,omitempty"`
	// Source records how the sample was obtained.
	Source Source `json:"source"`
}

// Snapshot is the full quota state for one auth, suitable for serialisation.
type Snapshot struct {
	// AuthID is the authenticated credential's stable id.
	AuthID string `json:"auth_id"`
	// Samples is the most recent batch parsed for this auth, in declaration order.
	Samples []Sample `json:"samples"`
	// ObservedAt is when the most recent batch was recorded.
	ObservedAt time.Time `json:"observed_at"`
}
