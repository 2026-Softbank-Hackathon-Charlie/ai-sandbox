package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
)

type recommendationAgentFunc func(context.Context, llm.RecommendationInput) (llm.RecommendationDecision, string, error)

func (f recommendationAgentFunc) Recommend(c context.Context, in llm.RecommendationInput) (llm.RecommendationDecision, string, error) {
	return f(c, in)
}

func recommendationEnvironment(t *testing.T, server string) map[string]string {
	t.Helper()
	raw, _ := json.Marshal(llm.RecommendationInput{SnapshotID: "snap_123", GeneratedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(5 * time.Minute), Candidates: []llm.RecommendationCandidate{}})
	return map[string]string{"HOP_RECOMMENDATION_ID": "rec_123", "HOP_RECOMMENDATION_INPUT": string(raw), "HOP_RECOMMENDATION_NONCE": strings.Repeat("n", 48), "PLATFORM_URL": server, "PLATFORM_TOKEN": "private-platform-token", "ANTHROPIC_API_KEY": "private-provider-key"}
}

func TestRecommendationCommandCallsOnlyBoundResultWithExistingCredentials(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/migration-recommendations/rec_123/result" || r.Header.Get("Authorization") != "Bearer private-platform-token" {
			t.Error("unexpected request boundary")
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	env := recommendationEnvironment(t, srv.URL)
	factoryCalls := 0
	factory := func(base, token string) recommendationAgent {
		factoryCalls++
		if base != srv.URL || token != env["PLATFORM_TOKEN"] {
			t.Fatal("existing platform connection changed")
		}
		return recommendationAgentFunc(func(ctx context.Context, in llm.RecommendationInput) (llm.RecommendationDecision, string, error) {
			if in.SnapshotID != "snap_123" {
				t.Fatal("snapshot binding changed")
			}
			return llm.RecommendationDecision{SnapshotID: in.SnapshotID, Recommendations: []llm.Recommendation{}, Model: "claude-sonnet-5-5"}, "ags_123", nil
		})
	}
	var output bytes.Buffer
	if code := runRecommendationCommand(context.Background(), func(k string) string { return env[k] }, factory, &output); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if factoryCalls != 1 || payload["status"] != "ready" || payload["nonce"] != env["HOP_RECOMMENDATION_NONCE"] || payload["snapshotId"] != "snap_123" || payload["model"] != "claude-sonnet-5-5" {
		t.Fatalf("callback not bound: %v", payload)
	}
	for _, private := range []string{env["HOP_RECOMMENDATION_NONCE"], env["PLATFORM_TOKEN"], env["ANTHROPIC_API_KEY"], env["HOP_RECOMMENDATION_INPUT"]} {
		if strings.Contains(output.String(), private) {
			t.Fatal("private runtime data printed")
		}
	}
}

func TestRecommendationCommandReportsOnlySanitizedFailure(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "AI failure", true: "invalid input"}[invalid], func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewDecoder(r.Body).Decode(&body) }))
			defer srv.Close()
			env := recommendationEnvironment(t, srv.URL)
			if invalid {
				env["HOP_RECOMMENDATION_INPUT"] += `{}`
			}
			calls := 0
			factory := func(string, string) recommendationAgent {
				calls++
				return recommendationAgentFunc(func(context.Context, llm.RecommendationInput) (llm.RecommendationDecision, string, error) {
					return llm.RecommendationDecision{}, "", errors.New("raw-private-provider-key private-platform-token")
				})
			}
			var output bytes.Buffer
			if code := runRecommendationCommand(context.Background(), func(k string) string { return env[k] }, factory, &output); code == 0 {
				t.Fatal("failure reported success")
			}
			if body["status"] != "unavailable" || body["nonce"] != env["HOP_RECOMMENDATION_NONCE"] || body["snapshotId"] != "snap_123" {
				t.Fatal("failure callback missing")
			}
			want := "ai_unavailable"
			if invalid {
				want = "invalid_input"
				if calls != 0 {
					t.Fatal("invalid snapshot spent AI call")
				}
			}
			if body["errorCode"] != want {
				t.Fatalf("error code: %v", body)
			}
			raw, _ := json.Marshal(body)
			if strings.Contains(string(raw), "raw-private") || strings.Contains(output.String(), "private") {
				t.Fatal("upstream error leaked")
			}
		})
	}
}

func TestRecommendationCommandNeverFollowsCallbackRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	env := recommendationEnvironment(t, srv.URL)
	factory := func(string, string) recommendationAgent {
		return recommendationAgentFunc(func(context.Context, llm.RecommendationInput) (llm.RecommendationDecision, string, error) {
			return llm.RecommendationDecision{SnapshotID: "snap_123", Recommendations: []llm.Recommendation{}}, "", nil
		})
	}
	if runRecommendationCommand(context.Background(), func(k string) string { return env[k] }, factory, &bytes.Buffer{}) == 0 || redirected {
		t.Fatal("credential-bearing callback followed redirect")
	}
}

func TestRecommendationCommandInvalidDestinationOrCapabilityDoesNothing(t *testing.T) {
	for _, mode := range []string{"public http", "userinfo", "query", "path", "nonce", "id"} {
		t.Run(mode, func(t *testing.T) {
			env := recommendationEnvironment(t, "https://api.hopcharlie.dev")
			switch mode {
			case "public http":
				env["PLATFORM_URL"] = "http://outside.example"
			case "userinfo":
				env["PLATFORM_URL"] = "https://secret@api.hopcharlie.dev"
			case "query":
				env["PLATFORM_URL"] += "?key=secret"
			case "path":
				env["PLATFORM_URL"] += "/path"
			case "nonce":
				env["HOP_RECOMMENDATION_NONCE"] = "short"
			case "id":
				env["HOP_RECOMMENDATION_ID"] = "../outside"
			}
			factory := func(string, string) recommendationAgent {
				t.Fatal("invalid callback configuration started AI")
				return nil
			}
			if runRecommendationCommand(context.Background(), func(k string) string { return env[k] }, factory, &bytes.Buffer{}) != 2 {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}
