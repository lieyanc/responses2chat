package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lieyan/responses2chat/internal/version"
)

func TestCheckOnlySelectsNewestStableRelease(t *testing.T) {
	originalVersion := version.Version
	defer func() { version.Version = originalVersion }()
	version.Version = "v1.0.0"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/releases/owner/repo/latest" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(releaseInfo{
			TagName: "v1.4.0",
			Assets:  []assetInfo{},
		})
	}))
	defer server.Close()

	u := testUpdater(Config{
		Channel:      "stable",
		Source:       "proxy",
		ProxyBaseURL: server.URL,
		Repo:         "owner/repo",
	})

	result, err := u.CheckOnly(context.Background())
	if err != nil {
		t.Fatalf("CheckOnly returned error: %v", err)
	}
	if !result.HasUpdate {
		t.Fatalf("expected update to be available")
	}
	if result.LatestVersion != "v1.4.0" {
		t.Fatalf("expected latest version v1.4.0, got %q", result.LatestVersion)
	}
}

func TestCheckOnlySelectsNewestPrerelease(t *testing.T) {
	originalVersion := version.Version
	originalCommit := version.Commit
	defer func() { version.Version = originalVersion }()
	defer func() { version.Commit = originalCommit }()
	version.Version = "dev-0007-20260401-aaaaaaa"
	version.Commit = "aaaaaaa"
	remoteVersion := "dev-0042-20260425-bbbbbbb"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/owner/repo/dev":
			_ = json.NewEncoder(w).Encode(releaseInfo{
				TagName:         "dev",
				TargetCommitish: "bbbbbbb",
				Prerelease:      true,
				Assets: []assetInfo{
					{
						Name:               "version.json",
						BrowserDownloadURL: "https://github.com/owner/repo/releases/download/dev/version.json",
					},
				},
			})
		case "/download/owner/repo/dev/version.json":
			_ = json.NewEncoder(w).Encode(releaseVersionInfo{
				Version:   remoteVersion,
				Commit:    "bbbbbbb",
				BuildTime: "2026-04-25T00:00:00Z",
				Tag:       "dev",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	u := testUpdater(Config{
		Channel:      "dev",
		Source:       "proxy",
		ProxyBaseURL: server.URL,
		Repo:         "owner/repo",
	})

	result, err := u.CheckOnly(context.Background())
	if err != nil {
		t.Fatalf("CheckOnly returned error: %v", err)
	}
	if !result.HasUpdate {
		t.Fatalf("expected prerelease update to be available")
	}
	if result.LatestVersion != remoteVersion {
		t.Fatalf("expected latest prerelease, got %q", result.LatestVersion)
	}
}

func TestCheckOnlySkipsDevReleaseForSameCommit(t *testing.T) {
	originalVersion := version.Version
	originalCommit := version.Commit
	defer func() { version.Version = originalVersion }()
	defer func() { version.Commit = originalCommit }()
	version.Version = "dev-0042-20260425-bbbbbbb"
	version.Commit = "bbbbbbb"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/owner/repo/dev":
			_ = json.NewEncoder(w).Encode(releaseInfo{
				TagName:         "dev",
				TargetCommitish: "bbbbbbb",
				Prerelease:      true,
				Assets: []assetInfo{
					{
						Name:               "version.json",
						BrowserDownloadURL: "https://github.com/owner/repo/releases/download/dev/version.json",
					},
				},
			})
		case "/download/owner/repo/dev/version.json":
			_ = json.NewEncoder(w).Encode(releaseVersionInfo{
				Version:   "dev-0042-20260425-bbbbbbb",
				Commit:    "bbbbbbb",
				BuildTime: "2026-04-25T00:00:00Z",
				Tag:       "dev",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	u := testUpdater(Config{
		Channel:      "dev",
		Source:       "proxy",
		ProxyBaseURL: server.URL,
		Repo:         "owner/repo",
	})

	result, err := u.CheckOnly(context.Background())
	if err != nil {
		t.Fatalf("CheckOnly returned error: %v", err)
	}
	if result.HasUpdate {
		t.Fatalf("did not expect update for same commit: %#v", result)
	}
	if result.LatestVersion != "dev-0042-20260425-bbbbbbb" {
		t.Fatalf("expected latest version from version metadata, got %q", result.LatestVersion)
	}
}

func TestPerformUpdateDownloadsAndVerifiesPrerelease(t *testing.T) {
	originalVersion := version.Version
	originalCommit := version.Commit
	defer func() { version.Version = originalVersion }()
	defer func() { version.Commit = originalCommit }()
	version.Version = "dev-0007-20260401-aaaaaaa"
	version.Commit = "aaaaaaa"

	cfg := Config{
		Channel: "dev",
		Source:  "proxy",
		Repo:    "owner/repo",
	}
	dataDir := t.TempDir()
	u := New(
		func() Config { return cfg },
		func() string { return dataDir },
		log.New(io.Discard, "", 0),
		RestartHooks{},
	)

	tag := "dev"
	remoteVersion := "dev-0042-20260425-bbbbbbb"
	targetName := u.targetName()
	binary := []byte("new binary")
	sum := fmt.Sprintf("%x", sha256.Sum256(binary))
	shaContent := sum + "  " + targetName + "\n"
	sign := setTestSigningKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/owner/repo/dev":
			_ = json.NewEncoder(w).Encode(releaseInfo{
				TagName:         tag,
				TargetCommitish: "bbbbbbb",
				Prerelease:      true,
				Assets: []assetInfo{
					{
						Name:               targetName,
						BrowserDownloadURL: "https://github.com/owner/repo/releases/download/" + tag + "/" + targetName,
						Size:               int64(len(binary)),
					},
					{
						Name:               targetName + ".sha256",
						BrowserDownloadURL: "https://github.com/owner/repo/releases/download/" + tag + "/" + targetName + ".sha256",
					},
					{
						Name:               targetName + ".sha256.sig",
						BrowserDownloadURL: "https://github.com/owner/repo/releases/download/" + tag + "/" + targetName + ".sha256.sig",
					},
					{
						Name:               "version.json",
						BrowserDownloadURL: "https://github.com/owner/repo/releases/download/" + tag + "/version.json",
					},
				},
			})
		case "/download/owner/repo/" + tag + "/" + targetName:
			_, _ = w.Write(binary)
		case "/download/owner/repo/" + tag + "/" + targetName + ".sha256":
			_, _ = w.Write([]byte(shaContent))
		case "/download/owner/repo/" + tag + "/" + targetName + ".sha256.sig":
			_, _ = w.Write([]byte(sign([]byte(shaContent))))
		case "/download/owner/repo/" + tag + "/version.json":
			_ = json.NewEncoder(w).Encode(releaseVersionInfo{
				Version:   remoteVersion,
				Commit:    "bbbbbbb",
				BuildTime: "2026-04-25T00:00:00Z",
				Tag:       tag,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg.ProxyBaseURL = server.URL

	u.performUpdate(context.Background())

	status := u.Status()
	if status.State != "ready" {
		t.Fatalf("expected update to be ready, got %q: %s", status.State, status.Error)
	}
	if status.LatestVersion != remoteVersion || u.pendingTag != tag {
		t.Fatalf("expected pending latest tag %q, got status=%q pending=%q", tag, status.LatestVersion, u.pendingTag)
	}
	if status.Progress != progressVerifyDone {
		t.Fatalf("expected overall progress %d, got %.0f", progressVerifyDone, status.Progress)
	}
	got, err := os.ReadFile(u.pendingBinaryPath)
	if err != nil {
		t.Fatalf("read pending binary: %v", err)
	}
	if string(got) != string(binary) {
		t.Fatalf("pending binary content mismatch")
	}
}

func TestApplyPendingMovesToApplyingBeforeAsyncRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	u := testUpdater(Config{})
	u.bgCtx = ctx
	u.hooks.BeforeExec = func(tag string) error {
		return context.Canceled
	}
	u.status.State = "ready"
	u.pendingBinaryPath = t.TempDir() + "/responses2chat-new"
	u.pendingTag = "v1.2.0"

	if err := u.ApplyPending(context.Background()); err != nil {
		t.Fatalf("ApplyPending returned error: %v", err)
	}

	status := u.Status()
	if status.State != "applying" {
		t.Fatalf("expected state applying immediately, got %q", status.State)
	}
	if status.Progress != progressApplying {
		t.Fatalf("expected applying progress %d, got %.0f", progressApplying, status.Progress)
	}
	if u.pendingBinaryPath != "" || u.pendingTag != "" {
		t.Fatalf("expected pending update to be consumed")
	}

	err := u.ApplyPending(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no pending update") {
		t.Fatalf("expected duplicate apply to be rejected, got %v", err)
	}

	time.Sleep(250 * time.Millisecond)
}

func TestPerformUpdateRejectsReleaseWithoutSHA256(t *testing.T) {
	originalVersion := version.Version
	defer func() { version.Version = originalVersion }()
	version.Version = "v1.0.0"

	cfg := Config{
		Channel: "stable",
		Source:  "proxy",
		Repo:    "owner/repo",
	}
	dataDir := t.TempDir()
	u := New(
		func() Config { return cfg },
		func() string { return dataDir },
		log.New(io.Discard, "", 0),
		RestartHooks{},
	)

	targetName := u.targetName()
	binary := []byte("new binary")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/owner/repo/latest":
			_ = json.NewEncoder(w).Encode(releaseInfo{
				TagName: "v1.4.0",
				Assets: []assetInfo{
					{
						Name:               targetName,
						BrowserDownloadURL: "https://github.com/owner/repo/releases/download/v1.4.0/" + targetName,
						Size:               int64(len(binary)),
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg.ProxyBaseURL = server.URL

	u.performUpdate(context.Background())

	status := u.Status()
	if status.State != "failed" {
		t.Fatalf("expected update to fail without sha256 asset, got state %q", status.State)
	}
	if !strings.Contains(status.Error, "sha256") {
		t.Fatalf("expected sha256 error, got %q", status.Error)
	}
}

func TestCheckOnlyGitHubDirectDevChannel(t *testing.T) {
	originalVersion := version.Version
	originalCommit := version.Commit
	defer func() { version.Version = originalVersion }()
	defer func() { version.Commit = originalCommit }()
	version.Version = "dev-0007-20260401-aaaaaaa"
	version.Commit = "aaaaaaa"
	remoteVersion := "dev-0042-20260425-bbbbbbb"

	sign := setTestSigningKey(t)
	metadata, err := json.Marshal(releaseVersionInfo{
		Version:   remoteVersion,
		Commit:    "bbbbbbb",
		BuildTime: "2026-04-25T00:00:00Z",
		Tag:       "dev",
	})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/owner/repo/releases/download/dev/version.json":
			_, _ = w.Write(metadata)
		case "/owner/repo/releases/download/dev/version.json.sig":
			_, _ = w.Write([]byte(sign(metadata)))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	setTestGitHubBaseURL(t, server.URL)

	u := testUpdater(Config{Channel: "dev", Repo: "owner/repo"})
	result, err := u.CheckOnly(context.Background())
	if err != nil {
		t.Fatalf("CheckOnly returned error: %v", err)
	}
	if !result.HasUpdate {
		t.Fatalf("expected update to be available")
	}
	if result.LatestVersion != remoteVersion {
		t.Fatalf("expected latest version %q, got %q", remoteVersion, result.LatestVersion)
	}
}

func TestCheckOnlyGitHubDirectStableChannel(t *testing.T) {
	originalVersion := version.Version
	defer func() { version.Version = originalVersion }()
	version.Version = "v1.0.0"

	sign := setTestSigningKey(t)
	metadata, err := json.Marshal(releaseVersionInfo{
		Version:   "v1.4.0",
		Commit:    "bbbbbbb",
		BuildTime: "2026-04-25T00:00:00Z",
		Tag:       "v1.4.0",
	})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/owner/repo/releases/latest/download/version.json":
			_, _ = w.Write(metadata)
		case "/owner/repo/releases/download/v1.4.0/version.json.sig":
			_, _ = w.Write([]byte(sign(metadata)))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	setTestGitHubBaseURL(t, server.URL)

	u := testUpdater(Config{Channel: "stable", Repo: "owner/repo"})
	result, err := u.CheckOnly(context.Background())
	if err != nil {
		t.Fatalf("CheckOnly returned error: %v", err)
	}
	if !result.HasUpdate {
		t.Fatalf("expected update to be available")
	}
	if result.LatestVersion != "v1.4.0" {
		t.Fatalf("expected latest version v1.4.0, got %q", result.LatestVersion)
	}
}

func TestCheckOnlyGitHubDirectRejectsBadSignature(t *testing.T) {
	originalVersion := version.Version
	defer func() { version.Version = originalVersion }()
	version.Version = "v1.0.0"

	setTestSigningKey(t)
	metadata, err := json.Marshal(releaseVersionInfo{Version: "v1.4.0", Tag: "v1.4.0"})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/owner/repo/releases/latest/download/version.json":
			_, _ = w.Write(metadata)
		case "/owner/repo/releases/download/v1.4.0/version.json.sig":
			_, _ = w.Write([]byte("bm90IGEgcmVhbCBzaWduYXR1cmU=")) // valid base64, wrong signature
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	setTestGitHubBaseURL(t, server.URL)

	u := testUpdater(Config{Channel: "stable", Repo: "owner/repo"})
	if _, err := u.CheckOnly(context.Background()); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature verification error, got %v", err)
	}
}

func TestWaitForIdleStopsWhenApplicationContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u := New(
		func() Config { return Config{} },
		func() string { return "" },
		log.New(io.Discard, "", 0),
		RestartHooks{IsBusy: func() bool { return true }},
	)

	if err := u.waitForIdle(ctx); err != context.Canceled {
		t.Fatalf("waitForIdle error = %v, want context.Canceled", err)
	}
}

func setTestSigningKey(t *testing.T) func(data []byte) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test signing key: %v", err)
	}
	original := signingPublicKeyHex
	signingPublicKeyHex = hex.EncodeToString(pub)
	t.Cleanup(func() { signingPublicKeyHex = original })
	return func(data []byte) string {
		return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
	}
}

func setTestGitHubBaseURL(t *testing.T, url string) {
	t.Helper()
	original := githubBaseURL
	githubBaseURL = url
	t.Cleanup(func() { githubBaseURL = original })
}

func testUpdater(cfg Config) *Updater {
	return New(
		func() Config { return cfg },
		func() string { return "" },
		log.New(io.Discard, "", 0),
		RestartHooks{},
	)
}
