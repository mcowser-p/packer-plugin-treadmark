//go:generate packer-sdc mapstructure-to-hcl2 -type Config,S3Config
//go:generate packer-sdc struct-markdown

// Package treadmark implements the `treadmark` post-processor. In "collect"
// mode it wraps the bundle the treadmark provisioner downloaded, together
// with the input artifact's files, into one artifact — so a downstream
// `manifest` post-processor records the image and the baseline side by side.
// In "offline" mode it captures the baseline host-side from the finished
// image (guestmount + `treadmark files init --root`), leaving the image
// untouched. Either mode can then upload the bundle to S3.
package treadmark

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/common"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/tmcli"
)

type S3Config struct {
	// Target bucket. Must already exist — the plugin checks reachability
	// with HeadBucket and never creates buckets.
	Bucket string `mapstructure:"bucket" required:"true"`

	// Key prefix for uploaded bundle files (default "treadmark/").
	// Interpolate build variables in HCL for per-image prefixes, e.g.
	// "treadmark/${local.name}/".
	Prefix string `mapstructure:"prefix"`

	// AWS region; empty defers to the default config chain.
	Region string `mapstructure:"region"`

	// Endpoint override for S3-compatible stores (MinIO etc.).
	Endpoint string `mapstructure:"endpoint"`

	// Use path-style addressing (usually wanted with endpoint overrides).
	ForcePathStyle bool `mapstructure:"force_path_style"`

	// Storage class; empty means the service default (STANDARD).
	StorageClass string `mapstructure:"storage_class"`

	// Server-side encryption: "", "AES256", or "aws:kms".
	SSE string `mapstructure:"sse"`

	// KMS key id/arn used when sse = "aws:kms".
	KMSKeyID string `mapstructure:"kms_key_id"`

	// Also upload the input image artifact (off by default; note S3
	// PutObject caps single objects at 5 GiB).
	IncludeImage bool `mapstructure:"include_image"`
}

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// "collect" (default) wraps the provisioner's bundle; "offline"
	// captures a baseline host-side from the built image via guestmount +
	// `treadmark files init --root` (Linux guest images only).
	Mode string `mapstructure:"mode"`

	// Bundle directory: where the provisioner wrote (collect) or where to
	// write (offline). Defaults to treadmark-output/<build name>; must
	// match the provisioner's output_dir in collect mode.
	OutputDir string `mapstructure:"output_dir"`

	// collect: tolerate a missing baseline.db instead of failing. The
	// default failure catches provisioner/post-processor ordering and
	// output_dir mismatches.
	AllowMissingBaseline bool `mapstructure:"allow_missing_baseline"`

	// offline: image to mount. Defaults to the first *.qcow2 in the input
	// artifact's files.
	ImagePath string `mapstructure:"image_path"`

	// offline (required): host-side treadmark config (YAML or JSON) with
	// the paths/excludes to baseline. The plugin writes a temp copy with
	// db_path pointed into output_dir and any report block stripped —
	// treadmark has no --db flag, the config is the only way to place the
	// DB on the host.
	ConfigSource string `mapstructure:"config_source"`

	// offline: treadmark binary on the build host. Default: "treadmark"
	// found on PATH.
	TreadmarkBinary string `mapstructure:"treadmark_binary"`

	// offline: capture strategy; only "guestmount" is implemented.
	OfflineStrategy string `mapstructure:"offline_strategy"`

	// offline: LIBGUESTFS_BACKEND for guestmount (default "direct",
	// matching the holy-qcow evidence tooling).
	LibguestfsBackend string `mapstructure:"libguestfs_backend"`

	// offline: extra environment for guestmount/treadmark. Overrides the
	// auto-pinned SUPERMIN_KERNEL / SUPERMIN_MODULES.
	ExtraEnv map[string]string `mapstructure:"extra_env"`

	// offline: run guestmount/treadmark under sudo (fallback for FUSE
	// policy or unreadable kernels).
	UseSudo bool `mapstructure:"use_sudo"`

	// offline: guestmount/guestunmount timeout (default 5m).
	MountTimeout time.Duration `mapstructure:"mount_timeout"`

	// offline: per-treadmark-command timeout (default 30m).
	InitTimeout time.Duration `mapstructure:"init_timeout"`

	// offline: skip the post-init verification scan.
	SkipVerify bool `mapstructure:"skip_verify"`

	// Drift policy for verification scans: "error" (default) or "warn".
	OnDrift string `mapstructure:"on_drift"`

	// offline: report formats written from the verification scan into
	// output_dir (json, ndjson, csv, sarif, md, html, txt).
	ReportFormats []string `mapstructure:"report_formats"`

	// Extra key/value pairs merged into metadata.json "custom"; keys the
	// provisioner already recorded win.
	Metadata map[string]string `mapstructure:"metadata"`

	// Optional upload destinations; each s3 block is one bucket.
	S3 []S3Config `mapstructure:"s3"`

	ctx interpolate.Context
}

type PostProcessor struct {
	config Config
}

func (p *PostProcessor) ConfigSpec() hcldec.ObjectSpec {
	return p.config.FlatMapstructure().HCL2Spec()
}

func (p *PostProcessor) Configure(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		PluginType:         "treadmark",
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
	}, raws...)
	if err != nil {
		return err
	}
	c := &p.config
	var errs *packersdk.MultiError
	appendErr := func(format string, a ...interface{}) {
		errs = packersdk.MultiErrorAppend(errs, fmt.Errorf(format, a...))
	}

	if c.Mode == "" {
		c.Mode = "collect"
	}
	if c.Mode != "collect" && c.Mode != "offline" {
		appendErr("mode must be collect or offline; got %q", c.Mode)
	}

	if c.OutputDir == "" {
		name := c.PackerBuildName
		if name == "" {
			name = "default"
		}
		c.OutputDir = filepath.Join("treadmark-output", name)
	}

	if c.OfflineStrategy == "" {
		c.OfflineStrategy = "guestmount"
	}
	if c.OfflineStrategy != "guestmount" {
		appendErr("offline_strategy: only \"guestmount\" is implemented; got %q", c.OfflineStrategy)
	}
	if c.LibguestfsBackend == "" {
		c.LibguestfsBackend = "direct"
	}
	if c.MountTimeout == 0 {
		c.MountTimeout = 5 * time.Minute
	}
	if c.InitTimeout == 0 {
		c.InitTimeout = 30 * time.Minute
	}
	if c.OnDrift == "" {
		c.OnDrift = "error"
	}
	if c.OnDrift != "error" && c.OnDrift != "warn" {
		appendErr("on_drift must be error or warn; got %q", c.OnDrift)
	}
	for _, f := range c.ReportFormats {
		if !tmcli.ValidReportFormat(f) {
			appendErr("report_formats: %q is not a treadmark report format (valid: %s)", f, strings.Join(tmcli.ReportFormats, ", "))
		}
	}

	if c.Mode == "offline" {
		if c.ConfigSource == "" {
			appendErr("offline mode requires config_source (treadmark has no --db flag; the plugin rewrites db_path in a temp copy of your config)")
		} else if _, err := os.Stat(c.ConfigSource); err != nil {
			appendErr("config_source: %s", err)
		}
	} else {
		for name, set := range map[string]bool{
			"image_path":       c.ImagePath != "",
			"config_source":    c.ConfigSource != "",
			"treadmark_binary": c.TreadmarkBinary != "",
			"report_formats":   len(c.ReportFormats) > 0,
			"use_sudo":         c.UseSudo,
		} {
			if set {
				appendErr("%s is only used in offline mode (set mode = \"offline\")", name)
			}
		}
	}

	for i, s := range c.S3 {
		if s.Bucket == "" {
			appendErr("s3 block %d: bucket is required", i+1)
		}
		switch s.SSE {
		case "", "AES256", "aws:kms":
		default:
			appendErr("s3 block %d: sse must be \"\", AES256, or aws:kms; got %q", i+1, s.SSE)
		}
		if s.KMSKeyID != "" && s.SSE != "aws:kms" {
			appendErr("s3 block %d: kms_key_id needs sse = \"aws:kms\"", i+1)
		}
		if c.S3[i].Prefix == "" {
			c.S3[i].Prefix = "treadmark/"
		}
	}

	if errs != nil && len(errs.Errors) > 0 {
		return errs
	}
	return nil
}

// PostProcess always returns (artifact, keep=true, forceOverride=true) —
// the same force-keep contract as the manifest post-processor — so the input
// image artifact can never be silently discarded through this step.
func (p *PostProcessor) PostProcess(ctx context.Context, ui packersdk.Ui, source packersdk.Artifact) (packersdk.Artifact, bool, bool, error) {
	switch p.config.Mode {
	case "offline":
		if err := p.runOffline(ctx, ui, source); err != nil {
			return source, true, true, err
		}
	default: // collect
		if err := p.checkCollect(); err != nil {
			return source, true, true, err
		}
	}
	art, err := p.finishBundle(ctx, ui, source)
	if err != nil {
		return source, true, true, err
	}
	return art, true, true, nil
}
