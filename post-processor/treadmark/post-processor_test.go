package treadmark

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/artifact"
	"github.com/mcowser-p/packer-plugin-treadmark/internal/metadata"
)

func configure(t *testing.T, raw map[string]interface{}) (*PostProcessor, error) {
	t.Helper()
	p := &PostProcessor{}
	return p, p.Configure(raw)
}

func testUi() packersdk.Ui {
	return &packersdk.BasicUi{
		Reader:      strings.NewReader(""),
		Writer:      io.Discard,
		ErrorWriter: io.Discard,
	}
}

type fakeArtifact struct {
	files []string
	id    string
}

func (f *fakeArtifact) BuilderId() string { return "test.builder" }
func (f *fakeArtifact) Files() []string   { return f.files }
func (f *fakeArtifact) Id() string        { return f.id }
func (f *fakeArtifact) String() string    { return "fake artifact" }
func (f *fakeArtifact) State(name string) interface{} {
	if name == "generated_data" {
		return map[string]interface{}{"SourceImageName": "x"}
	}
	return nil
}
func (f *fakeArtifact) Destroy() error { return nil }

func TestConfigureDefaultsAndErrors(t *testing.T) {
	p, err := configure(t, map[string]interface{}{"packer_build_name": "alma10"})
	if err != nil {
		t.Fatal(err)
	}
	c := p.config
	if c.Mode != "collect" || c.OnDrift != "error" || c.OfflineStrategy != "guestmount" || c.LibguestfsBackend != "direct" {
		t.Fatalf("defaults: %+v", c)
	}
	if c.OutputDir != filepath.Join("treadmark-output", "alma10") {
		t.Fatalf("output_dir: %s", c.OutputDir)
	}

	cases := []struct {
		name string
		raw  map[string]interface{}
		want string
	}{
		{"bad-mode", map[string]interface{}{"mode": "both"}, "mode must be"},
		{"offline-needs-config", map[string]interface{}{"mode": "offline"}, "requires config_source"},
		{"collect-rejects-offline-opts", map[string]interface{}{"image_path": "/x.qcow2"}, "only used in offline mode"},
		{"s3-needs-bucket", map[string]interface{}{"s3": []map[string]interface{}{{}}}, "bucket is required"},
		{"bad-sse", map[string]interface{}{"s3": []map[string]interface{}{{"bucket": "b", "sse": "rot13"}}}, "sse must be"},
		{"kms-needs-sse", map[string]interface{}{"s3": []map[string]interface{}{{"bucket": "b", "kms_key_id": "k"}}}, "needs sse"},
		{"bad-strategy", map[string]interface{}{"offline_strategy": "virt-copy-out"}, "only \"guestmount\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := configure(t, tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestConfigureS3PrefixDefault(t *testing.T) {
	p, err := configure(t, map[string]interface{}{
		"s3": []map[string]interface{}{{"bucket": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.config.S3[0].Prefix != "treadmark/" {
		t.Fatalf("prefix default: %q", p.config.S3[0].Prefix)
	}
}

func writeProvisionerBundle(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "baseline.db"), []byte("DBDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := &metadata.Sidecar{
		Mode:   "in-guest",
		OS:     "linux",
		Scope:  "files",
		Custom: map[string]string{"distro": "almalinux"},
	}
	if err := sc.Write(filepath.Join(dir, metadata.SidecarName)); err != nil {
		t.Fatal(err)
	}
}

func TestPostProcessCollect(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	writeProvisionerBundle(t, dir)
	imgDir := t.TempDir()
	img := filepath.Join(imgDir, "alma10.qcow2")
	if err := os.WriteFile(img, []byte("QCOW"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := configure(t, map[string]interface{}{
		"output_dir": dir,
		"metadata":   map[string]string{"build_number": "20260904", "distro": "SHOULD-NOT-WIN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeArtifact{files: []string{img}, id: "alma10"}
	art, keep, force, err := p.PostProcess(context.Background(), testUi(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !keep || !force {
		t.Fatal("post-processor must force-keep the input artifact")
	}
	if art.BuilderId() != artifact.BuilderID {
		t.Fatalf("builder id: %s", art.BuilderId())
	}

	files := art.Files()
	joined := strings.Join(files, "\n")
	for _, want := range []string{img, "baseline.db", "metadata.json", "SHA256SUMS"} {
		if !strings.Contains(joined, want) {
			t.Errorf("artifact files missing %s:\n%s", want, joined)
		}
	}
	assertFilesListedOnce(t, files)
	if !strings.HasPrefix(art.Id(), "alma10-treadmark-") {
		t.Fatalf("artifact id: %s", art.Id())
	}
	if art.State("generated_data") == nil {
		t.Fatal("generated_data not forwarded")
	}

	sc, err := metadata.Load(filepath.Join(dir, metadata.SidecarName))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Custom["distro"] != "almalinux" {
		t.Fatal("provisioner-recorded custom key must win")
	}
	if sc.Custom["build_number"] != "20260904" {
		t.Fatal("post-processor custom key missing")
	}
	if sc.Baseline == nil || sc.Baseline.SHA256 == "" {
		t.Fatal("baseline sha not computed")
	}
	if _, err := os.Stat(filepath.Join(dir, metadata.SumsName)); err != nil {
		t.Fatal("SHA256SUMS not written")
	}

	// The sums cover the final metadata.json.
	sums, _ := os.ReadFile(filepath.Join(dir, metadata.SumsName))
	sum, _, _ := metadata.FileSHA256(filepath.Join(dir, metadata.SidecarName))
	if !strings.Contains(string(sums), sum+"  metadata.json") {
		t.Fatal("SHA256SUMS does not match the final metadata.json")
	}
}

// assertFilesListedOnce fails if any path (or basename — the manifest
// post-processor's strip_path collapses to basenames) appears more than once.
func assertFilesListedOnce(t *testing.T, files []string) {
	t.Helper()
	byBase := map[string]int{}
	for _, f := range files {
		byBase[filepath.Base(f)]++
	}
	for base, n := range byBase {
		if n != 1 {
			t.Errorf("file %s listed %d times in artifact files: %v", base, n, files)
		}
	}
}

// The observed holy-qcow duplication: when the input artifact already lists
// the bundle files (e.g. a second treadmark post-processor over the same
// output_dir), each bundle file must still appear exactly once in Files().
func TestPostProcessCollectNoDuplicateBundleFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	writeProvisionerBundle(t, dir)
	img := filepath.Join(t.TempDir(), "alma10-20260904.qcow2")
	if err := os.WriteFile(img, []byte("QCOW"), 0o644); err != nil {
		t.Fatal(err)
	}

	p1, err := configure(t, map[string]interface{}{"output_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	first, _, _, err := p1.PostProcess(context.Background(), testUi(), &fakeArtifact{files: []string{img}, id: "alma10"})
	if err != nil {
		t.Fatal(err)
	}
	assertFilesListedOnce(t, first.Files())

	p2, err := configure(t, map[string]interface{}{"output_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := p2.PostProcess(context.Background(), testUi(), first)
	if err != nil {
		t.Fatal(err)
	}
	files := second.Files()
	assertFilesListedOnce(t, files)
	if len(files) != len(first.Files()) {
		t.Errorf("chained run changed the file list:\nfirst:  %v\nsecond: %v", first.Files(), files)
	}
	joined := strings.Join(files, "\n")
	for _, want := range []string{img, "baseline.db", "metadata.json", "SHA256SUMS"} {
		if !strings.Contains(joined, want) {
			t.Errorf("artifact files missing %s:\n%s", want, joined)
		}
	}
}

func TestPostProcessCollectMissingBundle(t *testing.T) {
	p, err := configure(t, map[string]interface{}{
		"output_dir": filepath.Join(t.TempDir(), "nope"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, keep, force, err := p.PostProcess(context.Background(), testUi(), &fakeArtifact{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want bundle-dir error, got %v", err)
	}
	if !keep || !force {
		t.Fatal("input must be kept even on error")
	}
}

func TestPostProcessCollectMissingBaseline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "something.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _ := configure(t, map[string]interface{}{"output_dir": dir})
	_, _, _, err := p.PostProcess(context.Background(), testUi(), &fakeArtifact{})
	if err == nil || !strings.Contains(err.Error(), "no baseline.db") {
		t.Fatalf("want baseline error, got %v", err)
	}

	p2, _ := configure(t, map[string]interface{}{"output_dir": dir, "allow_missing_baseline": true})
	if _, _, _, err := p2.PostProcess(context.Background(), testUi(), &fakeArtifact{}); err != nil {
		t.Fatalf("allow_missing_baseline should pass: %v", err)
	}
}

func TestPostProcessCollectShaMismatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	writeProvisionerBundle(t, dir)
	sc, _ := metadata.Load(filepath.Join(dir, metadata.SidecarName))
	sc.Baseline = &metadata.Baseline{SHA256: strings.Repeat("f", 64)}
	if err := sc.Write(filepath.Join(dir, metadata.SidecarName)); err != nil {
		t.Fatal(err)
	}
	p, _ := configure(t, map[string]interface{}{"output_dir": dir})
	_, _, _, err := p.PostProcess(context.Background(), testUi(), &fakeArtifact{})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want sha mismatch error, got %v", err)
	}
}

func TestRewriteConfig(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "golden.yaml")
	yaml := `db_path: /var/lib/treadmark/baseline.db
paths:
  - /etc
  - /usr/bin
exclude:
  - /etc/mtab
store_content: true
report:
  path: /var/lib/treadmark/reports/latest.json
`
	if err := os.WriteFile(src, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := configure(t, map[string]interface{}{
		"mode":          "offline",
		"config_source": src,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.rewriteConfig(dir, "/host/out/baseline.db")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("temp config is not valid JSON: %v\n%s", err, raw)
	}
	if m["db_path"] != "/host/out/baseline.db" {
		t.Fatalf("db_path: %v", m["db_path"])
	}
	if _, has := m["report"]; has {
		t.Fatal("report block must be stripped")
	}
	paths, ok := m["paths"].([]interface{})
	if !ok || len(paths) != 2 || paths[0] != "/etc" {
		t.Fatalf("paths mangled: %v", m["paths"])
	}
	if m["store_content"] != true {
		t.Fatalf("store_content mangled: %v", m["store_content"])
	}
}

func TestOfflineEnvExtraEnvWins(t *testing.T) {
	p, err := configure(t, map[string]interface{}{
		"mode":          "offline",
		"config_source": writeTempYAML(t),
		"extra_env": map[string]string{
			"SUPERMIN_KERNEL":  "/custom/kernel",
			"SUPERMIN_MODULES": "/custom/modules",
			"EXTRA_FLAG":       "1",
		},
		"libguestfs_backend": "libvirt",
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := p.offlineEnv()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"LIBGUESTFS_BACKEND=libvirt",
		"SUPERMIN_KERNEL=/custom/kernel",
		"SUPERMIN_MODULES=/custom/modules",
		"EXTRA_FLAG=1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %s", want)
		}
	}
	// extra_env must come after any auto-pinned values so it wins.
	if strings.LastIndex(joined, "SUPERMIN_KERNEL=") != strings.Index(joined, "SUPERMIN_KERNEL=/custom/kernel") {
		t.Error("extra_env SUPERMIN_KERNEL must be the last occurrence")
	}
}

func writeTempYAML(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("paths:\n  - /etc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
