package treadmark

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prepare(t *testing.T, raw map[string]interface{}) (*Provisioner, error) {
	t.Helper()
	p := &Provisioner{}
	return p, p.Prepare(raw)
}

func TestPrepareDefaults(t *testing.T) {
	p, err := prepare(t, map[string]interface{}{
		"packer_build_name": "alma10",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := p.config
	if c.OS != "auto" || c.InstallMethod != "auto" || c.TreadmarkVersion != "latest" || c.OnDrift != "error" {
		t.Fatalf("defaults: %+v", c)
	}
	if c.OutputDir != filepath.Join("treadmark-output", "alma10") {
		t.Fatalf("output_dir default: %s", c.OutputDir)
	}
}

func TestPrepareErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]interface{}
		want string
	}{
		{"bad-os", map[string]interface{}{"os": "plan9"}, "os must be"},
		{"msi-on-linux", map[string]interface{}{"os": "linux", "install_method": "msi"}, "not valid with os = linux"},
		{"deb-on-windows", map[string]interface{}{"os": "windows", "install_method": "deb"}, "not valid with os = windows"},
		{"scope-all-linux", map[string]interface{}{"os": "linux", "scope": "all"}, "Windows-only"},
		{"bad-scope", map[string]interface{}{"scope": "everything"}, "scope must be"},
		{"bad-drift", map[string]interface{}{"on_drift": "ignore"}, "on_drift must be"},
		{"bad-format", map[string]interface{}{"report_formats": []string{"pdf"}}, "not a treadmark report format"},
		{"binary-no-config", map[string]interface{}{"install_method": "binary"}, "requires config_source"},
		{"latest-custom-base", map[string]interface{}{"download_url_base": "https://mirror.internal/tm"}, "cannot be resolved against a custom"},
		{"missing-package", map[string]interface{}{"package_path": "/does/not/exist.deb"}, "package_path"},
		{"bad-arch", map[string]interface{}{"arch": "mips"}, "arch must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepare(t, tc.raw)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestPreparePackagePathConflicts(t *testing.T) {
	dir := t.TempDir()
	deb := filepath.Join(dir, "treadmark_0.11.0_amd64.deb")
	if err := os.WriteFile(deb, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// package_path + pinned version: conflict.
	if _, err := prepare(t, map[string]interface{}{
		"package_path":      deb,
		"treadmark_version": "0.10.0",
	}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutually-exclusive error, got %v", err)
	}

	// Extension/method mismatch.
	if _, err := prepare(t, map[string]interface{}{
		"package_path":   deb,
		"install_method": "rpm",
	}); err == nil || !strings.Contains(err.Error(), "implies install_method") {
		t.Fatalf("want extension mismatch error, got %v", err)
	}

	// Happy: extension implies method under auto.
	if _, err := prepare(t, map[string]interface{}{"package_path": deb}); err != nil {
		t.Fatal(err)
	}
}

func TestGuestDBPathSniffsConfigSource(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tm.yaml")
	if err := os.WriteFile(cfg, []byte("db_path: /opt/custom/baseline.db\npaths:\n  - /etc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := prepare(t, map[string]interface{}{"config_source": cfg})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.guestDBPath("linux"); got != "/opt/custom/baseline.db" {
		t.Fatalf("db path: %s", got)
	}

	p2, err := prepare(t, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if got := p2.guestDBPath("linux"); got != "/var/lib/treadmark/baseline.db" {
		t.Fatalf("default db path: %s", got)
	}
	if got := p2.guestDBPath("windows"); got != `C:\ProgramData\Treadmark\baseline.db` {
		t.Fatalf("windows default db path: %s", got)
	}
}

func TestResolveRuntimeRejectsUnsafePaths(t *testing.T) {
	p, err := prepare(t, map[string]interface{}{
		"os":          "linux",
		"staging_dir": `/var/tmp/pwn"; rm -rf /; echo "`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.resolveRuntime("linux", "amd64"); err == nil || !strings.Contains(err.Error(), "metacharacters") {
		t.Fatalf("want metacharacter rejection, got %v", err)
	}
}

func TestResolveRuntimeDefaults(t *testing.T) {
	p, err := prepare(t, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := p.resolveRuntime("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if rd.treadmarkPath != "/usr/bin/treadmark" || rd.configPath != "/etc/treadmark/treadmark.yaml" ||
		rd.stagingDir != "/var/tmp/packer-treadmark" || rd.scope != "files" || !rd.sudo {
		t.Fatalf("linux defaults: %+v", rd)
	}
	if rd.initTimeout.Minutes() != 15 {
		t.Fatalf("linux timeout: %s", rd.initTimeout)
	}

	rdw, err := p.resolveRuntime("windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if rdw.treadmarkPath != `C:\Program Files\Treadmark\treadmark.exe` || rdw.scope != "all" || rdw.method != "msi" {
		t.Fatalf("windows defaults: %+v", rdw)
	}
	if rdw.initTimeout.Minutes() != 45 {
		t.Fatalf("windows timeout: %s", rdw.initTimeout)
	}
}
