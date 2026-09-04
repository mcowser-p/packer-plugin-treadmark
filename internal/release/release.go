// Package release resolves and downloads treadmark release assets from
// GitHub releases (or an air-gapped mirror), verifies them against the
// release's SHA256SUMS, and caches them on the build host.
package release

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBaseURL is the release download base for treadmark. Asset URLs are
// formed as <base>/v<version>/<asset>.
const DefaultBaseURL = "https://github.com/mcowser-p/treadmark/releases/download"

const sumsAsset = "SHA256SUMS"

// Spec identifies one downloadable asset.
type Spec struct {
	Version string // resolved version, no leading "v"
	Method  string // deb | rpm | binary | msi
	Arch    string // guest arch, Go-style: amd64 | arm64
}

// AssetName maps a Spec to the asset filename treadmark's release pipeline
// publishes (verified against treadmark scripts/build-*.sh).
func AssetName(s Spec) (string, error) {
	switch s.Method {
	case "deb":
		if s.Arch != "amd64" && s.Arch != "arm64" {
			return "", fmt.Errorf("unsupported deb arch %q", s.Arch)
		}
		return fmt.Sprintf("treadmark_%s_%s.deb", s.Version, s.Arch), nil
	case "rpm":
		a := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[s.Arch]
		if a == "" {
			return "", fmt.Errorf("unsupported rpm arch %q", s.Arch)
		}
		return fmt.Sprintf("treadmark-%s-1.%s.rpm", s.Version, a), nil
	case "binary":
		a := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[s.Arch]
		if a == "" {
			return "", fmt.Errorf("unsupported binary arch %q", s.Arch)
		}
		return "treadmark-linux-" + a, nil
	case "msi":
		return fmt.Sprintf("treadmark-%s.msi", s.Version), nil
	default:
		return "", fmt.Errorf("unknown install method %q", s.Method)
	}
}

// Client downloads and caches release assets.
type Client struct {
	// BaseURL is the release download base (see DefaultBaseURL).
	BaseURL string
	// CacheDir holds downloaded assets; defaults to
	// $PACKER_CACHE_DIR/treadmark or the user cache dir.
	CacheDir string
	// SkipChecksum disables SHA256SUMS verification.
	SkipChecksum bool
	// HTTP is the client used for all requests; defaults to a 5-minute
	// timeout client.
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

// IsDefaultBase reports whether the client points at the canonical GitHub
// releases URL (the only base "latest" can be resolved against).
func (c *Client) IsDefaultBase() bool {
	return c.base() == DefaultBaseURL
}

func (c *Client) cacheDir() (string, error) {
	if c.CacheDir != "" {
		return c.CacheDir, nil
	}
	if d := os.Getenv("PACKER_CACHE_DIR"); d != "" {
		return filepath.Join(d, "treadmark"), nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "packer-plugin-treadmark"), nil
}

// ResolveLatest resolves the "latest" release tag via the GitHub API. It
// requires the default base URL (owner/repo are parsed from it) and honors
// GITHUB_TOKEN / GH_TOKEN for rate limits.
func (c *Client) ResolveLatest(ctx context.Context) (string, error) {
	u, err := url.Parse(c.base())
	if err != nil {
		return "", err
	}
	// Expect path: /<owner>/<repo>/releases/download
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Host != "github.com" || len(parts) < 2 {
		return "", fmt.Errorf("treadmark_version = \"latest\" needs the default GitHub download_url_base; pin an explicit version when using a mirror")
	}
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", parts[0], parts[1])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := firstEnv("GITHUB_TOKEN", "GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", fmt.Errorf("resolving latest treadmark release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving latest treadmark release: GitHub API returned %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(body.TagName, "v")
	if v == "" {
		return "", fmt.Errorf("latest release has no tag name")
	}
	return v, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Ensure returns a local path to the requested asset, downloading and
// verifying it if not already cached. Concurrent builds are safe: downloads
// land in a temp file and are moved into place atomically.
func (c *Client) Ensure(ctx context.Context, s Spec) (string, error) {
	asset, err := AssetName(s)
	if err != nil {
		return "", err
	}
	dir, err := c.cacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "v"+s.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, asset)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}

	tmp, err := os.CreateTemp(dir, "."+asset+".tmp-*")
	if err != nil {
		return "", err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	assetURL := fmt.Sprintf("%s/v%s/%s", c.base(), s.Version, asset)
	if err := c.fetch(ctx, assetURL, tmp); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	if !c.SkipChecksum {
		want, err := c.expectedSum(ctx, s.Version, asset)
		if err != nil {
			return "", err
		}
		got, err := fileSHA256(tmp.Name())
		if err != nil {
			return "", err
		}
		if got != want {
			return "", fmt.Errorf("checksum mismatch for %s: SHA256SUMS says %s, downloaded file is %s", asset, want, got)
		}
	}

	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (c *Client) fetch(ctx context.Context, rawURL string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: %s", rawURL, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// expectedSum downloads the release's SHA256SUMS and returns the digest
// recorded for asset.
func (c *Client) expectedSum(ctx context.Context, version, asset string) (string, error) {
	var buf strings.Builder
	sumsURL := fmt.Sprintf("%s/v%s/%s", c.base(), version, sumsAsset)
	if err := c.fetch(ctx, sumsURL, &buf); err != nil {
		return "", fmt.Errorf("fetching SHA256SUMS (set disable_checksum_verify = true to skip): %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(name) == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS for v%s has no entry for %s", version, asset)
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
