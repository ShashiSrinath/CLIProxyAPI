package planlimits

import (
	"context"
	"testing"
	"time"

	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAnchorAndWindowMonthClamp(t *testing.T) {
	reset := time.Date(2024, time.March, 31, 12, 0, 0, 0, time.UTC)
	start := WindowStart("monthly", reset)
	if start.Month() != time.February || start.Day() != 29 {
		t.Fatal(start)
	}
	a := &authpkg.Auth{Attributes: map[string]string{"base_url": "https://opencode.ai/zen/go/v1"}}
	got := Anchor(context.Background(), nil, a, Live{Windows: []Window{{Key: "monthly", Reset: reset}}})
	if !got.Equal(reset) {
		t.Fatal(got)
	}
	a.Attributes["base_url"] = "https://example.com"
	if !Anchor(context.Background(), nil, a, Live{}).IsZero() {
		t.Fatal("unexpected anchor")
	}
}

func TestWindowDurationAndMonthlyProviders(t *testing.T) {
	cases := map[string]time.Duration{"rolling": 5 * time.Hour, "weekly": 7 * 24 * time.Hour, "604800s": 7 * 24 * time.Hour, "monthly": 30 * 24 * time.Hour, "bogus": 0}
	for key, want := range cases {
		if got := (Window{Key: key}).Duration(); got != want {
			t.Fatalf("%s: %v", key, got)
		}
	}
	for base, want := range map[string]bool{"https://opencode.ai/zen/go/v1": true, "https://api.commandcode.ai/provider/v1": true, "https://amr-link.open-design.ai/v1": false} {
		if got := HasMonthlyLimit(&authpkg.Auth{Attributes: map[string]string{"base_url": base}}); got != want {
			t.Fatalf("%s: %v", base, got)
		}
	}
}
