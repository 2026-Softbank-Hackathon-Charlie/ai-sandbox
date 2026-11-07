package main

// hopai onboard: 플랫폼 밖 VM에서 docker compose로 돌던 앱의 레포에 플랫폼 계약 파일을 새 파일로만 더해 hopcharlie/onboard 브랜치로 push한다
// (platform ADR 0042 "2. 계약 PR"). 플랫폼이 온보딩의 patching, checking 상태에서 Job(KIND=onboard)으로 띄운다.
//
//	조사 결과(FACTS_FILE)와 재시도면 실패한 시험 배포 기록(CHECK_LOG_FILE) 읽기
//	→ 푸시 토큰(POST /v1/github/push-tokens. 플랫폼은 온보딩이 patching이나 checking인 레포에 준다)
//	→ 기본 브랜치를 클론하고, hopcharlie/onboard가 있으면 그 위에서(기본 브랜치에 새 파일만 더한 브랜치여야 함), 없으면 새로
//	→ AI가 클론 안의 파일을 읽고 새 파일만 씀(llm.Onboard). 기본 브랜치에 있는 경로는 도구가 거절한다
//	→ 기존 파일을 고쳐야만 하면 push하지 않고 종료 코드 3
//	→ 검사를 통과하면 커밋하고, 기본 브랜치 대비 바뀐 것이 모두 허용된 새 파일(A)인지 다시 본 뒤 hopcharlie/onboard에 fast-forward로만 push
//	→ 결과 JSON 한 줄을 표준 출력에 씀(head가 PR head). PR은 플랫폼이 연다
//
// AI는 명령을 실행하지 않고 토큰을 보지 않는다. 기본 브랜치와 다른 브랜치에는 push하지 않는다.

import (
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

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

// onboardBranch는 계약을 올리는 브랜치이자 PR head다 (platform internal/onboard.Branch). 이 브랜치 말고는 push하지 않는다.
const onboardBranch = "hopcharlie/onboard"

// onboardingID는 온보딩 ID다 (platform internal/onboard.ID).
var onboardingID = regexp.MustCompile(`^onb-[0-9a-f]{12}$`)

// 입력 상한
const (
	maxFactsBytes = 8 << 20
	// maxCheckLogBytes는 AI에 넣는 시험 배포 기록의 뒷부분이다. 플랫폼은 마지막 32 KiB(internal/onboard.CheckLogMax)를 준다.
	maxCheckLogBytes = 16 << 10
)

const onboardUsage = "사용법: hopai onboard [-repo OWNER/NAME] [-facts FILE] [-check-log FILE] [-onboarding ID]  (기본값은 REPO, FACTS_FILE, CHECK_LOG_FILE, ONBOARDING)"

// onboardArgs는 hopai onboard의 입력이다.
type onboardArgs struct {
	repo, factsFile, checkLogFile, onboarding string
}

// onboardResult는 표준 출력에 한 줄로 쓰는 결과다. 플랫폼은 head로 PR을 열고, 나머지를 리포트에 쓴다.
type onboardResult struct {
	Kind         string `json:"kind"` // "onboard"
	OnboardingID string `json:"onboarding,omitempty"`
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	// Base는 클론한 기본 브랜치 커밋, Head는 push한 커밋이다. push하지 않았으면 Head는 없다.
	Base    string `json:"base"`
	Head    string `json:"head,omitempty"`
	Session string `json:"session,omitempty"`
	// Files는 기본 브랜치에 더한 새 파일이다 (이 브랜치의 모든 커밋).
	Files []string `json:"files"`
	*llm.Onboarding
}

// onboardMain은 hopai onboard의 진입점이다. 종료 코드는 prepare와 같다 (성공 0, 실패 1, 사용법 2, 계약을 더할 수 없음 3).
func onboardMain(args []string) int {
	fs := flag.NewFlagSet("onboard", flag.ContinueOnError)
	a := onboardArgs{}
	fs.StringVar(&a.repo, "repo", os.Getenv("REPO"), "온보딩하는 레포 OWNER/NAME (REPO)")
	fs.StringVar(&a.factsFile, "facts", os.Getenv("FACTS_FILE"), "플랫폼 조사 결과 JSON 파일 (FACTS_FILE)")
	fs.StringVar(&a.checkLogFile, "check-log", os.Getenv("CHECK_LOG_FILE"), "재시도 때 실패한 시험 배포 기록 파일 (CHECK_LOG_FILE)")
	fs.StringVar(&a.onboarding, "onboarding", os.Getenv("ONBOARDING"), "온보딩 ID onb-... (ONBOARDING)")
	if err := fs.Parse(args); err != nil || a.repo == "" || a.factsFile == "" || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, onboardUsage)
		return 2
	}
	if k := os.Getenv("KIND"); k != "" && k != "onboard" {
		fmt.Fprintf(os.Stderr, "오류: KIND가 %q임 (onboard여야 함)\n", k)
		return 2
	}
	if b := os.Getenv("BRANCH"); b != "" && b != onboardBranch {
		fmt.Fprintf(os.Stderr, "오류: BRANCH가 %q임. hopai onboard는 %s에만 push함\n", b, onboardBranch)
		return 2
	}
	purl, ptok := os.Getenv("PLATFORM_URL"), os.Getenv("PLATFORM_TOKEN")
	if purl == "" || ptok == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "오류: 환경 변수 PLATFORM_URL, PLATFORM_TOKEN, ANTHROPIC_API_KEY가 필요함")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	pc := client.New(purl, ptok)
	agent := llm.New(llm.Config{
		Sink:          agentlog.Tee{agentlog.Platform{C: pc}, agentlog.Writer{W: os.Stderr}},
		MaxIterations: 30,
		OnLogError: func(err error) {
			fmt.Fprintln(os.Stderr, "경고: 에이전트 기록 일부를 플랫폼에 남기지 못함:", err)
		},
	})
	err := runOnboard(ctx, prepareEnv{
		pc:        pc,
		tokens:    platformPushTokens{base: purl, token: ptok, hc: http.DefaultClient},
		agent:     agent,
		github:    "https://github.com",
		githubAPI: "https://api.github.com",
		stdout:    os.Stdout, stderr: os.Stderr,
	}, a)
	return prepareExit(os.Stderr, err)
}

// readFacts는 조사 결과를 읽고 이 레포의 것인지, 계약을 더할 수 있는 상태인지 본다.
func readFacts(file, repo string) (*facts.Facts, error) {
	fh, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("조사 결과 파일을 열 수 없음: %w", err)
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxFactsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("조사 결과 파일을 읽을 수 없음: %w", err)
	}
	if len(b) > maxFactsBytes {
		return nil, fmt.Errorf("조사 결과가 %d MiB를 넘음", maxFactsBytes>>20)
	}
	var f facts.Facts
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("조사 결과가 facts.Facts JSON이 아님: %w", err)
	}
	switch {
	case f.Version < 1 || f.Version > facts.Version:
		return nil, fmt.Errorf("조사 결과 버전 %d를 읽을 수 없음 (이 hopai는 %d까지)", f.Version, facts.Version)
	case !strings.EqualFold(f.Repo, repo):
		return nil, fmt.Errorf("조사 결과의 레포(%s)가 -repo %s와 다름", f.Repo, repo)
	case f.App == "":
		return nil, errors.New("조사 결과에 앱 이름(app)이 없음")
	case f.GitHub == nil || !f.GitHub.Installed:
		return nil, errors.New("조사 결과에 GitHub 조사가 없음 (GitHub App이 설치되지 않음)")
	case len(f.Report.Blockers) > 0:
		codes := make([]string, len(f.Report.Blockers))
		for i, b := range f.Report.Blockers {
			codes[i] = b.Code
		}
		return nil, fmt.Errorf("조사 결과에 차단 사유가 있음 (%s). 계약을 더하지 않음", strings.Join(codes, ", "))
	}
	return &f, nil
}

// readCheckLog는 실패한 시험 배포 기록의 뒷부분을 비밀값을 가려 읽는다. file이 비면 첫 시도다.
func readCheckLog(file string) (string, error) {
	if file == "" {
		return "", nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("시험 배포 기록을 읽을 수 없음: %w", err)
	}
	return tail(mask.String(string(b)), maxCheckLogBytes), nil
}

// onboardChangesOK는 기본 브랜치 대비 바뀐 것이 모두 일반 파일로 새로 더한 허용된 이름인지 본다.
// 기본 브랜치 파일의 수정(M), 삭제(D), 종류 바뀜(T)이 하나라도 있으면 PR이 원본 배포를 바꿀 수 있으므로 push하지 않는다.
func onboardChangesOK(changes []gitChange, what string) ([]string, error) {
	var files []string
	for _, c := range changes {
		if c.Status != "A" {
			return nil, fmt.Errorf("%s: 기본 브랜치의 %q가 바뀜 (%s). 새 파일만 더하므로 push하지 않음", what, c.Path, c.Status)
		}
		if c.Mode != "100644" && c.Mode != "100755" {
			return nil, fmt.Errorf("%s: %q가 일반 파일이 아님 (모드 %s). push하지 않음", what, c.Path, c.Mode)
		}
		if err := llm.OnboardWritable(c.Path); err != nil {
			return nil, fmt.Errorf("%s: %w. push하지 않음", what, err)
		}
		files = append(files, c.Path)
	}
	return files, nil
}

func runOnboard(ctx context.Context, env prepareEnv, a onboardArgs) error {
	if !validRepo(a.repo) {
		return fmt.Errorf("-repo는 OWNER/NAME이어야 함: %q", a.repo)
	}
	if a.onboarding != "" && !onboardingID.MatchString(a.onboarding) {
		return fmt.Errorf("-onboarding은 onb-<소문자 hex 12자>여야 함: %q", a.onboarding)
	}
	f, err := readFacts(a.factsFile, a.repo)
	if err != nil {
		return err
	}
	checkLog, err := readCheckLog(a.checkLogFile)
	if err != nil {
		return err
	}
	me, err := env.pc.Me(ctx)
	if err != nil {
		return err
	}
	if me.Role != "agent" && me.Role != "operator" {
		return fmt.Errorf("토큰 역할이 %s임 (agent 필요)", me.Role)
	}

	// 1. 푸시 토큰
	tok, err := env.tokens.PushToken(ctx, a.repo)
	if err != nil {
		return err
	}
	repo := tok.Repo
	if !strings.EqualFold(repo, a.repo) {
		fmt.Fprintf(env.stderr, "레포 %s의 지금 이름은 %s입니다. 그 이름으로 클론합니다\n", a.repo, repo)
	}
	fmt.Fprintf(env.stderr, "푸시 토큰 받음 (%s, 만료 %s)\n", repo, tok.ExpiresAt.Local().Format("15:04"))

	// 2. 클론. 끝나면 지운다.
	tmp, err := os.MkdirTemp("", "hopai-onboard-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	g, err := newGitRunner(tmp, env.github, tok.Token)
	if err != nil {
		return err
	}
	g.trace = env.trace
	if size, err := repoSize(ctx, env.githubAPI, repo, tok.Token); err != nil {
		fmt.Fprintf(env.stderr, "경고: 클론 전에 레포 크기를 알지 못함 (%v). 클론한 뒤 크기를 봅니다\n", mask.String(err.Error()))
	} else if size > maxTreeBytes {
		return fmt.Errorf("레포 %s가 %d MB로 상한 %d MB를 넘음. 이렇게 큰 레포는 다루지 않음", repo, size>>20, maxTreeBytes>>20)
	}
	dir := filepath.Join(tmp, "repo")
	url := strings.TrimRight(env.github, "/") + "/" + repo + ".git"
	exists, err := g.remoteHasBranch(ctx, tmp, url, onboardBranch)
	if err != nil {
		return fmt.Errorf("레포 %s를 볼 수 없음: %w", repo, err)
	}
	base, defaultBranch, remote, err := g.cloneOnboard(ctx, tmp, dir, url, onboardBranch, exists)
	if err != nil {
		return fmt.Errorf("레포 %s를 클론하지 못함: %w", repo, err)
	}
	if err := treeSize(dir, maxTreeBytes); err != nil {
		return err
	}
	if f.GitHub.Head != "" && f.GitHub.Head != base {
		fmt.Fprintf(env.stderr, "경고: 기본 브랜치 %s가 조사 뒤 바뀜 (조사 %.12s, 지금 %.12s). 지금 커밋 위에 더합니다\n", defaultBranch, f.GitHub.Head, base)
	}
	existing, err := g.treeFiles(ctx, dir, base)
	if err != nil {
		return err
	}
	res := onboardResult{Kind: "onboard", OnboardingID: a.onboarding, Repo: repo, Branch: onboardBranch, Base: base, Files: []string{}}
	// 이미 있는 platform.yaml은 고칠 수 없다. 플랫폼은 맞는 platform.yaml이면 patching을 건너뛰므로, 여기 왔으면 맞지 않는 것이다.
	for _, p := range existing {
		if strings.EqualFold(p, appspec.FileName) {
			res.Onboarding = &llm.Onboarding{Preparation: llm.Preparation{
				Blockers: []llm.Blocker{{Need: "기존 " + appspec.FileName + " 수정", Reason: "기본 브랜치에 " + p + "가 이미 있어 새 파일만으로 계약을 더할 수 없음"}},
				Summary:  "기본 브랜치에 " + p + "가 이미 있습니다. 온보딩은 기존 파일을 고치지 않으므로 계약을 더하지 않았습니다.",
				Secrets:  []llm.Secret{}, Notes: []string{},
			}, Mapping: []llm.Mapping{}}
			writeOnboardResult(env.stdout, res)
			return fmt.Errorf("%w: 기본 브랜치에 %s가 있음", errNotDeployable, p)
		}
	}
	if exists {
		// 전에 만든 브랜치는 기본 브랜치에 새 파일만 더한 것이어야 한다. 아니면(기본 브랜치가 그사이 바뀌었거나 누가 고침) 이어서 고치지 않는다.
		changes, err := g.diffTrees(ctx, dir, base, remote)
		if err != nil {
			return err
		}
		if _, err := onboardChangesOK(changes, "있던 "+onboardBranch+" 브랜치"); err != nil {
			return fmt.Errorf("%w. 플랫폼이 브랜치를 지우고 다시 시작해야 함", err)
		}
	}
	index, err := g.index(ctx, dir)
	if err != nil {
		return err
	}
	how := "기본 브랜치에서 새로 만듦"
	if exists {
		how = "있던 브랜치에 이어서"
	}
	fmt.Fprintf(env.stderr, "클론: %s %s (%s, 기본 브랜치 %s %.12s)\n", repo, onboardBranch, how, defaultBranch, base)

	// 3. AI
	ob, session, err := env.agent.Onboard(ctx, llm.OnboardInput{
		Repo: repo, Branch: onboardBranch, Onboarding: a.onboarding, Continue: exists, Dir: dir,
		Facts: f, CheckLog: checkLog, Index: index, Existing: existing,
	})
	if err != nil {
		return fmt.Errorf("AI 온보딩 계약 실패 (세션 %s): %w", session, err)
	}
	res.Session, res.Onboarding = session, ob
	if !ob.Deployable {
		writeOnboardResult(env.stdout, res)
		printOnboardBlocked(env.stderr, ob)
		return fmt.Errorf("%w (AI 세션 %s)", errNotDeployable, session)
	}

	// 4. 커밋, 검사한 바이트와 같은지, 기본 브랜치에 새 파일만 더했는지 확인, push
	msg := ob.CommitMessage + "\n\nhopai onboard"
	if a.onboarding != "" {
		msg += " " + a.onboarding
	}
	sha, changes, err := g.commit(ctx, dir, msg+", AI 세션 "+session+"\n")
	if err != nil {
		return err
	}
	if err := g.verifyCommit(ctx, dir, changes, ob.Validated, ob.Files, append([]string{appspec.FileName}, ob.Dockerfiles...)); err != nil {
		return err
	}
	all, err := g.diffTrees(ctx, dir, base, "HEAD")
	if err != nil {
		return err
	}
	if res.Files, err = onboardChangesOK(all, "커밋"); err != nil {
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
	if err := g.push(ctx, dir, onboardBranch); err != nil {
		return err
	}
	res.Head = sha
	fmt.Fprintf(env.stderr, "push: %s %s (%.12s 다음), 새 파일 %s\n", repo, onboardBranch, base, strings.Join(res.Files, ", "))
	writeOnboardResult(env.stdout, res)
	return nil
}

// writeOnboardResult는 결과를 JSON 한 줄로 쓴다. AI 글은 제어 문자를 이미 뺐다.
func writeOnboardResult(w io.Writer, res onboardResult) {
	b, _ := json.Marshal(res)
	fmt.Fprintln(w, string(b))
}

// printOnboardBlocked는 계약을 더할 수 없는 이유를 운영자 기록(표준 오류)에 쓴다.
func printOnboardBlocked(w io.Writer, ob *llm.Onboarding) {
	fmt.Fprintf(w, "판단: 새 파일만으로 계약을 더할 수 없습니다. %s에 push하지 않았습니다.\n", onboardBranch)
	for _, b := range ob.Blockers {
		fmt.Fprintf(w, "  - %s: %s\n", printable(b.Need), printable(b.Reason))
	}
	fmt.Fprintln(w, "요약:", printable(ob.Summary))
}
