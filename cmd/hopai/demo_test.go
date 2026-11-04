package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
)

// testProposal은 plan 패키지의 예시 계획(AI가 대상 StorageClass gp3를 고른 것)이다.
func testProposal(t *testing.T) (*plan.Proposal, *eplan.Plan) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile("../../plan/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var d eplan.Plan
	if err := json.Unmarshal(read("draft.json"), &d); err != nil {
		t.Fatal(err)
	}
	p, is := plan.Parse(read("proposal.json"), &d)
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	return p, &d
}

func TestSourceStorageClass(t *testing.T) {
	gvr := map[schema.GroupVersionResource]string{llm.CNPGClusterGVR: "ClusterList"}
	cluster := func(storage map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": "moimroom-db-gke", "namespace": "moimroom"},
			"spec":     map[string]any{"storage": storage},
		}}
	}
	pvc := func(name, cluster, sc string) *corev1.PersistentVolumeClaim {
		p := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "moimroom", Labels: map[string]string{"cnpg.io/cluster": cluster}}}
		if sc != "" {
			p.Spec.StorageClassName = &sc
		}
		return p
	}
	ctx := context.Background()

	// 클러스터에 적은 값이 먼저다
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvr, cluster(map[string]any{"size": "5Gi", "storageClass": "premium-rwo"}))
	if sc, err := sourceStorageClass(ctx, fake.NewSimpleClientset(), dyn, "moimroom", "moimroom-db-gke"); err != nil || sc != "premium-rwo" {
		t.Fatalf("spec 값: %q %v", sc, err)
	}
	// 비웠으면(클러스터 기본값) 그 클러스터의 PVC가 실제로 받은 값. 다른 클러스터의 PVC는 보지 않는다
	dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvr, cluster(map[string]any{"size": "5Gi"}))
	cs := fake.NewSimpleClientset(pvc("other-1", "other-db", "gp3"), pvc("moimroom-db-gke-1", "moimroom-db-gke", "standard-rwo"))
	if sc, err := sourceStorageClass(ctx, cs, dyn, "moimroom", "moimroom-db-gke"); err != nil || sc != "standard-rwo" {
		t.Fatalf("PVC 값: %q %v", sc, err)
	}
	// 어디에도 없으면 오류
	if _, err := sourceStorageClass(ctx, fake.NewSimpleClientset(pvc("moimroom-db-gke-1", "moimroom-db-gke", "")), dyn, "moimroom", "moimroom-db-gke"); err == nil {
		t.Fatal("StorageClass를 모르면 오류여야 함")
	}
	// 원본 클러스터가 없으면 오류
	if _, err := sourceStorageClass(ctx, cs, dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvr), "moimroom", "moimroom-db-gke"); err == nil {
		t.Fatal("원본 클러스터가 없으면 오류여야 함")
	}
}

func TestCopySourceStorage(t *testing.T) {
	p, d := testProposal(t)
	next, ai, err := copySourceStorage(p, d, "standard-rwo")
	if err != nil {
		t.Fatal(err)
	}
	if ai != "gp3" {
		t.Fatalf("AI가 정한 값: %q", ai)
	}
	if !strings.Contains(next.Manifest, "storageClass: standard-rwo") || strings.Contains(next.Manifest, "gp3") {
		t.Fatalf("바뀐 매니페스트:\n%s", next.Manifest)
	}
	if !strings.Contains(p.Manifest, "storageClass: gp3") {
		t.Fatal("원래 계획은 그대로 둬야 함 (사본을 바꿈)")
	}
	if is := next.Check(d); len(is) > 0 {
		t.Fatalf("바꾼 계획도 검사를 통과해야 함:\n%s", is.Error())
	}

	// walStorage가 있으면 같이 바꾼다
	w := *p
	w.Manifest = strings.Replace(p.Manifest, "  resources:\n", "  walStorage:\n    size: 1Gi\n    storageClass: gp3\n  resources:\n", 1)
	next, _, err = copySourceStorage(&w, d, "standard-rwo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(next.Manifest, "storageClass: standard-rwo") != 2 {
		t.Fatalf("storage와 walStorage 둘 다 바꿔야 함:\n%s", next.Manifest)
	}

	// StorageClass 이름 규칙을 어기면 검사에 걸린다
	if _, _, err := copySourceStorage(p, d, "Not_A_Name"); err == nil {
		t.Fatal("잘못된 이름은 검사에 걸려야 함")
	}
	// 대상 매니페스트를 만들지 않는 계획(대상 클러스터가 이미 있음)에는 쓸 수 없다
	np := *p
	np.Plan.Target.Manifest = ""
	if _, _, err := copySourceStorage(&np, d, "standard-rwo"); err == nil || !strings.Contains(err.Error(), "매니페스트를 만들지 않아") {
		t.Fatalf("매니페스트 없는 계획: %v", err)
	}
}

func TestStorageClassExists(t *testing.T) {
	cs := fake.NewSimpleClientset(&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp3"}})
	for sc, want := range map[string]bool{"gp3": true, "standard-rwo": false} {
		if has, err := storageClassExists(context.Background(), cs, sc); err != nil || has != want {
			t.Errorf("%s: %v %v", sc, has, err)
		}
	}
}

func TestLogDemoSetting(t *testing.T) {
	mem := &agentlog.Memory{}
	has := false
	d := demoSetting{Scenario: demoScenario, SourceStorageClass: "standard-rwo", AIStorageClass: "gp3", TargetHasIt: &has}
	if err := logDemoSetting(context.Background(), mem, "pln_1", d); err != nil {
		t.Fatal(err)
	}
	if len(mem.Sessions) != 1 {
		t.Fatalf("세션 수: %d", len(mem.Sessions))
	}
	s := mem.Sessions[0]
	// AI가 한 일로 보이지 않게, 운영자 설정이라고 밝힌 별도 세션을 Plan에 붙인다
	if s.Info.Kind != "other" || s.Info.PlanRef != "pln_1" || !strings.Contains(s.Info.Title, "운영자 데모 설정") {
		t.Fatalf("세션: %+v", s.Info)
	}
	if !s.Done || !s.OK || len(s.Events) != 1 || s.Events[0].Type != agentlog.Decision {
		t.Fatalf("기록: %+v", s)
	}
	out, _ := json.Marshal(s.Events[0])
	for _, want := range []string{"standard-rwo", "gp3", "운영자가 데모를 위해"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("기록에 %q가 없음: %s", want, out)
		}
	}
}
