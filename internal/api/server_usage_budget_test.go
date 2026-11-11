package api

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// budgetTestPrices prices claude-sonnet-4-5 at $3/$15 per 1M tokens.
type budgetTestPrices struct{}

func (budgetTestPrices) Resolve(provider, model string) usagestats.Price {
	if provider == "claude" && model == "claude-sonnet-4-5" {
		return usagestats.Price{Source: "openrouter", Input: 3, Output: 15}
	}
	return usagestats.Price{Source: "none"}
}

func TestSetKeyBudgetCheckerFailsClosedWithoutTracking(t *testing.T) {
	usagestats.SetActive(nil)
	block := keyBudgetChecker()("sk-anything", config.APIKeyLimit{MonthlyUSD: 60})
	if block == nil || block.Reason != sdkhandlers.BudgetReasonUnavailable {
		t.Fatalf("block = %+v, want an unavailable block", block)
	}
}

func TestSetKeyBudgetCheckerReportsExceededWindow(t *testing.T) {
	tracker := usagestats.NewTracker("", 30)
	tracker.SetPricing(budgetTestPrices{})
	// $2 of input tokens exceeds a $1 daily cap.
	tracker.Record(context.Background(), coreusage.Record{
		APIKey:      "sk-budget-0123456789",
		Provider:    "claude",
		Model:       "claude-sonnet-4-5",
		RequestedAt: time.Now().Add(-time.Minute),
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.NewIndependentTokenBreakdown(1_000_000, 0, 0, 0, 0, 0),
		},
	})
	usagestats.SetActive(tracker)
	t.Cleanup(func() { usagestats.SetActive(nil) })
	check := keyBudgetChecker()

	if block := check("sk-budget-0123456789", config.APIKeyLimit{MonthlyUSD: 100}); block != nil {
		t.Fatalf("unexpected block under the limit: %+v", block)
	}
	block := check("sk-budget-0123456789", config.APIKeyLimit{DailyUSD: 1})
	if block == nil || block.Period != "day" || block.LimitUSD != 1 || block.SpentUSD != 3 {
		t.Fatalf("block = %+v, want the exceeded daily window", block)
	}
}
