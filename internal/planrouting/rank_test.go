package planrouting

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/planlimits"
)

var now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func days(n float64) time.Time { return now.Add(time.Duration(n * 24 * float64(time.Hour))) }

func ids(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.AuthID
	}
	return out
}

func assertOrder(t *testing.T, entries []Entry, want ...string) {
	t.Helper()
	got := ids(entries)
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// prodLike mirrors a real mix: weekly-only OpenDesign keys, OpenCode Go keys
// with monthly caps, and Command Code keys whose monthly cap is not a window.
func prodLike() []Candidate {
	return []Candidate{
		{AuthID: "opendesign-1", PlanIndex: 0, Windows: []planlimits.Window{
			{Key: "18000s", Reset: days(0.1), HasFraction: true},
			{Key: "604800s", Reset: days(3), Fraction: 1, HasFraction: true, Exhausted: true},
		}},
		{AuthID: "opencode-office", PlanIndex: 1, KnownMonthly: true, Windows: []planlimits.Window{
			{Key: "rolling", Reset: days(0.2), HasFraction: true},
			{Key: "weekly", Reset: days(1), HasFraction: true},
			{Key: "monthly", Reset: days(4), Fraction: 1, HasFraction: true, Exhausted: true},
		}},
		{AuthID: "opendesign-2", PlanIndex: 2, Windows: []planlimits.Window{
			{Key: "18000s", Reset: days(0.2), HasFraction: true},
			{Key: "604800s", Reset: days(4.2), Fraction: 0.68, HasFraction: true},
		}},
		{AuthID: "command-code", PlanIndex: 3, KnownMonthly: true, Windows: []planlimits.Window{
			{Key: "fiveHour", HasFraction: true},
			{Key: "weekly", HasFraction: true},
			{Key: "monthly", Reset: days(5)},
		}},
		{AuthID: "opencode-new", PlanIndex: 4, KnownMonthly: true, Windows: []planlimits.Window{
			{Key: "weekly", Reset: days(1), Fraction: 0.02, HasFraction: true},
			{Key: "monthly", Reset: days(29), Fraction: 0.01, HasFraction: true},
		}},
	}
}

func TestRankWeeklyBeforeMonthlyThenSoonestResetPausedLast(t *testing.T) {
	entries := Rank(prodLike(), now, true)
	assertOrder(t, entries, "opendesign-2", "command-code", "opencode-new", "opendesign-1", "opencode-office")
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.AuthID] = e
	}
	if e := byID["opendesign-2"]; e.LimitType != LimitWeekly || e.LimitTypeSource != "auto" || !e.FullResetAt.Equal(days(4.2)) || e.FullResetEstimated || e.RemainingFraction == nil || *e.RemainingFraction < 0.31 || *e.RemainingFraction > 0.33 {
		t.Fatalf("opendesign-2 = %+v", e)
	}
	if e := byID["command-code"]; e.LimitType != LimitMonthly || !e.FullResetAt.Equal(days(5)) || e.RemainingFraction != nil {
		t.Fatalf("command-code = %+v", e)
	}
	if e := byID["opendesign-1"]; e.State != StatePaused || !e.PausedUntil.Equal(days(3)) || e.PausedWindow != "Weekly" {
		t.Fatalf("opendesign-1 = %+v", e)
	}
	if e := byID["opencode-office"]; e.State != StatePaused || e.PausedWindow != "Monthly" {
		t.Fatalf("opencode-office = %+v", e)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Priority >= entries[i-1].Priority {
			t.Fatalf("priorities not strictly decreasing: %v", entries)
		}
	}
	if entries[2].Priority <= 0 || entries[3].Priority >= 0 {
		t.Fatalf("active must outrank unplanned (0), paused must not: %d %d", entries[2].Priority, entries[3].Priority)
	}
	if overrides := Overrides(entries); len(overrides) != 5 {
		t.Fatalf("overrides = %v", overrides)
	}
}

func TestRankRoutingAndLimitTypeOverrides(t *testing.T) {
	candidates := prodLike()
	candidates[4].Routing = RoutingPrefer    // opencode-new jumps to the top
	candidates[2].Routing = RoutingAvoid     // opendesign-2 drops to the bottom of active
	candidates[3].ConfigLimitType = "weekly" // command code treated as weekly-only
	entries := Rank(candidates, now, true)
	assertOrder(t, entries, "opencode-new", "command-code", "opendesign-2", "opendesign-1", "opencode-office")
	if entries[1].LimitType != LimitWeekly || entries[1].LimitTypeSource != "config" || !entries[1].FullResetEstimated || !entries[1].FullResetAt.Equal(days(7)) {
		t.Fatalf("command-code = %+v", entries[1])
	}
}

func TestRankSoonestResetWithinGroupAndHeadroomTieBreak(t *testing.T) {
	week := func(id string, index int, reset time.Time, used float64) Candidate {
		return Candidate{AuthID: id, PlanIndex: index, Windows: []planlimits.Window{{Key: "weekly", Reset: reset, Fraction: used, HasFraction: true}}}
	}
	entries := Rank([]Candidate{
		week("late", 0, days(6), 0),
		week("soon-busy", 1, days(2), 0.9),
		week("soon-idle", 2, days(2), 0.1),
		// Not started yet: assumed to reset a full window from now.
		{AuthID: "unstarted", PlanIndex: 3, Windows: []planlimits.Window{{Key: "weekly", HasFraction: true}}},
	}, now, true)
	assertOrder(t, entries, "soon-idle", "soon-busy", "late", "unstarted")
	if !entries[3].FullResetEstimated {
		t.Fatalf("unstarted = %+v", entries[3])
	}
}

func TestRankMonthlyWithoutWindowsUsesCycleEnd(t *testing.T) {
	entries := Rank([]Candidate{
		{AuthID: "a", PlanIndex: 0, ConfigLimitType: "monthly", CycleEnd: days(20)},
		{AuthID: "b", PlanIndex: 1, ConfigLimitType: "monthly", CycleEnd: days(3)},
		{AuthID: "off", PlanIndex: 2, Disabled: true},
	}, now, true)
	assertOrder(t, entries, "b", "a", "off")
	if !entries[0].FullResetAt.Equal(days(3)) || !entries[0].FullResetEstimated {
		t.Fatalf("b = %+v", entries[0])
	}
	if entries[2].State != StateDisabled {
		t.Fatalf("off = %+v", entries[2])
	}
	if _, ok := Overrides(entries)["off"]; ok {
		t.Fatal("disabled credentials must keep their configured priority")
	}
}

func TestRankManualWhenSmartRoutingDisabled(t *testing.T) {
	candidates := prodLike()
	candidates[0].ManualPriority = 1
	candidates[4].ManualPriority = 9
	entries := Rank(candidates, now, false)
	assertOrder(t, entries, "opencode-new", "opendesign-1", "opencode-office", "opendesign-2", "command-code")
	if entries[0].Priority != 9 || entries[0].Rank != 1 {
		t.Fatalf("first = %+v", entries[0])
	}
}

func TestLimitTypeDetection(t *testing.T) {
	if lt, src := LimitType("", false, []planlimits.Window{{Key: "monthly"}}); lt != LimitMonthly || src != "auto" {
		t.Fatal(lt, src)
	}
	if lt, _ := LimitType("", false, []planlimits.Window{{Key: "604800s"}}); lt != LimitWeekly {
		t.Fatal(lt)
	}
	if lt, _ := LimitType("", true, nil); lt != LimitMonthly {
		t.Fatal(lt)
	}
	if lt, src := LimitType("weekly", true, nil); lt != LimitWeekly || src != "config" {
		t.Fatal(lt, src)
	}
}
