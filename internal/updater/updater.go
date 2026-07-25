package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lieyan/responses2chat/internal/version"
)

type Config struct {
	Enabled       bool   `json:"enabled"`
	Channel       string `json:"channel"`
	CheckInterval int    `json:"check_interval"`
	Source        string `json:"source"`
	ProxyBaseURL  string `json:"proxy_base_url"`
	Repo          string `json:"repo"`
	AdminToken    string `json:"admin_token"`
}

type Status struct {
	State            string  `json:"state"`
	CurrentVersion   string  `json:"current_version"`
	LatestVersion    string  `json:"latest_version,omitempty"`
	IsPrerelease     bool    `json:"is_prerelease"`
	Progress         float64 `json:"progress,omitempty"`
	DownloadProgress float64 `json:"download_progress,omitempty"`
	Error            string  `json:"error,omitempty"`
	LastCheck        string  `json:"last_check,omitempty"`
	ReleaseNotes     string  `json:"release_notes,omitempty"`
}

type CheckResult struct {
	HasUpdate      bool   `json:"has_update"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	IsPrerelease   bool   `json:"is_prerelease"`
	ReleaseNotes   string `json:"release_notes,omitempty"`
	Channel        string `json:"channel"`
}

type RestartHooks struct {
	BeforeExec    func(tag string) error
	OnExecFailure func(error)
	// IsBusy reports whether the application is processing work that should
	// not be interrupted by a restart. When set, the updater waits for it to
	// return false (bounded by idleWaitTimeout) before applying an update.
	IsBusy func() bool
}

type Updater struct {
	cfg     func() Config
	dataDir func() string
	logger  *log.Logger
	hooks   RestartHooks

	mu     sync.RWMutex
	status Status

	bgCtx context.Context

	pendingBinaryPath string
	pendingTag        string
}

const (
	idleWaitTimeout  = 10 * time.Minute
	idlePollInterval = 5 * time.Second
	downloadTimeout  = 30 * time.Minute

	// SourceGitHub downloads release assets straight from
	// github.com/<repo>/releases/download/... links, which are CDN
	// redirects and not subject to GitHub REST API rate limits.
	SourceGitHub = "github"
	// SourceProxy routes release lookups and downloads through the
	// configured proxy_base_url mirror.
	SourceProxy = "proxy"

	progressChecking      = 5
	progressReleaseFound  = 10
	progressDownloadStart = 10
	progressDownloadDone  = 90
	progressVerifyStart   = 92
	progressVerifyDone    = 95
	progressApplying      = 98
	progressComplete      = 100
)

func New(cfg func() Config, dataDir func() string, logger *log.Logger, hooks RestartHooks) *Updater {
	if logger == nil {
		logger = log.Default()
	}
	return &Updater{
		cfg:     cfg,
		dataDir: dataDir,
		logger:  logger,
		hooks:   hooks,
		status: Status{
			State:          "idle",
			CurrentVersion: version.Version,
		},
	}
}

func (u *Updater) Status() Status {
	u.mu.RLock()
	defer u.mu.RUnlock()
	s := u.status
	s.CurrentVersion = version.Version
	return s
}

func (u *Updater) CheckOnly(ctx context.Context) (CheckResult, error) {
	cfg := normalizeConfig(u.cfg())
	result := CheckResult{
		CurrentVersion: version.Version,
		Channel:        cfg.Channel,
	}

	release, hasUpdate, err := u.checkForUpdate(ctx, cfg)
	if err != nil {
		return result, err
	}

	u.mu.Lock()
	u.status.LastCheck = time.Now().UTC().Format(time.RFC3339)
	u.mu.Unlock()

	if release == nil {
		return result, nil
	}

	result.HasUpdate = hasUpdate
	result.LatestVersion = release.displayVersion()
	result.IsPrerelease = release.Prerelease
	result.ReleaseNotes = release.Body

	u.mu.Lock()
	u.status.LatestVersion = release.displayVersion()
	u.status.IsPrerelease = release.Prerelease
	u.status.ReleaseNotes = release.Body
	u.mu.Unlock()

	return result, nil
}

func (u *Updater) StartUpdate(_ context.Context) {
	go u.performUpdate(u.bgContext())
}

func (u *Updater) ApplyPending(_ context.Context) error {
	u.mu.Lock()
	state := u.status.State
	path := u.pendingBinaryPath
	tag := u.pendingTag

	if state != "ready" || path == "" {
		u.mu.Unlock()
		return fmt.Errorf("no pending update to apply")
	}

	u.status.State = "applying"
	u.status.Progress = progressApplying
	u.status.DownloadProgress = 0
	u.pendingBinaryPath = ""
	u.pendingTag = ""
	u.mu.Unlock()

	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := u.waitForIdle(u.bgContext()); err != nil {
			u.setError("apply canceled while waiting for idle: " + err.Error())
			return
		}
		if err := u.applyUpdate(path, tag); err != nil {
			u.notifyExecFailure(err)
			u.setError("apply failed: " + err.Error())
		}
	}()
	return nil
}

func (u *Updater) DismissPending() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.status.State == "ready" {
		if u.pendingBinaryPath != "" {
			_ = os.Remove(u.pendingBinaryPath)
		}
		u.pendingBinaryPath = ""
		u.pendingTag = ""
		u.status.State = "idle"
		u.status.LatestVersion = ""
		u.status.Progress = 0
		u.status.DownloadProgress = 0
		u.status.Error = ""
	}
}

func (u *Updater) StartBackground(ctx context.Context) {
	cfg := normalizeConfig(u.cfg())
	if !cfg.Enabled {
		u.logger.Printf("update: disabled")
		return
	}
	u.bgCtx = ctx
	u.logger.Printf("update: enabled, channel=%s, interval=%ds", cfg.Channel, cfg.CheckInterval)
	go u.loop(ctx)
}

func (u *Updater) bgContext() context.Context {
	if u.bgCtx != nil {
		return u.bgCtx
	}
	return context.Background()
}

func (u *Updater) loop(ctx context.Context) {
	select {
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return
	}

	u.checkAndUpdate(ctx)

	for {
		cfg := normalizeConfig(u.cfg())
		interval := time.Duration(cfg.CheckInterval) * time.Second
		if interval < time.Minute {
			interval = time.Minute
		}
		select {
		case <-time.After(interval):
			u.checkAndUpdate(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (u *Updater) checkAndUpdate(ctx context.Context) {
	cfg := normalizeConfig(u.cfg())
	if !cfg.Enabled {
		return
	}
	u.performUpdate(ctx)
}

func (u *Updater) performUpdate(ctx context.Context) {
	cfg := normalizeConfig(u.cfg())

	u.mu.Lock()
	if u.status.State == "checking" || u.status.State == "ready" || u.status.State == "downloading" || u.status.State == "applying" {
		u.mu.Unlock()
		return
	}
	u.status.State = "checking"
	u.status.Progress = progressChecking
	u.status.Error = ""
	u.status.DownloadProgress = 0
	u.mu.Unlock()

	release, hasUpdate, err := u.checkForUpdate(ctx, cfg)
	if err != nil {
		u.setError("check failed: " + err.Error())
		return
	}
	if release == nil || !hasUpdate {
		u.mu.Lock()
		u.status.State = "idle"
		u.status.Progress = 0
		u.status.DownloadProgress = 0
		u.status.LastCheck = time.Now().UTC().Format(time.RFC3339)
		u.mu.Unlock()
		return
	}

	u.mu.Lock()
	u.status.LatestVersion = release.displayVersion()
	u.status.IsPrerelease = release.Prerelease
	u.status.ReleaseNotes = release.Body
	u.status.LastCheck = time.Now().UTC().Format(time.RFC3339)
	u.status.Progress = progressReleaseFound
	u.mu.Unlock()

	binaryPath, err := u.download(ctx, cfg, release)
	if err != nil {
		u.setError("download failed: " + err.Error())
		return
	}

	if cfg.Channel == "stable" {
		u.mu.Lock()
		u.status.State = "applying"
		u.status.Progress = progressApplying
		u.status.DownloadProgress = 0
		u.mu.Unlock()
		if err := u.waitForIdle(ctx); err != nil {
			u.setError("apply canceled while waiting for idle: " + err.Error())
			return
		}
		if err := u.applyUpdate(binaryPath, release.TagName); err != nil {
			u.notifyExecFailure(err)
			u.setError("apply failed: " + err.Error())
		}
		return
	}

	u.mu.Lock()
	u.status.State = "ready"
	u.status.Progress = progressVerifyDone
	u.status.DownloadProgress = 0
	u.pendingBinaryPath = binaryPath
	u.pendingTag = release.TagName
	u.mu.Unlock()
	u.logger.Printf("update: pre-release %s ready, waiting for admin confirmation", release.TagName)
}

func (u *Updater) setError(msg string) {
	u.logger.Printf("update: %s", msg)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.State = "failed"
	u.status.Error = msg
	u.status.LastCheck = time.Now().UTC().Format(time.RFC3339)
}

func clampProgress(progress float64) float64 {
	if progress < 0 {
		return 0
	}
	if progress > 100 {
		return 100
	}
	return progress
}

func overallDownloadProgress(downloadProgress float64) float64 {
	downloadProgress = clampProgress(downloadProgress)
	span := progressDownloadDone - progressDownloadStart
	return progressDownloadStart + downloadProgress*float64(span)/100
}

func (u *Updater) notifyExecFailure(err error) {
	if err == nil || u.hooks.OnExecFailure == nil {
		return
	}
	u.hooks.OnExecFailure(err)
}

// waitForIdle blocks until the application reports no in-flight work. A
// canceled application context aborts the update; only the deliberate idle
// timeout permits applying while work is still reported as active.
func (u *Updater) waitForIdle(ctx context.Context) error {
	if u.hooks.IsBusy == nil || !u.hooks.IsBusy() {
		return nil
	}
	u.logger.Printf("update: waiting for in-flight jobs to finish before applying (max %s)", idleWaitTimeout)
	deadline := time.After(idleWaitTimeout)
	ticker := time.NewTicker(idlePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			u.logger.Printf("update: idle wait timed out, applying anyway")
			return nil
		case <-ticker.C:
			if !u.hooks.IsBusy() {
				return nil
			}
		}
	}
}

type releaseInfo struct {
	TagName         string      `json:"tag_name"`
	TargetCommitish string      `json:"target_commitish"`
	Prerelease      bool        `json:"prerelease"`
	Body            string      `json:"body"`
	Assets          []assetInfo `json:"assets"`
	Version         string
	Commit          string
	BuildTime       string
}

type assetInfo struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type releaseVersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Tag       string `json:"tag"`
}

func (r releaseInfo) displayVersion() string {
	if strings.TrimSpace(r.Version) != "" {
		return strings.TrimSpace(r.Version)
	}
	return r.TagName
}

var (
	// githubBaseURL is a var so tests can point direct-source checks at a
	// local server.
	githubBaseURL = "https://github.com"
	// signingPublicKeyHex is the Ed25519 public key matching the
	// UPDATE_SIGNING_KEY repository secret used by CI to sign release
	// assets (see scripts/sign).
	signingPublicKeyHex = "84f05a289f964f3ac0c7ca590f43b1d8bb12ff0a19753cacff1a8cc9733443ed"
)

func (u *Updater) checkForUpdate(ctx context.Context, cfg Config) (*releaseInfo, bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var (
		release *releaseInfo
		err     error
	)
	if cfg.Source == SourceProxy {
		release, err = u.fetchReleaseViaProxy(checkCtx, cfg)
	} else {
		release, err = u.fetchReleaseFromGitHub(checkCtx, cfg)
	}
	if err != nil || release == nil {
		return nil, false, err
	}
	if !u.isNewer(*release, cfg.Channel) {
		u.logger.Printf("update: already up to date (%s)", release.displayVersion())
		return release, false, nil
	}
	return release, true, nil
}

// fetchReleaseFromGitHub resolves the latest release without touching the
// GitHub REST API: it fetches the signed version.json straight from the
// release download URL (fixed "dev" tag, or the "latest" redirect for
// stable) and synthesizes the asset list from the tag it names.
func (u *Updater) fetchReleaseFromGitHub(ctx context.Context, cfg Config) (*releaseInfo, error) {
	base := githubBaseURL + "/" + cfg.Repo + "/releases"
	versionURL := base + "/latest/download/version.json"
	if cfg.Channel != "stable" {
		versionURL = base + "/download/dev/version.json"
	}
	u.logger.Printf("update: checking %s", versionURL)

	body, status, err := u.httpGet(ctx, versionURL, 16*1024)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		u.logger.Printf("update: no release found for channel %s", cfg.Channel)
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", status)
	}

	var info releaseVersionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode version metadata: %w", err)
	}
	tag := strings.TrimSpace(info.Tag)
	if cfg.Channel != "stable" {
		tag = "dev"
	} else if tag == "" {
		return nil, fmt.Errorf("version metadata missing release tag")
	}

	// The signature lives next to the assets of the tag the metadata
	// names, so a tampered body cannot redirect verification elsewhere:
	// only the holder of the signing key can produce a matching pair.
	sig, sigStatus, err := u.httpGet(ctx, base+"/download/"+tag+"/version.json.sig", 4*1024)
	if err != nil {
		return nil, fmt.Errorf("fetch version metadata signature: %w", err)
	}
	if sigStatus != http.StatusOK {
		return nil, fmt.Errorf("version metadata signature returned status %d", sigStatus)
	}
	if err := verifySignature(body, sig); err != nil {
		return nil, fmt.Errorf("version metadata: %w", err)
	}

	targetName := u.targetName()
	assetNames := []string{targetName, targetName + ".sha256", targetName + ".sha256.sig", "version.json"}
	assets := make([]assetInfo, 0, len(assetNames))
	for _, name := range assetNames {
		assets = append(assets, assetInfo{
			Name:               name,
			BrowserDownloadURL: base + "/download/" + tag + "/" + name,
		})
	}
	return &releaseInfo{
		TagName:    tag,
		Prerelease: cfg.Channel != "stable",
		Assets:     assets,
		Version:    strings.TrimSpace(info.Version),
		Commit:     strings.TrimSpace(info.Commit),
		BuildTime:  strings.TrimSpace(info.BuildTime),
	}, nil
}

func (u *Updater) fetchReleaseViaProxy(ctx context.Context, cfg Config) (*releaseInfo, error) {
	tag := "latest"
	if cfg.Channel != "stable" {
		tag = "dev"
	}

	url := fmt.Sprintf("%s/api/releases/%s/%s", strings.TrimRight(cfg.ProxyBaseURL, "/"), cfg.Repo, tag)
	u.logger.Printf("update: checking %s", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		u.logger.Printf("update: no release found for channel %s", cfg.Channel)
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var release releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if cfg.Channel != "stable" {
		if err := u.loadReleaseVersion(ctx, cfg, &release); err != nil {
			u.logger.Printf("update: version metadata unavailable for %s: %v", release.TagName, err)
		}
	}
	return &release, nil
}

func (u *Updater) loadReleaseVersion(ctx context.Context, cfg Config, release *releaseInfo) error {
	var versionAsset *assetInfo
	for i := range release.Assets {
		if release.Assets[i].Name == "version.json" {
			versionAsset = &release.Assets[i]
			break
		}
	}
	if versionAsset == nil {
		return fmt.Errorf("version.json asset not found")
	}

	versionURL := u.resolveDownloadURL(cfg, versionAsset.BrowserDownloadURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("version metadata returned status %d", resp.StatusCode)
	}

	var info releaseVersionInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&info); err != nil {
		return fmt.Errorf("decode version metadata: %w", err)
	}
	release.Version = strings.TrimSpace(info.Version)
	release.Commit = strings.TrimSpace(info.Commit)
	release.BuildTime = strings.TrimSpace(info.BuildTime)
	return nil
}

func (u *Updater) isNewer(release releaseInfo, channel string) bool {
	current := version.Version
	if current == "dev" {
		return true
	}
	remoteTag := release.TagName
	if channel == "stable" {
		return semverGreater(remoteTag, current)
	}

	remoteCommit := normalizeCommit(release.Commit)
	if remoteCommit == "" {
		remoteCommit = normalizeCommit(release.TargetCommitish)
	}
	currentCommit := normalizeCommit(version.Commit)
	if remoteCommit != "" && currentCommit != "" {
		return remoteCommit != currentCommit
	}

	remoteVersion := release.displayVersion()
	if remoteTag == "dev" && remoteVersion == "dev" {
		u.logger.Printf("update: dev release missing comparable commit current=%s remote=%s, skipping", current, remoteTag)
		return false
	}

	remoteNum, remoteSHA := parseDevTag(remoteVersion)
	localNum, localSHA := parseDevTag(current)
	if remoteSHA != "" && localSHA != "" && remoteSHA == localSHA {
		return false
	}
	if remoteNum > 0 && localNum > 0 {
		return remoteNum > localNum
	}
	u.logger.Printf("update: cannot compare versions current=%s remote=%s, skipping", current, remoteTag)
	return false
}

func normalizeCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if commit == "" || commit == "unknown" {
		return ""
	}
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

func semverGreater(a, b string) bool {
	av := parseSemver(strings.TrimPrefix(a, "v"))
	bv := parseSemver(strings.TrimPrefix(b, "v"))
	for i := 0; i < 3; i++ {
		if av[i] > bv[i] {
			return true
		}
		if av[i] < bv[i] {
			return false
		}
	}
	return false
}

func parseSemver(s string) [3]int {
	var result [3]int
	parts := strings.SplitN(s, ".", 3)
	for i, p := range parts {
		if i >= 3 {
			break
		}
		if idx := strings.IndexByte(p, '-'); idx >= 0 {
			p = p[:idx]
		}
		n, _ := strconv.Atoi(p)
		result[i] = n
	}
	return result
}

func parseDevTag(tag string) (runNumber int, sha string) {
	parts := strings.SplitN(tag, "-", 4)
	if len(parts) >= 4 && parts[0] == "dev" {
		n, _ := strconv.Atoi(parts[1])
		return n, parts[3]
	}
	return 0, ""
}

func (u *Updater) targetName() string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	target := goos + "-" + goarch

	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return "responses2chat-" + target + ext
}

func (u *Updater) download(ctx context.Context, cfg Config, release *releaseInfo) (string, error) {
	u.mu.Lock()
	u.status.State = "downloading"
	u.status.Progress = progressDownloadStart
	u.status.DownloadProgress = 0
	u.mu.Unlock()

	targetName := u.targetName()
	var binaryAsset, sha256Asset, sigAsset *assetInfo
	for i := range release.Assets {
		a := &release.Assets[i]
		switch a.Name {
		case targetName:
			binaryAsset = a
		case targetName + ".sha256":
			sha256Asset = a
		case targetName + ".sha256.sig":
			sigAsset = a
		}
	}
	if binaryAsset == nil {
		return "", fmt.Errorf("no asset found for %s in release %s", targetName, release.TagName)
	}
	if sha256Asset == nil {
		return "", fmt.Errorf("release %s is missing %s.sha256, refusing unverified update", release.TagName, targetName)
	}
	if sigAsset == nil {
		return "", fmt.Errorf("release %s is missing %s.sha256.sig, refusing unsigned update", release.TagName, targetName)
	}

	updateDir := filepath.Join(u.dataDir(), "updates")
	if err := os.MkdirAll(updateDir, 0o755); err != nil {
		return "", fmt.Errorf("create update dir: %w", err)
	}

	finalName := "responses2chat-" + sanitizePathPart(release.TagName)
	if runtime.GOOS == "windows" {
		finalName += ".exe"
	}
	tmpPath := filepath.Join(updateDir, finalName+".tmp")
	finalPath := filepath.Join(updateDir, finalName)

	dlCtx, cancelDownload := context.WithTimeout(ctx, downloadTimeout)
	defer cancelDownload()

	downloadURL := u.resolveDownloadURL(cfg, binaryAsset.BrowserDownloadURL)
	if err := u.downloadFile(dlCtx, downloadURL, tmpPath, binaryAsset.Size); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("download binary: %w", err)
	}

	u.mu.Lock()
	u.status.Progress = progressVerifyStart
	u.mu.Unlock()

	shaBody, err := u.fetchAsset(dlCtx, cfg, sha256Asset, 1024)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("fetch sha256: %w", err)
	}
	sigBody, err := u.fetchAsset(dlCtx, cfg, sigAsset, 4*1024)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("fetch sha256 signature: %w", err)
	}
	if err := verifySignature(shaBody, sigBody); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("sha256 file: %w", err)
	}

	parts := strings.Fields(strings.TrimSpace(string(shaBody)))
	if len(parts) == 0 {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("empty sha256 file")
	}
	expectedHash := parts[0]
	actualHash, err := fileSHA256(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("compute sha256: %w", err)
	}
	if !strings.EqualFold(actualHash, expectedHash) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("sha256 mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	u.logger.Printf("update: signature and SHA256 verified for %s", release.TagName)

	u.mu.Lock()
	u.status.Progress = progressVerifyDone
	u.mu.Unlock()

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	u.logger.Printf("update: downloaded %s to %s", release.TagName, finalPath)
	return finalPath, nil
}

// resolveDownloadURL maps a GitHub browser_download_url onto the proxy
// mirror when the proxy source is selected; in direct mode the URL is used
// as-is.
func (u *Updater) resolveDownloadURL(cfg Config, browserURL string) string {
	if cfg.Source != SourceProxy {
		return browserURL
	}
	base := strings.TrimRight(cfg.ProxyBaseURL, "/")
	const ghPrefix = "https://github.com/"
	if !strings.HasPrefix(browserURL, ghPrefix) {
		return browserURL
	}
	path := strings.TrimPrefix(browserURL, ghPrefix)
	const relSegment = "/releases/download/"
	idx := strings.Index(path, relSegment)
	if idx < 0 {
		return browserURL
	}
	ownerRepo := path[:idx]
	tagAndAsset := path[idx+len(relSegment):]
	return base + "/download/" + ownerRepo + "/" + tagAndAsset
}

func (u *Updater) downloadFile(ctx context.Context, url, destPath string, expectedSize int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	totalSize := resp.ContentLength
	if totalSize <= 0 && expectedSize > 0 {
		totalSize = expectedSize
	}

	var written int64
	var lastProgress float64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := f.Write(buf[:n]); wErr != nil {
				return wErr
			}
			written += int64(n)
			if totalSize > 0 {
				progress := float64(written) / float64(totalSize) * 100
				if progress-lastProgress >= 1 || progress >= 100 {
					u.mu.Lock()
					u.status.DownloadProgress = clampProgress(progress)
					u.status.Progress = overallDownloadProgress(progress)
					u.mu.Unlock()
					lastProgress = progress
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	u.mu.Lock()
	u.status.DownloadProgress = progressComplete
	u.status.Progress = overallDownloadProgress(progressComplete)
	u.mu.Unlock()
	return nil
}

// httpGet fetches url and returns up to limit bytes of the body along with
// the status code. Network errors are returned; HTTP error statuses are not.
func (u *Updater) httpGet(ctx context.Context, url string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (u *Updater) fetchAsset(ctx context.Context, cfg Config, asset *assetInfo, limit int64) ([]byte, error) {
	body, status, err := u.httpGet(ctx, u.resolveDownloadURL(cfg, asset.BrowserDownloadURL), limit)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", asset.Name, status)
	}
	return body, nil
}

// verifySignature checks the base64 Ed25519 signature produced by
// scripts/sign against the embedded release signing public key.
func verifySignature(message, sig []byte) error {
	pub, err := hex.DecodeString(signingPublicKeyHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid embedded signing public key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), message, raw) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (u *Updater) applyUpdate(newBinaryPath, tag string) error {
	u.mu.Lock()
	u.status.State = "applying"
	u.status.Progress = progressApplying
	u.mu.Unlock()

	if runtime.GOOS == "windows" {
		return u.applyUpdateWindows(newBinaryPath, tag)
	}
	return u.applyUpdateUnix(newBinaryPath, tag)
}

func (u *Updater) applyUpdateUnix(newBinaryPath, tag string) error {
	if u.hooks.BeforeExec != nil {
		if err := u.hooks.BeforeExec(tag); err != nil {
			return fmt.Errorf("prepare restart: %w", err)
		}
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	backupPath := execPath + ".bak"
	if err := os.Rename(execPath, backupPath); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}
	if err := copyFile(newBinaryPath, execPath); err != nil {
		_ = os.Rename(backupPath, execPath)
		return fmt.Errorf("install new binary: %w", err)
	}
	if err := os.Chmod(execPath, 0o755); err != nil {
		_ = os.Rename(backupPath, execPath)
		_ = os.Remove(newBinaryPath)
		return fmt.Errorf("chmod new binary: %w", err)
	}

	_ = os.Remove(backupPath)
	_ = os.Remove(newBinaryPath)

	u.logger.Printf("update: restarting with new binary %s", tag)
	u.mu.Lock()
	u.status.Progress = progressComplete
	u.mu.Unlock()
	return replaceProcess(execPath, os.Args, os.Environ())
}

func (u *Updater) applyUpdateWindows(newBinaryPath, tag string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	execPath, err = filepath.Abs(execPath)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}

	updateDir := filepath.Dir(newBinaryPath)
	scriptPath := filepath.Join(updateDir, "apply-"+sanitizePathPart(tag)+".ps1")
	backupPath := execPath + ".bak"
	script := strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		fmt.Sprintf("$pidToWait = %d", os.Getpid()),
		"$exe = " + psQuote(execPath),
		"$new = " + psQuote(newBinaryPath),
		"$bak = " + psQuote(backupPath),
		"$argsList = " + psArray(os.Args[1:]),
		"$workDir = " + psQuote(cwd),
		"while (Get-Process -Id $pidToWait -ErrorAction SilentlyContinue) { Start-Sleep -Milliseconds 250 }",
		"if (Test-Path $bak) { Remove-Item -Force $bak }",
		"if (Test-Path $exe) { Move-Item -Force $exe $bak }",
		"Copy-Item -Force $new $exe",
		"Remove-Item -Force $new",
		"Start-Process -FilePath $exe -ArgumentList $argsList -WorkingDirectory $workDir",
		"Remove-Item -Force $PSCommandPath",
		"",
	}, "\r\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return fmt.Errorf("write apply script: %w", err)
	}

	if u.hooks.BeforeExec != nil {
		if err := u.hooks.BeforeExec(tag); err != nil {
			return fmt.Errorf("prepare restart: %w", err)
		}
	}

	proc, err := os.StartProcess("powershell.exe", []string{
		"powershell.exe",
		"-NoProfile",
		"-ExecutionPolicy", "Bypass",
		"-File", scriptPath,
	}, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Env:   os.Environ(),
	})
	if err != nil {
		return fmt.Errorf("start apply script: %w", err)
	}
	_ = proc.Release()

	u.logger.Printf("update: restarting with new binary %s", tag)
	u.mu.Lock()
	u.status.Progress = progressComplete
	u.mu.Unlock()
	os.Exit(0)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func normalizeConfig(cfg Config) Config {
	if strings.TrimSpace(cfg.Channel) == "" {
		cfg.Channel = "stable"
	}
	cfg.Channel = strings.ToLower(strings.TrimSpace(cfg.Channel))
	if cfg.Channel != "stable" {
		cfg.Channel = "dev"
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 3600
	}
	cfg.Source = strings.ToLower(strings.TrimSpace(cfg.Source))
	if cfg.Source != SourceProxy {
		cfg.Source = SourceGitHub
	}
	if strings.TrimSpace(cfg.ProxyBaseURL) == "" {
		cfg.ProxyBaseURL = "https://dl.repo.chycloud.top"
	}
	cfg.ProxyBaseURL = strings.TrimRight(strings.TrimSpace(cfg.ProxyBaseURL), "/")
	cfg.Repo = strings.TrimSpace(cfg.Repo)
	if cfg.Repo == "" {
		cfg.Repo = "lieyanc/responses2chat"
	}
	cfg.AdminToken = strings.TrimSpace(cfg.AdminToken)
	return cfg
}

func sanitizePathPart(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "update"
	}
	return b.String()
}

func psQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func psArray(values []string) string {
	if len(values) == 0 {
		return "@()"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, psQuote(value))
	}
	return "@(" + strings.Join(quoted, ", ") + ")"
}
