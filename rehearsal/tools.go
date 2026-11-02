package rehearsal

import (
	"context"
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

// SandboxTools는 진단 AI가 샌드박스를 더 보는 도구다. 샌드박스 토큰으로 만든 클라이언트를 받으므로
// 그 네임스페이스 밖은 RBAC가 막는다. 모두 읽기 전용이다.
func SandboxTools(cs kubernetes.Interface, dyn dynamic.Interface, ns string) []llm.Tool {
	none := map[string]any{}
	return []llm.Tool{
		{
			Name:        "sandbox_events",
			Description: "리허설 샌드박스의 경고 이벤트(시간순, 최근 30개)를 본다.",
			Properties:  none,
			Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return warningEvents(ctx, cs, ns), nil
			},
		},
		{
			Name:        "sandbox_cnpg_status",
			Description: "리허설 샌드박스의 CNPG Cluster 상태(phase, 인스턴스 준비 수, 조건 메시지)를 본다.",
			Properties:  none,
			Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
				list, err := dyn.Resource(llm.CNPGClusterGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				out := []map[string]any{}
				for _, c := range list.Items {
					st, _ := c.Object["status"].(map[string]any)
					spec, _ := c.Object["spec"].(map[string]any)
					m := map[string]any{"name": c.GetName()}
					for _, k := range []string{"phase", "phaseReason", "instances", "readyInstances", "currentPrimary"} {
						if v, ok := st[k]; ok {
							m[k] = v
						}
					}
					if s, ok := spec["storage"]; ok {
						m["storage"] = s
					}
					if conds, ok := st["conditions"].([]any); ok {
						var msgs []string
						for _, x := range conds {
							if cm, ok := x.(map[string]any); ok {
								msgs = append(msgs, mask.String(strings.TrimSpace(str(cm["type"])+"="+str(cm["status"])+" "+str(cm["message"]))))
							}
						}
						m["conditions"] = msgs
					}
					out = append(out, m)
				}
				return out, nil
			},
		},
		{
			Name:        "sandbox_pvcs",
			Description: "리허설 샌드박스의 PVC(이름, 상태, StorageClass, 요청 크기)를 본다.",
			Properties:  none,
			Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
				list, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				out := []map[string]any{}
				for _, p := range list.Items {
					sc := ""
					if p.Spec.StorageClassName != nil {
						sc = *p.Spec.StorageClassName
					}
					out = append(out, map[string]any{"name": p.Name, "phase": string(p.Status.Phase), "storageClass": sc, "request": p.Spec.Resources.Requests.Storage().String()})
				}
				return out, nil
			},
		},
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
