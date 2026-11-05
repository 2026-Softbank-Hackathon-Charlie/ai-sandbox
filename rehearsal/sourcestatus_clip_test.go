package rehearsal

import (
	"context"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
)

func TestClipEvidenceLimitsEachLineAndTheTotal(t *testing.T) {
	short := []string{"pvc/a  Pending  storageClass=gp3", "pod/b  Pending"}
	if got := clipEvidence(short); len(got) != 2 || got[0] != short[0] || got[1] != short[1] {
		t.Fatalf("짧은 줄은 그대로여야 함: %v", got)
	}

	// 경고 이벤트의 메시지는 길이 제한이 없다
	got := clipEvidence([]string{strings.Repeat("가", 1000)})
	if r := []rune(got[0]); len(r) != maxStatusLine+1 || !strings.HasSuffix(got[0], "…") {
		t.Fatalf("줄 하나는 %d글자 안으로 줄여야 함: %d글자", maxStatusLine, len(r))
	}

	// 전체는 플랫폼의 64 KiB 상한 아래로 맞춘다
	lines := make([]string, 400)
	for i := range lines {
		lines[i] = strings.Repeat("나", maxStatusLine)
	}
	got = clipEvidence(lines)
	total := 0
	for _, l := range got[:len(got)-1] {
		total += len(l)
	}
	if total > maxEvidenceBytes {
		t.Fatalf("전체 크기가 상한 %d바이트를 넘음: %d", maxEvidenceBytes, total)
	}
	if last := got[len(got)-1]; !strings.Contains(last, "줄 생략") {
		t.Fatalf("잘렸다는 표시가 마지막에 있어야 함: %q", last)
	}
}

func TestLogSourceStatusLogsOneEventPerLine(t *testing.T) {
	mem := &agentlog.Memory{}
	ses, err := mem.Start(context.Background(), agentlog.Start{})
	if err != nil {
		t.Fatal(err)
	}
	logSourceStatus(context.Background(), ses, []string{"pvc/a  Pending", "pod/b  Pending", "Warning  FailedScheduling  pod/b  0/3 nodes"})
	events := mem.Sessions[0].Events
	if len(events) != 3 {
		t.Fatalf("줄마다 이벤트 하나여야 함: %d개", len(events))
	}
	for i, e := range events {
		if e.Type != agentlog.ToolResult || e.Tool != "sandbox_source_status" || e.Status != "not_ready" {
			t.Errorf("이벤트 %d 형식: %+v", i, e)
		}
	}
	if events[1].Output != "pod/b  Pending" {
		t.Errorf("출력은 줄 그대로여야 함: %v", events[1].Output)
	}
	logSourceStatus(context.Background(), ses, nil)
	if len(mem.Sessions[0].Events) != 3 {
		t.Fatal("줄이 없으면 이벤트도 없어야 함")
	}
}
