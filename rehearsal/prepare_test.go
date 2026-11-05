package rehearsal

import (
	"context"
	"errors"
	"testing"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/sandbox"
)

// 미리 기본 Quota로 만든 샌드박스는 대상 매니페스트만큼 올리고, 이미 큰 값은 줄이지 않는다.
func TestRaiseQuota(t *testing.T) {
	q := resource.MustParse
	cs := fake.NewClientset(&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-quota", Namespace: "sb"}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
		corev1.ResourceRequestsCPU: q("4200m"), corev1.ResourceLimitsCPU: q("4200m"),
		corev1.ResourceRequestsMemory: q("16Gi"), corev1.ResourceLimitsMemory: q("16Gi"),
		corev1.ResourcePods: q("20"),
	}}})
	if err := raiseQuota(context.Background(), cs, "sb", sandbox.Config{CPU: "7", Memory: "11Gi"}); err != nil {
		t.Fatal(err)
	}
	got, err := cs.CoreV1().ResourceQuotas("sb").Get(context.Background(), "sandbox-quota", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h := got.Spec.Hard
	if c := h[corev1.ResourceRequestsCPU]; c.Cmp(q("7")) != 0 {
		t.Fatalf("requests.cpu = %s, want 7", c.String())
	}
	if m := h[corev1.ResourceLimitsMemory]; m.Cmp(q("16Gi")) != 0 {
		t.Fatalf("limits.memory = %s, want 16Gi (do not lower)", m.String())
	}
	if p := h[corev1.ResourcePods]; p.Cmp(q("20")) != 0 {
		t.Fatalf("pods = %s, want 20 (untouched)", p.String())
	}
}

// 계획의 원본·대상·스키마가 미리 준비할 때 쓴 초안과 다르면 미리 만든 것을 쓰지 않는다.
func TestPrewarmMatches(t *testing.T) {
	draft := eplan.Plan{}
	draft.Source.Cluster, draft.Source.Namespace, draft.Source.DSN = "moimroom-gcp-db", "charlie-apps", "postgres://app:${X}@h:5432/app"
	draft.Target.Cluster, draft.Target.Env = "moimroom-db", "eks"
	draft.Verify.Schemas = []string{"app"}
	p := &Prewarm{plan: draft}
	same := draft
	if !p.matches(&same) {
		t.Fatal("same plan should match")
	}
	for name, edit := range map[string]func(*eplan.Plan){
		"source cluster": func(x *eplan.Plan) { x.Source.Cluster = "other" },
		"target env":     func(x *eplan.Plan) { x.Target.Env = "gke" },
		"schemas":        func(x *eplan.Plan) { x.Verify.Schemas = []string{"app", "audit"} },
	} {
		x := draft
		x.Verify.Schemas = append([]string(nil), draft.Verify.Schemas...)
		edit(&x)
		if p.matches(&x) {
			t.Fatalf("%s changed but matched", name)
		}
	}
}

// 원본 클러스터 값이 없으면 임시 원본 설정을 AI 매니페스트에서 가져와야 하므로 미리 준비하지 않는다. nil은 Discard해도 된다.
func TestStartPrewarmNeedsSourceCluster(t *testing.T) {
	p := StartPrewarm(context.Background(), Config{}, &eplan.Plan{}, nil)
	if p != nil {
		t.Fatal("want nil without source cluster")
	}
	p.Discard(context.Background())
	// 미러·사설 레지스트리 이미지면 임시 원본 이미지를 매니페스트에서 가져와야 하므로 미리 준비하지 않는다
	if p := StartPrewarm(context.Background(), Config{}, &eplan.Plan{}, &plan.SourceCluster{Image: "registry.example.com/postgresql:17.2"}); p != nil {
		t.Fatal("want nil for a non-CNPG source image")
	}
}

// quota 컨트롤러가 status를 고쳐 Update가 충돌해도 다시 시도한다.
func TestRaiseQuotaRetriesConflict(t *testing.T) {
	q := resource.MustParse
	cs := fake.NewClientset(&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-quota", Namespace: "sb"}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
		corev1.ResourceRequestsCPU: q("1"), corev1.ResourceLimitsCPU: q("1"), corev1.ResourceRequestsMemory: q("1Gi"), corev1.ResourceLimitsMemory: q("1Gi"),
	}}})
	conflicts := 0
	cs.PrependReactor("update", "resourcequotas", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts < 2 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "resourcequotas"}, "sandbox-quota", errors.New("modified"))
		}
		return false, nil, nil
	})
	if err := raiseQuota(context.Background(), cs, "sb", sandbox.Config{CPU: "2", Memory: "2Gi"}); err != nil {
		t.Fatalf("err = %v after %d conflicts", err, conflicts)
	}
	got, _ := cs.CoreV1().ResourceQuotas("sb").Get(context.Background(), "sandbox-quota", metav1.GetOptions{})
	if c := got.Spec.Hard[corev1.ResourceLimitsCPU]; c.Cmp(q("2")) != 0 || conflicts != 2 {
		t.Fatalf("limits.cpu = %s, conflicts = %d", c.String(), conflicts)
	}
}
