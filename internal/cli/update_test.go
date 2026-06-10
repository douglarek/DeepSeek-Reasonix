package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalizeCLIVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{"v1.5.0", "v1.5.0", true},
		{"1.5.0", "v1.5.0", true},
		{"v1.5.0-rc.1", "v1.5.0-rc.1", true},
		{"dev", "", false},
		{"", "", false},
		{"abc", "vabc", false},
		{"v1", "v1", false},
		{"v1.2", "v1.2", false},
	}
	for _, tt := range tests {
		got, ok := normalizeCLIVersion(tt.input)
		if ok != tt.ok || got != tt.want {
			t.Errorf("normalizeCLIVersion(%q) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.5.0", "v1.5.0", 0},
		{"v1.5.0", "v1.6.0", -1},
		{"v1.6.0", "v1.5.0", 1},
		{"v1.5.0", "v2.0.0", -1},
		{"v0.9.7", "v1.0.0", -1},
		{"v1.0.0", "v0.9.7", 1},
		{"v1.5.0", "v1.5.1", -1},
	}
	for _, tt := range tests {
		got := compareSemver(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestPlatformAssetName(t *testing.T) {
	name, err := platformAssetName()
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "reasonix-" + runtime.GOOS + "-" + runtime.GOARCH
	if got := name[:len(wantPrefix)]; got != wantPrefix {
		t.Errorf("platformAssetName() prefix = %q, want %q", got, wantPrefix)
	}
	if runtime.GOOS == "windows" {
		if name[len(name)-4:] != ".zip" {
			t.Errorf("windows asset should end with .zip, got %q", name)
		}
	} else {
		if name[len(name)-7:] != ".tar.gz" {
			t.Errorf("unix asset should end with .tar.gz, got %q", name)
		}
	}
}

func TestSHA256ForFile(t *testing.T) {
	sums := "abcdef1234567890  reasonix-linux-amd64.tar.gz\n1122334455667788  reasonix-darwin-arm64.tar.gz\n"
	got, err := sha256ForFile(sums, "reasonix-linux-amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "abcdef1234567890" {
		t.Errorf("got %q, want abcdef1234567890", got)
	}

	_, err = sha256ForFile(sums, "reasonix-linux-arm64.tar.gz")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestExtractFromTarGz(t *testing.T) {
	binContent := []byte("fake-binary-content")

	// Build a tar.gz in memory.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{Name: "reasonix", Size: int64(len(binContent)), Mode: 0o755, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binContent); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := extractFromTarGz(buf.Bytes(), "reasonix")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binContent) {
		t.Errorf("extracted content mismatch")
	}

	_, err = extractFromTarGz(buf.Bytes(), "nonexistent")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reasonix")

	// Create initial file.
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Replace.
	if err := atomicReplace(target, []byte("new")); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("got %q, want 'new'", got)
	}

	// Verify no temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "reasonix" {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}
}

func TestIsCLITag(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{"v1.6.0", true},
		{"v0.1.0", true},
		{"v2.0.0-rc.1", true},
		{"desktop-v1.5.0", false},
		{"npm-v1.4.0", false},
		{"", false},
		{"v", false},
	}
	for _, tt := range tests {
		if got := isCLITag(tt.tag); got != tt.want {
			t.Errorf("isCLITag(%q) = %v, want %v", tt.tag, got, tt.want)
		}
	}
}

func TestFetchLatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"tag_name":"desktop-v1.5.0","assets":[]},
			{"tag_name":"npm-v1.4.0","assets":[]},
			{"tag_name":"v1.6.0","assets":[
				{"name":"reasonix-linux-amd64.tar.gz","browser_download_url":"http://example.com/reasonix-linux-amd64.tar.gz","size":12345},
				{"name":"SHA256SUMS","browser_download_url":"http://example.com/SHA256SUMS","size":100}
			]}
		]`)
	}))
	defer srv.Close()

	// Test the fetcher directly against our test server.
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rels []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		t.Fatal(err)
	}

	// Simulate the filtering logic from fetchLatestRelease.
	var rel *ghRelease
	for i := range rels {
		if isCLITag(rels[i].TagName) {
			rel = &rels[i]
			break
		}
	}
	if rel == nil {
		t.Fatal("no CLI release found")
	}
	if rel.TagName != "v1.6.0" {
		t.Errorf("TagName = %q, want v1.6.0", rel.TagName)
	}
	if len(rel.Assets) != 2 {
		t.Errorf("len(Assets) = %d, want 2", len(rel.Assets))
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{500, "500 B"},
		{2048, "2.0 KiB"},
		{19_000_000, "18.1 MiB"},
	}
	for _, tt := range tests {
		got := humanSize(tt.bytes)
		if got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestNormalizeTag(t *testing.T) {
	if got := normalizeTag("v1.0.0"); got != "v1.0.0" {
		t.Errorf("normalizeTag('v1.0.0') = %q", got)
	}
	if got := normalizeTag("1.0.0"); got != "v1.0.0" {
		t.Errorf("normalizeTag('1.0.0') = %q", got)
	}
}

func TestSHA256SumsURL(t *testing.T) {
	got := sha256SumsURL("v1.6.0")
	want := "https://github.com/esengine/DeepSeek-Reasonix/releases/download/v1.6.0/SHA256SUMS"
	if got != want {
		t.Errorf("sha256SumsURL = %q, want %q", got, want)
	}
}
