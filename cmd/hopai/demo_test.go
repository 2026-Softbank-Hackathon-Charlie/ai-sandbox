package main

import (
	"context"
	"encoding/json"
	"errors"
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
	k8stesting "k8s.io/client-go/testing"

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

var clusterGVR = map[schema.GroupVersionResource]string{llm.CNPGClusterGVR: "ClusterList"}

// sourceCNPG는 원본 CNPG 클러스터다. spec은 storage, walStorage만 받는다(nil이면 없음).
func sourceCNPG(storage, wal map[string]any) *dynamicfake.FakeDynamicClient {
	spec := map[string]any{"storage": storage}
	if wal != nil {
		spec["walStorage"] = wal
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), clusterGVR, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "moimroom-db-gke", "namespace": "moimroom"},
		"spec":     spec,
	}})
}

func pvc(name, cluster, role, sc string) *corev1.PersistentVolumeClaim {
	labels := map[string]string{pvcClusterLabel: cluster}
	if role != "" {
		labels[pvcRoleLabel] = role
	}
	p := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "moimroom", Labels: labels}}
	if sc != "" {
		p.Spec.StorageClassName = &sc
	}
	return p
}

func TestSourceStorageClasses(t *testing.T) {
	ctx := context.Background()
	get := func(dyn *dynamicfake.FakeDynamicClient, objs ...*corev1.PersistentVolumeClaim) (sourceStorage, error) {
		cs := fake.NewSimpleClientset()
		for _, o := range objs {
			_ = cs.Tracker().Add(o)
		}
		return sourceStorageClasses(ctx, cs, dyn, "moimroom", "moimroom-db-gke")
	}

	// 클러스터에 적은 값이 먼저다
	if s, err := get(sourceCNPG(map[string]any{"storageClass": "premium-rwo"}, map[string]any{"storageClass": "wal-ssd"})); err != nil || s != (sourceStorage{Data: "premium-rwo", WAL: "wal-ssd"}) {
		t.Fatalf("spec 값: %+v %v", s, err)
	}
	// 비웠으면 그 클러스터 PVC가 받은 값. WAL PVC가 먼저 나와도 데이터 값은 데이터 PVC에서 고르고, 다른 클러스터 PVC는 보지 않는다
	s, err := get(sourceCNPG(map[string]any{"size": "5Gi"}, map[string]any{"size": "1Gi"}),
		pvc("a-wal", "moimroom-db-gke", pvcRoleWAL, "wal-ssd"),
		pvc("b-other", "other-db", pvcRoleData, "gp3"),
		pvc("c-data", "moimroom-db-gke", pvcRoleData, "standard-rwo"))
	if err != nil || s != (sourceStorage{Data: "standard-rwo", WAL: "wal-ssd"}) {
		t.Fatalf("PVC 값: %+v %v", s, err)
	}
	// walStorage를 쓰지 않는 원본은 WAL 값을 비운다. 역할 라벨이 없는 예전 PVC는 데이터로 본다
	if s, err := get(sourceCNPG(map[string]any{"size": "5Gi"}, nil), pvc("old", "moimroom-db-gke", "", "standard-rwo")); err != nil || s != (sourceStorage{Data: "standard-rwo"}) {
		t.Fatalf("WAL 없음: %+v %v", s, err)
	}
	// 데이터 값을 어디서도 모르면 오류 (WAL PVC 값으로 대신하지 않음)
	if _, err := get(sourceCNPG(map[string]any{"size": "5Gi"}, nil), pvc("a-wal", "moimroom-db-gke", pvcRoleWAL, "wal-ssd")); err == nil {
		t.Fatal("데이터 StorageClass를 모르면 오류여야 함")
	}
	// 원본 클러스터가 없으면 오류
	if _, err := get(dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), clusterGVR)); err == nil {
		t.Fatal("원본 클러스터가 없으면 오류여야 함")
	}
	// PVC 목록 권한이 없으면 무엇이 필요한지 알린다
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := sourceStorageClasses(ctx, cs, sourceCNPG(map[string]any{"size": "5Gi"}, nil), "moimroom", "moimroom-db-gke"); err == nil || !strings.Contains(err.Error(), "persistentvolumeclaims list") {
		t.Fatalf("권한 오류: %v", err)
	}
}

func TestCopySourceStorage(t *testing.T) {
	p, d := testProposal(t)
	next, ai, err := copySourceStorage(p, d, sourceStorage{Data: "standard-rwo"})
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

	// walStorage가 있으면 원본 WAL 값으로, 원본이 WAL을 따로 쓰지 않으면 데이터 값으로
	w := *p
	w.Manifest = strings.Replace(p.Manifest, "  resources:\n", "  walStorage:\n    size: 1Gi\n    storageClass: gp3\n  resources:\n", 1)
	next, _, err = copySourceStorage(&w, d, sourceStorage{Data: "standard-rwo", WAL: "wal-ssd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next.Manifest, "storageClass: standard-rwo") || !strings.Contains(next.Manifest, "storageClass: wal-ssd") {
		t.Fatalf("storage와 walStorage를 각각 바꿔야 함:\n%s", next.Manifest)
	}
	next, _, _ = copySourceStorage(&w, d, sourceStorage{Data: "standard-rwo"})
	if strings.Count(next.Manifest, "storageClass: standard-rwo") != 2 {
		t.Fatalf("원본 WAL 값이 없으면 데이터 값으로:\n%s", next.Manifest)
	}

	// StorageClass 이름 규칙을 어기면 검사에 걸린다
	if _, _, err := copySourceStorage(p, d, sourceStorage{Data: "Not_A_Name"}); err == nil {
		t.Fatal("잘못된 이름은 검사에 걸려야 함")
	}
	// 대상 매니페스트를 만들지 않는 계획(대상 클러스터가 이미 있음)에는 쓸 수 없다
	np := *p
	np.Plan.Target.Manifest = ""
	if _, _, err := copySourceStorage(&np, d, sourceStorage{Data: "standard-rwo"}); err == nil || !strings.Contains(err.Error(), "매니페스트를 만들지 않아") {
		t.Fatalf("매니페스트 없는 계획: %v", err)
	}
}

func TestPrepareDemo(t *testing.T) {
	ctx := context.Background()
	p, d := testProposal(t) // AI는 gp3를 골랐다
	source := func(sc string) llm.Cluster {
		return llm.Cluster{Typed: fake.NewSimpleClientset(), Dynamic: sourceCNPG(map[string]any{"storageClass": sc}, nil)}
	}
	target := func(names ...string) *fake.Clientset {
		cs := fake.NewSimpleClientset()
		for _, n := range names {
			_ = cs.Tracker().Add(&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: n}})
		}
		return cs
	}

	// 원본 standard-rwo, 대상(EKS)에는 gp3뿐: 바꿔서 제출하고 1차가 실패할 조건
	next, demo, err := prepareDemo(ctx, p, d, source("standard-rwo"), target("gp3"), "moimroom", "moimroom-db-gke")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next.Manifest, "storageClass: standard-rwo") {
		t.Fatalf("바꾼 계획:\n%s", next.Manifest)
	}
	if *demo != (demoSetting{Scenario: demoScenario, SourceStorageClass: "standard-rwo", AIStorageClass: "gp3"}) {
		t.Fatalf("데모 설정: %+v", demo)
	}

	// 실패가 나지 않을 조건이면 시작하지 않는다
	for name, c := range map[string]struct {
		source llm.Cluster
		target *fake.Clientset
		want   string
	}{
		"AI가 이미 원본 값을 고름": {source("gp3"), target("gp3"), "이미 원본과 같은"},
		"대상에도 원본 값이 있음":   {source("standard-rwo"), target("gp3", "standard-rwo"), "대상 클러스터에도"},
	} {
		if _, _, err := prepareDemo(ctx, p, d, c.source, c.target, "moimroom", "moimroom-db-gke"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// 대상 목록을 읽지 못하면 실패할지 모르므로 멈춘다
	denied := target()
	denied.PrependReactor("list", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, _, err := prepareDemo(ctx, p, d, source("standard-rwo"), denied, "moimroom", "moimroom-db-gke"); err == nil || !strings.Contains(err.Error(), "확인할 수 없음") {
		t.Fatalf("대상 확인 실패: %v", err)
	}
	if !strings.Contains(p.Manifest, "storageClass: gp3") {
		t.Fatal("원래 계획은 그대로 둬야 함")
	}
}

func TestLogDemoSetting(t *testing.T) {
	for _, c := range []struct{ ai, want string }{
		{"gp3", `storageClass("gp3")`},
		// AI가 비웠으면 빈 따옴표가 아니라 기본값이라고 적는다
		{"", "storageClass(대상 클러스터 기본값)"},
	} {
		mem := &agentlog.Memory{}
		d := demoSetting{Scenario: demoScenario, SourceStorageClass: "standard-rwo", AIStorageClass: c.ai}
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
		out, _ := s.Events[0].Output.(string)
		for _, want := range []string{c.want, `"standard-rwo"`, "운영자가 데모를 위해"} {
			if !strings.Contains(out, want) {
				t.Errorf("기록에 %q가 없음: %s", want, out)
			}
		}
	}
}
