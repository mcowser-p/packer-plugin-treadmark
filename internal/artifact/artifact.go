// Package artifact implements the packersdk.Artifact returned by the
// treadmark post-processor: the input artifact's files plus the captured
// baseline bundle, so a downstream `manifest` post-processor records both.
package artifact

import (
	"fmt"
	"os"
	"path/filepath"
)

// BuilderID identifies artifacts produced by this plugin.
const BuilderID = "mcowser-p.treadmark"

type Artifact struct {
	// InputFiles are the upstream artifact's files (e.g. the qcow2). Never
	// touched by Destroy.
	InputFiles []string
	// BundleFiles are the files this plugin generated (baseline.db,
	// metadata.json, reports, SHA256SUMS).
	BundleFiles []string
	// InputID is the upstream artifact's Id(), may be empty.
	InputID string
	// BaselineSHA is the hex sha256 of baseline.db, may be empty when the
	// bundle was collected without a baseline.
	BaselineSHA string
	// Summary is the human-readable one-liner shown at the end of the build.
	Summary string
	// StateData holds forwarded state ("generated_data" from the input
	// artifact) plus a "treadmark" map of bundle essentials.
	StateData map[string]interface{}
}

func (a *Artifact) BuilderId() string {
	return BuilderID
}

// Files returns the input artifact's files followed by the bundle files,
// deduplicated (first occurrence wins). InputFiles can already contain the
// bundle — a chained treadmark post-processor, or a builder artifact that
// enumerates a directory holding output_dir — and without the dedupe every
// bundle file is reported twice to downstream consumers like manifest.
func (a *Artifact) Files() []string {
	seen := make(map[string]bool, len(a.InputFiles)+len(a.BundleFiles))
	out := make([]string, 0, len(a.InputFiles)+len(a.BundleFiles))
	add := func(files []string) {
		for _, f := range files {
			key := filepath.Clean(f)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, f)
		}
	}
	add(a.InputFiles)
	add(a.BundleFiles)
	return out
}

func (a *Artifact) Id() string {
	sha := a.BaselineSHA
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if a.InputID == "" {
		return "treadmark-" + sha
	}
	return a.InputID + "-treadmark-" + sha
}

func (a *Artifact) String() string {
	if a.Summary != "" {
		return a.Summary
	}
	return fmt.Sprintf("Treadmark baseline bundle (%d files)", len(a.BundleFiles))
}

func (a *Artifact) State(name string) interface{} {
	if a.StateData == nil {
		return nil
	}
	return a.StateData[name]
}

// Destroy removes only the files this plugin generated, never the input
// artifact's files.
func (a *Artifact) Destroy() error {
	var firstErr error
	for _, f := range a.BundleFiles {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
