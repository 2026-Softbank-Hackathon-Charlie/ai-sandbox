package rehearsal

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

// maxStatusLine은 상태 한 줄의 최대 글자 수다. 스케줄러 메시지는 노드마다 사유를 나열해서 길다.
const maxStatusLine = 300

// maxEvidenceBytes는 단서 전체 크기의 상한이다 (clipEvidence).
const maxEvidenceBytes = 48 << 10

// sourceNotReadyEvidence는 임시 원본이 제때 healthy가 되지 못했을 때 원인을 찾을 단서를 모은다.
// 2026-11-05 실제 EKS 리허설이 "임시 원본이 준비되지 않음: 6m0s 안에 healthy가 되지 않음"만 남기고 끝나서,
// 볼륨이 Pending인지, Pod가 스케줄되지 않았는지, 이미지를 못 받았는지 콘솔에서는 알 수 없었다.
// 샌드박스를 곧 지우므로 지우기 전에 Pod와 PVC의 상태와 경고 이벤트를 읽어 둔다.
// 모두 샌드박스 토큰이 이미 가진 읽기 권한(Pod, PVC, 이벤트)만 쓴다.
//
// lines는 리허설 세션 로그와 리포트의 evidence에 남길 줄이고, brief는 오류 메시지 끝에 붙일 짧은 요약이다
// (문제가 있는 PVC와 Pod 최대 3개, 없으면 빈 문자열).
func sourceNotReadyEvidence(ctx context.Context, cs kubernetes.Interface, ns string) (lines []string, brief string) {
	var problems []string

	pvcs, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		lines = append(lines, statusLine("PVC 목록을 읽지 못함: %v", err))
	} else {
		sort.Slice(pvcs.Items, func(i, j int) bool { return pvcs.Items[i].Name < pvcs.Items[j].Name })
		for _, p := range pvcs.Items {
			sc := "(기본값)"
			if p.Spec.StorageClassName != nil && *p.Spec.StorageClassName != "" {
				sc = *p.Spec.StorageClassName
			}
			lines = append(lines, statusLine("pvc/%s  %s  storageClass=%s", p.Name, p.Status.Phase, sc))
			if p.Status.Phase != corev1.ClaimBound {
				problems = append(problems, statusLine("pvc/%s %s(storageClass=%s)", p.Name, p.Status.Phase, sc))
			}
		}
	}

	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		lines = append(lines, statusLine("Pod 목록을 읽지 못함: %v", err))
	} else {
		sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
		for _, p := range pods.Items {
			detail := podDetail(&p)
			line := statusLine("pod/%s  %s%s", p.Name, p.Status.Phase, detail)
			lines = append(lines, line)
			if p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodSucceeded {
				problems = append(problems, statusLine("pod/%s %s%s", p.Name, p.Status.Phase, detail))
			}
		}
	}

	lines = append(lines, warningEvents(ctx, cs, ns)...)

	if len(problems) > 3 {
		problems = problems[:3]
	}
	return clipEvidence(lines), strings.Join(problems, "; ")
}

// logSourceStatus는 모은 줄을 줄마다 이벤트 하나로 리허설 세션에 남긴다.
// 한 이벤트에 여러 줄을 넣으면 콘솔이 줄바꿈을 접어서 읽기 어려울 수 있고, 격리 검사 결과도 줄마다 이벤트로 남기는 방식과 같다.
func logSourceStatus(ctx context.Context, ses agentlog.Session, lines []string) {
	for _, l := range lines {
		ses.Log(ctx, agentlog.Event{Type: agentlog.ToolResult, Tool: "sandbox_source_status", Status: "not_ready", Output: l})
	}
}

// clipEvidence는 줄마다 길이를 제한하고 전체 크기를 maxEvidenceBytes 안으로 맞춘다.
// 플랫폼은 이벤트 한 개의 입력과 출력을 64 KiB까지만 받는다. 경고 이벤트의 메시지는 길이 제한이 없어서 여기서 줄인다.
func clipEvidence(lines []string) []string {
	out := make([]string, 0, len(lines))
	total := 0
	for i, l := range lines {
		if r := []rune(l); len(r) > maxStatusLine {
			l = string(r[:maxStatusLine]) + "…"
		}
		if total+len(l) > maxEvidenceBytes {
			out = append(out, fmt.Sprintf("… (%d줄 생략)", len(lines)-i))
			break
		}
		total += len(l)
		out = append(out, l)
	}
	return out
}

// podDetail은 Pod가 왜 준비되지 않았는지 알려 주는 한 구절이다: 스케줄되지 않은 이유, 컨테이너가 기다리는 이유나 종료 코드.
func podDetail(p *corev1.Pod) string {
	var parts []string
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			parts = append(parts, strings.TrimSpace(fmt.Sprintf("스케줄 안 됨 %s %s", c.Reason, c.Message)))
		}
	}
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, s := range statuses {
		switch {
		case s.State.Waiting != nil && s.State.Waiting.Reason != "":
			parts = append(parts, strings.TrimSpace(fmt.Sprintf("%s 대기 %s %s", s.Name, s.State.Waiting.Reason, s.State.Waiting.Message)))
		case s.State.Terminated != nil && s.State.Terminated.ExitCode != 0:
			parts = append(parts, fmt.Sprintf("%s 종료 코드 %d %s", s.Name, s.State.Terminated.ExitCode, s.State.Terminated.Reason))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// statusLine은 줄을 만들고 비밀값을 가린 뒤 길이를 제한한다.
func statusLine(format string, args ...any) string {
	s := mask.String(fmt.Sprintf(format, args...))
	if r := []rune(s); len(r) > maxStatusLine {
		s = string(r[:maxStatusLine]) + "…"
	}
	return s
}
