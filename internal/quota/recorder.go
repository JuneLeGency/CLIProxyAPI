package quota

import (
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// RecordResponse parses provider rate-limit headers from an upstream response
// and stores them in the package-level Default store, keyed by auth.ID.
// Safe to call from any executor immediately after the HTTP response is
// available; nil-tolerant for missing auth, missing headers, or unsupported
// providers.
func RecordResponse(auth *cliproxyauth.Auth, status int, headers http.Header) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	samples := parseFor(auth.Provider, status, headers)
	if samples == nil {
		// Unknown provider — leave any existing snapshot alone rather than
		// clearing it. Successful Gemini calls go through this path.
		return
	}
	Default.Put(auth.ID, samples)
}

// parseFor dispatches to the provider-specific parser. Unknown providers
// return nil so the caller leaves the existing snapshot untouched.
func parseFor(provider string, status int, headers http.Header) []Sample {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "anthropic":
		return parseClaude(status, headers)
	case "codex", "openai", "openai-compat", "kimi":
		// OpenAI-compat upstreams (Codex, Kimi, generic /v1/chat/completions
		// providers) all share the x-ratelimit-* convention.
		return parseOpenAI(status, headers)
	case "gemini", "antigravity":
		return parseGemini(status, headers)
	}
	return nil
}
