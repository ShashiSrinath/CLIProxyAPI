// Package planrouting ranks subscription plan credentials by their live usage
// limits and feeds the order to the auth scheduler as dynamic priorities.
package planrouting

import (
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/planlimits"
)

// Limit types.
const (
	LimitWeekly  = "weekly"
	LimitMonthly = "monthly"
)

// Routing overrides.
const (
	RoutingAuto   = "auto"
	RoutingPrefer = "prefer"
	RoutingAvoid  = "avoid"
)

// States.
const (
	StateActive   = "active"
	StatePaused   = "paused"
	StateDisabled = "disabled"
)

// Priority bands. Active plan credentials outrank credentials without a plan
// (default priority 0); paused ones fall below them.
const (
	activeBase = 1000
	pausedBase = -1000
)

// Candidate is one plan credential to rank.
type Candidate struct {
	AuthID, Label, Provider string
	PlanID, PlanName        string
	PlanIndex               int
	// ConfigLimitType is the plan's configured limit type; empty auto-detects.
	ConfigLimitType string
	// KnownMonthly marks providers known to cap usage monthly even when the
	// usage API does not report a monthly window.
	KnownMonthly   bool
	Routing        string
	Windows        []planlimits.Window
	CycleEnd       time.Time
	Disabled       bool
	ManualPriority int
}

// Entry is one ranked credential.
type Entry struct {
	Rank               int
	Priority           int
	AuthID             string
	Label              string
	Provider           string
	PlanID             string
	PlanName           string
	LimitType          string
	LimitTypeSource    string
	Routing            string
	FullResetAt        time.Time
	FullResetEstimated bool
	RemainingFraction  *float64
	State              string
	PausedUntil        time.Time
	PausedWindow       string

	planIndex int
	manual    int
}

// LimitType resolves the effective limit type and whether it came from config.
func LimitType(configured string, knownMonthly bool, windows []planlimits.Window) (string, string) {
	switch configured {
	case LimitWeekly, LimitMonthly:
		return configured, "config"
	}
	if knownMonthly {
		return LimitMonthly, "auto"
	}
	for _, w := range windows {
		if w.Key == "monthly" {
			return LimitMonthly, "auto"
		}
	}
	return LimitWeekly, "auto"
}

func normalizeRouting(r string) string {
	switch r {
	case RoutingPrefer, RoutingAvoid:
		return r
	}
	return RoutingAuto
}

// fullReset returns when the window that defines the limit type fully resets.
// Windows that have not started yet (or whose reset is past) are assumed to
// start now, which makes them the least urgent.
func fullReset(c Candidate, limitType string, now time.Time) (time.Time, bool, *float64) {
	target := 7 * 24 * time.Hour
	if limitType == LimitMonthly {
		target = 30 * 24 * time.Hour
	}
	var best *planlimits.Window
	for i := range c.Windows {
		w := &c.Windows[i]
		d := w.Duration()
		if d == 0 || d > target+24*time.Hour {
			continue
		}
		if best == nil || d > best.Duration() {
			best = w
		}
	}
	if best == nil {
		if limitType == LimitMonthly && c.CycleEnd.After(now) {
			return c.CycleEnd, true, nil
		}
		return now.Add(target), true, nil
	}
	var remaining *float64
	if best.HasFraction {
		r := 1 - best.Fraction
		if r < 0 {
			r = 0
		}
		remaining = &r
	}
	if best.Reset.After(now) {
		return best.Reset, false, remaining
	}
	if limitType == LimitMonthly && c.CycleEnd.After(now) {
		return c.CycleEnd, true, remaining
	}
	return now.Add(best.Duration()), true, remaining
}

// paused reports the latest reset among exhausted windows.
func paused(windows []planlimits.Window, now time.Time) (bool, time.Time, string) {
	isPaused, until, label := false, time.Time{}, ""
	for _, w := range windows {
		if !w.Exhausted {
			continue
		}
		if !w.Reset.IsZero() && !w.Reset.After(now) {
			continue
		}
		isPaused = true
		if label == "" || w.Reset.After(until) {
			until, label = w.Reset, planlimits.WindowLabel(w.Key)
		}
	}
	return isPaused, until, label
}

func stateOrder(s string) int {
	switch s {
	case StateActive:
		return 0
	case StatePaused:
		return 1
	}
	return 2
}

func routingOrder(r string) int {
	switch r {
	case RoutingPrefer:
		return 0
	case RoutingAvoid:
		return 2
	}
	return 1
}

// Rank orders plan credentials. With smart routing it applies, in order:
// paused last, routing override, weekly-only before monthly-limited, soonest
// full reset, most remaining headroom, plan config order. Without it, entries
// follow manual priority. Priority is the scheduler value to apply.
func Rank(candidates []Candidate, now time.Time, smart bool) []Entry {
	entries := make([]Entry, 0, len(candidates))
	for _, c := range candidates {
		limitType, source := LimitType(c.ConfigLimitType, c.KnownMonthly, c.Windows)
		reset, estimated, remaining := fullReset(c, limitType, now)
		e := Entry{
			AuthID: c.AuthID, Label: c.Label, Provider: c.Provider, PlanID: c.PlanID, PlanName: c.PlanName,
			LimitType: limitType, LimitTypeSource: source, Routing: normalizeRouting(c.Routing),
			FullResetAt: reset, FullResetEstimated: estimated, RemainingFraction: remaining,
			State: StateActive, planIndex: c.PlanIndex, manual: c.ManualPriority,
		}
		if c.Disabled {
			e.State = StateDisabled
		} else if isPaused, until, label := paused(c.Windows, now); isPaused {
			e.State, e.PausedUntil, e.PausedWindow = StatePaused, until, label
		}
		entries = append(entries, e)
	}
	less := func(a, b Entry) bool {
		if a.planIndex != b.planIndex {
			return a.planIndex < b.planIndex
		}
		return a.AuthID < b.AuthID
	}
	if !smart {
		sort.SliceStable(entries, func(i, j int) bool {
			a, b := entries[i], entries[j]
			if stateOrder(a.State) != stateOrder(b.State) && (a.State == StateDisabled || b.State == StateDisabled) {
				return stateOrder(a.State) < stateOrder(b.State)
			}
			if a.manual != b.manual {
				return a.manual > b.manual
			}
			return less(a, b)
		})
		for i := range entries {
			entries[i].Rank, entries[i].Priority = i+1, entries[i].manual
		}
		return entries
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if stateOrder(a.State) != stateOrder(b.State) {
			return stateOrder(a.State) < stateOrder(b.State)
		}
		if a.State == StatePaused {
			// Unknown resume times sort last.
			if !a.PausedUntil.Equal(b.PausedUntil) {
				if a.PausedUntil.IsZero() || b.PausedUntil.IsZero() {
					return b.PausedUntil.IsZero()
				}
				return a.PausedUntil.Before(b.PausedUntil)
			}
			return less(a, b)
		}
		if routingOrder(a.Routing) != routingOrder(b.Routing) {
			return routingOrder(a.Routing) < routingOrder(b.Routing)
		}
		if a.LimitType != b.LimitType {
			return a.LimitType == LimitWeekly
		}
		if !a.FullResetAt.Equal(b.FullResetAt) {
			return a.FullResetAt.Before(b.FullResetAt)
		}
		ra, rb := -1.0, -1.0
		if a.RemainingFraction != nil {
			ra = *a.RemainingFraction
		}
		if b.RemainingFraction != nil {
			rb = *b.RemainingFraction
		}
		if ra != rb {
			return ra > rb
		}
		return less(a, b)
	})
	n := len(entries)
	for i := range entries {
		entries[i].Rank = i + 1
		switch entries[i].State {
		case StateActive:
			entries[i].Priority = activeBase + n - i
		case StatePaused:
			entries[i].Priority = pausedBase + n - i
		default:
			entries[i].Priority = entries[i].manual
		}
	}
	return entries
}

// Overrides returns the scheduler priorities to apply for ranked entries.
func Overrides(entries []Entry) map[string]int {
	out := make(map[string]int, len(entries))
	for _, e := range entries {
		if e.State == StateDisabled {
			continue
		}
		out[e.AuthID] = e.Priority
	}
	return out
}
