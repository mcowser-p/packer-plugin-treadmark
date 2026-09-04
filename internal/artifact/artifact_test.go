package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactSemantics(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "baseline.db")
	if err := os.WriteFile(bundle, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "image.qcow2")
	if err := os.WriteFile(input, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Artifact{
		InputFiles:  []string{input},
		BundleFiles: []string{bundle},
		InputID:     "alma10",
		BaselineSHA: "abcdef0123456789",
		StateData: map[string]interface{}{
			"generated_data": map[string]interface{}{"k": "v"},
		},
	}

	if a.BuilderId() != BuilderID {
		t.Fatal("builder id")
	}
	files := a.Files()
	if len(files) != 2 || files[0] != input || files[1] != bundle {
		t.Fatalf("files: %v", files)
	}
	if got := a.Id(); got != "alma10-treadmark-abcdef012345" {
		t.Fatalf("id: %s", got)
	}
	if a.State("generated_data") == nil {
		t.Fatal("state not forwarded")
	}
	if a.State("nope") != nil {
		t.Fatal("unknown state should be nil")
	}

	if err := a.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Fatal("bundle file should be removed by Destroy")
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatal("input file must never be removed by Destroy")
	}
	// Destroy is idempotent.
	if err := a.Destroy(); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactNoInput(t *testing.T) {
	a := &Artifact{BaselineSHA: "abc"}
	if got := a.Id(); got != "treadmark-abc" {
		t.Fatalf("id: %s", got)
	}
	if a.String() == "" {
		t.Fatal("string should never be empty")
	}
}
