package config

import "testing"

func TestV8AccessAPIKeyProvidersLoads(t *testing.T) {
	data := []byte(`config-version: 8
access:
  api-keys:
    - "client-a"
    - "client.b"
  api-key-providers:
    "client.b": ["codex", "claude"]
`)
	if err := ValidateV8Config(data); err != nil {
		t.Fatalf("ValidateV8Config: %v", err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	got := cfg.APIKeyProviders["client.b"]
	if len(got) != 2 || got[0] != "codex" || got[1] != "claude" {
		t.Fatalf("APIKeyProviders[client.b] = %v", got)
	}
	if _, ok := cfg.APIKeyProviders["client-a"]; ok {
		t.Fatal("unrestricted key must not get an entry")
	}
}

func TestLegacyAPIKeyProvidersMigratesToAccess(t *testing.T) {
	data := []byte(`api-keys:
  - "client-a"
api-key-providers:
  "client-a": ["gemini"]
`)
	normalized, _, err := NormalizeConfigLayout(data, true)
	if err != nil {
		t.Fatalf("NormalizeConfigLayout: %v", err)
	}
	if err = ValidateV8Config(normalized); err != nil {
		t.Fatalf("ValidateV8Config: %v\n%s", err, normalized)
	}
	cfg, err := ParseConfigBytes(normalized)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if got := cfg.APIKeyProviders["client-a"]; len(got) != 1 || got[0] != "gemini" {
		t.Fatalf("APIKeyProviders = %v\n%s", cfg.APIKeyProviders, normalized)
	}
}
