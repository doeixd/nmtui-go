package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// --- Constants ---

const (
	githubOwner       = "doeixd"
	githubRepo        = "nmtui-go"
	latestReleaseURL  = "https://api.github.com/repos/" + githubOwner + "/" + githubRepo + "/releases/latest"
	allReleasesURL    = "https://api.github.com/repos/" + githubOwner + "/" + githubRepo + "/releases"
	updateCacheName   = "update-check.json"
	cacheFreshnessSeconds = 86400 // 24 hours
	httpTimeout       = 10 * time.Second
	downloadTimeout   = 5 * time.Minute
	pkgMgrTimeout     = 2 * time.Second
)

// --- Types ---

type installSource int

const (
	installSourceUnknown  installSource = iota
	installSourceGitHub                         // installed from GitHub releases
	installSourceAUR                            // installed via pacman / AUR
	installSourceDeb                            // installed via apt / dpkg
	installSourceRPM                            // installed via rpm / dnf
	installSourcePackaged                       // generic: some package manager owns this
)

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
	HTMLURL    string    `json:"html_url"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type updateCheckResult struct {
	CurrentVersion string
	LatestVersion  string
	UpdateAvail    bool
	ReleaseURL     string
}

type updateCheckCache struct {
	LatestVersion string `json:"latest_version"`
	CheckedAt     int64  `json:"checked_at"`
	ReleaseURL    string `json:"release_url"`
}

// --- Update Step / Progress / Options ---

type UpdateStep int

const (
	UpdateStepChecking UpdateStep = iota
	UpdateStepFound
	UpdateStepDownloadingChecksums
	UpdateStepDownloadingArchive
	UpdateStepVerifying
	UpdateStepExtracting
	UpdateStepBackingUp
	UpdateStepReplacing
	UpdateStepVerifyingInstall
	UpdateStepDone
	UpdateStepAlreadyUpToDate
)

type UpdateProgress struct {
	Step    UpdateStep
	Message string
}

type UpdateOptions struct {
	Ctx             context.Context
	KeepBackup      bool
	AllowPrerelease bool
	ProgressFn      func(UpdateProgress)
}

// --- Prerelease / Config Helpers ---

func isPrerelease(tag string) bool {
	v := strings.TrimPrefix(tag, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return false
	}
	patch := parts[2]
	return strings.ContainsAny(patch, "-+")
}

func getAllowPrereleaseConfig() bool {
	return os.Getenv("NMTUI_UPDATE_PRERELEASE") == "1"
}

func getKeepBackupConfig() bool {
	v := os.Getenv("NMTUI_UPDATE_KEEP_BACKUP")
	return v != "0" && strings.ToLower(v) != "false"
}

func fetchLatestStableOrPrerelease(ctx context.Context, allowPrerelease bool) (*ghRelease, error) {
	if !allowPrerelease {
		return fetchLatestReleaseCtx(ctx)
	}
	client := newHTTPClient(httpTimeout)
	req, err := newGitHubRequestWithContext(ctx, "GET", allReleasesURL+"?per_page=10", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	log.Printf("Update: fetching %s (prerelease allowed)", allReleasesURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("GitHub API rate limit exceeded (HTTP 403). Set GITHUB_TOKEN to increase the limit")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	var releases []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding releases JSON: %w", err)
	}
	for i := range releases {
		if !releases[i].Draft {
			log.Printf("Update: first non-draft release is %s (prerelease=%v)", releases[i].TagName, releases[i].Prerelease)
			return &releases[i], nil
		}
	}
	return nil, fmt.Errorf("no non-draft releases found for %s/%s", githubOwner, githubRepo)
}

// --- Backup / Verify / Rollback ---

func backupBinary(currentPath string) (string, error) {
	backupPath := currentPath + ".old"
	_ = os.Remove(backupPath)
	if err := os.Rename(currentPath, backupPath); err != nil {
		return "", fmt.Errorf("backing up %s to %s: %w", currentPath, backupPath, err)
	}
	log.Printf("Update: backed up %s to %s", currentPath, backupPath)
	return backupPath, nil
}

func verifyBinary(binaryPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("binary verification failed (%s --version): %w", binaryPath, err)
	}
	log.Printf("Update: verified %s --version exits 0", binaryPath)
	return nil
}

func rollbackBinary(currentPath, backupPath string) error {
	if err := os.Rename(backupPath, currentPath); err != nil {
		return fmt.Errorf("rollback failed (rename %s -> %s): %w", backupPath, currentPath, err)
	}
	log.Printf("Update: rolled back %s from %s", currentPath, backupPath)
	return nil
}

// --- Version Comparison ---

// parseVersion parses "v1.2.3" or "1.2.3" into [3]int{1,2,3}.
func parseVersion(v string) ([3]int, error) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return [3]int{}, fmt.Errorf("invalid version format: %q (expected X.Y.Z)", v)
	}
	var result [3]int
	for i, p := range parts {
		// Strip any pre-release suffix from the patch component (e.g., "3-beta")
		if i == 2 {
			if idx := strings.IndexAny(p, "-+"); idx >= 0 {
				p = p[:idx]
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, fmt.Errorf("invalid version component %q in %q: %w", parts[i], v, err)
		}
		result[i] = n
	}
	return result, nil
}

// isNewer returns true if candidate is a newer version than current.
func isNewer(current, candidate string) (bool, error) {
	cur, err := parseVersion(current)
	if err != nil {
		return false, fmt.Errorf("current version: %w", err)
	}
	cand, err := parseVersion(candidate)
	if err != nil {
		return false, fmt.Errorf("candidate version: %w", err)
	}
	for i := 0; i < 3; i++ {
		if cand[i] > cur[i] {
			return true, nil
		}
		if cand[i] < cur[i] {
			return false, nil
		}
	}
	return false, nil // equal
}

// --- Package Manager Detection ---

// detectInstallSource determines how the binary was installed.
// Priority: build-time InstallMethod > runtime package manager check > path heuristic.
func detectInstallSource() installSource {
	// 1. Build-time flag (most reliable)
	switch strings.ToLower(InstallMethod) {
	case "github", "binary":
		return installSourceGitHub
	case "aur", "pacman":
		return installSourceAUR
	case "deb", "apt", "dpkg":
		return installSourceDeb
	case "rpm", "dnf", "yum":
		return installSourceRPM
	case "":
		// Fall through to runtime detection
	default:
		return installSourcePackaged
	}

	// 2. Runtime detection
	binPath, err := os.Executable()
	if err != nil {
		log.Printf("Update: cannot determine executable path: %v", err)
		return installSourceUnknown
	}
	binPath, err = filepath.EvalSymlinks(binPath)
	if err != nil {
		log.Printf("Update: cannot resolve symlinks for %s: %v", binPath, err)
		return installSourceUnknown
	}

	if src := detectInstallSourceRuntime(binPath); src != installSourceUnknown {
		return src
	}

	// 3. Path heuristic
	return detectInstallSourceByPath(binPath)
}

// detectInstallSourceRuntime queries package managers to see if they own the binary.
func detectInstallSourceRuntime(binaryPath string) installSource {
	checks := []struct {
		cmd  string
		args []string
		src  installSource
	}{
		{"pacman", []string{"-Qo", binaryPath}, installSourceAUR},
		{"dpkg", []string{"-S", binaryPath}, installSourceDeb},
		{"rpm", []string{"-qf", binaryPath}, installSourceRPM},
	}

	for _, c := range checks {
		if _, err := exec.LookPath(c.cmd); err != nil {
			continue // command not installed, skip
		}
		ctx, cancel := context.WithTimeout(context.Background(), pkgMgrTimeout)
		cmd := exec.CommandContext(ctx, c.cmd, c.args...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		err := cmd.Run()
		cancel()
		if err == nil {
			log.Printf("Update: %s owns %s", c.cmd, binaryPath)
			return c.src
		}
	}
	return installSourceUnknown
}

// detectInstallSourceByPath guesses install method from binary location.
func detectInstallSourceByPath(binaryPath string) installSource {
	dir := filepath.Dir(binaryPath)
	switch {
	case strings.HasPrefix(dir, "/usr/bin"),
		strings.HasPrefix(dir, "/usr/sbin"):
		return installSourcePackaged
	case strings.HasPrefix(dir, "/usr/local/bin"),
		strings.HasPrefix(dir, "/usr/local/sbin"):
		return installSourceGitHub
	}

	home, err := os.UserHomeDir()
	if err == nil {
		localBin := filepath.Join(home, ".local", "bin")
		if strings.HasPrefix(dir, localBin) {
			return installSourceGitHub
		}
	}

	return installSourceUnknown
}

// installSourceName returns a human-readable name for the install source.
func installSourceName(src installSource) string {
	switch src {
	case installSourceGitHub:
		return "GitHub release"
	case installSourceAUR:
		return "pacman (AUR)"
	case installSourceDeb:
		return "apt/dpkg"
	case installSourceRPM:
		return "rpm/dnf"
	case installSourcePackaged:
		return "system package manager"
	default:
		return "unknown"
	}
}

// packageManagerUpdateHint returns the suggested update command for the install source.
func packageManagerUpdateHint(src installSource) string {
	switch src {
	case installSourceAUR:
		return "yay -Syu nmtui-go  (or paru -Syu nmtui-go)"
	case installSourceDeb:
		return "sudo apt update && sudo apt upgrade nmtui-go"
	case installSourceRPM:
		return "sudo dnf upgrade nmtui-go"
	default:
		return "your system package manager"
	}
}

// isPackageManaged returns true if the install source is a package manager (not GitHub/unknown).
func isPackageManaged(src installSource) bool {
	switch src {
	case installSourceAUR, installSourceDeb, installSourceRPM, installSourcePackaged:
		return true
	default:
		return false
	}
}

// --- GitHub API ---

// newHTTPClient creates an HTTP client with a User-Agent and optional auth.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// newGitHubRequestWithContext creates a request with appropriate headers and context.
func newGitHubRequestWithContext(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "nmtui-go/"+Version)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// newGitHubRequest creates a request with appropriate headers (uses background context).
func newGitHubRequest(method, url string, body io.Reader) (*http.Request, error) {
	return newGitHubRequestWithContext(context.Background(), method, url, body)
}

// fetchLatestReleaseCtx queries the GitHub API for the latest release with context.
func fetchLatestReleaseCtx(ctx context.Context) (*ghRelease, error) {
	client := newHTTPClient(httpTimeout)
	req, err := newGitHubRequestWithContext(ctx, "GET", latestReleaseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	log.Printf("Update: fetching %s", latestReleaseURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("GitHub API rate limit exceeded (HTTP 403). Set GITHUB_TOKEN to increase the limit")
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases found for %s/%s", githubOwner, githubRepo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	var release ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decoding release JSON: %w", err)
	}

	log.Printf("Update: latest release is %s", release.TagName)
	return &release, nil
}

// fetchLatestRelease queries the GitHub API for the latest release.
func fetchLatestRelease() (*ghRelease, error) {
	return fetchLatestReleaseCtx(context.Background())
}

// --- Update Cache ---

// updateCacheDir returns the cache directory path, creating it if needed.
// Respects XDG_CACHE_HOME on Linux; falls back to os.UserCacheDir().
func updateCacheDir() (string, error) {
	var baseDir string
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		baseDir = xdg
	} else {
		var err error
		baseDir, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine cache directory: %w", err)
		}
	}
	dir := filepath.Join(baseDir, "nmtui-go")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("cannot create cache directory %s: %w", dir, err)
	}
	return dir, nil
}

// updateCachePath returns the full path to the update cache file.
func updateCachePath() (string, error) {
	dir, err := updateCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, updateCacheName), nil
}

// loadUpdateCache reads and parses the cache file. Returns nil on any error.
func loadUpdateCache() *updateCheckCache {
	path, err := updateCachePath()
	if err != nil {
		log.Printf("Update: cache path error: %v", err)
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Update: cache read error: %v", err)
		}
		return nil
	}
	var cache updateCheckCache
	if err := json.Unmarshal(data, &cache); err != nil {
		log.Printf("Update: cache decode error, deleting: %v", err)
		_ = os.Remove(path)
		return nil
	}
	return &cache
}

// saveUpdateCache writes the cache file atomically.
func saveUpdateCache(c *updateCheckCache) error {
	path, err := updateCachePath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding cache: %w", err)
	}

	// Write to temp file then rename for atomicity
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".update-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp cache file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing temp cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp cache: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming cache file: %w", err)
	}
	return nil
}

// --- Cached Update Check (for TUI background notification) ---

// checkForUpdate checks if a newer version is available, using the 24h cache.
// Returns nil result if disabled via env var or on error.
func checkForUpdate() (*updateCheckResult, error) {
	if os.Getenv("NMTUI_NO_UPDATE_CHECK") == "1" {
		log.Printf("Update: check disabled via NMTUI_NO_UPDATE_CHECK")
		return nil, nil
	}

	// Check cache first
	if cache := loadUpdateCache(); cache != nil {
		age := time.Now().Unix() - cache.CheckedAt
		if age < cacheFreshnessSeconds {
			log.Printf("Update: using cached result (age: %ds)", age)
			newer, err := isNewer(Version, cache.LatestVersion)
			if err != nil {
				log.Printf("Update: version compare error: %v", err)
				return nil, nil
			}
			return &updateCheckResult{
				CurrentVersion: Version,
				LatestVersion:  cache.LatestVersion,
				UpdateAvail:    newer,
				ReleaseURL:     cache.ReleaseURL,
			}, nil
		}
		log.Printf("Update: cache expired (age: %ds)", age)
	}

	// Fetch from GitHub
	release, err := fetchLatestStableOrPrerelease(context.Background(), getAllowPrereleaseConfig())
	if err != nil {
		return nil, fmt.Errorf("checking for update: %w", err)
	}

	// Save to cache
	cacheEntry := &updateCheckCache{
		LatestVersion: release.TagName,
		CheckedAt:     time.Now().Unix(),
		ReleaseURL:    release.HTMLURL,
	}
	if err := saveUpdateCache(cacheEntry); err != nil {
		log.Printf("Update: failed to save cache: %v", err)
	}

	newer, err := isNewer(Version, release.TagName)
	if err != nil {
		return nil, fmt.Errorf("comparing versions: %w", err)
	}

	return &updateCheckResult{
		CurrentVersion: Version,
		LatestVersion:  release.TagName,
		UpdateAvail:    newer,
		ReleaseURL:     release.HTMLURL,
	}, nil
}

// --- Asset Selection ---

// findAsset finds the correct archive asset for the given OS and architecture.
func findAsset(assets []ghAsset, goos, goarch string) (*ghAsset, error) {
	// goreleaser naming: nmtui-go_<version>_<os>_<arch>.tar.gz
	suffix := fmt.Sprintf("_%s_%s.tar.gz", goos, goarch)
	for i := range assets {
		if strings.HasSuffix(assets[i].Name, suffix) {
			return &assets[i], nil
		}
	}
	return nil, fmt.Errorf("no release asset found for %s/%s (looked for *%s)", goos, goarch, suffix)
}

// findChecksumAsset finds the checksums.txt asset.
func findChecksumAsset(assets []ghAsset) (*ghAsset, error) {
	for i := range assets {
		if assets[i].Name == "checksums.txt" {
			return &assets[i], nil
		}
	}
	return nil, fmt.Errorf("no checksums.txt asset found in release")
}

// --- Download ---

// downloadFileCtx downloads a URL and returns the response body contents (context-aware).
func downloadFileCtx(ctx context.Context, url string) ([]byte, error) {
	client := newHTTPClient(downloadTimeout)
	req, err := newGitHubRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned HTTP %d for %s", resp.StatusCode, url)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading download body: %w", err)
	}
	return data, nil
}

// downloadFile downloads a URL and returns the response body contents.
func downloadFile(url string) ([]byte, error) {
	return downloadFileCtx(context.Background(), url)
}

// downloadToFileCtx downloads a URL to a file in the specified directory (context-aware).
func downloadToFileCtx(ctx context.Context, url, destDir string) (string, error) {
	client := newHTTPClient(downloadTimeout)
	req, err := newGitHubRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned HTTP %d for %s", resp.StatusCode, url)
	}

	tmp, err := os.CreateTemp(destDir, ".nmtui-download-*")
	if err != nil {
		return "", fmt.Errorf("creating temp file in %s: %w", destDir, err)
	}
	tmpName := tmp.Name()

	_, err = io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	if err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("writing download to %s: %w", tmpName, err)
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("closing download file: %w", closeErr)
	}

	return tmpName, nil
}

// downloadToFile downloads a URL to a file in the specified directory.
func downloadToFile(url, destDir string) (string, error) {
	return downloadToFileCtx(context.Background(), url, destDir)
}

// --- Checksum Verification ---

// verifyChecksum checks the SHA256 of archivePath against the checksums content.
// checksumsContent is the raw text of checksums.txt (goreleaser format: "<hash>  <filename>").
func verifyChecksum(archivePath, archiveFileName, checksumsContent string) error {
	// Find expected hash
	var expectedHash string
	for _, line := range strings.Split(checksumsContent, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "<sha256>  <filename>" (two spaces)
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[1]) == archiveFileName {
			expectedHash = strings.TrimSpace(parts[0])
			break
		}
	}
	if expectedHash == "" {
		return fmt.Errorf("no checksum found for %s in checksums.txt", archiveFileName)
	}

	// Compute actual hash
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing archive: %w", err)
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(actualHash, expectedHash) {
		return fmt.Errorf("checksum mismatch for %s:\n  expected: %s\n  actual:   %s", archiveFileName, expectedHash, actualHash)
	}

	log.Printf("Update: checksum verified for %s", archiveFileName)
	return nil
}

// --- Archive Extraction ---

// extractBinaryFromTarGz extracts the nmtui-go binary from a tar.gz archive.
// Returns the path to a temporary file containing the extracted binary.
func extractBinaryFromTarGz(archivePath, destDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("opening archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("creating gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading tar entry: %w", err)
		}

		// Look for the binary: goreleaser puts it at <dir>/nmtui-go
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) != "nmtui-go" {
			continue
		}

		// Extract to temp file in the same directory as target binary
		tmp, err := os.CreateTemp(destDir, ".nmtui-go-new-*")
		if err != nil {
			return "", fmt.Errorf("creating temp file for binary: %w", err)
		}
		tmpName := tmp.Name()

		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return "", fmt.Errorf("extracting binary: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpName)
			return "", fmt.Errorf("closing extracted binary: %w", err)
		}

		log.Printf("Update: extracted binary to %s (%d bytes)", tmpName, hdr.Size)
		return tmpName, nil
	}

	return "", fmt.Errorf("binary 'nmtui-go' not found in archive")
}

// --- Self-Update Orchestrator ---

func report(opts UpdateOptions, step UpdateStep, msg string) {
	if opts.ProgressFn != nil {
		opts.ProgressFn(UpdateProgress{Step: step, Message: msg})
	}
}

// performSelfUpdateCore is the core update logic with context support and progress callbacks.
func performSelfUpdateCore(opts UpdateOptions) (string, error) {
	ctx := opts.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	// Check install source
	src := detectInstallSource()
	if isPackageManaged(src) {
		return "", fmt.Errorf("this installation is managed by %s.\nUse `%s` to update instead",
			installSourceName(src), packageManagerUpdateHint(src))
	}

	// Get current binary path
	currentBin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine executable path: %w", err)
	}
	currentBin, err = filepath.EvalSymlinks(currentBin)
	if err != nil {
		return "", fmt.Errorf("cannot resolve executable path: %w", err)
	}

	// Capture file mode before any backup rename
	info, err := os.Stat(currentBin)
	if err != nil {
		return "", fmt.Errorf("stat current binary: %w", err)
	}
	origMode := info.Mode()

	// Check context
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Fetch latest release
	report(opts, UpdateStepChecking, "Checking for updates...")
	release, err := fetchLatestStableOrPrerelease(ctx, opts.AllowPrerelease)
	if err != nil {
		return "", err
	}

	newer, err := isNewer(Version, release.TagName)
	if err != nil {
		return "", fmt.Errorf("comparing versions: %w", err)
	}
	if !newer {
		report(opts, UpdateStepAlreadyUpToDate, "Already up to date ("+Version+")")
		return "", nil
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	report(opts, UpdateStepFound, fmt.Sprintf("Found %s (current: %s)", release.TagName, Version))

	// Find assets
	archiveAsset, err := findAsset(release.Assets, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	checksumAsset, err := findChecksumAsset(release.Assets)
	if err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Download checksums
	report(opts, UpdateStepDownloadingChecksums, "Downloading checksums...")
	checksumsData, err := downloadFileCtx(ctx, checksumAsset.BrowserDownloadURL)
	if err != nil {
		return "", fmt.Errorf("downloading checksums: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Download archive
	destDir := filepath.Dir(currentBin)
	report(opts, UpdateStepDownloadingArchive, fmt.Sprintf("Downloading %s...", archiveAsset.Name))
	archivePath, err := downloadToFileCtx(ctx, archiveAsset.BrowserDownloadURL, destDir)
	if err != nil {
		return "", fmt.Errorf("downloading archive: %w", err)
	}
	defer os.Remove(archivePath)

	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Verify checksum
	report(opts, UpdateStepVerifying, "Verifying checksum...")
	if err := verifyChecksum(archivePath, archiveAsset.Name, string(checksumsData)); err != nil {
		return "", err
	}

	// Extract binary
	report(opts, UpdateStepExtracting, "Extracting binary...")
	newBinPath, err := extractBinaryFromTarGz(archivePath, destDir)
	if err != nil {
		return "", err
	}
	defer func() {
		os.Remove(newBinPath)
	}()

	// Always backup for rollback safety
	report(opts, UpdateStepBackingUp, "Backing up current binary...")
	backupPath, backupErr := backupBinary(currentBin)
	if backupErr != nil {
		return "", backupErr
	}

	// Replace binary: chmod + rename
	report(opts, UpdateStepReplacing, "Replacing binary...")
	if err := os.Chmod(newBinPath, origMode); err != nil {
		_ = rollbackBinary(currentBin, backupPath)
		return "", fmt.Errorf("setting permissions on new binary: %w", err)
	}
	if err := os.Rename(newBinPath, currentBin); err != nil {
		_ = rollbackBinary(currentBin, backupPath)
		return "", fmt.Errorf("replacing binary at %s: %w\nThe binary location may require elevated privileges or different file ownership.", currentBin, err)
	}

	// Verify new binary
	report(opts, UpdateStepVerifyingInstall, "Verifying new binary...")
	if err := verifyBinary(currentBin); err != nil {
		log.Printf("Update: new binary verification failed, rolling back: %v", err)
		if rbErr := rollbackBinary(currentBin, backupPath); rbErr != nil {
			return "", fmt.Errorf("verification failed: %w; rollback also failed: %v", err, rbErr)
		}
		return "", fmt.Errorf("new binary failed verification (rolled back): %w", err)
	}

	// Clean up backup unless user wants to keep it
	if !opts.KeepBackup {
		_ = os.Remove(backupPath)
	}

	newVersion := strings.TrimPrefix(release.TagName, "v")
	report(opts, UpdateStepDone, fmt.Sprintf("Updated to %s!", newVersion))

	// Update cache
	_ = saveUpdateCache(&updateCheckCache{
		LatestVersion: release.TagName,
		CheckedAt:     time.Now().Unix(),
		ReleaseURL:    release.HTMLURL,
	})

	return newVersion, nil
}

// performSelfUpdate is the CLI wrapper for --update with stdout progress.
func performSelfUpdate() (string, error) {
	// Dev build warning
	if Commit == "none" {
		fmt.Println("Warning: you are running a development build.")
		fmt.Println("Updating will replace it with the latest release.")
	}

	return performSelfUpdateCore(UpdateOptions{
		Ctx:             context.Background(),
		KeepBackup:      getKeepBackupConfig(),
		AllowPrerelease: getAllowPrereleaseConfig(),
		ProgressFn: func(p UpdateProgress) {
			// Each step closes the previous line and opens its own.
			// Flow: Checking... Found | Checksums... done | Archive... done |
			//       Verify... ok | Extract... done | Backup... done |
			//       Replace... done | VerifyInstall... done | Done
			switch p.Step {
			case UpdateStepChecking:
				fmt.Print("Checking for updates... ")
			case UpdateStepAlreadyUpToDate:
				fmt.Println("already up to date")
				fmt.Printf("Current version: %s\n", Version)
			case UpdateStepFound:
				fmt.Println(p.Message)
			case UpdateStepDownloadingChecksums:
				fmt.Print("Downloading checksums... ")
			case UpdateStepDownloadingArchive:
				fmt.Println("done")
				fmt.Print(p.Message + " ")
			case UpdateStepVerifying:
				fmt.Println("done")
				fmt.Print("Verifying checksum... ")
			case UpdateStepExtracting:
				fmt.Println("ok")
				fmt.Print("Extracting binary... ")
			case UpdateStepBackingUp:
				fmt.Println("done")
				fmt.Print("Backing up... ")
			case UpdateStepReplacing:
				fmt.Println("done")
				fmt.Print("Replacing binary... ")
			case UpdateStepVerifyingInstall:
				fmt.Println("done")
				fmt.Print("Verifying install... ")
			case UpdateStepDone:
				fmt.Println("done")
				fmt.Printf("\n%s\n", p.Message)
			}
		},
	})
}

// --- CLI Wrappers ---

// performSelfUpdateCLI is the CLI entry point for --update.
func performSelfUpdateCLI() int {
	newVersion, err := performSelfUpdate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if newVersion == "" {
		// Already up to date
		return 0
	}
	return 0
}

// performCheckUpdateCLI is the CLI entry point for --check-update.
func performCheckUpdateCLI() int {
	fmt.Print("Checking for updates... ")
	release, err := fetchLatestRelease()
	if err != nil {
		fmt.Println("failed")
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	newer, err := isNewer(Version, release.TagName)
	if err != nil {
		fmt.Println("failed")
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if newer {
		fmt.Printf("update available!\n")
		fmt.Printf("  Current: %s\n", Version)
		fmt.Printf("  Latest:  %s\n", release.TagName)
		fmt.Printf("  Run `nmtui-go --update` to update.\n")
	} else {
		fmt.Printf("up to date\n")
		fmt.Printf("  Version: %s\n", Version)
	}
	return 0
}
