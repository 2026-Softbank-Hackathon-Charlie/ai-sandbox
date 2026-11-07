package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TelemetryCheck runs the same agent, SDK and tool wrappers as production.
// It has no platform, cluster, file-writing or repository tools.
func TelemetryCheck(ctx context.Context) (map[string]any, error) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "LANGFUSE_BASE_URL", "LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY", "HOPCHARLIE_RUN_ID"} {
		if os.Getenv(name) == "" {
			return nil, errors.New("telemetry-check requires the configured model, Langfuse and run identity")
		}
	}
	var calls, tools atomic.Int64
	limit := option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if calls.Load() >= 4 {
			return nil, errors.New("telemetry-check model HTTP budget exhausted")
		}
		calls.Add(1)
		return next(req)
	})
	agent := New(Config{MaxTokens: 1024, MaxRetries: 1, MaxIterations: 2, CallTimeout: 30 * time.Second,
		Sink: &agentlog.Memory{}, Options: []option.RequestOption{option.WithMaxRetries(0), limit}})
	if agent.obs == nil {
		return nil, errors.New("telemetry-check tracing is not configured")
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false,
		"properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"ok"}}, "marker": map[string]any{"type": "string", "enum": []string{"HOPAI_LANGFUSE_LIVE_OK"}}},
		"required":   []string{"status", "marker"}})
	result, session, err := run(ctx, agent, task[map[string]any]{
		session: agentlog.Start{Kind: "telemetry-check", Title: "HopCharlie live instrumentation check"},
		system:  "You are a read-only diagnostic agent. Call read_diagnostics once to get the marker, then return the required JSON. Do not invent the marker and do not perform other operations.",
		user:    "Check the synthetic observation pipeline. Read the diagnostic marker using the provided tool and return status ok and that marker.",
		tools: []Tool{{Name: "read_diagnostics", Description: "Reads an in-memory synthetic marker. No external effects.", Properties: map[string]any{}, Run: func(_ context.Context, _ json.RawMessage) (any, error) {
			tools.Add(1)
			return map[string]any{"synthetic": true, "marker": "HOPAI_LANGFUSE_LIVE_OK"}, nil
		}}},
		schema: schema,
		parse: func(raw []byte) (map[string]any, plan.Issues) {
			var out map[string]any
			if json.Unmarshal(raw, &out) != nil || out["status"] != "ok" || out["marker"] != "HOPAI_LANGFUSE_LIVE_OK" {
				return nil, plan.Issues{{Path: "result", Message: "Return the diagnostic status and marker from read_diagnostics"}}
			}
			return out, nil
		},
		summary: func(out map[string]any) (string, any) { return "read-only diagnostic completed", out },
	})
	if err != nil {
		return nil, err
	}
	result["session"] = session
	result["model_http_calls"] = calls.Load()
	result["tool_calls"] = tools.Load()
	return result, nil
}
