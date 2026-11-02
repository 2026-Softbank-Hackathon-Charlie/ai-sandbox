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
// 예:
//
//	hopai run -app moimroom -from gke -to eks \
//	  -env gke=~/.kube/gke-readonly.yaml -env eks=~/.kube/eks-readonly.yaml \
//	  -sandbox-kubeconfig ~/.kube/eks-sandbox-admin.yaml -target db=moimroom-db-eks-rw.moimroom.svc.cluster.local:5432
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

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
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "사용법: hopai run -app <앱> -from <원본 환경> -to <대상 환경> -env <환경>=<kubeconfig>[@<context>] ... -sandbox-kubeconfig <파일> [옵션]")
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
	out := fs.String("out", "", "리포트 JSON을 쓸 파일 (비우면 표준 출력)")
	must(fs.Parse(os.Args[2:]))
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
	d, err := pc.DraftPlan(ctx, req)
	must(err)
	var draft eplan.Plan
	b, _ := json.Marshal(d)
	must(json.Unmarshal(b, &draft))

	// 2. AI 계획
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
	agent := llm.New(llm.Config{Sink: sink, OnLogError: func(err error) {
		fmt.Fprintln(os.Stderr, "경고: 에이전트 기록 일부를 플랫폼에 남기지 못함:", err)
	}})
	scope := llm.PlanScope{App: *app, Namespace: draft.Source.Namespace, Envs: []string{*from, *to}}
	prop, planSession, err := agent.Plan(ctx, llm.PlanInput{App: *app, Draft: &draft, Tools: llm.PlanTools(scope, pc, clusters)})
	must(err)

	// 3. 제출과 첨부
	rec, err := submit(ctx, pc, prop)
	must(err)
	fmt.Fprintf(os.Stderr, "Plan %s 제출 (revision %d, 계획 세션 %s)\n", rec.Ref, rec.Revision, planSession)

	// 4. 리허설
	adminCfg, err := restConfig(*sbKube, *sbCtx)
	must(err)
	admin, err := kubernetes.NewForConfig(adminCfg)
	must(err)
	if *prodNS == "" {
		*prodNS = draft.Target.Namespace
	}
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
		Isolation:     sandbox.IsolationConfig{ProdNamespace: *prodNS, Targets: targets},
		DiagnoseTools: diagTools,
		Rows:          *rows, MaxRuns: *maxRuns,
		OnAttempt: func(a rehearsal.Attempt) {
			fmt.Fprintf(os.Stderr, "리허설 %d회차 (%s): 통과=%v 엔진=%s %.0f초\n", a.N, a.Namespace, a.Passed, a.Outcome, a.DurationS)
		},
	})

	printRehearsalWarnings(os.Stderr, rep)
	// 5. 수정안 반영과 리포트 첨부
	result := map[string]any{"planRef": rec.Ref, "planSession": planSession}
	if rep != nil {
		result["rehearsal"] = rep
		if rep.Changed {
			if !samePlan(&rep.Final.Plan, &prop.Plan) {
				// 제출한 Plan은 고칠 수 없으므로 새로 제출한다. 앞의 Plan은 운영자가 반려한다(agent 토큰은 반려 못 함).
				nrec, err := submit(ctx, pc, rep.Final)
				must(err)
				fmt.Fprintf(os.Stderr, "Plan 값이 바뀌어 새 Plan %s 제출. 앞의 Plan %s는 반려해 주세요\n", nrec.Ref, rec.Ref)
				result["supersedes"], result["planRef"] = rec.Ref, nrec.Ref
				rec = nrec
			} else {
				_, err := pc.PutPlanAttachment(ctx, rec.Ref, plan.ManifestName, []byte(rep.Final.Manifest))
				must(err)
			}
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
	if rep == nil || !rep.Passed || err != nil {
		fmt.Fprintf(w, "리허설을 통과하지 못했습니다. Plan %s는 승인하지 마세요\n", ref)
		return 1
	}
	fmt.Fprintf(w, "리허설 통과. 콘솔에서 Plan %s (revision %d)를 확인하고 승인하세요\n", ref, revision)
	return 0
}

func submit(ctx context.Context, pc *client.Client, p *plan.Proposal) (*apiv1.PlanRecord, error) {
	raw, err := json.Marshal(p.Plan)
	if err != nil {
		return nil, err
	}
	rec, err := pc.SubmitPlan(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("Plan 제출 실패: %w", err)
	}
	if p.Plan.Target.Manifest == plan.ManifestRef {
		if _, err := pc.PutPlanAttachment(ctx, rec.Ref, plan.ManifestName, []byte(p.Manifest)); err != nil {
			return nil, fmt.Errorf("매니페스트 첨부 실패: %w", err)
		}
	}
	return pc.GetPlan(ctx, rec.Ref)
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
