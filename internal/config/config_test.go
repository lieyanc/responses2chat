package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTemplateIsValidAndExtractable(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal(Template, &cfg); err != nil {
		t.Fatalf("embedded template is not valid JSON: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := WriteTemplate(path); err != nil {
		t.Fatalf("WriteTemplate: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load extracted template: %v", err)
	}
	if loaded.ListenAddr != ":8080" {
		t.Fatalf("listen_addr = %q, want :8080", loaded.ListenAddr)
	}
	if loaded.ReasoningPassthrough {
		t.Fatal("template must default reasoning_passthrough to false")
	}
	if err := loaded.ValidateServer(); err == nil {
		t.Fatal("template must not pass server validation before an upstream is set")
	}
}

func TestWriteTemplateDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"listen_addr":":9"}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteTemplate(path); err == nil {
		t.Fatal("expected error when config already exists")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("existing config was modified: %s", data)
	}
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"upstream_base_url": " https://up.example/v1 "}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("listen_addr default = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.UpstreamBaseURL != "https://up.example/v1" {
		t.Fatalf("upstream_base_url = %q, want trimmed URL", cfg.UpstreamBaseURL)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("want not-exist error, got %v", err)
	}
}

func TestProxyURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty", "", false},
		{"http", "http://127.0.0.1:7890", false},
		{"https", "https://proxy.example:443", false},
		{"socks5", "socks5://user:pass@127.0.0.1:1080", false},
		{"socks5h", "socks5h://127.0.0.1:1080", false},
		{"unsupported scheme", "ftp://127.0.0.1:21", true},
		{"missing host", "http://", true},
		{"not a URL", "://bad", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{UpstreamBaseURL: "https://up.example/v1", UpstreamProxyURL: tc.raw}
			parsed, err := cfg.ProxyURL()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ProxyURL(%q): expected error", tc.raw)
				}
				if cfg.ValidateServer() == nil {
					t.Fatalf("ValidateServer must reject upstream_proxy_url %q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProxyURL(%q): %v", tc.raw, err)
			}
			if (tc.raw == "") != (parsed == nil) {
				t.Fatalf("ProxyURL(%q) = %v, nil only for empty input", tc.raw, parsed)
			}
			if err := cfg.ValidateServer(); err != nil {
				t.Fatalf("ValidateServer: %v", err)
			}
		})
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error for invalid JSON")
	}
}

func TestMigrateAddsMissingSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
  "listen_addr": ":9090",
  "upstream_base_url": "https://up.example/v1",
  "custom_note": "keep me",
  "update": {
    "channel": "dev"
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := Migrate(path)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	want := map[string]bool{
		"upstream_chat_completions_url": true,
		"upstream_proxy_url":            true,
		"reasoning_passthrough":         true,
		"update.source":                 true,
		"update.proxy_base_url":         true,
		"update.repo":                   true,
	}
	if len(added) != len(want) {
		t.Fatalf("added = %v, want keys %v", added, want)
	}
	for _, path := range added {
		if !want[path] {
			t.Fatalf("unexpected added path %q", path)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("migrated file is not valid JSON: %v", err)
	}
	if raw["custom_note"] != "keep me" {
		t.Fatal("unknown key was not preserved")
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9090" {
		t.Fatalf("listen_addr = %q, existing value was not preserved", cfg.ListenAddr)
	}
	if cfg.UpstreamBaseURL != "https://up.example/v1" {
		t.Fatalf("upstream_base_url = %q, existing value was not preserved", cfg.UpstreamBaseURL)
	}
	if cfg.Update.Channel != "dev" {
		t.Fatalf("update.channel = %q, nested existing value was not preserved", cfg.Update.Channel)
	}
	if cfg.Update.Source != "github" {
		t.Fatalf("update.source = %q, want template default github", cfg.Update.Source)
	}
}

func TestMigrateLeavesCompleteConfigUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := WriteTemplate(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	added, err := Migrate(path)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v, want none for a complete config", added)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("complete config file was rewritten")
	}
}

func TestMigrateMissingFile(t *testing.T) {
	_, err := Migrate(filepath.Join(t.TempDir(), "missing.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("want not-exist error, got %v", err)
	}
}

func TestMigrateInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(path); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
