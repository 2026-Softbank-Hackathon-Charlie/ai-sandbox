// Package llm은 도구를 쓰는 AI 에이전트다 (본선 규칙: tool-using agent).
//
// 에이전트는 읽기 전용 도구로 조사하고 JSON 하나로 답한다. 실행은 하지 않는다.
// 도구 호출, 결과, AI 응답(토큰 사용량 포함), 판단은 agentlog로 플랫폼 세션에 실시간으로 남긴다.
// 응답은 두 겹으로 막는다. API에 JSON 스키마를 넘기고(구조화 출력), 받은 뒤 plan 패키지의 검사를 다시 돌린다.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
)

// DefaultModel은 고정 버전 모델 ID다. 별칭이 아니다.
const DefaultModel = "claude-opus-5-5"

// Config는 에이전트 설정이다. 0값이면 기본값을 쓴다.
type Config struct {
	Model string
	// Effort는 생각 깊이다. 기본 medium (데모 시간 안에 끝나도록).
	Effort anthropic.BetaOutputConfigEffort
	// CallTimeout은 AI 호출 한 번의 상한이다. 기본 90초.
	CallTimeout time.Duration
	// MaxRetries는 검사에 걸렸을 때 다시 묻는 횟수다. 기본 2.
	MaxRetries int
	// MaxIterations는 한 번의 답을 얻는 동안 AI 호출(도구 사용 포함) 상한이다. 기본 10.
	MaxIterations int
	// Sink는 세션 기록을 받는 곳이다. 비우면 기록하지 않는다(테스트).
	Sink agentlog.Sink
	// OnLogError는 작업은 성공했지만 세션 기록 일부를 남기지 못했을 때 불린다.
	// 기록 실패 때문에 검사를 통과한 결과를 버리지 않는다. 비우면 무시한다.
	OnLogError func(error)
	// Options는 SDK 옵션이다. 테스트에서 option.WithBaseURL로 가짜 서버를 붙인다.
	// API 키는 기본으로 ANTHROPIC_API_KEY 환경 변수에서 읽는다.
	Options []option.RequestOption
}

// Agent는 계획과 진단을 맡는다.
type Agent struct {
	api anthropic.Client
	cfg Config
}

func New(cfg Config) *Agent {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Effort == "" {
		cfg.Effort = anthropic.BetaOutputConfigEffortMedium
	}
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 90 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 2
	}
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 10
	}
	if cfg.Sink == nil {
		cfg.Sink = &agentlog.Memory{}
	}
	// SDK 자체 재시도(429, 5xx)는 1번만. 전체 시간이 데모 안에 들어가야 한다.
	opts := append([]option.RequestOption{option.WithMaxRetries(1), option.WithRequestTimeout(cfg.CallTimeout)}, cfg.Options...)
	return &Agent{api: anthropic.NewClient(opts...), cfg: cfg}
}

// 오류
var (
	// ErrRefused는 대체 모델까지 거절했을 때다. 다시 묻지 않는다.
	ErrRefused = errors.New("AI가 요청을 거절함")
	// ErrTruncated는 응답이 출력 상한에서 잘렸을 때다.
	ErrTruncated = errors.New("AI 응답이 출력 상한에서 잘림")
	// ErrNoAnswer는 도구만 부르다가 답 없이 호출 상한에 닿았을 때다.
	ErrNoAnswer = errors.New("AI가 호출 상한 안에 답하지 않음")
	// ErrBlocked는 도구가 허용 범위 밖의 요청을 막았을 때 돌려준다. 'blocked' 이벤트로 남는다.
	ErrBlocked = errors.New("허용 범위 밖이라 막음")
	// ErrInfeasible은 원본 실측값으로 보아 어떤 Plan도 검사를 통과할 수 없을 때다. AI를 부르지 않는다.
	ErrInfeasible = errors.New("지금 상한값으로는 계획할 수 없음")
)

// Tool은 AI가 쓸 수 있는 도구 하나다. Run 안에서 허용 범위를 다시 검사한다.
type Tool struct {
	Name        string
	Description string
	// Properties, Required는 입력 JSON 스키마다.
	Properties map[string]any
	Required   []string
	Run        func(ctx context.Context, input json.RawMessage) (any, error)
}

// loggedTool은 도구 호출마다 세션에 tool_call, tool_result(또는 blocked, error)를 남긴다.
type loggedTool struct {
	t   Tool
	ses agentlog.Session
}

func (l loggedTool) Name() string        { return l.t.Name }
func (l loggedTool) Description() string { return l.t.Description }
func (l loggedTool) InputSchema() anthropic.BetaToolInputSchemaParam {
	return anthropic.BetaToolInputSchemaParam{
		Properties:  l.t.Properties,
		Required:    l.t.Required,
		ExtraFields: map[string]any{"additionalProperties": false},
	}
}

func (l loggedTool) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in any
	_ = json.Unmarshal(input, &in)
	l.ses.Log(ctx, agentlog.Event{Type: agentlog.ToolCall, Tool: l.t.Name, Input: in})
	start := time.Now()
	out, err := l.t.Run(ctx, input)
	d := time.Since(start)
	switch {
	case errors.Is(err, ErrBlocked):
		l.ses.Log(ctx, agentlog.Event{Type: agentlog.Blocked, Tool: l.t.Name, Input: in, Output: err.Error(), Duration: d})
		return nil, err
	case err != nil:
		l.ses.Log(ctx, agentlog.Event{Type: agentlog.ToolResult, Tool: l.t.Name, Status: "error", Output: err.Error(), Duration: d})
		return nil, err
	}
	l.ses.Log(ctx, agentlog.Event{Type: agentlog.ToolResult, Tool: l.t.Name, Status: "ok", Output: out, Duration: d})
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return []anthropic.BetaToolResultBlockParamContentUnion{{OfText: &anthropic.BetaTextBlockParam{Text: string(b)}}}, nil
}

// task는 에이전트 작업 하나다.
type task[T any] struct {
	session agentlog.Start
	system  string
	user    string
	tools   []Tool
	schema  []byte
	parse   func([]byte) (T, plan.Issues)
	// summary는 성공했을 때 세션 요약과 decision 이벤트 내용이다.
	summary func(T) (string, any)
}

// run은 세션을 열고, 도구를 쓰며 답을 받고, 검사에 걸리면 걸린 항목을 넣어 다시 묻는다.
func run[T any](ctx context.Context, a *Agent, t task[T]) (T, string, error) {
	var zero T
	t.session.Model = a.cfg.Model
	ses, err := a.cfg.Sink.Start(ctx, t.session)
	if err != nil {
		return zero, "", err
	}
	tools := make([]anthropic.BetaTool, len(t.tools))
	for i, x := range t.tools {
		tools[i] = loggedTool{t: x, ses: ses}
	}
	msgs := []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(t.user))}

	fail := func(err error) (T, string, error) {
		ses.Log(ctx, agentlog.Event{Type: agentlog.Error, Output: err.Error()})
		if ferr := ses.Finish(ctx, false, err.Error()); ferr != nil {
			err = errors.Join(err, ferr)
		}
		return zero, ses.ID(), err
	}

	for attempt := 1; attempt <= a.cfg.MaxRetries+1; attempt++ {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		runner := a.api.Beta.Messages.NewToolRunner(tools, anthropic.BetaToolRunnerParams{
			BetaMessageNewParams: anthropic.BetaMessageNewParams{
				Model:     a.cfg.Model,
				MaxTokens: 16000,
				System:    []anthropic.BetaTextBlockParam{{Text: t.system}},
				Messages:  msgs,
				OutputConfig: anthropic.BetaOutputConfigParam{
					Effort: a.cfg.Effort,
					Format: anthropic.BetaJSONOutputFormatParam{Schema: json.RawMessage(t.schema)},
				},
				// 안전 분류기가 거절하면 서버가 권장 대체 모델로 다시 돌린다
				Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
				Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
			},
			MaxIterations: a.cfg.MaxIterations,
		})
		var last *anthropic.BetaMessage
		for {
			start := time.Now()
			m, err := runner.NextMessage(ctx)
			if err != nil {
				return fail(fmt.Errorf("AI 호출 실패: %w", err))
			}
			if m == nil {
				break
			}
			last = m
			ses.Log(ctx, messageEvent(m, time.Since(start)))
			switch m.StopReason {
			case anthropic.BetaStopReasonRefusal:
				return fail(fmt.Errorf("%w (%s)", ErrRefused, m.StopDetails.Category))
			case anthropic.BetaStopReasonMaxTokens:
				return fail(ErrTruncated)
			}
		}
		if last == nil || last.StopReason == anthropic.BetaStopReasonToolUse {
			return fail(ErrNoAnswer)
		}
		v, is := t.parse([]byte(text(last)))
		if len(is) == 0 {
			sum, detail := t.summary(v)
			ses.Log(ctx, agentlog.Event{Type: agentlog.Decision, Status: "ok", Output: detail})
			if err := ses.Finish(ctx, true, sum); err != nil && a.cfg.OnLogError != nil {
				a.cfg.OnLogError(err)
			}
			return v, ses.ID(), nil
		}
		for i := range is {
			is[i].Path = mask.String(is[i].Path)
			is[i].Message = mask.String(is[i].Message)
		}
		issues := is.Error()
		ses.Log(ctx, agentlog.Event{Type: agentlog.Error, Status: "check", Output: issues})
		if attempt > a.cfg.MaxRetries {
			return fail(&plan.RetryError{Attempts: attempt, Last: is})
		}
		msgs = append(runner.Messages(), anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
			"방금 낸 JSON이 검사에 걸렸다. 아래 항목을 고쳐서 JSON 전체를 다시 내라.\n- "+strings.ReplaceAll(issues, "\n", "\n- "))))
	}
	return fail(ErrNoAnswer)
}

// messageEvent는 AI 응답 한 번을 기록으로 바꾼다. 토큰 사용량을 함께 남긴다 (AI 비용 근거).
func messageEvent(m *anthropic.BetaMessage, d time.Duration) agentlog.Event {
	var calls []string
	for _, b := range m.Content {
		if u, ok := b.AsAny().(anthropic.BetaToolUseBlock); ok {
			calls = append(calls, u.Name)
		}
	}
	out := map[string]any{"stopReason": string(m.StopReason), "model": string(m.Model)}
	if len(calls) > 0 {
		out["toolCalls"] = calls
	}
	if s := text(m); s != "" {
		out["text"] = agentlog.Truncate(s, 2000)
	}
	return agentlog.Event{Type: agentlog.Message, Output: out, Duration: d, InputTokens: m.Usage.InputTokens, OutputTokens: m.Usage.OutputTokens}
}

func text(m *anthropic.BetaMessage) string {
	var sb strings.Builder
	for _, b := range m.Content {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}
