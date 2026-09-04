package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAssetName(t *testing.T) {
	cases := []struct {
		spec Spec
		want string
		err  bool
	}{
		{Spec{"0.11.0", "deb", "amd64"}, "treadmark_0.11.0_amd64.deb", false},
		{Spec{"0.11.0", "deb", "arm64"}, "treadmark_0.11.0_arm64.deb", false},
		{Spec{"0.11.0", "rpm", "amd64"}, "treadmark-0.11.0-1.x86_64.rpm", false},
		{Spec{"0.11.0", "rpm", "arm64"}, "treadmark-0.11.0-1.aarch64.rpm", false},
		{Spec{"0.11.0", "binary", "amd64"}, "treadmark-linux-x86_64", false},
		{Spec{"0.11.0", "msi", "amd64"}, "treadmark-0.11.0.msi", false},
		{Spec{"0.11.0", "deb", "mips"}, "", true},
		{Spec{"0.11.0", "snap", "amd64"}, "", true},
	}
	for _, tc := range cases {
		got, err := AssetName(tc.spec)
		if (err != nil) != tc.err {
			t.Errorf("%+v: err=%v", tc.spec, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%+v: got %q want %q", tc.spec, got, tc.want)
		}
	}
}

func serveRelease(t *testing.T, asset string, content []byte, sumsLine string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0.11.0/"+asset, func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	})
	mux.HandleFunc("/v0.11.0/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sumsLine)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureDownloadsAndVerifies(t *testing.T) {
	content := []byte("fake-deb-content")
	sum := sha256.Sum256(content)
	asset := "treadmark_0.11.0_amd64.deb"
	srv := serveRelease(t, asset, content, hex.EncodeToString(sum[:])+"  "+asset+"\n")

	c := &Client{BaseURL: srv.URL, CacheDir: t.TempDir()}
	got, err := c.Ensure(context.Background(), Spec{"0.11.0", "deb", "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(got)
	if err != nil || string(raw) != string(content) {
		t.Fatalf("cached file wrong: %v %q", err, raw)
	}
	if filepath.Base(got) != asset {
		t.Fatalf("cache name: %s", got)
	}
	// Second call is a cache hit — nuke the server to prove it.
	srv.Close()
	if _, err := c.Ensure(context.Background(), Spec{"0.11.0", "deb", "amd64"}); err != nil {
		t.Fatalf("cache hit failed: %v", err)
	}
}

func TestEnsureChecksumMismatch(t *testing.T) {
	asset := "treadmark_0.11.0_amd64.deb"
	srv := serveRelease(t, asset, []byte("tampered"), "deadbeef  "+asset+"\n")
	c := &Client{BaseURL: srv.URL, CacheDir: t.TempDir()}
	_, err := c.Ensure(context.Background(), Spec{"0.11.0", "deb", "amd64"})
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	// Nothing may land in the cache on failure.
	entries, _ := os.ReadDir(filepath.Join(c.CacheDir, "v0.11.0"))
	for _, e := range entries {
		if e.Name() == asset {
			t.Fatal("tampered file must not be cached under its final name")
		}
	}
}

func TestEnsureSkipChecksum(t *testing.T) {
	asset := "treadmark_0.11.0_amd64.deb"
	// No SHA256SUMS served at all.
	mux := http.NewServeMux()
	mux.HandleFunc("/v0.11.0/"+asset, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("content"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &Client{BaseURL: srv.URL, CacheDir: t.TempDir()}
	if _, err := c.Ensure(context.Background(), Spec{"0.11.0", "deb", "amd64"}); err == nil {
		t.Fatal("expected failure without SHA256SUMS")
	}
	c.SkipChecksum = true
	if _, err := c.Ensure(context.Background(), Spec{"0.11.0", "deb", "amd64"}); err != nil {
		t.Fatalf("skip-checksum path failed: %v", err)
	}
}

func TestEnsureMissingSumEntry(t *testing.T) {
	asset := "treadmark_0.11.0_amd64.deb"
	srv := serveRelease(t, asset, []byte("content"), "deadbeef  some-other-file\n")
	c := &Client{BaseURL: srv.URL, CacheDir: t.TempDir()}
	if _, err := c.Ensure(context.Background(), Spec{"0.11.0", "deb", "amd64"}); err == nil {
		t.Fatal("expected missing-entry error")
	}
}

func TestResolveLatestRejectsCustomBase(t *testing.T) {
	c := &Client{BaseURL: "https://mirror.example.com/treadmark"}
	if _, err := c.ResolveLatest(context.Background()); err == nil {
		t.Fatal("expected error for non-GitHub base")
	}
	if c.IsDefaultBase() {
		t.Fatal("custom base must not report as default")
	}
	if !(&Client{}).IsDefaultBase() {
		t.Fatal("empty base must report as default")
	}
}
