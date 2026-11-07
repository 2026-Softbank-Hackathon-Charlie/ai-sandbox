package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestInstrumentedAgentPreservesToolsAndSurvivesExporterFailure(t *testing.T) {
	for _, collectorStatus := range []int{200, 503} {
		t.Run(http.StatusText(collectorStatus), func(t *testing.T) {
			var mu sync.Mutex
			var spans []*tracepb.Span
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/public/otel/v1/traces" || r.Header.Get("x-langfuse-ingestion-version") != "4" {
					t.Error("wrong OTLP endpoint/version")
				}
				if user, pass, ok := r.BasicAuth(); !ok || user != "pk-lf-test" || pass != "sk-lf-test-secret-12345" {
					t.Error("wrong project auth")
				}
				body, _ := io.ReadAll(r.Body)
				var request collectortrace.ExportTraceServiceRequest
				if proto.Unmarshal(body, &request) != nil {
					t.Error("invalid OTLP protobuf")
				}
				mu.Lock()
				for _, resource := range request.ResourceSpans {
					for _, scope := range resource.ScopeSpans {
						spans = append(spans, scope.Spans...)
					}
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.WriteHeader(collectorStatus)
			}))
			defer collector.Close()
			t.Setenv("LANGFUSE_BASE_URL", collector.URL)
			t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-lf-test")
			t.Setenv("LANGFUSE_SECRET_KEY", "sk-lf-test-secret-12345")
			t.Setenv("HOPCHARLIE_RUN_ID", "test-hopai-job")
			api := &fakeAPI{replies: []reply{{tool: "read_diagnostics", toolInput: "{}"}, {text: "{\"ok\":true}"}}}
			agent, sink := newAgent(t, api)
			called := 0
			schema := []byte("{\"type\":\"object\",\"properties\":{\"ok\":{\"type\":\"boolean\"}},\"required\":[\"ok\"],\"additionalProperties\":false}")
			result, _, err := run(context.Background(), agent, task[bool]{
				session: agentlog.Start{Kind: "plan", Title: "unit check"},
				system:  "Read a diagnostic and return JSON.", user: "synthetic",
				tools: []Tool{{Name: "read_diagnostics", Properties: map[string]any{}, Run: func(context.Context, json.RawMessage) (any, error) {
					called++
					return map[string]any{"marker": "ok"}, nil
				}}},
				schema: schema,
				parse: func(raw []byte) (bool, plan.Issues) {
					var out map[string]bool
					if json.Unmarshal(raw, &out) != nil || !out["ok"] {
						return false, plan.Issues{{Path: "ok", Message: "required"}}
					}
					return true, nil
				},
				summary: func(ok bool) (string, any) { return "done", map[string]bool{"ok": ok} },
			})
			if err != nil || !result || called != 1 {
				t.Fatalf("observer changed workflow: %v", err)
			}
			if len(api.requests) != 2 || len(sink.Sessions) != 1 || !sink.Sessions[0].OK {
				t.Fatal("existing SDK/session behavior changed")
			}
			mu.Lock()
			defer mu.Unlock()
			counts := map[string]int{}
			var root *tracepb.Span
			for _, span := range spans {
				for _, a := range span.Attributes {
					if a.Key == "langfuse.observation.type" {
						counts[a.Value.GetStringValue()]++
					}
				}
				if span.Name == "hopai.plan" {
					root = span
				}
			}
			if root == nil || counts["agent"] != 1 || counts["generation"] != 2 || counts["tool"] != 1 {
				t.Fatalf("missing real execution spans: %v", counts)
			}
			for _, span := range spans {
				if string(span.TraceId) != string(root.TraceId) {
					t.Fatal("trace split")
				}
				if span != root && string(span.ParentSpanId) != string(root.SpanId) {
					t.Fatal("incorrect parent")
				}
				for _, a := range span.Attributes {
					if strings.Contains(a.Value.GetStringValue(), "sk-lf-test-secret") {
						t.Fatal("credential leaked")
					}
				}
			}
		})
	}
}
