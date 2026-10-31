package management

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/planlimits"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/planrouting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type planWindow struct {
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	WindowStart    string   `json:"window_start"`
	ResetsAt       string   `json:"resets_at"`
	UsedFraction   *float64 `json:"used_fraction"`
	ValueUSD       float64  `json:"value_usd"`
	EstCapacityUSD *float64 `json:"est_capacity_usd"`
	EstHeadroomUSD *float64 `json:"est_headroom_usd"`
	ProviderUsed   *float64 `json:"provider_used"`
	ProviderLimit  *float64 `json:"provider_limit"`
	// ProviderRemaining is set when the provider reports only a remaining balance.
	ProviderRemaining *float64 `json:"provider_remaining"`
	Unit              string   `json:"unit"`
	Exhausted         bool     `json:"exhausted"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
func usageQuery(from, to time.Time, loc *time.Location, bucket string) usagestats.SummaryQuery {
	return usagestats.SummaryQuery{From: from, To: to, Bucket: bucket, Location: loc}
}

// GetUsagePlans returns subscription value and live usage for configured plans.
func (h *Handler) GetUsagePlans(c *gin.Context) {
	now := time.Now()
	tz := strings.TrimSpace(c.Query("tz"))
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid_query", "message": "invalid timezone"})
		return
	}
	offset := 0
	if s := c.Query("cycle_offset"); s != "" {
		offset, err = strconv.Atoi(s)
		if err != nil || offset > 0 || offset < -12 {
			c.JSON(400, gin.H{"error": "invalid_query", "message": "cycle_offset must be between -12 and 0"})
			return
		}
	}
	tracker, pricing, cfg := h.usageDashboardState()
	payload := gin.H{"enabled": tracker != nil && pricing != nil, "generated_at": stamp(now), "tz": tz, "pricing": gin.H{"source": "none", "fetched_at": "", "catalog_size": 0, "error": ""}, "totals": gin.H{"price_usd": 0, "value_usd": 0, "multiplier": nil, "projected_value_usd": 0, "unassigned_value_usd": 0}, "plans": []any{}, "unassigned": []any{}, "available_credentials": []any{}, "providers": []string{}}
	if tracker == nil || pricing == nil || cfg == nil {
		c.JSON(http.StatusOK, payload)
		return
	}
	payload["pricing"] = pricing.Status()
	c.Header("Cache-Control", "no-store")
	var auths []*authpkg.Auth
	if h.authManager != nil {
		auths = h.authManager.List()
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	plans := cfg.UsageTracking.Subscriptions
	providers := map[string]string{}
	providerSet := map[string]bool{}
	for _, a := range auths {
		providers[a.ID] = a.Provider
		providerSet[a.Provider] = true
	}
	owners, legacy := usagestats.PlanOwners(plans, providers)
	available := []any{}
	providerList := []string{}
	for p := range providerSet {
		providerList = append(providerList, p)
	}
	sort.Strings(providerList)
	for _, a := range auths {
		id := ""
		if i, ok := owners[a.ID]; ok {
			id = plans[i].ID
		}
		available = append(available, gin.H{"auth_id": a.ID, "label": planlimits.Label(a), "provider": a.Provider, "plan_id": id})
	}
	payload["providers"] = providerList
	payload["available_credentials"] = available
	lives := make(map[string]planlimits.Live)
	anchors := make(map[string]time.Time)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range auths {
		if _, ok := owners[a.ID]; !ok {
			continue
		}
		wg.Add(1)
		go func(a *authpkg.Auth) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
			defer cancel()
			live := planlimits.Fetch(ctx, cfg, a)
			anchor := time.Time{}
			if plans[owners[a.ID]].AnchorDate == "" {
				anchor = planlimits.Anchor(ctx, cfg, a, live)
			}
			mu.Lock()
			lives[a.ID] = live
			anchors[a.ID] = anchor
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	resultPlans := []any{}
	totalPrice, totalValue, totalProjection := 0.0, 0.0, 0.0
	for i, p := range plans {
		cycle := p.Cycle
		if cycle == "" {
			cycle = "monthly"
		}
		anchor := time.Date(now.In(loc).Year(), now.In(loc).Month(), 1, 0, 0, 0, 0, loc)
		source := "default"
		if p.AnchorDate != "" {
			if v, e := time.ParseInLocation("2006-01-02", p.AnchorDate, loc); e == nil {
				anchor = v
				source = "config"
			}
		} else {
			for _, a := range auths {
				if owner, ok := owners[a.ID]; ok && owner == i && !anchors[a.ID].IsZero() {
					t := anchors[a.ID].In(loc)
					anchor = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
					source = "provider"
					break
				}
			}
		}
		start, end := usagestats.CycleBounds(now, anchor, cycle, offset, loc)
		until := end
		if now.Before(until) {
			until = now
		}
		ids := map[string]bool{}
		for id, owner := range owners {
			if owner == i {
				ids[id] = true
			}
		}
		legacyProvider := ""
		if j, ok := legacy[p.Provider]; ok && j == i {
			legacyProvider = p.Provider
		}
		summary := tracker.SummaryForCredentials(usageQuery(start, until, loc, "day"), ids, legacyProvider, pricing)
		series := []any{}
		cumulative := 0.0
		for _, point := range summary.Series {
			cumulative += point.CostUSD
			series = append(series, gin.H{"t": point.T, "value_usd": point.CostUSD, "cumulative_usd": cumulative})
		}
		for j := range summary.Models {
			summary.Models[j].Keys = nil
		}
		elapsed := until.Sub(start).Hours() / 24
		length := end.Sub(start).Hours() / 24
		lastStart := until.Add(-7 * 24 * time.Hour)
		if lastStart.Before(start) {
			lastStart = start
		}
		last := tracker.SummaryForCredentials(usageQuery(lastStart, until, loc, "day"), ids, legacyProvider, pricing).Totals.CostUSD
		projection := usagestats.PlanProjection(summary.Totals.CostUSD, last, elapsed, length)
		hourly := tracker.SummaryForCredentials(usageQuery(start, until, loc, "hour"), ids, legacyProvider, pricing)
		breakEven := usagestats.BreakEven(hourly.Series, p.PriceUSD)
		tokens := summary.Totals.InputTokens + summary.Totals.OutputTokens
		var effective any
		if tokens > 0 {
			effective = p.PriceUSD / (float64(tokens) / 1e6)
		}
		unpriced := int64(0)
		for _, m := range summary.Models {
			if !m.Priced {
				unpriced += m.Totals.InputTokens + m.Totals.OutputTokens
			}
		}
		share := 0.0
		if tokens > 0 {
			share = float64(unpriced) / float64(tokens)
		}
		credentials := []any{}
		knownMonthly := false
		var planWindows []planlimits.Window
		for _, a := range auths {
			if !ids[a.ID] {
				continue
			}
			single := map[string]bool{a.ID: true}
			value := tracker.SummaryForCredentials(usageQuery(start, until, loc, "day"), single, "", pricing).Totals.CostUSD
			windows := []planWindow{}
			for _, w := range lives[a.ID].Windows {
				val := 0.0
				if !w.Start.IsZero() && w.Start.Before(now) {
					val = tracker.SummaryForCredentials(usageQuery(w.Start, now, loc, "day"), single, "", pricing).Totals.CostUSD
				}
				var fraction, capacity, headroom *float64
				if w.HasFraction {
					f := w.Fraction
					fraction = &f
					capacity, headroom = usagestats.WindowCapacity(val, w.Fraction)
				}
				windows = append(windows, planWindow{Key: w.Key, Label: planlimits.WindowLabel(w.Key), WindowStart: stamp(w.Start), ResetsAt: stamp(w.Reset), UsedFraction: fraction, ValueUSD: val, EstCapacityUSD: capacity, EstHeadroomUSD: headroom, ProviderUsed: w.Used, ProviderLimit: w.Limit, ProviderRemaining: w.Remaining, Unit: w.Unit, Exhausted: w.Exhausted})
			}
			if planlimits.HasMonthlyLimit(a) {
				knownMonthly = true
			}
			planWindows = append(planWindows, lives[a.ID].Windows...)
			credentials = append(credentials, gin.H{"auth_id": a.ID, "label": planlimits.Label(a), "provider": a.Provider, "status": a.Status, "disabled": a.Disabled, "value_usd": value, "windows": windows, "windows_error": lives[a.ID].Err})
		}
		limitType, limitSource := planrouting.LimitType(p.LimitType, knownMonthly, planWindows)
		resultPlans = append(resultPlans, gin.H{"id": p.ID, "name": p.Name, "provider": p.Provider, "notes": p.Notes, "price_usd": p.PriceUSD, "cycle": cycle, "anchor_date": anchor.Format("2006-01-02"), "anchor_source": source, "cycle_start": stamp(start), "cycle_end": stamp(end), "elapsed_days": elapsed, "length_days": length, "value_usd": summary.Totals.CostUSD, "multiplier": usagestats.PlanMultiplier(summary.Totals.CostUSD, p.PriceUSD), "break_even_at": breakEven, "projected_value_usd": projection, "last_7d_value_usd": last, "effective_usd_per_mtok": effective, "unpriced_token_share": share, "totals": summary.Totals, "series": series, "models": summary.Models, "credentials": credentials, "limit_type": limitType, "limit_type_source": limitSource, "routing": planRoutingMode(p.Routing)})
		totalPrice += p.PriceUSD
		totalValue += summary.Totals.CostUSD
		totalProjection += projection
	}
	// Unassigned usage uses the current calendar month as a common comparison period.
	monthStart, monthEnd := usagestats.CycleBounds(now, time.Date(now.In(loc).Year(), now.In(loc).Month(), 1, 0, 0, 0, 0, loc), "monthly", offset, loc)
	if now.Before(monthEnd) {
		monthEnd = now
	}
	q := usageQuery(monthStart, monthEnd, loc, "day")
	unassignedTotal := tracker.SummaryUnassigned(q, owners, legacy, pricing).Totals.CostUSD
	unassigned := []any{}
	for _, a := range auths {
		if _, ok := owners[a.ID]; ok {
			continue
		}
		s := tracker.SummaryForCredentials(q, map[string]bool{a.ID: true}, "", pricing)
		if s.Totals.Requests > 0 || s.Totals.CostUSD > 0 {
			unassigned = append(unassigned, gin.H{"auth_id": a.ID, "label": planlimits.Label(a), "provider": a.Provider, "value_usd": s.Totals.CostUSD, "requests": s.Totals.Requests})
		}
	}
	// Include historical credentials and legacy provider buckets even after removal.
	for identity, provider := range tracker.UnassignedIdentities(q, owners, legacy) {
		if _, exists := providers[identity]; exists {
			continue
		}
		id, label, fallback := identity, identity, ""
		ids := map[string]bool{identity: true}
		if strings.HasPrefix(identity, "\x00") {
			id = ""
			label = provider
			fallback = provider
			ids = map[string]bool{}
		}
		s := tracker.SummaryForCredentials(q, ids, fallback, pricing)
		if s.Totals.Requests > 0 || s.Totals.CostUSD > 0 {
			unassigned = append(unassigned, gin.H{"auth_id": id, "label": label, "provider": provider, "value_usd": s.Totals.CostUSD, "requests": s.Totals.Requests})
		}
	}
	payload["routing"] = h.routingPayload(c.Request.Context())
	payload["plans"] = resultPlans
	payload["unassigned"] = unassigned
	payload["totals"] = gin.H{"price_usd": totalPrice, "value_usd": totalValue, "multiplier": usagestats.PlanMultiplier(totalValue, totalPrice), "projected_value_usd": totalProjection, "unassigned_value_usd": unassignedTotal}
	c.JSON(http.StatusOK, payload)
}

func planRoutingMode(routing string) string {
	switch routing {
	case planrouting.RoutingPrefer, planrouting.RoutingAvoid:
		return routing
	}
	return planrouting.RoutingAuto
}

// routingPayload reports the order smart routing currently applies to plan credentials.
func (h *Handler) routingPayload(ctx context.Context) gin.H {
	payload := gin.H{"enabled": false, "computed_at": "", "refresh_seconds": int(planrouting.RefreshInterval / time.Second), "order": []any{}}
	if h.planRouting == nil {
		if h.cfg != nil {
			payload["enabled"] = h.cfg.UsageTracking.SmartRoutingEnabled()
		}
		return payload
	}
	// Management saves update h.cfg before the async server reload reaches the
	// engine; applying it here makes the response reflect the saved plans.
	if h.cfg != nil {
		h.planRouting.Apply(h.cfg)
	}
	snap := h.planRouting.Snapshot(ctx)
	order := make([]any, 0, len(snap.Entries))
	for _, e := range snap.Entries {
		order = append(order, gin.H{
			"rank": e.Rank, "auth_id": e.AuthID, "label": e.Label, "provider": e.Provider,
			"plan_id": e.PlanID, "plan_name": e.PlanName, "priority": e.Priority,
			"limit_type": e.LimitType, "limit_type_source": e.LimitTypeSource, "routing": e.Routing,
			"full_reset_at": stamp(e.FullResetAt), "full_reset_estimated": e.FullResetEstimated,
			"remaining_fraction": e.RemainingFraction, "state": e.State,
			"paused_until": stamp(e.PausedUntil), "paused_window": e.PausedWindow,
		})
	}
	payload["enabled"] = snap.Enabled
	payload["computed_at"] = stamp(snap.ComputedAt)
	payload["order"] = order
	return payload
}
