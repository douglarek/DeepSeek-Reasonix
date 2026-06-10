// Package cli implements the `reasonix update` subcommand: it checks GitHub
// Releases for a newer version, downloads the platform-matched archive,
// verifies its SHA-256 checksum, and atomically replaces the running binary.
package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/netclient"
)

const (
	ghOwner = "esengine"
	ghRepo  = "DeepSeek-Reasonix"
	ghAPI   = "https://api.github.com/repos/" + ghOwner + "/" + ghRepo + "/releases"
)

// ghRelease is the subset of the GitHub Release API response we need.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []ghAsset
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// updateCommand implements `reasonix update [--check] [--force]`.
func updateCommand(args []string, currentVersion string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "check for updates without installing")
	force := fs.Bool("force", false, "update even if already on the latest version")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cur, okCur := normalizeCLIVersion(currentVersion)
	if !okCur {
		fmt.Fprintf(os.Stderr, "cannot update a dev/unreleased build (%s)\n", currentVersion)
		return 1
	}

	cfg, _ := config.Load()
	client := httpClient(cfg)

	fmt.Printf("current: %s\n", currentVersion)

	rel, err := fetchLatestRelease(context.Background(), client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: check latest release: %v\n", err)
		return 1
	}

	latest := normalizeTag(rel.TagName)
	_, okLatest := normalizeCLIVersion(latest)
	if !okLatest {
		fmt.Fprintf(os.Stderr, "error: latest release tag %q is not valid semver\n", rel.TagName)
		return 1
	}

	fmt.Printf("latest:  %s\n", latest)

	if !okCur || !okLatest {
		// dev build, already printed error above for okCur; this guards okLatest.
		return 1
	}

	if compareSemver(latest, cur) <= 0 {
		if *force {
			fmt.Println("forcing reinstall of the same version…")
		} else {
			fmt.Println("reasonix is already up to date.")
			return 0
		}
	}

	if *checkOnly {
		return 0
	}

	assetName, err := platformAssetName()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Find the asset in the release.
	var asset *ghAsset
	for i := range rel.Assets {
		if rel.Assets[i].Name == assetName {
			asset = &rel.Assets[i]
			break
		}
	}
	if asset == nil {
		fmt.Fprintf(os.Stderr, "error: release %s does not contain %s\n", latest, assetName)
		return 1
	}

	// Download the archive.
	fmt.Printf("downloading %s (%s)…\n", asset.Name, humanSize(asset.Size))
	archiveData, err := httpGet(context.Background(), client, asset.BrowserDownloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: download %s: %v\n", asset.BrowserDownloadURL, err)
		return 1
	}

	// Download and parse SHA256SUMS.
	fmt.Println("verifying SHA-256…")
	sumsData, err := httpGet(context.Background(), client, sha256SumsURL(latest))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not download SHA256SUMS: %v — skipping checksum verification\n", err)
	} else {
		want, err := sha256ForFile(string(sumsData), asset.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v — skipping checksum verification\n", err)
		} else {
			got := fmt.Sprintf("%x", sha256.Sum256(archiveData))
			if !strings.EqualFold(got, want) {
				fmt.Fprintf(os.Stderr, "error: SHA-256 mismatch: got %s, want %s\n", got, want)
				return 1
			}
		}
	}

	// Extract the binary from the archive.
	binaryName := "reasonix"
	if runtime.GOOS == "windows" {
		binaryName = "reasonix.exe"
	}

	binData, err := extractBinaryFromArchive(archiveData, assetName, binaryName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: extract %s from archive: %v\n", binaryName, err)
		return 1
	}

	// Determine the target path (the running executable).
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: locate running executable: %v\n", err)
		return 1
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve executable path: %v\n", err)
		return 1
	}

	// Atomic replace: write to a temp file in the same directory, then rename.
	fmt.Printf("installing to %s…\n", exePath)
	if err := atomicReplace(exePath, binData); err != nil {
		fmt.Fprintf(os.Stderr, "error: replace binary: %v\n", err)
		return 1
	}

	fmt.Printf("\nreasonix updated: %s → %s\n", currentVersion, latest)
	return 0
}

// httpClient returns an *http.Client that respects the user's proxy config.
func httpClient(cfg *config.Config) *http.Client {
	if cfg != nil {
		if c, err := netclient.NewHTTPClient(cfg.NetworkProxySpec(), netclient.TransportOptions{}); err == nil {
			return c
		}
	}
	return &http.Client{}
}

// isCLITag reports whether a tag belongs to the CLI release namespace (v*).
// Tags like "desktop-v1.5.0" or "npm-v1.4.0" are excluded.
func isCLITag(tag string) bool {
	tag = strings.TrimSpace(tag)
	return len(tag) >= 2 && tag[0] == 'v' && tag[1] >= '0' && tag[1] <= '9'
}

// fetchLatestRelease queries the GitHub Releases API and returns the newest
// release whose tag belongs to the CLI namespace (v*). Tags with a prefix such
// as "desktop-v" or "npm-v" are skipped.
func fetchLatestRelease(ctx context.Context, client *http.Client) (*ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ghAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", ghAPI, resp.Status)
	}
	var rels []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}
	for i := range rels {
		if isCLITag(rels[i].TagName) {
			return &rels[i], nil
		}
	}
	return nil, fmt.Errorf("no CLI release (v*) found in recent releases")
}

// httpGet fetches a URL fully into memory.
func httpGet(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// platformAssetName returns the GoReleaser archive name for the running platform.
// Matches the name_template: "reasonix-{{ .Os }}-{{ .Arch }}" with tar.gz (unix)
// or zip (windows).
func platformAssetName() (string, error) {
	osName := runtime.GOOS // darwin, linux, windows
	arch := runtime.GOARCH // amd64, arm64
	ext := "tar.gz"
	if osName == "windows" {
		ext = "zip"
	}
	if osName == "" || arch == "" {
		return "", fmt.Errorf("unsupported platform: %s/%s", osName, arch)
	}
	return fmt.Sprintf("reasonix-%s-%s.%s", osName, arch, ext), nil
}

// sha256SumsURL returns the download URL for the SHA256SUMS file of a release.
func sha256SumsURL(tag string) string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/SHA256SUMS", ghOwner, ghRepo, tag)
}

// sha256ForFile parses a SHA256SUMS body (lines of "<hex>  <name>") and returns
// the hex digest for the given file name.
func sha256ForFile(sums, name string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("%s not listed in SHA256SUMS", name)
}

// extractBinaryFromArchive extracts a named regular file from a .tar.gz or
// .zip archive.
func extractBinaryFromArchive(data []byte, archiveName, binaryName string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractFromZip(data, binaryName)
	}
	return extractFromTarGz(data, binaryName)
}

func extractFromTarGz(data []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && (h.Name == name || strings.HasSuffix(h.Name, "/"+name)) {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("%q not found in tar.gz archive", name)
}

func extractFromZip(data []byte, name string) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(f.Name)
		if base == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("%q not found in zip archive", name)
}

// atomicReplace writes data to a temp file next to target, then renames it
// into place. The temp file is cleaned up on failure.
func atomicReplace(target string, data []byte) error {
	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, ".update-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	success := false
	defer func() {
		if !success {
			os.Remove(tmp)
		}
	}()

	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	success = true
	return nil
}

// --- version helpers ---

// normalizeCLIVersion canonicalizes a version string to "vX.Y.Z" (pre-release
// suffixes like "-rc.1" are preserved). Returns ok=false for "dev" or anything
// that doesn't start with a valid major.minor.patch.
func normalizeCLIVersion(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return "", false
	}
	v = normalizeTag(v)
	// Split off any pre-release suffix (e.g. "v1.5.0-rc.1" → core "1.5.0").
	core := strings.TrimPrefix(v, "v")
	if i := strings.Index(core, "-"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) < 3 {
		return v, false
	}
	for _, p := range parts[:3] {
		if p == "" {
			return v, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return v, false
			}
		}
	}
	return v, true
}

// normalizeTag ensures the version starts with "v".
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return tag
}

// compareSemver compares two "vX.Y.Z" strings numerically. Returns -1, 0, or 1.
func compareSemver(a, b string) int {
	return compareParts(parseSemver(a), parseSemver(b))
}

func parseSemver(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		if i >= 3 {
			break
		}
		n := 0
		for _, c := range p {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			} else {
				break
			}
		}
		out[i] = n
	}
	return out
}

func compareParts(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// humanSize returns a human-readable byte size.
func humanSize(bytes int64) string {
	const (
		_KiB = 1024
		_MiB = 1024 * _KiB
	)
	switch {
	case bytes >= _MiB:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(_MiB))
	case bytes >= _KiB:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/float64(_KiB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
