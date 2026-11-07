package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func recorder(t *testing.T) (*Recorder, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	r := &Recorder{provider: provider, tracer: provider.Tracer("test"), secrets: []string{"sk-lf-private-canary-123456789"}}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, exporter
}
func attrs(span tracetest.SpanStub) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	for _, a := range span.Attributes {
		out[string(a.Key)] = a.Value
	}
	return out
}

func TestMiddlewarePreservesSDKBodiesAndCorrelatesSpans(t *testing.T) {
	t.Setenv("HOPCHARLIE_RUN_ID", "hopai-check-123")
	r, exporter := recorder(t)
	ctx, root := r.Agent(context.Background(), "plan", "check", "requested-model", map[string]any{"input_tokens": 7})
	input := string(mustJSON(t, map[string]any{"model": "requested-model", "messages": []any{map[string]any{"role": "user", "content": "sk-lf-private-canary-123456789"}}}))
	output := string(mustJSON(t, map[string]any{"model": "actual-model", "content": []any{map[string]any{"type": "text", "text": "done"}}, "usage": map[string]any{"input_tokens": 12, "output_tokens": 8}}))
	req := httptest.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(input)).WithContext(ctx)
	res, err := r.Middleware()(req, func(req *http.Request) (*http.Response, error) {
		if trace.SpanFromContext(req.Context()).SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Fatal("generation has another trace")
		}
		raw, err := io.ReadAll(req.Body)
		if err != nil || string(raw) != input {
			t.Fatal("request body changed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(output))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil || string(raw) != output {
		t.Fatal("response body changed")
	}
	_ = res.Body.Close()
	_, tool := r.Tool(ctx, "read_diagnostics", map[string]any{"input_tokens": 7})
	r.Result(tool, map[string]any{"marker": "ok"}, nil)
	r.End(tool)
	r.Result(root, map[string]any{"ok": true}, nil)
	r.Finish(ctx, root)
	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d spans", len(spans))
	}
	for _, s := range spans {
		if s.SpanContext.TraceID() != root.SpanContext().TraceID() {
			t.Fatal("trace mismatch")
		}
		a := attrs(s)
		if a["langfuse.session.id"].AsString() != "hopai-check-123" {
			t.Fatal("missing Job correlation")
		}
		if s.Name != "hopai.plan" && s.Parent.SpanID() != root.SpanContext().SpanID() {
			t.Fatal("parent mismatch")
		}
		switch s.Name {
		case "anthropic.messages":
			if a["gen_ai.response.model"].AsString() != "actual-model" || a["gen_ai.usage.input_tokens"].AsInt64() != 12 || a["gen_ai.usage.output_tokens"].AsInt64() != 8 {
				t.Fatal("model/usage mismatch")
			}
			body := a["langfuse.observation.input"].AsString()
			if strings.Contains(body, "sk-lf-private-canary") || !json.Valid([]byte(body)) {
				t.Fatal("credential leak or invalid IO")
			}
		case "hopai.plan":
			var in map[string]any
			if json.Unmarshal([]byte(a["langfuse.observation.input"].AsString()), &in) != nil || in["input_tokens"] != float64(7) {
				t.Fatal("numeric fields changed by masking")
			}
		}
	}
}

func TestOversizedBodiesRemainUnchangedAndPartialJSONIsOmitted(t *testing.T) {
	r, exporter := recorder(t)
	body := string(mustJSON(t, map[string]any{"messages": strings.Repeat("x", captureLimit+200)}))
	res, err := r.Middleware()(httptest.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(body)), func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		if string(raw) != body {
			t.Fatal("large request changed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(raw) != body {
		t.Fatal("large response changed")
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatal("missing span")
	}
	a := attrs(spans[0])
	if !a["langfuse.observation.metadata.input_truncated"].AsBool() || !a["langfuse.observation.metadata.output_truncated"].AsBool() {
		t.Fatal("truncation missing")
	}
	if _, ok := a["langfuse.observation.input"]; ok {
		t.Fatal("partial input exported")
	}
	if _, ok := a["langfuse.observation.output"]; ok {
		t.Fatal("partial output exported")
	}
}

func TestMiddlewareDoesNotRetryOrReplaceTransportErrors(t *testing.T) {
	r, exporter := recorder(t)
	want := errors.New("transport canary")
	calls := 0
	next := option.MiddlewareNext(func(*http.Request) (*http.Response, error) { calls++; return nil, want })
	_, err := r.Middleware()(httptest.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}")), next)
	if err != want || calls != 1 {
		t.Fatal("observer changed the request failure")
	}
	if len(exporter.GetSpans()) != 1 {
		t.Fatal("failed request not recorded")
	}
}

func TestDisabledAndPartialConfiguration(t *testing.T) {
	for _, key := range []string{"LANGFUSE_BASE_URL", "LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"} {
		t.Setenv(key, "")
	}
	r, err := NewFromEnv()
	if err != nil || r != nil {
		t.Fatal("unset tracing should be disabled")
	}
	t.Setenv("LANGFUSE_SECRET_KEY", "never-print-this-secret")
	_, err = NewFromEnv()
	if err == nil || strings.Contains(err.Error(), "never-print") {
		t.Fatal("partial config leaked or was accepted")
	}
	var disabled *Recorder
	ctx, span := disabled.Agent(context.Background(), "plan", "x", "model", nil)
	disabled.Result(span, nil, nil)
	disabled.Finish(ctx, span)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
