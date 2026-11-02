package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

// 계획 도구는 모두 읽기 전용이다 (설계 문서 5장). Secret을 읽는 도구는 없다.
// 도구 함수 안에서 허용 범위(이번 이전의 두 환경, 앱 네임스페이스)를 다시 검사하고, 밖이면 ErrBlocked로 막는다.

// Cluster는 환경 하나의 읽기 전용 클라이언트다. 읽기 전용 ServiceAccount의 kubeconfig로 만든다.
type Cluster struct {
	Typed   kubernetes.Interface
	Dynamic dynamic.Interface
}

// PlanScope는 계획 도구가 볼 수 있는 범위다.
type PlanScope struct {
	App       string
	Namespace string
	// Envs는 이번 이전의 원본과 대상 환경 이름이다.
	Envs []string
}

// CNPGClusterGVR은 CloudNativePG Cluster 리소스다.
var CNPGClusterGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}

// PlanTools는 계획 작업의 도구다. platform은 비워도 된다(쿠버네티스 도구만).
func PlanTools(scope PlanScope, platform *client.Client, clusters map[string]Cluster) []Tool {
	envOK := func(env string) error {
		if !slices.Contains(scope.Envs, env) {
			return fmt.Errorf("%w: 이번 이전의 환경(%v)만 볼 수 있음, 받은 값 %q", ErrBlocked, scope.Envs, env)
		}
		if _, ok := clusters[env]; !ok {
			return fmt.Errorf("환경 %q의 읽기 전용 접속 정보가 없음", env)
		}
		return nil
	}
	envProp := map[string]any{"env": map[string]any{"type": "string", "description": "환경 이름 (원본 또는 대상)"}}

	tools := []Tool{
		{
			Name:        "list_storage_classes",
			Description: "환경의 StorageClass 이름, 프로비저너, 기본 여부, 볼륨 바인딩 방식을 본다. 대상 매니페스트의 storageClass를 정할 때 쓴다.",
			Properties:  envProp, Required: []string{"env"},
			Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Env string }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				if err := envOK(in.Env); err != nil {
					return nil, err
				}
				list, err := clusters[in.Env].Typed.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				out := []map[string]any{}
				for _, sc := range list.Items {
					m := map[string]any{"name": sc.Name, "provisioner": sc.Provisioner, "default": sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"}
					if sc.VolumeBindingMode != nil {
						m["volumeBindingMode"] = string(*sc.VolumeBindingMode)
					}
					out = append(out, m)
				}
				return out, nil
			},
		},
		{
			Name:        "node_capacity",
			Description: "환경의 노드 수와 노드별 할당 가능 CPU, 메모리, 준비 상태를 본다. 대상 매니페스트의 resources와 instances를 정할 때 쓴다.",
			Properties:  envProp, Required: []string{"env"},
			Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Env string }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				if err := envOK(in.Env); err != nil {
					return nil, err
				}
				list, err := clusters[in.Env].Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				out := []map[string]any{}
				for _, n := range list.Items {
					ready := false
					for _, c := range n.Status.Conditions {
						if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
							ready = true
						}
					}
					out = append(out, map[string]any{
						"name": n.Name, "ready": ready,
						"cpu": n.Status.Allocatable.Cpu().String(), "memory": n.Status.Allocatable.Memory().String(),
						"arch": n.Status.NodeInfo.Architecture,
					})
				}
				sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
				return out, nil
			},
		},
		{
			Name:        "get_cnpg_cluster",
			Description: "앱 네임스페이스의 CNPG Cluster 설정(spec의 instances, imageName, storage, resources, bootstrap, replica)과 상태 요약을 본다.",
			Properties: map[string]any{
				"env":  envProp["env"],
				"name": map[string]any{"type": "string", "description": "CNPG Cluster 이름"},
			},
			Required: []string{"env", "name"},
			Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Env, Name string }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				if err := envOK(in.Env); err != nil {
					return nil, err
				}
				obj, err := clusters[in.Env].Dynamic.Resource(CNPGClusterGVR).Namespace(scope.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				spec, _ := obj.Object["spec"].(map[string]any)
				status, _ := obj.Object["status"].(map[string]any)
				keep := map[string]any{}
				for _, k := range []string{"instances", "imageName", "storage", "walStorage", "resources", "bootstrap", "replica", "postgresql"} {
					if v, ok := spec[k]; ok {
						keep[k] = v
					}
				}
				st := map[string]any{}
				for _, k := range []string{"phase", "instances", "readyInstances", "currentPrimary"} {
					if v, ok := status[k]; ok {
						st[k] = v
					}
				}
				return map[string]any{"name": obj.GetName(), "namespace": obj.GetNamespace(), "spec": keep, "status": st}, nil
			},
		},
	}
	if platform != nil {
		tools = append(tools, Tool{
			Name:        "get_app",
			Description: "플랫폼에 등록된 앱 설정(네임스페이스, DB 이름, 환경별 CNPG 클러스터와 DB 호스트, 버킷)을 본다. 비밀번호는 없다.",
			Properties:  map[string]any{}, Required: nil,
			Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
				list, err := platform.ListApps(ctx)
				if err != nil {
					return nil, err
				}
				for _, a := range list.Apps {
					if a.Name == scope.App {
						return a, nil
					}
				}
				return nil, fmt.Errorf("앱 %q가 플랫폼에 없음", scope.App)
			},
		})
	}
	return tools
}
