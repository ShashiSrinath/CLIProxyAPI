package management

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesConfigCredentialsWithQuotaProbe(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	const apiKey = "oc_sk_test_0123456789abcdef"
	manager := coreauth.NewManager(nil, nil, nil)
	for _, record := range []*coreauth.Auth{
		{
			ID:       "openai-compatibility:opencode-go:1",
			Provider: "openai-compatible-opencode-go",
			Label:    "opencode-go",
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				"source":      "config:opencode-go[1]",
				"base_url":    "https://opencode.ai/zen/go/v1",
				"compat_name": "opencode-go",
				"api_key":     apiKey,
			},
			Metadata: map[string]any{"quota_probe": util.OpenCodeGoQuotaProbe("https://opencode.ai/zen/go/v1")},
		},
		{
			// Config credentials without a quota probe stay out of the listing.
			ID:       "openai-compatibility:openrouter:1",
			Provider: "openai-compatible-openrouter",
			Label:    "openrouter",
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				"source":   "config:openrouter[1]",
				"base_url": "https://openrouter.ai/api/v1",
				"api_key":  "sk-or-test",
			},
		},
	} {
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register auth record: %v", errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	entry := firstAuthFileEntry(t, h)
	if entry["provider"] != "openai-compatible-opencode-go" || entry["source"] != "config" {
		t.Fatalf("unexpected entry: %#v", entry)
	}
	if entry["supports_quota"] != true || entry["quota_probe"] == nil {
		t.Fatalf("expected quota probe on entry: %#v", entry)
	}
	if account, _ := entry["account"].(string); account == "" || strings.Contains(account, apiKey) {
		t.Fatalf("account must be present and masked, got %q", account)
	}
}
