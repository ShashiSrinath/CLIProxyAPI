package management

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// fixedPrices resolves claude-sonnet-4-5 at $3/$15 per 1M tokens.
type fixedPrices struct{}

func (fixedPrices) Resolve(provider, model string) usagestats.Price {
	if provider == "claude" && model == "claude-sonnet-4-5" {
		return usagestats.Price{Source: "openrouter", Input: 3, Output: 15}
	}
	return usagestats.Price{Source: "none"}
}

// usageRecord builds one priced upstream attempt for rawKey.
func usageRecord(rawKey string, at time.Time) coreusage.Record {
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

func newKeyBudgetsTestSource(t *testing.T, records ...coreusage.Record) plansTestSource {
	t.Helper()
	tracker := usagestats.NewTracker("", 30)
	tracker.SetPricing(fixedPrices{})
	for _, record := range records {
		tracker.Record(context.Background(), record)
	}
	return plansTestSource{tracker: tracker, pricing: usagestats.NewPricing("")}
}

func TestGetUsageKeyBudgetsListsConfiguredKeysWithoutRawKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const withLimit = "sk-averylongrawkey9f2c"
	const withoutLimit = "sk-plain"

	cfg := &config.Config{}
	cfg.APIKeys = []string{withLimit, withoutLimit}
	cfg.APIKeyNames = map[string]string{withLimit: "CI"}
	cfg.APIKeyModels = map[string][]string{withLimit: {"claude-*"}}
	cfg.APIKeyProviders = map[string][]string{withLimit: {"claude"}}
	cfg.APIKeyLimits = map[string]config.APIKeyLimit{
		withLimit: {DailyUSD: 10, WeeklyUSD: 50, MonthlyUSD: 300, Timezone: "UTC"},
	}

	source := newKeyBudgetsTestSource(t, usageRecord(withLimit, time.Now().Add(-time.Minute)))
	h := &Handler{cfg: cfg, usageDashboard: source}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/key-budgets", nil)
	h.GetUsageKeyBudgets(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), withLimit) || strings.Contains(rec.Body.String(), withoutLimit) {
		t.Fatalf("response leaks a raw client key: %s", rec.Body.String())
	}
	var body struct {
		Enabled bool `json:"enabled"`
		Keys    []struct {
			ID        string                    `json:"id"`
			Masked    string                    `json:"masked"`
			Name      string                    `json:"name"`
			Models    []string                  `json:"models"`
			Providers []string                  `json:"providers"`
			Limits    *config.APIKeyLimit       `json:"limits"`
			Windows   []usagestats.BudgetWindow `json:"windows"`
			Exceeded  bool                      `json:"exceeded"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !body.Enabled {
		t.Fatal("tracking should be reported as enabled")
	}
	if len(body.Keys) != 2 {
		t.Fatalf("keys = %d, want one row per configured key", len(body.Keys))
	}
	limited := body.Keys[0]
	if limited.ID != usagestats.KeyIDForRaw(withLimit) || limited.Masked != "sk-a…9f2c" || limited.Name != "CI" {
		t.Fatalf("first row identity = %+v", limited)
	}
	if len(limited.Models) != 1 || limited.Models[0] != "claude-*" || len(limited.Providers) != 1 || limited.Providers[0] != "claude" {
		t.Fatalf("first row allow-lists = %+v", limited)
	}
	if limited.Limits == nil || limited.Limits.MonthlyUSD != 300 {
		t.Fatalf("first row limits = %+v", limited.Limits)
	}
	if len(limited.Windows) != 3 {
		t.Fatalf("windows = %+v, want day, week and month", limited.Windows)
	}
	if math.Abs(limited.Windows[0].SpentUSD-3) > 1e-9 || limited.Windows[0].LimitUSD != 10 {
		t.Fatalf("day window = %+v", limited.Windows[0])
	}
	if limited.Exceeded {
		t.Fatalf("day spend of $3 must stay under the $10 cap: %+v", limited.Windows[0])
	}

	plain := body.Keys[1]
	if plain.ID != usagestats.KeyIDForRaw(withoutLimit) || plain.Masked != "sk…" {
		t.Fatalf("second row identity = %+v", plain)
	}
	if plain.Limits != nil || len(plain.Windows) != 0 || plain.Exceeded {
		t.Fatalf("unlimited row = %+v", plain)
	}
}

func TestGetUsageKeyBudgetsReportsExceededAndDisabledTracking(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "sk-exceeded-9f2c"
	cfg := &config.Config{}
	cfg.APIKeys = []string{key}
	cfg.APIKeyLimits = map[string]config.APIKeyLimit{key: {DailyUSD: 2}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/key-budgets", nil)
	h := &Handler{cfg: cfg, usageDashboard: newKeyBudgetsTestSource(t, usageRecord(key, time.Now().Add(-time.Minute)))}
	h.GetUsageKeyBudgets(c)

	var body struct {
		Keys []struct {
			Windows  []usagestats.BudgetWindow `json:"windows"`
			Exceeded bool                      `json:"exceeded"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Keys) != 1 || !body.Keys[0].Exceeded || len(body.Keys[0].Windows) != 1 || !body.Keys[0].Windows[0].Exceeded {
		t.Fatalf("exceeded row = %+v", body.Keys)
	}

	disabled := &Handler{cfg: cfg}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/key-budgets", nil)
	disabled.GetUsageKeyBudgets(c)
	var offBody struct {
		Enabled bool `json:"enabled"`
		Keys    []struct {
			Windows []usagestats.BudgetWindow `json:"windows"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &offBody); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if offBody.Enabled || len(offBody.Keys) != 1 || len(offBody.Keys[0].Windows) != 0 {
		t.Fatalf("disabled payload = %+v", offBody)
	}
}
