// Package agentlog는 AI의 작업을 세션 단위로 플랫폼에 남긴다 (platform ADR 0008, /v1/agent/sessions).
// 도구 호출과 결과, 막힌 명령, 판단이 콘솔과 CLI에 실시간으로 보이고, 감사 로그에도 남는다.
//
// 기록은 AI가 하지 않는다. 에이전트 코드가 도구를 부를 때마다 남기므로 AI가 기록을 고치거나 빼먹을 수 없다.
package agentlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

// 세션 종류 (AgentSessionKind)
const (
	KindPlan      = "plan"
	KindRehearsal = "rehearsal"
	KindDiagnose  = "diagnose"
	KindExplain   = "explain"
)

// 이벤트 종류 (AgentEventType)
const (
	ToolCall   = "tool_call"
	ToolResult = "tool_result"
	Message    = "message"
	Blocked    = "blocked"
	Decision   = "decision"
	Error      = "error"
)

// Start는 세션을 열 때 정보다.
type Start struct {
	Kind    string
	Title   string
	PlanRef string
	RunID   string
	Model   string
}

// Event는 세션에 남기는 기록 하나다.
type Event struct {
	Type     string
	Tool     string
	Input    any
	Output   any
	Status   string
	Duration time.Duration
	// InputTokens, OutputTokens는 AI 호출 한 번의 사용량이다 (Message 이벤트에만).
	InputTokens  int64
	OutputTokens int64
}

// Session은 열린 세션이다.
type Session interface {
	ID() string
	// Log는 이벤트를 남긴다. 실패해도 작업을 멈추지 않고 모아 두었다가 Finish에서 돌려준다.
	Log(ctx context.Context, e Event)
	// Finish는 세션을 닫는다. ok가 아니면 failed로 닫는다. 남기지 못한 이벤트가 있었으면 오류다.
	Finish(ctx context.Context, ok bool, summary string) error
}

// Sink는 세션을 여는 곳이다. 플랫폼, 메모리, 터미널 출력 구현이 있다.
type Sink interface {
	Start(ctx context.Context, s Start) (Session, error)
}

// redact는 플랫폼에 보내기 전에 문자열 값의 비밀을 가린다. 플랫폼도 저장 전에 가리지만 한 번 더 막는다.
// JSON 구조는 그대로 두고 문자열만 바꾼다.
func redact(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var x any
	if json.Unmarshal(b, &x) != nil {
		return nil
	}
	return walk(x)
}

func walk(x any) any {
	switch t := x.(type) {
	case string:
		return mask.String(t)
	case []any:
		for i := range t {
			t[i] = walk(t[i])
		}
		return t
	case map[string]any:
		for k, v := range t {
			if _, isStr := v.(string); isStr && mask.SecretKey(k) {
				t[k] = mask.Placeholder
				continue
			}
			t[k] = walk(v)
		}
		return t
	default:
		return t
	}
}

/* ---------- 메모리 (테스트, 시험 실행) ---------- */

// Memory는 세션을 메모리에 모은다.
type Memory struct {
	mu       sync.Mutex
	Sessions []*MemorySession
}

type MemorySession struct {
	mu      sync.Mutex
	Info    Start
	Id      string
	Events  []Event
	Done    bool
	OK      bool
	Summary string
}

func (m *Memory) Start(_ context.Context, s Start) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := &MemorySession{Info: s, Id: fmt.Sprintf("mem_%d", len(m.Sessions)+1)}
	m.Sessions = append(m.Sessions, ms)
	return ms, nil
}

func (s *MemorySession) ID() string { return s.Id }

func (s *MemorySession) Log(_ context.Context, e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Input, e.Output = redact(e.Input), redact(e.Output)
	s.Events = append(s.Events, e)
}

func (s *MemorySession) Finish(_ context.Context, ok bool, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Done, s.OK, s.Summary = true, ok, summary
	return nil
}

/* ---------- 터미널 출력 ---------- */

// Writer는 세션과 이벤트를 한 줄씩 출력한다. 다른 Sink와 함께 쓴다 (Tee).
type Writer struct {
	W io.Writer
}

type writerSession struct {
	w  io.Writer
	id string
	mu sync.Mutex
}

func (w Writer) Start(_ context.Context, s Start) (Session, error) {
	fmt.Fprintf(w.W, "== [%s] %s\n", s.Kind, s.Title)
	return &writerSession{w: w.W, id: "stdout"}, nil
}

func (s *writerSession) ID() string { return s.id }

func (s *writerSession) Log(_ context.Context, e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := e.Type
	if e.Tool != "" {
		line += " " + e.Tool
	}
	if e.Status != "" {
		line += " (" + e.Status + ")"
	}
	if e.Input != nil {
		line += " " + short(redact(e.Input))
	}
	if e.Output != nil {
		line += " → " + short(redact(e.Output))
	}
	if e.InputTokens+e.OutputTokens > 0 {
		line += fmt.Sprintf(" [토큰 %d/%d]", e.InputTokens, e.OutputTokens)
	}
	fmt.Fprintln(s.w, "   "+line)
}

func (s *writerSession) Finish(_ context.Context, ok bool, summary string) error {
	st := "완료"
	if !ok {
		st = "실패"
	}
	fmt.Fprintf(s.w, "== %s: %s\n", st, summary)
	return nil
}

func short(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if s == "" {
		s = fmt.Sprint(v)
	}
	if len(s) > 200 {
		s = s[:199] + "…"
	}
	return s
}

/* ---------- 여러 곳에 동시에 ---------- */

// Tee는 여러 Sink에 같은 세션을 연다. 첫 번째 Sink의 세션 ID를 쓴다.
type Tee []Sink

type teeSession []Session

func (t Tee) Start(ctx context.Context, s Start) (Session, error) {
	var out teeSession
	for i, k := range t {
		ss, err := k.Start(ctx, s)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			continue
		}
		out = append(out, ss)
	}
	return out, nil
}

func (t teeSession) ID() string { return t[0].ID() }

func (t teeSession) Log(ctx context.Context, e Event) {
	for _, s := range t {
		s.Log(ctx, e)
	}
}

func (t teeSession) Finish(ctx context.Context, ok bool, summary string) error {
	var errs []error
	for _, s := range t {
		errs = append(errs, s.Finish(ctx, ok, summary))
	}
	return errors.Join(errs...)
}
