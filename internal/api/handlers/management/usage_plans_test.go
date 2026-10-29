package management

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUsagePlansDisabledContractAndMask(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("GET", "/v8/management/observability/usage/plans", nil)
	(&Handler{}).GetUsagePlans(c)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"enabled", "generated_at", "tz", "pricing", "totals", "plans", "unassigned", "available_credentials", "providers"} {
		if _, ok := got[k]; !ok {
			t.Fatal(k)
		}
	}
	if got["enabled"] != false {
		t.Fatal(got)
	}
	secret := "sk-abcdef123456789"
	label := credentialLabel(&authpkg.Auth{Attributes: map[string]string{"compat_name": "test", "api_key": secret}})
	if strings.Contains(label, secret) || label != "test · sk-a…6789" {
		t.Fatal(label)
	}
}

type plansTestSource struct {
	tracker *usagestats.Tracker
	pricing *usagestats.Pricing
}

func (s plansTestSource) UsageTracker() *usagestats.Tracker { return s.tracker }
func (s plansTestSource) UsagePricing() *usagestats.Pricing { return s.pricing }
func TestUsagePlansEnabledContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "sk-abcdef123456789"
	session := "private-session-token"
	manager := authpkg.NewManager(nil, nil, nil)
	_, err := manager.Register(context.Background(), &authpkg.Auth{ID: "safe-id", Provider: "compat", Attributes: map[string]string{"compat_name": "example", "api_key": secret, "opendesign_session": session}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.UsageTracking.Subscriptions = []config.SubscriptionPlan{{ID: "plan", Provider: "compat", PriceUSD: 10}}
	h := &Handler{cfg: cfg, authManager: manager, usageDashboard: plansTestSource{usagestats.NewTracker("", 30), usagestats.NewPricing("")}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("GET", "/v8/management/observability/usage/plans", nil)
	h.GetUsagePlans(c)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), session) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var body struct {
		Plans []map[string]any `json:"plans"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Plans) != 1 {
		t.Fatal(err, rec.Body.String())
	}
	for _, key := range []string{"id", "anchor_source", "cycle_start", "cycle_end", "multiplier", "break_even_at", "projected_value_usd", "last_7d_value_usd", "effective_usd_per_mtok", "unpriced_token_share", "totals", "series", "models", "credentials"} {
		if _, ok := body.Plans[0][key]; !ok {
			t.Fatal(key)
		}
	}
}

func TestProviderAnchorAndWindowMonthClamp(t *testing.T) {
	reset := time.Date(2024, time.March, 31, 12, 0, 0, 0, time.UTC)
	start := windowStart("monthly", reset)
	if start.Month() != time.February || start.Day() != 29 {
		t.Fatal(start)
	}
	a := &authpkg.Auth{Attributes: map[string]string{"base_url": "https://opencode.ai/zen/go/v1"}}
	got := providerAnchor(context.Background(), nil, a, liveResult{windows: []liveWindow{{key: "monthly", reset: reset}}})
	if !got.Equal(reset) {
		t.Fatal(got)
	}
	a.Attributes["base_url"] = "https://example.com"
	if !providerAnchor(context.Background(), nil, a, liveResult{}).IsZero() {
		t.Fatal("unexpected anchor")
	}
}
