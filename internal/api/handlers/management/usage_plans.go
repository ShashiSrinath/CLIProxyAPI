package management

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type planWindow struct {
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	WindowStart    string   `json:"window_start"`
	ResetsAt       string   `json:"resets_at"`
	UsedFraction   float64  `json:"used_fraction"`
	ValueUSD       float64  `json:"value_usd"`
	EstCapacityUSD *float64 `json:"est_capacity_usd"`
	EstHeadroomUSD *float64 `json:"est_headroom_usd"`
	ProviderUsed   *float64 `json:"provider_used"`
	ProviderLimit  *float64 `json:"provider_limit"`
	Unit           string   `json:"unit"`
	Exhausted      bool     `json:"exhausted"`
}
type liveWindow struct {
	key          string
	start, reset time.Time
	fraction     float64
	used, limit  *float64
	unit         string
	exhausted    bool
}
type liveResult struct {
	windows   []liveWindow
	err       string
	at        time.Time
	signature string
}
type anchorResult struct {
	end, at   time.Time
	signature string
}

var plansCache = struct {
	sync.Mutex
	windows map[string]liveResult
	anchors map[string]anchorResult
}{windows: map[string]liveResult{}, anchors: map[string]anchorResult{}}

func credentialLabel(a *authpkg.Auth) string {
	if name := a.Attributes["compat_name"]; name != "" {
		key := a.Attributes["api_key"]
		mask := "••••"
		if len(key) > 8 {
			mask = key[:4] + "…" + key[len(key)-4:]
		}
		return name + " · " + mask
	}
	for _, k := range []string{"email", "account"} {
		if v := a.Attributes[k]; v != "" {
			return v
		}
		if v, ok := a.Metadata[k].(string); ok && v != "" {
			return v
		}
	}
	if a.FileName != "" {
		return filepath.Base(a.FileName)
	}
	return filepath.Base(a.ID)
}
func liveSignature(a *authpkg.Auth) string {
	return a.Attributes["base_url"] + "\x00" + a.Attributes["api_key"] + "\x00" + a.Attributes["opendesign_session"] + "\x00" + a.Attributes["opendesign_workspace_id"] + "\x00" + a.ProxyURL
}
func liveUsage(ctx context.Context, cfg *config.Config, a *authpkg.Auth) liveResult {
	sig := liveSignature(a)
	now := time.Now()
	plansCache.Lock()
	cached, ok := plansCache.windows[a.ID]
	plansCache.Unlock()
	if ok && cached.signature == sig && now.Sub(cached.at) < time.Minute {
		return cached
	}
	result := liveResult{at: now, signature: sig, windows: []liveWindow{}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := helps.NewProxyAwareHTTPClient(ctx, cfg, a, 10*time.Second)
	base, key := a.Attributes["base_url"], a.Attributes["api_key"]
	var err error
	switch {
	case util.IsOpenCodeGoBaseURL(base):
		var rows []helps.OpenCodeGoUsageWindow
		rows, err = helps.FetchOpenCodeGoUsage(ctx, client, base, key)
		for _, w := range rows {
			start := windowStart(w.Key, w.ResetsAt)
			result.windows = append(result.windows, liveWindow{key: w.Key, start: start, reset: w.ResetsAt, fraction: w.Percent / 100, unit: "percent", exhausted: w.Exhausted()})
		}
	case util.IsCommandCodeBaseURL(base):
		var rows []helps.CommandCodeUsageWindow
		rows, err = helps.FetchCommandCodeUsage(ctx, client, base, key)
		for _, w := range rows {
			fraction := 0.0
			if w.Cap > 0 {
				fraction = w.Used / w.Cap
			}
			used, limit := w.Used, w.Cap
			result.windows = append(result.windows, liveWindow{key: w.Key, start: windowStart(w.Key, w.ResetsAt), reset: w.ResetsAt, fraction: fraction, used: &used, limit: &limit, unit: "usd", exhausted: w.Exhausted()})
		}
	case util.IsOpenDesignBaseURL(base) && a.Attributes["opendesign_session"] != "" && a.Attributes["opendesign_workspace_id"] != "":
		var usage *helps.OpenDesignUsage
		usage, err = helps.FetchOpenDesignUsage(ctx, client, a.Attributes["opendesign_session"], a.Attributes["opendesign_workspace_id"])
		if usage != nil {
			for _, w := range usage.Windows {
				fraction := 0.0
				if w.LimitCredits > 0 {
					fraction = w.UsedCredits / w.LimitCredits
				}
				used, limit := w.UsedCredits, w.LimitCredits
				k := fmt.Sprintf("%ds", w.DurationSeconds)
				result.windows = append(result.windows, liveWindow{key: k, start: w.WindowStart, reset: w.ResetsAt, fraction: fraction, used: &used, limit: &limit, unit: "credits", exhausted: w.LimitCredits > 0 && w.UsedCredits >= w.LimitCredits})
			}
		}
	}
	if err != nil {
		result.err = "Unable to fetch usage windows"
	}
	plansCache.Lock()
	plansCache.windows[a.ID] = result
	plansCache.Unlock()
	return result
}
func windowStart(key string, reset time.Time) time.Time {
	if reset.IsZero() {
		return time.Time{}
	}
	switch key {
	case "rolling", "fiveHour":
		return reset.Add(-5 * time.Hour)
	case "weekly":
		return reset.Add(-7 * 24 * time.Hour)
	case "monthly":
		y, m, d := reset.Date()
		first := time.Date(y, m-1, 1, reset.Hour(), reset.Minute(), reset.Second(), reset.Nanosecond(), reset.Location())
		last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, reset.Location()).Day()
		if d > last {
			d = last
		}
		return time.Date(first.Year(), first.Month(), d, reset.Hour(), reset.Minute(), reset.Second(), reset.Nanosecond(), reset.Location())
	}
	return reset
}
func providerAnchor(ctx context.Context, cfg *config.Config, a *authpkg.Auth, live liveResult) time.Time {
	base := a.Attributes["base_url"]
	if util.IsOpenCodeGoBaseURL(base) {
		for _, w := range live.windows {
			if w.key == "monthly" {
				return w.reset
			}
		}
		return time.Time{}
	}
	if !util.IsCommandCodeBaseURL(base) {
		return time.Time{}
	}
	sig := liveSignature(a)
	plansCache.Lock()
	cached, ok := plansCache.anchors[a.ID]
	plansCache.Unlock()
	if ok && cached.signature == sig && time.Since(cached.at) < time.Hour {
		return cached.end
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	end, err := helps.FetchCommandCodeSubscriptionPeriodEnd(ctx, helps.NewProxyAwareHTTPClient(ctx, cfg, a, 10*time.Second), base, a.Attributes["api_key"])
	if err == nil && !end.IsZero() {
		plansCache.Lock()
		plansCache.anchors[a.ID] = anchorResult{end: end, at: time.Now(), signature: sig}
		plansCache.Unlock()
	}
	return end
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
		available = append(available, gin.H{"auth_id": a.ID, "label": credentialLabel(a), "provider": a.Provider, "plan_id": id})
	}
	payload["providers"] = providerList
	payload["available_credentials"] = available
	lives := make(map[string]liveResult)
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
			live := liveUsage(ctx, cfg, a)
			anchor := time.Time{}
			if plans[owners[a.ID]].AnchorDate == "" {
				anchor = providerAnchor(ctx, cfg, a, live)
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
		for _, a := range auths {
			if !ids[a.ID] {
				continue
			}
			single := map[string]bool{a.ID: true}
			value := tracker.SummaryForCredentials(usageQuery(start, until, loc, "day"), single, "", pricing).Totals.CostUSD
			windows := []planWindow{}
			for _, w := range lives[a.ID].windows {
				val := 0.0
				if !w.start.IsZero() && w.start.Before(now) {
					val = tracker.SummaryForCredentials(usageQuery(w.start, now, loc, "day"), single, "", pricing).Totals.CostUSD
				}
				cap, head := usagestats.WindowCapacity(val, w.fraction)
				windows = append(windows, planWindow{Key: w.key, Label: windowLabel(w.key), WindowStart: stamp(w.start), ResetsAt: stamp(w.reset), UsedFraction: w.fraction, ValueUSD: val, EstCapacityUSD: cap, EstHeadroomUSD: head, ProviderUsed: w.used, ProviderLimit: w.limit, Unit: w.unit, Exhausted: w.exhausted})
			}
			credentials = append(credentials, gin.H{"auth_id": a.ID, "label": credentialLabel(a), "provider": a.Provider, "status": a.Status, "disabled": a.Disabled, "value_usd": value, "windows": windows, "windows_error": lives[a.ID].err})
		}
		resultPlans = append(resultPlans, gin.H{"id": p.ID, "name": p.Name, "provider": p.Provider, "notes": p.Notes, "price_usd": p.PriceUSD, "cycle": cycle, "anchor_date": anchor.Format("2006-01-02"), "anchor_source": source, "cycle_start": stamp(start), "cycle_end": stamp(end), "elapsed_days": elapsed, "length_days": length, "value_usd": summary.Totals.CostUSD, "multiplier": usagestats.PlanMultiplier(summary.Totals.CostUSD, p.PriceUSD), "break_even_at": breakEven, "projected_value_usd": projection, "last_7d_value_usd": last, "effective_usd_per_mtok": effective, "unpriced_token_share": share, "totals": summary.Totals, "series": series, "models": summary.Models, "credentials": credentials})
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
			unassigned = append(unassigned, gin.H{"auth_id": a.ID, "label": credentialLabel(a), "provider": a.Provider, "value_usd": s.Totals.CostUSD, "requests": s.Totals.Requests})
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
	payload["plans"] = resultPlans
	payload["unassigned"] = unassigned
	payload["totals"] = gin.H{"price_usd": totalPrice, "value_usd": totalValue, "multiplier": usagestats.PlanMultiplier(totalValue, totalPrice), "projected_value_usd": totalProjection, "unassigned_value_usd": unassignedTotal}
	c.JSON(http.StatusOK, payload)
}
func windowLabel(key string) string {
	switch key {
	case "rolling", "fiveHour":
		return "5 hour"
	case "weekly":
		return "Weekly"
	case "monthly":
		return "Monthly"
	}
	// OpenDesign windows are keyed by duration, e.g. "18000s".
	if d, err := time.ParseDuration(key); err == nil {
		switch {
		case d == 5*time.Hour:
			return "5 hour"
		case d == 7*24*time.Hour:
			return "Weekly"
		case d%(24*time.Hour) == 0:
			return fmt.Sprintf("%d day", d/(24*time.Hour))
		case d%time.Hour == 0:
			return fmt.Sprintf("%d hour", d/time.Hour)
		}
	}
	return key
}
