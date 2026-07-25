// Package config loads the single config.json file that carries every
// runtime setting. Environment variables are deliberately not consulted:
// the config file is the only source of configuration.
package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Template is the pristine config.json bundled into the binary; it is
// extracted on first startup so operators edit a real file instead of
// copying one from documentation.
//
//go:embed config.template.json
var Template []byte

// DefaultPath is where config.json is looked up when -config is not given,
// relative to the working directory.
const DefaultPath = "config.json"

// Update carries the settings consumed by `responses2chat update`. Empty
// fields fall back to the updater's built-in defaults.
type Update struct {
	Channel      string `json:"channel"`
	Source       string `json:"source"`
	ProxyBaseURL string `json:"proxy_base_url"`
	Repo         string `json:"repo"`
}

type Config struct {
	ListenAddr                 string `json:"listen_addr"`
	UpstreamBaseURL            string `json:"upstream_base_url"`
	UpstreamChatCompletionsURL string `json:"upstream_chat_completions_url"`
	// UpstreamProxyURL routes upstream requests through the given proxy
	// (http, https, socks5 or socks5h). Empty falls back to the standard
	// proxy environment variables (HTTP_PROXY/HTTPS_PROXY/NO_PROXY).
	UpstreamProxyURL     string `json:"upstream_proxy_url"`
	ReasoningPassthrough bool   `json:"reasoning_passthrough"`
	Update               Update `json:"update"`
}

// Load parses the file at path and applies defaults. Serving traffic
// additionally requires ValidateServer; `update` runs without an upstream.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	cfg.UpstreamBaseURL = strings.TrimSpace(cfg.UpstreamBaseURL)
	cfg.UpstreamChatCompletionsURL = strings.TrimSpace(cfg.UpstreamChatCompletionsURL)
	cfg.UpstreamProxyURL = strings.TrimSpace(cfg.UpstreamProxyURL)
	return cfg, nil
}

// ProxyURL parses upstream_proxy_url, returning nil when it is unset.
func (c Config) ProxyURL() (*url.URL, error) {
	if c.UpstreamProxyURL == "" {
		return nil, nil
	}
	parsed, err := url.Parse(c.UpstreamProxyURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid upstream_proxy_url %q", c.UpstreamProxyURL)
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
		return parsed, nil
	default:
		return nil, fmt.Errorf("upstream_proxy_url %q: scheme must be http, https, socks5 or socks5h", c.UpstreamProxyURL)
	}
}

// ValidateServer checks the fields required to run the gateway.
func (c Config) ValidateServer() error {
	if c.UpstreamBaseURL == "" && c.UpstreamChatCompletionsURL == "" {
		return fmt.Errorf("upstream_base_url or upstream_chat_completions_url is required")
	}
	if _, err := c.ProxyURL(); err != nil {
		return err
	}
	return nil
}

// WriteTemplate writes the embedded template to path, refusing to touch an
// existing file.
func WriteTemplate(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(Template); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}
