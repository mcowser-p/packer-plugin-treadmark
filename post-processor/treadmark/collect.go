package treadmark

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/artifact"
	"github.com/mcowser-p/packer-plugin-treadmark/internal/metadata"
	"github.com/mcowser-p/packer-plugin-treadmark/internal/s3put"
	pluginversion "github.com/mcowser-p/packer-plugin-treadmark/version"
)

// checkCollect validates that the provisioner's bundle is actually there —
// the failure catches ordering and output_dir mismatches.
func (p *PostProcessor) checkCollect() error {
	c := &p.config
	fi, err := os.Stat(c.OutputDir)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("collect mode: bundle dir %s not found — did the treadmark provisioner run in this build with the same output_dir?", c.OutputDir)
	}
	if !c.AllowMissingBaseline {
		if _, err := os.Stat(filepath.Join(c.OutputDir, "baseline.db")); err != nil {
			return fmt.Errorf("collect mode: no baseline.db in %s — the treadmark provisioner must run before this post-processor with the same output_dir (or set allow_missing_baseline = true)", c.OutputDir)
		}
	}
	return nil
}

// finishBundle finalizes metadata.json + SHA256SUMS, performs any S3
// uploads, and assembles the combined artifact.
func (p *PostProcessor) finishBundle(ctx context.Context, ui packersdk.Ui, source packersdk.Artifact) (packersdk.Artifact, error) {
	c := &p.config
	dir := c.OutputDir

	scPath := filepath.Join(dir, metadata.SidecarName)
	sc, err := metadata.Load(scPath)
	if err != nil {
		sc = &metadata.Sidecar{
			Mode:        "collect",
			BuildName:   c.PackerBuildName,
			BuilderType: c.PackerBuilderType,
		}
	}
	if sc.PluginVersion == "" {
		sc.PluginVersion = pluginversion.Version
	}
	sc.MergeCustom(c.Metadata)

	dbPath := filepath.Join(dir, "baseline.db")
	if _, err := os.Stat(dbPath); err == nil {
		sum, size, err := metadata.FileSHA256(dbPath)
		if err != nil {
			return nil, err
		}
		if sc.Baseline == nil {
			sc.Baseline = &metadata.Baseline{}
		}
		if sc.Baseline.SHA256 != "" && sc.Baseline.SHA256 != sum {
			return nil, fmt.Errorf("baseline.db does not match the sha256 recorded in metadata.json (%s vs %s) — bundle mixed between builds or tampered with", sc.Baseline.SHA256, sum)
		}
		sc.Baseline.SHA256 = sum
		sc.Baseline.SizeBytes = size
	}

	// Predict the upload set and object keys before writing anything, so the
	// uploaded metadata.json records its own S3 destination.
	preFiles, err := metadata.BundleFiles(dir)
	if err != nil {
		return nil, err
	}
	predicted := map[string]bool{metadata.SidecarName: true, metadata.SumsName: true}
	for _, f := range preFiles {
		predicted[f] = true
	}
	uploadNames := make([]string, 0, len(predicted))
	for name := range predicted {
		uploadNames = append(uploadNames, name)
	}
	sort.Strings(uploadNames)

	var inputFiles []string
	inputID := ""
	var genData interface{}
	if source != nil {
		inputFiles = source.Files()
		inputID = source.Id()
		genData = source.State("generated_data")
	}

	putters := make([]*s3put.Putter, 0, len(c.S3))
	sc.S3 = nil
	for _, sb := range c.S3 {
		putter, err := s3put.New(ctx, s3put.Options{
			Bucket:         sb.Bucket,
			Prefix:         sb.Prefix,
			Region:         sb.Region,
			Endpoint:       sb.Endpoint,
			ForcePathStyle: sb.ForcePathStyle,
			StorageClass:   sb.StorageClass,
			SSE:            sb.SSE,
			KMSKeyID:       sb.KMSKeyID,
		})
		if err != nil {
			return nil, err
		}
		if err := putter.Check(ctx); err != nil {
			return nil, err
		}
		putters = append(putters, putter)
		keys := make([]string, 0, len(uploadNames))
		for _, n := range uploadNames {
			keys = append(keys, putter.Key(n))
		}
		if sb.IncludeImage {
			for _, f := range inputFiles {
				keys = append(keys, putter.Key(filepath.Base(f)))
			}
		}
		sc.S3 = append(sc.S3, metadata.S3Result{
			Bucket: sb.Bucket,
			Prefix: strings.Trim(sb.Prefix, "/"),
			Keys:   keys,
		})
	}

	if err := sc.Write(scPath); err != nil {
		return nil, err
	}
	if _, err := metadata.WriteBundleSums(dir); err != nil {
		return nil, err
	}

	// Upload after the sums are final, so the uploaded SHA256SUMS covers the
	// uploaded metadata.json.
	for i, putter := range putters {
		sb := c.S3[i]
		ui.Say(fmt.Sprintf("treadmark: uploading bundle to s3://%s/%s", sb.Bucket, strings.Trim(sb.Prefix, "/")))
		for _, n := range uploadNames {
			sum := ""
			if s, _, err := metadata.FileSHA256(filepath.Join(dir, n)); err == nil {
				sum = s
			}
			if _, err := putter.UploadFile(ctx, dir, n, sum); err != nil {
				return nil, err
			}
		}
		if sb.IncludeImage {
			for _, f := range inputFiles {
				sum, _, err := metadata.FileSHA256(f)
				if err != nil {
					return nil, fmt.Errorf("hashing input artifact file %s: %w", f, err)
				}
				ui.Say(fmt.Sprintf("treadmark: uploading input artifact %s (this can take a while)", filepath.Base(f)))
				if _, err := putter.UploadFile(ctx, filepath.Dir(f), filepath.Base(f), sum); err != nil {
					return nil, err
				}
			}
		}
	}

	bundleNow, err := metadata.BundleFiles(dir)
	if err != nil {
		return nil, err
	}
	bundleNow = append(bundleNow, metadata.SumsName)
	bundleAbs := make([]string, 0, len(bundleNow))
	for _, rel := range bundleNow {
		bundleAbs = append(bundleAbs, filepath.Join(dir, rel))
	}

	sha := ""
	entries := int64(0)
	if sc.Baseline != nil {
		sha = sc.Baseline.SHA256
		entries = sc.Baseline.Entries
	}
	summary := fmt.Sprintf("Treadmark baseline bundle: %s", dir)
	if sha != "" {
		summary += fmt.Sprintf(" (db sha256 %.12s", sha)
		if entries > 0 {
			summary += fmt.Sprintf(", %d entries", entries)
		}
		summary += ")"
	}
	if len(sc.S3) > 0 {
		summary += fmt.Sprintf(", uploaded to s3://%s/%s", sc.S3[0].Bucket, sc.S3[0].Prefix)
	}

	state := map[string]interface{}{}
	if genData != nil {
		state["generated_data"] = genData
	}
	tstate := map[string]interface{}{
		"output_dir":      dir,
		"baseline_sha256": sha,
		"mode":            sc.Mode,
	}
	var s3urls []string
	for _, res := range sc.S3 {
		for _, k := range res.Keys {
			s3urls = append(s3urls, "s3://"+res.Bucket+"/"+k)
		}
	}
	if len(s3urls) > 0 {
		tstate["s3_urls"] = s3urls
	}
	state["treadmark"] = tstate

	return &artifact.Artifact{
		InputFiles:  inputFiles,
		BundleFiles: bundleAbs,
		InputID:     inputID,
		BaselineSHA: sha,
		Summary:     summary,
		StateData:   state,
	}, nil
}
