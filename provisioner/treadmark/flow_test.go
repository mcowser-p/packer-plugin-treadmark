package treadmark

// Full-flow tests over a scripted mock communicator: every guest command the
// provisioner issues is asserted verbatim (golden strings), then the
// host-side bundle is checked. No network, no real guests.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"

	"github.com/mcowser-p/packer-plugin-treadmark/internal/metadata"
)

type mockStep struct {
	exit   int
	stdout string
}

type mockComm struct {
	t         *testing.T
	steps     []mockStep
	commands  []string
	uploads   map[string][]byte
	downloads map[string][]byte
}

func newMockComm(t *testing.T, steps []mockStep, downloads map[string][]byte) *mockComm {
	return &mockComm{
		t:         t,
		steps:     steps,
		uploads:   map[string][]byte{},
		downloads: downloads,
	}
}

func (m *mockComm) Start(ctx context.Context, cmd *packersdk.RemoteCmd) error {
	i := len(m.commands)
	m.commands = append(m.commands, cmd.Command)
	if i >= len(m.steps) {
		m.t.Fatalf("unexpected command #%d: %q (script exhausted)", i, cmd.Command)
	}
	st := m.steps[i]
	if st.stdout != "" && cmd.Stdout != nil {
		_, _ = cmd.Stdout.Write([]byte(st.stdout))
	}
	cmd.SetExited(st.exit)
	return nil
}

func (m *mockComm) Upload(dst string, src io.Reader, fi *os.FileInfo) error {
	b, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	m.uploads[dst] = b
	return nil
}

func (m *mockComm) UploadDir(dst string, src string, exclude []string) error {
	return fmt.Errorf("not implemented")
}

func (m *mockComm) Download(src string, dst io.Writer) error {
	b, ok := m.downloads[src]
	if !ok {
		return fmt.Errorf("no canned download for %s", src)
	}
	_, err := dst.Write(b)
	return err
}

func (m *mockComm) DownloadDir(src string, dst string, exclude []string) error {
	return fmt.Errorf("not implemented")
}

func testUi() packersdk.Ui {
	return &packersdk.BasicUi{
		Reader:      strings.NewReader(""),
		Writer:      io.Discard,
		ErrorWriter: io.Discard,
	}
}

func infoJSON(dbSHA string) string {
	return fmt.Sprintf(`{"db_path": "/var/lib/treadmark/baseline.db", "db_sha256": "%s", "db_size_bytes": 6, "tracked_entries": 42, "baseline_created_at": "2026-09-04T10:00:00Z", "baseline_host": "packer-build", "baseline_os": "linux"}`, dbSHA)
}

func TestLinuxFlowGolden(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "out")
	pkgDir := t.TempDir()
	deb := filepath.Join(pkgDir, "treadmark_0.11.0_amd64.deb")
	if err := os.WriteFile(deb, []byte("fake-deb"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgSrc := filepath.Join(pkgDir, "golden.yaml")
	cfgContent := "paths:\n  - /etc\nstore_content: false\n"
	if err := os.WriteFile(cfgSrc, []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}

	dbBytes := []byte("DBDATA")
	dbSHA := sha256.Sum256(dbBytes)
	dbSHAHex := hex.EncodeToString(dbSHA[:])

	p := &Provisioner{}
	if err := p.Prepare(map[string]interface{}{
		"packer_build_name": "alma10",
		"install_method":    "deb",
		"package_path":      deb,
		"config_source":     cfgSrc,
		"report_formats":    []string{"json"},
		"output_dir":        outDir,
		"metadata":          map[string]string{"distro": "almalinux"},
	}); err != nil {
		t.Fatal(err)
	}

	steps := []mockStep{
		{0, "Linux x86_64\n"},     // probe
		{0, ""},                   // mkdir staging
		{0, ""},                   // install deb
		{0, ""},                   // install config
		{0, "treadmark 0.11.0\n"}, // --version
		{0, ""},                   // files init
		{0, ""},                   // files scan --report
		{0, infoJSON(dbSHAHex)},   // baseline info
		{0, ""},                   // cp/chmod staging copy
		{0, ""},                   // deferred staging cleanup
	}
	comm := newMockComm(t, steps, map[string][]byte{
		"/var/tmp/packer-treadmark/baseline.db":    dbBytes,
		"/var/tmp/packer-treadmark/init-scan.json": []byte(`{"drift": []}`),
	})

	if err := p.Provision(context.Background(), testUi(), comm, map[string]interface{}{
		"PackerRunUUID": "uuid-123",
	}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		`uname -s -m`,
		`mkdir -p "/var/tmp/packer-treadmark" && chmod 0755 "/var/tmp/packer-treadmark"`,
		`sudo sh -c 'dpkg -i "/var/tmp/packer-treadmark/treadmark_0.11.0_amd64.deb"'`,
		`sudo sh -c 'install -D -m 0644 "/var/tmp/packer-treadmark/treadmark-config-upload" "/etc/treadmark/treadmark.yaml"'`,
		`"/usr/bin/treadmark" --version`,
		`sudo "/usr/bin/treadmark" files init --config "/etc/treadmark/treadmark.yaml" --force`,
		`sudo "/usr/bin/treadmark" files scan --config "/etc/treadmark/treadmark.yaml" --report "/var/tmp/packer-treadmark/init-scan.json"`,
		`sudo "/usr/bin/treadmark" baseline info --config "/etc/treadmark/treadmark.yaml" --json`,
		`sudo sh -c 'cp "/var/lib/treadmark/baseline.db" "/var/tmp/packer-treadmark/baseline.db" && chmod 0644 "/var/tmp/packer-treadmark/baseline.db"'`,
		`sudo sh -c 'rm -rf "/var/tmp/packer-treadmark"'`,
	}
	if len(comm.commands) != len(want) {
		t.Fatalf("command count %d != %d:\n%s", len(comm.commands), len(want), strings.Join(comm.commands, "\n"))
	}
	for i := range want {
		if comm.commands[i] != want[i] {
			t.Errorf("command %d:\n got %s\nwant %s", i, comm.commands[i], want[i])
		}
	}

	if got := comm.uploads["/var/tmp/packer-treadmark/treadmark_0.11.0_amd64.deb"]; string(got) != "fake-deb" {
		t.Fatalf("package upload: %q", got)
	}
	if got := comm.uploads["/var/tmp/packer-treadmark/treadmark-config-upload"]; string(got) != cfgContent {
		t.Fatalf("config upload: %q", got)
	}

	// Host bundle.
	for _, f := range []string{"baseline.db", "init-scan.json", "baseline-info.json", "metadata.json", "SHA256SUMS"} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
			t.Errorf("bundle missing %s: %v", f, err)
		}
	}
	sc, err := metadata.Load(filepath.Join(outDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Mode != "in-guest" || sc.OS != "linux" || sc.Scope != "files" {
		t.Fatalf("sidecar: %+v", sc)
	}
	if sc.TreadmarkVersion != "0.11.0" || sc.PackerRunUUID != "uuid-123" || sc.BuildName != "alma10" {
		t.Fatalf("sidecar ids: %+v", sc)
	}
	if sc.Baseline == nil || sc.Baseline.SHA256 != dbSHAHex || sc.Baseline.Entries != 42 {
		t.Fatalf("sidecar baseline: %+v", sc.Baseline)
	}
	if len(sc.Reports) != 1 || sc.Reports[0] != "init-scan.json" {
		t.Fatalf("sidecar reports: %v", sc.Reports)
	}
	if sc.Custom["distro"] != "almalinux" {
		t.Fatalf("sidecar custom: %v", sc.Custom)
	}
}

func TestLinuxFlowTransitCorruption(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "out")
	pkgDir := t.TempDir()
	deb := filepath.Join(pkgDir, "t.deb")
	if err := os.WriteFile(deb, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provisioner{}
	if err := p.Prepare(map[string]interface{}{
		"install_method": "deb",
		"package_path":   deb,
		"skip_verify":    true,
		"output_dir":     outDir,
	}); err != nil {
		t.Fatal(err)
	}

	steps := []mockStep{
		{0, "Linux x86_64\n"},
		{0, ""}, // mkdir
		{0, ""}, // install
		{0, "treadmark 0.11.0\n"},
		{0, ""}, // init
		{0, infoJSON("1111111111111111111111111111111111111111111111111111111111111111")},
		{0, ""}, // cp
		{0, ""}, // cleanup
	}
	comm := newMockComm(t, steps, map[string][]byte{
		"/var/tmp/packer-treadmark/baseline.db": []byte("DIFFERENT-BYTES"),
	})
	err := p.Provision(context.Background(), testUi(), comm, map[string]interface{}{})
	if err == nil || !strings.Contains(err.Error(), "corrupted in transit") {
		t.Fatalf("want transit corruption error, got %v", err)
	}
}

func TestLinuxFlowDriftPolicy(t *testing.T) {
	for _, tc := range []struct {
		onDrift string
		wantErr bool
	}{{"error", true}, {"warn", false}} {
		t.Run(tc.onDrift, func(t *testing.T) {
			outDir := filepath.Join(t.TempDir(), "out")
			pkgDir := t.TempDir()
			deb := filepath.Join(pkgDir, "t.deb")
			if err := os.WriteFile(deb, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			p := &Provisioner{}
			if err := p.Prepare(map[string]interface{}{
				"install_method": "deb",
				"package_path":   deb,
				"on_drift":       tc.onDrift,
				"output_dir":     outDir,
			}); err != nil {
				t.Fatal(err)
			}
			steps := []mockStep{
				{0, "Linux x86_64\n"},
				{0, ""}, // mkdir
				{0, ""}, // install
				{0, "treadmark 0.11.0\n"},
				{0, ""}, // init
				{1, ""}, // verify scan: DRIFT
			}
			if !tc.wantErr {
				steps = append(steps,
					mockStep{0, infoJSON("")}, // baseline info
					mockStep{0, ""},           // cp
				)
			}
			steps = append(steps, mockStep{0, ""}) // cleanup
			comm := newMockComm(t, steps, map[string][]byte{
				"/var/tmp/packer-treadmark/baseline.db": []byte("DBDATA"),
			})
			err := p.Provision(context.Background(), testUi(), comm, map[string]interface{}{})
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "drift")) {
				t.Fatalf("want drift error, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("warn policy should pass: %v", err)
			}
		})
	}
}

func TestWindowsFlowGolden(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "out")
	pkgDir := t.TempDir()
	msi := filepath.Join(pkgDir, "treadmark-0.11.0.msi")
	if err := os.WriteFile(msi, []byte("fake-msi"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgSrc := filepath.Join(pkgDir, "golden-win.yaml")
	if err := os.WriteFile(cfgSrc, []byte("paths:\n  - C:\\Windows\\System32\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbBytes := []byte("WINDB")
	dbSHA := sha256.Sum256(dbBytes)
	dbSHAHex := hex.EncodeToString(dbSHA[:])

	p := &Provisioner{}
	if err := p.Prepare(map[string]interface{}{
		"os":             "windows",
		"install_method": "msi",
		"package_path":   msi,
		"config_source":  cfgSrc,
		"report_formats": []string{"json"},
		"output_dir":     outDir,
	}); err != nil {
		t.Fatal(err)
	}

	winInfo := strings.ReplaceAll(infoJSON(dbSHAHex), "linux", "windows")
	steps := []mockStep{
		{0, ""},                   // staging New-Item
		{0, ""},                   // install.ps1
		{0, ""},                   // config.ps1
		{0, "treadmark 0.11.0\n"}, // version.ps1
		{0, ""},                   // init.ps1
		{0, ""},                   // verify.ps1 (all scan)
		{0, ""},                   // report-json.ps1
		{0, `{"hives": []}`},      // registry-scan.ps1
		{0, winInfo},              // info.ps1
		{0, ""},                   // stage-db.ps1
		{0, ""},                   // cleanup Remove-Item
	}
	comm := newMockComm(t, steps, map[string][]byte{
		"C:/Windows/Temp/packer-treadmark/baseline.db":    dbBytes,
		"C:/Windows/Temp/packer-treadmark/init-scan.json": []byte(`{}`),
	})

	if err := p.Provision(context.Background(), testUi(), comm, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}

	// Command lines: scripts are invoked via powershell -File.
	wantCmds := []string{
		`powershell -NoProfile -Command "New-Item -ItemType Directory -Force -Path 'C:\Windows\Temp\packer-treadmark' | Out-Null"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\install.ps1"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\config.ps1"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\version.ps1"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\init.ps1"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\verify.ps1"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\report-json.ps1"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\registry-scan.ps1"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\info.ps1"`,
		`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Windows\Temp\packer-treadmark\stage-db.ps1"`,
		`powershell -NoProfile -Command "Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -Path 'C:\Windows\Temp\packer-treadmark'"`,
	}
	if len(comm.commands) != len(wantCmds) {
		t.Fatalf("command count %d != %d:\n%s", len(comm.commands), len(wantCmds), strings.Join(comm.commands, "\n"))
	}
	for i := range wantCmds {
		if comm.commands[i] != wantCmds[i] {
			t.Errorf("command %d:\n got %s\nwant %s", i, comm.commands[i], wantCmds[i])
		}
	}

	// Script contents (golden where it matters).
	install := string(comm.uploads["C:/Windows/Temp/packer-treadmark/install.ps1"])
	for _, frag := range []string{
		"msiexec.exe",
		`'/i','C:\Windows\Temp\packer-treadmark\treadmark-0.11.0.msi'`,
		"'/qn','/norestart','/l*v'",
		"3010",
		// Failure evidence: the log tail must be dumped to the build output
		// before the deferred staging cleanup deletes the log file.
		`Get-Content 'C:\Windows\Temp\packer-treadmark\msi-install.log' -Tail 120`,
		// ...and emitted as raw UTF-8 bytes: PS 5.1 host output over a raw
		// SSH exec channel is UTF-16LE, unreadable in the packer log.
		"[Console]::OpenStandardOutput()",
	} {
		if !strings.Contains(install, frag) {
			t.Errorf("install.ps1 missing %q:\n%s", frag, install)
		}
	}
	initPS := string(comm.uploads["C:/Windows/Temp/packer-treadmark/init.ps1"])
	wantInit := "& 'C:\\Program Files\\Treadmark\\treadmark.exe' all init --config 'C:\\ProgramData\\Treadmark\\treadmark.yaml' --force\nexit $LASTEXITCODE\n"
	if initPS != wantInit {
		t.Errorf("init.ps1:\n got %q\nwant %q", initPS, wantInit)
	}

	// Bundle: registry scan JSON captured host-side.
	for _, f := range []string{"baseline.db", "init-scan.json", "registry-scan.json", "baseline-info.json", "metadata.json", "SHA256SUMS"} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
			t.Errorf("bundle missing %s: %v", f, err)
		}
	}
	sc, err := metadata.Load(filepath.Join(outDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.OS != "windows" || sc.Scope != "all" || sc.Baseline.SHA256 != dbSHAHex {
		t.Fatalf("sidecar: %+v baseline %+v", sc, sc.Baseline)
	}
}

func TestWindowsFlowMSIFailure(t *testing.T) {
	pkgDir := t.TempDir()
	msi := filepath.Join(pkgDir, "treadmark-0.11.0.msi")
	if err := os.WriteFile(msi, []byte("fake-msi"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provisioner{}
	if err := p.Prepare(map[string]interface{}{
		"os":             "windows",
		"install_method": "msi",
		"package_path":   msi,
		"output_dir":     filepath.Join(t.TempDir(), "out"),
	}); err != nil {
		t.Fatal(err)
	}

	// install.ps1 exits 1 (its msiexec branch already dumped the log tail to
	// the UI stream); the provisioner must fail with the triage hint and
	// still run the deferred staging cleanup.
	steps := []mockStep{
		{0, ""}, // staging New-Item
		{1, ""}, // install.ps1: msiexec failed
		{0, ""}, // deferred staging cleanup
	}
	comm := newMockComm(t, steps, nil)
	err := p.Provision(context.Background(), testUi(), comm, map[string]interface{}{})
	if err == nil {
		t.Fatal("want install failure, got success")
	}
	for _, frag := range []string{"installing treadmark MSI failed (exit 1)", "1618", "1603"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error missing %q: %v", frag, err)
		}
	}
	last := comm.commands[len(comm.commands)-1]
	if !strings.Contains(last, "Remove-Item") {
		t.Errorf("staging cleanup did not run; last command: %s", last)
	}
}
