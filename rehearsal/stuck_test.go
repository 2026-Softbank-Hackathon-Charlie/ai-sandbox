package rehearsal

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func warning(name string, count int32, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "sb"},
		Type:           "Warning",
		Reason:         "ProvisioningFailed",
		Message:        `storageclass.storage.k8s.io "gp3" not found`,
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "db-1"},
		Count:          count,
		LastTimestamp:  metav1.NewTime(at),
	}
}

// fake clientset은 type=Warning 필드 선택을 거르지 않지만, 넣는 이벤트가 모두 Warning이라 상관없다.
func runWatch(t *testing.T, cs *fake.Clientset, window, wait time.Duration, during func()) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	out := make(chan []string, 1)
	stopped := make(chan struct{})
	go func() { watchStuck(ctx, cs, "sb", window, out, func() {}); close(stopped) }()
	if during != nil {
		go during()
	}
	select {
	case <-out:
		return true
	case <-stopped:
		return false
	}
}

func TestStuckIgnoresOneOffWarning(t *testing.T) {
	cs := fake.NewClientset()
	fired := runWatch(t, cs, 3*time.Second, 12*time.Second, func() {
		time.Sleep(1 * time.Second)
		_, _ = cs.CoreV1().Events("sb").Create(context.Background(), warning("w1", 1, time.Now()), metav1.CreateOptions{})
		// 한 번 나고 다시 일어나지 않음 (풀림)
	})
	if fired {
		t.Fatal("한 번 나고 풀린 경고로 엔진을 멈추면 안 됨")
	}
}

func TestStuckIgnoresWarningsFromBeforeTheRun(t *testing.T) {
	cs := fake.NewClientset(warning("old", 5, time.Now().Add(-time.Minute)))
	if runWatch(t, cs, 3*time.Second, 10*time.Second, nil) {
		t.Fatal("이 실행 전부터 있던 경고로 멈추면 안 됨")
	}
}

func TestStuckFiresOnRecurringWarning(t *testing.T) {
	cs := fake.NewClientset()
	fired := runWatch(t, cs, 3*time.Second, 20*time.Second, func() {
		time.Sleep(1 * time.Second)
		ctx := context.Background()
		_, _ = cs.CoreV1().Events("sb").Create(ctx, warning("w1", 1, time.Now()), metav1.CreateOptions{})
		for i := int32(2); i < 20; i++ {
			time.Sleep(1500 * time.Millisecond)
			_, _ = cs.CoreV1().Events("sb").Update(ctx, warning("w1", i, time.Now()), metav1.UpdateOptions{})
		}
	})
	if !fired {
		t.Fatal("계속 되풀이되는 경고는 멈춰야 함")
	}
}

func TestFailedJobPods(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	pod := func(name, job string, phase corev1.PodPhase, created time.Time) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "sb", CreationTimestamp: metav1.NewTime(created)}, Status: corev1.PodStatus{Phase: phase}}
		if job != "" {
			p.Labels = map[string]string{"job-name": job}
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "full-recovery", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
		return p
	}
	now := time.Now()
	objs := []runtime.Object{
		pod("dst-1-pgbasebackup-a", "dst-1-pgbasebackup", corev1.PodFailed, now),
		pod("dst-1-pgbasebackup-b", "dst-1-pgbasebackup", corev1.PodFailed, now.Add(time.Second)),
		pod("old-job-x", "old-job", corev1.PodFailed, since.Add(-time.Hour)), // 이 실행 전
		pod("plain", "", corev1.PodFailed, now),                              // Job Pod가 아님
	}
	cs := fake.NewClientset(objs...)
	if got := failedJobPods(context.Background(), cs, "sb", since, 3); len(got) != 0 {
		t.Fatalf("두 번 실패로는 멈추지 않아야 함: %v", got)
	}
	_, _ = cs.CoreV1().Pods("sb").Create(context.Background(), pod("dst-1-pgbasebackup-c", "dst-1-pgbasebackup", corev1.PodFailed, now.Add(2*time.Second)), metav1.CreateOptions{})
	got := failedJobPods(context.Background(), cs, "sb", since, 3)
	if len(got) == 0 || !strings.Contains(got[0], "job/dst-1-pgbasebackup") || !strings.Contains(got[0], "3번 실패") || !strings.Contains(got[0], "pod/dst-1-pgbasebackup-c") || !strings.Contains(got[0], "종료 코드 1") {
		t.Fatalf("근거: %v", got)
	}
}
