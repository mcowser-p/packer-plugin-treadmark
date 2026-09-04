package tmcli

import (
	"strings"
	"testing"
)

func TestEvalScanExit(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		onDrift string
		wantErr bool
		warn    bool
	}{
		{"clean-error", 0, "error", false, false},
		{"clean-warn", 0, "warn", false, false},
		{"drift-error", 1, "error", true, false},
		{"drift-warn", 1, "warn", false, true},
		{"error-error", 2, "error", true, false},
		{"error-warn", 2, "warn", true, false}, // exit 2 always fails
		{"weird-code", 7, "warn", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, err := EvalScanExit(tc.code, tc.onDrift)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if (warn != "") != tc.warn {
				t.Fatalf("warn = %q, want warn %v", warn, tc.warn)
			}
		})
	}
}

func TestShQuote(t *testing.T) {
	if got := ShQuote("/plain/path"); got != "'/plain/path'" {
		t.Fatalf("got %s", got)
	}
	if got := ShQuote("it's"); got != `'it'"'"'s'` {
		t.Fatalf("got %s", got)
	}
}

func TestPSQuote(t *testing.T) {
	if got := PSQuote(`C:\Program Files\Treadmark\treadmark.exe`); got != `'C:\Program Files\Treadmark\treadmark.exe'` {
		t.Fatalf("got %s", got)
	}
	if got := PSQuote("it's"); got != "'it''s'" {
		t.Fatalf("got %s", got)
	}
}

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"treadmark 0.11.0":        "0.11.0",
		"treadmark v0.11.0":       "0.11.0",
		"treadmark 0.11.0\n":      "0.11.0",
		"something else entirely": "something else entirely",
	}
	for in, want := range cases {
		if got := ParseVersion(in); got != strings.TrimSpace(want) {
			t.Errorf("ParseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidReportFormat(t *testing.T) {
	for _, f := range ReportFormats {
		if !ValidReportFormat(f) {
			t.Errorf("%s should be valid", f)
		}
	}
	for _, f := range []string{"", "pdf", "yaml", "JSON"} {
		if ValidReportFormat(f) {
			t.Errorf("%s should be invalid", f)
		}
	}
}
