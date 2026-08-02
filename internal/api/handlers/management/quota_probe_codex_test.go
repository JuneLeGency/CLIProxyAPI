package management

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSelectProbeBuilderUsesCodexUsageEndpoint(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct-123"},
	}
	builder, label := selectProbeBuilder(auth)
	if builder == nil || label != "codex" {
		t.Fatalf("builder=%v label=%q", builder != nil, label)
	}
	req, err := builder(auth, "access-token")
	if err != nil {
		t.Fatalf("build probe: %v", err)
	}
	if req.Method != "GET" || req.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
		t.Errorf("ChatGPT-Account-ID = %q", got)
	}
}
