package main

// 데모 실패 시나리오 (README 9장).
//
// AI는 조사 도구로 대상 클러스터의 StorageClass를 보고 맞게 고르므로, 그대로 두면 리허설이 처음부터 통과해서
// "리허설 실패 → AI 진단 → 수정 → 재통과"를 보여 줄 수 없다. -demo-copy-source-storage를 주면 AI 계획의
// storageClass를 원본 DB가 실제로 쓰는 StorageClass로 바꿔 제출한다. 원본 설정을 그대로 옮긴 계획과 같다.
// 대상 클러스터에 그 StorageClass가 없으면 첫 리허설에서 DB 볼륨이 실제로 Pending이 되고, 멈춤 감지와 AI 진단이
// 실제로 돈다. 바꾼 것은 감추지 않는다: 표준 오류, 결과 JSON, 플랫폼 에이전트 기록(운영자 데모 설정 세션)에 남는다.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
)

// sourceStorageClass는 원본 CNPG 클러스터가 실제로 쓰는 StorageClass다. spec.storage.storageClass를 적었으면 그 값,
// 비웠으면(클러스터 기본값) 그 클러스터 PVC의 storageClassName이다. CNPG는 PVC에 cnpg.io/cluster 라벨을 붙인다.
func sourceStorageClass(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, ns, cluster string) (string, error) {
	obj, err := dyn.Resource(llm.CNPGClusterGVR).Namespace(ns).Get(ctx, cluster, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if sc, _, _ := unstructured.NestedString(obj.Object, "spec", "storage", "storageClass"); sc != "" {
		return sc, nil
	}
	pvcs, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + cluster})
	if err != nil {
		return "", err
	}
	for _, p := range pvcs.Items {
		if p.Spec.StorageClassName != nil && *p.Spec.StorageClassName != "" {
			return *p.Spec.StorageClassName, nil
		}
	}
	return "", fmt.Errorf("원본 클러스터 %s/%s의 spec.storage.storageClass도 비었고 PVC에도 StorageClass가 없음", ns, cluster)
}

// copySourceStorage는 계획의 storage(있으면 walStorage도)의 storageClass를 sc로 바꾼 사본과, AI가 정했던 값을 돌려준다.
// 진단 수정안과 같은 manifest-patch라서 바꾼 뒤에도 계획 검사를 그대로 거친다.
func copySourceStorage(p *plan.Proposal, draft *eplan.Plan, sc string) (*plan.Proposal, string, error) {
	if !p.ManifestAttached() {
		return nil, "", errors.New("이 계획은 대상 매니페스트를 만들지 않아(대상 클러스터가 이미 있음) StorageClass를 바꿀 수 없음")
	}
	var m struct {
		Spec struct {
			Storage    *struct{ StorageClass string } `json:"storage"`
			WalStorage *struct{ StorageClass string } `json:"walStorage"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(p.Manifest), &m); err != nil {
		return nil, "", fmt.Errorf("대상 매니페스트를 읽지 못함: %w", err)
	}
	if m.Spec.Storage == nil {
		return nil, "", errors.New("대상 매니페스트에 spec.storage가 없음")
	}
	v, _ := json.Marshal(sc)
	ops := []plan.Op{{Op: "add", Path: "/spec/storage/storageClass", Value: v}}
	if m.Spec.WalStorage != nil {
		ops = append(ops, plan.Op{Op: "add", Path: "/spec/walStorage/storageClass", Value: v})
	}
	next, is := plan.Fix{Kind: plan.KindManifestPatch, Patch: ops}.Apply(p, draft)
	if len(is) > 0 {
		return nil, "", fmt.Errorf("바꾼 계획이 검사를 통과하지 못함:\n%s", is.Error())
	}
	return next, m.Spec.Storage.StorageClass, nil
}

// storageClassExists는 대상 클러스터에 그 StorageClass가 있는지 본다. 있으면 첫 리허설이 실패하지 않는다.
func storageClassExists(ctx context.Context, cs kubernetes.Interface, sc string) (bool, error) {
	list, err := cs.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(list.Items, func(x storagev1.StorageClass) bool { return x.Name == sc }), nil
}

// demoSetting은 바꾼 내용이다. 결과 JSON과 에이전트 기록에 남는다.
type demoSetting struct {
	Scenario string `json:"scenario"`
	// SourceStorageClass는 원본이 쓰는 값이고, 계획을 이 값으로 바꿔 제출했다.
	SourceStorageClass string `json:"sourceStorageClass"`
	// AIStorageClass는 AI가 처음 정한 값이다(비었으면 대상 클러스터 기본값).
	AIStorageClass string `json:"aiStorageClass"`
	// TargetHasIt는 대상 클러스터에도 원본 값이 있는지다. true면 첫 리허설이 실패하지 않는다. 확인하지 못했으면 nil.
	TargetHasIt *bool `json:"targetHasIt,omitempty"`
}

const demoScenario = "copy-source-storage"

// logDemoSetting은 AI가 한 일이 아니라 운영자 설정이라는 것을 플랫폼 에이전트 기록에 따로 남긴다.
// 콘솔의 AI 활동과 이전 화면(planRef)에 리허설 실패보다 먼저 보인다.
func logDemoSetting(ctx context.Context, sink agentlog.Sink, planRef string, d demoSetting) error {
	ses, err := sink.Start(ctx, agentlog.Start{Kind: "other", Title: "운영자 데모 설정: 원본 StorageClass 그대로 옮기기", PlanRef: planRef})
	if err != nil {
		return err
	}
	ses.Log(ctx, agentlog.Event{
		Type:   agentlog.Decision,
		Status: "ok",
		Input:  d,
		Output: fmt.Sprintf("AI 계획의 storageClass %q를 원본 값 %q로 바꿔 제출했다. AI가 아니라 운영자가 데모를 위해 정한 것이다. "+
			"대상 클러스터에 이 StorageClass가 없으면 첫 리허설이 실패하고, AI 진단이 대상 값으로 고친다.", d.AIStorageClass, d.SourceStorageClass),
	})
	return ses.Finish(ctx, true, "원본 StorageClass "+d.SourceStorageClass+"로 제출")
}
