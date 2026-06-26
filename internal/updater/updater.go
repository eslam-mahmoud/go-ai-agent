package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
)

const (
	repoOwner   = "eslam-mahmoud"
	repoName    = "go-ai-agent"
	releasesAPI = "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/releases/latest"
)

// Release describes a newer version available for download.
type Release struct {
	Version  string // e.g. "v0.2.0"
	AssetURL string // direct download URL for this platform's binary
	Checksum string // sha256 hex from checksums.txt (empty if not published)
}

// AssetName returns the release asset filename for the current platform.
func AssetName() string {
	return fmt.Sprintf("madar-%s-%s", runtime.GOOS, runtime.GOARCH)
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// Check queries the GitHub Releases API for the latest release and returns
// a non-nil Release when a version newer than currentVersion is available.
// Returns nil if already up-to-date or if no releases exist.
func Check(ctx context.Context, currentVersion string) (*Release, error) {
	return checkFrom(ctx, currentVersion, releasesAPI)
}

// checkFrom is the testable inner implementation that accepts a custom API URL.
func checkFrom(ctx context.Context, currentVersion, apiURL string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "madar-updater/1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no releases published yet
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API returned HTTP %d", resp.StatusCode)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release JSON: %w", err)
	}

	if rel.TagName == currentVersion {
		return nil, nil // already on latest
	}

	name := AssetName()
	var assetURL, checksumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case name:
			assetURL = a.BrowserDownloadURL
		case "checksums.txt":
			checksumURL = a.BrowserDownloadURL
		}
	}

	if assetURL == "" {
		return nil, fmt.Errorf("release %s has no asset for %s", rel.TagName, name)
	}

	var checksum string
	if checksumURL != "" {
		checksum, err = fetchChecksum(ctx, checksumURL, name)
		if err != nil {
			return nil, fmt.Errorf("fetch checksum: %w", err)
		}
	}

	return &Release{
		Version:  rel.TagName,
		AssetURL: assetURL,
		Checksum: checksum,
	}, nil
}

// Apply downloads rel.AssetURL, verifies sha256 against rel.Checksum (if set),
// and atomically replaces the currently running binary.
func Apply(ctx context.Context, rel *Release) error {
	cur, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	return applyTo(ctx, rel, cur)
}

// applyTo is the testable inner implementation that writes to destPath.
func applyTo(ctx context.Context, rel *Release, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.AssetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "madar-updater/1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	tmp := destPath + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write binary: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	if rel.Checksum != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != rel.Checksum {
			os.Remove(tmp)
			return fmt.Errorf("checksum mismatch: want %s got %s", rel.Checksum, got)
		}
	}

	if err := os.Rename(tmp, destPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

func fetchChecksum(ctx context.Context, url, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "madar-updater/1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s in checksums.txt", assetName)
}
