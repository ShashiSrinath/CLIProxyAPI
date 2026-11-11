package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
)

// usageTracking owns the per-client-key usage tracker and the model price
// catalog. Both run only while tracking is enabled in config.
type usageTracking struct {
	mu      sync.Mutex
	started bool
	dataDir string
	tracker *usagestats.Tracker
	pricing *usagestats.Pricing
}

// keyBudgetChecker builds the spend-budget hook used by the API handlers. When usage
// tracking is disabled the hook reports an unavailable budget, so keys with a
// configured spend limit fail closed instead of bypassing enforcement.
func keyBudgetChecker() sdkhandlers.KeyBudgetChecker {
	return func(rawKey string, limit config.APIKeyLimit) *sdkhandlers.BudgetBlock {
		tracker := usagestats.Active()
		if tracker == nil {
			return &sdkhandlers.BudgetBlock{Reason: sdkhandlers.BudgetReasonUnavailable}
		}
		status := tracker.KeyBudget(rawKey, limit, time.Now())
		for _, window := range status.Windows {
			if window.Exceeded {
				return &sdkhandlers.BudgetBlock{
					Period:   window.Period,
					LimitUSD: window.LimitUSD,
					SpentUSD: window.SpentUSD,
					ResetsAt: window.ResetsAt,
				}
			}
		}
		return nil
	}
}

// setKeyBudgetChecker installs the spend-budget hook used by the API handlers.
func setKeyBudgetChecker() {
	sdkhandlers.SetKeyBudgetChecker(keyBudgetChecker())
}

// resolveUsageDataDir returns the configured usage data directory, or
// "<auth-dir>/usage" when unset.
func resolveUsageDataDir(cfg *config.Config) string {
	if dir := strings.TrimSpace(cfg.UsageTracking.DataDir); dir != "" {
		if resolved, err := util.ResolveAuthDir(dir); err == nil && resolved != "" {
			return resolved
		}
		return dir
	}
	authDir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err != nil || authDir == "" {
		authDir = cfg.AuthDir
	}
	return filepath.Join(authDir, "usage")
}

func usagePricingHTTPClient(cfg *config.Config) *http.Client {
	return util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second})
}

// start enables tracking for the lifetime of the server and applies cfg.
func (u *usageTracking) start(cfg *config.Config) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.started = true
	u.mu.Unlock()
	u.apply(cfg)
}

// apply starts, stops, or reconfigures tracking to match cfg. It does nothing
// before start, so servers built only for tests never touch the data dir.
func (u *usageTracking) apply(cfg *config.Config) {
	if u == nil || cfg == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.started {
		return
	}

	if cfg.UsageTracking.Disabled {
		u.stopLocked(context.Background())
		return
	}
	dataDir := resolveUsageDataDir(cfg)
	if u.tracker != nil && u.dataDir != dataDir {
		u.stopLocked(context.Background())
	}
	if u.tracker == nil {
		tracker := usagestats.NewTracker(dataDir, cfg.UsageTracking.RetentionDays)
		if err := tracker.Start(context.Background()); err != nil {
			log.Errorf("usage tracking: failed to start: %v", err)
			return
		}
		pricing := usagestats.NewPricing(dataDir)
		pricing.SetHTTPClient(usagePricingHTTPClient(cfg))
		pricing.Start(context.Background(), cfg.UsageTracking.Pricing)
		tracker.SetPricing(pricing)
		u.tracker, u.pricing, u.dataDir = tracker, pricing, dataDir
		usagestats.SetActive(tracker)
		return
	}
	u.tracker.SetRetentionDays(cfg.UsageTracking.RetentionDays)
	u.pricing.SetHTTPClient(usagePricingHTTPClient(cfg))
	u.pricing.Apply(cfg.UsageTracking.Pricing)
}

func (u *usageTracking) stop(ctx context.Context) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.started = false
	u.stopLocked(ctx)
}

func (u *usageTracking) stopLocked(ctx context.Context) {
	if u.tracker == nil {
		return
	}
	usagestats.SetActive(nil)
	if err := u.tracker.Stop(ctx); err != nil {
		log.Errorf("usage tracking: failed to flush on stop: %v", err)
	}
	u.pricing.Stop()
	u.tracker, u.pricing, u.dataDir = nil, nil, ""
}

// UsageTracker implements management.UsageDashboardSource.
func (u *usageTracking) UsageTracker() *usagestats.Tracker {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.tracker
}

// UsagePricing implements management.UsageDashboardSource.
func (u *usageTracking) UsagePricing() *usagestats.Pricing {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pricing
}
