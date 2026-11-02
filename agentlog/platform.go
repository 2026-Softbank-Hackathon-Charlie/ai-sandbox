package agentlog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

// Platform은 플랫폼 API(/v1/agent/sessions)에 세션을 남긴다. agent 토큰을 쓴다.
type Platform struct {
	C *client.Client
	// Timeout은 요청 하나의 상한이다. 기본 5초. 기록 때문에 AI 작업이 오래 멈추지 않게 한다.
	Timeout time.Duration
}

type platformSession struct {
	p    Platform
	id   string
	mu   sync.Mutex
	errs []error
}

func (p Platform) timeout() time.Duration {
	if p.Timeout == 0 {
		return 5 * time.Second
	}
	return p.Timeout
}

func ptr[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

func (p Platform) Start(ctx context.Context, s Start) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	ses, err := p.C.CreateAgentSession(ctx, apiv1.CreateAgentSessionRequest{
		Kind:    apiv1.AgentSessionKind(s.Kind),
		Title:   s.Title,
		PlanRef: ptr(s.PlanRef),
		RunId:   ptr(s.RunID),
		Model:   ptr(s.Model),
	})
	if err != nil {
		return nil, fmt.Errorf("에이전트 세션 열기 실패: %w", err)
	}
	return &platformSession{p: p, id: ses.Id}, nil
}

func (s *platformSession) ID() string { return s.id }

func (s *platformSession) Log(ctx context.Context, e Event) {
	ctx, cancel := context.WithTimeout(ctx, s.p.timeout())
	defer cancel()
	req := apiv1.AgentEventRequest{
		Type:   apiv1.AgentEventType(e.Type),
		Tool:   ptr(e.Tool),
		Input:  redact(e.Input),
		Output: redact(e.Output),
		Status: ptr(e.Status),
	}
	if e.Duration > 0 {
		ms := e.Duration.Milliseconds()
		req.DurationMillis = &ms
	}
	if e.InputTokens+e.OutputTokens > 0 {
		req.Usage = &apiv1.TokenUsage{InputTokens: e.InputTokens, OutputTokens: e.OutputTokens}
	}
	if _, err := s.p.C.AppendAgentEvent(ctx, s.id, req); err != nil {
		s.mu.Lock()
		s.errs = append(s.errs, fmt.Errorf("이벤트 %s %s: %w", e.Type, e.Tool, err))
		s.mu.Unlock()
	}
}

func (s *platformSession) Finish(ctx context.Context, ok bool, summary string) error {
	ctx, cancel := context.WithTimeout(ctx, s.p.timeout())
	defer cancel()
	st := apiv1.AgentSessionStatus("succeeded")
	if !ok {
		st = "failed"
	}
	_, err := s.p.C.FinishAgentSession(ctx, s.id, apiv1.FinishAgentSessionRequest{Status: st, Summary: ptr(summary)})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errs = append(s.errs, fmt.Errorf("세션 닫기 실패: %w", err))
	}
	if len(s.errs) > 0 {
		return fmt.Errorf("세션 %s 기록 일부 실패: %w", s.id, errors.Join(s.errs...))
	}
	return nil
}
