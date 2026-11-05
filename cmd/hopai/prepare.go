package main

// hopai prepare: 들여온 오픈소스 레포를 AI가 고쳐 플랫폼에 배포할 수 있게 만들고 hopcharlie 브랜치로 push한다.
//
//	플랫폼에서 레포 하나짜리 푸시 토큰 받기(POST /v1/github/push-tokens, platform ADR 0023)
//	→ git으로 클론(hopcharlie 브랜치가 있으면 그 위에서, 없으면 기본 브랜치에서 새로)
//	→ (-deployment) 지난 배포의 실패 줄을 가려서 AI에 넣음
//	→ AI가 클론 안의 파일만 읽고 씀(llm.Prepare): platform.yaml과 Dockerfile
//	→ 검사를 통과하면 고정 작성자로 커밋하고 hopcharlie에 fast-forward로만 push
//	→ 운영자가 할 일 출력(시크릿 넣기, 앱 등록 또는 자동 재배포)
//
// AI는 명령을 실행하지 않고 토큰을 보지 않는다. 커밋과 push는 이 실행기가 한다. 앱 등록(charlie app add)은 운영자만 한다.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

// prepareBranch는 AI가 고친 것을 올리는 브랜치다. 운영자는 이 브랜치로 앱을 등록하고, 이후 push는 자동 재배포된다.
const prepareBranch = "hopcharlie"

const prepareUsage = "사용법: hopai prepare -repo OWNER/NAME [-deployment DEP_ID]"

// prepareMain은 hopai prepare의 진입점이다. 종료 코드는 hopai run과 같다 (성공 0, 실패 1, 사용법 2).
func prepareMain(args []string) int {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	repo := fs.String("repo", "", "들여온 레포 OWNER/NAME (플랫폼 scripts/import-repo.sh로 만든 비공개 사본)")
	dep := fs.String("deployment", "", "지난 배포 ID. 주면 그 배포의 실패 줄을 AI에 넣는다")
	if err := fs.Parse(args); err != nil || *repo == "" || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, prepareUsage)
		return 2
	}
	purl, ptok := os.Getenv("PLATFORM_URL"), os.Getenv("PLATFORM_TOKEN")
	if purl == "" || ptok == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "오류: 환경 변수 PLATFORM_URL, PLATFORM_TOKEN, ANTHROPIC_API_KEY가 필요함")
		return 1
	}
	// Ctrl-C나 종료 신호를 받으면 git과 AI 호출을 멈추고 임시 클론을 지운 뒤 끝낸다
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	pc := client.New(purl, ptok)
	sink := agentlog.Tee{agentlog.Platform{C: pc}, agentlog.Writer{W: os.Stderr}}
	agent := llm.New(llm.Config{
		Sink: sink,
		// 조사, 파일 쓰기, 답까지 한 번에 하므로 계획보다 호출이 많다
		MaxIterations: 30,
		OnLogError: func(err error) {
			fmt.Fprintln(os.Stderr, "경고: 에이전트 기록 일부를 플랫폼에 남기지 못함:", err)
		},
	})
	err := runPrepare(ctx, prepareEnv{
		pc:        pc,
		tokens:    platformPushTokens{base: purl, token: ptok, hc: http.DefaultClient},
		agent:     agent,
		github:    "https://github.com",
		githubAPI: "https://api.github.com",
		stdout:    os.Stdout, stderr: os.Stderr,
	}, *repo, *dep)
	if err != nil {
		fmt.Fprintln(os.Stderr, "오류:", err)
		return 1
	}
	return 0
}

// prepareEnv는 hopai prepare가 쓰는 바깥 세계다. 테스트는 가짜 AI·플랫폼과 로컬 git 원격을 넣는다.
type prepareEnv struct {
	pc     *client.Client
	tokens pushTokenIssuer
	agent  *llm.Agent
	// github는 클론과 push 주소의 앞부분이다 (<github>/<OWNER>/<NAME>.git). 실제로는 https://github.com이다.
	github string
	// githubAPI는 클론 전에 레포 크기를 묻는 GitHub API 주소다. 실제로는 https://api.github.com이다.
	githubAPI      string
	stdout, stderr io.Writer
	// trace는 테스트가 git 명령줄을 본다.
	trace func(args []string)
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)

func validRepo(repo string) bool {
	return repoName.MatchString(repo) && !strings.HasSuffix(repo, "/.") && !strings.HasSuffix(repo, "/..")
}

func runPrepare(ctx context.Context, env prepareEnv, requested, depID string) error {
	if !validRepo(requested) {
		return fmt.Errorf("-repo는 OWNER/NAME이어야 함: %q", requested)
	}
	me, err := env.pc.Me(ctx)
	if err != nil {
		return err
	}
	if me.Role != "agent" && me.Role != "operator" {
		return fmt.Errorf("토큰 역할이 %s임 (agent 필요)", me.Role)
	}

	// 1. 푸시 토큰. 레포 하나에만 쓸 수 있고 곧 만료된다. 이 프로세스 메모리와 git 자식 프로세스 환경에만 둔다.
	// 플랫폼은 GitHub의 지금 레포 이름을 돌려준다. 이름이 바뀐 레포면 그 이름으로 클론한다.
	tok, err := env.tokens.PushToken(ctx, requested)
	if err != nil {
		return err
	}
	repo := tok.Repo
	if !strings.EqualFold(repo, requested) {
		fmt.Fprintf(env.stderr, "레포 %s의 지금 이름은 %s입니다. 그 이름으로 클론합니다\n", requested, repo)
	}
	fmt.Fprintf(env.stderr, "푸시 토큰 받음 (%s, 만료 %s)\n", repo, tok.ExpiresAt.Local().Format("15:04"))
	sameRepo := func(r string) bool { return strings.EqualFold(r, repo) || strings.EqualFold(r, requested) }

	// 2. 지난 배포의 실패 줄. 그 배포가 이 레포의 앱 것인지 확인하지 못하면 멈춘다.
	var failure string
	if depID != "" {
		d, lines, err := lastDeployment(ctx, env.pc, depID)
		if err != nil {
			return err
		}
		app, err := env.pc.GetApp(ctx, d.App)
		switch {
		case err != nil:
			return fmt.Errorf("배포 %s의 앱 %s를 읽지 못해 이 레포의 배포인지 확인할 수 없음: %w", d.Id, d.App, err)
		case app.Deploy == nil:
			return fmt.Errorf("배포 %s의 앱 %s에 GitHub 배포 정보가 없어 이 레포의 배포인지 확인할 수 없음", d.Id, d.App)
		case !sameRepo(app.Deploy.Repo):
			return fmt.Errorf("배포 %s는 레포 %s의 앱 %s 것임 (-repo %s)", d.Id, app.Deploy.Repo, d.App, requested)
		}
		if d.Status != apiv1.DeploymentStatusFailed {
			fmt.Fprintf(env.stderr, "경고: 배포 %s는 실패하지 않았음 (상태 %s). 기록은 그대로 AI에 넣습니다\n", d.Id, d.Status)
		}
		failure = lines
		fmt.Fprintf(env.stderr, "지난 배포 %s (%s)의 기록 %d줄을 AI에 넣습니다\n", d.Id, d.Status, strings.Count(lines, "\n")+1)
	}

	// 3. 클론. 끝나면(실패, Ctrl-C 포함) 지운다.
	tmp, err := os.MkdirTemp("", "hopai-prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	g, err := newGitRunner(tmp, env.github, tok.Token)
	if err != nil {
		return err
	}
	g.trace = env.trace
	// 너무 큰 레포는 받기 전에 거른다. 크기를 모르면 받은 뒤에 본다.
	if size, err := repoSize(ctx, env.githubAPI, repo, tok.Token); err != nil {
		fmt.Fprintf(env.stderr, "경고: 클론 전에 레포 크기를 알지 못함 (%v). 클론한 뒤 크기를 봅니다\n", mask.String(err.Error()))
	} else if size > maxTreeBytes {
		return fmt.Errorf("레포 %s가 %d MB로 상한 %d MB를 넘음. 이렇게 큰 레포는 다루지 않음", repo, size>>20, maxTreeBytes>>20)
	}
	dir := filepath.Join(tmp, "repo")
	url := strings.TrimRight(env.github, "/") + "/" + repo + ".git"
	exists, err := g.remoteHasBranch(ctx, tmp, url, prepareBranch)
	if err != nil {
		return fmt.Errorf("레포 %s를 볼 수 없음: %w", repo, err)
	}
	base, err := g.clone(ctx, tmp, dir, url, prepareBranch, exists)
	if err != nil {
		return fmt.Errorf("레포 %s를 클론하지 못함: %w", repo, err)
	}
	if err := treeSize(dir, maxTreeBytes); err != nil {
		return err
	}
	index, err := g.index(ctx, dir)
	if err != nil {
		return err
	}
	how := "기본 브랜치에서 새로 만듦"
	if exists {
		how = "있던 브랜치에 이어서"
	}
	fmt.Fprintf(env.stderr, "클론: %s %s (%s, %.12s)\n", repo, prepareBranch, how, base)

	// 4. AI
	prep, session, err := env.agent.Prepare(ctx, llm.PrepareInput{Repo: repo, Branch: prepareBranch, Continue: exists, Dir: dir, LastDeployment: failure, Index: index})
	if err != nil {
		return fmt.Errorf("AI 배포 준비 실패 (세션 %s): %w", session, err)
	}

	// 5. 커밋, 검사한 바이트와 같은지 확인, push. 검사를 통과한 뒤에만 한다.
	sha, changes, err := g.commit(ctx, dir, prep.CommitMessage+"\n\nhopai prepare, AI 세션 "+session+"\n")
	if err != nil {
		return err
	}
	if err := g.verifyCommit(ctx, dir, changes, prep.Validated, prep.Files, append([]string{appspec.FileName}, prep.Dockerfiles...)); err != nil {
		return err
	}
	if time.Until(tok.ExpiresAt) < 5*time.Minute {
		fresh, err := env.tokens.PushToken(ctx, repo)
		if err != nil {
			return err
		}
		if !strings.EqualFold(fresh.Repo, repo) {
			return fmt.Errorf("새로 받은 푸시 토큰의 레포(%s)가 클론한 레포(%s)와 다름", fresh.Repo, repo)
		}
		g.token = fresh.Token
	}
	if err := g.push(ctx, dir, prepareBranch); err != nil {
		return err
	}

	// 6. 운영자가 할 일
	var apps *apiv1.AppList
	if apps, err = env.pc.ListApps(ctx); err != nil {
		fmt.Fprintf(env.stderr, "경고: 등록된 앱을 읽지 못함 (%v)\n", mask.String(err.Error()))
	}
	printPrepared(env.stdout, prepared{repo: repo, existed: exists, base: base, sha: sha, session: session, changes: changes, prep: prep, apps: apps, sameRepo: sameRepo})
	return nil
}

type prepared struct {
	repo      string
	existed   bool
	base, sha string
	session   string
	changes   []gitChange
	prep      *llm.Preparation
	apps      *apiv1.AppList
	// sameRepo는 앱의 레포가 이 레포인지 본다 (이름이 바뀐 레포는 전 이름도).
	sameRepo func(string) bool
}

// printable은 AI나 레포에서 온 글을 터미널에 찍기 전에 제어 문자와 서식 문자(양방향 글자 등)를 뺀다.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			return -1
		}
		return r
	}, s)
}

// printPrepared는 결과와 운영자가 할 일을 출력한다.
func printPrepared(w io.Writer, r prepared) {
	if r.sameRepo == nil {
		r.sameRepo = func(x string) bool { return strings.EqualFold(x, r.repo) }
	}
	app, auto, other := findApp(r.apps, r.sameRepo, prepareBranch)
	name := r.prep.Spec.Name
	if app != nil {
		name = app.Name
	}
	how := "새로 만듦"
	if r.existed {
		how = "이어서 고침"
	}
	fmt.Fprintf(w, "레포: %s\n브랜치: %s (%s)\n커밋: %s (%.12s 다음)\nAI 세션: %s\n앱 이름: %s\n",
		r.repo, prepareBranch, how, r.sha, r.base, r.session, name)
	if name != r.prep.Spec.Name {
		fmt.Fprintf(w, "경고: platform.yaml의 name(%s)이 등록된 앱 이름(%s)과 다릅니다. 플랫폼이 배포를 거절할 수 있습니다\n", r.prep.Spec.Name, name)
	}
	fmt.Fprintln(w, "바꾼 파일:")
	for _, c := range r.changes {
		fmt.Fprintln(w, "  "+printable(c.String()))
	}
	fmt.Fprintln(w, "요약:", printable(r.prep.Summary))
	if len(r.prep.Notes) > 0 {
		fmt.Fprintln(w, "메모:")
		for _, n := range r.prep.Notes {
			fmt.Fprintln(w, "  - "+printable(n))
		}
	}

	fmt.Fprintln(w, "\n다음 단계:")
	switch {
	case app != nil && auto:
		fmt.Fprintf(w, "  앱 %s가 이 레포의 %s 브랜치를 배포합니다. push가 자동 재배포를 시작했습니다.\n", name, prepareBranch)
	case app != nil && other == "":
		fmt.Fprintf(w, "  앱 %s는 %s 브랜치의 자동 배포가 꺼져 있습니다. 배포하려면:\n  charlie deploy %s --branch %s --watch\n", name, prepareBranch, name, prepareBranch)
	case app != nil:
		fmt.Fprintf(w, "  앱 %s는 이 레포의 %s 브랜치를 배포합니다. %s 브랜치도 배포하려면:\n  charlie app branch add %s %s --watch\n", name, other, prepareBranch, name, prepareBranch)
	case r.apps == nil:
		fmt.Fprintf(w, "  등록된 앱을 확인하지 못했습니다. 이 레포의 %s 브랜치를 배포하는 앱이 있으면 push가 자동 재배포를 시작합니다. 없으면 운영자가 등록합니다:\n  charlie app add --repo %s --branch %s --env eks --watch\n", prepareBranch, r.repo, prepareBranch)
	default:
		fmt.Fprintf(w, "  운영자가 앱을 등록합니다 (operator 역할). 이후 %s 브랜치에 push하면 자동으로 다시 배포됩니다.\n  charlie app add --repo %s --branch %s --env eks --watch\n", prepareBranch, r.repo, prepareBranch)
	}
	if len(r.prep.Secrets) > 0 {
		fmt.Fprintln(w, "  앱이 쓰는 시크릿을 넣습니다 (등록된 앱에만 넣을 수 있음. 값은 charlie가 화면에 보이지 않게 묻습니다):")
		for _, s := range r.prep.Secrets {
			fmt.Fprintf(w, "  charlie app secret set %s %s   # %s\n", name, s.Name, printable(s.Purpose))
		}
		fmt.Fprintf(w, "  시크릿이 없어 배포가 실패했으면 넣은 뒤 다시 배포합니다: charlie deploy %s --branch %s --watch\n", name, prepareBranch)
	}
}

// findApp은 이 레포를 등록한 앱을 찾는다. 이 레포의 branch를 배포하는 앱(기본 브랜치나 추가한 인스턴스)을 먼저 찾고,
// 없으면 이 레포의 첫 앱을 돌려준다. auto는 그 앱이 branch를 자동 배포하는지, other는 branch를 배포하지 않을 때 앱의 브랜치다.
func findApp(apps *apiv1.AppList, sameRepo func(string) bool, branch string) (app *apiv1.App, auto bool, other string) {
	if apps == nil {
		return nil, false, ""
	}
	var first *apiv1.App
	for i := range apps.Apps {
		a := &apps.Apps[i]
		if a.Deploy == nil || !sameRepo(a.Deploy.Repo) {
			continue
		}
		if a.Deploy.Branch == branch {
			return a, a.Deploy.AutoDeploy, ""
		}
		for _, in := range a.Deploy.Instances {
			if in.Branch == branch {
				return a, in.AutoDeploy, ""
			}
		}
		if first == nil {
			first = a
		}
	}
	if first != nil {
		return first, false, first.Deploy.Branch
	}
	return nil, false, ""
}

// repoSize는 GitHub API가 알려 주는 레포 크기(바이트)다 (GET /repos/{owner}/{name}의 size, KB). 푸시 토큰으로 읽는다.
func repoSize(ctx context.Context, api, repo string, token secret) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api, "/")+"/repos/"+repo, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GitHub API 요청 실패: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GitHub API가 HTTP %d로 답함", resp.StatusCode)
	}
	var out struct {
		Size *int64 `json:"size"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.Size == nil {
		return 0, errors.New("GitHub API 응답에 size가 없음")
	}
	return *out.Size << 10, nil
}

/* ---------- 푸시 토큰 (platform ADR 0023) ---------- */

// pushToken은 플랫폼이 준 GitHub 푸시 토큰이다. 레포 하나에만 쓸 수 있고 곧 만료된다.
type pushToken struct {
	Repo      string
	Token     secret
	ExpiresAt time.Time
}

// pushTokenIssuer는 푸시 토큰을 받는 곳이다. 테스트는 가짜를 쓴다.
type pushTokenIssuer interface {
	PushToken(ctx context.Context, repo string) (*pushToken, error)
}

// errNotImported는 레포가 플랫폼에 들여온 레포(토픽 hopcharlie-imported)가 아닐 때다.
var errNotImported = errors.New("들여온 레포가 아님")

// platformPushTokens는 POST /v1/github/push-tokens를 부른다. 고정한 platform 모듈(pkg/client)에 아직 없어 직접 부른다.
type platformPushTokens struct {
	base, token string
	hc          *http.Client
}

func (p platformPushTokens) PushToken(ctx context.Context, repo string) (*pushToken, error) {
	body, _ := json.Marshal(map[string]string{"repo": repo})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.base, "/")+"/v1/github/push-tokens", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("푸시 토큰 요청 실패: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("푸시 토큰 응답을 읽을 수 없음: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e apiv1.Error
		_ = json.Unmarshal(b, &e)
		code, msg := e.Error.Code, mask.String(e.Error.Message)
		if code == "" {
			msg = mask.String(agentlog.Truncate(strings.Join(strings.Fields(string(b)), " "), 200))
		}
		switch {
		case resp.StatusCode == http.StatusForbidden && code == "repo_not_imported":
			return nil, fmt.Errorf("%w: %s (%s). platform 레포의 scripts/import-repo.sh로 먼저 들여오세요", errNotImported, repo, msg)
		case resp.StatusCode == http.StatusNotFound && code == "":
			// 모르는 경로는 Error 본문 없이 404로 끝난다
			return nil, fmt.Errorf("플랫폼에 푸시 토큰 API(POST /v1/github/push-tokens, platform ADR 0023)가 없음. 플랫폼을 올린 뒤 다시 실행하세요 (HTTP 404 %s: %s)", code, msg)
		}
		return nil, fmt.Errorf("푸시 토큰 발급 실패 (HTTP %d %s): %s", resp.StatusCode, code, msg)
	}
	var out struct {
		Repo      string    `json:"repo"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	// 디코딩 오류 문구에 응답 내용이 섞일 수 있어 원문 오류는 붙이지 않는다
	if json.Unmarshal(b, &out) != nil || out.Token == "" {
		return nil, errors.New("푸시 토큰 응답 형식이 다름 ({repo, token, expiresAt}이어야 함)")
	}
	// 토큰은 레포 ID에 묶여 있고, 이름이 바뀐 레포면 GitHub의 지금 이름이 온다 (ADR 0023)
	if !validRepo(out.Repo) {
		return nil, fmt.Errorf("푸시 토큰 응답의 레포 이름이 OWNER/NAME이 아님: %q", out.Repo)
	}
	return &pushToken{Repo: out.Repo, Token: secret(out.Token), ExpiresAt: out.ExpiresAt}, nil
}

/* ---------- 지난 배포 ---------- */

// deploymentReader는 지난 배포를 읽는 플랫폼 API다 (*client.Client). agent 토큰으로 읽을 수 있다.
type deploymentReader interface {
	GetDeployment(ctx context.Context, id string) (*apiv1.Deployment, error)
	StreamDeploymentEvents(ctx context.Context, id string, after int64, fn func(client.DeploymentMessage) error) error
}

// 지난 배포 기록을 AI에 넣는 상한
const (
	maxFailureErrorLines = 40      // 오류처럼 보이는 줄 (뒤에서부터)
	maxFailureTailLines  = 30      // 마지막 줄
	maxFailureBytes      = 8 << 10 // 가린 뒤 전체 크기 (뒤를 남김)
	eventWait            = 10 * time.Second
)

var errorLine = regexp.MustCompile(`(?i)error|fail|fatal|denied|not found|no such|panic|exception|cannot|could not|unable|killed|oom|exit (code|status)|exited`)

// lastDeployment는 배포의 상태와, 이벤트 중 오류처럼 보이는 줄과 마지막 줄을 비밀값을 가려 돌려준다.
// 이벤트는 SSE다. 끝난 배포는 기록을 다 보내고 닫히고, 끝나지 않은 배포는 열려 있으므로 eventWait까지 받은 것을 쓴다.
func lastDeployment(ctx context.Context, pc deploymentReader, id string) (*apiv1.Deployment, string, error) {
	d, err := pc.GetDeployment(ctx, id)
	if err != nil {
		return nil, "", fmt.Errorf("배포 %s를 읽을 수 없음: %w", id, err)
	}
	var lines []string
	sctx, cancel := context.WithTimeout(ctx, eventWait)
	defer cancel()
	err = pc.StreamDeploymentEvents(sctx, id, 0, func(m client.DeploymentMessage) error {
		e := m.Event
		if e == nil || e.Message == nil || strings.TrimSpace(*e.Message) == "" {
			return nil
		}
		tag := string(e.Type)
		if e.Phase != nil {
			tag += " " + string(*e.Phase)
		}
		if e.Env != nil {
			tag += " " + *e.Env
		}
		for _, l := range strings.Split(strings.TrimRight(*e.Message, "\r\n"), "\n") {
			lines = append(lines, "["+tag+"] "+strings.TrimRight(l, "\r"))
		}
		return nil
	})
	if err != nil && sctx.Err() == nil {
		return nil, "", fmt.Errorf("배포 %s의 이벤트를 읽을 수 없음: %w", id, err)
	}

	keep := map[int]bool{}
	for i, n := len(lines)-1, 0; i >= 0 && n < maxFailureErrorLines; i-- {
		if errorLine.MatchString(lines[i]) {
			keep[i], n = true, n+1
		}
	}
	for i := max(0, len(lines)-maxFailureTailLines); i < len(lines); i++ {
		keep[i] = true
	}
	head := fmt.Sprintf("배포 %s: 앱 %s, 브랜치 %s, 커밋 %.12s, 상태 %s", d.Id, d.App, d.Branch, d.Commit, d.Status)
	if d.Error != nil {
		head += "\n오류: " + agentlog.Truncate(*d.Error, 1000)
	}
	var body []string
	prev := -1
	for i, l := range lines {
		if !keep[i] {
			continue
		}
		if prev >= 0 && i != prev+1 {
			body = append(body, "...")
		}
		body = append(body, l)
		prev = i
	}
	if len(body) == 0 {
		return d, mask.String(head), nil
	}
	return d, mask.String(head) + "\n" + tail(mask.String(strings.Join(body, "\n")), maxFailureBytes), nil
}

// tail은 s의 뒤 n바이트 안쪽을 줄 단위로 남긴다.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return "...\n" + s
}
