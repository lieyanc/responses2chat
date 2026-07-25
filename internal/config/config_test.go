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

func TestLoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error for invalid JSON")
	}
}
