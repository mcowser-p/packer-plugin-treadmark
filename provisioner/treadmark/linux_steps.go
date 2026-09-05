package treadmark

// In-guest capture flow for Linux guests. Two conventions are load-bearing
// for hardened images (see holy-qcow's harden-cis.sh): treadmark is always
// invoked by full path — never from a staging dir — because CIS remediation
// mounts /tmp (and possibly /var/tmp) noexec, and every command is a plain
// argv line so nothing needs to be staged executable.

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"time"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/tmcli"
)

func (p *Provisioner) provisionLinux(ctx context.Context, r *runner, rd *resolved, pkgPath, pkgVersion string) (*guestResults, error) {
	c := &p.config
	res := &guestResults{}

	// tm builds a treadmark invocation (full path, optional sudo).
	tm := func(args string) string {
		s := fmt.Sprintf(`"%s" %s`, rd.treadmarkPath, args)
		if rd.sudo {
			s = "sudo " + s
		}
		return s
	}
	// sh wraps a compound script for sh -c (optional sudo).
	sh := func(script string) string {
		s := "sh -c " + tmcli.ShQuote(script)
		if rd.sudo {
			s = "sudo " + s
		}
		return s
	}

	mustRun := func(what, cmdline string, timeout time.Duration) error {
		code, err := r.run(ctx, cmdline, timeout)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if code != 0 {
			return fmt.Errorf("%s failed (exit %d)", what, code)
		}
		return nil
	}

	if err := mustRun("creating staging dir",
		fmt.Sprintf(`mkdir -p "%s" && chmod 0755 "%s"`, rd.stagingDir, rd.stagingDir), time.Minute); err != nil {
		return nil, err
	}
	defer func() {
		// Best-effort staging cleanup, even on failure; fresh context in
		// case the build's was cancelled.
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = r.run(cctx, sh(fmt.Sprintf(`rm -rf "%s"`, rd.stagingDir)), time.Minute)
	}()

	// Resolve install_method auto with an in-guest probe, then fetch the
	// matching asset (deferred until now because the method decides it).
	if rd.method == "auto" {
		if code, err := r.run(ctx, "test -f /etc/debian_version", 30*time.Second); err == nil && code == 0 {
			rd.method = "deb"
		} else if code, err := r.run(ctx, "test -f /etc/redhat-release -o -f /etc/system-release", 30*time.Second); err == nil && code == 0 {
			rd.method = "rpm"
		} else {
			rd.method = "binary"
			if c.ConfigSource == "" {
				return nil, fmt.Errorf("guest has neither dpkg nor rpm markers; falling back to the standalone binary requires config_source (no config ships with it)")
			}
		}
		r.ui.Say(fmt.Sprintf("treadmark: guest package manager suggests install_method %s", rd.method))
		if pkgPath == "" {
			var err error
			pkgPath, err = p.fetchPackage(ctx, r.ui, rd, pkgVersion)
			if err != nil {
				return nil, err
			}
		}
	}

	if rd.method != "none" {
		if pkgPath == "" {
			return nil, fmt.Errorf("internal error: no package resolved for install_method %s", rd.method)
		}
		remotePkg := path.Join(rd.stagingDir, filepath.Base(pkgPath))
		r.ui.Say(fmt.Sprintf("treadmark: uploading %s", filepath.Base(pkgPath)))
		if err := r.uploadFile(pkgPath, remotePkg); err != nil {
			return nil, err
		}
		var install string
		switch rd.method {
		case "deb":
			install = fmt.Sprintf(`dpkg -i "%s"`, remotePkg)
		case "rpm":
			install = fmt.Sprintf(`if command -v dnf >/dev/null 2>&1; then dnf install -y "%s"; else rpm -Uvh --replacepkgs "%s"; fi`, remotePkg, remotePkg)
		case "binary":
			install = fmt.Sprintf(`install -D -m 0755 "%s" "%s"`, remotePkg, rd.treadmarkPath)
		}
		if err := mustRun("installing treadmark", sh(install), 10*time.Minute); err != nil {
			return nil, err
		}
	}

	if c.ConfigSource != "" {
		remoteCfg := path.Join(rd.stagingDir, "treadmark-config-upload")
		if err := r.uploadFile(c.ConfigSource, remoteCfg); err != nil {
			return nil, err
		}
		if err := mustRun("installing treadmark config",
			sh(fmt.Sprintf(`install -D -m 0644 "%s" "%s"`, remoteCfg, rd.configPath)), time.Minute); err != nil {
			return nil, err
		}
	} else if rd.method == "none" {
		if code, err := r.run(ctx, fmt.Sprintf(`test -f "%s"`, rd.configPath), 30*time.Second); err != nil || code != 0 {
			return nil, fmt.Errorf("install_method none and no config at %s in the guest; set config_source", rd.configPath)
		}
	}

	out, code, err := r.capture(ctx, fmt.Sprintf(`"%s" --version`, rd.treadmarkPath), time.Minute)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("treadmark not runnable at %s (exit %d): %v", rd.treadmarkPath, code, err)
	}
	res.treadmarkVersion = tmcli.ParseVersion(out)

	r.ui.Say("treadmark: capturing baseline (files init)")
	if err := mustRun("treadmark files init",
		tm(fmt.Sprintf(`files init --config "%s" --force`, rd.configPath)), rd.initTimeout); err != nil {
		return nil, err
	}

	// Verification scans; the first report format rides the verify scan.
	// Each additional format is another full filesystem walk.
	if !c.SkipVerify || len(c.ReportFormats) > 0 {
		formats := c.ReportFormats
		if len(formats) == 0 {
			formats = []string{""}
		}
		for _, f := range formats {
			args := fmt.Sprintf(`files scan --config "%s"`, rd.configPath)
			if f != "" {
				args += fmt.Sprintf(` --report "%s/init-scan.%s"`, rd.stagingDir, f)
			}
			code, err := r.run(ctx, tm(args), rd.initTimeout)
			if err != nil {
				return nil, err
			}
			warn, err := tmcli.EvalScanExit(code, c.OnDrift)
			if err != nil {
				return nil, err
			}
			if warn != "" {
				r.ui.Error("treadmark: " + warn)
			}
			if f != "" {
				res.reports = append(res.reports, "init-scan."+f)
			}
		}
	}

	out, code, err = r.capture(ctx, tm(fmt.Sprintf(`baseline info --config "%s" --json`, rd.configPath)), 2*time.Minute)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("treadmark baseline info failed (exit %d): %v", code, err)
	}
	res.baselineInfoJSON = []byte(out)

	if !c.SkipDownload {
		// The baseline dir is 0700 root; stage a world-readable copy the
		// connecting user can scp out.
		staged := path.Join(rd.stagingDir, "baseline.db")
		if err := mustRun("staging baseline for download",
			sh(fmt.Sprintf(`cp "%s" "%s" && chmod 0644 "%s"`, rd.dbPath, staged, staged)), 5*time.Minute); err != nil {
			return nil, err
		}
		if err := r.downloadFile(staged, filepath.Join(c.OutputDir, "baseline.db")); err != nil {
			return nil, err
		}
		res.baselineOnHost = true
		if len(res.reports) > 0 {
			// The scan writes reports into the staging dir itself, running
			// under sudo with the CALLING session's umask -- on CIS-hardened
			// guests (pam umask 027) they land 0640 root:root and the
			// connecting user's scp is denied. Make them world-readable
			// explicitly, same treatment as the staged baseline copy.
			if err := mustRun("staging reports for download",
				sh(fmt.Sprintf(`chmod 0644 "%s"/init-scan.*`, rd.stagingDir)), time.Minute); err != nil {
				return nil, err
			}
		}
		for _, rel := range res.reports {
			if err := r.downloadFile(path.Join(rd.stagingDir, rel), filepath.Join(c.OutputDir, rel)); err != nil {
				return nil, err
			}
		}
	}

	if c.RemoveFromImage {
		r.ui.Say("treadmark: removing baseline from the image (capture-only)")
		if err := mustRun("removing baseline from image",
			sh(fmt.Sprintf(`rm -f "%s"`, rd.dbPath)), time.Minute); err != nil {
			return nil, err
		}
	}

	return res, nil
}
