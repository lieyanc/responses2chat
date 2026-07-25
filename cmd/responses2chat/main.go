package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

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

	upstream := os.Getenv("UPSTREAM_CHAT_COMPLETIONS_URL")
	if upstream == "" {
		base := os.Getenv("UPSTREAM_BASE_URL")
		if base == "" {
			log.Fatal("UPSTREAM_CHAT_COMPLETIONS_URL or UPSTREAM_BASE_URL is required")
		}
		upstream = gateway.ChatCompletionsURL(base)
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	handler, err := gateway.New(gateway.Config{
		UpstreamURL: upstream,
		MaxBodySize: 32 << 20,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
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
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("responses2chat listening on %s, upstream=%s", addr, upstream)
	log.Fatal(server.ListenAndServe())
}

// runUpdate implements `responses2chat update`: a synchronous self-update
// that checks the signed GitHub release, downloads, verifies, and replaces
// the current binary in place.
func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	channel := fs.String("channel", envOr("UPDATE_CHANNEL", "stable"), "release channel: stable or dev")
	source := fs.String("source", envOr("UPDATE_SOURCE", "github"), "download source: github or proxy")
	proxyURL := fs.String("proxy-url", os.Getenv("UPDATE_PROXY_URL"), "proxy mirror base URL (source=proxy)")
	repo := fs.String("repo", os.Getenv("UPDATE_REPO"), "GitHub repo owner/name")
	checkOnly := fs.Bool("check", false, "only check for a new version, do not install")
	_ = fs.Parse(args)

	dataDir, err := os.UserCacheDir()
	if err != nil {
		dataDir = os.TempDir()
	}
	dataDir = filepath.Join(dataDir, "responses2chat")

	cfg := updater.Config{
		Enabled:      true,
		Channel:      *channel,
		Source:       *source,
		ProxyBaseURL: *proxyURL,
		Repo:         *repo,
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
