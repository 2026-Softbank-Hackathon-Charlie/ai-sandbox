package rehearsal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
)

type recordingSink struct {
	session *recordingSession
	err     error
}

func (s recordingSink) Start(context.Context, agentlog.Start) (agentlog.Session, error) {
	return s.session, s.err
}

type recordingSession struct {
	agentlog.MemorySession
	err          error
	finishCtxErr error
	finishLimit  time.Duration
	deleteCtxErr error
	deleteLimit  time.Duration
}

func (s *recordingSession) Log(ctx context.Context, e agentlog.Event) {
	if e.Tool == "sandbox_delete" {
		s.deleteCtxErr = ctx.Err()
		if deadline, bounded := ctx.Deadline(); bounded {
			s.deleteLimit = time.Until(deadline)
		}
	}
	s.MemorySession.Log(ctx, e)
}

func (s *recordingSession) Finish(ctx context.Context, ok bool, summary string) error {
	s.finishCtxErr = ctx.Err()
	if deadline, bounded := ctx.Deadline(); bounded {
		s.finishLimit = time.Until(deadline)
	}
	_ = s.MemorySession.Finish(ctx, ok, summary)
	return s.err
}

func TestRehearseFinalizesAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewClientset(&discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": "kubernetes"}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
	})
	cs.PrependReactor("create", "serviceaccounts", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "token" {
			cancel()
			return true, nil, context.Canceled
		}
		return false, nil, nil
	})
	ses := &recordingSession{err: errors.New("recording failed postgres://app:record-secret@db/app")}
	p := proposal(t)
	rep, err := Rehearse(ctx, Config{Admin: cs, Proposal: p, Draft: &p.Plan, Sink: recordingSink{session: ses}, MaxRuns: 1})
	if rep == nil || rep.Passed || !errors.Is(err, context.Canceled) || !ses.Done || ses.OK {
		t.Fatalf("canceled execution = report:%+v, error:%v, session:%+v", rep, err, ses)
	}
	if ses.finishCtxErr != nil || ses.finishLimit <= 0 || ses.finishLimit > 5*time.Second {
		t.Fatalf("finish must use a fresh bounded context: error=%v limit=%v", ses.finishCtxErr, ses.finishLimit)
	}
	if len(rep.Warnings) != 1 || strings.Contains(rep.Warnings[0], "record-secret") {
		t.Fatalf("recording failure must be a masked warning: %v", rep.Warnings)
	}
}

func TestFinishRehearsalPreservesExecutionResult(t *testing.T) {
	for _, passed := range []bool{false, true} {
		ses := &recordingSession{err: errors.New("recording failed postgres://app:record-secret@db/app")}
		rep := &Report{Passed: passed}
		finishRehearsal(context.Background(), ses, rep, "result")
		if rep.Passed != passed || ses.OK != passed || !ses.Done || len(rep.Warnings) != 1 || strings.Contains(rep.Warnings[0], "record-secret") {
			t.Fatalf("recording changed execution result: report=%+v session=%+v", rep, ses)
		}
	}
}

func TestRehearseReportsCleanupBeforeCallback(t *testing.T) {
	for _, deleteFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "request accepted", true: "delete failed"}[deleteFails], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cs := fake.NewClientset(&discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": "kubernetes"}},
				Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
			})
			cs.PrependReactor("create", "serviceaccounts", func(a ktesting.Action) (bool, runtime.Object, error) {
				if a.GetSubresource() == "token" {
					cancel()
					return true, nil, context.Canceled
				}
				return false, nil, nil
			})
			cs.PrependReactor("delete", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) {
				if deleteFails {
					return true, nil, errors.New("delete failed postgres://app:cleanup-secret@db/app")
				}
				return true, nil, nil // Accepted, but namespace has not disappeared.
			})
			var observed Attempt
			ses := &recordingSession{}
			p := proposal(t)
			rep, err := Rehearse(ctx, Config{Admin: cs, Proposal: p, Draft: &p.Plan, Sink: recordingSink{session: ses}, MaxRuns: 1, OnAttempt: func(a Attempt) { observed = a }})
			if rep == nil || rep.Passed || !errors.Is(err, context.Canceled) || observed.Cleanup == nil {
				t.Fatalf("cleanup unavailable to callback: report=%+v error=%v callback=%+v", rep, err, observed)
			}
			cleanup := rep.Attempts[0].Cleanup
			if deleteFails {
				if cleanup.Status != "failed" || cleanup.Error == "" || strings.Contains(cleanup.Error, "cleanup-secret") || len(rep.Warnings) != 1 {
					t.Fatalf("cleanup failure not reported safely: %+v warnings=%v", cleanup, rep.Warnings)
				}
			} else if cleanup.Status != "delete_requested" || cleanup.Error != "" || len(rep.Warnings) != 0 {
				t.Fatalf("request acceptance must not claim deletion complete: %+v", cleanup)
			}
			if _, err := cs.CoreV1().Namespaces().Get(context.Background(), observed.Namespace, metav1.GetOptions{}); err != nil {
				t.Fatalf("fake namespace should still exist after accepted request: %v", err)
			}
			if ses.deleteCtxErr != nil || ses.deleteLimit <= 0 || ses.deleteLimit > time.Minute {
				t.Fatalf("cleanup must use a fresh bounded context: error=%v limit=%v", ses.deleteCtxErr, ses.deleteLimit)
			}
			events := ses.Events
			last := events[len(events)-1]
			if last.Tool != "sandbox_delete" || last.Status != cleanup.Status {
				t.Fatalf("cleanup event disagrees with report: %+v vs %+v", last, cleanup)
			}
		})
	}
}

func TestRehearseSessionStartFailureRemainsFatal(t *testing.T) {
	want := errors.New("session unavailable")
	p := proposal(t)
	rep, err := Rehearse(context.Background(), Config{Proposal: p, Draft: &p.Plan, Sink: recordingSink{err: want}})
	if rep != nil || !errors.Is(err, want) {
		t.Fatalf("start failure = report:%+v error:%v", rep, err)
	}
}
