package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	App string
	// Namespace는 환경별 네임스페이스(Namespaces)에 없는 환경에 쓰는 네임스페이스다.
	Namespace string
	// Namespaces는 환경별 DB 네임스페이스다. 환경마다 다를 수 있다(gke는 charlie-apps, eks는 moimroom).
	Namespaces map[string]string
	// Envs는 이번 이전의 원본과 대상 환경 이름이다.
	Envs []string
}

// NamespaceFor는 환경의 DB 네임스페이스다. 환경별 값이 없으면 Namespace를 쓴다.
func (s PlanScope) NamespaceFor(env string) string {
	if ns := s.Namespaces[env]; ns != "" {
		return ns
	}
	return s.Namespace
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
			Description: "환경의 노드별 할당 가능(cpu, memory), 이미 떠 있는 Pod가 요청한 양(requestedCpu, requestedMemory), 남은 여유(freeCpu, freeMemory), 준비 상태, 새 Pod를 받지 않는 노드(unschedulable, NoSchedule taint)를 본다. 대상 매니페스트의 resources와 instances를 정할 때 쓴다. Pod를 읽지 못하면 free 없이 할당 가능만 준다.",
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
				// 노드마다 떠 있는 Pod의 requests 합. 스케줄러가 보는 것은 할당 가능이 아니라 이 여유다.
				// 2026-11-05 실제 EKS: 할당 가능 1930m만 보고 2인스턴스×500m를 잡아 리허설 1회차가 CPU 부족으로 멈췄다.
				reqs, podErr := nodeRequests(ctx, clusters[in.Env].Typed)
				out := []map[string]any{}
				for _, n := range list.Items {
					ready := false
					for _, c := range n.Status.Conditions {
						if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
							ready = true
						}
					}
					m := map[string]any{
						"name": n.Name, "ready": ready,
						"cpu": n.Status.Allocatable.Cpu().String(), "memory": n.Status.Allocatable.Memory().String(),
						"arch": n.Status.NodeInfo.Architecture,
					}
					// 새 Pod가 못 오는 노드: cordon, NoSchedule·NoExecute taint (control-plane 등). 여유가 있어도 쓸 수 없다.
					var taints []string
					for _, tt := range n.Spec.Taints {
						if tt.Effect == corev1.TaintEffectNoSchedule || tt.Effect == corev1.TaintEffectNoExecute {
							taints = append(taints, tt.Key+":"+string(tt.Effect))
						}
					}
					if n.Spec.Unschedulable {
						m["unschedulable"] = true
					}
					if len(taints) > 0 {
						m["taints"] = taints
					}
					if podErr == nil {
						r := reqs[n.Name]
						freeCPU, freeMem := n.Status.Allocatable.Cpu().DeepCopy(), n.Status.Allocatable.Memory().DeepCopy()
						freeCPU.Sub(r.cpu)
						freeMem.Sub(r.mem)
						m["requestedCpu"], m["requestedMemory"] = r.cpu.String(), r.mem.String()
						m["freeCpu"], m["freeMemory"] = freeCPU.String(), freeMem.String()
					} else {
						m["free"] = "unknown: " + podErr.Error()
					}
					out = append(out, m)
				}
				sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
				return out, nil
			},
		},
		{
			Name:        "get_cnpg_cluster",
			Description: "환경의 DB 네임스페이스(환경마다 다를 수 있음)에 있는 CNPG Cluster 설정(spec의 instances, imageName, storage, resources, bootstrap, replica)과 상태 요약을 본다.",
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
				obj, err := clusters[in.Env].Dynamic.Resource(CNPGClusterGVR).Namespace(scope.NamespaceFor(in.Env)).Get(ctx, in.Name, metav1.GetOptions{})
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

type podRequests struct{ cpu, mem resource.Quantity }

// nodeRequests는 끝나지 않은 Pod의 requests를 노드별로 더한다. 스케줄러처럼 컨테이너 합과
// init 컨테이너 가운데 큰 값 중 큰 것을 Pod 하나의 요청으로 본다.
func nodeRequests(ctx context.Context, cs kubernetes.Interface) (map[string]podRequests, error) {
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "status.phase!=Succeeded,status.phase!=Failed"})
	if err != nil {
		return nil, err
	}
	out := map[string]podRequests{}
	for _, p := range pods.Items {
		// field selector를 지원하지 않는 API(가짜 클라이언트 포함)에서도 끝난 Pod는 세지 않는다.
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		var cpu, mem, initCPU, initMem resource.Quantity
		for _, c := range p.Spec.Containers {
			cpu.Add(*c.Resources.Requests.Cpu())
			mem.Add(*c.Resources.Requests.Memory())
		}
		for _, c := range p.Spec.InitContainers {
			// 사이드카(restartPolicy: Always)는 앱 컨테이너와 함께 계속 떠 있으므로 스케줄러처럼 더한다.
			if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
				cpu.Add(*c.Resources.Requests.Cpu())
				mem.Add(*c.Resources.Requests.Memory())
				continue
			}
			if q := c.Resources.Requests.Cpu(); q.Cmp(initCPU) > 0 {
				initCPU = q.DeepCopy()
			}
			if q := c.Resources.Requests.Memory(); q.Cmp(initMem) > 0 {
				initMem = q.DeepCopy()
			}
		}
		if initCPU.Cmp(cpu) > 0 {
			cpu = initCPU
		}
		if initMem.Cmp(mem) > 0 {
			mem = initMem
		}
		if p.Spec.Overhead != nil {
			cpu.Add(*p.Spec.Overhead.Cpu())
			mem.Add(*p.Spec.Overhead.Memory())
		}
		r := out[p.Spec.NodeName]
		r.cpu.Add(cpu)
		r.mem.Add(mem)
		out[p.Spec.NodeName] = r
	}
	return out, nil
}
