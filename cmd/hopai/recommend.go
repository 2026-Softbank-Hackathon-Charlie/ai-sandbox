package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

type recommendationAgent interface {
	Recommend(context.Context, llm.RecommendationInput) (llm.RecommendationDecision, string, error)
}

type recommendationCallback struct {
	Nonce           string               `json:"nonce"`
	SnapshotID      string               `json:"snapshotId"`
	Status          string               `json:"status"`
	Model           string               `json:"model,omitempty"`
	SessionID       string               `json:"sessionId,omitempty"`
	Recommendations []llm.Recommendation `json:"recommendations"`
	ErrorCode       string               `json:"errorCode,omitempty"`
}

var recommendationJobID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$`)
var recommendationNonce = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)

func recommendMain(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "사용법: hopai recommend (운영자가 준비한 추천 Job에서 실행)")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	return runRecommendationCommand(ctx, os.Getenv, func(base, token string) recommendationAgent {
		// Same provider, default model, effort and telemetry as hopai run. The
		// recommendation task narrows budgets and exposes only its snapshot tool.
		return llm.New(llm.Config{Sink: agentlog.Platform{C: client.New(base, token)}})
	}, os.Stderr)
}

// runRecommendationCommand never loads kubeconfigs or calls plan/rehearsal APIs.
// The nonce is exclusively a callback capability, never model or session input.
func runRecommendationCommand(ctx context.Context, getenv func(string) string, newAgent func(string, string) recommendationAgent, output io.Writer) int {
	base, err := recommendationPlatformURL(getenv("PLATFORM_URL"))
	id, nonce, token := getenv("HOP_RECOMMENDATION_ID"), getenv("HOP_RECOMMENDATION_NONCE"), getenv("PLATFORM_TOKEN")
	if err != nil || !recommendationJobID.MatchString(id) || !recommendationNonce.MatchString(nonce) || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") || getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Fprintln(output, "추천 실행 설정을 확인하지 못했습니다.")
		return 2
	}
	raw := []byte(getenv("HOP_RECOMMENDATION_INPUT"))
	in, inputErr := llm.ParseRecommendationInput(raw, time.Now().UTC())
	callback := recommendationCallback{Nonce: nonce, Status: "unavailable", ErrorCode: "invalid_input", Recommendations: []llm.Recommendation{}}
	// Invalid snapshots may still carry a safe identifier for a bounded failure
	// callback. The platform also checks the capability's exact stored snapshot.
	if inputErr != nil {
		var minimal struct {
			SnapshotID string `json:"snapshotId"`
		}
		// Only recover an opaque ID for an authenticated failure callback. A
		// trailing object still makes the complete input invalid and spends no AI.
		if len(raw) <= llm.RecommendationInputLimit && json.NewDecoder(bytes.NewReader(raw)).Decode(&minimal) == nil && recommendationJobID.MatchString(minimal.SnapshotID) {
			callback.SnapshotID = minimal.SnapshotID
		}
	} else {
		callback.SnapshotID = in.SnapshotID
		decision, session, aiErr := newAgent(base, token).Recommend(ctx, in)
		if aiErr == nil {
			callback.Status, callback.ErrorCode = "ready", ""
			callback.Model, callback.SessionID, callback.Recommendations = decision.Model, session, decision.Recommendations
		} else {
			callback.ErrorCode = "ai_unavailable"
		}
	}
	if deliverRecommendation(ctx, base, token, id, callback) != nil {
		fmt.Fprintln(output, "추천 결과를 플랫폼에 전달하지 못했습니다.")
		return 1
	}
	if callback.Status != "ready" {
		fmt.Fprintln(output, "확인 가능한 AI 추천을 만들지 못했습니다.")
		return 1
	}
	fmt.Fprintln(output, "환경 이전 추천을 플랫폼에 전달했습니다.")
	return 0
}

func recommendationPlatformURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("invalid platform destination")
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		internal := host == "hopcharlie-api" || host == "localhost" || ip != nil && ip.IsLoopback() || strings.HasSuffix(host, ".svc.cluster.local")
		if u.Scheme != "http" || !internal {
			return "", fmt.Errorf("insecure platform destination")
		}
	}
	u.Path = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func deliverRecommendation(ctx context.Context, base, token, id string, callback recommendationCallback) error {
	raw, err := json.Marshal(callback)
	if err != nil || len(raw) > 32<<10 {
		return fmt.Errorf("invalid callback")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/migration-recommendations/"+url.PathEscape(id)+"/result", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("invalid callback")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	httpClient := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("callback unavailable")
	}
	defer response.Body.Close()
	// No upstream error body or credential-bearing URL is surfaced in logs.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("callback rejected")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return nil
}
