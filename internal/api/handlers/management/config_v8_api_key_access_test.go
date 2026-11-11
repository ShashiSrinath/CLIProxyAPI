package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8AccessAPIKeyModelsAndLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\naccess:\n  api-keys: [client]\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	router.GET("/v8/management/config/*path", h.ConfigV8)

	request := func(method, url, body string, status int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, url, strings.NewReader(body)))
		if recorder.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", method, url, recorder.Code, recorder.Body.String())
		}
	}

	request(http.MethodPut, "/v8/management/config/access/api-key-models", `{"client": ["deepseek-*", "kimi-k2*"]}`, http.StatusOK)
	request(http.MethodPut, "/v8/management/config/access/api-key-limits", `{"client": {"daily-usd": 5, "timezone": "Asia/Kolkata"}}`, http.StatusOK)

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "api-key-models:") || !strings.Contains(string(saved), "api-key-limits:") {
		t.Fatalf("saved config is missing the new sections:\n%s", saved)
	}
	parsed, err := config.ParseConfigBytes(saved)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if got := parsed.APIKeyModels["client"]; len(got) != 2 || got[0] != "deepseek-*" {
		t.Fatalf("APIKeyModels = %v", parsed.APIKeyModels)
	}
	if limit := parsed.APIKeyLimits["client"]; limit.DailyUSD != 5 || limit.Timezone != "Asia/Kolkata" {
		t.Fatalf("APIKeyLimits = %+v", limit)
	}

	// A negative amount is rejected and the stored file keeps the previous value.
	request(http.MethodPut, "/v8/management/config/access/api-key-limits", `{"client": {"daily-usd": -1}}`, http.StatusUnprocessableEntity)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(saved) {
		t.Fatalf("failed write changed the file:\n%s", after)
	}

	// An invalid timezone is rejected as well.
	request(http.MethodPut, "/v8/management/config/access/api-key-limits", `{"client": {"daily-usd": 1, "timezone": "Nowhere/City"}}`, http.StatusUnprocessableEntity)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v8/management/config/access/api-key-models", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "deepseek-*") {
		t.Fatalf("GET api-key-models: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
