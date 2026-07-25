package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lieyan/responses2chat/internal/config"
	"github.com/lieyan/responses2chat/internal/gateway"
	"github.com/lieyan/responses2chat/internal/updater"
	"github.com/lieyan/responses2chat/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-version", "--version":
			fmt.Printf("responses2chat %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
			return
		case "update":
			runUpdate(os.Args[2:])
			return
		}
	}

	configPath := flag.String("config", config.DefaultPath, "path to config.json")
	flag.Parse()

	added, err := config.Migrate(*configPath)
	switch {
	case os.IsNotExist(err):
		if writeErr := config.WriteTemplate(*configPath); writeErr != nil {
			log.Fatalf("config %s not found and writing the bundled template failed: %v", *configPath, writeErr)
		}
		log.Fatalf("first run: wrote config template to %s; set upstream_base_url (or upstream_chat_completions_url) and start again", *configPath)
	case err != nil:
		// Missing keys only matter cosmetically: Load falls back to the same
		// defaults, so a read-only config file must not block startup.
		log.Printf("warning: config %s: could not add missing settings: %v", *configPath, err)
	case len(added) > 0:
		log.Printf("config %s: added missing settings: %s", *configPath, strings.Join(added, ", "))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := cfg.ValidateServer(); err != nil {
		log.Fatalf("config %s: %v", *configPath, err)
	}

	upstream := cfg.UpstreamChatCompletionsURL
	if upstream == "" {
		upstream = gateway.ChatCompletionsURL(cfg.UpstreamBaseURL)
	}

	proxy := http.ProxyFromEnvironment
	proxyURL, err := cfg.ProxyURL()
	if err != nil {
		log.Fatalf("config %s: %v", *configPath, err)
	}
	if proxyURL != nil {
		proxy = http.ProxyURL(proxyURL)
	}

	handler, err := gateway.New(gateway.Config{
		UpstreamURL:          upstream,
		MaxBodySize:          32 << 20,
		ReasoningPassthrough: cfg.ReasoningPassthrough,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				Proxy:                 proxy,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 5 * time.Minute,
			},
		},
		Logger: log.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if proxyURL != nil {
		log.Printf("responses2chat listening on %s, upstream=%s, proxy=%s", cfg.ListenAddr, upstream, proxyURL.Redacted())
	} else {
		log.Printf("responses2chat listening on %s, upstream=%s", cfg.ListenAddr, upstream)
	}
	log.Fatal(server.ListenAndServe())
}

// runUpdate implements `responses2chat update`: a synchronous self-update
// that checks the signed GitHub release, downloads, verifies, and replaces
// the current binary in place. Defaults come from the update section of
// config.json when present; command-line flags override the file.
func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath, "path to config.json")
	channel := fs.String("channel", "", "release channel: stable or dev")
	source := fs.String("source", "", "download source: github or proxy")
	proxyURL := fs.String("proxy-url", "", "proxy mirror base URL (source=proxy)")
	repo := fs.String("repo", "", "GitHub repo owner/name")
	checkOnly := fs.Bool("check", false, "only check for a new version, do not install")
	_ = fs.Parse(args)

	// A missing config file is fine for update: built-in defaults apply.
	fileCfg, err := config.Load(*configPath)
	if err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}

	upd := fileCfg.Update
	if *channel != "" {
		upd.Channel = *channel
	}
	if *source != "" {
		upd.Source = *source
	}
	if *proxyURL != "" {
		upd.ProxyBaseURL = *proxyURL
	}
	if *repo != "" {
		upd.Repo = *repo
	}

	dataDir, err := os.UserCacheDir()
	if err != nil {
		dataDir = os.TempDir()
	}
	dataDir = filepath.Join(dataDir, "responses2chat")

	cfg := updater.Config{
		Enabled:      true,
		Channel:      upd.Channel,
		Source:       upd.Source,
		ProxyBaseURL: upd.ProxyBaseURL,
		Repo:         upd.Repo,
	}
	u := updater.New(
		func() updater.Config { return cfg },
		func() string { return dataDir },
		log.Default(),
		updater.RestartHooks{},
	)

	ctx := context.Background()
	if *checkOnly {
		result, err := u.CheckOnly(ctx)
		if err != nil {
			log.Fatalf("update check failed: %v", err)
		}
		if result.HasUpdate {
			fmt.Printf("update available: %s (current %s, channel %s)\n", result.LatestVersion, result.CurrentVersion, result.Channel)
		} else {
			fmt.Printf("up to date: %s (channel %s)\n", result.CurrentVersion, result.Channel)
		}
		return
	}
	if err := u.RunOnce(ctx); err != nil {
		log.Fatalf("update failed: %v", err)
	}
}
