package management

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// probeTimeout caps a single quota-refresh probe. It must be long enough for
// a token refresh + a 1-token completion round-trip even on slow links.
const probeTimeout = 20 * time.Second

// probeResult summarises what happened on a single probe attempt. It is
// returned by ProbeQuota and serialised by the refresh handlers so the
// frontend can show per-account success/error states.
type probeResult struct {
	AuthID       string `json:"auth_id"`
	StatusCode   int    `json:"status_code,omitempty"`
	HeaderHit    bool   `json:"header_hit"`
	Error        string `json:"error,omitempty"`
	Skipped      bool   `json:"skipped,omitempty"`
	SkipReason   string `json:"skip_reason,omitempty"`
	BodySnippet  string `json:"body_snippet,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
	ProbedAt     time.Time
	ProbedAtUnix int64 `json:"probed_at_unix"`
}

// probeBuilder constructs the (URL, method, headers, body) tuple for a
// minimal "1-token" completion request against a specific provider. It is
// allowed to inspect auth.Attributes / auth.Metadata for per-credential
// base URLs (api-key auths can override the provider default).
type probeBuilder func(auth *coreauth.Auth, token string) (req *http.Request, err error)

// ProbeQuota sends a minimal probe request to the upstream provider so the
// quota.Default store gets a fresh rate-limit-header sample, then returns a
// result describing what happened. Disabled accounts are explicitly
// supported — the whole point of manual refresh is to let operators check
// disabled credentials without re-enabling them first.
func (h *Handler) ProbeQuota(ctx context.Context, auth *coreauth.Auth) probeResult {
	start := time.Now()
	res := probeResult{ProbedAt: start.UTC(), ProbedAtUnix: start.Unix()}
	if auth == nil {
		res.Skipped = true
		res.SkipReason = "auth not found"
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}
	res.AuthID = auth.ID

	builder, providerLabel := selectProbeBuilder(auth)
	if builder == nil {
		res.Skipped = true
		res.SkipReason = fmt.Sprintf("no probe template for provider %q", providerLabel)
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	token, errToken := h.resolveTokenForAuth(probeCtx, auth)
	if errToken != nil {
		res.Error = fmt.Sprintf("token refresh failed: %v", errToken)
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}

	req, errBuild := builder(auth, token)
	if errBuild != nil {
		res.Error = fmt.Sprintf("build probe: %v", errBuild)
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}
	req = req.WithContext(probeCtx)

	client := &http.Client{
		Timeout:   probeTimeout,
		Transport: h.apiCallTransport(auth),
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		res.Error = fmt.Sprintf("probe request failed: %v", errDo)
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	res.StatusCode = resp.StatusCode
	// Record headers even on non-2xx — 429 specifically carries the most
	// useful Retry-After / remaining=0 signal. RecordResponse no-ops if the
	// parser yields zero samples, so untranslatable responses don't clobber.
	before, _ := quota.Default.Get(auth.ID)
	quota.RecordResponse(auth, resp.StatusCode, resp.Header)
	after, _ := quota.Default.Get(auth.ID)
	res.HeaderHit = !after.ObservedAt.Equal(before.ObservedAt)

	// Capture a tiny body snippet on errors so the frontend can show why
	// the probe came back 4xx/5xx (e.g. "invalid_api_key"). 256 bytes is
	// enough for any structured provider error.
	if resp.StatusCode >= 400 {
		buf := &bytes.Buffer{}
		_, _ = io.CopyN(buf, resp.Body, 256)
		res.BodySnippet = strings.TrimSpace(buf.String())
	}

	res.DurationMS = time.Since(start).Milliseconds()
	return res
}

// selectProbeBuilder returns the probe builder appropriate for auth.Provider
// and the canonical provider label used in skip messages. Returns (nil, label)
// when the provider has no template registered.
func selectProbeBuilder(auth *coreauth.Auth) (probeBuilder, string) {
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "claude", "anthropic":
		return buildClaudeProbe, provider
	case "codex", "openai":
		return buildOpenAIProbe, provider
	case "openai-compat", "kimi", "iflow", "qwen":
		// Same wire format as OpenAI — base_url usually lives in attributes.
		return buildOpenAICompatProbe, provider
	case "gemini":
		return buildGeminiAPIKeyProbe, provider
	}
	// Unknown provider: try generic openai-compat as a best-effort. The user
	// explicitly opted in to this fallback so unrecognised providers still
	// get a chance to surface fresh quota data.
	return buildOpenAICompatProbe, provider
}

// buildClaudeProbe sends the minimal Anthropic Messages probe that returns
// rate-limit headers on both the API-key and OAuth-beta paths. Headers are
// kept minimal — Claude Code device fingerprinting / cache-control injection
// are NOT applied here because the probe is internal traffic the user is
// running on their own account, and a stripped probe is much less likely to
// dirty the OAuth session's "real client" state.
func buildClaudeProbe(auth *coreauth.Auth, token string) (*http.Request, error) {
	baseURL := "https://api.anthropic.com"
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			baseURL = strings.TrimRight(v, "/")
		}
	}
	useAPIKey := auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""

	body := []byte(`{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"."}]}`)
	url := baseURL + "/v1/messages?beta=true"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if useAPIKey {
		req.Header.Set("x-api-key", token)
		req.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
		// OAuth-beta requires the beta tag; without it the OAuth token is
		// rejected with 401 even though it would otherwise be valid.
		req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	}
	return req, nil
}

// buildOpenAIProbe targets the canonical OpenAI Chat Completions endpoint.
// Codex (ChatGPT-backend) auths are NOT routed here — they have a custom
// endpoint that needs ChatGPT-specific cookies which probeBuilder can't fake;
// for now we share the OpenAI template and accept that codex-oauth auths
// may probe-fail with 401, which still surfaces the right "credential dead"
// signal.
func buildOpenAIProbe(auth *coreauth.Auth, token string) (*http.Request, error) {
	baseURL := "https://api.openai.com"
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			baseURL = strings.TrimRight(v, "/")
		}
	}
	model := "gpt-5-nano"
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["probe_model"]); v != "" {
			model = v
		}
	}
	body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"."}]}`, model))
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

// buildOpenAICompatProbe is the same as buildOpenAIProbe but does NOT default
// to the public OpenAI host — base_url must come from auth.Attributes. This
// avoids accidentally pointing iflow/qwen probes at api.openai.com when the
// credential is missing config.
func buildOpenAICompatProbe(auth *coreauth.Auth, token string) (*http.Request, error) {
	if auth.Attributes == nil {
		return nil, fmt.Errorf("openai-compat probe needs attributes.base_url")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/")
	if baseURL == "" {
		// Fall back to the public OpenAI host so a probe at least reaches
		// SOMETHING. Generic-fallback callers expect this lenient behaviour.
		baseURL = "https://api.openai.com"
	}
	model := strings.TrimSpace(auth.Attributes["probe_model"])
	if model == "" {
		model = strings.TrimSpace(auth.Attributes["default_model"])
	}
	if model == "" {
		model = "gpt-3.5-turbo"
	}
	body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"."}]}`, model))
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// buildGeminiAPIKeyProbe hits generativelanguage.googleapis.com with the
// API-key from auth.Attributes. OAuth-flavoured gemini auths (gemini-cli /
// vertex / antigravity) do NOT come through here — they have provider
// strings other than "gemini" and probeQuota will skip them with a clear
// "no probe template" reason.
func buildGeminiAPIKeyProbe(auth *coreauth.Auth, token string) (*http.Request, error) {
	if auth.Attributes == nil {
		return nil, fmt.Errorf("gemini probe needs attributes.api_key")
	}
	apiKey := strings.TrimSpace(auth.Attributes["api_key"])
	if apiKey == "" {
		// Fall back to resolved token (which is what tokenValueForAuth would
		// have returned for api-key auths anyway). This keeps the function
		// usable when the api_key is stored only in metadata.
		apiKey = token
	}
	if apiKey == "" {
		return nil, fmt.Errorf("gemini probe needs an api_key")
	}
	model := "gemini-2.5-flash-lite"
	if v := strings.TrimSpace(auth.Attributes["probe_model"]); v != "" {
		model = v
	}
	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		model, apiKey,
	)
	body := []byte(`{"contents":[{"parts":[{"text":"."}]}],"generationConfig":{"maxOutputTokens":1}}`)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}
