package util

import "testing"

func TestCommandCodeBaseURLAndCreditsURL(t *testing.T) {
	base := "https://api.commandcode.ai/provider/v1"
	if !IsCommandCodeBaseURL(base) {
		t.Fatalf("expected %q to be detected as Command Code", base)
	}
	if IsCommandCodeBaseURL("https://opencode.ai/zen/go/v1") {
		t.Fatalf("OpenCode Go base URL must not be detected as Command Code")
	}
	if got := CommandCodeCreditsURL(base); got != "https://api.commandcode.ai/alpha/billing/credits" {
		t.Fatalf("credits url = %q", got)
	}
	if got := CommandCodeCreditsURL(""); got != "https://api.commandcode.ai/alpha/billing/credits" {
		t.Fatalf("fallback credits url = %q", got)
	}
}

func TestCommandCodeQuotaProbe(t *testing.T) {
	probe := CommandCodeQuotaProbe("https://api.commandcode.ai/provider/v1")
	if probe["url"] != "https://api.commandcode.ai/alpha/billing/credits" {
		t.Fatalf("probe url = %v", probe["url"])
	}
	mapping, ok := probe["mapping"].(map[string]any)
	if !ok {
		t.Fatalf("mapping missing: %#v", probe["mapping"])
	}
	groups, ok := mapping["groups"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("groups = %#v", mapping["groups"])
	}
	group := groups[0].(map[string]any)
	buckets, ok := group["buckets"].([]any)
	if !ok || len(buckets) != 2 {
		t.Fatalf("buckets = %#v", group["buckets"])
	}
	first := buckets[0].(map[string]any)
	if first["used_amount"] != "windowLimits.fiveHour.used" || first["total_amount"] != "windowLimits.fiveHour.cap" {
		t.Fatalf("fiveHour bucket = %#v", first)
	}
}
