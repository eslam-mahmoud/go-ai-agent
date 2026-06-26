package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetName(t *testing.T) {
	name := AssetName()
	if !strings.HasPrefix(name, "madar-") {
		t.Errorf("AssetName = %q, want prefix 'madar-'", name)
	}
	parts := strings.Split(name, "-")
	if len(parts) != 3 {
		t.Errorf("AssetName = %q, want 3 dash-separated parts", name)
	}
}

func TestCheck_alreadyUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.0.0",
			"assets":   []any{},
		})
	}))
	defer srv.Close()

	rel, err := checkFrom(context.Background(), "v1.0.0", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		t.Errorf("expected nil when already up to date, got %+v", rel)
	}
}

func TestCheck_noReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rel, err := checkFrom(context.Background(), "dev", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		t.Errorf("expected nil for 404 response, got %+v", rel)
	}
}

func TestCheck_newVersionAvailable(t *testing.T) {
	name := AssetName()

	checksumSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "deadbeef  %s\n", name)
	}))
	defer checksumSrv.Close()

	releaseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.0.0",
			"assets": []map[string]string{
				{"name": name, "browser_download_url": "http://example.com/binary"},
				{"name": "checksums.txt", "browser_download_url": checksumSrv.URL},
			},
		})
	}))
	defer releaseSrv.Close()

	rel, err := checkFrom(context.Background(), "v1.0.0", releaseSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil {
		t.Fatal("expected non-nil release")
	}
	if rel.Version != "v2.0.0" {
		t.Errorf("Version = %q, want v2.0.0", rel.Version)
	}
	if rel.Checksum != "deadbeef" {
		t.Errorf("Checksum = %q, want deadbeef", rel.Checksum)
	}
	if rel.AssetURL != "http://example.com/binary" {
		t.Errorf("AssetURL = %q", rel.AssetURL)
	}
}

func TestCheck_missingPlatformAsset(t *testing.T) {
	releaseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.0.0",
			"assets":   []any{},
		})
	}))
	defer releaseSrv.Close()

	_, err := checkFrom(context.Background(), "v1.0.0", releaseSrv.URL)
	if err == nil {
		t.Fatal("expected error when no asset for current platform")
	}
}

func TestApplyTo_success(t *testing.T) {
	content := []byte("fake madar binary v2")
	h := sha256.Sum256(content)
	checksum := hex.EncodeToString(h[:])

	binarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer binarySrv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "madar")
	// Write a placeholder "old" binary.
	os.WriteFile(dest, []byte("old binary"), 0755)

	rel := &Release{
		Version:  "v2.0.0",
		AssetURL: binarySrv.URL,
		Checksum: checksum,
	}

	if err := applyTo(context.Background(), rel, dest); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(dest)
	if string(got) != string(content) {
		t.Errorf("binary content after update = %q, want %q", got, content)
	}
}

func TestApplyTo_checksumMismatch(t *testing.T) {
	binarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake binary"))
	}))
	defer binarySrv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "madar")
	os.WriteFile(dest, []byte("old binary"), 0755)

	rel := &Release{
		Version:  "v2.0.0",
		AssetURL: binarySrv.URL,
		Checksum: "0000000000000000000000000000000000000000000000000000000000000000",
	}

	if err := applyTo(context.Background(), rel, dest); err == nil {
		t.Fatal("expected checksum mismatch error")
	}

	// Old binary must still be in place.
	got, _ := os.ReadFile(dest)
	if string(got) != "old binary" {
		t.Errorf("old binary was clobbered on checksum failure")
	}
}

func TestApplyTo_noChecksum(t *testing.T) {
	content := []byte("new binary no checksum")
	binarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer binarySrv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "madar")
	os.WriteFile(dest, []byte("old"), 0755)

	rel := &Release{Version: "v2.0.0", AssetURL: binarySrv.URL}
	if err := applyTo(context.Background(), rel, dest); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(content) {
		t.Errorf("binary not updated: got %q", got)
	}
}
