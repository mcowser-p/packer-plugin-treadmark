package treadmark

// Offline capture: guestmount the finished image read-only on the build
// host and run the host's treadmark against the mount with --root, writing
// the baseline straight into output_dir. treadmark stores logical paths
// (--root mapping), so the resulting DB is comparable against live hosts.
// The guestmount/supermin environment handling mirrors holy-qcow's
// evidence.sh, which validated this pattern on this lab's hosts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"gopkg.in/yaml.v3"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/metadata"
	"github.com/mcowser-p/packer-plugin-treadmark/internal/tmcli"
	pluginversion "github.com/mcowser-p/packer-plugin-treadmark/version"
)

func (p *PostProcessor) runOffline(ctx context.Context, ui packersdk.Ui, source packersdk.Artifact) error {
	c := &p.config

	img := c.ImagePath
	if img == "" && source != nil {
		for _, f := range source.Files() {
			if strings.HasSuffix(f, ".qcow2") {
				img = f
				break
			}
		}
	}
	if img == "" {
		return fmt.Errorf("offline mode: input artifact has no *.qcow2 file and image_path is not set")
	}
	if _, err := os.Stat(img); err != nil {
		return fmt.Errorf("offline mode: image: %w", err)
	}

	tmBin := c.TreadmarkBinary
	if tmBin == "" {
		found, err := exec.LookPath("treadmark")
		if err != nil {
			return fmt.Errorf("offline mode needs treadmark on the build host: not found on PATH — install it (pipx install treadmark, or the .deb/.rpm from its releases) or set treadmark_binary")
		}
		tmBin = found
	}
	for _, tool := range []string{"guestmount", "guestunmount"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("offline mode needs libguestfs on the build host: %s not found (install guestfs-tools / libguestfs-tools)", tool)
		}
	}

	env, err := p.offlineEnv()
	if err != nil {
		return err
	}

	scratch, err := os.MkdirTemp("", "packer-treadmark-offline-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	mnt := filepath.Join(scratch, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		return err
	}

	if err := os.MkdirAll(c.OutputDir, 0o755); err != nil {
		return err
	}
	absOut, err := filepath.Abs(c.OutputDir)
	if err != nil {
		return err
	}

	cfgPath, err := p.rewriteConfig(scratch, filepath.Join(absOut, "baseline.db"))
	if err != nil {
		return err
	}

	ui.Say(fmt.Sprintf("treadmark: mounting %s read-only via guestmount", filepath.Base(img)))
	if out, code, err := p.hostRun(ctx, env, c.MountTimeout, "guestmount", "-a", img, "-i", "--ro", mnt); err != nil || code != 0 {
		return fmt.Errorf("guestmount failed (exit %d, %v):\n%s\nhints: LIBGUESTFS_BACKEND=%s is set; on Debian/Ubuntu hosts a root-only /boot/vmlinuz breaks supermin — fix with `sudo dpkg-statoverride --update --add root root 0644 /boot/vmlinuz-$(uname -r)`, or set use_sudo = true, or point extra_env.SUPERMIN_KERNEL at a readable kernel", code, err, tail(out), c.LibguestfsBackend)
	}
	mounted := true
	unmount := func() error {
		if !mounted {
			return nil
		}
		out, code, err := p.hostRun(context.Background(), env, c.MountTimeout, "guestunmount", "--retry=5", mnt)
		if err == nil && code == 0 {
			mounted = false
			return nil
		}
		out2, code2, err2 := p.hostRun(context.Background(), env, time.Minute, "fusermount", "-u", mnt)
		if err2 == nil && code2 == 0 {
			mounted = false
			return nil
		}
		return fmt.Errorf("guestunmount failed (exit %d, %v): %s — fusermount fallback also failed (exit %d, %v): %s", code, err, tail(out), code2, err2, tail(out2))
	}
	defer unmount() //nolint:errcheck // best-effort on early-error paths; checked explicitly below

	if _, err := os.Stat(filepath.Join(mnt, "etc")); err != nil {
		return fmt.Errorf("mounted image has no /etc — offline mode supports Linux guest images only in v1 (for Windows images use the in-guest provisioner)")
	}

	verOut, code, err := p.hostRun(ctx, env, time.Minute, tmBin, "--version")
	if err != nil || code != 0 {
		return fmt.Errorf("%s --version failed (exit %d): %v", tmBin, code, err)
	}
	tmVersion := tmcli.ParseVersion(verOut)

	ui.Say("treadmark: capturing offline baseline (files init --root)")
	if out, code, err := p.hostRun(ctx, env, c.InitTimeout, tmBin, "files", "init", "--config", cfgPath, "--root", mnt, "--force"); err != nil || code != 0 {
		return fmt.Errorf("treadmark files init failed (exit %d, %v):\n%s", code, err, tail(out))
	}

	var reports []string
	if !c.SkipVerify || len(c.ReportFormats) > 0 {
		formats := c.ReportFormats
		if len(formats) == 0 {
			formats = []string{""}
		}
		for _, f := range formats {
			args := []string{"files", "scan", "--config", cfgPath, "--root", mnt}
			if f != "" {
				rel := "init-scan." + f
				args = append(args, "--report", filepath.Join(absOut, rel))
				reports = append(reports, rel)
			}
			out, code, err := p.hostRun(ctx, env, c.InitTimeout, tmBin, args...)
			if err != nil {
				return err
			}
			warn, err := tmcli.EvalScanExit(code, c.OnDrift)
			if err != nil {
				return fmt.Errorf("%w\n%s", err, tail(out))
			}
			if warn != "" {
				ui.Error("treadmark: " + warn)
			}
		}
	}

	infoOut, code, err := p.hostRun(ctx, env, time.Minute, tmBin, "baseline", "info", "--config", cfgPath, "--json")
	if err != nil || code != 0 {
		return fmt.Errorf("treadmark baseline info failed (exit %d): %v", code, err)
	}
	if err := os.WriteFile(filepath.Join(absOut, "baseline-info.json"), []byte(infoOut), 0o644); err != nil {
		return err
	}

	if err := unmount(); err != nil {
		return err
	}

	baseline, err := metadata.BaselineFromInfoJSON([]byte(infoOut))
	if err != nil {
		ui.Error(fmt.Sprintf("treadmark: could not parse baseline info output: %s", err))
		baseline = nil
	}
	sc := &metadata.Sidecar{
		BuildName:        c.PackerBuildName,
		BuilderType:      c.PackerBuilderType,
		PluginVersion:    pluginversion.Version,
		TreadmarkVersion: tmVersion,
		Mode:             "offline",
		OS:               "linux",
		Scope:            "files",
		CapturedAt:       time.Now().UTC().Format(time.RFC3339),
		Baseline:         baseline,
		Reports:          reports,
	}
	sc.MergeCustom(c.Metadata)
	return sc.Write(filepath.Join(absOut, metadata.SidecarName))
}

// hostRun executes a host command with the offline environment, returning
// combined output and the exit code. use_sudo prefixes `sudo -n`.
func (p *PostProcessor) hostRun(ctx context.Context, env []string, timeout time.Duration, name string, args ...string) (string, int, error) {
	if p.config.UseSudo {
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), -1, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode(), nil
		}
		return string(out), -1, err
	}
	return string(out), 0, nil
}

// offlineEnv assembles the environment for guestmount/treadmark: the
// LIBGUESTFS_BACKEND, auto-pinned SUPERMIN_KERNEL/SUPERMIN_MODULES for the
// running kernel (mirroring holy-qcow's evidence.sh), then extra_env
// overrides last.
func (p *PostProcessor) offlineEnv() ([]string, error) {
	c := &p.config
	env := os.Environ()
	env = append(env, "LIBGUESTFS_BACKEND="+c.LibguestfsBackend)
	_, hasK := c.ExtraEnv["SUPERMIN_KERNEL"]
	_, hasM := c.ExtraEnv["SUPERMIN_MODULES"]
	if !hasK || !hasM {
		if rel, err := exec.Command("uname", "-r").Output(); err == nil {
			r := strings.TrimSpace(string(rel))
			kernel := "/boot/vmlinuz-" + r
			if !hasK {
				if f, err := os.Open(kernel); err == nil {
					f.Close()
					env = append(env, "SUPERMIN_KERNEL="+kernel)
				} else if os.IsPermission(err) && !c.UseSudo {
					return nil, fmt.Errorf("%s is not readable, so libguestfs/supermin will fail — fix with `sudo dpkg-statoverride --update --add root root 0644 %s`, or set use_sudo = true, or provide extra_env.SUPERMIN_KERNEL", kernel, kernel)
				}
			}
			if !hasM {
				if fi, err := os.Stat("/lib/modules/" + r); err == nil && fi.IsDir() {
					env = append(env, "SUPERMIN_MODULES=/lib/modules/"+r)
				}
			}
		}
	}
	for k, v := range c.ExtraEnv {
		env = append(env, k+"="+v)
	}
	return env, nil
}

// rewriteConfig writes a temp copy of config_source with db_path pointed at
// dbPath and any report block stripped. The copy is JSON: treadmark accepts
// JSON configs unconditionally (YAML needs its bundled PyYAML), and
// re-encoding sidesteps YAML quoting concerns entirely.
func (p *PostProcessor) rewriteConfig(scratch, dbPath string) (string, error) {
	raw, err := os.ReadFile(p.config.ConfigSource)
	if err != nil {
		return "", err
	}
	var m map[string]interface{}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("parsing config_source %s: %w", p.config.ConfigSource, err)
	}
	if m == nil {
		m = map[string]interface{}{}
	}
	m["db_path"] = dbPath
	delete(m, "report")
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("re-encoding config_source as JSON: %w", err)
	}
	path := filepath.Join(scratch, "treadmark-config.json")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// tail returns the last few lines of command output for error messages.
func tail(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}
