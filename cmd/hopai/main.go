// hopai는 D 영역의 전체 흐름을 한 번에 돌린다. 사람의 승인 전까지만 하고, 실행은 하지 않는다.
//
//	Plan 초안(플랫폼) → AI 계획(읽기 전용 도구) → Plan 제출 + 대상 매니페스트 첨부
//	→ 대상 클러스터 샌드박스에서 실제 엔진으로 리허설(실패하면 AI 진단과 수정, 최대 3회)
//	→ 고친 매니페스트와 리허설 리포트를 첨부로 올림 → 운영자가 콘솔에서 승인
//
// 비밀값은 환경 변수로만 받는다 (명령줄 옵션은 셸 기록과 프로세스 목록에 남는다).
//
//	ANTHROPIC_API_KEY   Claude API 키
//	PLATFORM_URL        플랫폼 API 주소
//	PLATFORM_TOKEN      플랫폼 agent 토큰 (Plan 제출, 첨부, 에이전트 세션. 승인은 못 함)
//
// 운영 DB 비밀번호는 받지 않는다. 원본 실측은 플랫폼이 초안과 함께 한다 (platform #47).
//
// hopai prepare는 들여온 오픈소스 레포를 AI가 고쳐 hopcharlie 브랜치로 push한다 (prepare.go).
//
// 예:
//
//	hopai run -app moimroom -from gke -to eks \
//	  -env gke=~/.kube/gke-readonly.yaml -env eks=~/.kube/eks-readonly.yaml \
//	  -sandbox-kubeconfig ~/.kube/eks-sandbox-admin.yaml -target db=moimroom-db-eks-rw.moimroom.svc.cluster.local:5432
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/rehearsal"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/sandbox"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "prepare" {
		os.Exit(prepareMain(os.Args[2:]))
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "사용법: hopai run -app <앱> -from <원본 환경> -to <대상 환경> -env <환경>=<kubeconfig>[@<context>] ... -sandbox-kubeconfig <파일> [옵션]")
		fmt.Fprintln(os.Stderr, "        "+strings.TrimPrefix(prepareUsage, "사용법: "))
		os.Exit(2)
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	app := fs.String("app", "", "앱 이름 (플랫폼 PLATFORM_APPS_FILE의 이름)")
	from := fs.String("from", "", "원본 환경 이름")
	to := fs.String("to", "", "대상 환경 이름")
	stream := fs.String("stream", "", "k6 쓰기 기록 스트림 이름 (verify.writtenRecords)")
	var envs envFlags
	fs.Var(&envs, "env", "환경별 읽기 전용 kubeconfig: <환경>=<파일>[@<context>] (AI 조사 도구가 씀, 여러 번)")
	sbKube := fs.String("sandbox-kubeconfig", "", "대상 클러스터의 샌드박스 관리 kubeconfig (샌드박스 생성·삭제, 격리 검사, port-forward)")
	sbCtx := fs.String("sandbox-context", "", "그 kubeconfig의 컨텍스트 (비우면 현재 컨텍스트)")
	prodNS := fs.String("prod-ns", "", "격리 검사에서 읽혀선 안 되는 운영 네임스페이스 (기본: 앱 네임스페이스)")
	var targets targetFlags
	fs.Var(&targets, "target", "샌드박스에서 막혀야 하는 곳 <이름>=<호스트>:<포트> (예: 운영 DB, 여러 번)")
	rows := fs.Int("rows", 20000, "리허설 테스트 데이터 행 수")
	maxRuns := fs.Int("max-runs", 3, "리허설 최대 실행 횟수")
	noMeasure := fs.Bool("no-measure", false, "플랫폼에 원본 실측을 요청하지 않음 (접속 수·초기 복사 검사 없이 계획)")
	out := fs.String("out", "", "리포트 JSON을 쓸 파일 (비우면 표준 출력)")
	demoCopy := fs.Bool("demo-copy-source-storage", false, "데모: AI 계획의 storageClass를 원본 DB가 쓰는 값으로 바꿔 제출한다(원본 설정을 그대로 옮긴 경우). "+
		"대상에 그 StorageClass가 없으면 첫 리허설이 실패하고 AI가 진단해 고친다. 바꾼 사실은 에이전트 기록에 운영자 설정으로 남는다")
	model := fs.String("model", llm.DefaultModel, "계획·진단에 쓸 Claude 모델 ID")
	effort := fs.String("effort", "medium", "AI 생각 깊이: low, medium, high, xhigh, max")
	noPrewarm := fs.Bool("no-prewarm", false, "AI 계획과 동시에 첫 리허설 샌드박스·임시 원본을 미리 준비하지 않음")
	allowOthers := fs.Bool("allow-other-primaries", false, "원본과 같은 네임스페이스에 다른 primary CNPG 클러스터가 있어도 진행한다. 원본이 앱이 실제로 쓰는 DB가 맞을 때만 쓴다")
	must(fs.Parse(os.Args[2:]))
	eff, err := llm.ParseEffort(*effort)
	must(err)
	for name, v := range map[string]string{"-app": *app, "-from": *from, "-to": *to, "-sandbox-kubeconfig": *sbKube} {
		if v == "" {
			must(fmt.Errorf("%s가 필요함", name))
		}
	}
	purl, ptok := os.Getenv("PLATFORM_URL"), os.Getenv("PLATFORM_TOKEN")
	if purl == "" || ptok == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		must(errors.New("환경 변수 PLATFORM_URL, PLATFORM_TOKEN, ANTHROPIC_API_KEY가 필요함"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	pc := client.New(purl, ptok)
	me, err := pc.Me(ctx)
	must(err)
	if me.Role != "agent" && me.Role != "operator" {
		must(fmt.Errorf("토큰 역할이 %s임 (agent 필요)", me.Role))
	}

	// 1. Plan 초안
	req := apiv1.PlanDraftRequest{App: *app, Source: *from, Target: *to}
	if *stream != "" {
		req.WrittenRecordsStream = stream
	}
	// 2. 원본 실측. 플랫폼은 이전을 시작할 때 같은 값을 재고 부족하면 막는다(409 capacity_insufficient).
	// 승인 뒤에 막히지 않도록 초안을 받을 때 플랫폼에 같은 값을 재 달라고 한다(platform #47, ADR 0017).
	// 이 실행기는 운영 DB에 붙지 않고 비밀번호도 갖지 않는다. 못 재면 용량 검사 없이 계획하고, 플랫폼이 시작할 때 잰다.
	d, stats, err := draftWithStats(ctx, pc, req, !*noMeasure, os.Stderr)
	must(err)
	var draft eplan.Plan
	b, _ := json.Marshal(d)
	must(json.Unmarshal(b, &draft))

	// 3. AI 계획
	clusters := map[string]llm.Cluster{}
	for _, e := range envs {
		cfg, err := restConfig(e.path, e.context)
		must(err)
		cs, err := kubernetes.NewForConfig(cfg)
		must(err)
		dyn, err := dynamic.NewForConfig(cfg)
		must(err)
		clusters[e.name] = llm.Cluster{Typed: cs, Dynamic: dyn}
	}
	sink := agentlog.Tee{agentlog.Platform{C: pc}, agentlog.Writer{W: os.Stderr}}
	agent := llm.New(llm.Config{Model: *model, Effort: eff, Sink: sink, OnLogError: func(err error) {
		fmt.Fprintln(os.Stderr, "경고: 에이전트 기록 일부를 플랫폼에 남기지 못함:", err)
	}})
	// 원본 CNPG 클러스터의 이미지와 hot standby 설정. 대상은 원본의 물리 복제본이라 이 값들이 원본과 맞아야 하고,
	// 리허설은 임시 원본을 이 값으로 띄워 실제 원본을 흉내 낸다.
	// 원본 환경의 읽기 전용 kubeconfig(-env)가 없거나 읽지 못하면 이 검사 없이 계획한다.
	var src *plan.SourceCluster
	if c, ok := clusters[*from]; !ok {
		fmt.Fprintf(os.Stderr, "경고: 원본 환경 %s의 -env kubeconfig가 없어 원본 PostgreSQL 버전과 설정을 모름. 이 검사 없이 계획합니다\n", *from)
	} else if sc, err := sourceCluster(ctx, c.Dynamic, draft.Source.Namespace, draft.Source.Cluster); err != nil {
		fmt.Fprintf(os.Stderr, "경고: 원본 CNPG 클러스터를 읽지 못함 (%v). 버전·설정 검사 없이 계획합니다\n", err)
	} else {
		// 원본이 replica이거나 같은 네임스페이스에 다른 primary가 있으면 카탈로그가 낡은 것일 수 있다. 승인 전에 멈춘다.
		if err := plan.CheckSourceRole(sc, *allowOthers); err != nil {
			must(fmt.Errorf("원본 검사 실패:\n%w", err))
		}
		if sc.OthersUnknown {
			fmt.Fprintf(os.Stderr, "경고: %s 네임스페이스의 다른 CNPG 클러스터 목록을 읽지 못해 원본이 맞는 primary인지 확인하지 못함\n", sc.Namespace)
		}
		src = sc
		fmt.Fprintf(os.Stderr, "원본 클러스터: %s/%s %s (%s), hot standby 설정 %v\n", sc.Namespace, sc.Name, sc.Image, plan.MajorLabel(sc.Image), sc.Parameters)
	}
	// 리허설이 도는 대상 클러스터의 관리용 접속. 데모 준비도 이것으로 대상 StorageClass를 확인한다.
	adminCfg, err := restConfig(*sbKube, *sbCtx)
	must(err)
	admin, err := kubernetes.NewForConfig(adminCfg)
	must(err)
	if *prodNS == "" {
		*prodNS = draft.Target.Namespace
	}
	isolation := sandbox.IsolationConfig{ProdNamespace: *prodNS, Targets: targets}
	// AI가 계획하는 동안 첫 리허설의 샌드박스, 격리 검사, 임시 원본을 미리 준비한다(원본 클러스터 값을 읽었을 때만).
	var prewarm *rehearsal.Prewarm
	if !*noPrewarm {
		prewarm = rehearsal.StartPrewarm(ctx, rehearsal.Config{Admin: admin, AdminREST: adminCfg, Isolation: isolation, Rows: *rows}, &draft, src)
		if prewarm != nil {
			fmt.Fprintln(os.Stderr, "AI 계획과 동시에 첫 리허설 샌드박스와 임시 원본을 준비합니다")
		}
	}

	scope := llm.PlanScope{App: *app, Namespace: draft.Source.Namespace, Envs: []string{*from, *to}}
	prop, planSession, err := agent.Plan(ctx, llm.PlanInput{App: *app, Draft: &draft, Tools: llm.PlanTools(scope, pc, clusters), Source: stats, SourceCluster: src})
	if err != nil {
		prewarm.Discard(ctx)
	}
	must(err)

	// 데모 실패 시나리오(README 9장): AI 계획의 storageClass를 원본 값으로 바꿔 제출한다.
	// 실패가 나지 않을 조건(AI가 이미 원본 값을 골랐거나 대상에도 그 값이 있음)이면 시작하지 않는다.
	var demo *demoSetting
	if *demoCopy {
		c, ok := clusters[*from]
		if !ok {
			err = fmt.Errorf("-demo-copy-source-storage에는 원본 환경 %s의 -env kubeconfig가 필요함", *from)
		} else {
			prop, demo, err = prepareDemo(ctx, prop, &draft, c, admin, draft.Source.Namespace, draft.Source.Cluster)
		}
		if err != nil {
			prewarm.Discard(ctx) // os.Exit 전에 미리 만든 샌드박스를 지운다
		}
		must(err)
		fmt.Fprintf(os.Stderr, "데모 설정: AI가 정한 storageClass(%s)를 원본 값 %q로 바꿔 제출합니다. 대상에 없어 첫 리허설이 실패합니다\n", scLabel(demo.AIStorageClass), demo.SourceStorageClass)
	}

	// 4. 제출과 첨부
	rec, submitted, err := submit(ctx, pc, prop)
	if err != nil {
		prewarm.Discard(ctx)
	}
	must(err)
	fmt.Fprintf(os.Stderr, "Plan %s 제출 (revision %d, 계획 세션 %s)\n", rec.Ref, rec.Revision, planSession)
	// AI가 쓴 Plan·대상 매니페스트·근거를 리포트 옆에 바로 남긴다. 플랫폼에는 첨부를 내려받는 API가 없고,
	// 리허설이 실패하거나 중간에 멈춰도 무엇을 리허설했는지 볼 수 있어야 한다. 비밀값은 가린다.
	if *out != "" {
		if path, err := saveProposal(*out, prop); err != nil {
			fmt.Fprintln(os.Stderr, "경고: AI 계획을 파일로 남기지 못함:", err)
		} else {
			fmt.Fprintln(os.Stderr, "AI 계획:", path)
		}
	}
	if demo != nil {
		if err := logDemoSetting(ctx, sink, rec.Ref, *demo); err != nil {
			fmt.Fprintln(os.Stderr, "경고: 데모 설정을 에이전트 기록에 남기지 못함:", err)
		}
	}

	// 5. 리허설
	diagScope := llm.PlanScope{App: *app, Namespace: draft.Target.Namespace, Envs: []string{*to}}
	var diagTools []llm.Tool
	for _, t := range llm.PlanTools(diagScope, nil, clusters) {
		if t.Name == "list_storage_classes" || t.Name == "node_capacity" {
			diagTools = append(diagTools, t)
		}
	}
	rep, rerr := rehearsal.Rehearse(ctx, rehearsal.Config{
		Admin: admin, AdminREST: adminCfg,
		Proposal: prop, Draft: &draft, PlanRef: rec.Ref, PlanSession: planSession,
		Diagnoser: agent, Sink: sink,
		Isolation:     isolation,
		Prewarm:       prewarm,
		DiagnoseTools: diagTools,
		Rows:          *rows, MaxRuns: *maxRuns,
		OnAttempt: func(a rehearsal.Attempt) {
			fmt.Fprintf(os.Stderr, "리허설 %d회차 (%s): 통과=%v 엔진=%s %.0f초\n", a.N, a.Namespace, a.Passed, a.Outcome, a.DurationS)
		},
	})

	printRehearsalWarnings(os.Stderr, rep)
	// 6. 수정안 반영과 리포트 첨부
	result := map[string]any{"planRef": rec.Ref, "planSession": planSession}
	if stats != nil {
		result["sourceStats"] = stats
	}
	if src != nil {
		result["sourceCluster"] = src
	}
	if demo != nil {
		result["demo"] = demo
	}
	if rep != nil {
		result["rehearsal"] = rep
		// 수정안은 리허설을 통과했을 때만 올린다. 통과하지 못한 수정안이 승인 대상 revision이 되면 안 된다.
		if rep.Changed && rep.Passed && rerr == nil {
			if !samePlan(&rep.Final.Plan, &prop.Plan) {
				// 제출한 Plan은 고칠 수 없으므로 새로 제출한다. 앞의 Plan은 운영자가 반려한다(agent 토큰은 반려 못 함).
				nrec, nsub, err := submit(ctx, pc, rep.Final)
				must(err)
				fmt.Fprintf(os.Stderr, "Plan 값이 바뀌어 새 Plan %s 제출. 앞의 Plan %s는 반려해 주세요\n", nrec.Ref, rec.Ref)
				result["supersedes"], result["planRef"] = rec.Ref, nrec.Ref
				rec, submitted = nrec, nsub
			} else {
				_, err := pc.PutPlanAttachment(ctx, rec.Ref, plan.ManifestName, []byte(rep.Final.Manifest))
				must(err)
			}
		}
		// 승인 게이트(platform #24)가 비교할 값을 올리기 전에 같은지 본다. 다르면 리포트는 남기되 승인 대상이 아니다.
		if rep.Passed && rerr == nil {
			cur, err := pc.GetPlan(ctx, rec.Ref)
			must(err)
			rerr = matchCandidate(rep.Candidate, submitted, cur)
		}
		rb, _ := json.MarshalIndent(rep, "", "  ")
		_, err := pc.PutPlanAttachment(ctx, rec.Ref, "rehearsal-report.json", rb)
		must(err)
	}
	final, err := pc.GetPlan(ctx, rec.Ref)
	must(err)
	result["revision"] = final.Revision
	if rerr != nil {
		result["error"] = rerr.Error()
	}
	enc, _ := json.MarshalIndent(result, "", "  ")
	if *out != "" {
		must(os.WriteFile(*out, enc, 0o600))
	} else {
		fmt.Println(string(enc))
	}
	if code := reportRehearsal(os.Stderr, rep, rerr, rec.Ref, final.Revision); code != 0 {
		os.Exit(code)
	}
}

func printRehearsalWarnings(w io.Writer, rep *rehearsal.Report) {
	if rep != nil {
		for _, warning := range rep.Warnings {
			fmt.Fprintf(w, "경고: %s\n", mask.String(warning))
		}
	}
}

func reportRehearsal(w io.Writer, rep *rehearsal.Report, err error, ref string, revision int64) int {
	if rep != nil && rep.Passed && errors.Is(err, errCandidate) {
		// 리포트에는 passed가 남으므로 리허설 실패로 안내하면 원인을 잘못 읽는다.
		fmt.Fprintf(w, "리허설은 통과했지만 플랫폼에 올라간 후보가 리허설한 것과 다릅니다 (%v). Plan %s는 승인하지 마세요\n", err, ref)
		return 1
	}
	if rep == nil || !rep.Passed || err != nil {
		fmt.Fprintf(w, "리허설을 통과하지 못했습니다. Plan %s는 승인하지 마세요\n", ref)
		return 1
	}
	fmt.Fprintf(w, "리허설 통과. 콘솔에서 Plan %s (revision %d)를 확인하고 승인하세요\n", ref, revision)
	return 0
}

// submit은 Plan을 제출하고 매니페스트를 첨부한다. 돌려주는 해시는 실제로 보낸 본문의 것이다.
func submit(ctx context.Context, pc *client.Client, p *plan.Proposal) (*apiv1.PlanRecord, string, error) {
	raw, err := p.SubmitBody()
	if err != nil {
		return nil, "", err
	}
	rec, err := pc.SubmitPlan(ctx, raw)
	if err != nil {
		return nil, "", fmt.Errorf("Plan 제출 실패: %w", err)
	}
	if p.ManifestAttached() {
		if _, err := pc.PutPlanAttachment(ctx, rec.Ref, plan.ManifestName, []byte(p.Manifest)); err != nil {
			return nil, "", fmt.Errorf("매니페스트 첨부 실패: %w", err)
		}
	}
	rec, err = pc.GetPlan(ctx, rec.Ref)
	return rec, plan.SHA256Hex(raw), err
}

// errCandidate는 플랫폼에 올라간 Plan이나 첨부가 리허설한 후보와 다를 때다.
var errCandidate = errors.New("후보 불일치")

// matchCandidate는 리허설한 후보가 플랫폼에 올라간 Plan 본문(submitted)과 target.yaml 첨부와 같은지 본다.
func matchCandidate(c plan.Digest, submitted string, rec *apiv1.PlanRecord) error {
	if c.PlanSHA256 != submitted {
		return fmt.Errorf("%w: 리허설한 Plan(%.12s)과 제출한 Plan %s(%.12s)이 다름", errCandidate, c.PlanSHA256, rec.Ref, submitted)
	}
	attached := ""
	if rec.Attachments != nil {
		for _, a := range *rec.Attachments {
			if a.Name == plan.ManifestName {
				attached = a.Sha256
			}
		}
	}
	if c.ManifestSHA256 != attached {
		return fmt.Errorf("%w: 리허설한 매니페스트(%.12s)와 Plan %s의 %s 첨부(%.12s)가 다름", errCandidate, c.ManifestSHA256, rec.Ref, plan.ManifestName, attached)
	}
	return nil
}

func samePlan(a, b *eplan.Plan) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func restConfig(path, context string) (*rest.Config, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
		&clientcmd.ConfigOverrides{CurrentContext: context},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 30 * time.Second
	return cfg, nil
}

type envFlag struct{ name, path, context string }
type envFlags []envFlag

func (e *envFlags) String() string { return fmt.Sprint(*e) }
func (e *envFlags) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return fmt.Errorf("<환경>=<kubeconfig>[@<context>] 형식이어야 함: %q", v)
	}
	path, ctx, _ := strings.Cut(rest, "@")
	if slices.ContainsFunc(*e, func(x envFlag) bool { return x.name == name }) {
		return fmt.Errorf("환경 %q가 두 번 나옴", name)
	}
	*e = append(*e, envFlag{name, path, ctx})
	return nil
}

type targetFlags []sandbox.Target

func (t *targetFlags) String() string { return fmt.Sprint(*t) }
func (t *targetFlags) Set(v string) error {
	name, addr, ok := strings.Cut(v, "=")
	i := strings.LastIndex(addr, ":")
	if !ok || i < 0 {
		return fmt.Errorf("<이름>=<호스트>:<포트> 형식이어야 함: %q", v)
	}
	var port int
	if _, err := fmt.Sscanf(addr[i+1:], "%d", &port); err != nil {
		return fmt.Errorf("포트가 숫자가 아님: %q", v)
	}
	*t = append(*t, sandbox.Target{Name: name, Host: addr[:i], Port: port, Expect: sandbox.ExpectBlocked})
	return nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "오류:", err)
		os.Exit(1)
	}
}

// sourceStats는 플랫폼이 잰 원본 값을 계획 검사의 형식으로 바꾼다. 재지 못했으면 nil이다.
// 크기와 WAL 속도는 플랫폼이 못 잰 경우 빠지는데, 그때는 0으로 두어 초기 복사 검사가 막지 않게 한다.
func sourceStats(res *apiv1.PlanDraftResult) *plan.SourceStats {
	s := res.SourceStats
	if s == nil {
		return nil
	}
	out := &plan.SourceStats{Sessions: s.Sessions, MaxConnections: s.MaxConnections, MaxWorkerProcesses: s.MaxWorkerProcesses, WindowSeconds: s.WindowSeconds}
	if s.Bytes != nil {
		out.Bytes = *s.Bytes
	}
	if s.WalBytesPerSecond != nil {
		out.WALBytesPerSecond = *s.WalBytesPerSecond
	}
	return out
}

// reasonOf는 플랫폼이 원본을 재지 못한 이유다 (not_measured: 비밀번호 변수 없음, 닿지 못함, 쿼리 실패).
func reasonOf(res *apiv1.PlanDraftResult) string {
	if res.SourceStatsReason != nil {
		return string(*res.SourceStatsReason)
	}
	return "이유 없음"
}

// drafter는 플랫폼의 Plan 초안 API다 (*client.Client).
type drafter interface {
	DraftPlan(context.Context, apiv1.PlanDraftRequest) (*apiv1.EnginePlan, error)
	DraftPlanWithSourceStats(context.Context, apiv1.PlanDraftRequest) (*apiv1.PlanDraftResult, error)
}

// draftWithStats는 Plan 초안을 받는다. measure면 플랫폼에 원본 실측도 같이 요청한다.
// 플랫폼이 실측을 모르면(platform #47 이전) 모르는 필드로 거절하거나, 예전 형식(Plan 자체)으로 답해 Plan이 비어 읽힌다.
// 그럴 때는 실측 없는 초안으로 다시 받고 용량 검사 없이 계획한다. 플랫폼이 시작할 때 다시 잰다.
func draftWithStats(ctx context.Context, pc drafter, req apiv1.PlanDraftRequest, measure bool, w io.Writer) (*apiv1.EnginePlan, *plan.SourceStats, error) {
	if measure {
		res, err := pc.DraftPlanWithSourceStats(ctx, req)
		switch {
		case err != nil:
			fmt.Fprintf(w, "경고: 원본 실측과 함께 초안을 받지 못함 (%v). 실측 없이 다시 받습니다\n", err)
		case res.Plan.Id == "":
			fmt.Fprintln(w, "경고: 플랫폼이 원본 실측을 지원하지 않음 (응답에 Plan이 없음). 실측 없이 다시 받습니다")
		default:
			stats := sourceStats(res)
			if stats == nil {
				fmt.Fprintf(w, "경고: 플랫폼이 원본을 재지 못함 (%s). 접속 수·초기 복사 검사 없이 계획합니다\n", reasonOf(res))
			} else {
				fmt.Fprintf(w, "원본 실측: 세션 %d, max_connections %d (대상 %d 이상 필요), max_worker_processes %d, 크기 %d바이트, 변경 %.0f바이트/초\n",
					stats.Sessions, stats.MaxConnections, stats.RequiredConnections(), stats.MaxWorkerProcesses, stats.Bytes, stats.WALBytesPerSecond)
			}
			return &res.Plan, stats, nil
		}
	}
	d, err := pc.DraftPlan(ctx, req)
	return d, nil, err
}

// sourceCluster는 원본 CNPG 클러스터에서 이미지와 hot standby 설정을 읽는다.
// 이미지는 실제로 도는 것(status.image)을 먼저 보고, 없으면 spec.imageName을 본다.
func sourceCluster(ctx context.Context, dyn dynamic.Interface, ns, name string) (*plan.SourceCluster, error) {
	obj, err := dyn.Resource(llm.CNPGClusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	sc := &plan.SourceCluster{Name: name, Namespace: ns}
	sc.Replica, _, _ = unstructured.NestedBool(obj.Object, "spec", "replica", "enabled")
	// 같은 네임스페이스의 다른 primary. 카탈로그가 낡은 DB를 가리키는 사고를 승인 전에 잡는 근거다.
	// 목록을 읽지 못하면(권한 등) 이 검사만 건너뛰고 알린다.
	if list, err := dyn.Resource(llm.CNPGClusterGVR).Namespace(ns).List(ctx, metav1.ListOptions{}); err != nil {
		sc.OthersUnknown = true
	} else {
		for _, o := range list.Items {
			if replica, _, _ := unstructured.NestedBool(o.Object, "spec", "replica", "enabled"); o.GetName() != name && !replica {
				sc.OtherPrimaries = append(sc.OtherPrimaries, o.GetName())
			}
		}
		slices.Sort(sc.OtherPrimaries)
	}
	if s, _, _ := unstructured.NestedString(obj.Object, "status", "image"); s != "" {
		sc.Image = s
	} else if s, _, _ := unstructured.NestedString(obj.Object, "spec", "imageName"); s != "" {
		sc.Image = s
	} else {
		return nil, errors.New("status.image와 spec.imageName이 모두 비어 있음")
	}
	params, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "postgresql", "parameters")
	for _, k := range plan.StandbyParams {
		if v, ok := params[k]; ok {
			if sc.Parameters == nil {
				sc.Parameters = map[string]string{}
			}
			sc.Parameters[k] = v
		}
	}
	return sc, nil
}

// saveProposal은 AI 계획을 리포트 파일 옆(<리포트 이름>-proposal.json)에 비밀값을 가려 쓴다.
func saveProposal(out string, prop *plan.Proposal) (string, error) {
	b, err := mask.JSON(prop)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		return "", err
	}
	path := strings.TrimSuffix(out, filepath.Ext(out)) + "-proposal.json"
	return path, os.WriteFile(path, buf.Bytes(), 0o600)
}
