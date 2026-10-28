package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

func TestOpenDesignFetchAndCooldown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != OpenDesignCookie+"=abc%3D" {
			t.Errorf("incorrect cookie")
		}
		switch r.URL.Path {
		case "/api/auth/get-session":
			w.Write([]byte(`{"session":{"expiresAt":"2026-01-02T00:00:00Z"},"user":{"email":"test@example.org"}}`))
		case "/api/v1/workspaces/current":
			w.Write([]byte(`{"workspaceId":"ws","workspaceName":"Work"}`))
		case "/api/v1/workspaces/ws/billing/coding-plan-usage":
			if r.Header.Get("x-vela-workspace-id") != "ws" {
				t.Error("missing workspace header")
			}
			w.Write([]byte(`{"windows":[{"remainingCredits":"0","usedCredits":"10","limitCredits":"10","resetsAt":"2026-01-01T09:00:00Z"}]}`))
		case "/api/v1/workspaces/ws/wallet/balance":
			w.Write([]byte(`{"balanceUsd":"0.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	original := util.OpenDesignAuthBaseURL
	util.OpenDesignAuthBaseURL = server.URL
	defer func() { util.OpenDesignAuthBaseURL = original }()
	session, err := FetchOpenDesignSession(context.Background(), server.Client(), "abc%3D")
	if err != nil || session.User.Email != "test@example.org" {
		t.Fatalf("session: %v", err)
	}
	workspace, err := FetchOpenDesignCurrentWorkspace(context.Background(), server.Client(), "abc%3D")
	if err != nil || workspace.ID != "ws" {
		t.Fatalf("workspace: %v", err)
	}
	usage, err := FetchOpenDesignUsage(context.Background(), server.Client(), "abc%3D", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := OpenDesignQuotaCooldown(usage, now); !ok || d != 6*time.Hour {
		t.Fatalf("cooldown: %v %v", d, ok)
	}
	usage.Wallet.BalanceUSD = 1
	if _, ok := OpenDesignQuotaCooldown(usage, now); ok {
		t.Fatal("positive wallet blocked")
	}
	usage.Wallet.BalanceUSD = 0
	usage.Windows[0].RemainingCredits = 1
	if _, ok := OpenDesignQuotaCooldown(usage, now); ok {
		t.Fatal("available window blocked")
	}
}
