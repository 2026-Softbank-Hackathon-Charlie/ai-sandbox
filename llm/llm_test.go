package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	text            string
	tool            string
	toolInput       string
	stopReason      string
	category        string
	textFromRequest func(map[string]any) string
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
	if rp.textFromRequest != nil {
		rp.text = rp.textFromRequest(req)
	}
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

// 원본(gke)과 대상(eks)의 DB 네임스페이스가 다르다(charlie-apps, moimroom). 2026-11-05 AI는 대상 클러스터를
// 원본의 네임스페이스에서 찾아 not found를 받았다. 환경마다 그 환경의 네임스페이스를 봐야 한다.
func TestGetCNPGClusterUsesEnvNamespace(t *testing.T) {
	cnpg := func(ns, name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": name, "namespace": ns},
			"spec":     map[string]any{"instances": int64(1)},
		}}
	}
	dyn := func(objs ...runtime.Object) *dynfake.FakeDynamicClient {
		return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{CNPGClusterGVR: "ClusterList"}, objs...)
	}
	cs := map[string]Cluster{
		"gke": {Typed: fake.NewClientset(), Dynamic: dyn(cnpg("charlie-apps", "moimroom-db-gke"))},
		"eks": {Typed: fake.NewClientset(), Dynamic: dyn(cnpg("moimroom", "moimroom-db"))},
	}
	get := func(sc PlanScope, env, name string) error {
		t.Helper()
		for _, tl := range PlanTools(sc, nil, cs) {
			if tl.Name == "get_cnpg_cluster" {
				_, err := tl.Run(context.Background(), json.RawMessage(fmt.Sprintf(`{"env":%q,"name":%q}`, env, name)))
				return err
			}
		}
		t.Fatal("get_cnpg_cluster 도구가 없음")
		return nil
	}

	perEnv := PlanScope{App: "moimroom", Namespace: "charlie-apps", Namespaces: map[string]string{"gke": "charlie-apps", "eks": "moimroom"}, Envs: []string{"gke", "eks"}}
	for env, name := range map[string]string{"gke": "moimroom-db-gke", "eks": "moimroom-db"} {
		if err := get(perEnv, env, name); err != nil {
			t.Errorf("%s/%s: 환경별 네임스페이스에서 찾아야 함: %v", env, name, err)
		}
	}
	// 환경별 값이 없으면 Namespace를 쓴다.
	legacy := PlanScope{App: "moimroom", Namespace: "charlie-apps", Envs: []string{"gke", "eks"}}
	if err := get(legacy, "gke", "moimroom-db-gke"); err != nil {
		t.Errorf("Namespaces가 없으면 Namespace로 찾아야 함: %v", err)
	}

	// 이름을 잘못 짚으면(2026-11-05 AI는 moimroom-db-eks를 조회했다) 어느 환경과 네임스페이스를 봤는지, 그 네임스페이스에
	// 실제로 있는 Cluster 이름이 오류에 있어야 한다. AI가 스스로 고치고 운영자가 원인을 가릴 수 있다.
	err := get(perEnv, "eks", "moimroom-db-eks")
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("없는 이름은 NotFound여야 함(원래 오류를 감싸야 함): %v", err)
	}
	for _, want := range []string{"환경 eks", "네임스페이스 moimroom", "moimroom-db-eks", "있는 Cluster: moimroom-db"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("오류에 %q가 없음: %v", want, err)
		}
	}
	// 그 네임스페이스에 Cluster가 하나도 없으면 그렇게 알린다.
	if err := get(PlanScope{App: "moimroom", Namespace: "empty", Envs: []string{"gke", "eks"}}, "gke", "x"); err == nil || !strings.Contains(err.Error(), "Cluster가 없음") {
		t.Errorf("빈 네임스페이스는 Cluster가 없다고 알려야 함: %v", err)
	}
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
		t.Error("Opus 5.5·Sonnet 5.5에 강제 도구 선택을 보내면 400")
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

func TestPlanEchoesSanitizedDraftAndRestoresImmutableCredentials(t *testing.T) {
	for _, plain := range []bool{false, true} {
		t.Run(map[bool]string{false: "environment references", true: "plaintext credentials"}[plain], func(t *testing.T) {
			d := draft(t)
			if plain {
				d.Source.DSN = strings.Replace(d.Source.DSN, "${GKE_MOIMROOM_PGPASSWORD}", "source-plain-secret", 1)
				d.Target.DSN = strings.Replace(d.Target.DSN, "${EKS_MOIMROOM_PGPASSWORD}", "target-plain-secret", 1)
				d.Gateway.Token = "gateway-plain-secret"
			}
			var calls int
			echo := func(req map[string]any) string {
				calls++
				messages := req["messages"].([]any)
				content := messages[0].(map[string]any)["content"].([]any)
				prompt := content[0].(map[string]any)["text"].(string)
				_, raw, ok := strings.Cut(prompt, "<draft>\n")
				if !ok {
					t.Error("draft missing from actual API prompt")
					return "{}"
				}
				raw, _, _ = strings.Cut(raw, "\n</draft>")
				var safe eplan.Plan
				if err := json.Unmarshal([]byte(raw), &safe); err != nil {
					t.Errorf("draft prompt must be valid JSON: %v", err)
					return "{}"
				}
				sent, _ := json.Marshal(req)
				for _, secret := range []string{"source-plain-secret", "target-plain-secret", "gateway-plain-secret"} {
					if strings.Contains(string(sent), secret) {
						t.Error("plaintext credential escaped into an API request")
					}
				}
				if !plain && (safe.Source.DSN != d.Source.DSN || safe.Gateway.Token != d.Gateway.Token) {
					t.Error("safe symbolic references must survive unchanged")
				}
				var answer plan.Proposal
				_ = json.Unmarshal([]byte(readFile(t, "../plan/testdata/proposal.json")), &answer)
				thresholds := answer.Plan.Thresholds
				answer.Plan = safe
				answer.Plan.Thresholds = thresholds
				answer.Plan.Target.Manifest = plan.ManifestRef
				if calls == 1 {
					answer.Plan.Target.DSN = strings.Replace(answer.Plan.Target.DSN, "moimroom-db-eks-rw", "changed-host", 1)
				}
				b, _ := json.Marshal(answer)
				return string(b)
			}
			f := &fakeAPI{replies: []reply{{textFromRequest: echo}, {textFromRequest: echo}, {textFromRequest: echo}}}
			a, _ := newAgent(t, f)
			p, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: d})
			if err != nil || p == nil {
				t.Fatalf("safe echo must produce an executable proposal: %v", err)
			}
			if calls != 2 {
				t.Fatal("changed immutable host must be rejected before restoring credentials")
			}
			if p.Plan.Source.DSN != d.Source.DSN || p.Plan.Target.DSN != d.Target.DSN || p.Plan.Gateway.Token != d.Gateway.Token {
				t.Fatal("validated proposal must retain the caller's original immutable credentials")
			}
			if p.Plan.Verify.FenceGate == nil || !*p.Plan.Verify.FenceGate {
				t.Fatal("the echoed draft must receive a safe fenceGate default")
			}
		})
	}
}

func TestPlanRetryErrorMasksRestoredCredentialParseFailures(t *testing.T) {
	const secret = "source%plain-secret"
	d := draft(t)
	d.Source.DSN = strings.Replace(d.Source.DSN, "${GKE_MOIMROOM_PGPASSWORD}", secret, 1)
	answer := strings.Replace(readFile(t, "../plan/testdata/proposal.json"), "${GKE_MOIMROOM_PGPASSWORD}", "***", 1)
	f := &fakeAPI{replies: []reply{{text: answer}, {text: answer}, {text: answer}}}
	a, _ := newAgent(t, f)
	_, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: d})
	var retry *plan.RetryError
	if !errors.As(err, &retry) || len(retry.Last) == 0 {
		t.Fatalf("restored malformed credential must fail validation: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("final error exposed a restored credential")
	}
	for _, issue := range retry.Last {
		if strings.Contains(issue.Path, secret) || strings.Contains(issue.Message, secret) {
			t.Error("structured retry issues exposed a restored credential")
		}
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

func TestPlanUsesSourceStats(t *testing.T) {
	v := readFile(t, "../plan/testdata/proposal.json")
	withConns := strings.Replace(v, `instances: 2\n`, `instances: 2\n  postgresql:\n    parameters:\n      max_connections: \"200\"\n      max_worker_processes: \"8\"\n`, 1)
	if withConns == v {
		t.Fatal("max_connections를 넣을 자리가 없음")
	}
	f := &fakeAPI{replies: []reply{{text: v}, {text: withConns}}}
	a, _ := newAgent(t, f)
	stats := &plan.SourceStats{Sessions: 80, MaxConnections: 200, MaxWorkerProcesses: 8, Bytes: 2 << 30, WALBytesPerSecond: 1 << 20}
	p, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t), Source: stats})
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != stats {
		t.Fatal("결과에 실측값이 붙어 있어야 수정안 검사에도 쓰임")
	}
	first, _ := json.Marshal(f.requests[0]["messages"])
	if !strings.Contains(string(first), "source_stats") || !strings.Contains(string(first), `requiredMaxConnections\": 200`) {
		t.Fatalf("실측값과 요구 접속 수가 AI에 전달돼야 함: %s", first)
	}
	second, _ := json.Marshal(f.requests[1]["messages"])
	if !strings.Contains(string(second), "max_connections") || !strings.Contains(string(second), "200 이상") {
		t.Fatalf("max_connections가 없으면 다시 물어야 함: %s", second)
	}
}

func TestPlanInfeasibleSourceSkipsAI(t *testing.T) {
	f := &fakeAPI{}
	a, sink := newAgent(t, f)
	big := &plan.SourceStats{Sessions: 10, MaxConnections: 100, Bytes: 200 << 30, WALBytesPerSecond: 1 << 20}
	_, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t), Source: big})
	if !errors.Is(err, ErrInfeasible) || !strings.Contains(err.Error(), "catchupTimeout 상한") {
		t.Fatalf("계획 불가 이유를 바로 돌려줘야 함: %v", err)
	}
	if len(f.requests) != 0 || len(sink.Sessions) != 0 {
		t.Fatalf("AI를 부르지 않아야 함: 요청 %d번, 세션 %d개", len(f.requests), len(sink.Sessions))
	}
}

func TestPlanChecksSourceCluster(t *testing.T) {
	// 원본(moimroom)은 17.11인데 AI가 18.6을 쓰면 다시 묻는다
	v := readFile(t, "../plan/testdata/proposal.json")
	pg18 := strings.Replace(v, "postgresql:17.11", "postgresql:18.6", 1)
	f := &fakeAPI{replies: []reply{{text: pg18}, {text: v}}}
	a, _ := newAgent(t, f)
	sc := &plan.SourceCluster{Image: "ghcr.io/cloudnative-pg/postgresql:17.11"}
	p, _, err := a.Plan(context.Background(), PlanInput{App: "moimroom", Draft: draft(t), SourceCluster: sc})
	if err != nil {
		t.Fatal(err)
	}
	if p.SourceCluster != sc {
		t.Fatal("결과에 원본 클러스터 값이 붙어 있어야 수정안 검사와 리허설에도 쓰임")
	}
	first, _ := json.Marshal(f.requests[0]["messages"])
	if !strings.Contains(string(first), "source_cluster") || !strings.Contains(string(first), "PostgreSQL 17") {
		t.Fatalf("원본 클러스터 값이 AI에 전달돼야 함: %s", first)
	}
	second, _ := json.Marshal(f.requests[1]["messages"])
	if !strings.Contains(string(second), "PostgreSQL 17 이미지여야 함") {
		t.Fatalf("메이저 버전이 다르면 다시 물어야 함: %s", second)
	}
}

func TestDiagnoseShowsSourceCluster(t *testing.T) {
	v := readFile(t, "../plan/testdata/proposal.json")
	var p plan.Proposal
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		t.Fatal(err)
	}
	p.SourceCluster = &plan.SourceCluster{Image: "ghcr.io/cloudnative-pg/postgresql:17.11"}
	diag := `{"cause":"임시 원본과 대상의 메이저 버전이 달라 복제할 수 없음","evidence":["pg_basebackup: error: incompatible server version 18.6"],"fix":{"kind":"manifest-patch","patch":[{"op":"replace","path":"/spec/imageName","value":"ghcr.io/cloudnative-pg/postgresql:17.6"}]}}`
	f := &fakeAPI{replies: []reply{{text: diag}}}
	a, _ := newAgent(t, f)
	if _, _, err := a.Diagnose(context.Background(), DiagnoseInput{Proposal: &p, Draft: draft(t), Log: "pg_basebackup: error: incompatible server version 18.6"}); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(f.requests[0]["messages"])
	if !strings.Contains(string(first), "source_cluster") || !strings.Contains(string(first), "PostgreSQL 17") {
		t.Fatalf("진단에도 원본 클러스터 값이 전달돼야 함: %s", first)
	}
}

// node_capacity는 할당 가능에서 떠 있는 Pod의 requests를 뺀 여유를 준다 (init 컨테이너는 큰 값 하나).
func TestNodeCapacityFree(t *testing.T) {
	q := resource.MustParse
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{
		Allocatable: corev1.ResourceList{corev1.ResourceCPU: q("1930m"), corev1.ResourceMemory: q("7Gi")},
		Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}}
	// 새 Pod를 받지 않는 노드는 표시한다
	cp := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Spec: corev1.NodeSpec{Unschedulable: true,
		Taints: []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}, {Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule}}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: q("2"), corev1.ResourceMemory: q("8Gi")}}}
	always := corev1.ContainerRestartPolicyAlways
	ctr := func(cpu, mem string) corev1.Container {
		return corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q(cpu), corev1.ResourceMemory: q(mem)}}}
	}
	pod := func(name, nodeName string, init []corev1.Container, cs ...corev1.Container) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "x"}, Spec: corev1.PodSpec{NodeName: nodeName, InitContainers: init, Containers: cs}}
	}
	sidecar := ctr("50m", "32Mi")
	sidecar.RestartPolicy = &always
	done := pod("done", "n1", nil, ctr("1", "1Gi"))
	done.Status.Phase = corev1.PodSucceeded
	cs := fake.NewClientset(node, cp, done,
		pod("c", "n1", []corev1.Container{sidecar}, ctr("100m", "64Mi")),                                    // 사이드카는 더함: 150m, 96Mi
		pod("a", "n1", []corev1.Container{ctr("500m", "64Mi")}, ctr("100m", "128Mi"), ctr("200m", "128Mi")), // max(300m, 500m) = 500m, 256Mi
		pod("b", "n1", nil, ctr("250m", "1Gi")),
		pod("pending", "", nil, ctr("4", "4Gi")), // 아직 노드가 없으면 세지 않음, 끝난 Pod(done)도 세지 않음
	)
	var tool Tool
	for _, tl := range PlanTools(PlanScope{App: "a", Namespace: "x", Envs: []string{"eks"}}, nil, map[string]Cluster{"eks": {Typed: cs, Dynamic: clusters()["eks"].Dynamic}}) {
		if tl.Name == "node_capacity" {
			tool = tl
		}
	}
	got, err := tool.Run(context.Background(), json.RawMessage(`{"env":"eks"}`))
	if err != nil {
		t.Fatal(err)
	}
	nodes := got.([]map[string]any)
	n := nodes[0]
	if n["requestedCpu"] != "900m" || n["freeCpu"] != "1030m" || n["requestedMemory"] != "1376Mi" {
		t.Fatalf("node = %v, want requestedCpu 900m, freeCpu 1030m, requestedMemory 1376Mi", n)
	}
	if _, ok := n["unschedulable"]; ok {
		t.Fatalf("n1 = %v, want no unschedulable", n)
	}
	n2 := nodes[1]
	if n2["unschedulable"] != true || fmt.Sprint(n2["taints"]) != "[node-role.kubernetes.io/control-plane:NoSchedule]" {
		t.Fatalf("n2 = %v, want unschedulable and only the NoSchedule taint", n2)
	}
}

func TestParseEffort(t *testing.T) {
	if e, err := ParseEffort(""); err != nil || e != "" {
		t.Fatalf("empty = %q, %v", e, err)
	}
	if e, err := ParseEffort("high"); err != nil || string(e) != "high" {
		t.Fatalf("high = %q, %v", e, err)
	}
	if _, err := ParseEffort("ultra"); err == nil {
		t.Fatal("want error for unknown effort")
	}
}
