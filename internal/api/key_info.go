package api

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	managementHandlers "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
)

// keyInfoMoneyScale rounds money to six decimals so float noise never leaks into
// the response.
const keyInfoMoneyScale = 1e6

// keyInfoBudget is one spend window in the OpenRouter budget shape.
type keyInfoBudget struct {
	Scope         string  `json:"scope"`
	GuardrailID   any     `json:"guardrail_id"`
	BudgetID      any     `json:"budget_id"`
	LimitUSD      float64 `json:"limit_usd"`
	SpendUSD      float64 `json:"spend_usd"`
	RemainingUSD  float64 `json:"remaining_usd"`
	ResetInterval string  `json:"reset_interval"`
	ResetsAt      string  `json:"resets_at"`
	IncludeBYOK   bool    `json:"include_byok"`
}

// keyInfoData is the body of GET /v1/key in the OpenRouter key-info shape, with
// CLIProxyAPI extensions appended.
type keyInfoData struct {
	Label                  string          `json:"label"`
	Limit                  *float64        `json:"limit"`
	LimitRemaining         *float64        `json:"limit_remaining"`
	LimitReset             *string         `json:"limit_reset"`
	IncludeBYOKInLimit     bool            `json:"include_byok_in_limit"`
	Usage                  float64         `json:"usage"`
	UsageDaily             float64         `json:"usage_daily"`
	UsageWeekly            float64         `json:"usage_weekly"`
	UsageMonthly           float64         `json:"usage_monthly"`
	BYOKUsage              float64         `json:"byok_usage"`
	BYOKUsageDaily         float64         `json:"byok_usage_daily"`
	BYOKUsageWeekly        float64         `json:"byok_usage_weekly"`
	BYOKUsageMonthly       float64         `json:"byok_usage_monthly"`
	IsFreeTier             bool            `json:"is_free_tier"`
	FreeModelDailyRequests any             `json:"free_model_daily_requests"`
	RateLimit              any             `json:"rate_limit"`
	Budgets                []keyInfoBudget `json:"budgets"`
	EffectiveBudget        *keyInfoBudget  `json:"effective_budget"`
	AllowedModels          []string        `json:"allowed_models"`
	AllowedProviders       []string        `json:"allowed_providers"`
	Timezone               string          `json:"timezone"`
	TrackingEnabled        bool            `json:"tracking_enabled"`
	Blocked                bool            `json:"blocked"`
}

// handleKeyInfo serves the authenticated client key's own usage, allow-lists and
// spend budgets. It never goes through the execution path, so the key's own
// budget and model allow-list cannot block it.
func (s *Server) handleKeyInfo(c *gin.Context) {
	rawKey := ginRequestAPIKey(c)
	if rawKey == "" {
		c.Header("Cache-Control", "no-store")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{
			"message": "API key required",
			"type":    "authentication_error",
			"code":    "missing_api_key",
		}})
		return
	}
	now := time.Now()
	data := buildKeyInfo(s.getConfig(), usagestats.Active(), rawKey, now)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// ginRequestAPIKey returns the client API key that authenticated the request.
func ginRequestAPIKey(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// buildKeyInfo assembles the key-info payload of one raw client key.
func buildKeyInfo(cfg *config.Config, tracker *usagestats.Tracker, rawKey string, now time.Time) keyInfoData {
	row := managementHandlers.KeyBudgetRowFor(cfg, tracker, rawKey, now)
	timezone := "UTC"
	if cfg != nil {
		timezone = keyInfoTimezone(cfg.APIKeyLimits[rawKey].Timezone)
	}
	data := keyInfoData{
		Label:            row.Masked,
		Budgets:          []keyInfoBudget{},
		AllowedModels:    append([]string{}, row.Models...),
		AllowedProviders: append([]string{}, row.Providers...),
		Timezone:         timezone,
		TrackingEnabled:  tracker != nil,
	}
	if row.Name != "" {
		data.Label = row.Name
	}
	if row.Limits != nil {
		if tracker == nil {
			data.Budgets = keyInfoDisabledBudgets(*row.Limits, now)
		} else {
			data.Budgets = keyInfoBudgets(row.Windows)
		}
	}
	data.EffectiveBudget = keyInfoEffectiveBudget(data.Budgets)
	if data.EffectiveBudget != nil {
		limit := data.EffectiveBudget.LimitUSD
		remaining := math.Max(0, data.EffectiveBudget.RemainingUSD)
		reset := data.EffectiveBudget.ResetInterval
		data.Limit = &limit
		data.LimitRemaining = &remaining
		data.LimitReset = &reset
	}
	data.Blocked = row.Exceeded || (tracker == nil && row.Limits != nil)
	if tracker == nil {
		return data
	}
	keyID := usagestats.KeyIDForRaw(rawKey)
	data.Usage = roundKeyInfoMoney(tracker.KeySpendSince(keyID, time.Time{}))
	data.UsageDaily = roundKeyInfoMoney(tracker.KeySpendSince(keyID, keyInfoWindowStart(now, timezone, usagestats.BudgetPeriodDay)))
	data.UsageWeekly = roundKeyInfoMoney(tracker.KeySpendSince(keyID, keyInfoWindowStart(now, timezone, usagestats.BudgetPeriodWeek)))
	data.UsageMonthly = roundKeyInfoMoney(tracker.KeySpendSince(keyID, keyInfoWindowStart(now, timezone, usagestats.BudgetPeriodMonth)))
	return data
}

// keyInfoBudgets converts the tracked spend windows into the response shape.
func keyInfoBudgets(windows []usagestats.BudgetWindow) []keyInfoBudget {
	budgets := make([]keyInfoBudget, 0, len(windows))
	for _, window := range windows {
		budgets = append(budgets, keyInfoBudget{
			Scope:         "api_key",
			LimitUSD:      roundKeyInfoMoney(window.LimitUSD),
			SpendUSD:      roundKeyInfoMoney(window.SpentUSD),
			RemainingUSD:  roundKeyInfoMoney(window.LimitUSD - window.SpentUSD),
			ResetInterval: usagestats.BudgetInterval(window.Period),
			ResetsAt:      window.ResetsAt.UTC().Format(time.RFC3339),
		})
	}
	return budgets
}

// keyInfoDisabledBudgets lists the configured windows of a key while usage
// tracking is off. Enforcement fails closed in that state, so every window
// reports no remaining budget.
func keyInfoDisabledBudgets(limit config.APIKeyLimit, now time.Time) []keyInfoBudget {
	windows := usagestats.BudgetWindows(limit, now)
	budgets := make([]keyInfoBudget, 0, len(windows))
	for _, window := range windows {
		budgets = append(budgets, keyInfoBudget{
			Scope:         "api_key",
			LimitUSD:      roundKeyInfoMoney(window.LimitUSD),
			ResetInterval: usagestats.BudgetInterval(window.Period),
			ResetsAt:      window.ResetsAt.UTC().Format(time.RFC3339),
		})
	}
	return budgets
}

// keyInfoEffectiveBudget picks the budget that blocks first: the lowest remaining
// amount, preferring the shortest interval on a tie.
func keyInfoEffectiveBudget(budgets []keyInfoBudget) *keyInfoBudget {
	if len(budgets) == 0 {
		return nil
	}
	best := budgets[0]
	for _, budget := range budgets[1:] {
		if budget.RemainingUSD < best.RemainingUSD {
			best = budget
			continue
		}
		if budget.RemainingUSD == best.RemainingUSD && keyInfoIntervalRank(budget.ResetInterval) < keyInfoIntervalRank(best.ResetInterval) {
			best = budget
		}
	}
	selected := best
	return &selected
}

// keyInfoIntervalRank orders reset intervals from shortest to longest.
func keyInfoIntervalRank(interval string) int {
	switch interval {
	case "daily":
		return 0
	case "weekly":
		return 1
	case "monthly":
		return 2
	default:
		return 3
	}
}

// keyInfoWindowStart returns the start of the spend window containing now.
func keyInfoWindowStart(now time.Time, timezone, period string) time.Time {
	start, _ := usagestats.BudgetWindowBounds(now, timezone, period)
	return start
}

// keyInfoTimezone resolves the timezone reported to the client. An empty or
// invalid IANA name falls back to UTC, matching budget enforcement.
func keyInfoTimezone(timezone string) string {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return "UTC"
	}
	return timezone
}

// roundKeyInfoMoney rounds a USD amount to six decimals and normalizes negative
// zero so the JSON never contains "-0".
func roundKeyInfoMoney(value float64) float64 {
	rounded := math.Round(value*keyInfoMoneyScale) / keyInfoMoneyScale
	if rounded == 0 {
		return 0
	}
	return rounded
}
