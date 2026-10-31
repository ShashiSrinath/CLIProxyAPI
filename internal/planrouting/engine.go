package planrouting

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/planlimits"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// RefreshInterval is how often live limits are re-read and the order reapplied.
const RefreshInterval = 2 * time.Minute

// staleLiveTTL keeps the last successful windows when a fetch fails.
const staleLiveTTL = 30 * time.Minute

// Snapshot is the most recently applied routing order.
type Snapshot struct {
	Enabled    bool
	ComputedAt time.Time
	Entries    []Entry
}

// Engine periodically ranks plan credentials and applies dynamic priorities.
type Engine struct {
	manager *authpkg.Manager

	mu        sync.Mutex
	cfg       *config.Config
	snapshot  Snapshot
	signature string
	lastGood  map[string]planlimits.Live
	cancel    context.CancelFunc
	wake      chan struct{}

	refreshMu sync.Mutex
}

// NewEngine creates an engine bound to an auth manager.
func NewEngine(manager *authpkg.Manager) *Engine {
	return &Engine{manager: manager, lastGood: map[string]planlimits.Live{}, wake: make(chan struct{}, 1)}
}

func configSignature(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	data, _ := json.Marshal(struct {
		Smart bool
		Plans []config.SubscriptionPlan
	}{cfg.UsageTracking.SmartRoutingEnabled(), cfg.UsageTracking.Subscriptions})
	return string(data)
}

// Start begins periodic refreshes using cfg.
func (e *Engine) Start(cfg *config.Config) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.cancel != nil {
		e.mu.Unlock()
		e.Apply(cfg)
		return
	}
	e.cfg = cfg
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.mu.Unlock()
	go e.loop(ctx)
}

// Apply updates the configuration and refreshes soon when plans changed.
func (e *Engine) Apply(cfg *config.Config) {
	if e == nil {
		return
	}
	e.mu.Lock()
	changed := configSignature(cfg) != configSignature(e.cfg)
	e.cfg = cfg
	e.mu.Unlock()
	if changed {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
}

// Stop ends the refresh loop and restores configured priorities.
func (e *Engine) Stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if e.manager != nil {
		e.manager.SetPriorityOverrides(nil)
	}
}

func (e *Engine) loop(ctx context.Context) {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()
	for {
		e.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-e.wake:
		}
	}
}

// Snapshot returns the current order, refreshing first when it is stale or the
// plan configuration changed since it was computed.
func (e *Engine) Snapshot(ctx context.Context) Snapshot {
	if e == nil {
		return Snapshot{}
	}
	e.mu.Lock()
	fresh := e.signature == configSignature(e.cfg) && time.Since(e.snapshot.ComputedAt) < RefreshInterval
	snap := e.snapshot
	e.mu.Unlock()
	if fresh {
		return snap
	}
	return e.Refresh(ctx)
}

// Refresh re-reads live limits, ranks plan credentials and applies priorities.
func (e *Engine) Refresh(ctx context.Context) Snapshot {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	e.mu.Lock()
	cfg := e.cfg
	e.mu.Unlock()
	if cfg == nil || e.manager == nil {
		return Snapshot{}
	}
	now := time.Now()
	smart := cfg.UsageTracking.SmartRoutingEnabled()
	plans := cfg.UsageTracking.Subscriptions
	auths := e.manager.List()
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	providers := make(map[string]string, len(auths))
	for _, a := range auths {
		providers[a.ID] = a.Provider
	}
	owners, _ := usagestats.PlanOwners(plans, providers)

	lives := make(map[string]planlimits.Live)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range auths {
		if _, ok := owners[a.ID]; !ok || a.Disabled {
			continue
		}
		wg.Add(1)
		go func(a *authpkg.Auth) {
			defer wg.Done()
			live := planlimits.Fetch(ctx, cfg, a)
			mu.Lock()
			lives[a.ID] = live
			mu.Unlock()
		}(a)
	}
	wg.Wait()

	candidates := make([]Candidate, 0, len(lives))
	for _, a := range auths {
		index, ok := owners[a.ID]
		if !ok {
			continue
		}
		plan := plans[index]
		live := lives[a.ID]
		e.mu.Lock()
		if live.Err == "" && len(live.Windows) > 0 {
			e.lastGood[a.ID] = live
		} else if previous, okPrevious := e.lastGood[a.ID]; okPrevious && live.Err != "" && now.Sub(previous.At) < staleLiveTTL {
			live = previous
		}
		e.mu.Unlock()
		candidates = append(candidates, Candidate{
			AuthID: a.ID, Label: planlimits.Label(a), Provider: a.Provider,
			PlanID: plan.ID, PlanName: plan.Name, PlanIndex: index,
			ConfigLimitType: plan.LimitType, KnownMonthly: planlimits.HasMonthlyLimit(a),
			Routing: plan.Routing, Windows: live.Windows, CycleEnd: cycleEnd(plan, now),
			Disabled:       a.Disabled || a.Status == authpkg.StatusDisabled,
			ManualPriority: manualPriority(a),
		})
	}
	entries := Rank(candidates, now, smart)
	if smart {
		e.manager.SetPriorityOverrides(Overrides(entries))
	} else {
		e.manager.SetPriorityOverrides(nil)
	}
	snap := Snapshot{Enabled: smart, ComputedAt: now, Entries: entries}
	e.mu.Lock()
	previous := e.snapshot
	e.snapshot = snap
	e.signature = configSignature(cfg)
	e.mu.Unlock()
	logOrderChange(previous, snap)
	return snap
}

func cycleEnd(plan config.SubscriptionPlan, now time.Time) time.Time {
	anchor := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if plan.AnchorDate != "" {
		if parsed, err := time.Parse("2006-01-02", plan.AnchorDate); err == nil {
			anchor = parsed
		}
	}
	cycle := plan.Cycle
	if cycle == "" {
		cycle = "monthly"
	}
	_, end := usagestats.CycleBounds(now, anchor, cycle, 0, time.UTC)
	return end
}

// manualPriority reads the configured priority, ignoring dynamic overrides.
func manualPriority(a *authpkg.Auth) int {
	if a == nil || a.Attributes == nil {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(a.Attributes["priority"]))
	if err != nil {
		return 0
	}
	return value
}

func logOrderChange(previous, next Snapshot) {
	order := func(s Snapshot) string {
		parts := make([]string, 0, len(s.Entries))
		for _, e := range s.Entries {
			parts = append(parts, e.PlanID+"/"+e.State)
		}
		return strings.Join(parts, ",")
	}
	if previous.Enabled == next.Enabled && order(previous) == order(next) {
		return
	}
	if next.Enabled && len(next.Entries) > 0 {
		log.Infof("smart routing: order %s", order(next))
	}
}
