package treadmark

// Acceptance test: a real `packer build` with the docker builder. The
// provisioner downloads the treadmark .deb from its GitHub release, installs
// it in an ubuntu:24.04 container (as root — disable_sudo), captures a
// baseline over /etc, and the collect post-processor finalizes the bundle.
//
// Requirements: PACKER_ACC=1, packer + docker on PATH, and the dev plugin
// installed (`make dev`). CI wires this in .github/workflows/ci.yml.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/metadata"
)

func TestAccLinuxDocker(t *testing.T) {
	if os.Getenv("PACKER_ACC") == "" {
		t.Skip("PACKER_ACC not set")
	}
	for _, bin := range []string{"packer", "docker"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}

	tplDir, err := filepath.Abs(filepath.Join("..", "..", "testacc"))
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "bundle")

	run := func(fatal bool, args ...string) {
		t.Helper()
		cmd := exec.Command("packer", args...)
		cmd.Dir = tplDir
		cmd.Env = append(os.Environ(), "PKR_VAR_output_dir="+outDir, "CHECKPOINT_DISABLE=1")
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			if !fatal {
				t.Logf("packer %v failed (tolerated): %v\n%s", args, err, buf.String())
				return
			}
			t.Fatalf("packer %v failed: %v\n%s", args, err, buf.String())
		}
		t.Logf("packer %v:\n%s", args, buf.String())
	}

	// init is best-effort: until the first GitHub release exists, resolving
	// the treadmark plugin 404s even though `make dev` installed it locally.
	// If a required plugin is genuinely missing, the build fails right after.
	run(false, "init", ".")
	run(true, "build", ".")

	for _, f := range []string{"baseline.db", "baseline-info.json", "init-scan.json", "metadata.json", "SHA256SUMS"} {
		if fi, err := os.Stat(filepath.Join(outDir, f)); err != nil || fi.Size() == 0 {
			t.Errorf("bundle file %s missing or empty: %v", f, err)
		}
	}
	sc, err := metadata.Load(filepath.Join(outDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Mode != "in-guest" || sc.OS != "linux" {
		t.Fatalf("sidecar: %+v", sc)
	}
	if sc.Baseline == nil || sc.Baseline.Entries == 0 {
		t.Fatalf("baseline empty: %+v", sc.Baseline)
	}
	sum, _, err := metadata.FileSHA256(filepath.Join(outDir, "baseline.db"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Baseline.SHA256 != sum {
		t.Fatalf("metadata sha %s != actual %s", sc.Baseline.SHA256, sum)
	}
	t.Logf("acceptance: %d entries, treadmark %s", sc.Baseline.Entries, sc.TreadmarkVersion)
}
