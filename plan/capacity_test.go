package plan

import (
	"strings"
	"testing"
)

// withParams는 정상 출력의 대상 매니페스트에 postgresql.parameters를 넣는다.
func withParams(t *testing.T, p *Proposal, params map[string]string) *Proposal {
	t.Helper()
	var b strings.Builder
	b.WriteString("  instances: 2\n  postgresql:\n    parameters:\n")
	for _, k := range []string{"max_connections", "max_worker_processes"} {
		if v, ok := params[k]; ok {
			b.WriteString("      " + k + ": \"" + v + "\"\n")
		}
	}
	q := *p
	q.Manifest = strings.Replace(p.Manifest, "  instances: 2\n", b.String(), 1)
	if q.Manifest == p.Manifest {
		t.Fatal("매니페스트에 넣을 자리가 없음")
	}
	return &q
}

func withConns(t *testing.T, p *Proposal, n string) *Proposal {
	return withParams(t, p, map[string]string{"max_connections": n})
}

func stats() *SourceStats {
	// 원본 max_connections 200, 세션 80(× 1.2 = 96) → 200 이상 필요. 2 GiB → 100 MiB/s로 21초
	return &SourceStats{Sessions: 80, MaxConnections: 200, MaxWorkerProcesses: 8, Bytes: 2 << 30, WALBytesPerSecond: 1 << 20, WindowSeconds: 5}
}

func TestRequiredConnections(t *testing.T) {
	for _, c := range []struct {
		sessions, maxConn, want int
	}{
		{80, 200, 200},  // 원본 설정이 더 큼: hot standby 조건
		{190, 200, 228}, // 세션 × 1.2가 더 큼: 플랫폼 기준
		{0, 100, 100},
	} {
		s := &SourceStats{Sessions: c.sessions, MaxConnections: c.maxConn}
		if got := s.RequiredConnections(); got != c.want {
			t.Errorf("세션 %d, 원본 %d: got %d want %d", c.sessions, c.maxConn, got, c.want)
		}
	}
}

func TestCapacityPasses(t *testing.T) {
	p := withConns(t, proposal(t), "200")
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
		"max_connections 없음": {func(p *Proposal) {}, "200 이상으로 정해야 함"},
		// 세션 기준(96)은 넘지만 원본보다 작으면 대상 replica가 뜨지 않는다
		"원본보다 작은 max_connections": {func(p *Proposal) { *p = *withConns(t, p, "150") }, "150이면 부족함"},
		"원본보다 작은 max_worker_processes": {func(p *Proposal) {
			*p = *withParams(t, p, map[string]string{"max_connections": "200", "max_worker_processes": "4"})
		}, "max_worker_processes: 4이면 부족함"},
		"CNPG 기본값보다 큰 원본 worker": {func(p *Proposal) {
			p.Source.MaxWorkerProcesses = 64
			*p = *withConns(t, p, "200")
		}, "32이면 부족함"},
		"초기 복사보다 짧은 catchupTimeout": {func(p *Proposal) {
			p.Source.Bytes = 80 << 30 // 100 MiB/s로 약 13.7분, 상한 15분 안
			*p = *withConns(t, p, "200")
		}, "최소 13m39s 걸림"},
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

func TestCheckFeasible(t *testing.T) {
	if err := CheckFeasible(stats()); err != nil {
		t.Fatalf("작은 원본은 계획할 수 있어야 함: %v", err)
	}
	if err := CheckFeasible(nil); err != nil {
		t.Fatal("실측값이 없으면 막지 않음")
	}
	big := stats()
	big.Bytes = 200 << 30 // 약 34분 > 상한 15분
	if err := CheckFeasible(big); err == nil || !strings.Contains(err.Error(), "catchupTimeout 상한 15m0s") {
		t.Fatalf("상한 안에 복사할 수 없으면 계획 불가: %v", err)
	}
	fast := stats()
	fast.WALBytesPerSecond = 150 << 20
	if err := CheckFeasible(fast); err == nil || !strings.Contains(err.Error(), "따라잡을 수 없음") {
		t.Fatalf("변경이 복사보다 빠르면 계획 불가: %v", err)
	}
}

func TestCapacityZeroCatchupReportedOnce(t *testing.T) {
	p := withConns(t, proposal(t), "200")
	p.Source = stats()
	p.Plan.Thresholds.CatchupTimeout.Duration = 0
	if n := strings.Count(p.Check(draft(t)).Error(), "plan.thresholds.catchupTimeout"); n != 1 {
		t.Fatalf("비운 catchupTimeout은 범위 검사 한 번만 걸려야 함: %d번", n)
	}
}

func TestCapacitySkippedWithoutStats(t *testing.T) {
	// 실측하지 못했으면 지금처럼 max_connections 없이도 통과한다 (플랫폼이 시작할 때 잰다).
	if is := proposal(t).Check(draft(t)); len(is) > 0 {
		t.Fatalf("실측값이 없으면 용량 검사를 하지 않음:\n%s", is.Error())
	}
}

func TestFixKeepsCapacityCheck(t *testing.T) {
	p := withConns(t, proposal(t), "200")
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
