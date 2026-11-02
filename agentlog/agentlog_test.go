package agentlog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

// fakePlatform은 /v1/agent/sessions 흉내를 낸다. 받은 요청을 기록한다.
type fakePlatform struct {
	mu      sync.Mutex
	calls   []call
	failLog bool
}

type call struct {
	method, path, auth string
	body               map[string]any
}

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	f.calls = append(f.calls, call{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
	w.Header().Set("Content-Type", "application/json")
	now := time.Now().UTC().Format(time.RFC3339)
	session := map[string]any{"id": "ags_1", "kind": "plan", "title": "t", "actor": "planner", "status": "running", "createdAt": now,
		"totals": map[string]any{"events": 0, "toolCalls": 0, "inputTokens": 0, "outputTokens": 0, "durationMillis": 0}}
	switch {
	case r.URL.Path == "/v1/agent/sessions":
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(session)
	case strings.HasSuffix(r.URL.Path, "/events"):
		if f.failLog {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":{"code":"session_full","message":"full"}}`))
			return
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"seq": len(f.calls), "sessionId": "ags_1", "time": now, "type": body["type"]})
	case strings.HasSuffix(r.URL.Path, "/finish"):
		session["status"] = body["status"]
		_ = json.NewEncoder(w).Encode(session)
	}
}

func platform(t *testing.T, f *fakePlatform) Platform {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return Platform{C: client.New(srv.URL, "agent-token-for-test")}
}

func TestPlatformSession(t *testing.T) {
	f := &fakePlatform{}
	p := platform(t, f)
	ctx := context.Background()
	s, err := p.Start(ctx, Start{Kind: KindRehearsal, Title: "샌드박스 리허설", PlanRef: "pln_1", Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	s.Log(ctx, Event{Type: ToolCall, Tool: "sandbox_apply", Input: map[string]any{"dsn": "postgres://app:RealPassw0rd@db:5432/app", "count": 3}})
	s.Log(ctx, Event{Type: Message, Duration: 1500 * time.Millisecond, InputTokens: 1200, OutputTokens: 300})
	s.Log(ctx, Event{Type: Blocked, Tool: "kubectl", Input: "kubectl get secrets -n moimroom", Output: "RBAC"})
	if err := s.Finish(ctx, true, "통과"); err != nil {
		t.Fatal(err)
	}

	if len(f.calls) != 5 {
		t.Fatalf("요청 5번(열기, 이벤트 3, 닫기)이어야 함: %d", len(f.calls))
	}
	open := f.calls[0]
	if open.method != "POST" || open.body["kind"] != "rehearsal" || open.body["planRef"] != "pln_1" || open.auth != "Bearer agent-token-for-test" {
		t.Fatalf("세션 열기: %+v", open)
	}
	if _, ok := open.body["runId"]; ok {
		t.Fatal("빈 runId는 보내지 않아야 함")
	}
	ev := f.calls[1].body
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), "RealPassw0rd") {
		t.Fatalf("비밀번호가 플랫폼에 전달됨: %s", raw)
	}
	if ev["input"].(map[string]any)["count"] != float64(3) {
		t.Fatal("숫자 값은 그대로 남아야 함")
	}
	usage := f.calls[2].body["usage"].(map[string]any)
	if usage["inputTokens"] != float64(1200) || f.calls[2].body["durationMillis"] != float64(1500) {
		t.Fatalf("토큰과 시간: %+v", f.calls[2].body)
	}
	if f.calls[3].body["type"] != "blocked" || f.calls[4].body["status"] != "succeeded" {
		t.Fatalf("막힌 명령, 닫기: %+v %+v", f.calls[3].body, f.calls[4].body)
	}
}

func TestPlatformLogErrorsReportedOnFinish(t *testing.T) {
	f := &fakePlatform{failLog: true}
	p := platform(t, f)
	s, err := p.Start(context.Background(), Start{Kind: KindPlan, Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	s.Log(context.Background(), Event{Type: ToolCall, Tool: "x"}) // 실패해도 멈추지 않는다
	err = s.Finish(context.Background(), false, "실패")
	if err == nil || !strings.Contains(err.Error(), "기록 일부 실패") {
		t.Fatalf("남기지 못한 기록은 닫을 때 알려야 함: %v", err)
	}
	if f.calls[len(f.calls)-1].body["status"] != "failed" {
		t.Fatal("ok가 아니면 failed로 닫아야 함")
	}
}

func TestTeeAndWriter(t *testing.T) {
	var buf bytes.Buffer
	mem := &Memory{}
	s, err := Tee{mem, Writer{W: &buf}}.Start(context.Background(), Start{Kind: KindPlan, Title: "계획"})
	if err != nil {
		t.Fatal(err)
	}
	s.Log(context.Background(), Event{Type: ToolCall, Tool: "list_storage_classes", Input: map[string]any{"env": "eks", "token": "abcdefghijklmnop"}})
	_ = s.Finish(context.Background(), true, "끝")
	if s.ID() != "mem_1" || len(mem.Sessions[0].Events) != 1 {
		t.Fatalf("첫 번째 Sink의 세션 ID를 써야 함: %s", s.ID())
	}
	out := buf.String()
	if !strings.Contains(out, "tool_call list_storage_classes") || strings.Contains(out, "abcdefghijklmnop") {
		t.Fatalf("출력: %s", out)
	}
}

func TestTruncateKeepsRunes(t *testing.T) {
	s := strings.Repeat("가", 100) // 300바이트
	out := Truncate(s, 50)
	if len(out) > 50 || !utf8.ValidString(out) || !strings.HasSuffix(out, "…") {
		t.Fatalf("한글을 반으로 자르면 안 됨: %q (%d바이트)", out, len(out))
	}
	if Truncate("짧음", 50) != "짧음" {
		t.Fatal("짧으면 그대로")
	}
}
