package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
)

// reply는 가짜 API의 응답 하나다. tool이 있으면 tool_use 블록을 돌려준다.
type reply struct {
	text       string
	tool       string
	toolInput  string
	stopReason string
	category   string
}

// fakeAPI는 Messages API 흉내를 낸다. 정해 둔 응답을 차례로 돌려주고, 받은 요청을 기록한다.
type fakeAPI struct {
	mu       sync.Mutex
	replies  []reply
	requests []map[string]any
	headers  []http.Header
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(b, &req)
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())
	rp := f.replies[len(f.requests)-1]
	var content []any
	stop := rp.stopReason
	if rp.tool != "" {
		content = append(content, map[string]any{"type": "tool_use", "id": "toolu_" + rp.tool, "name": rp.tool, "input": json.RawMessage(rp.toolInput)})
		if stop == "" {
			stop = "tool_use"
		}
	} else {
		content = append(content, map[string]any{"type": "text", "text": rp.text})
	}
	if stop == "" {
		stop = "end_turn"
	}
	var details any
	if stop == "refusal" {
		details = map[string]any{"type": "refusal", "category": rp.category}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
		"content": content, "stop_reason": stop, "stop_details": details,
		"usage": map[string]any{"input_tokens": 1200, "output_tokens": 300},
	})
}

func newAgent(t *testing.T, f *fakeAPI) (*Agent, *agentlog.Memory) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	sink := &agentlog.Memory{}
	return New(Config{
		Sink:    sink,
		Options: []option.RequestOption{option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0)},
	}), sink
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func draft(t *testing.T) *eplan.Plan {
	t.Helper()
	var p eplan.Plan
	if err := json.Unmarshal([]byte(readFile(t, "../plan/testdata/draft.json")), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// clusters는 원본(gke)과 대상(eks)의 가짜 클러스터다. 대상에는 StorageClass gp3가 있다.
func clusters() map[string]Cluster {
	cnpg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "moimroom-db-gke", "namespace": "moimroom"},
		"spec":     map[string]any{"instances": int64(2), "storage": map[string]any{"size": "5Gi", "storageClass": "standard-rwo"}, "superuserSecret": map[string]any{"name": "x"}},
		"status":   map[string]any{"phase": "Cluster in healthy state", "readyInstances": int64(2)},
	}}
	dyn := func(objs ...runtime.Object) *dynfake.FakeDynamicClient {
		return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{CNPGClusterGVR: "ClusterList"}, objs...)
	}
	return map[string]Cluster{
		"gke": {Typed: fake.NewClientset(), Dynamic: dyn(cnpg)},
		"eks": {Typed: fake.NewClientset(&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp3", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: "ebs.csi.aws.com"}), Dynamic: dyn()},
	}
}

func scope() PlanScope {
	return PlanScope{App: "moimroom", Namespace: "moimroom", Envs: []string{"gke", "eks"}}
}

func TestPlanUsesToolsAndLogsEverything(t *testing.T) {
	f := &fakeAPI{replies: []reply{
		{tool: "get_cnpg_cluster", toolInput: `{"env":"gke","name":"moimroom-db-gke"}`},
		{tool: "list_storage_classes", toolInput: `{"env":"eks"}`},
		{text: readFile(t, "../plan/testdata/proposal.json")},
	}}
	a, sink := newAgent(t, f)
	p, sid, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t), Tools: PlanTools(scope(), nil, clusters())})
	if err != nil {
		t.Fatal(err)
	}
	if p.Plan.Target.Manifest != plan.ManifestRef || sid != "mem_1" {
		t.Fatalf("결과: %+v %s", p.Plan.Target, sid)
	}

	// 요청 형태: 도구 3개, 구조화 출력, 대체 모델, 강제 도구 선택 없음
	req := f.requests[0]
	if req["model"] != DefaultModel {
		t.Errorf("모델: %v", req["model"])
	}
	if tools, _ := req["tools"].([]any); len(tools) != 3 {
		t.Errorf("도구 3개가 넘어가야 함: %v", len(tools))
	}
	oc, _ := req["output_config"].(map[string]any)
	if f, _ := oc["format"].(map[string]any); f["type"] != "json_schema" {
		t.Errorf("JSON 스키마가 안 넘어감: %v", oc)
	}
	if req["fallbacks"] != "default" || !strings.Contains(f.headers[0].Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("대체 모델 설정: %v", req["fallbacks"])
	}
	if _, ok := req["tool_choice"]; ok {
		t.Error("claude-opus-5-5에 강제 도구 선택을 보내면 400")
	}
	// 도구 결과가 두 번째 요청에 실려 감
	second, _ := json.Marshal(f.requests[1]["messages"])
	if !strings.Contains(string(second), "tool_result") || !strings.Contains(string(second), "standard-rwo") {
		t.Fatalf("도구 결과가 AI에 돌아가야 함: %s", second)
	}
	if strings.Contains(string(second), "superuserSecret") {
		t.Fatal("CNPG 설정 중 허용하지 않은 키가 AI에 넘어감")
	}

	// 세션 기록: 도구 호출·결과, AI 응답(토큰), 판단
	s := sink.Sessions[0]
	if s.Info.Kind != agentlog.KindPlan || !s.Done || !s.OK {
		t.Fatalf("세션: %+v", s)
	}
	var types []string
	var tokens int64
	for _, e := range s.Events {
		types = append(types, e.Type+":"+e.Tool)
		tokens += e.InputTokens
	}
	want := "message: tool_call:get_cnpg_cluster tool_result:get_cnpg_cluster message: tool_call:list_storage_classes tool_result:list_storage_classes message: decision:"
	if strings.Join(types, " ") != want {
		t.Fatalf("기록 순서:\n got %s\nwant %s", strings.Join(types, " "), want)
	}
	if tokens != 3600 {
		t.Fatalf("토큰 합계: %d", tokens)
	}
}

func TestToolOutsideScopeIsBlocked(t *testing.T) {
	f := &fakeAPI{replies: []reply{
		{tool: "list_storage_classes", toolInput: `{"env":"prod-other"}`},
		{text: readFile(t, "../plan/testdata/proposal.json")},
	}}
	a, sink := newAgent(t, f)
	if _, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t), Tools: PlanTools(scope(), nil, clusters())}); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	for _, e := range sink.Sessions[0].Events {
		if e.Type == agentlog.Blocked && e.Tool == "list_storage_classes" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("범위 밖 환경은 blocked로 남아야 함: %+v", sink.Sessions[0].Events)
	}
	second, _ := json.Marshal(f.requests[1]["messages"])
	if !strings.Contains(string(second), "is_error") {
		t.Fatalf("막힌 결과는 오류로 AI에 돌아가야 함: %s", second)
	}
}

func TestPlanRetrySendsIssues(t *testing.T) {
	v := readFile(t, "../plan/testdata/proposal.json")
	bad := strings.Replace(v, "moimroom-db-eks-rw.moimroom.eks.ts.net:5432", "evil.example.com:5432", 1)
	f := &fakeAPI{replies: []reply{{text: bad}, {text: v}}}
	a, sink := newAgent(t, f)
	if _, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t)}); err != nil {
		t.Fatal(err)
	}
	msgs := f.requests[1]["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("재요청은 사용자, 직전 응답, 걸린 항목 순서여야 함: %d개", len(msgs))
	}
	last, _ := json.Marshal(msgs[2])
	if !strings.Contains(string(last), "plan.target.dsn") {
		t.Fatalf("걸린 항목이 전달돼야 함: %s", last)
	}
	var checkErrors int
	for _, e := range sink.Sessions[0].Events {
		if e.Type == agentlog.Error && e.Status == "check" {
			checkErrors++
		}
	}
	if checkErrors != 1 {
		t.Fatalf("검사 실패도 기록에 남아야 함: %d", checkErrors)
	}
}

func TestPlanGivesUp(t *testing.T) {
	f := &fakeAPI{replies: []reply{{text: "{}"}, {text: "{}"}, {text: "{}"}}}
	a, sink := newAgent(t, f)
	_, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t)})
	if !plan.IsRetryExhausted(err) || len(f.requests) != 3 {
		t.Fatalf("세 번 묻고 포기해야 함: %v, 요청 %d번", err, len(f.requests))
	}
	if s := sink.Sessions[0]; !s.Done || s.OK {
		t.Fatalf("세션은 실패로 닫혀야 함: %+v", s)
	}
}

func TestRefusalAndTruncationStop(t *testing.T) {
	for name, c := range map[string]struct {
		r    reply
		want error
	}{
		"거절": {reply{stopReason: "refusal", category: "cyber"}, ErrRefused},
		"잘림": {reply{text: `{"plan":`, stopReason: "max_tokens"}, ErrTruncated},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeAPI{replies: []reply{c.r}}
			a, _ := newAgent(t, f)
			_, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t)})
			if !errors.Is(err, c.want) || len(f.requests) != 1 {
				t.Fatalf("바로 멈춰야 함: %v, 요청 %d번", err, len(f.requests))
			}
		})
	}
}

func TestDiagnoseMasksAndValidates(t *testing.T) {
	p, is := plan.Parse([]byte(readFile(t, "../plan/testdata/proposal.json")), draft(t))
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	good := `{"cause":"StorageClass gp3가 대상에 없음","evidence":["storageclass.storage.k8s.io \"gp3\" not found"],"fix":{"kind":"manifest-patch","patch":[{"op":"replace","path":"/spec/storage/storageClass","value":"standard"}]}}`
	outOfScope := strings.Replace(good, "/spec/storage/storageClass", "/spec/replica/primary", 1)
	f := &fakeAPI{replies: []reply{{text: outOfScope}, {text: good}}}
	a, sink := newAgent(t, f)
	log := "prepare failed: target cluster not healthy\nconnecting postgres://app:RealPassw0rd@pg-src-rw:5432/app\nWarning ProvisioningFailed storageclass.storage.k8s.io \"gp3\" not found"
	d, _, err := a.Diagnose(context.Background(), DiagnoseInput{PlanRef: "pln_1", Proposal: p, Draft: draft(t), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Fix.Patch[0].Value) != `"standard"` {
		t.Fatalf("수정안: %+v", d.Fix)
	}
	sent, _ := json.Marshal(f.requests[0])
	if strings.Contains(string(sent), "RealPassw0rd") {
		t.Fatal("비밀번호가 AI에 전달됨")
	}
	retry, _ := json.Marshal(f.requests[1]["messages"])
	if !strings.Contains(string(retry), "고칠 수 없는 매니페스트 경로") {
		t.Fatalf("허용 범위 밖 수정안이면 다시 물어야 함: %s", retry)
	}
	if s := sink.Sessions[0]; s.Info.Kind != agentlog.KindDiagnose || s.Info.PlanRef != "pln_1" {
		t.Fatalf("진단 세션은 planRef와 함께 열려야 함: %+v", s.Info)
	}
}

// failingFinish는 세션을 닫을 때만 실패하는 Sink다 (플랫폼 기록 실패 흉내).
type failingFinish struct{ agentlog.Memory }

type failingSession struct{ *agentlog.MemorySession }

func (f *failingFinish) Start(ctx context.Context, s agentlog.Start) (agentlog.Session, error) {
	ses, _ := f.Memory.Start(ctx, s)
	return failingSession{ses.(*agentlog.MemorySession)}, nil
}

func (s failingSession) Finish(context.Context, bool, string) error {
	return errors.New("세션 닫기 실패")
}

func TestLogFailureDoesNotDiscardResult(t *testing.T) {
	f := &fakeAPI{replies: []reply{{text: readFile(t, "../plan/testdata/proposal.json")}}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	var logErr error
	a := New(Config{
		Sink:       &failingFinish{},
		OnLogError: func(err error) { logErr = err },
		Options:    []option.RequestOption{option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0)},
	})
	p, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t)})
	if err != nil || p == nil {
		t.Fatalf("검사를 통과한 결과는 기록 실패로 버리면 안 됨: %v", err)
	}
	if logErr == nil {
		t.Fatal("기록 실패는 알려야 함")
	}
}
