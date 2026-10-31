// Package planlimits reads live subscription usage windows for plan credentials.
package planlimits

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Window is one provider usage limit window.
type Window struct {
	Key          string
	Start, Reset time.Time
	// Fraction is the used share of the window; valid only when HasFraction.
	Fraction    float64
	HasFraction bool
	Used, Limit *float64
	// Remaining is the provider-reported balance left, when it only reports a balance.
	Remaining *float64
	Unit      string
	Exhausted bool
}

// Duration returns the nominal length of the window, or 0 when unknown.
func (w Window) Duration() time.Duration {
	switch w.Key {
	case "rolling", "fiveHour":
		return 5 * time.Hour
	case "weekly":
		return 7 * 24 * time.Hour
	case "monthly":
		return 30 * 24 * time.Hour
	}
	if d, err := time.ParseDuration(w.Key); err == nil && d > 0 {
		return d
	}
	return 0
}

// Live is the result of one live usage fetch.
type Live struct {
	Windows   []Window
	Err       string
	At        time.Time
	signature string
}

type anchorResult struct {
	end, at   time.Time
	signature string
}

var cache = struct {
	sync.Mutex
	windows map[string]Live
	anchors map[string]anchorResult
}{windows: map[string]Live{}, anchors: map[string]anchorResult{}}

// CacheTTL bounds how long a live fetch is reused.
const CacheTTL = time.Minute

// Label returns a display label that never exposes raw keys.
func Label(a *authpkg.Auth) string {
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

func signature(a *authpkg.Auth) string {
	return a.Attributes["base_url"] + "\x00" + a.Attributes["api_key"] + "\x00" + a.Attributes["opendesign_session"] + "\x00" + a.Attributes["opendesign_workspace_id"] + "\x00" + a.ProxyURL
}

// HasMonthlyLimit reports whether the credential's provider is known to cap usage per month.
func HasMonthlyLimit(a *authpkg.Auth) bool {
	base := a.Attributes["base_url"]
	return util.IsOpenCodeGoBaseURL(base) || util.IsCommandCodeBaseURL(base)
}

// Fetch returns the live usage windows of a credential, cached for CacheTTL.
// Credentials without a supported usage API return no windows and no error.
func Fetch(ctx context.Context, cfg *config.Config, a *authpkg.Auth) Live {
	sig := signature(a)
	now := time.Now()
	cache.Lock()
	cached, ok := cache.windows[a.ID]
	cache.Unlock()
	if ok && cached.signature == sig && now.Sub(cached.At) < CacheTTL {
		return cached
	}
	result := Live{At: now, signature: sig, Windows: []Window{}}
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
			result.Windows = append(result.Windows, Window{Key: w.Key, Start: WindowStart(w.Key, w.ResetsAt), Reset: w.ResetsAt, Fraction: w.Percent / 100, HasFraction: true, Unit: "percent", Exhausted: w.Exhausted()})
		}
	case util.IsCommandCodeBaseURL(base):
		var rows []helps.CommandCodeUsageWindow
		var credits helps.CommandCodeCredits
		rows, credits, err = helps.FetchCommandCodeBilling(ctx, client, base, key)
		for _, w := range rows {
			fraction := 0.0
			if w.Cap > 0 {
				fraction = w.Used / w.Cap
			}
			used, limit := w.Used, w.Cap
			result.Windows = append(result.Windows, Window{Key: w.Key, Start: WindowStart(w.Key, w.ResetsAt), Reset: w.ResetsAt, Fraction: fraction, HasFraction: true, Used: &used, Limit: &limit, Unit: "usd", Exhausted: w.Exhausted()})
		}
		if err == nil && credits.Known {
			// Monthly plan credits refill at the subscription period end; the API
			// reports only the remaining balance, not the monthly allowance.
			end := Anchor(ctx, cfg, a, result)
			remaining := credits.Monthly + credits.Purchased
			result.Windows = append(result.Windows, Window{Key: "monthly", Start: WindowStart("monthly", end), Reset: end, Remaining: &remaining, Unit: "usd", Exhausted: remaining <= 0})
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
				result.Windows = append(result.Windows, Window{Key: k, Start: w.WindowStart, Reset: w.ResetsAt, Fraction: fraction, HasFraction: true, Used: &used, Limit: &limit, Unit: "credits", Exhausted: w.LimitCredits > 0 && w.UsedCredits >= w.LimitCredits})
			}
		}
	}
	if err != nil {
		result.Err = "Unable to fetch usage windows"
	}
	cache.Lock()
	cache.windows[a.ID] = result
	cache.Unlock()
	return result
}

// WindowStart derives a window start from its reset for providers that only report resets.
func WindowStart(key string, reset time.Time) time.Time {
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

// Anchor returns a provider-reported billing period end, or zero when unknown.
func Anchor(ctx context.Context, cfg *config.Config, a *authpkg.Auth, live Live) time.Time {
	base := a.Attributes["base_url"]
	if util.IsOpenCodeGoBaseURL(base) {
		for _, w := range live.Windows {
			if w.Key == "monthly" {
				return w.Reset
			}
		}
		return time.Time{}
	}
	if !util.IsCommandCodeBaseURL(base) {
		return time.Time{}
	}
	sig := signature(a)
	cache.Lock()
	cached, ok := cache.anchors[a.ID]
	cache.Unlock()
	if ok && cached.signature == sig && time.Since(cached.at) < time.Hour {
		return cached.end
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	end, err := helps.FetchCommandCodeSubscriptionPeriodEnd(ctx, helps.NewProxyAwareHTTPClient(ctx, cfg, a, 10*time.Second), base, a.Attributes["api_key"])
	if err == nil && !end.IsZero() {
		cache.Lock()
		cache.anchors[a.ID] = anchorResult{end: end, at: time.Now(), signature: sig}
		cache.Unlock()
	}
	return end
}

// WindowLabel returns a human label for a window key.
func WindowLabel(key string) string {
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
