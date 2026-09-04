// Package metadata writes the bundle sidecar (metadata.json) and the
// chain-of-custody SHA256SUMS file that accompany every captured baseline.
package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	SchemaVersion = 1
	SidecarName   = "metadata.json"
	SumsName      = "SHA256SUMS"
)

// Baseline is the provenance block extracted from `treadmark baseline info --json`.
type Baseline struct {
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Entries   int64  `json:"entries,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Host      string `json:"host,omitempty"`
	OS        string `json:"os,omitempty"`
}

// S3Result records where a bundle was uploaded.
type S3Result struct {
	Bucket string   `json:"bucket"`
	Prefix string   `json:"prefix"`
	Keys   []string `json:"keys"`
}

// Sidecar is the metadata.json document written next to baseline.db.
type Sidecar struct {
	SchemaVersion    int               `json:"schema_version"`
	BuildName        string            `json:"build_name,omitempty"`
	BuilderType      string            `json:"builder_type,omitempty"`
	PackerRunUUID    string            `json:"packer_run_uuid,omitempty"`
	PluginVersion    string            `json:"plugin_version,omitempty"`
	TreadmarkVersion string            `json:"treadmark_version,omitempty"`
	Mode             string            `json:"mode"` // "in-guest" or "offline"
	OS               string            `json:"os,omitempty"`
	Scope            string            `json:"scope,omitempty"`
	CapturedAt       string            `json:"captured_at,omitempty"` // RFC3339 UTC
	Baseline         *Baseline         `json:"baseline,omitempty"`
	Reports          []string          `json:"reports,omitempty"`
	S3               []S3Result        `json:"s3,omitempty"`
	Custom           map[string]string `json:"custom,omitempty"`
}

// BaselineFromInfoJSON parses `treadmark baseline info --json` output,
// tolerating absent keys (older treadmark versions).
func BaselineFromInfoJSON(raw []byte) (*Baseline, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing baseline info JSON: %w", err)
	}
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	num := func(k string) int64 {
		if v, ok := m[k].(float64); ok {
			return int64(v)
		}
		return 0
	}
	return &Baseline{
		SHA256:    str("db_sha256"),
		SizeBytes: num("db_size_bytes"),
		Entries:   num("tracked_entries"),
		CreatedAt: str("baseline_created_at"),
		Host:      str("baseline_host"),
		OS:        str("baseline_os"),
	}, nil
}

// Load reads a sidecar from path.
func Load(path string) (*Sidecar, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Sidecar
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &s, nil
}

// Write serializes the sidecar to path (0644, indented).
func (s *Sidecar) Write(path string) error {
	s.SchemaVersion = SchemaVersion
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// MergeCustom merges m into the sidecar's Custom map; existing keys win so a
// provisioner-recorded value is not clobbered by a later post-processor.
func (s *Sidecar) MergeCustom(m map[string]string) {
	if len(m) == 0 {
		return
	}
	if s.Custom == nil {
		s.Custom = map[string]string{}
	}
	for k, v := range m {
		if _, exists := s.Custom[k]; !exists {
			s.Custom[k] = v
		}
	}
}

// FileSHA256 returns the hex sha256 and size of the file at path.
func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// BundleFiles lists every regular file under dir (recursively), as
// dir-relative slash paths, sorted. SHA256SUMS itself is excluded.
func BundleFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == SumsName {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// WriteBundleSums writes SHA256SUMS over every file in dir (recursive,
// excluding SHA256SUMS itself) in `sha256sum` format, and returns its path.
func WriteBundleSums(dir string) (string, error) {
	files, err := BundleFiles(dir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, rel := range files {
		sum, _, err := FileSHA256(filepath.Join(dir, rel))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, rel)
	}
	out := filepath.Join(dir, SumsName)
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return out, nil
}
