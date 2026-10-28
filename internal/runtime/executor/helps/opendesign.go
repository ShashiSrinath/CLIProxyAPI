package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// OpenDesignCookie names the dashboard session cookie.
const OpenDesignCookie = "__Secure-better-auth.session_token"

// OpenDesignSession describes a dashboard session.
type OpenDesignSession struct {
	// Session holds session expiration data.
	Session struct {
		// ExpiresAt is the session expiry time.
		ExpiresAt time.Time `json:"expiresAt"`
	} `json:"session"`
	// User holds user details.
	User struct {
		// Email is the user email address.
		Email string `json:"email"`
	} `json:"user"`
	// RotatedToken is a replacement session token, if supplied.
	RotatedToken string `json:"-"`
}

// OpenDesignWorkspace describes a dashboard workspace.
type OpenDesignWorkspace struct {
	// ID is the workspace identifier.
	ID string `json:"workspaceId"`
	// Name is the workspace name.
	Name string `json:"workspaceName"`
}

// OpenDesignWindow describes a usage window.
type OpenDesignWindow struct {
	// DurationSeconds is the window duration in seconds.
	DurationSeconds int64 `json:"durationSeconds"`
	// UsedCredits is the number of credits used.
	UsedCredits float64 `json:"usedCredits,string"`
	// LimitCredits is the credit limit.
	LimitCredits float64 `json:"limitCredits,string"`
	// RemainingCredits is the remaining credit balance.
	RemainingCredits float64 `json:"remainingCredits,string"`
	// WindowStart is the start of the window.
	WindowStart time.Time `json:"windowStart"`
	// ResetsAt is the next reset time.
	ResetsAt time.Time `json:"resetsAt"`
}

// OpenDesignUsage describes usage and wallet balance.
type OpenDesignUsage struct {
	// WorkspaceID is the workspace identifier.
	WorkspaceID string `json:"workspaceId"`
	// Tier is the coding plan tier.
	Tier string `json:"tier"`
	// Eligible indicates coding plan eligibility.
	Eligible bool `json:"eligible"`
	// GeneratedAt is when the usage data was generated.
	GeneratedAt time.Time `json:"generatedAt"`
	// Windows contains the usage windows.
	Windows []OpenDesignWindow `json:"windows"`
	// Wallet holds the wallet balance.
	Wallet struct {
		// BalanceUSD is the wallet balance in US dollars.
		BalanceUSD float64 `json:"balanceUsd,string"`
	} `json:"-"`
}

// OpenDesignHTTPError identifies a rejected dashboard request without exposing its body.
type OpenDesignHTTPError struct {
	// Status is the upstream HTTP status.
	Status int
}

// Error describes the rejected dashboard request.
func (e *OpenDesignHTTPError) Error() string { return fmt.Sprintf("opendesign: HTTP %d", e.Status) }

func fetchOpenDesign(ctx context.Context, client *http.Client, path, token, workspace string, out any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, util.OpenDesignAuthURL(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", OpenDesignCookie+"="+token)
	req.Header.Set("Accept", "application/json")
	if workspace != "" {
		req.Header.Set("x-vela-workspace-id", workspace)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opendesign request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &OpenDesignHTTPError{Status: resp.StatusCode}
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out); err != nil {
		return nil, fmt.Errorf("opendesign invalid response")
	}
	return resp, nil
}

// FetchOpenDesignSession fetches the dashboard session.
func FetchOpenDesignSession(ctx context.Context, client *http.Client, token string) (*OpenDesignSession, error) {
	var result *OpenDesignSession
	resp, err := fetchOpenDesign(ctx, client, "/api/auth/get-session", token, "", &result)
	if err != nil || result == nil {
		return result, err
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == OpenDesignCookie && cookie.Value != "" && cookie.Value != token {
			result.RotatedToken = cookie.Value
		}
	}
	return result, nil
}

// FetchOpenDesignCurrentWorkspace fetches the current workspace.
func FetchOpenDesignCurrentWorkspace(ctx context.Context, client *http.Client, token string) (*OpenDesignWorkspace, error) {
	var result OpenDesignWorkspace
	_, err := fetchOpenDesign(ctx, client, "/api/v1/workspaces/current", token, "", &result)
	return &result, err
}

// FetchOpenDesignUsage fetches usage and wallet balance.
func FetchOpenDesignUsage(ctx context.Context, client *http.Client, token, workspace string) (*OpenDesignUsage, error) {
	var result OpenDesignUsage
	path := "/api/v1/workspaces/" + url.PathEscape(workspace)
	_, err := fetchOpenDesign(ctx, client, path+"/billing/coding-plan-usage", token, workspace, &result)
	if err != nil {
		return nil, err
	}
	_, err = fetchOpenDesign(ctx, client, path+"/wallet/balance", token, workspace, &result.Wallet)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// OpenDesignQuotaCooldown returns the cooldown for exhausted usage.
func OpenDesignQuotaCooldown(usage *OpenDesignUsage, now time.Time) (time.Duration, bool) {
	if usage == nil || usage.Wallet.BalanceUSD > 0 {
		return 0, false
	}
	var latest time.Time
	for _, w := range usage.Windows {
		if w.RemainingCredits <= 0 && w.ResetsAt.After(latest) {
			latest = w.ResetsAt
		}
	}
	if !latest.After(now) {
		return 0, false
	}
	return min(latest.Sub(now), 6*time.Hour), true
}
