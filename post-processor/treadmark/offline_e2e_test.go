package treadmark

// Local-only end-to-end: offline capture against a real qcow2 with real
// guestmount + treadmark. Needs a KVM/libguestfs host. Run via:
//
//	make e2e-offline QCOW2=/path/to/image.qcow2 [TREADMARK_BIN=/path/to/treadmark]
//
// Gated on PACKER_ACC=1 + TREADMARK_E2E_QCOW2 so CI (no KVM on GitHub
// runners) never attempts it.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/metadata"
)

func TestOfflineE2E(t *testing.T) {
	if os.Getenv("PACKER_ACC") == "" {
		t.Skip("PACKER_ACC not set")
	}
	qcow2 := os.Getenv("TREADMARK_E2E_QCOW2")
	if qcow2 == "" {
		t.Skip("TREADMARK_E2E_QCOW2 not set")
	}
	if _, err := os.Stat(qcow2); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg := filepath.Join(dir, "golden.yaml")
	// Small path set: fast walk, still meaningful (packages + config).
	if err := os.WriteFile(cfg, []byte(`paths:
  - /etc
  - /usr/bin
exclude:
  - /etc/mtab
  - /etc/resolv.conf
  - /etc/machine-id
  - /etc/hostname
store_content: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	raw := map[string]interface{}{
		"mode":           "offline",
		"image_path":     qcow2,
		"config_source":  cfg,
		"output_dir":     outDir,
		"report_formats": []string{"json"},
		"metadata":       map[string]string{"e2e": "offline"},
	}
	if bin := os.Getenv("TREADMARK_E2E_BIN"); bin != "" {
		raw["treadmark_binary"] = bin
	}

	p := &PostProcessor{}
	if err := p.Configure(raw); err != nil {
		t.Fatal(err)
	}

	art, keep, force, err := p.PostProcess(context.Background(), testUi(), &fakeArtifact{files: []string{qcow2}, id: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if !keep || !force {
		t.Fatal("must force-keep input")
	}

	for _, f := range []string{"baseline.db", "baseline-info.json", "init-scan.json", "metadata.json", "SHA256SUMS"} {
		if fi, err := os.Stat(filepath.Join(outDir, f)); err != nil || fi.Size() == 0 {
			t.Errorf("bundle file %s missing or empty: %v", f, err)
		}
	}

	sc, err := metadata.Load(filepath.Join(outDir, metadata.SidecarName))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Mode != "offline" || sc.OS != "linux" {
		t.Fatalf("sidecar: %+v", sc)
	}
	if sc.Baseline == nil || sc.Baseline.Entries == 0 {
		t.Fatalf("baseline should have entries: %+v", sc.Baseline)
	}
	if sc.Custom["e2e"] != "offline" {
		t.Fatalf("custom: %v", sc.Custom)
	}
	t.Logf("offline e2e: %d entries, db sha256 %s, artifact %s", sc.Baseline.Entries, sc.Baseline.SHA256, art)
}
