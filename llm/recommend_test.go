package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

func recommendationInput() RecommendationInput {
	from, to := "gke", "onprem"
	observed := time.Now().UTC()
	return RecommendationInput{SnapshotID: "rec_123", GeneratedAt: time.Now().UTC().Add(-time.Second), ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Candidates: []RecommendationCandidate{{ID: "candidate-1", App: "moimroom-mig", Source: &from, Target: &to, AllowedActions: []string{"stay", "move", "insufficient"}, Title: "온프레미스 이전 검토", Reasons: []string{"반출 정책과 일치합니다", "대상 환경이 준비됐습니다"}, Missing: []string{}, Sources: []RecommendationSource{{Title: "공식 요금", URL: "https://aws.amazon.com/ec2/pricing/on-demand/", ObservedAt: &observed}}}}}
}

func TestRecommendationUsesExistingModelAndOnlySnapshotTool(t *testing.T) {
	f := &fakeAPI{replies: []reply{{tool: "recommendation_snapshot", toolInput: `{}`}, {text: `{"snapshotId":"rec_123","recommendations":[{"id":"candidate-1","action":"move","reasons":["반출 정책과 일치합니다"]}]}`}}}
	a, sink := newAgent(t, f)
	result, session, err := a.Recommend(context.Background(), recommendationInput())
	if err != nil || session == "" || len(result.Recommendations) != 1 || result.Model != "claude-opus-5-5" {
		t.Fatalf("recommendation failed: %+v session=%q err=%v", result, session, err)
	}
	if len(f.requests) != 2 || f.requests[0]["model"] != a.cfg.Model || f.requests[0]["max_tokens"] != float64(1500) {
		t.Fatalf("model configuration or request budget changed: %d", len(f.requests))
	}
	tools := f.requests[0]["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "recommendation_snapshot" {
		t.Fatal("recommendation has tools beyond the read-only snapshot")
	}
	if len(sink.Sessions) != 1 {
		t.Fatal("recommendation was not audited")
	}
}

func TestRecommendationRejectsInventedFactsAndBypassedToolWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		useTool      bool
	}{
		{"invented savings", `{"snapshotId":"rec_123","recommendations":[{"id":"candidate-1","action":"move","reasons":["월 99% 절감"]}]}`, true},
		{"wrong snapshot", `{"snapshotId":"other","recommendations":[]}`, true},
		{"wrong candidate", `{"snapshotId":"rec_123","recommendations":[{"id":"other","action":"move","reasons":[]}]}`, true},
		{"unknown field", `{"snapshotId":"rec_123","recommendations":[],"command":"migrate"}`, true},
		{"skipped tool", `{"snapshotId":"rec_123","recommendations":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := []reply{{text: tc.answer}}
			if tc.useTool {
				r = append([]reply{{tool: "recommendation_snapshot", toolInput: `{}`}}, r...)
			}
			f := &fakeAPI{replies: r}
			a, _ := newAgent(t, f)
			if _, _, err := a.Recommend(context.Background(), recommendationInput()); err == nil {
				t.Fatal("unverified answer accepted")
			}
			if len(f.requests) != len(r) {
				t.Fatal("invalid output retried beyond the recommendation budget")
			}
		})
	}
}

func TestRecommendationInvalidOrStaleInputDoesNotCallAI(t *testing.T) {
	for _, name := range []string{"expired", "future", "duplicate", "credential source", "invalid action", "large"} {
		t.Run(name, func(t *testing.T) {
			in := recommendationInput()
			switch name {
			case "expired":
				in.ExpiresAt = time.Now().Add(-time.Second)
			case "future":
				in.GeneratedAt = time.Now().Add(time.Hour)
			case "duplicate":
				in.Candidates = append(in.Candidates, in.Candidates[0])
			case "credential source":
				in.Candidates[0].Sources[0].URL = "https://user:secret@aws.amazon.com/"
			case "invalid action":
				in.Candidates[0].AllowedActions = []string{"run"}
			case "large":
				in.Candidates[0].Reasons[0] = strings.Repeat("x", 65537)
			}
			f := &fakeAPI{}
			a, _ := newAgent(t, f)
			if _, _, err := a.Recommend(context.Background(), in); err == nil {
				t.Fatal("invalid input accepted")
			}
			if len(f.requests) != 0 {
				t.Fatal("invalid snapshot spent a model call")
			}
		})
	}
}

func TestRecommendationDisablesSDKRetry(t *testing.T) {
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(429)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "rate_limit_error", "message": "test"}})
	}))
	defer srv.Close()
	a := New(Config{Options: []option.RequestOption{option.WithBaseURL(srv.URL), option.WithAPIKey("test-key")}})
	if _, _, err := a.Recommend(context.Background(), recommendationInput()); err == nil {
		t.Fatal("expected upstream refusal")
	}
	if count.Load() != 1 {
		t.Fatalf("bounded recommendation retried %d times", count.Load())
	}
}

func TestRecommendationSchemaUsesSupportedClaudeConstraints(t *testing.T) {
	var schema any
	if err := json.Unmarshal(recommendationSchema, &schema); err != nil {
		t.Fatal(err)
	}
	var check func(any)
	check = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for key, value := range x {
				if key == "maxItems" || key == "maxLength" || key == "minimum" || key == "maximum" {
					t.Errorf("unsupported Claude schema bound: %s", key)
				}
				check(value)
			}
		case []any:
			for _, value := range x {
				check(value)
			}
		}
	}
	check(schema)
}

func TestRecommendationFreezesCallerSnapshot(t *testing.T) {
	in := recommendationInput()
	f := &fakeAPI{replies: []reply{{tool: "recommendation_snapshot", toolInput: `{}`, textFromRequest: func(map[string]any) string {
		in.Candidates[0].Reasons[0] = "mutated after authorization"
		return ""
	}}, {text: `{"snapshotId":"rec_123","recommendations":[{"id":"candidate-1","action":"move","reasons":["반출 정책과 일치합니다"]}]}`}}}
	a, _ := newAgent(t, f)
	if _, _, err := a.Recommend(context.Background(), in); err != nil {
		t.Fatal("caller mutation replaced the fixed evidence snapshot")
	}
}
