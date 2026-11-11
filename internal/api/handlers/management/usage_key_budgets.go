package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
)

// KeyBudgetRow is one configured client API key with its allow-lists and spend
// status. The raw key is never included: only its stable id and masked form.
type KeyBudgetRow struct {
	ID        string                    `json:"id"`
	Masked    string                    `json:"masked"`
	Name      string                    `json:"name"`
	Models    []string                  `json:"models"`
	Providers []string                  `json:"providers"`
	Limits    *config.APIKeyLimit       `json:"limits"`
	Windows   []usagestats.BudgetWindow `json:"windows"`
	Exceeded  bool                      `json:"exceeded"`
}

// keyBudgetsPayload is the body of GET /observability/usage/key-budgets.
type keyBudgetsPayload struct {
	Enabled     bool           `json:"enabled"`
	GeneratedAt string         `json:"generated_at"`
	Keys        []KeyBudgetRow `json:"keys"`
}

// GetUsageKeyBudgets returns the configured model allow-lists and spend windows of
// every client API key that is listed in access.api-keys, in configuration order.
func (h *Handler) GetUsageKeyBudgets(c *gin.Context) {
	tracker, _, cfg := h.usageDashboardState()
	now := time.Now()
	payload := keyBudgetsPayload{
		Enabled:     tracker != nil,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Keys:        []KeyBudgetRow{},
	}
	if cfg == nil {
		c.JSON(http.StatusOK, payload)
		return
	}
	for _, raw := range cfg.APIKeys {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		payload.Keys = append(payload.Keys, KeyBudgetRowFor(cfg, tracker, raw, now))
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, payload)
}

// KeyBudgetRowFor builds the row of one raw client API key. The spend windows are
// only filled when tracking is enabled; a nil tracker leaves them empty.
func KeyBudgetRowFor(cfg *config.Config, tracker *usagestats.Tracker, raw string, now time.Time) KeyBudgetRow {
	row := KeyBudgetRow{
		ID:        usagestats.KeyIDForRaw(raw),
		Masked:    usagestats.MaskKeyForRaw(raw),
		Models:    []string{},
		Providers: []string{},
		Windows:   []usagestats.BudgetWindow{},
	}
	if cfg == nil {
		return row
	}
	row.Name = strings.TrimSpace(cfg.APIKeyNames[raw])
	row.Models = append(row.Models, cfg.APIKeyModels[raw]...)
	row.Providers = append(row.Providers, cfg.APIKeyProviders[raw]...)
	limit, hasLimit := cfg.APIKeyLimits[raw]
	if !hasLimit || !limit.HasCap() {
		return row
	}
	row.Limits = &limit
	if tracker == nil {
		return row
	}
	status := tracker.KeyBudget(raw, limit, now)
	row.Windows = status.Windows
	row.Exceeded = status.Exceeded
	return row
}
