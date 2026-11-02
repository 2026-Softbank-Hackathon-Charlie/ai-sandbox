package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/rehearsal"
)

func TestReportRehearsalSeparatesWarningsFromExecution(t *testing.T) {
	for _, tc := range []struct {
		name, report string
		err          error
		code         int
		warning      bool
	}{
		{"pass", `{"passed":true}`, nil, 0, false},
		{"pass with delivery warning", `{"passed":true,"warnings":["log delivery incomplete password=do-not-print"]}`, nil, 0, true},
		{"execution failed with warning", `{"passed":false,"warnings":["cleanup failed password=do-not-print"]}`, nil, 1, true},
		{"fatal", `{"passed":true}`, errors.New("execution interrupted"), 1, false},
		{"no report", "null", nil, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rep *rehearsal.Report
			if err := json.Unmarshal([]byte(tc.report), &rep); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			printRehearsalWarnings(&out, rep)
			if code := reportRehearsal(&out, rep, tc.err, "pln_test", 4); code != tc.code {
				t.Fatalf("exit=%d, want %d", code, tc.code)
			}
			text := out.String()
			wantWarnings := 0
			if tc.warning {
				wantWarnings = 1
			}
			if strings.Count(text, "경고:") != wantWarnings || strings.Contains(text, "do-not-print") {
				t.Fatalf("warning missing or secret leaked: %s", text)
			}
			if strings.Contains(text, "승인하지 마세요") != (tc.code != 0) {
				t.Fatalf("approval advice contradicts exit status: %s", text)
			}
			if !strings.Contains(text, "pln_test") || tc.code == 0 && !strings.Contains(text, "revision 4") {
				t.Fatalf("review candidate missing: %s", text)
			}
		})
	}
}
