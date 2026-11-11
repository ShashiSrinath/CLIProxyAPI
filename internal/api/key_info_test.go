package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// keyInfoTestPrices prices claude-sonnet-4-5 at $3/$15 per 1M tokens.
type keyInfoTestPrices struct{}

func (keyInfoTestPrices) Resolve(provider, model string) usagestats.Price {
	if provider == "claude" && model == "claude-sonnet-4-5" {
		return usagestats.Price{Source: "openrouter", Input: 3, Output: 15}
	}
	return usagestats.Price{Source: "none"}
}

// keyInfoTestNow is a Tuesday afternoon away from every window boundary.
func keyInfoTestNow() time.Time {
	return time.Date(2026, time.November, 10, 13, 30, 0, 0, time.UTC)
}

// keyInfoTestRecord is one $3 priced attempt of rawKey.
func keyInfoTestRecord(rawKey string, at time.Time) coreusage.Record {
	return coreusage.Record{
		APIKey:      rawKey,
		Provider:    "claude",
		Model:       "claude-sonnet-4-5",
		RequestedAt: at,
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.NewIndependentTokenBreakdown(1_000_000, 0, 0, 0, 0, 0),
		},
	}
}

func keyInfoTestTracker(t *testing.T, records ...coreusage.Record) *usagestats.Tracker {
	t.Helper()
	tracker := usagestats.NewTracker("", 30)
	tracker.SetPricing(keyInfoTestPrices{})
	for _, record := range records {
		tracker.Record(context.Background(), record)
	}
	return tracker
}

func keyInfoTestConfig(keys ...string) *config.Config {
	return &config.Config{SDKConfig: config.SDKConfig{APIKeys: keys}}
}

func TestBuildKeyInfoWithoutCapsReportsUsageAndNulls(t *testing.T) {
	const key = "sk-averylongrawkey9f2c"
	now := keyInfoTestNow()
	cfg := keyInfoTestConfig(key)
	tracker := keyInfoTestTracker(t, keyInfoTestRecord(key, now.Add(-time.Minute)))

	data := buildKeyInfo(cfg, tracker, key, now)

	if data.Label != "sk-a…9f2c" {
		t.Fatalf("label = %q, want the masked key", data.Label)
	}
	if data.Limit != nil || data.LimitRemaining != nil || data.LimitReset != nil {
		t.Fatalf("uncapped key must report null limits: %+v", data)
	}
	if data.Budgets == nil || len(data.Budgets) != 0 || data.EffectiveBudget != nil {
		t.Fatalf("uncapped key must report no budgets: %+v", data)
	}
	if !data.TrackingEnabled || data.Blocked {
		t.Fatalf("tracking/blocked = %t/%t", data.TrackingEnabled, data.Blocked)
	}
	if data.Timezone != "UTC" {
		t.Fatalf("timezone = %q", data.Timezone)
	}
	for name, got := range map[string]float64{
		"usage":         data.Usage,
		"usage_daily":   data.UsageDaily,
		"usage_weekly":  data.UsageWeekly,
		"usage_monthly": data.UsageMonthly,
	} {
		if math.Abs(got-3) > 1e-9 {
			t.Fatalf("%s = %v, want 3", name, got)
		}
	}
	if data.AllowedModels == nil || len(data.AllowedModels) != 0 || data.AllowedProviders == nil || len(data.AllowedProviders) != 0 {
		t.Fatalf("allow-lists = %v/%v", data.AllowedModels, data.AllowedProviders)
	}
}

func TestBuildKeyInfoPicksEffectiveBudget(t *testing.T) {
	const key = "sk-averylongrawkey9f2c"
	now := keyInfoTestNow()
	cfg := keyInfoTestConfig(key)
	cfg.APIKeyNames = map[string]string{key: "CI"}
	cfg.APIKeyModels = map[string][]string{key: {"claude-*"}}
	cfg.APIKeyProviders = map[string][]string{key: {"claude"}}
	cfg.APIKeyLimits = map[string]config.APIKeyLimit{
		key: {DailyUSD: 10, WeeklyUSD: 50, MonthlyUSD: 60, Timezone: "Asia/Kolkata"},
	}
	tracker := keyInfoTestTracker(t, keyInfoTestRecord(key, now.Add(-time.Minute)))

	data := buildKeyInfo(cfg, tracker, key, now)

	if data.Label != "CI" {
		t.Fatalf("label = %q", data.Label)
	}
	if data.Timezone != "Asia/Kolkata" {
		t.Fatalf("timezone = %q", data.Timezone)
	}
	if len(data.Budgets) != 3 {
		t.Fatalf("budgets = %+v, want day, week and month", data.Budgets)
	}
	for i, want := range []struct {
		interval string
		limit    float64
	}{{"daily", 10}, {"weekly", 50}, {"monthly", 60}} {
		budget := data.Budgets[i]
		if budget.Scope != "api_key" || budget.ResetInterval != want.interval || budget.LimitUSD != want.limit {
			t.Fatalf("budget %d = %+v", i, budget)
		}
		if budget.SpendUSD != 3 || budget.RemainingUSD != want.limit-3 || budget.ResetsAt == "" {
			t.Fatalf("budget %d spend = %+v", i, budget)
		}
	}
	if data.EffectiveBudget == nil || data.EffectiveBudget.ResetInterval != "daily" {
		t.Fatalf("effective budget = %+v, want the daily window", data.EffectiveBudget)
	}
	if data.Limit == nil || *data.Limit != 10 || data.LimitRemaining == nil || *data.LimitRemaining != 7 {
		t.Fatalf("limits = %v/%v", data.Limit, data.LimitRemaining)
	}
	if data.LimitReset == nil || *data.LimitReset != "daily" {
		t.Fatalf("limit reset = %v", data.LimitReset)
	}
	if data.Blocked {
		t.Fatal("a key under every cap must not be blocked")
	}
	if len(data.AllowedModels) != 1 || data.AllowedModels[0] != "claude-*" || len(data.AllowedProviders) != 1 || data.AllowedProviders[0] != "claude" {
		t.Fatalf("allow-lists = %v/%v", data.AllowedModels, data.AllowedProviders)
	}
}

func TestBuildKeyInfoEffectiveBudgetTieTakesShortestInterval(t *testing.T) {
	const key = "sk-averylongrawkey9f2c"
	now := keyInfoTestNow()
	cfg := keyInfoTestConfig(key)
	cfg.APIKeyLimits = map[string]config.APIKeyLimit{
		key: {DailyUSD: 10, WeeklyUSD: 10, MonthlyUSD: 10},
	}
	tracker := keyInfoTestTracker(t, keyInfoTestRecord(key, now.Add(-time.Minute)))

	data := buildKeyInfo(cfg, tracker, key, now)

	if data.EffectiveBudget == nil || data.EffectiveBudget.ResetInterval != "daily" {
		t.Fatalf("effective budget = %+v, want the daily window on a tie", data.EffectiveBudget)
	}
}

func TestBuildKeyInfoReportsExceededWindowAsBlocked(t *testing.T) {
	const key = "sk-averylongrawkey9f2c"
	now := keyInfoTestNow()
	cfg := keyInfoTestConfig(key)
	cfg.APIKeyLimits = map[string]config.APIKeyLimit{key: {MonthlyUSD: 2}}
	tracker := keyInfoTestTracker(t, keyInfoTestRecord(key, now.Add(-time.Minute)))

	data := buildKeyInfo(cfg, tracker, key, now)

	if !data.Blocked {
		t.Fatal("an exhausted window must block the key")
	}
	if len(data.Budgets) != 1 || data.Budgets[0].RemainingUSD >= 0 {
		t.Fatalf("budgets = %+v, want a negative remaining amount", data.Budgets)
	}
	if data.EffectiveBudget == nil || data.EffectiveBudget.RemainingUSD >= 0 {
		t.Fatalf("effective budget = %+v", data.EffectiveBudget)
	}
	if data.LimitRemaining == nil || *data.LimitRemaining != 0 {
		t.Fatalf("limit remaining = %v, want it floored at 0", data.LimitRemaining)
	}
}

func TestBuildKeyInfoFailsClosedWhileTrackingIsDisabled(t *testing.T) {
	const key = "sk-averylongrawkey9f2c"
	cfg := keyInfoTestConfig(key)
	cfg.APIKeyLimits = map[string]config.APIKeyLimit{key: {DailyUSD: 5, WeeklyUSD: 20}}

	data := buildKeyInfo(cfg, nil, key, keyInfoTestNow())

	if data.TrackingEnabled || !data.Blocked {
		t.Fatalf("tracking/blocked = %t/%t", data.TrackingEnabled, data.Blocked)
	}
	if data.Usage != 0 || data.UsageDaily != 0 || data.UsageWeekly != 0 || data.UsageMonthly != 0 {
		t.Fatalf("usage = %+v, want zeros", data)
	}
	if len(data.Budgets) != 2 {
		t.Fatalf("budgets = %+v, want the configured windows", data.Budgets)
	}
	for _, budget := range data.Budgets {
		if budget.SpendUSD != 0 || budget.RemainingUSD != 0 || budget.ResetsAt == "" {
			t.Fatalf("disabled budget = %+v", budget)
		}
	}
	if data.EffectiveBudget == nil || data.Limit == nil || *data.Limit != 5 || data.LimitRemaining == nil || *data.LimitRemaining != 0 {
		t.Fatalf("effective budget = %+v", data.EffectiveBudget)
	}
	if data.LimitReset == nil || *data.LimitReset != "daily" {
		t.Fatalf("limit reset = %v", data.LimitReset)
	}
}

func TestBuildKeyInfoFallsBackToUTCForInvalidTimezone(t *testing.T) {
	const key = "sk-averylongrawkey9f2c"
	cfg := keyInfoTestConfig(key)
	cfg.APIKeyLimits = map[string]config.APIKeyLimit{key: {DailyUSD: 5, Timezone: "Not/AZone"}}

	data := buildKeyInfo(cfg, nil, key, keyInfoTestNow())

	if data.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want UTC", data.Timezone)
	}
}

func TestKeyInfoRequiresAPIKey(t *testing.T) {
	server := newTestServerWithConfig(t, keyInfoTestConfig())

	req := httptest.NewRequest(http.MethodGet, "/v1/key", nil)
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body.Error.Message != "API key required" || body.Error.Type != "authentication_error" || body.Error.Code != "missing_api_key" {
		t.Fatalf("error = %+v", body.Error)
	}
}

func TestKeyInfoServesOwnKeyOnBothRoutes(t *testing.T) {
	const key = "sk-averylongrawkey9f2c"
	cfg := keyInfoTestConfig(key)
	cfg.APIKeyLimits = map[string]config.APIKeyLimit{key: {MonthlyUSD: 60}}
	tracker := keyInfoTestTracker(t, keyInfoTestRecord(key, time.Now().Add(-time.Minute)))
	usagestats.SetActive(tracker)
	t.Cleanup(func() { usagestats.SetActive(nil) })
	server := newTestServerWithConfig(t, cfg)

	for _, path := range []string{"/v1/key", "/api/v1/key"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			server.engine.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
			if body := rec.Body.String(); strings.Contains(body, key) {
				t.Fatalf("response leaks the raw key: %s", body)
			}
			var body struct {
				Data keyInfoData `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
			}
			if body.Data.Label != "sk-a…9f2c" {
				t.Fatalf("label = %q", body.Data.Label)
			}
			if !body.Data.TrackingEnabled || body.Data.Blocked {
				t.Fatalf("tracking/blocked = %t/%t", body.Data.TrackingEnabled, body.Data.Blocked)
			}
			if body.Data.EffectiveBudget == nil || body.Data.EffectiveBudget.ResetInterval != "monthly" {
				t.Fatalf("effective budget = %+v", body.Data.EffectiveBudget)
			}
			if len(body.Data.Budgets) != 1 || math.Abs(body.Data.Usage-body.Data.Budgets[0].SpendUSD) > 1e-9 {
				t.Fatalf("usage = %v budgets = %+v", body.Data.Usage, body.Data.Budgets)
			}
		})
	}
}
