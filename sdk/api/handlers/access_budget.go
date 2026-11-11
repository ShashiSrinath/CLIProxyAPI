package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// BudgetReasonUnavailable marks a block reported while no usable budget source
// exists. Budgeted keys fail closed in that case.
const BudgetReasonUnavailable = "unavailable"

// BudgetBlock describes an exhausted spend window that blocks a request. Reason
// is empty for an exhausted window and BudgetReasonUnavailable when the budget
// could not be evaluated at all.
type BudgetBlock struct {
	Period   string
	LimitUSD float64
	SpentUSD float64
	ResetsAt time.Time
	Reason   string
}

// KeyBudgetChecker reports whether a client API key may spend more. Returning a
// non-nil block rejects the request; nil lets it through.
type KeyBudgetChecker func(apiKey string, limit config.APIKeyLimit) *BudgetBlock

// keyBudgetChecker holds the optional checker registered by the server. The SDK
// package stays free of the internal usage tracker by calling through this hook.
var keyBudgetChecker atomic.Pointer[KeyBudgetChecker]

// SetKeyBudgetChecker installs the spend-budget hook. A nil function disables
// enforcement.
func SetKeyBudgetChecker(fn KeyBudgetChecker) {
	if fn == nil {
		keyBudgetChecker.Store(nil)
		return
	}
	checker := fn
	keyBudgetChecker.Store(&checker)
}

func activeKeyBudgetChecker() KeyBudgetChecker {
	if checker := keyBudgetChecker.Load(); checker != nil {
		return *checker
	}
	return nil
}

// requestAPIKey returns the client API key that authenticated the request.
func requestAPIKey(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// checkRequestAccess enforces the per-key model allow-list and spend budget of the
// client API key that authenticated the request.
func (h *BaseAPIHandler) checkRequestAccess(ctx context.Context, modelName string) *interfaces.ErrorMessage {
	if h == nil {
		return nil
	}
	if errMsg := h.checkModelAccess(ctx, modelName); errMsg != nil {
		return errMsg
	}
	return h.checkKeyBudget(ctx)
}

// checkKeyBudget rejects requests of client API keys whose spend window is
// exhausted. Without an installed checker nothing is enforced; the server installs
// one that fails closed while usage tracking is unavailable.
func (h *BaseAPIHandler) checkKeyBudget(ctx context.Context) *interfaces.ErrorMessage {
	if h == nil || ctx == nil || h.Cfg == nil || len(h.Cfg.APIKeyLimits) == 0 {
		return nil
	}
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	apiKey := requestAPIKey(ginCtx)
	if apiKey == "" {
		return nil
	}
	limit, ok := h.Cfg.APIKeyLimits[apiKey]
	if !ok || !limit.HasCap() {
		return nil
	}
	checker := activeKeyBudgetChecker()
	if checker == nil {
		return nil
	}
	return budgetBlockedError(checker(apiKey, limit))
}

// budgetBlockedError converts a budget block into an HTTP 429 response body.
func budgetBlockedError(block *BudgetBlock) *interfaces.ErrorMessage {
	if block == nil {
		return nil
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "insufficient_quota",
			"code":    budgetErrorCode(block),
			"message": budgetBlockMessage(block),
		},
	})
	if errMarshal != nil {
		payload = []byte(`{"error":{"type":"insufficient_quota","code":"budget_exceeded","message":"API key budget exhausted"}}`)
	}
	return &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New(string(payload))}
}

func budgetErrorCode(block *BudgetBlock) string {
	if block.Reason == BudgetReasonUnavailable {
		return "budget_unavailable"
	}
	return "budget_exceeded"
}

// budgetBlockMessage renders the operator-facing explanation of a block.
func budgetBlockMessage(block *BudgetBlock) string {
	if block.Reason == BudgetReasonUnavailable {
		return "usage tracking is disabled; budgeted API keys are blocked"
	}
	period := budgetPeriodAdverb(block.Period)
	resets := ""
	if !block.ResetsAt.IsZero() {
		resets = "; resets at " + block.ResetsAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("API key %s budget of $%.2f exhausted ($%.2f spent)%s", period, roundCents(block.LimitUSD), ceilCents(block.SpentUSD), resets)
}

// budgetPeriodAdverb renders a window period the way the error message reads it.
func budgetPeriodAdverb(period string) string {
	switch period {
	case "day":
		return "daily"
	case "week":
		return "weekly"
	case "month":
		return "monthly"
	default:
		return "spend"
	}
}

func roundCents(value float64) float64 {
	return math.Round(value*100) / 100
}

// ceilCents rounds a spent amount up so an exceeded window never reads as if it
// were still under its limit.
func ceilCents(value float64) float64 {
	return math.Ceil(value*100) / 100
}
