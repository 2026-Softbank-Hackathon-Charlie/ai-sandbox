package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
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

func TestMatchCandidate(t *testing.T) {
	att := func(sum string) *apiv1.PlanRecord {
		list := []apiv1.PlanAttachment{{Name: "rehearsal-report.json", Sha256: "rrr"}, {Name: plan.ManifestName, Sha256: sum}}
		return &apiv1.PlanRecord{Ref: "pln_test", Attachments: &list}
	}
	c := plan.Digest{PlanSHA256: "ppp", ManifestSHA256: "mmm"}
	if err := matchCandidate(c, "ppp", att("mmm")); err != nil {
		t.Fatalf("같은 후보인데 걸림: %v", err)
	}
	if err := matchCandidate(c, "other", att("mmm")); err == nil || !strings.Contains(err.Error(), "제출한 Plan") {
		t.Fatalf("다른 Plan 본문을 놓침: %v", err)
	}
	if err := matchCandidate(c, "ppp", att("old")); err == nil || !strings.Contains(err.Error(), plan.ManifestName) {
		t.Fatalf("리허설하지 않은 매니페스트 첨부를 놓침: %v", err)
	}
	if err := matchCandidate(c, "ppp", &apiv1.PlanRecord{Ref: "pln_test"}); err == nil {
		t.Fatal("매니페스트 첨부가 없는데 통과함")
	}
	if err := matchCandidate(plan.Digest{PlanSHA256: "ppp"}, "ppp", &apiv1.PlanRecord{Ref: "pln_test"}); err != nil {
		t.Fatalf("매니페스트 없는 Plan인데 걸림: %v", err)
	}
}
