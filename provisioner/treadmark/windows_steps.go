package treadmark

// In-guest capture flow for Windows guests. Commands run as uploaded .ps1
// scripts via `powershell -File`, which behaves identically whether the SSH
// DefaultShell is PowerShell (holy-qcow images) or cmd. The MSI's PATH entry
// is invisible to the already-open SSH session, so treadmark.exe is always
// invoked by full path (documented gotcha in treadmark windows/README.md).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/tmcli"
)

// winJoin joins Windows path segments with backslashes.
func winJoin(parts ...string) string {
	return strings.Join(parts, `\`)
}

// winDir returns the directory part of a Windows path.
func winDir(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i > 0 {
		return p[:i]
	}
	return p
}

// fslash converts a Windows path for the communicator's SFTP-based
// Upload/Download, which is happier with forward slashes.
func fslash(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}

func (p *Provisioner) provisionWindows(ctx context.Context, r *runner, rd *resolved, pkgPath string) (*guestResults, error) {
	c := &p.config
	res := &guestResults{}
	stg := rd.stagingDir
	pq := tmcli.PSQuote

	scratch, err := os.MkdirTemp("", "packer-treadmark-ps-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)

	// stagePS uploads a named script; runPS/capPS execute it.
	stagePS := func(name, script string) (string, error) {
		local := filepath.Join(scratch, name+".ps1")
		if err := os.WriteFile(local, []byte(script), 0o644); err != nil {
			return "", err
		}
		remote := winJoin(stg, name+".ps1")
		if err := r.uploadFile(local, fslash(remote)); err != nil {
			return "", err
		}
		return fmt.Sprintf(`powershell -NoProfile -ExecutionPolicy Bypass -File "%s"`, remote), nil
	}
	runPS := func(name, script string, timeout time.Duration) (int, error) {
		cmdline, err := stagePS(name, script)
		if err != nil {
			return -1, err
		}
		return r.run(ctx, cmdline, timeout)
	}
	capPS := func(name, script string, timeout time.Duration) (string, int, error) {
		cmdline, err := stagePS(name, script)
		if err != nil {
			return "", -1, err
		}
		return r.capture(ctx, cmdline, timeout)
	}
	mustPS := func(what, name, script string, timeout time.Duration) error {
		code, err := runPS(name, script, timeout)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if code != 0 {
			return fmt.Errorf("%s failed (exit %d)", what, code)
		}
		return nil
	}

	// Staging dir first — every script upload depends on it. This one runs
	// as a direct command since no script can exist yet.
	mk := fmt.Sprintf(`powershell -NoProfile -Command "New-Item -ItemType Directory -Force -Path '%s' | Out-Null"`, stg)
	if code, err := r.run(ctx, mk, time.Minute); err != nil || code != 0 {
		return nil, fmt.Errorf("creating staging dir %s (exit %d): %v", stg, code, err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		rm := fmt.Sprintf(`powershell -NoProfile -Command "Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -Path '%s'"`, stg)
		_, _ = r.run(cctx, rm, time.Minute)
	}()

	if rd.method == "msi" {
		if pkgPath == "" {
			return nil, fmt.Errorf("internal error: no MSI resolved")
		}
		remoteMsi := winJoin(stg, filepath.Base(pkgPath))
		msiLog := winJoin(stg, "msi-install.log")
		r.ui.Say(fmt.Sprintf("treadmark: uploading %s", filepath.Base(pkgPath)))
		if err := r.uploadFile(pkgPath, fslash(remoteMsi)); err != nil {
			return nil, err
		}
		install := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$p = Start-Process -FilePath 'msiexec.exe' -ArgumentList @('/i',%s,'/qn','/norestart','/l*v',%s) -Wait -PassThru
if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) {
  Write-Error ('msiexec exited ' + $p.ExitCode + '; see the install log in the staging dir')
  exit 1
}
exit 0
`, pq(remoteMsi), pq(msiLog))
		if err := mustPS("installing treadmark MSI", "install", install, 10*time.Minute); err != nil {
			return nil, fmt.Errorf("%w (msiexec log was at %s; exit 1618 = another install in progress, 1603 = fatal — rerun with the log preserved via skip cleanup)", err, msiLog)
		}
	}

	if c.ConfigSource != "" {
		remoteCfg := winJoin(stg, "treadmark-config-upload")
		if err := r.uploadFile(c.ConfigSource, fslash(remoteCfg)); err != nil {
			return nil, err
		}
		cfgScript := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path %s | Out-Null
Copy-Item -Force %s %s
exit 0
`, pq(winDir(rd.configPath)), pq(remoteCfg), pq(rd.configPath))
		if err := mustPS("installing treadmark config", "config", cfgScript, time.Minute); err != nil {
			return nil, err
		}
	} else if rd.method == "none" {
		check := fmt.Sprintf(`if (-not (Test-Path %s)) { Write-Error 'no treadmark config in guest; set config_source'; exit 1 }
exit 0
`, pq(rd.configPath))
		if err := mustPS("checking for treadmark config", "config-check", check, time.Minute); err != nil {
			return nil, err
		}
	}

	out, code, err := capPS("version", fmt.Sprintf("& %s --version\nexit $LASTEXITCODE\n", pq(rd.treadmarkPath)), 2*time.Minute)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("treadmark not runnable at %s (exit %d): %v", rd.treadmarkPath, code, err)
	}
	res.treadmarkVersion = tmcli.ParseVersion(out)

	r.ui.Say(fmt.Sprintf("treadmark: capturing baseline (%s init)", rd.scope))
	initScript := fmt.Sprintf("& %s %s init --config %s --force\nexit $LASTEXITCODE\n", pq(rd.treadmarkPath), rd.scope, pq(rd.configPath))
	if err := mustPS("treadmark "+rd.scope+" init", "init", initScript, rd.initTimeout); err != nil {
		return nil, err
	}

	evalScan := func(code int) error {
		warn, err := tmcli.EvalScanExit(code, c.OnDrift)
		if err != nil {
			return err
		}
		if warn != "" {
			r.ui.Error("treadmark: " + warn)
		}
		return nil
	}

	if !c.SkipVerify {
		verify := fmt.Sprintf("& %s %s scan --config %s\nexit $LASTEXITCODE\n", pq(rd.treadmarkPath), rd.scope, pq(rd.configPath))
		code, err := runPS("verify", verify, rd.initTimeout)
		if err != nil {
			return nil, err
		}
		if err := evalScan(code); err != nil {
			return nil, err
		}
	}

	// File-half reports (`all` accepts no --report flag, so reports always
	// come from `files scan`; the registry half is captured as JSON below).
	if rd.scope != "registry" {
		for _, f := range c.ReportFormats {
			rel := "init-scan." + f
			scan := fmt.Sprintf("& %s files scan --config %s --report %s\nexit $LASTEXITCODE\n",
				pq(rd.treadmarkPath), pq(rd.configPath), pq(winJoin(stg, rel)))
			code, err := runPS("report-"+f, scan, rd.initTimeout)
			if err != nil {
				return nil, err
			}
			if err := evalScan(code); err != nil {
				return nil, err
			}
			res.reports = append(res.reports, rel)
		}
	}
	if rd.scope != "files" && len(c.ReportFormats) > 0 {
		regScan := fmt.Sprintf("& %s registry scan --config %s --json\nexit $LASTEXITCODE\n", pq(rd.treadmarkPath), pq(rd.configPath))
		out, code, err := capPS("registry-scan", regScan, rd.initTimeout)
		if err != nil {
			return nil, err
		}
		if err := evalScan(code); err != nil {
			return nil, err
		}
		res.registryScanJSON = []byte(out)
	}

	out, code, err = capPS("info", fmt.Sprintf("& %s baseline info --config %s --json\nexit $LASTEXITCODE\n", pq(rd.treadmarkPath), pq(rd.configPath)), 2*time.Minute)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("treadmark baseline info failed (exit %d): %v", code, err)
	}
	res.baselineInfoJSON = []byte(out)

	if !c.SkipDownload {
		staged := winJoin(stg, "baseline.db")
		stage := fmt.Sprintf("$ErrorActionPreference = 'Stop'\nCopy-Item -Force %s %s\nexit 0\n", pq(rd.dbPath), pq(staged))
		if err := mustPS("staging baseline for download", "stage-db", stage, 5*time.Minute); err != nil {
			return nil, err
		}
		if err := r.downloadFile(fslash(staged), filepath.Join(c.OutputDir, "baseline.db")); err != nil {
			return nil, err
		}
		res.baselineOnHost = true
		for _, rel := range res.reports {
			if err := r.downloadFile(fslash(winJoin(stg, rel)), filepath.Join(c.OutputDir, rel)); err != nil {
				return nil, err
			}
		}
	}

	if c.RemoveFromImage {
		r.ui.Say("treadmark: removing baseline from the image (capture-only)")
		rm := fmt.Sprintf("Remove-Item -Force -ErrorAction Stop %s\nexit 0\n", pq(rd.dbPath))
		if err := mustPS("removing baseline from image", "remove-db", rm, time.Minute); err != nil {
			return nil, err
		}
	}

	return res, nil
}
