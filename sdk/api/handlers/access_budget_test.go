package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestCheckKeyBudgetUsesRegisteredChecker(t *testing.T) {
	limit := config.APIKeyLimit{MonthlyUSD: 60}
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{APIKeyLimits: map[string]config.APIKeyLimit{"budgeted": limit}}}
	_, ctx := accessTestContext("budgeted")

	var gotKey string
	var gotLimit config.APIKeyLimit
	resets := time.Date(2026, time.December, 1, 0, 0, 0, 0, time.FixedZone("IST", 5*3600+30*60))
	SetKeyBudgetChecker(func(apiKey string, l config.APIKeyLimit) *BudgetBlock {
		gotKey, gotLimit = apiKey, l
		return &BudgetBlock{Period: "month", LimitUSD: 60, SpentUSD: 60.12, ResetsAt: resets}
	})
	t.Cleanup(func() { SetKeyBudgetChecker(nil) })

	errMsg := h.checkKeyBudget(ctx)
	if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %+v", errMsg)
	}
	if gotKey != "budgeted" || gotLimit.MonthlyUSD != 60 {
		t.Fatalf("checker arguments = %q %+v", gotKey, gotLimit)
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(errMsg.Error.Error()), &body); err != nil {
		t.Fatalf("unmarshal %s: %v", errMsg.Error.Error(), err)
	}
	if body.Error.Type != "insufficient_quota" || body.Error.Code != "budget_exceeded" {
		t.Fatalf("error = %+v", body.Error)
	}
	want := "API key monthly budget of $60.00 exhausted ($60.12 spent); resets at 2026-12-01T00:00:00+05:30"
	if body.Error.Message != want {
		t.Fatalf("message = %q, want %q", body.Error.Message, want)
	}
}

func TestCheckKeyBudgetAllowsUnderLimitAndSkipsUnbudgetedKeys(t *testing.T) {
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{APIKeyLimits: map[string]config.APIKeyLimit{
		"budgeted": {DailyUSD: 1},
		"zero":     {Timezone: "UTC"},
	}}}
	SetKeyBudgetChecker(func(string, config.APIKeyLimit) *BudgetBlock { return nil })
	t.Cleanup(func() { SetKeyBudgetChecker(nil) })

	_, ctx := accessTestContext("budgeted")
	if errMsg := h.checkKeyBudget(ctx); errMsg != nil {
		t.Fatalf("allowed key blocked: %+v", errMsg)
	}
	_, ctx = accessTestContext("zero")
	if errMsg := h.checkKeyBudget(ctx); errMsg != nil {
		t.Fatalf("key without a positive limit must not be checked: %+v", errMsg)
	}
	_, ctx = accessTestContext("unlisted")
	if errMsg := h.checkKeyBudget(ctx); errMsg != nil {
		t.Fatalf("unlisted key must not be checked: %+v", errMsg)
	}
}

func TestCheckKeyBudgetFailsClosedWhenCheckerReportsUnavailable(t *testing.T) {
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{APIKeyLimits: map[string]config.APIKeyLimit{
		"budgeted": {WeeklyUSD: 20},
	}}}
	SetKeyBudgetChecker(func(string, config.APIKeyLimit) *BudgetBlock {
		return &BudgetBlock{Reason: BudgetReasonUnavailable}
	})
	t.Cleanup(func() { SetKeyBudgetChecker(nil) })
	_, ctx := accessTestContext("budgeted")
	errMsg := h.checkKeyBudget(ctx)
	if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 while tracking is unavailable, got %+v", errMsg)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(errMsg.Error.Error()), &body); err != nil {
		t.Fatalf("unmarshal %s: %v", errMsg.Error.Error(), err)
	}
	if body.Error.Code != "budget_unavailable" || body.Error.Message != "usage tracking is disabled; budgeted API keys are blocked" {
		t.Fatalf("error = %+v", body.Error)
	}
}

func TestCheckKeyBudgetWithoutCheckerIsNotEnforced(t *testing.T) {
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{APIKeyLimits: map[string]config.APIKeyLimit{
		"budgeted": {WeeklyUSD: 20},
	}}}
	SetKeyBudgetChecker(nil)
	_, ctx := accessTestContext("budgeted")
	if errMsg := h.checkKeyBudget(ctx); errMsg != nil {
		t.Fatalf("no checker must mean no enforcement, got %+v", errMsg)
	}
}

func TestExecuteWithAuthManagerEnforcesPerKeyBudget(t *testing.T) {
	s := newFallbackTestSetup(t, 0, 0, false)
	s.handler.Cfg.APIKeyLimits = map[string]config.APIKeyLimit{"budgeted": {DailyUSD: 5}}
	SetKeyBudgetChecker(func(string, config.APIKeyLimit) *BudgetBlock {
		return &BudgetBlock{Period: "day", LimitUSD: 5, SpentUSD: 5.5, ResetsAt: time.Date(2026, time.October, 19, 0, 0, 0, 0, time.UTC)}
	})
	t.Cleanup(func() { SetKeyBudgetChecker(nil) })

	_, ctx := accessTestContext("budgeted")
	_, _, errMsg := s.handler.ExecuteWithAuthManager(ctx, "openai", "fbtest-main-model", []byte(`{"model":"fbtest-main-model"}`), "")
	if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %+v", errMsg)
	}
	if got := s.main.Models(); len(got) != 0 {
		t.Fatalf("blocked request reached the provider: %v", got)
	}
}

func TestExecuteWithAuthManagerEnforcesModelAllowList(t *testing.T) {
	s := newFallbackTestSetup(t, 0, 0, false)
	s.handler.Cfg.APIKeyModels = map[string][]string{"restricted": {"fbtest-fallback-*"}}

	_, ctx := accessTestContext("restricted")
	_, _, errMsg := s.handler.ExecuteWithAuthManager(ctx, "openai", "fbtest-main-model", []byte(`{"model":"fbtest-main-model"}`), "")
	if errMsg == nil || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", errMsg)
	}
	if got := s.main.Models(); len(got) != 0 {
		t.Fatalf("denied request reached the provider: %v", got)
	}

	_, ctx = accessTestContext("restricted")
	body, _, errMsg := s.handler.ExecuteWithAuthManager(ctx, "openai", "fbtest-fallback-model", []byte(`{"model":"fbtest-fallback-model"}`), "")
	if errMsg != nil {
		t.Fatalf("allowed model rejected: %v", errMsg.Error)
	}
	if len(body) == 0 {
		t.Fatal("allowed request produced no response")
	}
}

func TestExecuteStreamWithAuthManagerEnforcesPerKeyBudget(t *testing.T) {
	s := newFallbackTestSetup(t, 0, 0, false)
	s.handler.Cfg.APIKeyLimits = map[string]config.APIKeyLimit{"budgeted": {WeeklyUSD: 20}}
	SetKeyBudgetChecker(func(string, config.APIKeyLimit) *BudgetBlock {
		return &BudgetBlock{Period: "week", LimitUSD: 20, SpentUSD: 21, ResetsAt: time.Date(2026, time.October, 19, 0, 0, 0, 0, time.UTC)}
	})
	t.Cleanup(func() { SetKeyBudgetChecker(nil) })

	_, ctx := accessTestContext("budgeted")
	dataChan, _, errChan := s.handler.ExecuteStreamWithAuthManager(ctx, "openai", "fbtest-main-model", []byte(`{"model":"fbtest-main-model"}`), "")
	if dataChan != nil {
		t.Fatal("blocked stream must not produce a data channel")
	}
	var status int
	for msg := range errChan {
		if msg != nil {
			status = msg.StatusCode
		}
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if got := s.main.Models(); len(got) != 0 {
		t.Fatalf("blocked stream reached the provider: %v", got)
	}
}
