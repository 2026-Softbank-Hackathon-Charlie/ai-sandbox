package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/sandbox"
)

// prepared는 엔진을 돌리기 직전까지 준비된 샌드박스다: 격리 검사를 통과했고, 임시 원본이 떠서 테스트 데이터가 들어 있다.
type prepared struct {
	sb       *sandbox.Sandbox
	token    string
	aiCS     kubernetes.Interface
	aiDyn    dynamic.Interface
	db       DBInfo
	srcProxy *Proxy
	dstProxy *Proxy
	tempSC   string // 임시 원본 디스크의 StorageClass ("" = 클러스터 기본값)
	srcDSN   string
	dstDSN   string
}

func (p *prepared) close() {
	if p == nil {
		return
	}
	if p.srcProxy != nil {
		p.srcProxy.Close()
	}
	if p.dstProxy != nil {
		p.dstProxy.Close()
	}
}

// prepareSandbox는 샌드박스를 만들고, 격리 검사와 임시 원본(생성, healthy 대기, 테스트 데이터)을 동시에 진행한다.
// 임시 원본은 AI가 아니라 리허설이 정해진 매니페스트로 만드는 것이라 격리 검사 결과를 기다리지 않는다. 격리 검사는
// 엔진(샌드박스 토큰으로 AI가 쓴 매니페스트를 적용)이 돌기 전에 반드시 끝나고, 실패하면 ErrIsolation으로 멈춘다.
// manifest가 비어 있으면(AI 계획 전에 미리 준비할 때) Quota는 기본값이고, 임시 원본의 이미지·설정은 원본 클러스터 값만 쓴다.
// 실패하면 a.Error에 남기고 (nil, nil)을, 리허설 전체를 멈춰야 하면 오류를 돌려준다. a.Namespace가 있으면 호출자가 지운다.
func prepareSandbox(ctx context.Context, c *Config, ses agentlog.Session, a *Attempt, pl *eplan.Plan, src *plan.SourceCluster, manifest string) (pp *prepared, fatal error) {
	log := func(e agentlog.Event) { ses.Log(ctx, e) }
	failf := func(format string, args ...any) {
		a.Error = mask.String(fmt.Sprintf(format, args...))
		log(agentlog.Event{Type: agentlog.Error, Status: fmt.Sprintf("attempt %d", a.N), Output: a.Error})
	}

	// 1. 샌드박스
	base, err := SandboxConfig(SandboxApp(pl.Source.Namespace), manifest)
	if err != nil {
		return nil, err
	}
	sc, err := sandbox.ForCNPG(ctx, c.Admin, base)
	if err != nil {
		return nil, err
	}
	log(agentlog.Event{Type: agentlog.ToolCall, Tool: "sandbox_create", Input: map[string]any{"attempt": a.N, "cluster": pl.Target.Env}})
	t0 := time.Now()
	sb, err := sandbox.Create(ctx, c.Admin, sc, time.Now())
	if err != nil {
		log(agentlog.Event{Type: agentlog.ToolResult, Tool: "sandbox_create", Status: "error", Output: err.Error()})
		return nil, err
	}
	a.Namespace = sb.Namespace
	log(agentlog.Event{Type: agentlog.ToolResult, Tool: "sandbox_create", Status: "ok", Duration: time.Since(t0),
		Output: map[string]any{"namespace": sb.Namespace, "expiresAt": sb.ExpiresAt, "networkPolicy": "기본 차단 + CNPG 오퍼레이터·API 서버", "quota": fmt.Sprintf("CPU %s, 메모리 %s, Pod 20", base.CPU, base.Memory)}})

	// 2. 샌드박스 토큰과 격리 검사 (백그라운드)
	token, _, err := sandbox.Token(ctx, c.Admin, sb, 0, time.Now())
	if err != nil {
		return nil, err
	}
	aiREST := sandbox.TokenConfig(c.AdminREST, token)
	aiCS, err := kubernetes.NewForConfig(aiREST)
	if err != nil {
		return nil, err
	}
	aiDyn, err := dynamic.NewForConfig(aiREST)
	if err != nil {
		return nil, err
	}
	iso := c.Isolation
	iso.APIServerAllowed = true
	var (
		isoRes  []sandbox.CheckResult
		isoErr  error
		isoDone = make(chan struct{})
	)
	go func() {
		defer close(isoDone)
		isoRes, isoErr = sandbox.CheckIsolation(ctx, c.Admin, aiCS, sb, iso)
	}()
	var once sync.Once
	var isoFatal error
	// waitIsolation은 격리 검사가 끝나길 기다려 기록하고, 깨졌으면 ErrIsolation을 돌려준다. 몇 번 불러도 한 번만 기록한다.
	waitIsolation := func() error {
		once.Do(func() {
			<-isoDone
			a.Isolation = isoRes
			logIsolation(ctx, ses, isoRes)
			if isoErr != nil || !sandbox.AllPass(isoRes) {
				if isoErr != nil {
					failf("격리 검사 실패: %v", isoErr)
				} else {
					failf("격리 검사 실패: %s", strings.Join(sandbox.Failed(isoRes), ", "))
				}
				isoFatal = ErrIsolation
			}
		})
		return isoFatal
	}
	// 어떤 길로 끝나든 격리 검사 Pod가 남지 않게, 그리고 격리가 깨졌으면 그 판정이 먼저 오게 한다.
	defer func() {
		if err := waitIsolation(); err != nil {
			pp.close()
			pp, fatal = nil, err
		}
	}()

	// 3. 임시 원본
	db, err := ParseDB(pl.Source.DSN)
	if err != nil {
		failf("원본 DSN: %v", err)
		return nil, nil
	}
	srcObj := SourceManifest(pl, sb.Namespace, db)
	if err := PrepareTempSource(srcObj, src, manifest); err != nil {
		failf("임시 원본 설정: %v", err)
		return nil, nil
	}
	scs, err := c.Admin.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		failf("샌드박스 클러스터의 StorageClass를 읽을 수 없음: %v", err)
		return nil, nil
	}
	// 임시 원본과 대상이 같은 클래스를 보게 Retain을 바꾼 사본으로 고른다. 바꿀 수 없으면 임시 원본을 만들기 전에 멈춘다.
	sbManifest, _, err := SandboxStorageClasses(manifest, scs.Items)
	if err != nil {
		failf("샌드박스 대상 디스크: %v", err)
		return nil, nil
	}
	tempSC, err := TempStorageClass(scs.Items, sbManifest)
	if err != nil {
		failf("임시 원본 디스크: %v", err)
		return nil, nil
	}
	if tempSC != "" {
		if err := unstructured.SetNestedField(srcObj.Object, tempSC, "spec", "storage", "storageClass"); err != nil {
			failf("임시 원본 설정: %v", err)
			return nil, nil
		}
	}
	log(agentlog.Event{Type: agentlog.ToolCall, Tool: "sandbox_source", Input: map[string]any{"cluster": SandboxSource(pl), "rows": c.Rows, "schemas": pl.Verify.Schemas, "storageClass": tempSC}})
	t0 = time.Now()
	if _, err := aiDyn.Resource(llm.CNPGClusterGVR).Namespace(sb.Namespace).Create(ctx, srcObj, metav1.CreateOptions{}); err != nil {
		failf("임시 원본 생성 실패: %v", err)
		return nil, nil
	}
	if err := waitHealthy(ctx, aiDyn, sb.Namespace, SandboxSource(pl), c.SourceTimeout); err != nil {
		// 샌드박스를 곧 지우므로 원인 단서(Pod·PVC 상태, 경고 이벤트)를 지우기 전에 세션 로그와 리포트에 남긴다.
		var brief string
		a.Evidence, brief = sourceNotReadyEvidence(ctx, aiCS, sb.Namespace)
		logSourceStatus(ctx, ses, a.Evidence)
		if brief != "" {
			failf("임시 원본이 준비되지 않음: %v (%s)", err, brief)
		} else {
			failf("임시 원본이 준비되지 않음: %v", err)
		}
		return nil, nil
	}
	sec, err := aiCS.CoreV1().Secrets(sb.Namespace).Get(ctx, SandboxSource(pl)+"-app", metav1.GetOptions{})
	if err != nil {
		failf("임시 원본 앱 계정을 읽을 수 없음: %v", err)
		return nil, nil
	}
	password := string(sec.Data["password"])
	p := &prepared{sb: sb, token: token, aiCS: aiCS, aiDyn: aiDyn, db: db, tempSC: tempSC}
	if p.srcProxy, err = NewProxy(c.Admin, c.AdminREST, sb.Namespace, SandboxSource(pl)); err != nil {
		return nil, err
	}
	if p.dstProxy, err = NewProxy(c.Admin, c.AdminREST, sb.Namespace, pl.Target.Cluster); err != nil {
		p.close()
		return nil, err
	}
	p.srcDSN, p.dstDSN = DSN(db, password, p.srcProxy.Port()), DSN(db, password, p.dstProxy.Port())
	if err := Seed(ctx, p.srcDSN, pl.Verify.Schemas, c.Rows); err != nil {
		p.close()
		failf("%v", err)
		return nil, nil
	}
	log(agentlog.Event{Type: agentlog.ToolResult, Tool: "sandbox_source", Status: "ok", Duration: time.Since(t0), Output: map[string]any{"cluster": SandboxSource(pl), "rows": c.Rows}})
	return p, nil
}

// raiseQuota는 미리 기본값으로 만든 샌드박스의 Quota를 대상 매니페스트에 맞게 올린다. 줄이지는 않는다.
// Pod가 뜨고 지는 동안 quota 컨트롤러가 status를 계속 고치므로 충돌(409)이 나면 다시 읽어 시도한다.
func raiseQuota(ctx context.Context, cs kubernetes.Interface, ns string, want sandbox.Config) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error { return raiseQuotaOnce(ctx, cs, ns, want) })
}

func raiseQuotaOnce(ctx context.Context, cs kubernetes.Interface, ns string, want sandbox.Config) error {
	q, err := cs.CoreV1().ResourceQuotas(ns).Get(ctx, "sandbox-quota", metav1.GetOptions{})
	if err != nil {
		return err
	}
	changed := false
	for name, v := range map[corev1.ResourceName]string{
		corev1.ResourceRequestsCPU: want.CPU, corev1.ResourceLimitsCPU: want.CPU,
		corev1.ResourceRequestsMemory: want.Memory, corev1.ResourceLimitsMemory: want.Memory,
	} {
		w, err := resource.ParseQuantity(v)
		if err != nil {
			return err
		}
		if cur, ok := q.Spec.Hard[name]; !ok || cur.Cmp(w) < 0 {
			q.Spec.Hard[name] = w
			changed = true
		}
	}
	if !changed {
		return nil
	}
	_, err = cs.CoreV1().ResourceQuotas(ns).Update(ctx, q, metav1.UpdateOptions{})
	return err
}

// Prewarm은 AI가 계획하는 동안 첫 회차의 샌드박스와 임시 원본을 미리 준비한다. 임시 원본은 초안에 있는 값(원본 클러스터
// 이름과 네임스페이스, DSN, 검증 스키마, 대상 클러스터 이름)과 원본 클러스터에서 읽은 이미지·설정만 쓰므로 AI 계획이 필요 없다.
// AI 계획은 이 값들을 바꿀 수 없고(plan 검사), 그래도 다르면 리허설은 미리 준비한 것을 버리고 새로 만든다.
type Prewarm struct {
	plan   eplan.Plan
	c      Config
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	events []agentlog.Event
	a      Attempt
	pp     *prepared
	fatal  error
	taken  bool
}

// StartPrewarm은 미리 준비를 시작한다. 원본 클러스터 값(src)이 없거나 원본 이미지가 CNPG 공식 이미지가 아니면
// (미러·사설 레지스트리) 임시 원본 이미지를 AI 매니페스트에서 가져와야 하므로 시작하지 않고 nil을 돌려준다.
// 그대로 미리 띄우면 오퍼레이터 기본 이미지(다른 메이저 버전일 수 있음)로 떠서 1회차가 버전 불일치로 실패한다.
// c에는 Admin, AdminREST, Isolation, Rows(와 시간 상한)만 쓴다.
func StartPrewarm(ctx context.Context, c Config, draft *eplan.Plan, src *plan.SourceCluster) *Prewarm {
	if src == nil || draft == nil || !plan.IsCNPGImage(src.Image) {
		return nil
	}
	c.defaults()
	pctx, cancel := context.WithCancel(ctx)
	p := &Prewarm{plan: *draft, c: c, cancel: cancel, done: make(chan struct{}), a: Attempt{N: 1}}
	go func() {
		defer close(p.done)
		start := time.Now()
		pp, fatal := prepareSandbox(pctx, &p.c, (*bufSession)(p), &p.a, &p.plan, src, "")
		p.mu.Lock()
		p.pp, p.fatal = pp, fatal
		p.a.DurationS = time.Since(start).Seconds()
		p.mu.Unlock()
	}()
	return p
}

// matches는 계획이 미리 준비할 때 쓴 초안 값과 같은지 본다.
func (p *Prewarm) matches(pl *eplan.Plan) bool {
	return pl.Source.Cluster == p.plan.Source.Cluster && pl.Source.Namespace == p.plan.Source.Namespace &&
		pl.Source.DSN == p.plan.Source.DSN && pl.Target.Cluster == p.plan.Target.Cluster &&
		pl.Target.Env == p.plan.Target.Env && slices.Equal(pl.Verify.Schemas, p.plan.Verify.Schemas)
}

// take는 준비가 끝나길 기다려 결과를 넘긴다. 한 번만 넘기고, 그 뒤의 정리는 받은 쪽이 한다.
func (p *Prewarm) take(ctx context.Context) (Attempt, *prepared, []agentlog.Event, error) {
	select {
	case <-p.done:
	case <-ctx.Done():
		p.cancel()
		<-p.done
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.taken {
		return Attempt{N: 1}, nil, nil, errors.New("미리 준비한 샌드박스를 이미 썼음")
	}
	p.taken = true
	return p.a, p.pp, p.events, p.fatal
}

// Discard는 리허설이 쓰지 않을 때(계획 실패 등) 미리 만든 샌드박스를 지운다. take 뒤에는 아무것도 하지 않는다.
func (p *Prewarm) Discard(ctx context.Context) {
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
	p.mu.Lock()
	if p.taken {
		p.mu.Unlock()
		return
	}
	p.taken = true
	ns, pp := p.a.Namespace, p.pp
	p.mu.Unlock()
	pp.close()
	if ns != "" {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		_ = sandbox.Delete(dctx, p.c.Admin, ns)
	}
}

// bufSession은 리허설 세션이 열리기 전(Plan 제출 전)의 기록을 모아 두었다가, 리허설이 미리 준비한 것을 쓸 때 옮긴다.
type bufSession Prewarm

func (b *bufSession) ID() string { return "" }
func (b *bufSession) Log(_ context.Context, e agentlog.Event) {
	b.mu.Lock()
	b.events = append(b.events, e)
	b.mu.Unlock()
}
func (b *bufSession) Finish(context.Context, bool, string) error { return nil }
