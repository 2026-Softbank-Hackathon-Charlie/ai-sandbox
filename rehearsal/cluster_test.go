//go:build cluster

// 실제 쿠버네티스(CNPG 설치됨)에서 리허설을 돌리는 테스트다. AI 없이 계획 예시 파일로 엔진만 실제로 돌린다.
//
//	REHEARSAL_KUBECONFIG=<kind kubeconfig> REHEARSAL_SC=standard go test -tags cluster -run Cluster -v -timeout 40m ./rehearsal/
package rehearsal

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/sandbox"
)

func clusterConfig(t *testing.T) Config {
	kc := os.Getenv("REHEARSAL_KUBECONFIG")
	if kc == "" {
		t.Skip("REHEARSAL_KUBECONFIG가 없음")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kc)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Admin: cs, AdminREST: cfg, PlanRef: "local-test",
		Sink:      agentlog.Tee{&agentlog.Memory{}, agentlog.Writer{W: os.Stderr}},
		Isolation: sandbox.IsolationConfig{ProdNamespace: "shop"},
		MaxRuns:   1, Rows: 5000,
	}
}

// kind에 맞게 인스턴스와 자원을 줄이고 StorageClass만 바꾼 계획
func kindProposal(t *testing.T, sc string) Config {
	c := clusterConfig(t)
	p := proposal(t)
	m := p.Manifest
	m = strings.Replace(m, "instances: 2", "instances: 1", 1)
	m = strings.Replace(m, "storageClass: gp3", "storageClass: "+sc, 1)
	m = strings.Replace(m, "size: 5Gi", "size: 1Gi", 1)
	p.Manifest = m
	p.Plan.Thresholds.CatchupTimeout.Duration = 5 * time.Minute
	c.Proposal, c.Draft = p, &p.Plan
	return c
}

func TestClusterRehearsalPasses(t *testing.T) {
	sc := os.Getenv("REHEARSAL_SC")
	if sc == "" {
		sc = "standard"
	}
	c := kindProposal(t, sc)
	rep, err := Rehearse(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Attempts[0]
	t.Logf("결과: 통과=%v 엔진=%s 단계=%v %.0f초 오류=%s", a.Passed, a.Outcome, a.Steps, a.DurationS, a.Error)
	if !rep.Passed || a.Outcome != "succeeded" {
		t.Fatalf("통과해야 함: %+v", a)
	}
	for step, st := range a.Steps {
		if st != "succeeded" {
			t.Errorf("단계 %s: %s", step, st)
		}
	}
	if !sandbox.AllPass(a.Isolation) {
		t.Fatalf("격리 검사: %+v", a.Isolation)
	}
}

func TestClusterRehearsalCatchesWrongStorageClass(t *testing.T) {
	c := kindProposal(t, "gp3")
	c.FailFast = 20 * time.Second
	rep, err := Rehearse(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Attempts[0]
	t.Logf("결과: 통과=%v 엔진=%s 실패=%s %.0f초\n근거:\n%s", a.Passed, a.Outcome, a.Error, a.DurationS, strings.Join(a.Evidence, "\n"))
	if rep.Passed {
		t.Fatal("없는 StorageClass로는 통과하면 안 됨")
	}
	if !strings.Contains(strings.Join(a.Evidence, "\n"), `"gp3" not found`) {
		t.Fatalf("근거에 StorageClass 오류가 있어야 함: %v", a.Evidence)
	}
	if a.Steps["fence"] != "pending" {
		t.Fatalf("쓰기 보류 전에 멈춰야 함 (원본 그대로): %v", a.Steps)
	}
}
