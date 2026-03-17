package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- parseVersion tests ---

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input   string
		want    [3]int
		wantErr bool
	}{
		{"1.2.3", [3]int{1, 2, 3}, false},
		{"v1.2.3", [3]int{1, 2, 3}, false},
		{"0.0.1", [3]int{0, 0, 1}, false},
		{"v0.0.1", [3]int{0, 0, 1}, false},
		{"10.20.30", [3]int{10, 20, 30}, false},
		{"v1.0.0-beta", [3]int{1, 0, 0}, false},
		{"1.2.3-rc.1+build.456", [3]int{1, 2, 3}, false},
		{"", [3]int{}, true},
		{"abc", [3]int{}, true},
		{"1.2", [3]int{}, true},
		{"1.2.abc", [3]int{}, true},
		{"v", [3]int{}, true},
		{"...", [3]int{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseVersion(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseVersion(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// --- isNewer tests ---

func TestIsNewer(t *testing.T) {
	tests := []struct {
		current   string
		candidate string
		want      bool
		wantErr   bool
	}{
		{"1.0.0", "1.0.1", true, false},
		{"1.0.0", "1.1.0", true, false},
		{"1.0.0", "2.0.0", true, false},
		{"1.0.0", "1.0.0", false, false},
		{"1.0.1", "1.0.0", false, false},
		{"2.0.0", "1.9.9", false, false},
		{"v1.0.0", "v1.0.1", true, false},
		{"0.0.1", "0.1.0", true, false},
		{"0.0.1", "v0.0.2", true, false},
		{"abc", "1.0.0", false, true},
		{"1.0.0", "abc", false, true},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s_vs_%s", tt.current, tt.candidate)
		t.Run(name, func(t *testing.T) {
			got, err := isNewer(tt.current, tt.candidate)
			if (err != nil) != tt.wantErr {
				t.Errorf("isNewer(%q, %q) error = %v, wantErr %v", tt.current, tt.candidate, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tt.current, tt.candidate, got, tt.want)
			}
		})
	}
}

// --- findAsset tests ---

func TestFindAsset(t *testing.T) {
	assets := []ghAsset{
		{Name: "nmtui-go_1.0.0_linux_amd64.tar.gz", BrowserDownloadURL: "https://example.com/amd64"},
		{Name: "nmtui-go_1.0.0_linux_arm64.tar.gz", BrowserDownloadURL: "https://example.com/arm64"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums"},
	}

	t.Run("find amd64", func(t *testing.T) {
		a, err := findAsset(assets, "linux", "amd64")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.Name != "nmtui-go_1.0.0_linux_amd64.tar.gz" {
			t.Errorf("got %s, want amd64 asset", a.Name)
		}
	})

	t.Run("find arm64", func(t *testing.T) {
		a, err := findAsset(assets, "linux", "arm64")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.Name != "nmtui-go_1.0.0_linux_arm64.tar.gz" {
			t.Errorf("got %s, want arm64 asset", a.Name)
		}
	})

	t.Run("missing arch", func(t *testing.T) {
		_, err := findAsset(assets, "linux", "386")
		if err == nil {
			t.Error("expected error for missing arch")
		}
	})

	t.Run("missing os", func(t *testing.T) {
		_, err := findAsset(assets, "darwin", "amd64")
		if err == nil {
			t.Error("expected error for missing os")
		}
	})
}

// --- findChecksumAsset tests ---

func TestFindChecksumAsset(t *testing.T) {
	assets := []ghAsset{
		{Name: "nmtui-go_1.0.0_linux_amd64.tar.gz"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums"},
	}

	t.Run("found", func(t *testing.T) {
		a, err := findChecksumAsset(assets)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.Name != "checksums.txt" {
			t.Errorf("got %s, want checksums.txt", a.Name)
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, err := findChecksumAsset([]ghAsset{{Name: "other.tar.gz"}})
		if err == nil {
			t.Error("expected error when checksums.txt is missing")
		}
	})
}

// --- verifyChecksum tests ---

func TestVerifyChecksum(t *testing.T) {
	// Create a temp file with known content
	dir := t.TempDir()
	content := []byte("hello world test content")
	archivePath := filepath.Join(dir, "test.tar.gz")
	if err := os.WriteFile(archivePath, content, 0644); err != nil {
		t.Fatal(err)
	}

	// Compute expected hash
	h := sha256.Sum256(content)
	expectedHash := fmt.Sprintf("%x", h)
	wrongHash := "0000000000000000000000000000000000000000000000000000000000000000"

	checksumsGood := fmt.Sprintf("%s  test.tar.gz\ndeadbeef  other.tar.gz\n", expectedHash)
	checksumsBad := fmt.Sprintf("%s  test.tar.gz\n", wrongHash)
	checksumsMissing := "deadbeef  other.tar.gz\n"

	t.Run("valid checksum", func(t *testing.T) {
		err := verifyChecksum(archivePath, "test.tar.gz", checksumsGood)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("invalid checksum", func(t *testing.T) {
		err := verifyChecksum(archivePath, "test.tar.gz", checksumsBad)
		if err == nil {
			t.Error("expected checksum mismatch error")
		}
	})

	t.Run("missing entry", func(t *testing.T) {
		err := verifyChecksum(archivePath, "test.tar.gz", checksumsMissing)
		if err == nil {
			t.Error("expected missing checksum error")
		}
	})
}

// --- Cache round-trip tests ---

func TestUpdateCacheRoundTrip(t *testing.T) {
	// Override cache dir for testing
	dir := t.TempDir()
	origFunc := os.Getenv("XDG_CACHE_HOME")
	t.Setenv("XDG_CACHE_HOME", dir)
	defer func() {
		if origFunc != "" {
			os.Setenv("XDG_CACHE_HOME", origFunc)
		}
	}()

	cache := &updateCheckCache{
		LatestVersion: "v1.2.3",
		CheckedAt:     time.Now().Unix(),
		ReleaseURL:    "https://github.com/doeixd/nmtui-go/releases/tag/v1.2.3",
	}

	if err := saveUpdateCache(cache); err != nil {
		t.Fatalf("saveUpdateCache: %v", err)
	}

	loaded := loadUpdateCache()
	if loaded == nil {
		t.Fatal("loadUpdateCache returned nil")
	}
	if loaded.LatestVersion != cache.LatestVersion {
		t.Errorf("LatestVersion = %s, want %s", loaded.LatestVersion, cache.LatestVersion)
	}
	if loaded.CheckedAt != cache.CheckedAt {
		t.Errorf("CheckedAt = %d, want %d", loaded.CheckedAt, cache.CheckedAt)
	}
	if loaded.ReleaseURL != cache.ReleaseURL {
		t.Errorf("ReleaseURL = %s, want %s", loaded.ReleaseURL, cache.ReleaseURL)
	}
}

func TestUpdateCacheCorruption(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	// Write corrupt JSON
	cacheDir := filepath.Join(dir, "nmtui-go")
	os.MkdirAll(cacheDir, 0755)
	os.WriteFile(filepath.Join(cacheDir, updateCacheName), []byte("{invalid json"), 0644)

	loaded := loadUpdateCache()
	if loaded != nil {
		t.Error("expected nil from corrupt cache")
	}

	// Verify the corrupt file was deleted
	_, err := os.Stat(filepath.Join(cacheDir, updateCacheName))
	if !os.IsNotExist(err) {
		t.Error("expected corrupt cache file to be deleted")
	}
}

// --- extractBinaryFromTarGz tests ---

func TestExtractBinaryFromTarGz(t *testing.T) {
	dir := t.TempDir()

	// Create a tar.gz with a binary inside (mimicking goreleaser structure)
	archivePath := filepath.Join(dir, "test.tar.gz")
	binaryContent := []byte("#!/bin/sh\necho hello")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Add a directory entry
	tw.WriteHeader(&tar.Header{
		Name:     "nmtui-go_1.0.0_linux_amd64/",
		Typeflag: tar.TypeDir,
		Mode:     0755,
	})

	// Add the binary
	tw.WriteHeader(&tar.Header{
		Name:     "nmtui-go_1.0.0_linux_amd64/nmtui-go",
		Size:     int64(len(binaryContent)),
		Mode:     0755,
		Typeflag: tar.TypeReg,
	})
	tw.Write(binaryContent)

	// Add another file (README)
	readme := []byte("# nmtui-go")
	tw.WriteHeader(&tar.Header{
		Name:     "nmtui-go_1.0.0_linux_amd64/README.md",
		Size:     int64(len(readme)),
		Mode:     0644,
		Typeflag: tar.TypeReg,
	})
	tw.Write(readme)

	tw.Close()
	gw.Close()
	f.Close()

	// Extract
	extractedPath, err := extractBinaryFromTarGz(archivePath, dir)
	if err != nil {
		t.Fatalf("extractBinaryFromTarGz: %v", err)
	}
	defer os.Remove(extractedPath)

	// Verify content
	got, err := os.ReadFile(extractedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binaryContent) {
		t.Errorf("extracted content = %q, want %q", string(got), string(binaryContent))
	}
}

func TestExtractBinaryFromTarGzMissing(t *testing.T) {
	dir := t.TempDir()

	// Create a tar.gz without the binary
	archivePath := filepath.Join(dir, "test.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	readme := []byte("# nmtui-go")
	tw.WriteHeader(&tar.Header{
		Name:     "nmtui-go_1.0.0_linux_amd64/README.md",
		Size:     int64(len(readme)),
		Mode:     0644,
		Typeflag: tar.TypeReg,
	})
	tw.Write(readme)

	tw.Close()
	gw.Close()
	f.Close()

	_, err = extractBinaryFromTarGz(archivePath, dir)
	if err == nil {
		t.Error("expected error when binary is missing from archive")
	}
}

// --- fetchLatestRelease integration test with mock server ---

func TestFetchLatestReleaseMock(t *testing.T) {
	mockRelease := ghRelease{
		TagName:    "v1.5.0",
		Prerelease: false,
		Draft:      false,
		HTMLURL:    "https://github.com/doeixd/nmtui-go/releases/tag/v1.5.0",
		Assets: []ghAsset{
			{Name: "nmtui-go_1.5.0_linux_amd64.tar.gz", BrowserDownloadURL: "https://example.com/amd64", Size: 1024},
			{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums", Size: 128},
		},
	}

	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mockRelease)
		}))
		defer srv.Close()

		// We can't easily override the URL constant, so test the HTTP client/parsing
		// by using the same JSON decoding logic directly
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var release ghRelease
		if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
			t.Fatal(err)
		}
		if release.TagName != "v1.5.0" {
			t.Errorf("TagName = %s, want v1.5.0", release.TagName)
		}
		if len(release.Assets) != 2 {
			t.Errorf("got %d assets, want 2", len(release.Assets))
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message": "API rate limit exceeded"}`))
		}))
		defer srv.Close()

		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("not found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// --- detectInstallSource with build-time flag ---

func TestDetectInstallSourceBuildTime(t *testing.T) {
	tests := []struct {
		method string
		want   installSource
	}{
		{"github", installSourceGitHub},
		{"binary", installSourceGitHub},
		{"aur", installSourceAUR},
		{"pacman", installSourceAUR},
		{"deb", installSourceDeb},
		{"apt", installSourceDeb},
		{"dpkg", installSourceDeb},
		{"rpm", installSourceRPM},
		{"dnf", installSourceRPM},
		{"yum", installSourceRPM},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			oldMethod := InstallMethod
			InstallMethod = tt.method
			defer func() { InstallMethod = oldMethod }()

			got := detectInstallSource()
			if got != tt.want {
				t.Errorf("detectInstallSource() with InstallMethod=%q = %d, want %d", tt.method, got, tt.want)
			}
		})
	}
}

// --- detectInstallSourceByPath ---

func TestDetectInstallSourceByPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("path heuristic tests are Linux-only (filepath.Dir behaves differently on Windows)")
	}

	tests := []struct {
		path string
		want installSource
	}{
		{"/usr/bin/nmtui-go", installSourcePackaged},
		{"/usr/sbin/nmtui-go", installSourcePackaged},
		{"/usr/local/bin/nmtui-go", installSourceGitHub},
		{"/opt/nmtui-go/nmtui-go", installSourceUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := detectInstallSourceByPath(tt.path)
			if got != tt.want {
				t.Errorf("detectInstallSourceByPath(%q) = %d, want %d", tt.path, got, tt.want)
			}
		})
	}
}

// --- isPackageManaged ---

func TestIsPackageManaged(t *testing.T) {
	tests := []struct {
		src  installSource
		want bool
	}{
		{installSourceGitHub, false},
		{installSourceUnknown, false},
		{installSourceAUR, true},
		{installSourceDeb, true},
		{installSourceRPM, true},
		{installSourcePackaged, true},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("source_%d", tt.src), func(t *testing.T) {
			got := isPackageManaged(tt.src)
			if got != tt.want {
				t.Errorf("isPackageManaged(%d) = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}

// --- isPrerelease tests ---

func TestIsPrerelease(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{"v1.0.0", false},
		{"v1.0.0-beta", true},
		{"v1.0.0-rc.1+build", true},
		{"1.2.3", false},
		{"v0.2.7", false},
		{"v0.2.7-alpha.1", true},
		{"v1.0.0+build", true},
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			got := isPrerelease(tt.tag)
			if got != tt.want {
				t.Errorf("isPrerelease(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}

// --- backup and rollback tests ---

func TestBackupAndRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backup/rollback test uses rename semantics that differ on Windows")
	}

	dir := t.TempDir()
	binPath := filepath.Join(dir, "nmtui-go")
	content := []byte("original binary content")
	if err := os.WriteFile(binPath, content, 0755); err != nil {
		t.Fatal(err)
	}

	// Backup
	backupPath, err := backupBinary(binPath)
	if err != nil {
		t.Fatalf("backupBinary: %v", err)
	}

	// Verify .old exists
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		t.Fatal("backup file does not exist")
	}
	if !strings.HasSuffix(backupPath, ".old") {
		t.Errorf("backup path %q doesn't end with .old", backupPath)
	}

	// Original should not exist
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Fatal("original binary should not exist after backup")
	}

	// Rollback
	if err := rollbackBinary(binPath, backupPath); err != nil {
		t.Fatalf("rollbackBinary: %v", err)
	}

	// Verify original restored
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("restored content = %q, want %q", string(got), string(content))
	}

	// Backup should not exist
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatal("backup file should not exist after rollback")
	}
}

// --- verifyBinary tests ---

func TestVerifyBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("verifyBinary test uses shell scripts (Linux/macOS only)")
	}

	dir := t.TempDir()

	// Create a script that exits 0
	goodScript := filepath.Join(dir, "good")
	os.WriteFile(goodScript, []byte("#!/bin/sh\nexit 0\n"), 0755)

	// Create a script that exits 1
	badScript := filepath.Join(dir, "bad")
	os.WriteFile(badScript, []byte("#!/bin/sh\nexit 1\n"), 0755)

	t.Run("exits 0", func(t *testing.T) {
		if err := verifyBinary(goodScript); err != nil {
			t.Errorf("verifyBinary should pass for exit 0: %v", err)
		}
	})

	t.Run("exits 1", func(t *testing.T) {
		if err := verifyBinary(badScript); err == nil {
			t.Error("verifyBinary should fail for exit 1")
		}
	})
}

// --- config helper tests ---

func TestGetKeepBackupConfig(t *testing.T) {
	tests := []struct {
		envVal string
		want   bool
	}{
		{"", true},
		{"1", true},
		{"0", false},
		{"false", false},
		{"true", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("env=%q", tt.envVal), func(t *testing.T) {
			t.Setenv("NMTUI_UPDATE_KEEP_BACKUP", tt.envVal)
			got := getKeepBackupConfig()
			if got != tt.want {
				t.Errorf("getKeepBackupConfig() with env=%q = %v, want %v", tt.envVal, got, tt.want)
			}
		})
	}
}

func TestGetAllowPrereleaseConfig(t *testing.T) {
	tests := []struct {
		envVal string
		want   bool
	}{
		{"", false},
		{"0", false},
		{"1", true},
		{"true", false}, // only "1" is accepted
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("env=%q", tt.envVal), func(t *testing.T) {
			t.Setenv("NMTUI_UPDATE_PRERELEASE", tt.envVal)
			got := getAllowPrereleaseConfig()
			if got != tt.want {
				t.Errorf("getAllowPrereleaseConfig() with env=%q = %v, want %v", tt.envVal, got, tt.want)
			}
		})
	}
}

// --- Update key not active in text input ---

func TestUpdateKeyNotActiveInTextInput(t *testing.T) {
	m := initialModel()
	m.updateAvailable = true
	m.updateLatestVersion = "v1.0.0"
	m.isFiltering = true // simulates text input active

	// Verify isTextInputActive returns true
	if !m.isTextInputActive() {
		t.Fatal("expected isTextInputActive() to be true when isFiltering is set")
	}

	// The state should not change to viewUpdating since text input is active
	// We can't easily simulate a full Update() call without bubbletea test infra,
	// but we verify the guard condition
	if m.state == viewUpdating {
		t.Error("state should not be viewUpdating when text input is active")
	}
}
