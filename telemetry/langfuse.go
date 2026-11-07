// Package telemetry records hopai's existing execution path in Langfuse.
// It owns a dedicated provider; it never changes global OpenTelemetry state.
package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
	"github.com/anthropics/anthropic-sdk-go/option"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const captureLimit = 64 << 10

type Recorder struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	secrets  []string
}

// NewFromEnv is disabled unless configured. Errors contain no supplied values.
func NewFromEnv() (*Recorder, error) {
	base, public, secret := os.Getenv("LANGFUSE_BASE_URL"), os.Getenv("LANGFUSE_PUBLIC_KEY"), os.Getenv("LANGFUSE_SECRET_KEY")
	if base == "" && public == "" && secret == "" {
		return nil, nil
	}
	if base == "" || public == "" || secret == "" {
		return nil, errors.New("Langfuse configuration incomplete; tracing disabled")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Langfuse endpoint invalid; tracing disabled")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/public/otel/v1/traces"
	exporter, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(u.String()),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization":                "Basic " + base64.StdEncoding.EncodeToString([]byte(public+":"+secret)),
			"x-langfuse-ingestion-version": "4",
		}),
		otlptracehttp.WithTimeout(time.Second),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, errors.New("Langfuse exporter initialization failed; tracing disabled")
	}
	p := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "hopcharlie-hopai"))),
		sdktrace.WithBatcher(quietExporter{exporter}, sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(time.Second)),
	)
	return &Recorder{provider: p, tracer: p.Tracer("hopcharlie/llm"), secrets: []string{public, secret, os.Getenv("ANTHROPIC_API_KEY")}}, nil
}

// Never pass exporter transport/header diagnostics to a global error handler.
type quietExporter struct{ sdktrace.SpanExporter }

func (e quietExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if e.SpanExporter.ExportSpans(ctx, spans) != nil {
		return errors.New("Langfuse span export failed")
	}
	return nil
}
func (e quietExporter) Shutdown(ctx context.Context) error {
	if e.SpanExporter.Shutdown(ctx) != nil {
		return errors.New("Langfuse exporter shutdown failed")
	}
	return nil
}

func (r *Recorder) clean(s string) string {
	s = mask.String(s)
	for _, secret := range r.secrets {
		if len(secret) >= 8 {
			s = strings.ReplaceAll(s, secret, mask.Placeholder)
		}
	}
	return s
}

func (r *Recorder) knownValues(v any) any {
	switch x := v.(type) {
	case string:
		for _, secret := range r.secrets {
			if len(secret) >= 8 {
				x = strings.ReplaceAll(x, secret, mask.Placeholder)
			}
		}
		return x
	case []any:
		for i, item := range x {
			x[i] = r.knownValues(item)
		}
		return x
	case map[string]any:
		for key, item := range x {
			x[key] = r.knownValues(item)
		}
		return x
	default:
		return v
	}
}

func (r *Recorder) IO(span trace.Span, field string, value any) {
	if r == nil || span == nil || value == nil {
		return
	}
	b, err := mask.JSON(value)
	if err != nil || len(b) > captureLimit {
		span.SetAttributes(attribute.Bool("langfuse.observation.metadata."+field+"_truncated", true))
		return
	}
	var masked any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if decoder.Decode(&masked) != nil {
		return
	}
	b, err = json.Marshal(r.knownValues(masked))
	if err != nil || len(b) > captureLimit {
		span.SetAttributes(attribute.Bool("langfuse.observation.metadata."+field+"_truncated", true))
		return
	}
	span.SetAttributes(attribute.String("langfuse.observation."+field, string(b)))
}

func (r *Recorder) Agent(ctx context.Context, kind, title, model string, input any) (context.Context, trace.Span) {
	if r == nil {
		return ctx, nil
	}
	ctx, span := r.tracer.Start(ctx, "hopai."+kind, trace.WithAttributes(
		attribute.String("langfuse.observation.type", "agent"),
		attribute.String("langfuse.trace.name", r.clean(title)),
		attribute.String("langfuse.environment", "dev"),
		attribute.String("langfuse.observation.metadata.requested_model", model),
	))
	if id := os.Getenv("HOPCHARLIE_RUN_ID"); id != "" {
		span.SetAttributes(attribute.String("langfuse.session.id", id), attribute.String("langfuse.observation.metadata.job_id", id))
	}
	r.correlate(span)
	r.IO(span, "input", input)
	return ctx, span
}

func (r *Recorder) Session(span trace.Span, id string) {
	if r != nil && span != nil {
		span.SetAttributes(attribute.String("langfuse.observation.metadata.platform_session_id", id))
	}
}

func (r *Recorder) correlate(span trace.Span) {
	if r != nil && span != nil {
		if id := os.Getenv("HOPCHARLIE_RUN_ID"); id != "" {
			span.SetAttributes(attribute.String("langfuse.session.id", id), attribute.String("langfuse.observation.metadata.job_id", id))
		}
	}
}

func (r *Recorder) Tool(ctx context.Context, name string, input any) (context.Context, trace.Span) {
	if r == nil {
		return ctx, nil
	}
	ctx, span := r.tracer.Start(ctx, name, trace.WithAttributes(attribute.String("langfuse.observation.type", "tool")))
	r.correlate(span)
	r.IO(span, "input", input)
	return ctx, span
}

func (r *Recorder) Result(span trace.Span, output any, err error) {
	if r == nil || span == nil {
		return
	}
	if err != nil {
		span.SetStatus(codes.Error, "operation failed")
		span.SetAttributes(attribute.String("langfuse.observation.level", "ERROR"),
			attribute.String("langfuse.observation.status_message", r.clean(err.Error())))
		return
	}
	r.IO(span, "output", output)
	span.SetStatus(codes.Ok, "")
}

func (r *Recorder) End(span trace.Span) {
	if span != nil {
		span.End()
	}
}

// Called inside llm.run, before callers can os.Exit. Export never changes its result.
func (r *Recorder) Finish(ctx context.Context, span trace.Span) {
	if r == nil || span == nil {
		return
	}
	span.End()
	flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = r.provider.ForceFlush(flush)
}

// Close is useful for long-lived embedders and tests. CLI roots flush independently.
func (r *Recorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.provider.Shutdown(ctx)
}

// capturedBody observes only reads the HTTP client/SDK already makes. It
// delegates Close and all read results; it never drains, replays or retries.
type capturedBody struct {
	io.ReadCloser
	mu        sync.Mutex
	data      []byte
	truncated bool
	done      func(bool, error)
	once      sync.Once
}

func (b *capturedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.mu.Lock()
		room := captureLimit - len(b.data)
		take := n
		if take > room {
			take = room
			b.truncated = true
		}
		b.data = append(b.data, p[:take]...)
		b.mu.Unlock()
	}
	if err != nil {
		b.finish(err == io.EOF, err)
	}
	return n, err
}
func (b *capturedBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish(false, err)
	return err
}
func (b *capturedBody) finish(complete bool, err error) {
	if b.done != nil {
		b.once.Do(func() { b.done(complete, err) })
	}
}
func (b *capturedBody) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...), b.truncated
}
func decode(b *capturedBody) (map[string]any, bool) {
	if b == nil {
		return nil, false
	}
	data, truncated := b.snapshot()
	if truncated {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return nil, false
	}
	return obj, true
}

func (r *Recorder) Middleware() option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if r == nil || !strings.HasSuffix(strings.TrimRight(req.URL.Path, "/"), "/messages") {
			return next(req)
		}
		ctx, span := r.tracer.Start(req.Context(), "anthropic.messages", trace.WithAttributes(
			attribute.String("langfuse.observation.type", "generation"),
			attribute.String("gen_ai.system", "anthropic"),
		))
		r.correlate(span)
		req = req.Clone(ctx)
		var input *capturedBody
		if req.Body != nil {
			input = &capturedBody{ReadCloser: req.Body}
			req.Body = input
		}
		response, err := next(req)
		finish := func(output *capturedBody, complete bool, readErr error) {
			if in, ok := decode(input); ok {
				r.IO(span, "input", map[string]any{"system": in["system"], "messages": in["messages"]})
				if model, ok := in["model"].(string); ok {
					span.SetAttributes(attribute.String("gen_ai.request.model", model), attribute.String("langfuse.observation.model.name", model))
				}
			} else {
				span.SetAttributes(attribute.Bool("langfuse.observation.metadata.input_truncated", true))
			}
			if response != nil {
				span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
			}
			if err != nil || response == nil || response.StatusCode >= 400 || (readErr != nil && readErr != io.EOF) {
				span.SetStatus(codes.Error, "Anthropic HTTP request failed")
				span.SetAttributes(attribute.String("langfuse.observation.level", "ERROR"))
			} else if out, ok := decode(output); ok && complete {
				r.IO(span, "output", out["content"])
				if model, ok := out["model"].(string); ok {
					span.SetAttributes(attribute.String("gen_ai.response.model", model), attribute.String("langfuse.observation.model.name", model))
				}
				if usage, ok := out["usage"].(map[string]any); ok {
					details := map[string]any{}
					for _, key := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
						if value, ok := usage[key].(float64); ok {
							name := key
							if key == "input_tokens" {
								name = "input"
								span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", int64(value)))
							}
							if key == "output_tokens" {
								name = "output"
								span.SetAttributes(attribute.Int64("gen_ai.usage.output_tokens", int64(value)))
							}
							details[name] = int64(value)
						}
					}
					if b, e := json.Marshal(details); e == nil {
						span.SetAttributes(attribute.String("langfuse.observation.usage_details", string(b)))
					}
				}
				span.SetStatus(codes.Ok, "")
			} else {
				span.SetAttributes(attribute.Bool("langfuse.observation.metadata.output_truncated", true))
			}
			span.End()
		}
		if err != nil || response == nil || response.Body == nil {
			finish(nil, false, err)
			return response, err
		}
		output := &capturedBody{ReadCloser: response.Body}
		output.done = func(complete bool, readErr error) { finish(output, complete, readErr) }
		response.Body = output
		return response, err
	}
}
