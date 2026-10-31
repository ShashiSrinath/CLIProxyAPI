package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSetPriorityOverridesReordersScheduler(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	t.Cleanup(func() { manager.SetPriorityOverrides(nil) })
	for _, id := range []string{"auth-a", "auth-b"} {
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: "gemini", Attributes: map[string]string{"priority": "5"}}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}
	pickAll := func(want string) {
		t.Helper()
		for i := 0; i < 4; i++ {
			got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickNext() error = %v", errPick)
			}
			if got == nil || got.ID != want {
				t.Fatalf("pick %d = %v, want %s", i, got, want)
			}
		}
	}

	manager.SetPriorityOverrides(map[string]int{"auth-b": 1002, "auth-a": 1001})
	pickAll("auth-b")
	manager.SetPriorityOverrides(map[string]int{"auth-a": 1002, "auth-b": 1001})
	pickAll("auth-a")
	// Overrides beat configured priorities, including negative pause bands.
	manager.SetPriorityOverrides(map[string]int{"auth-a": -999})
	pickAll("auth-b")
	if got := PriorityOverrides(); len(got) != 1 || got["auth-a"] != -999 {
		t.Fatalf("PriorityOverrides() = %v", got)
	}
	manager.SetPriorityOverrides(nil)
	if got := authPriority(&Auth{ID: "auth-a", Attributes: map[string]string{"priority": "5"}}); got != 5 {
		t.Fatalf("authPriority after reset = %d, want 5", got)
	}
}
