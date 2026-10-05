package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func accessTestContext(apiKey string) (*gin.Context, context.Context) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if apiKey != "" {
		ginCtx.Set("userApiKey", apiKey)
	}
	return ginCtx, context.WithValue(context.Background(), "gin", ginCtx)
}

func TestProviderAllowListAllows(t *testing.T) {
	cfg := &config.SDKConfig{APIKeyProviders: map[string][]string{
		"restricted": {" Codex ", "opencode-go"},
		"compat":     {"openai-compatibility"},
		"empty":      {},
	}}

	if ProviderAllowListForKey(cfg, "other") != nil || ProviderAllowListForKey(cfg, "empty") != nil {
		t.Fatal("keys without entries must be unrestricted")
	}

	restricted := ProviderAllowListForKey(cfg, "restricted")
	for provider, want := range map[string]bool{
		"codex":                         true,
		"openai-compatible-opencode-go": true,
		"claude":                        false,
		"openai-compatible-other":       false,
		"":                              false,
	} {
		if got := restricted.Allows(provider); got != want {
			t.Errorf("restricted.Allows(%q) = %v, want %v", provider, got, want)
		}
	}

	compat := ProviderAllowListForKey(cfg, "compat")
	if !compat.Allows("openai-compatible-any") || !compat.Allows("openai-compatibility") || compat.Allows("gemini") {
		t.Fatal("openai-compatibility must match only openai-compatibility providers")
	}
}

func TestRestrictProvidersForRequest(t *testing.T) {
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{APIKeyProviders: map[string][]string{"restricted": {"claude"}}}}

	_, ctx := accessTestContext("restricted")
	got, errMsg := h.restrictProvidersForRequest(ctx, []string{"codex", "claude"}, "m")
	if errMsg != nil || !reflect.DeepEqual(got, []string{"claude"}) {
		t.Fatalf("providers = %v, err = %v", got, errMsg)
	}

	_, errMsg = h.restrictProvidersForRequest(ctx, []string{"codex"}, "m")
	if errMsg == nil || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", errMsg)
	}

	_, ctx = accessTestContext("unrestricted")
	got, errMsg = h.restrictProvidersForRequest(ctx, []string{"codex"}, "m")
	if errMsg != nil || !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("unrestricted providers = %v, err = %v", got, errMsg)
	}

	if errMsg = h.checkPluginExecutorAccess(context.Background(), "plugin-x", "m"); errMsg != nil {
		t.Fatal("requests without a client key must not be restricted")
	}
	_, ctx = accessTestContext("restricted")
	if errMsg = h.checkPluginExecutorAccess(ctx, "plugin-x", "m"); errMsg == nil {
		t.Fatal("plugin executor not in allow-list must be rejected")
	}
}

func TestFilterModelsForRequest(t *testing.T) {
	r := registry.GetGlobalRegistry()
	r.RegisterClient("access-test-codex", "codex", []*registry.ModelInfo{{ID: "access-test-codex-model"}})
	r.RegisterClient("access-test-claude", "claude", []*registry.ModelInfo{{ID: "access-test-claude-model"}})
	t.Cleanup(func() {
		r.UnregisterClient("access-test-codex")
		r.UnregisterClient("access-test-claude")
	})

	h := &BaseAPIHandler{Cfg: &config.SDKConfig{APIKeyProviders: map[string][]string{"restricted": {"claude"}}}}
	models := []map[string]any{
		{"id": "access-test-codex-model"},
		{"id": "access-test-claude-model"},
		{"name": "models/access-test-claude-model"},
	}

	ginCtx, _ := accessTestContext("restricted")
	got := h.FilterModelsForRequest(ginCtx, models)
	if len(got) != 2 || got[0]["id"] != "access-test-claude-model" || got[1]["name"] != "models/access-test-claude-model" {
		t.Fatalf("filtered models = %v", got)
	}

	ginCtx, _ = accessTestContext("unrestricted")
	if got = h.FilterModelsForRequest(ginCtx, models); len(got) != 3 {
		t.Fatalf("unrestricted models = %v", got)
	}
}
