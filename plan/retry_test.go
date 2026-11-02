package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scripted는 정해 둔 응답을 차례로 돌려주는 가짜 AI다. 받은 issues도 기록한다.
type scripted struct {
	replies []string
	seen    []Issues
}

func (s *scripted) ask(_ context.Context, is Issues) ([]byte, error) {
	s.seen = append(s.seen, is)
	return []byte(s.replies[len(s.seen)-1]), nil
}

func parser(t *testing.T) func([]byte) (*Proposal, Issues) {
	d := draft(t)
	return func(raw []byte) (*Proposal, Issues) { return Parse(raw, d) }
}

func TestRetryPassesIssuesBack(t *testing.T) {
	v := read(t, "proposal.json")
	bad := replace(t, v, `"maxDowntime": "30s"`, `"maxDowntime": "5m"`)
	ai := &scripted{replies: []string{"```json\n" + v + "```", bad, v}}

	p, n, err := Retry(context.Background(), 2, ai.ask, parser(t))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || p == nil {
		t.Fatalf("세 번째에 통과해야 함: n=%d", n)
	}
	if ai.seen[0] != nil {
		t.Fatal("첫 호출에는 걸린 항목이 없어야 함")
	}
	if !strings.Contains(ai.seen[1].Error(), "JSON으로 읽을 수 없음") {
		t.Fatalf("두 번째 호출에 형식 오류가 전달돼야 함: %v", ai.seen[1])
	}
	if !strings.Contains(ai.seen[2].Error(), "maxDowntime") {
		t.Fatalf("세 번째 호출에 고정 규칙 오류가 전달돼야 함: %v", ai.seen[2])
	}
}

func TestRetryGivesUpAtLimit(t *testing.T) {
	ai := &scripted{replies: []string{"x", "y", "z", "never asked"}}
	_, n, err := Retry(context.Background(), 2, ai.ask, parser(t))
	if !IsRetryExhausted(err) {
		t.Fatalf("상한 초과 오류여야 함: %v", err)
	}
	if n != 3 || len(ai.seen) != 3 {
		t.Fatalf("정확히 3번 불러야 함: n=%d, 호출=%d", n, len(ai.seen))
	}
}

func TestRetryStopsOnCallError(t *testing.T) {
	calls := 0
	ask := func(context.Context, Issues) ([]byte, error) {
		calls++
		return nil, context.DeadlineExceeded
	}
	_, _, err := Retry(context.Background(), 2, ask, parser(t))
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("타임아웃이면 바로 멈춰야 함: err=%v, 호출=%d", err, calls)
	}
}

func TestRetryRespectsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ai := &scripted{replies: []string{read(t, "proposal.json")}}
	_, n, err := Retry(ctx, 2, ai.ask, parser(t))
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("취소되면 부르지 않아야 함: err=%v, n=%d", err, n)
	}
}
