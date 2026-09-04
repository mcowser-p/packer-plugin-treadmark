package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaselineFromInfoJSON(t *testing.T) {
	raw := []byte(`{
		"db_path": "/var/lib/treadmark/baseline.db",
		"db_size_bytes": 1048576,
		"db_sha256": "abc123",
		"baseline_created_at": "2026-09-04T10:00:00Z",
		"baseline_host": "packer-build",
		"baseline_os": "linux",
		"tracked_entries": 14231
	}`)
	b, err := BaselineFromInfoJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.SHA256 != "abc123" || b.SizeBytes != 1048576 || b.Entries != 14231 || b.Host != "packer-build" || b.OS != "linux" {
		t.Fatalf("unexpected parse: %+v", b)
	}
}

func TestBaselineFromInfoJSONTolerant(t *testing.T) {
	b, err := BaselineFromInfoJSON([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if b.SHA256 != "" || b.Entries != 0 {
		t.Fatalf("unexpected: %+v", b)
	}
	if _, err := BaselineFromInfoJSON([]byte("not json")); err == nil {
		t.Fatal("expected error on garbage")
	}
}

func TestSidecarRoundTripAndMerge(t *testing.T) {
	dir := t.TempDir()
	sc := &Sidecar{
		Mode:   "in-guest",
		OS:     "linux",
		Custom: map[string]string{"distro": "almalinux"},
	}
	sc.MergeCustom(map[string]string{"distro": "SHOULD-NOT-WIN", "build_number": "20260904"})
	p := filepath.Join(dir, SidecarName)
	if err := sc.Write(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version %d", got.SchemaVersion)
	}
	if got.Custom["distro"] != "almalinux" {
		t.Fatalf("existing key clobbered: %q", got.Custom["distro"])
	}
	if got.Custom["build_number"] != "20260904" {
		t.Fatalf("new key missing: %+v", got.Custom)
	}
}

func TestWriteBundleSums(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "baseline.db"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "x.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := WriteBundleSums(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "  baseline.db\n") || !strings.Contains(s, "  sub/x.json\n") {
		t.Fatalf("sums content:\n%s", s)
	}
	if strings.Contains(s, SumsName) {
		t.Fatal("SHA256SUMS must not list itself")
	}
	// Rewriting after adding a file updates the list and still excludes itself.
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteBundleSums(dir); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(p)
	if !strings.Contains(string(raw), "  metadata.json\n") {
		t.Fatalf("rewritten sums missing metadata.json:\n%s", raw)
	}
}

func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if size != 5 || sum != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("sum=%s size=%d", sum, size)
	}
}
