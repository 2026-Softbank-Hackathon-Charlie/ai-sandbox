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
		{"candidate mismatch", `{"passed":true}`, matchCandidate(plan.Digest{PlanSHA256: "ppp"}, "other", &apiv1.PlanRecord{Ref: "pln_test"}), 1, false},
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
			// 리허설은 통과했는데 후보만 다르면 리허설 실패로 안내하지 않는다.
			failed := tc.code != 0 && !errors.Is(tc.err, errCandidate)
			if strings.Contains(text, "통과하지 못했습니다") != failed {
				t.Fatalf("candidate mismatch must not read as a failed rehearsal: %s", text)
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

func TestSourceStatsFromPlatform(t *testing.T) {
	notMeasured := apiv1.PlanDraftResultSourceStatsReasonNotMeasured
	if s := sourceStats(&apiv1.PlanDraftResult{SourceStatsReason: &notMeasured}); s != nil {
		t.Fatalf("재지 못했으면 nil이어야 함: %+v", s)
	}
	if got := reasonOf(&apiv1.PlanDraftResult{SourceStatsReason: &notMeasured}); got != "not_measured" {
		t.Fatalf("이유: %s", got)
	}
	size, wal := int64(2<<30), 1048576.0
	s := sourceStats(&apiv1.PlanDraftResult{SourceStats: &apiv1.SourceStats{Sessions: 80, MaxConnections: 200, MaxWorkerProcesses: 8, Bytes: &size, WalBytesPerSecond: &wal, WindowSeconds: 5}})
	if s == nil || s.Sessions != 80 || s.MaxConnections != 200 || s.MaxWorkerProcesses != 8 || s.Bytes != size || s.WALBytesPerSecond != wal || s.WindowSeconds != 5 {
		t.Fatalf("옮긴 값: %+v", s)
	}
	// 크기와 WAL 속도를 못 쟀으면 0으로 두어 초기 복사 검사가 막지 않는다. 접속 수 검사는 그대로 한다.
	s = sourceStats(&apiv1.PlanDraftResult{SourceStats: &apiv1.SourceStats{Sessions: 80, MaxConnections: 200, MaxWorkerProcesses: 8}})
	if s == nil || s.Bytes != 0 || s.WALBytesPerSecond != 0 || s.RequiredConnections() < 200 {
		t.Fatalf("일부만 잰 값: %+v", s)
	}
}
