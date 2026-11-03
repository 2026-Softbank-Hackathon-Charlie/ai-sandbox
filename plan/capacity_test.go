package plan

import (
	"strings"
	"testing"
	"time"
)

// withMaxConnections는 정상 출력의 대상 매니페스트에 max_connections를 넣는다.
func withMaxConnections(t *testing.T, p *Proposal, n string) *Proposal {
	t.Helper()
	q := *p
	q.Manifest = strings.Replace(p.Manifest, "  instances: 2\n", "  instances: 2\n  postgresql:\n    parameters:\n      max_connections: \""+n+"\"\n", 1)
	if q.Manifest == p.Manifest {
		t.Fatal("매니페스트에 넣을 자리가 없음")
	}
	return &q
}

func stats() *SourceStats {
	// 세션 80개 → 96 이상 필요, 2 GiB → 100 MiB/s로 21초
	return &SourceStats{Sessions: 80, MaxConnections: 200, Bytes: 2 << 30, WALBytesPerSecond: 1 << 20, Window: 5 * time.Second}
}

func TestCapacityPasses(t *testing.T) {
	p := withMaxConnections(t, proposal(t), "200")
	p.Source = stats()
	if is := p.Check(draft(t)); len(is) > 0 {
		t.Fatalf("통과해야 함:\n%s", is.Error())
	}
}

func TestCapacityRejects(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*Proposal)
		want string
	}{
		"max_connections 없음": {func(p *Proposal) {}, "96 이상으로 정해야 함"},
		"max_connections 부족": {func(p *Proposal) { *p = *withMaxConnections(t, p, "90") }, "90이면 부족함"},
		"초기 복사보다 짧은 catchupTimeout": {func(p *Proposal) {
			p.Source.Bytes = 200 << 30 // 100 MiB/s로 약 34분
			*p = *withMaxConnections(t, p, "200")
		}, "최소 34m8s 걸림"},
		"변경이 복사보다 빠름": {func(p *Proposal) {
			p.Source.WALBytesPerSecond = 150 << 20
			*p = *withMaxConnections(t, p, "200")
		}, "따라잡을 수 없음"},
	} {
		t.Run(name, func(t *testing.T) {
			p := proposal(t)
			p.Source = stats()
			c.edit(p)
			if is := p.Check(draft(t)); !strings.Contains(is.Error(), c.want) {
				t.Fatalf("%q가 들어간 항목이 없음. 걸린 항목:\n%s", c.want, is.Error())
			}
		})
	}
}

func TestCapacitySkippedWithoutStats(t *testing.T) {
	// 실측하지 못했으면 지금처럼 max_connections 없이도 통과한다 (플랫폼이 시작할 때 잰다).
	if is := proposal(t).Check(draft(t)); len(is) > 0 {
		t.Fatalf("실측값이 없으면 용량 검사를 하지 않음:\n%s", is.Error())
	}
}

func TestFixKeepsCapacityCheck(t *testing.T) {
	p := withMaxConnections(t, proposal(t), "200")
	p.Source = stats()
	p.Source.Bytes = 50 << 30 // 100 MiB/s로 약 8.5분
	ok := Fix{Kind: KindPlanParam, Patch: []Op{{"replace", "/thresholds/catchupTimeout", []byte(`"12m"`)}}}
	next, is := ok.Apply(p, draft(t))
	if len(is) > 0 || next.Source != p.Source {
		t.Fatalf("수정안 사본에도 실측값이 붙어 있어야 함: %v", is)
	}
	short := Fix{Kind: KindPlanParam, Patch: []Op{{"replace", "/thresholds/catchupTimeout", []byte(`"5m"`)}}}
	if _, is := short.Apply(p, draft(t)); !strings.Contains(is.Error(), "catchupTimeout") {
		t.Fatalf("수정안이 catchupTimeout을 복사 시간보다 줄이면 걸려야 함:\n%s", is.Error())
	}
}
