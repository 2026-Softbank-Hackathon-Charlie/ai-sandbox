// Package rehearsal은 승인 전에 대상 클러스터의 샌드박스에서 실제 엔진으로 Plan을 미리 돌린다 (설계 문서 7장).
//
// 한 번의 실행: 샌드박스(CNPG 모드) 생성 → 격리 검사 → 임시 원본 CNPG와 테스트 데이터 → AI가 쓴 대상 매니페스트로
// 엔진 9단계 실행(샌드박스 토큰, 게이트웨이 없음) → 실패하면 샌드박스가 살아 있는 동안 AI 진단 → 샌드박스 삭제.
// 수정안은 Plan·매니페스트 사본에 적용해 새 샌드박스에서 다시 돌린다. 원본은 바꾸지 않는다. 고정값을 돌려주는 단계는 없다.
package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/sandbox"
)

// Diagnoser는 리허설 실패를 진단한다. llm.Agent가 이 모양이다.
type Diagnoser interface {
	Diagnose(ctx context.Context, in llm.DiagnoseInput) (*plan.Diagnosis, string, error)
}

// Config는 리허설 설정이다.
type Config struct {
	// Admin은 대상 클러스터의 관리자 쪽 접속이다. 샌드박스 생성·삭제, 격리 검사 Pod, port-forward에만 쓴다.
	Admin     kubernetes.Interface
	AdminREST *rest.Config

	Proposal *plan.Proposal
	Draft    *eplan.Plan
	PlanRef  string
	// PlanSession은 이 계획을 만든 계획 세션 ID다. 계획 세션은 Plan 제출 전에 열려 planRef가 없으므로,
	// 리허설 세션(planRef 있음)의 첫 기록에 남겨 승인 화면에서 찾아갈 수 있게 한다.
	PlanSession string

	Diagnoser Diagnoser
	Sink      agentlog.Sink
	// Isolation은 샌드박스마다 돌리는 격리 검사다. ProdNamespace는 필수.
	Isolation sandbox.IsolationConfig
	// DiagnoseTools는 진단 AI에 더 줄 도구다 (예: 대상 환경의 list_storage_classes).
	DiagnoseTools []llm.Tool

	MaxRuns int // 기본 3 (설계 문서 7장)
	Rows    int // 테스트 데이터 행 수, 기본 20000
	// FailFast는 같은 경고 이벤트가 이만큼 이어지면 엔진을 멈추고 실패로 본다. 기본 25초.
	// 예: StorageClass가 없으면 PVC가 영원히 Pending이라 엔진의 catchupTimeout(분 단위)까지 기다릴 필요가 없다.
	FailFast      time.Duration
	SourceTimeout time.Duration // 임시 원본이 healthy가 될 때까지, 기본 6분
	RunTimeout    time.Duration // 엔진 실행 한 번의 상한, 기본 10분
	OnAttempt     func(Attempt)
}

// Attempt는 실행 한 번의 기록이다.
type Attempt struct {
	N         int                   `json:"n"`
	Namespace string                `json:"namespace"`
	Candidate plan.Digest           `json:"candidate"` // 이 회차에 돌린 계획
	Passed    bool                  `json:"passed"`
	Outcome   string                `json:"outcome,omitempty"` // 엔진 결과
	Steps     map[string]string     `json:"steps,omitempty"`   // 단계별 상태
	Coverage  *Coverage             `json:"coverage,omitempty"`
	Cleanup   *CleanupResult        `json:"cleanup,omitempty"`
	Isolation []sandbox.CheckResult `json:"isolation,omitempty"`
	Error     string                `json:"error,omitempty"`
	Evidence  []string              `json:"evidence,omitempty"`
	Diagnosis *plan.Diagnosis       `json:"diagnosis,omitempty"`
	FixDiff   string                `json:"fixDiff,omitempty"`
	DurationS float64               `json:"durationSec"`
}

// CleanupResult distinguishes an accepted deletion request from a cleanup failure.
// A request does not prove that Kubernetes has finished deleting the namespace.
type CleanupResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ReportSchema는 rehearsal-report.json의 형식 버전이다. 플랫폼 승인 게이트(platform #24)가 이 값으로 읽는 법을 고른다.
const ReportSchema = "hopcharlie.rehearsal/v1"

// Report는 리허설 전체 결과다.
type Report struct {
	SchemaVersion string `json:"schemaVersion"`
	Passed        bool   `json:"passed"`
	// Candidate는 Passed를 정한 마지막 회차의 계획이다. 승인할 Plan 본문과 target.yaml 첨부의 해시가 이 값과 같아야 한다.
	Candidate plan.Digest `json:"candidate"`
	Attempts  []Attempt   `json:"attempts"`
	Warnings  []string    `json:"warnings,omitempty"`
	// Final은 마지막 계획(수정안 반영)이다. Changed면 플랫폼에 다시 올려야 한다.
	Final        *plan.Proposal `json:"-"`
	Changed      bool           `json:"changed"`
	ManifestDiff string         `json:"manifestDiff,omitempty"`
	Session      string         `json:"session"`
}

// ErrIsolation은 샌드박스 격리가 깨졌을 때다. 그 샌드박스에서는 아무것도 실행하지 않고 리허설 전체를 멈춘다.
var ErrIsolation = errors.New("샌드박스 격리 검사 실패")

func (c *Config) defaults() {
	if c.MaxRuns == 0 {
		c.MaxRuns = 3
	}
	if c.Rows == 0 {
		c.Rows = 20000
	}
	if c.FailFast == 0 {
		c.FailFast = 25 * time.Second
	}
	if c.SourceTimeout == 0 {
		c.SourceTimeout = 6 * time.Minute
	}
	if c.RunTimeout == 0 {
		c.RunTimeout = 10 * time.Minute
	}
	if c.Sink == nil {
		c.Sink = &agentlog.Memory{}
	}
}

// Rehearse reports execution and fixture verification through Report.Passed.
// Session start or execution failures can return errors. Recording and cleanup
// failures remain warnings and do not change the execution result.
func Rehearse(ctx context.Context, c Config) (*Report, error) {
	c.defaults()
	if c.Proposal == nil || c.Draft == nil {
		return nil, errors.New("계획과 초안이 필요함")
	}
	if c.MaxRuns < 1 || c.Rows < 4 {
		return nil, errors.New("maxRuns must be positive and rows must be at least 4")
	}
	ses, err := c.Sink.Start(ctx, agentlog.Start{Kind: agentlog.KindRehearsal, Title: "샌드박스 리허설 (" + c.Proposal.Plan.ID + ")", PlanRef: c.PlanRef})
	if err != nil {
		return nil, err
	}
	rep := &Report{SchemaVersion: ReportSchema, Session: ses.ID()}
	if c.PlanSession != "" {
		ses.Log(ctx, agentlog.Event{Type: agentlog.Message, Tool: "plan", Output: map[string]any{"planRef": c.PlanRef, "planSession": c.PlanSession, "planId": c.Proposal.Plan.ID}})
	}
	cur := c.Proposal
	var fatal error
	for n := 1; n <= c.MaxRuns; n++ {
		a, next, err := runOnce(ctx, &c, ses, n, cur, n < c.MaxRuns)
		rep.Attempts = append(rep.Attempts, a)
		if a.Cleanup != nil && a.Cleanup.Status == "failed" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("sandbox cleanup %s: %s", a.Namespace, a.Cleanup.Error))
		}
		if c.OnAttempt != nil {
			c.OnAttempt(a)
		}
		if err != nil {
			fatal = err
			break
		}
		if a.Passed || next == nil {
			break
		}
		cur = next
	}
	last := rep.Attempts[len(rep.Attempts)-1]
	rep.Passed = last.Passed && fatal == nil
	rep.Candidate = last.Candidate
	rep.Final = cur
	rep.Changed = cur != c.Proposal
	if rep.Changed && cur.Manifest != c.Proposal.Manifest {
		rep.ManifestDiff = Diff(c.Proposal.Manifest, cur.Manifest)
	}
	sum := fmt.Sprintf("%d회 실행, %s", len(rep.Attempts), map[bool]string{true: "통과", false: "통과하지 못함"}[rep.Passed])
	if rep.Changed {
		sum += ", AI 수정안 반영"
	}
	finishRehearsal(ctx, ses, rep, sum)
	return rep, fatal
}

func finishRehearsal(ctx context.Context, ses agentlog.Session, rep *Report, summary string) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := ses.Finish(fctx, rep.Passed, summary); err != nil {
		rep.Warnings = append(rep.Warnings, mask.String(fmt.Sprintf("session recording: %v", err)))
	}
}

func cleanupAttempt(ctx context.Context, c *Config, ses agentlog.Session, a *Attempt) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	a.Cleanup = &CleanupResult{Status: "delete_requested"}
	output := map[string]any{"namespace": a.Namespace}
	if err := sandbox.Delete(dctx, c.Admin, a.Namespace); err != nil {
		a.Cleanup.Status = "failed"
		a.Cleanup.Error = mask.String(err.Error())
		output["error"] = a.Cleanup.Error
	}
	ses.Log(dctx, agentlog.Event{Type: agentlog.ToolResult, Tool: "sandbox_delete", Status: a.Cleanup.Status, Output: output})
}

// runOnce는 새 샌드박스에서 한 번 돌린다. 실패하고 다시 돌릴 수 있으면 샌드박스를 지우기 전에 진단해서 다음 계획을 돌려준다.
func runOnce(ctx context.Context, c *Config, ses agentlog.Session, n int, cur *plan.Proposal, canRetry bool) (a Attempt, next *plan.Proposal, fatal error) {
	start := time.Now()
	a = Attempt{N: n}
	defer func() { a.DurationS = time.Since(start).Seconds() }()
	if a.Candidate, fatal = cur.Digest(); fatal != nil {
		return a, nil, fmt.Errorf("계획 해시 계산 실패: %w", fatal)
	}
	failf := func(format string, args ...any) {
		a.Error = mask.String(fmt.Sprintf(format, args...))
		ses.Log(ctx, agentlog.Event{Type: agentlog.Error, Status: fmt.Sprintf("attempt %d", n), Output: a.Error})
	}

	// 1. 샌드박스
	base, err := SandboxConfig(SandboxApp(cur.Plan.Source.Namespace), cur.Manifest)
	if err != nil {
		return a, nil, err
	}
	sc, err := sandbox.ForCNPG(ctx, c.Admin, base)
	if err != nil {
		return a, nil, err
	}
	ses.Log(ctx, agentlog.Event{Type: agentlog.ToolCall, Tool: "sandbox_create", Input: map[string]any{"attempt": n, "cluster": cur.Plan.Target.Env}})
	t0 := time.Now()
	sb, err := sandbox.Create(ctx, c.Admin, sc, time.Now())
	if err != nil {
		ses.Log(ctx, agentlog.Event{Type: agentlog.ToolResult, Tool: "sandbox_create", Status: "error", Output: err.Error()})
		return a, nil, err
	}
	a.Namespace = sb.Namespace
	ses.Log(ctx, agentlog.Event{Type: agentlog.ToolResult, Tool: "sandbox_create", Status: "ok", Duration: time.Since(t0),
		Output: map[string]any{"namespace": sb.Namespace, "expiresAt": sb.ExpiresAt, "networkPolicy": "기본 차단 + CNPG 오퍼레이터·API 서버", "quota": fmt.Sprintf("CPU %s, 메모리 %s, Pod 20", base.CPU, base.Memory)}})
	defer cleanupAttempt(ctx, c, ses, &a)

	// 2. AI 권한(샌드박스 토큰)과 격리 검사
	token, _, err := sandbox.Token(ctx, c.Admin, sb, 0, time.Now())
	if err != nil {
		return a, nil, err
	}
	aiREST := sandbox.TokenConfig(c.AdminREST, token)
	aiCS, err := kubernetes.NewForConfig(aiREST)
	if err != nil {
		return a, nil, err
	}
	aiDyn, err := dynamic.NewForConfig(aiREST)
	if err != nil {
		return a, nil, err
	}
	iso := c.Isolation
	iso.APIServerAllowed = true
	rs, err := sandbox.CheckIsolation(ctx, c.Admin, aiCS, sb, iso)
	a.Isolation = rs
	logIsolation(ctx, ses, rs)
	if err != nil || !sandbox.AllPass(rs) {
		if err != nil {
			failf("격리 검사 실패: %v", err)
		} else {
			failf("격리 검사 실패: %s", strings.Join(sandbox.Failed(rs), ", "))
		}
		return a, nil, ErrIsolation
	}

	// 3. 임시 원본
	db, err := ParseDB(cur.Plan.Source.DSN)
	if err != nil {
		failf("원본 DSN: %v", err)
		return a, nil, nil
	}
	src := SourceManifest(&cur.Plan, sb.Namespace, db)
	if err := PrepareTempSource(src, cur.SourceCluster, cur.Manifest); err != nil {
		failf("임시 원본 설정: %v", err)
		return a, nil, nil
	}
	scs, err := c.Admin.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		failf("샌드박스 클러스터의 StorageClass를 읽을 수 없음: %v", err)
		return a, nil, nil
	}
	tempSC, err := TempStorageClass(scs.Items, cur.Manifest)
	if err != nil {
		failf("임시 원본 디스크: %v", err)
		return a, nil, nil
	}
	if tempSC != "" {
		if err := unstructured.SetNestedField(src.Object, tempSC, "spec", "storage", "storageClass"); err != nil {
			failf("임시 원본 설정: %v", err)
			return a, nil, nil
		}
	}
	ses.Log(ctx, agentlog.Event{Type: agentlog.ToolCall, Tool: "sandbox_source", Input: map[string]any{"cluster": cur.Plan.Source.Cluster, "rows": c.Rows, "schemas": cur.Plan.Verify.Schemas, "storageClass": tempSC}})
	t0 = time.Now()
	if _, err := aiDyn.Resource(llm.CNPGClusterGVR).Namespace(sb.Namespace).Create(ctx, src, metav1.CreateOptions{}); err != nil {
		failf("임시 원본 생성 실패: %v", err)
		return a, nil, nil
	}
	if err := waitHealthy(ctx, aiDyn, sb.Namespace, cur.Plan.Source.Cluster, c.SourceTimeout); err != nil {
		// 샌드박스를 곧 지우므로 원인 단서(Pod·PVC 상태, 경고 이벤트)를 지우기 전에 세션 로그와 리포트에 남긴다.
		var brief string
		a.Evidence, brief = sourceNotReadyEvidence(ctx, aiCS, sb.Namespace)
		logSourceStatus(ctx, ses, a.Evidence)
		if brief != "" {
			failf("임시 원본이 준비되지 않음: %v (%s)", err, brief)
		} else {
			failf("임시 원본이 준비되지 않음: %v", err)
		}
		return a, nil, nil
	}
	sec, err := aiCS.CoreV1().Secrets(sb.Namespace).Get(ctx, cur.Plan.Source.Cluster+"-app", metav1.GetOptions{})
	if err != nil {
		failf("임시 원본 앱 계정을 읽을 수 없음: %v", err)
		return a, nil, nil
	}
	password := string(sec.Data["password"])
	srcProxy, err := NewProxy(c.Admin, c.AdminREST, sb.Namespace, cur.Plan.Source.Cluster)
	if err != nil {
		return a, nil, err
	}
	defer srcProxy.Close()
	dstProxy, err := NewProxy(c.Admin, c.AdminREST, sb.Namespace, cur.Plan.Target.Cluster)
	if err != nil {
		return a, nil, err
	}
	defer dstProxy.Close()
	srcDSN, dstDSN := DSN(db, password, srcProxy.Port()), DSN(db, password, dstProxy.Port())
	if err := Seed(ctx, srcDSN, cur.Plan.Verify.Schemas, c.Rows); err != nil {
		failf("%v", err)
		return a, nil, nil
	}
	ses.Log(ctx, agentlog.Event{Type: agentlog.ToolResult, Tool: "sandbox_source", Status: "ok", Duration: time.Since(t0), Output: map[string]any{"cluster": cur.Plan.Source.Cluster, "rows": c.Rows}})

	// 4. 엔진 (샌드박스 토큰, 게이트웨이 없음)
	dir, err := os.MkdirTemp("", "hop-rehearsal-")
	if err != nil {
		return a, nil, err
	}
	defer os.RemoveAll(dir)
	kc, err := TokenKubeconfig(dir, c.AdminREST, token)
	if err != nil {
		return a, nil, err
	}
	manifest, err := TargetManifest(cur.Manifest, &cur.Plan, sb.Namespace)
	if err != nil {
		failf("%v", err)
		return a, nil, nil
	}
	mpath := dir + string(os.PathSeparator) + plan.ManifestName
	if err := os.WriteFile(mpath, []byte(manifest), 0o600); err != nil {
		return a, nil, err
	}
	sp := SandboxPlan(&cur.Plan, sb.Namespace, kc, mpath, srcDSN, dstDSN)
	if err := sp.Validate(); err != nil {
		failf("샌드박스 Plan: %v", err)
		return a, nil, nil
	}
	log := &engineLog{}
	eng, err := engine.New(sp, engine.Options{StateDir: dir, PollInterval: time.Second, Emit: func(ev engine.Event) {
		log.add(ev)
		logEngineEvent(ctx, ses, ev)
	}})
	if err != nil {
		failf("엔진 준비 실패: %v", err)
		return a, nil, nil
	}
	rctx, cancel := context.WithTimeoutCause(ctx, c.RunTimeout, errors.New("리허설 시간 상한"))
	defer cancel()
	// 감시는 엔진이 끝나면 바로 멈추고, 끝난 것을 확인한 뒤에 결과를 본다.
	// 그래야 엔진이 끝난 뒤의 늦은 판정이 성공한 실행을 실패로 바꾸지 못한다.
	wctx, stopWatch := context.WithCancel(rctx)
	stuck := make(chan []string, 1)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		watchStuck(wctx, aiCS, sb.Namespace, c.FailFast, stuck, cancel)
	}()
	runErr := eng.Run(rctx)
	stopWatch()
	<-watched
	eng.Close()
	st := eng.State()
	a.Outcome = st.Outcome
	a.Steps = map[string]string{}
	for k, v := range st.Steps {
		a.Steps[k] = v.Status
	}
	select {
	case ev := <-stuck:
		if st.Outcome != engine.OutcomeSucceeded {
			a.Evidence = ev
			runErr = fmt.Errorf("기다려도 풀리지 않는 문제로 멈춤 (같은 경고가 %s 넘게 이어지거나 작업 Job이 거듭 실패): %w", c.FailFast, runErr)
		}
	default:
	}
	if runErr == nil && st.Outcome == engine.OutcomeSucceeded {
		coverage, err := fixtureCoverage(st, sp.Verify.Schemas, c.Rows)
		if err != nil {
			failf("fixture verification coverage: %v", err)
			return a, nil, nil
		}
		a.Coverage = coverage
		a.Passed = true
		ses.Log(ctx, agentlog.Event{Type: agentlog.Decision, Status: "ok", Output: fmt.Sprintf("%d회차 통과: 엔진 %s, 쓰기 보류 %dms", n, st.Outcome, st.DowntimeMillis)})
		return a, nil, nil
	}
	if len(a.Evidence) == 0 {
		a.Evidence = warningEvents(ctx, aiCS, sb.Namespace)
	}
	failf("%d회차 실패: 엔진 %s, 단계 %s: %v", n, st.Outcome, st.FailureStep, firstErr(runErr, st.FailureError))
	if !canRetry || c.Diagnoser == nil {
		return a, nil, nil
	}

	// 5. 샌드박스가 살아 있는 동안 진단
	tools := append(SandboxTools(aiCS, aiDyn, sb.Namespace), c.DiagnoseTools...)
	d, _, err := c.Diagnoser.Diagnose(ctx, llm.DiagnoseInput{
		PlanRef: c.PlanRef, Proposal: cur, Draft: c.Draft, Tools: tools,
		Log: failureLog(a, st, log.tail(40)),
	})
	if err != nil {
		failf("진단 실패: %v", err)
		return a, nil, nil
	}
	a.Diagnosis = d
	nx, is := d.Fix.Apply(cur, c.Draft)
	if len(is) > 0 {
		failf("수정안 적용 실패: %s", is.Error())
		return a, nil, nil
	}
	if nx.Manifest != cur.Manifest {
		a.FixDiff = Diff(cur.Manifest, nx.Manifest)
	}
	ses.Log(ctx, agentlog.Event{Type: agentlog.Decision, Status: "fix", Output: map[string]any{"attempt": n, "cause": d.Cause, "fix": d.Fix}})
	return a, nx, nil
}

// SandboxApp은 샌드박스 이름에 넣을 앱 이름이다. 네임스페이스는 63자까지지만 샌드박스 앱 이름은 32자까지라,
// 길면 앞 27자에 원래 이름의 해시 4자리를 붙인다 (같은 네임스페이스는 늘 같은 이름).
func SandboxApp(namespace string) string {
	if len(namespace) <= 32 {
		return namespace
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(namespace))
	return strings.TrimRight(namespace[:27], "-") + "-" + fmt.Sprintf("%08x", h.Sum32())[:4]
}

func firstErr(err error, s string) string {
	if err != nil {
		return err.Error()
	}
	return s
}

// logIsolation은 차단 테스트 결과를 남긴다. 막혀야 해서 막힌 명령은 blocked로 남아 콘솔의 '막힌 명령' 표에 나온다.
func logIsolation(ctx context.Context, ses agentlog.Session, rs []sandbox.CheckResult) {
	for _, r := range rs {
		tool := "kubectl"
		if strings.HasPrefix(r.Command, "curl") || strings.HasPrefix(r.Command, "nc ") || strings.HasPrefix(r.Command, "nslookup") {
			tool = strings.Fields(r.Command)[0]
		}
		e := agentlog.Event{Tool: tool, Input: map[string]any{"command": r.Command}, Output: r.Got, Status: map[bool]string{true: "expected", false: "unexpected"}[r.Pass]}
		if r.Expect == sandbox.ExpectBlocked && r.Got == sandbox.ExpectBlocked {
			e.Type = agentlog.Blocked
			e.Output = isolationGuard(r)
		} else {
			e.Type = agentlog.ToolResult
		}
		ses.Log(ctx, e)
	}
}

func isolationGuard(r sandbox.CheckResult) string {
	if (r.Name == "host-pod-create" || r.Name == "host-pod-admission") && r.Detail != "" {
		return r.Detail
	}
	if strings.HasPrefix(r.Command, "kubectl") {
		return "RBAC: 샌드박스 계정 권한 밖"
	}
	return "NetworkPolicy: 샌드박스 밖으로 나가는 연결 차단"
}

// logEngineEvent는 엔진 단계 이벤트를 리허설 세션에 남긴다. 로그와 지표는 양이 많아 남기지 않고 진단 로그에만 넣는다.
func logEngineEvent(ctx context.Context, ses agentlog.Session, ev engine.Event) {
	tool := "engine." + ev.Step
	switch ev.Kind {
	case engine.EventStepStarted:
		ses.Log(ctx, agentlog.Event{Type: agentlog.ToolCall, Tool: tool})
	case engine.EventStepSucceeded:
		e := agentlog.Event{Type: agentlog.ToolResult, Tool: tool, Status: "ok", Output: ev.Message}
		if ms, ok := ev.Data["durationMillis"].(int64); ok {
			e.Duration = time.Duration(ms) * time.Millisecond
		}
		ses.Log(ctx, e)
	case engine.EventStepFailed:
		ses.Log(ctx, agentlog.Event{Type: agentlog.ToolResult, Tool: tool, Status: "failed", Output: ev.Message})
	case engine.EventRollback, engine.EventHalt, engine.EventDone:
		ses.Log(ctx, agentlog.Event{Type: agentlog.Message, Tool: "engine", Status: ev.Kind, Output: ev.Message})
	}
}

// engineLog는 엔진 이벤트를 모아 두었다가 진단 로그로 쓴다.
type engineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *engineLog) add(ev engine.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := ev.Time.UTC().Format("15:04:05") + " " + ev.Kind
	if ev.Step != "" {
		line += " [" + ev.Step + "]"
	}
	if ev.Message != "" {
		line += " " + ev.Message
	}
	l.lines = append(l.lines, line)
}

func (l *engineLog) tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) <= n {
		return append([]string(nil), l.lines...)
	}
	return append([]string(nil), l.lines[len(l.lines)-n:]...)
}

func failureLog(a Attempt, st engine.State, engineTail []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "rehearsal %s attempt=%d outcome=%s failureStep=%s\n", a.Namespace, a.N, st.Outcome, st.FailureStep)
	if a.Error != "" {
		sb.WriteString("error: " + a.Error + "\n")
	}
	keys := make([]string, 0, len(st.Steps))
	for k := range st.Steps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if r := st.Steps[k]; r.Status != engine.StatusPending {
			fmt.Fprintf(&sb, "step %s %s %s\n", k, r.Status, r.Error)
		}
	}
	sb.WriteString("--- engine events\n")
	for _, l := range engineTail {
		sb.WriteString(l + "\n")
	}
	sb.WriteString("--- kubernetes warning events\n")
	for _, l := range a.Evidence {
		sb.WriteString(l + "\n")
	}
	return sb.String()
}

func waitHealthy(ctx context.Context, dyn dynamic.Interface, ns, name string, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		obj, err := dyn.Resource(llm.CNPGClusterGVR).Namespace(ns).Get(wctx, name, metav1.GetOptions{})
		if err == nil {
			if phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase"); phase == "Cluster in healthy state" {
				return nil
			}
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("%s 안에 healthy가 되지 않음", timeout)
		case <-time.After(3 * time.Second):
		}
	}
}

// 진행을 막는 경고 이유. 이 경고가 FailFast 넘게 이어지면 기다려도 풀리지 않는 환경 문제로 본다.
var stuckReasons = map[string]bool{
	"ProvisioningFailed": true, "FailedScheduling": true, "FailedMount": true, "FailedAttachVolume": true,
	"ErrImagePull": true, "ImagePullBackOff": true, "Failed": true, "BackOff": true, "FailedCreate": true,
}

// recurring은 경고 하나를 지켜본 기록이다. 시각은 모두 이 프로세스의 시계로 잰다(클러스터와 시계가 달라도 되게).
type recurring struct {
	since      time.Time // 처음 본 때
	lastChange time.Time // 횟수나 마지막 시각이 마지막으로 바뀐 때
	count      int32
	last       time.Time
}

// watchStuck은 샌드박스의 경고 이벤트를 보다가, 같은 경고가 window 넘게 **계속 되풀이되면** 엔진을 멈춘다.
// 이 실행 전부터 있던 경고는 보지 않고, window 동안 다시 일어나지 않은 경고는 풀린 것으로 보고 잊는다.
// 잠깐 났다 사라진 경고 때문에 정상 실행을 멈추지 않기 위해서다.
func watchStuck(ctx context.Context, cs kubernetes.Interface, ns string, window time.Duration, out chan<- []string, cancel context.CancelFunc) {
	seen := map[string]*recurring{}
	started := time.Now()
	ignore := map[string]bool{} // 이 실행 전에 이미 있던 경고
	if list, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"}); err == nil {
		for _, e := range list.Items {
			ignore[eventKey(e)+"@"+eventTime(e).String()] = true
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		// 경고 이벤트 없이 작업 Pod만 거듭 실패하는 경우(예: 메이저 버전이 달라 pg_basebackup이 거절됨)
		if lines := failedJobPods(ctx, cs, ns, started, failedJobPodLimit); len(lines) > 0 {
			out <- append(warningEvents(context.WithoutCancel(ctx), cs, ns), lines...)
			cancel()
			return
		}
		list, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
		if err != nil {
			continue
		}
		now := time.Now()
		for _, e := range list.Items {
			if !stuckReasons[e.Reason] || ignore[eventKey(e)+"@"+eventTime(e).String()] {
				continue
			}
			key := eventKey(e)
			r, ok := seen[key]
			switch {
			case !ok:
				seen[key] = &recurring{since: now, lastChange: now, count: eventCount(e), last: eventTime(e)}
				continue
			case eventCount(e) != r.count || eventTime(e).After(r.last):
				r.count, r.last, r.lastChange = eventCount(e), eventTime(e), now
			}
			if now.Sub(r.lastChange) > window {
				delete(seen, key) // 다시 일어나지 않았다: 풀린 것으로 본다
				continue
			}
			if now.Sub(r.since) > window && r.lastChange.After(r.since) && now.Sub(started) > window {
				out <- warningEvents(context.WithoutCancel(ctx), cs, ns)
				cancel()
				return
			}
		}
	}
}

func eventCount(e corev1.Event) int32 {
	if e.Series != nil {
		return e.Series.Count
	}
	return e.Count
}

func eventKey(e corev1.Event) string {
	return e.Reason + "/" + e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
}

// warningEvents는 네임스페이스의 경고 이벤트를 시간순 로그 줄로 돌려준다. 진단 AI에 넘기는 근거다.
// failedJobPodLimit은 같은 Job의 Pod가 몇 번 실패하면 멈춘 것으로 보는지다. Job은 10초, 20초, 40초 간격으로 다시 시도한다.
const failedJobPodLimit = 3

// failedJobPods는 CNPG 작업 Job(pg_basebackup, join 등)의 Pod가 since 뒤에 limit번 넘게 실패했는지 본다.
// 이런 실패는 쿠버네티스 경고 이벤트를 남기지 않고 Job이 다시 시도만 해서, 재시도가 다 끝날 때(10분 넘게)까지 드러나지 않는다.
// CNPG 1.30.1에서 대상 PostgreSQL 17, 원본 18일 때 실제로 이랬다. 마지막 Pod 로그의 오류 줄을 근거로 붙인다.
func failedJobPods(ctx context.Context, cs kubernetes.Interface, ns string, since time.Time, limit int) []string {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	byJob := map[string][]corev1.Pod{}
	for _, p := range pods.Items {
		job := p.Labels["job-name"]
		if job == "" || p.Status.Phase != corev1.PodFailed || p.CreationTimestamp.Time.Before(since) {
			continue
		}
		byJob[job] = append(byJob[job], p)
	}
	jobs := make([]string, 0, len(byJob))
	for job := range byJob {
		jobs = append(jobs, job)
	}
	sort.Strings(jobs)
	var out []string
	for _, job := range jobs {
		ps := byJob[job]
		if len(ps) < limit {
			continue
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i].CreationTimestamp.Before(&ps[j].CreationTimestamp) })
		last := ps[len(ps)-1]
		out = append(out, mask.String(fmt.Sprintf("Failed  job/%s  Pod가 %d번 실패함 (마지막 pod/%s%s)", job, len(ps), last.Name, exitSummary(last))))
		out = append(out, logErrors(ctx, cs, ns, last.Name)...)
	}
	return out
}

func exitSummary(p corev1.Pod) string {
	for _, c := range p.Status.ContainerStatuses {
		if t := c.State.Terminated; t != nil && t.ExitCode != 0 {
			return fmt.Sprintf(", %s %s 종료 코드 %d", c.Name, t.Reason, t.ExitCode)
		}
	}
	return ""
}

// logErrors는 Pod 로그 끝에서 오류 줄만 가려서 가져온다. 비밀값은 가린다.
func logErrors(ctx context.Context, cs kubernetes.Interface, ns, pod string) []string {
	tail := int64(100)
	b, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if l := strings.ToLower(line); strings.Contains(l, "error") || strings.Contains(l, "fatal") {
			out = append(out, mask.String("  log  pod/"+pod+"  "+strings.TrimSpace(line)))
		}
	}
	if len(out) > 5 {
		out = out[len(out)-5:]
	}
	return out
}

func warningEvents(ctx context.Context, cs kubernetes.Interface, ns string) []string {
	list, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err != nil {
		return nil
	}
	evs := list.Items
	sort.Slice(evs, func(i, j int) bool { return eventTime(evs[i]).Before(eventTime(evs[j])) })
	seen := map[string]bool{}
	var out []string
	for _, e := range evs {
		line := fmt.Sprintf("Warning  %s  %s/%s  %s", e.Reason, strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name, strings.TrimSpace(e.Message))
		if !seen[line] {
			seen[line] = true
			out = append(out, mask.String(line))
		}
	}
	if len(out) > 30 {
		out = out[len(out)-30:]
	}
	return out
}

func eventTime(e corev1.Event) time.Time {
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		return e.Series.LastObservedTime.Time
	}
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	return e.EventTime.Time
}
