package management

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOpenDesignSession(t *testing.T) {
	for _, input := range []string{"abc%3D", "__Secure-better-auth.session_token=abc%3D", "Cookie: a=foo; __Secure-better-auth.session_token=abc%3D; b=bar", `curl https://example.org -H 'cookie: x=y; __Secure-better-auth.session_token=abc%3D'`, `curl https://example.org -H "Cookie: __Secure-better-auth.session_token=abc%3D"`, `curl https://example.org -b 'x=y; __Secure-better-auth.session_token=abc%3D'`, `curl ^ --cookie "__Secure-better-auth.session_token=abc%3D"`} {
		if got := parseOpenDesignSession(input); got != "abc%3D" {
			t.Errorf("%q: got %q", input, got)
		}
	}
	for _, input := range []string{"", "Cookie: foo=bar", `curl -H 'cookie: x=y'`} {
		if got := parseOpenDesignSession(input); got != "" {
			t.Errorf("%q: got %q", input, got)
		}
	}
}

func TestOpenDesignManagement(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	const token = "test-session-secret"
	sessionResponse := `{"session":{"expiresAt":"2030-01-01T00:00:00Z"},"user":{"email":"user@example.com"}}`
	unauthorized := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "__Secure-better-auth.session_token="+token {
			t.Errorf("unexpected cookie: %q", r.Header.Get("Cookie"))
		}
		if unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/auth/get-session":
			fmt.Fprint(w, sessionResponse)
		case "/api/v1/workspaces/current":
			fmt.Fprint(w, `{"workspaceId":"ws","workspaceName":"Workspace"}`)
		case "/api/v1/workspaces/ws/billing/coding-plan-usage":
			fmt.Fprint(w, `{"workspaceId":"ws","tier":"go","eligible":true,"generatedAt":"2026-01-01T00:00:00Z","windows":[{"durationSeconds":18000,"usedCredits":"3","limitCredits":"10","remainingCredits":"7","windowStart":"2026-01-01T00:00:00Z","resetsAt":"2026-01-02T00:00:00Z"},{"durationSeconds":604800,"usedCredits":"4","limitCredits":"20","remainingCredits":"16","windowStart":"2026-01-01T00:00:00Z","resetsAt":"2026-01-08T00:00:00Z"}]}`)
		case "/api/v1/workspaces/ws/wallet/balance":
			fmt.Fprint(w, `{"balanceUsd":"1.25"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	old := util.OpenDesignAuthBaseURL
	util.OpenDesignAuthBaseURL = server.URL
	t.Cleanup(func() { util.OpenDesignAuthBaseURL = old })
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `config-version: 8
api-keys:
    openai-compatibility: [{"base-url": "https://amr-link.open-design.ai/v1", "keys": [{"api-key": "sk-test"}], "name": "design"}]
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "design:1", Provider: "openai-compatible-design", Status: coreauth.StatusActive, Attributes: map[string]string{"source": "config:design[0]", "base_url": "https://amr-link.open-design.ai/v1", "compat_name": "design", "api_key": "sk-test"}}
	if _, err = manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	other := &coreauth.Auth{ID: "other:1", Provider: "other", Attributes: map[string]string{"base_url": "https://example.org"}}
	if _, err = manager.Register(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, manager)
	h.tokenStore = &memoryAuthStore{}
	call := func(method string, handler gin.HandlerFunc, index, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(method, "/?auth_index="+index, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(ConfigV8ContextKey, true)
		handler(c)
		return rec
	}
	check := func(rec *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if rec.Code != status || (code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`)) {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	connect := func(index, value string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"auth_index": index, "session": value})
		return call(http.MethodPost, h.OpenDesignConnect, "", string(b))
	}
	check(call(http.MethodGet, h.OpenDesignUsage, auth.Index, ""), 409, "opendesign_not_connected")
	rec := call(http.MethodGet, h.OpenDesignStatus, auth.Index, "")
	check(rec, 200, "")
	if !strings.Contains(rec.Body.String(), `"connected":false`) {
		t.Fatal(rec.Body.String())
	}
	check(connect("unknown", token), 404, "auth_not_found")
	check(connect(other.Index, token), 400, "not_opendesign_credential")
	sessionResponse = "null"
	check(connect(auth.Index, token), 422, "opendesign_session_invalid")
	sessionResponse = `{"session":{"expiresAt":"2030-01-01T00:00:00Z"},"user":{"email":"user@example.com"}}`
	rec = connect(auth.Index, `curl 'https://example.org' -H 'Cookie: __Secure-better-auth.session_token=`+token+`'`)
	check(rec, 200, "")
	var body map[string]any
	if err = json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]any{"connected": true, "valid": true, "email": "user@example.com", "workspace_id": "ws", "workspace_name": "Workspace"} {
		if body[k] != v {
			t.Errorf("%s = %v", k, body[k])
		}
	}
	if body["expires_at"] == nil || body["checked_at"] == nil {
		t.Fatal(body)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "opendesign-session") || !strings.Contains(string(saved), "opendesign-workspace-id") {
		t.Fatal(string(saved))
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.OpenAICompatibility[0].APIKeyEntries[0]
	if entry.OpenDesignSession != token || entry.OpenDesignWorkspaceID != "ws" {
		t.Fatal("wrong saved fields")
	}
	rec = call(http.MethodGet, h.OpenDesignUsage, auth.Index, "")
	check(rec, 200, "")
	if err = json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	windows := body["windows"].([]any)
	if windows[0].(map[string]any)["label"] != "5h" || windows[1].(map[string]any)["label"] != "weekly" || windows[0].(map[string]any)["used_credits"] != float64(3) || body["wallet"].(map[string]any)["balance_usd"] != 1.25 {
		t.Fatal(body)
	}
	unauthorized = true
	check(call(http.MethodGet, h.OpenDesignUsage, auth.Index, ""), 412, "opendesign_session_expired")
	unauthorized = false
	// A synthesized auth contains the session attribute; the listing must never expose it.
	auth.Attributes["opendesign_session"] = token
	if _, err = manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	rec = call(http.MethodGet, h.ListAuthFiles, "", "")
	check(rec, 200, "")
	if strings.Contains(rec.Body.String(), token) || !strings.Contains(rec.Body.String(), `"opendesign":{"connected":true}`) {
		t.Fatal(rec.Body.String())
	}
	check(call(http.MethodDelete, h.OpenDesignDisconnect, auth.Index, ""), 200, "")
	saved, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "opendesign-") {
		t.Fatal(string(saved))
	}
}
