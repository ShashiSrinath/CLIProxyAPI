package usagestats

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"testing"
	"time"
)

func TestPlanCycleBounds(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2024, 1, 31, 0, 0, 0, 0, loc)
	now := time.Date(2024, 3, 10, 12, 0, 0, 0, loc)
	start, end := CycleBounds(now, anchor, "monthly", 0, loc)
	if start.Format("2006-01-02 -0700") != "2024-02-29 +0530" || end.Format("2006-01-02") != "2024-03-31" {
		t.Fatalf("%v %v", start, end)
	}
	start, end = CycleBounds(now, anchor, "monthly", -1, loc)
	if start.Day() != 31 || start.Month() != time.January || end.Day() != 29 {
		t.Fatalf("%v %v", start, end)
	}
	start, end = CycleBounds(now, anchor, "yearly", 0, loc)
	if start.Year() != 2024 || end.Year() != 2025 {
		t.Fatalf("%v %v", start, end)
	}
	start, end = CycleBounds(now, anchor, "weekly", 0, loc)
	if end.Sub(start) != 7*24*time.Hour || start.After(now) || !end.After(now) {
		t.Fatalf("%v %v", start, end)
	}
}
func TestPlanMatching(t *testing.T) {
	plans := []config.SubscriptionPlan{{Provider: "x"}, {Provider: "x", Credentials: []string{"a"}}, {Provider: "x"}}
	owners, legacy := PlanOwners(plans, map[string]string{"a": "x", "b": "x", "c": "y"})
	if owners["a"] != 1 || owners["b"] != 0 || legacy["x"] != 0 {
		t.Fatal(owners, legacy)
	}
	if _, ok := owners["c"]; ok {
		t.Fatal(owners)
	}
}

func TestPlanMatchingLegacyForSoleOwner(t *testing.T) {
	plans := []config.SubscriptionPlan{
		{Provider: "x", Credentials: []string{"a"}},
		{Provider: "y", Credentials: []string{"b"}},
		{Provider: "z", Credentials: []string{"d"}},
	}
	// x is fully owned by plan 0; y is split with an unassigned credential; z is
	// split across plan 2 and an unassigned credential too.
	_, legacy := PlanOwners(plans, map[string]string{"a": "x", "b": "y", "c": "y", "d": "z", "e": "z"})
	if i, ok := legacy["x"]; !ok || i != 0 {
		t.Fatal(legacy)
	}
	if _, ok := legacy["y"]; ok {
		t.Fatal(legacy)
	}
	if _, ok := legacy["z"]; ok {
		t.Fatal(legacy)
	}
}
func TestPlanValueMath(t *testing.T) {
	series := []SeriesPoint{{T: "first", Stats: Stats{CostUSD: 3}}, {T: "second", Stats: Stats{CostUSD: 4}}}
	if got := BreakEven(series, 5); got == nil || *got != "second" || BreakEven(series, 8) != nil {
		t.Fatal("break even")
	}
	if PlanMultiplier(10, 0) != nil || *PlanMultiplier(10, 2) != 5 {
		t.Fatal("multiplier")
	}
	if p := PlanProjection(70, 140, 7, 28); p != 420 {
		t.Fatal(p)
	}
	if p := PlanProjection(10, 0, 2, 30); p != 150 {
		t.Fatal(p)
	}
	if c, h := WindowCapacity(4, .049); c != nil || h != nil {
		t.Fatal(c, h)
	}
	if c, h := WindowCapacity(4, .1); *c != 40 || *h != 36 {
		t.Fatal(c, h)
	}
}
