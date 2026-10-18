package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
)

// pricingRefreshTimeout bounds a manual OpenRouter catalog refresh.
const pricingRefreshTimeout = 60 * time.Second

// UsageDashboardSource exposes the running usage tracker and price catalog.
// Both return nil while usage tracking is disabled.
type UsageDashboardSource interface {
	UsageTracker() *usagestats.Tracker
	UsagePricing() *usagestats.Pricing
}

// SetUsageDashboard wires the usage tracker and price catalog into the handler.
func (h *Handler) SetUsageDashboard(source UsageDashboardSource) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.usageDashboard = source
	h.mu.Unlock()
}

func (h *Handler) usageDashboardState() (*usagestats.Tracker, *usagestats.Pricing, *config.Config) {
	h.mu.Lock()
	source, cfg := h.usageDashboard, h.cfg
	h.mu.Unlock()
	if source == nil {
		return nil, nil, cfg
	}
	return source.UsageTracker(), source.UsagePricing(), cfg
}

// configuredUsageKeys lists client API keys with their names and provider allow-lists.
func configuredUsageKeys(cfg *config.Config) []usagestats.ConfiguredKey {
	if cfg == nil {
		return nil
	}
	keys := make([]usagestats.ConfiguredKey, 0, len(cfg.APIKeys))
	for _, raw := range cfg.APIKeys {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		keys = append(keys, usagestats.ConfiguredKey{
			Raw:       raw,
			Name:      strings.TrimSpace(cfg.APIKeyNames[raw]),
			Providers: cfg.APIKeyProviders[raw],
		})
	}
	return keys
}

// GetUsageSummary returns usage per client API key and model for a time range.
func (h *Handler) GetUsageSummary(c *gin.Context) {
	tracker, pricing, cfg := h.usageDashboardState()
	query, err := usagestats.ParseSummaryQuery(c.Request.URL.Query(), time.Now())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_query", "message": err.Error()})
		return
	}
	if tracker == nil || pricing == nil {
		c.JSON(http.StatusOK, usagestats.DisabledSummary(query))
		return
	}
	summary := tracker.Summary(query, configuredUsageKeys(cfg), pricing)
	summary.Pricing = pricing.Status()
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, summary)
}

// DeleteUsageSummary clears all stored usage.
func (h *Handler) DeleteUsageSummary(c *gin.Context) {
	tracker, _, _ := h.usageDashboardState()
	if tracker == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "usage_tracking_disabled"})
		return
	}
	if err := tracker.Clear(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "clear_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func usagePricingPayload(pricing *usagestats.Pricing, cfg *config.Config) gin.H {
	overrides := map[string]config.ModelPriceOverride{}
	disabled := false
	if cfg != nil {
		for pattern, override := range cfg.UsageTracking.Pricing.Overrides {
			overrides[pattern] = override
		}
		disabled = cfg.UsageTracking.Pricing.OpenRouterDisabled
	}
	payload := gin.H{
		"source":              "none",
		"fetched_at":          "",
		"error":               "",
		"openrouter_disabled": disabled,
		"catalog":             []usagestats.CatalogEntry{},
		"overrides":           overrides,
	}
	if pricing == nil {
		payload["error"] = "usage tracking is disabled"
		return payload
	}
	status := pricing.Status()
	payload["source"] = status.Source
	if !status.FetchedAt.IsZero() {
		payload["fetched_at"] = status.FetchedAt.UTC().Format(time.RFC3339)
	}
	payload["error"] = status.Error
	payload["catalog"] = pricing.Catalog()
	return payload
}

// GetUsagePricing returns the price catalog and configured overrides.
func (h *Handler) GetUsagePricing(c *gin.Context) {
	_, pricing, cfg := h.usageDashboardState()
	c.JSON(http.StatusOK, usagePricingPayload(pricing, cfg))
}

// RefreshUsagePricing refetches the OpenRouter catalog. Fetch failures are
// reported in the payload's error field and the previous catalog is kept.
func (h *Handler) RefreshUsagePricing(c *gin.Context) {
	_, pricing, cfg := h.usageDashboardState()
	if pricing == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "usage_tracking_disabled"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), pricingRefreshTimeout)
	defer cancel()
	_ = pricing.Refresh(ctx)
	c.JSON(http.StatusOK, usagePricingPayload(pricing, cfg))
}
