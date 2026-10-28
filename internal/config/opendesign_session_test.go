package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenDesignSessionRoundTripsV8 checks that the OpenDesign session fields load from and
// save back into a v8 key entry, using the flow-style layout the management API writes.
func TestOpenDesignSessionRoundTripsV8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := `config-version: 8
api-keys:
    openai-compatibility: [{"base-url": "https://amr-link.open-design.ai/v1", "keys": [{"api-key": "sk-amr-test"}], "models": [{"name": "deepseek-v4.1-flash", "alias": "deepseek-flash"}], "name": "Opendesign Go 1"}]
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if len(cfg.OpenAICompatibility) != 1 || len(cfg.OpenAICompatibility[0].APIKeyEntries) != 1 {
		t.Fatalf("unexpected openai-compatibility entries: %+v", cfg.OpenAICompatibility)
	}
	entry := &cfg.OpenAICompatibility[0].APIKeyEntries[0]
	entry.OpenDesignSession = "abc.def%3D"
	entry.OpenDesignWorkspaceID = "ws-1"

	for _, migrate := range []bool{true, false} {
		if err = SaveConfigPreserveComments(path, cfg, migrate); err != nil {
			t.Fatalf("SaveConfigPreserveComments(migrate=%v) error = %v", migrate, err)
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		if strings.Contains(string(data), "api-key-entries") {
			t.Fatalf("saved config switched to the legacy layout:\n%s", data)
		}
		reloaded, errLoad := LoadConfig(path)
		if errLoad != nil {
			t.Fatalf("LoadConfig() after save error = %v", errLoad)
		}
		got := reloaded.OpenAICompatibility[0].APIKeyEntries[0]
		if got.APIKey != "sk-amr-test" || got.OpenDesignSession != "abc.def%3D" || got.OpenDesignWorkspaceID != "ws-1" {
			t.Fatalf("migrate=%v: reloaded entry = %+v\n%s", migrate, got, data)
		}
	}

	entry.OpenDesignSession, entry.OpenDesignWorkspaceID = "", ""
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "opendesign-") {
		t.Fatalf("cleared fields remain in saved config:\n%s", data)
	}
}
