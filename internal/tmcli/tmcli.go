// Package tmcli builds treadmark command fragments and interprets treadmark
// exit codes. treadmark's exit codes are API (see treadmark CLAUDE.md):
// 0 = matches baseline, 1 = drift found, 2 = error.
package tmcli

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	ExitClean = 0
	ExitDrift = 1
	ExitError = 2
)

// Report formats treadmark infers from the --report path extension.
var ReportFormats = []string{"json", "ndjson", "csv", "sarif", "md", "html", "txt"}

// ValidReportFormat reports whether f is a format treadmark can write.
func ValidReportFormat(f string) bool {
	for _, v := range ReportFormats {
		if f == v {
			return true
		}
	}
	return false
}

// EvalScanExit maps a treadmark scan/verify exit code to a build outcome
// under an on_drift policy ("error" or "warn"). A non-empty warning string
// means the build continues with a warning; a non-nil error fails the build.
// Exit 2 always fails regardless of policy.
func EvalScanExit(code int, onDrift string) (string, error) {
	switch code {
	case ExitClean:
		return "", nil
	case ExitDrift:
		msg := "treadmark scan reported drift against the just-captured baseline (exit 1)"
		if onDrift == "warn" {
			return msg, nil
		}
		return "", fmt.Errorf("%s; the config monitors paths that changed mid-build — tune the exclude list or set on_drift = \"warn\"", msg)
	case ExitError:
		return "", fmt.Errorf("treadmark exited 2 (error)")
	default:
		return "", fmt.Errorf("treadmark exited with unexpected code %d", code)
	}
}

// ShQuote single-quotes s for a POSIX shell.
func ShQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// PSQuote single-quotes s for PowerShell.
func PSQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

var versionRe = regexp.MustCompile(`treadmark\s+v?(\d+\.\d+\.\d+[^\s]*)`)

// ParseVersion extracts a bare version ("0.11.0") from `treadmark --version`
// output. Unrecognized output is returned trimmed, never empty-on-match.
func ParseVersion(out string) string {
	if m := versionRe.FindStringSubmatch(out); len(m) == 2 {
		return m[1]
	}
	return strings.TrimSpace(out)
}
