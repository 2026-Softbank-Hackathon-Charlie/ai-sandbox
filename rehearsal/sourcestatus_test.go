package rehearsal

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSourceNotReadyEvidenceShowsPendingVolumeAndUnschedulablePod(t *testing.T) {
	sc := "gp3"
	ns := "sandbox-moimroom-tbng"
	cs := fake.NewClientset(
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "moimroom-db-gke-1", Namespace: ns},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "moimroom-db-gke-1", Namespace: ns},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient cpu.",
				}},
			},
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: ns},
			Type:           corev1.EventTypeWarning,
			Reason:         "ProvisioningFailed",
			Message:        "storageclass \"gp3\" not found",
			InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "moimroom-db-gke-1"},
			LastTimestamp:  metav1.NewTime(time.Now()),
		},
	)
	lines, brief := sourceNotReadyEvidence(context.Background(), cs, ns)
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"pvc/moimroom-db-gke-1  Pending  storageClass=gp3",
		"pod/moimroom-db-gke-1  Pending (스케줄 안 됨 Unschedulable 0/3 nodes are available: 3 Insufficient cpu.)",
		"Warning  ProvisioningFailed  persistentvolumeclaim/moimroom-db-gke-1",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("%q가 없음:\n%s", want, all)
		}
	}
	if !strings.Contains(brief, "pvc/moimroom-db-gke-1 Pending(storageClass=gp3)") || !strings.Contains(brief, "Insufficient cpu") {
		t.Errorf("요약에 문제 있는 PVC와 Pod가 있어야 함: %q", brief)
	}
}

func TestSourceNotReadyEvidenceShowsImagePullAndIgnoresHealthyObjects(t *testing.T) {
	ns := "sandbox-x"
	cs := fake.NewClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: ns},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pull", Namespace: ns},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "postgres",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
				}},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "bound", Namespace: ns},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		},
	)
	lines, brief := sourceNotReadyEvidence(context.Background(), cs, ns)
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "pod/pull  Pending (postgres 대기 ImagePullBackOff Back-off pulling image)") {
		t.Errorf("이미지 풀 실패가 보여야 함:\n%s", all)
	}
	if !strings.Contains(all, "pvc/bound  Bound  storageClass=(기본값)") {
		t.Errorf("기본 StorageClass 표시:\n%s", all)
	}
	if strings.Contains(brief, "ok") || strings.Contains(brief, "bound") || !strings.Contains(brief, "pod/pull") {
		t.Errorf("요약에는 문제 있는 것만 있어야 함: %q", brief)
	}
}

func TestSourceNotReadyEvidenceEmptyNamespaceAndLongLines(t *testing.T) {
	lines, brief := sourceNotReadyEvidence(context.Background(), fake.NewClientset(), "sandbox-empty")
	if len(lines) != 0 || brief != "" {
		t.Fatalf("아무것도 없으면 비어 있어야 함: %v %q", lines, brief)
	}
	long := strings.Repeat("노드 ", 400)
	got := statusLine("pod/x %s", long)
	if r := []rune(got); len(r) > maxStatusLine+1 {
		t.Fatalf("길이를 제한해야 함: %d", len(r))
	}
	if masked := statusLine("DATABASE_URL=postgres://app:s3cr3t-plain@db/app"); strings.Contains(masked, "s3cr3t-plain") {
		t.Fatalf("비밀값은 가려야 함: %s", masked)
	}
}
