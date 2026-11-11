package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestModelAllowListForKey(t *testing.T) {
	cfg := &config.SDKConfig{APIKeyModels: map[string][]string{
		"restricted": {" DeepSeek-* ", "kimi-k2*", "  "},
		"empty":      {"", " "},
	}}
	if ModelAllowListForKey(cfg, "other") != nil || ModelAllowListForKey(cfg, "empty") != nil {
		t.Fatal("keys without usable patterns must be unrestricted")
	}
	if ModelAllowListForKey(nil, "restricted") != nil {
		t.Fatal("nil config must be unrestricted")
	}
	allowed := ModelAllowListForKey(cfg, "restricted")
	if !reflect.DeepEqual([]string(allowed), []string{"deepseek-*", "kimi-k2*"}) {
		t.Fatalf("allow list = %v", allowed)
	}
}

func TestModelAllowListAllows(t *testing.T) {
	allowed := ModelAllowList{"claude-*", "deepseek-*", "kimi-k2*", "glm-5*", "["}
	for model, want := range map[string]bool{
		"claude-sonnet-4-5":          true,
		"CLAUDE-SONNET-4-5":          true,
		"claude-sonnet-4-5(high)":    true,
		"team/claude-sonnet-4-5":     true,
		"deepseek-v3.2":              true,
		"kimi-k2-thinking(low)":      true,
		"glm-5":                      true,
		"gpt-5":                      false,
		"":                           false,
		"team/claude-sonnet-4-5/low": false,
	} {
		if got := allowed.Allows(model); got != want {
			t.Errorf("Allows(%q) = %v, want %v", model, got, want)
		}
	}
	if !ModelAllowList(nil).Allows("anything") {
		t.Fatal("nil allow list must allow everything")
	}
}

func TestCheckRequestAccessRejectsDisallowedModel(t *testing.T) {
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{APIKeyModels: map[string][]string{
		"restricted": {"claude-*"},
	}}}
	_, ctx := accessTestContext("restricted")
	if errMsg := h.checkRequestAccess(ctx, "claude-sonnet-4-5(high)"); errMsg != nil {
		t.Fatalf("allowed model rejected: %+v", errMsg)
	}
	errMsg := h.checkRequestAccess(ctx, "gpt-5")
	if errMsg == nil || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", errMsg)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(errMsg.Error.Error()), &body); err != nil || body.Error.Code != "model_not_allowed" {
		t.Fatalf("error body = %s (%v)", errMsg.Error.Error(), err)
	}

	_, ctx = accessTestContext("unrestricted")
	if errMsg = h.checkRequestAccess(ctx, "gpt-5"); errMsg != nil {
		t.Fatalf("unrestricted key blocked: %+v", errMsg)
	}
}

func TestFilterModelsForRequestAppliesModelAllowList(t *testing.T) {
	r := registry.GetGlobalRegistry()
	r.RegisterClient("access-models-codex", "codex", []*registry.ModelInfo{{ID: "access-models-codex-model"}})
	r.RegisterClient("access-models-claude", "claude", []*registry.ModelInfo{{ID: "access-models-claude-model"}})
	t.Cleanup(func() {
		r.UnregisterClient("access-models-codex")
		r.UnregisterClient("access-models-claude")
	})

	h := &BaseAPIHandler{Cfg: &config.SDKConfig{APIKeyModels: map[string][]string{
		"restricted": {"access-models-claude-*"},
	}}}
	models := []map[string]any{
		{"id": "access-models-codex-model"},
		{"id": "access-models-claude-model"},
		{"name": "models/access-models-claude-model"},
	}
	ginCtx, _ := accessTestContext("restricted")
	got := h.FilterModelsForRequest(ginCtx, models)
	if len(got) != 2 || got[0]["id"] != "access-models-claude-model" {
		t.Fatalf("filtered models = %v", got)
	}

	ginCtx, _ = accessTestContext("unrestricted")
	if got = h.FilterModelsForRequest(ginCtx, models); len(got) != 3 {
		t.Fatalf("unrestricted models = %v", got)
	}
}

func TestModelFallbackSkipsTargetOutsideAllowList(t *testing.T) {
	s := newFallbackTestSetup(t, http.StatusTooManyRequests, 0, true)
	s.handler.Cfg.APIKeyModels = map[string][]string{"restricted": {"fbtest-main-*"}}

	_, ctx := accessTestContext("restricted")
	_, _, errMsg := s.handler.ExecuteWithAuthManager(ctx, "openai", "fbtest-main-model", []byte(`{"model":"fbtest-main-model"}`), "")
	if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected the main model's 429, got %+v", errMsg)
	}
	if got := s.pooled.Models(); len(got) != 0 {
		t.Fatalf("fallback outside the allow-list was used: %v", got)
	}
}

func TestModelAllowListFromEmptyContextIsUnrestricted(t *testing.T) {
	if !modelAllowListFromContext(&config.SDKConfig{}, context.Background()).Allows("any") {
		t.Fatal("empty context must be unrestricted")
	}
}
