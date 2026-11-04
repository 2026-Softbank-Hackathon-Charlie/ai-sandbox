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
	// 실제 클러스터의 운영자 기본 이미지(임시 원본이 쓰는 것)와 메이저 버전을 맞출 때 쓴다. 예: ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie
	if img := os.Getenv("REHEARSAL_IMAGE"); img != "" {
		m = strings.Replace(m, "imageName: ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie", "imageName: "+img, 1)
	}
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

// 실제 원본에 맞춰 CNPG 기본값(max_connections 100, max_worker_processes 32)보다 작게 정한 대상도 리허설을 통과해야 한다.
// 임시 원본이 기본값으로 뜨면 hot standby 조건에 걸려 대상이 뜨지 않는다.
func TestClusterRehearsalWithSmallStandbyParams(t *testing.T) {
	c := kindProposal(t, "standard")
	c.Proposal.Manifest = strings.Replace(c.Proposal.Manifest, "  instances: 1\n",
		"  instances: 1\n  postgresql:\n    parameters:\n      max_connections: \"60\"\n      max_worker_processes: \"8\"\n", 1)
	rep, err := Rehearse(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Attempts[0]
	t.Logf("결과: 통과=%v 엔진=%s %.0f초 오류=%s", a.Passed, a.Outcome, a.DurationS, a.Error)
	if !rep.Passed || a.Outcome != "succeeded" {
		t.Fatalf("통과해야 함: %+v", a)
	}
}

// 원본과 대상의 PostgreSQL 메이저 버전이 다르면 경고 이벤트 없이 pg_basebackup Job만 거듭 실패한다.
// 계획 검사는 원본 이미지를 읽었을 때만 막으므로, 리허설은 Job 실패로 빨리 멈추고 로그의 오류 줄을 근거로 남겨야 한다.
func TestClusterRehearsalCatchesMajorVersionMismatch(t *testing.T) {
	c := kindProposal(t, "standard")
	c.Proposal.SourceImage = "ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie"
	c.Proposal.Manifest = strings.Replace(c.Proposal.Manifest, "imageName: ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie", "imageName: ghcr.io/cloudnative-pg/postgresql:17.11", 1)
	start := time.Now()
	rep, err := Rehearse(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Attempts[0]
	t.Logf("결과: 통과=%v %.0f초 오류=%s\n근거:\n%s", a.Passed, a.DurationS, a.Error, strings.Join(a.Evidence, "\n"))
	if rep.Passed || time.Since(start) > 4*time.Minute {
		t.Fatalf("catchupTimeout(5분) 전에 멈춰야 함: 통과=%v %s", rep.Passed, time.Since(start))
	}
	if !strings.Contains(strings.Join(a.Evidence, "\n"), "incompatible server version") {
		t.Fatalf("pg_basebackup 오류 줄이 근거에 있어야 함: %v", a.Evidence)
	}
}
