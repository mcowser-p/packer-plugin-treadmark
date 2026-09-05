//go:generate packer-sdc mapstructure-to-hcl2 -type Config
//go:generate packer-sdc struct-markdown

// Package treadmark implements the `treadmark` provisioner: it installs
// treadmark inside the guest being built, captures a file-integrity baseline
// (`treadmark files init`, or `all init` on Windows), verifies it with a
// scan, records provenance (`treadmark baseline info --json`), and downloads
// the resulting bundle (baseline.db + reports + metadata) to the build host.
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
	"gopkg.in/yaml.v3"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/metadata"
	"github.com/mcowser-p/packer-plugin-treadmark/internal/release"
	"github.com/mcowser-p/packer-plugin-treadmark/internal/tmcli"
	"github.com/mcowser-p/packer-plugin-treadmark/version"
)

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// Guest operating system: "auto" (default), "linux", or "windows".
	// Auto-detection probes `uname -s -m`, then falls back to `cmd /c echo
	// %OS%`.
	OS string `mapstructure:"os"`

	// Guest CPU architecture, Go-style: "amd64" or "arm64". Detected from
	// the OS probe when unset; Windows defaults to amd64.
	Arch string `mapstructure:"arch"`

	// How to install treadmark in the guest: "auto" (default), "deb",
	// "rpm", "binary", "msi", or "none" (treadmark is already installed).
	// Auto picks deb/rpm/binary from /etc/debian_version and
	// /etc/redhat-release, and msi on Windows.
	InstallMethod string `mapstructure:"install_method"`

	// treadmark release to install, without the leading "v" (e.g.
	// "0.11.0"). "latest" (default) resolves via the GitHub API.
	TreadmarkVersion string `mapstructure:"treadmark_version"`

	// Path to a local .deb/.rpm/.msi/binary to upload instead of
	// downloading a release.
	PackagePath string `mapstructure:"package_path"`

	// Base URL for release downloads; override for air-gapped mirrors.
	// Defaults to the treadmark GitHub releases URL. When overridden,
	// treadmark_version must be pinned (no "latest").
	DownloadURLBase string `mapstructure:"download_url_base"`

	// Skip SHA256SUMS verification of downloaded release assets.
	DisableChecksumVerify bool `mapstructure:"disable_checksum_verify"`

	// Full path of the treadmark executable in the guest. Defaults to
	// /usr/bin/treadmark (Linux) or C:\Program Files\Treadmark\treadmark.exe
	// (Windows). Always invoked by full path.
	TreadmarkPath string `mapstructure:"treadmark_path"`

	// In-guest config file used by every treadmark command. Defaults to
	// /etc/treadmark/treadmark.yaml (Linux) or
	// C:\ProgramData\Treadmark\treadmark.yaml (Windows).
	ConfigPath string `mapstructure:"config_path"`

	// Local treadmark config (YAML or JSON) uploaded to config_path before
	// the baseline is captured. Required when install_method is "binary"
	// (the standalone binary ships no config); with "none", the guest must
	// already have a config at config_path if this is unset.
	ConfigSource string `mapstructure:"config_source"`

	// What to baseline: "files" (Linux default), "all" (Windows default:
	// files + registry), or "registry" (Windows only).
	Scope string `mapstructure:"scope"`

	// Skip the post-init verification scan. The scan proves the baseline
	// is readable and that nothing changed mid-capture; it is also the
	// only way report files are produced.
	SkipVerify bool `mapstructure:"skip_verify"`

	// What to do when the verification scan exits 1 (drift): "error"
	// (default, fail the build) or "warn". Exit 2 always fails the build.
	OnDrift string `mapstructure:"on_drift"`

	// Report formats to write from the verification scan (one `treadmark
	// files scan --report init-scan.<ext>` per entry — each is a full
	// filesystem walk). Valid: json, ndjson, csv, sarif, md, html, txt.
	// On Windows the registry half is additionally captured as
	// registry-scan.json.
	ReportFormats []string `mapstructure:"report_formats"`

	// Skip downloading the bundle to the host (the baseline then only
	// ships inside the image).
	SkipDownload bool `mapstructure:"skip_download"`

	// Host directory receiving the bundle. Defaults to
	// treadmark-output/<build name>.
	OutputDir string `mapstructure:"output_dir"`

	// Delete the baseline from the guest after downloading it
	// (capture-only mode; by default the baseline ships in the image so
	// clones can self-verify).
	RemoveFromImage bool `mapstructure:"remove_from_image"`

	// Do not prefix guest commands with sudo (e.g. the docker builder,
	// which runs as root). Linux only; ignored on Windows.
	DisableSudo bool `mapstructure:"disable_sudo"`

	// Guest staging directory for package upload and world-readable copies
	// of the bundle. Data only — nothing is ever executed from it (CIS
	// hardening mounts /tmp noexec; /var/tmp may be too). Defaults to
	// /var/tmp/packer-treadmark (Linux) or
	// C:\Windows\Temp\packer-treadmark (Windows). Removed afterwards.
	StagingDir string `mapstructure:"staging_dir"`

	// Timeout for each long-running treadmark command (init and each
	// scan). Defaults to 15m on Linux, 45m on Windows.
	InitTimeout time.Duration `mapstructure:"init_timeout"`

	// Extra key/value pairs recorded under "custom" in metadata.json
	// (build_number, distro, ...).
	Metadata map[string]string `mapstructure:"metadata"`

	ctx interpolate.Context
}

type Provisioner struct {
	config Config
}

func (p *Provisioner) ConfigSpec() hcldec.ObjectSpec {
	return p.config.FlatMapstructure().HCL2Spec()
}

func (p *Provisioner) Prepare(raws ...interface{}) error {
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

	if c.OS == "" {
		c.OS = "auto"
	}
	switch c.OS {
	case "auto", "linux", "windows":
	default:
		appendErr("os must be auto, linux, or windows; got %q", c.OS)
	}

	switch c.Arch {
	case "", "amd64", "arm64":
	default:
		appendErr("arch must be amd64 or arm64; got %q", c.Arch)
	}

	if c.InstallMethod == "" {
		c.InstallMethod = "auto"
	}
	switch c.InstallMethod {
	case "auto", "deb", "rpm", "binary", "msi", "none":
	default:
		appendErr("install_method must be auto, deb, rpm, binary, msi, or none; got %q", c.InstallMethod)
	}
	if c.OS == "linux" && c.InstallMethod == "msi" {
		appendErr("install_method msi is not valid with os = linux")
	}
	if c.OS == "windows" {
		switch c.InstallMethod {
		case "deb", "rpm", "binary":
			appendErr("install_method %s is not valid with os = windows (use msi or none)", c.InstallMethod)
		}
	}

	if c.TreadmarkVersion == "" {
		c.TreadmarkVersion = "latest"
	}
	if c.PackagePath != "" {
		if c.TreadmarkVersion != "latest" {
			appendErr("package_path and treadmark_version are mutually exclusive; the package file determines the version")
		}
		if _, err := os.Stat(c.PackagePath); err != nil {
			appendErr("package_path: %s", err)
		}
		if m := methodFromExt(c.PackagePath); m != "" && c.InstallMethod != "auto" && c.InstallMethod != m {
			appendErr("package_path extension implies install_method %q but %q was configured", m, c.InstallMethod)
		}
	}

	if c.DownloadURLBase == "" {
		c.DownloadURLBase = release.DefaultBaseURL
	}
	if c.DownloadURLBase != release.DefaultBaseURL && c.TreadmarkVersion == "latest" && c.PackagePath == "" && c.InstallMethod != "none" {
		appendErr("treadmark_version = \"latest\" cannot be resolved against a custom download_url_base; pin an explicit version")
	}

	if c.ConfigSource != "" {
		if _, err := os.Stat(c.ConfigSource); err != nil {
			appendErr("config_source: %s", err)
		}
	}
	if c.InstallMethod == "binary" && c.ConfigSource == "" {
		appendErr("install_method = \"binary\" requires config_source: the standalone binary ships no /etc/treadmark/treadmark.yaml")
	}

	switch c.Scope {
	case "", "files", "all", "registry":
	default:
		appendErr("scope must be files, all, or registry; got %q", c.Scope)
	}
	if c.OS == "linux" && (c.Scope == "all" || c.Scope == "registry") {
		appendErr("scope %q is Windows-only (the registry baseline has no Linux equivalent)", c.Scope)
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

	if c.OutputDir == "" {
		name := c.PackerBuildName
		if name == "" {
			name = "default"
		}
		c.OutputDir = filepath.Join("treadmark-output", name)
	}

	if errs != nil && len(errs.Errors) > 0 {
		return errs
	}
	return nil
}

func methodFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".deb":
		return "deb"
	case ".rpm":
		return "rpm"
	case ".msi":
		return "msi"
	default:
		return ""
	}
}

// resolved holds per-build values that depend on the probed guest OS.
type resolved struct {
	os            string
	arch          string
	method        string
	scope         string
	treadmarkPath string
	configPath    string
	stagingDir    string
	dbPath        string
	initTimeout   time.Duration
	sudo          bool
}

// guestResults is what the OS-specific flows hand back for bundle assembly.
type guestResults struct {
	treadmarkVersion string
	baselineInfoJSON []byte
	registryScanJSON []byte
	reports          []string // bundle-relative names downloaded to output_dir
	baselineOnHost   bool
}

func (p *Provisioner) Provision(ctx context.Context, ui packersdk.Ui, comm packersdk.Communicator, generatedData map[string]interface{}) error {
	c := &p.config
	r := &runner{comm: comm, ui: ui}

	osName, arch, err := p.resolveGuest(ctx, r)
	if err != nil {
		return err
	}
	rd, err := p.resolveRuntime(osName, arch)
	if err != nil {
		return err
	}
	ui.Say(fmt.Sprintf("treadmark: guest is %s/%s; scope %s; install via %s", rd.os, rd.arch, rd.scope, rd.method))

	pkgPath, pkgVersion, err := p.localPackage(ctx, ui, rd)
	if err != nil {
		return err
	}

	var res *guestResults
	switch rd.os {
	case "linux":
		res, err = p.provisionLinux(ctx, r, rd, pkgPath, pkgVersion)
	case "windows":
		res, err = p.provisionWindows(ctx, r, rd, pkgPath)
	default:
		err = fmt.Errorf("unsupported guest os %q", rd.os)
	}
	if err != nil {
		return err
	}

	if c.SkipDownload {
		ui.Say("treadmark: skip_download set; baseline remains in the image only")
		return nil
	}
	return p.writeBundle(ui, rd, res, generatedData)
}

// resolveGuest returns the guest OS and arch, probing over the communicator
// when the config leaves either on auto. The probes run with stderr captured
// rather than surfaced: each one failing is the expected outcome on the other
// OS (uname under a PowerShell DefaultShell prints a multi-line
// CommandNotFoundException), so their noise only ever reaches the user inside
// the error when both probes fail.
func (p *Provisioner) resolveGuest(ctx context.Context, r *runner) (string, string, error) {
	c := &p.config
	osName, arch := c.OS, c.Arch
	if osName == "windows" && arch == "" {
		arch = "amd64"
	}
	if osName != "auto" && arch != "" {
		return osName, arch, nil
	}

	out, unameStderr, code, err := r.captureQuiet(ctx, "uname -s -m", 30*time.Second)
	if err == nil && code == 0 && strings.Contains(out, "Linux") {
		if osName == "windows" {
			return "", "", fmt.Errorf("os = windows configured but the guest answers to uname as Linux")
		}
		f := strings.Fields(out)
		probed := "amd64"
		if len(f) >= 2 {
			switch f[1] {
			case "x86_64":
				probed = "amd64"
			case "aarch64", "arm64":
				probed = "arm64"
			default:
				return "", "", fmt.Errorf("unsupported guest architecture %q", f[1])
			}
		}
		if arch == "" {
			arch = probed
		}
		return "linux", arch, nil
	}

	out, cmdStderr, code, err := r.captureQuiet(ctx, `cmd /c "echo %OS% %PROCESSOR_ARCHITECTURE%"`, 30*time.Second)
	if err == nil && code == 0 && strings.Contains(out, "Windows_NT") {
		if osName == "linux" {
			return "", "", fmt.Errorf("os = linux configured but the guest reports Windows_NT")
		}
		if arch == "" {
			arch = "amd64"
			if strings.Contains(out, "ARM64") {
				arch = "arm64"
			}
		}
		return "windows", arch, nil
	}
	return "", "", fmt.Errorf("could not detect the guest OS (uname and cmd both failed); set os = \"linux\" or \"windows\" explicitly%s", probeNotes(unameStderr, cmdStderr))
}

// probeNotes condenses the quiet probes' stderr into a suffix for the
// detection-failure error — the only place that output can still surface.
func probeNotes(unameStderr, cmdStderr string) string {
	var notes []string
	if s := firstLine(unameStderr); s != "" {
		notes = append(notes, "uname said: "+s)
	}
	if s := firstLine(cmdStderr); s != "" {
		notes = append(notes, "cmd said: "+s)
	}
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, "; ") + ")"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func (p *Provisioner) resolveRuntime(osName, arch string) (*resolved, error) {
	c := &p.config
	rd := &resolved{os: osName, arch: arch}

	rd.method = c.InstallMethod
	rd.scope = c.Scope
	rd.treadmarkPath = c.TreadmarkPath
	rd.configPath = c.ConfigPath
	rd.stagingDir = c.StagingDir
	rd.initTimeout = c.InitTimeout

	switch osName {
	case "linux":
		if rd.scope == "" {
			rd.scope = "files"
		}
		if rd.scope != "files" {
			return nil, fmt.Errorf("scope %q is Windows-only", rd.scope)
		}
		if rd.method == "msi" {
			return nil, fmt.Errorf("install_method msi is not valid on a Linux guest")
		}
		if rd.treadmarkPath == "" {
			rd.treadmarkPath = "/usr/bin/treadmark"
		}
		if rd.configPath == "" {
			rd.configPath = "/etc/treadmark/treadmark.yaml"
		}
		if rd.stagingDir == "" {
			rd.stagingDir = "/var/tmp/packer-treadmark"
		}
		if rd.initTimeout == 0 {
			rd.initTimeout = 15 * time.Minute
		}
		rd.sudo = !c.DisableSudo
		if !strings.HasPrefix(rd.stagingDir, "/") || strings.Count(strings.TrimRight(rd.stagingDir, "/"), "/") < 2 {
			return nil, fmt.Errorf("staging_dir %q must be an absolute path at least two levels deep", rd.stagingDir)
		}
	case "windows":
		if rd.scope == "" {
			rd.scope = "all"
		}
		switch rd.method {
		case "auto":
			rd.method = "msi"
		case "msi", "none":
		default:
			return nil, fmt.Errorf("install_method %q is not valid on a Windows guest (use msi or none)", rd.method)
		}
		if rd.treadmarkPath == "" {
			rd.treadmarkPath = `C:\Program Files\Treadmark\treadmark.exe`
		}
		if rd.configPath == "" {
			rd.configPath = `C:\ProgramData\Treadmark\treadmark.yaml`
		}
		if rd.stagingDir == "" {
			rd.stagingDir = `C:\Windows\Temp\packer-treadmark`
		}
		if rd.initTimeout == 0 {
			rd.initTimeout = 45 * time.Minute
		}
	}

	// Paths get embedded in quoted shell/PowerShell fragments; reject
	// metacharacters rather than trying to escape them.
	for name, v := range map[string]string{
		"staging_dir":    rd.stagingDir,
		"treadmark_path": rd.treadmarkPath,
		"config_path":    rd.configPath,
	} {
		if err := checkPathSafe(name, v); err != nil {
			return nil, err
		}
	}

	rd.dbPath = p.guestDBPath(osName)
	if err := checkPathSafe("db_path (from config_source)", rd.dbPath); err != nil {
		return nil, err
	}
	return rd, nil
}

// checkPathSafe rejects characters that would break the quoted command
// fragments these paths are spliced into.
func checkPathSafe(name, v string) error {
	if strings.ContainsAny(v, "'\"$`\n") {
		return fmt.Errorf("%s %q contains shell metacharacters (quotes, $, backtick, or newline) — pick a plainer path", name, v)
	}
	return nil
}

// guestDBPath returns where the baseline DB lands in the guest: treadmark's
// per-OS default, unless the uploaded config_source overrides db_path.
func (p *Provisioner) guestDBPath(osName string) string {
	def := "/var/lib/treadmark/baseline.db"
	if osName == "windows" {
		def = `C:\ProgramData\Treadmark\baseline.db`
	}
	if p.config.ConfigSource == "" {
		return def
	}
	raw, err := os.ReadFile(p.config.ConfigSource)
	if err != nil {
		return def
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return def
	}
	if v, ok := m["db_path"].(string); ok && v != "" {
		return v
	}
	return def
}

// localPackage returns a host path to the package to upload (empty for
// install_method none) and the version it corresponds to when known.
func (p *Provisioner) localPackage(ctx context.Context, ui packersdk.Ui, rd *resolved) (string, string, error) {
	c := &p.config
	if rd.method == "none" {
		return "", "", nil
	}
	if c.PackagePath != "" {
		return c.PackagePath, "", nil
	}
	cl := &release.Client{BaseURL: c.DownloadURLBase, SkipChecksum: c.DisableChecksumVerify}
	ver := c.TreadmarkVersion
	if ver == "latest" {
		v, err := cl.ResolveLatest(ctx)
		if err != nil {
			return "", "", err
		}
		ver = v
		ui.Say(fmt.Sprintf("treadmark: latest release is v%s", ver))
	}
	method := rd.method
	if method == "auto" {
		// Linux with method still unresolved: provisionLinux probes the
		// package manager first and calls back into the release client, so
		// this path only fetches once the method is concrete.
		return "", ver, nil
	}
	path, err := cl.Ensure(ctx, release.Spec{Version: ver, Method: method, Arch: rd.arch})
	if err != nil {
		return "", "", err
	}
	ui.Say(fmt.Sprintf("treadmark: using %s", filepath.Base(path)))
	return path, ver, nil
}

// fetchPackage downloads a concrete method's asset (used after the in-guest
// package-manager probe resolves install_method auto).
func (p *Provisioner) fetchPackage(ctx context.Context, ui packersdk.Ui, rd *resolved, ver string) (string, error) {
	c := &p.config
	cl := &release.Client{BaseURL: c.DownloadURLBase, SkipChecksum: c.DisableChecksumVerify}
	if ver == "" || ver == "latest" {
		v, err := cl.ResolveLatest(ctx)
		if err != nil {
			return "", err
		}
		ver = v
	}
	path, err := cl.Ensure(ctx, release.Spec{Version: ver, Method: rd.method, Arch: rd.arch})
	if err != nil {
		return "", err
	}
	ui.Say(fmt.Sprintf("treadmark: using %s", filepath.Base(path)))
	return path, nil
}

// writeBundle assembles the host-side bundle: baseline-info.json,
// registry-scan.json, metadata.json, and SHA256SUMS.
func (p *Provisioner) writeBundle(ui packersdk.Ui, rd *resolved, res *guestResults, generatedData map[string]interface{}) error {
	c := &p.config
	dir := c.OutputDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	var baseline *metadata.Baseline
	if len(res.baselineInfoJSON) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "baseline-info.json"), res.baselineInfoJSON, 0o644); err != nil {
			return err
		}
		b, err := metadata.BaselineFromInfoJSON(res.baselineInfoJSON)
		if err != nil {
			ui.Error(fmt.Sprintf("treadmark: could not parse baseline info output: %s", err))
		} else {
			baseline = b
		}
	}
	if len(res.registryScanJSON) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "registry-scan.json"), res.registryScanJSON, 0o644); err != nil {
			return err
		}
	}

	if res.baselineOnHost {
		sum, size, err := metadata.FileSHA256(filepath.Join(dir, "baseline.db"))
		if err != nil {
			return fmt.Errorf("hashing downloaded baseline.db: %w", err)
		}
		if baseline == nil {
			baseline = &metadata.Baseline{}
		}
		if baseline.SHA256 != "" && baseline.SHA256 != sum {
			return fmt.Errorf("baseline.db corrupted in transit: guest reports sha256 %s, downloaded file is %s", baseline.SHA256, sum)
		}
		baseline.SHA256 = sum
		baseline.SizeBytes = size
	}

	runUUID, _ := generatedData["PackerRunUUID"].(string)
	sc := &metadata.Sidecar{
		BuildName:        c.PackerBuildName,
		BuilderType:      c.PackerBuilderType,
		PackerRunUUID:    runUUID,
		PluginVersion:    version.Version,
		TreadmarkVersion: res.treadmarkVersion,
		Mode:             "in-guest",
		OS:               rd.os,
		Scope:            rd.scope,
		CapturedAt:       time.Now().UTC().Format(time.RFC3339),
		Baseline:         baseline,
		Reports:          res.reports,
	}
	sc.MergeCustom(c.Metadata)
	if err := sc.Write(filepath.Join(dir, metadata.SidecarName)); err != nil {
		return err
	}
	if _, err := metadata.WriteBundleSums(dir); err != nil {
		return err
	}
	ui.Say(fmt.Sprintf("treadmark: bundle written to %s", dir))
	return nil
}
