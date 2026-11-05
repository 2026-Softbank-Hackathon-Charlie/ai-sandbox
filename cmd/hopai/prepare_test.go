package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
)

// 마스킹 규칙에 걸리지 않는 토큰이다. 가리기에 기대지 않고 애초에 어디에도 넘기지 않는지 본다.
const testPushToken = "PUSHTOKEN7f3a9c1e5b"

const testRepo = "2026-Softbank-Hackathon-Charlie/shop"

/* ---------- 가짜 AI ---------- */

type aiReply struct {
	tool, input, text string
	// before는 이 응답을 주기 전에 불린다 (그사이 누가 push하는 경우).
	before func()
}

type fakeAI struct {
	mu       sync.Mutex
	replies  []aiReply
	requests []string
}

func (f *fakeAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, string(b))
	rp := f.replies[len(f.requests)-1]
	if rp.before != nil {
		rp.before()
	}
	content, stop := []any{map[string]any{"type": "text", "text": rp.text}}, "end_turn"
	if rp.tool != "" {
		content, stop = []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", len(f.requests)), "name": rp.tool, "input": json.RawMessage(rp.input)}}, "tool_use"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
		"content": content, "stop_reason": stop, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
}

func write(path, content string) aiReply {
	b, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return aiReply{tool: "write_file", input: string(b)}
}

const (
	shopSpec   = "name: shop\nservices:\n  - name: web\n    dockerfile: hopcharlie.Dockerfile\n    port: 8080\n    healthCheck: /healthz\n    public: true\n"
	shopAnswer = `{"summary":"platform.yaml과 hopcharlie.Dockerfile을 추가했다.","commitMessage":"feat: HopCharlie 배포 설정 추가","secrets":[{"name":"ADMIN_PASSWORD","purpose":"관리자 비밀번호"}],"notes":["첫 로그인은 admin"]}`
)

/* ---------- 가짜 플랫폼 ---------- */

type fakePlatformServer struct {
	mu          sync.Mutex
	notImported bool
	rename      string // 푸시 토큰 응답의 레포 이름 (GitHub에서 이름이 바뀐 레포)
	apps        string // GET /v1/apps 응답
	appStatus   int    // GET /v1/apps/shop 오류 상태
	appNoDeploy bool   // GET /v1/apps/shop에 deploy가 없음
	appRepo     string // GET /v1/apps/shop의 deploy.repo (기본 testRepo)
	seen        []string
}

func (f *fakePlatformServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.String()+" "+string(b))
	apps := f.apps
	f.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/me":
		fmt.Fprint(w, `{"name":"hopai","role":"agent"}`)
	case r.URL.Path == "/v1/github/push-tokens":
		if f.notImported {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":"repo_not_imported","message":"repository has no topic hopcharlie-imported"}}`)
			return
		}
		var req struct{ Repo string }
		_ = json.Unmarshal(b, &req)
		if f.rename != "" {
			req.Repo = f.rename
		}
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"repo":%q,"token":%q,"expiresAt":%q}`, req.Repo, testPushToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	case r.URL.Path == "/v1/deployments/dep_1":
		fmt.Fprintf(w, `{"id":"dep_1","app":"shop","branch":"hopcharlie","commit":"9fceb02d0ae598e95dc970b74767f19372d61af8","status":"failed","error":"environment eks did not become healthy","trigger":"push","triggeredBy":"x","ref":"hopcharlie","instance":"shop","createdAt":%q,"environments":[],"images":[]}`, now)
	case r.URL.Path == "/v1/deployments/dep_1/events":
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(seq int, typ, phase, msg string) {
			d, _ := json.Marshal(map[string]any{"seq": seq, "deploymentId": "dep_1", "time": now, "type": typ, "phase": phase, "message": msg})
			fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", typ, seq, d)
		}
		fmt.Fprint(w, "event: snapshot\ndata: {\"id\":\"dep_1\",\"status\":\"failed\"}\n\n")
		ev(1, "phase", "building", "building web")
		for i := 2; i < 200; i++ {
			msg := fmt.Sprintf("step %d ok", i)
			if i == 5 {
				msg = "npm WARN could not resolve optional dependency"
			}
			ev(i, "log", "building", msg)
		}
		ev(200, "log", "syncing", "Error: listen tcp :80: bind: permission denied")
		ev(201, "log", "syncing", "connecting postgres://app:Secr3tPw@db:5432/app")
		ev(202, "done", "failed", "environment eks did not become healthy")
		fmt.Fprint(w, "event: snapshot\ndata: {\"id\":\"dep_1\",\"status\":\"failed\"}\n\n")
	case r.URL.Path == "/v1/apps":
		if apps == "" {
			apps = `{"apps":[]}`
		}
		fmt.Fprint(w, apps)
	case r.URL.Path == "/v1/apps/shop":
		repo := testRepo
		if f.appRepo != "" {
			repo = f.appRepo
		}
		switch {
		case f.appStatus != 0:
			w.WriteHeader(f.appStatus)
			fmt.Fprint(w, `{"error":{"code":"app_not_found","message":"app not found"}}`)
		case f.appNoDeploy:
			fmt.Fprint(w, `{"name":"shop","namespace":"shop","database":{"name":"app","user":"app","schemas":[]},"environments":[]}`)
		default:
			fmt.Fprintf(w, `{"name":"shop","namespace":"shop","database":{"name":"app","user":"app","schemas":[]},"environments":[],"deploy":{"repo":%q,"branch":"hopcharlie","autoDeploy":true,"environments":["eks"],"instances":[],"createdAt":%q,"createdBy":"op"}}`, repo, now)
		}
	case r.URL.Path == "/v1/agent/sessions":
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"id":"ags_1","kind":"other","title":"t","actor":"hopai","status":"running","createdAt":%q,"totals":{"events":0,"toolCalls":0,"inputTokens":0,"outputTokens":0,"durationMillis":0}}`, now)
	case strings.HasSuffix(r.URL.Path, "/events"):
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"seq":1,"sessionId":"ags_1","time":%q,"type":"message"}`, now)
	case strings.HasSuffix(r.URL.Path, "/finish"):
		fmt.Fprintf(w, `{"id":"ags_1","kind":"other","title":"t","actor":"hopai","status":"succeeded","createdAt":%q,"totals":{"events":0,"toolCalls":0,"inputTokens":0,"outputTokens":0,"durationMillis":0}}`, now)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePlatformServer) requests() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.seen, "\n")
}

/* ---------- 로컬 git 원격 ---------- */

// hermeticGitConfig는 빈 git 설정 파일이다. 테스트가 개발자 컴퓨터의 git 설정(시스템, 전역)에 기대지 않게 한다.
var hermeticGitConfig string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hopai-test-git-")
	if err != nil {
		panic(err)
	}
	hermeticGitConfig = filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(hermeticGitConfig, nil, 0o600); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_CONFIG_GLOBAL", hermeticGitConfig)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// gitT는 테스트 준비용 git이다. 테스트가 부모 환경에 운영자 설정을 흉내 내 넣어도 준비 단계는 설정 없이 돈다.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "user.name=Upstream", "-c", "user.email=up@example.com", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+hermeticGitConfig, "HOME="+filepath.Dir(hermeticGitConfig))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// localRemote는 <root>/remote/<testRepo>.git 맨 레포를 만들고 main에 커밋 하나를 올린다. GitHub 대신 쓸 file:// 주소를 돌려준다.
func localRemote(t *testing.T) (github, bare string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git이 없음")
	}
	root := t.TempDir()
	bare = filepath.Join(root, "remote", filepath.FromSlash(testRepo)+".git")
	gitT(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	work := filepath.Join(root, "work")
	gitT(t, root, "init", "--quiet", "-b", "main", work)
	for p, c := range map[string]string{"README.md": "# shop\n", "main.go": "package main\n"} {
		if err := os.WriteFile(filepath.Join(work, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "--quiet", "-m", "upstream")
	gitT(t, work, "push", "--quiet", bare, "main")
	p := filepath.ToSlash(filepath.Join(root, "remote"))
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p, bare
}

// fakeGitHubAPI는 GET /repos/{owner}/{name}만 흉내 낸다 (size는 KB).
type fakeGitHubAPI struct {
	mu       sync.Mutex
	size     int64
	status   int
	lastAuth string
}

func (f *fakeGitHubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuth = r.Header.Get("Authorization")
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	fmt.Fprintf(w, `{"full_name":%q,"size":%d}`, strings.TrimPrefix(r.URL.Path, "/repos/"), f.size)
}

func (f *fakeGitHubAPI) auth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

type prepareRun struct {
	env            prepareEnv
	ai             *fakeAI
	platform       *fakePlatformServer
	stdout, stderr *bytes.Buffer
	argv           *[][]string
	sink           *agentlog.Memory
	github         *fakeGitHubAPI
}

func newPrepareRun(t *testing.T, github string, plat *fakePlatformServer, replies ...aiReply) *prepareRun {
	t.Helper()
	ai := &fakeAI{replies: replies}
	aiSrv := httptest.NewServer(ai)
	t.Cleanup(aiSrv.Close)
	platSrv := httptest.NewServer(plat)
	t.Cleanup(platSrv.Close)
	gh := &fakeGitHubAPI{size: 10}
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	pc := client.New(platSrv.URL, "agent-token")
	mem := &agentlog.Memory{}
	var stdout, stderr bytes.Buffer
	var argv [][]string
	agent := llm.New(llm.Config{
		Sink:          agentlog.Tee{agentlog.Platform{C: pc}, mem, agentlog.Writer{W: &stderr}},
		MaxIterations: 30,
		Options:       []option.RequestOption{option.WithBaseURL(aiSrv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0)},
	})
	return &prepareRun{
		env: prepareEnv{
			pc: pc, tokens: platformPushTokens{base: platSrv.URL, token: "agent-token", hc: http.DefaultClient},
			agent: agent, github: github, githubAPI: ghSrv.URL, stdout: &stdout, stderr: &stderr,
			trace: func(a []string) { argv = append(argv, a) },
		},
		ai: ai, platform: plat, stdout: &stdout, stderr: &stderr, argv: &argv, sink: mem, github: gh,
	}
}

// assertNoToken은 토큰이 출력, 기록, git 명령줄, AI 요청, 플랫폼 기록 요청 어디에도 없는지 본다.
func (r *prepareRun) assertNoToken(t *testing.T) {
	t.Helper()
	events, _ := json.Marshal(r.sink.Sessions)
	argv, _ := json.Marshal(*r.argv)
	ai := strings.Join(r.ai.requests, "\n")
	plat := r.platform.requests()
	for name, s := range map[string]string{"stdout": r.stdout.String(), "stderr": r.stderr.String(), "agentlog": string(events), "git argv": string(argv), "AI 요청": ai, "플랫폼 요청": plat} {
		if strings.Contains(s, testPushToken) {
			t.Errorf("푸시 토큰이 %s에 있음", name)
		}
	}
	if len(*r.argv) == 0 {
		t.Error("git을 부르지 않음")
	}
}

func TestPrepareEndToEnd(t *testing.T) {
	github, bare := localRemote(t)
	plat := &fakePlatformServer{}

	// 1회차: hopcharlie가 없음. 기본 브랜치에서 새로 만들고, 지난 배포의 실패 줄을 AI에 넣는다
	r := newPrepareRun(t, github, plat,
		write("platform.yaml", shopSpec),
		write("hopcharlie.Dockerfile", "FROM ghcr.io/example/shop:1.2.3\nUSER 65532\n"),
		aiReply{text: shopAnswer},
	)
	if err := runPrepare(context.Background(), r.env, testRepo, "dep_1"); err != nil {
		t.Fatalf("%v\n%s", err, r.stderr)
	}
	main := gitT(t, bare, "rev-parse", "main")
	first := gitT(t, bare, "rev-parse", "hopcharlie")
	if gitT(t, bare, "rev-parse", "hopcharlie^") != main {
		t.Fatal("hopcharlie는 main 위에 커밋 하나여야 함")
	}
	if got := gitT(t, bare, "log", "-1", "--format=%an <%ae>|%cn|%s", "hopcharlie"); got != "HopCharlie AI <ai@hopcharlie.dev>|HopCharlie AI|feat: HopCharlie 배포 설정 추가" {
		t.Fatalf("커밋 작성자와 메시지: %s", got)
	}
	if body := gitT(t, bare, "log", "-1", "--format=%b", "hopcharlie"); !strings.Contains(body, "AI 세션 ags_1") {
		t.Fatalf("커밋에 AI 세션이 남아야 함: %s", body)
	}
	if files := gitT(t, bare, "show", "--name-only", "--format=", "hopcharlie"); files != "hopcharlie.Dockerfile\nplatform.yaml" {
		t.Fatalf("커밋한 파일: %q", files)
	}
	out := r.stdout.String()
	for _, want := range []string{first, "ags_1", "charlie app add --repo " + testRepo + " --branch hopcharlie --env eks --watch", "charlie app secret set shop ADMIN_PASSWORD", "새로 만듦"} {
		if !strings.Contains(out, want) {
			t.Errorf("출력에 %q가 없음:\n%s", want, out)
		}
	}
	// 지난 배포: 오류 줄은 AI에 가고, 비밀번호는 가리고, 오류 아닌 중간 줄은 뺀다
	prompt := r.ai.requests[0]
	if !strings.Contains(prompt, "bind: permission denied") || !strings.Contains(prompt, "did not become healthy") {
		t.Fatalf("지난 배포의 실패 줄이 AI에 가야 함: %s", prompt)
	}
	if strings.Contains(prompt, "Secr3tPw") || strings.Contains(prompt, "step 50 ok") {
		t.Fatalf("비밀번호가 가려지지 않았거나 줄이 줄지 않음: %s", prompt)
	}
	// git -c 설정들 뒤가 하위 명령이다
	pushes := 0
	for _, a := range *r.argv {
		i := 0
		for i+1 < len(a) && a[i] == "-c" {
			i += 2
		}
		if a[i] != "push" {
			continue
		}
		pushes++
		if got := strings.Join(a[i+1:], " "); got != "--porcelain --no-follow-tags origin refs/heads/hopcharlie:refs/heads/hopcharlie" {
			t.Fatalf("push는 hopcharlie에만, 강제 없이: %s", got)
		}
	}
	if pushes != 1 {
		t.Fatalf("push %d번", pushes)
	}
	r.assertNoToken(t)

	// 2회차: hopcharlie가 있음. 그 위에서 이어서 고치고 fast-forward로 push한다. 이 레포를 hopcharlie로 배포하는 앱이 있다
	plat.apps = fmt.Sprintf(`{"apps":[{"name":"shop","namespace":"shop","database":{"name":"app","user":"app","schemas":[]},"environments":[],"deploy":{"repo":%q,"branch":"hopcharlie","autoDeploy":true,"environments":["eks"],"instances":[],"createdAt":"2026-11-05T00:00:00Z","createdBy":"op"}}]}`, testRepo)
	r2 := newPrepareRun(t, github, plat,
		aiReply{tool: "read_file", input: `{"path":"platform.yaml"}`},
		write("hopcharlie.Dockerfile", "FROM ghcr.io/example/shop:1.2.4\nUSER 65532\n"),
		aiReply{text: strings.Replace(shopAnswer, "feat: HopCharlie 배포 설정 추가", "fix: 이미지 1.2.4로", 1)},
	)
	if err := runPrepare(context.Background(), r2.env, testRepo, ""); err != nil {
		t.Fatalf("%v\n%s", err, r2.stderr)
	}
	if gitT(t, bare, "rev-parse", "hopcharlie^") != first || gitT(t, bare, "rev-parse", "main") != main {
		t.Fatal("2회차는 1회차 커밋 위에 fast-forward여야 하고 main은 그대로여야 함")
	}
	if !strings.Contains(r2.ai.requests[0], "전에 만든 hopcharlie 브랜치") || !strings.Contains(r2.ai.requests[0], "platform.yaml (") {
		t.Fatal("이어서 고친다는 것과 기존 platform.yaml이 AI에 보여야 함")
	}
	if out := r2.stdout.String(); !strings.Contains(out, "push가 자동 재배포를 시작했습니다") || !strings.Contains(out, "이어서 고침") {
		t.Fatalf("다음 단계: %s", out)
	}
	r2.assertNoToken(t)
}

func TestPreparePushRejected(t *testing.T) {
	github, bare := localRemote(t)
	// AI가 일하는 사이 누가 hopcharlie를 만들어 push한다
	other := func() {
		work := filepath.Join(t.TempDir(), "other")
		gitT(t, "", "clone", "--quiet", bare, work)
		gitT(t, work, "switch", "--quiet", "-c", "hopcharlie")
		_ = os.WriteFile(filepath.Join(work, "other.txt"), []byte("x\n"), 0o644)
		gitT(t, work, "add", "-A")
		gitT(t, work, "commit", "--quiet", "-m", "someone else")
		gitT(t, work, "push", "--quiet", "origin", "hopcharlie")
	}
	r := newPrepareRun(t, github, &fakePlatformServer{},
		write("platform.yaml", shopSpec),
		aiReply{tool: "write_file", input: `{"path":"hopcharlie.Dockerfile","content":"FROM x:1\n"}`, before: other},
		aiReply{text: shopAnswer},
	)
	err := runPrepare(context.Background(), r.env, testRepo, "")
	if !errors.Is(err, errPushRejected) || !strings.Contains(err.Error(), "다시 실행") {
		t.Fatalf("push가 거절됐다고 알려야 함: %v", err)
	}
	if got := gitT(t, bare, "log", "-1", "--format=%s", "hopcharlie"); got != "someone else" {
		t.Fatalf("남의 커밋을 덮어씀: %s", got)
	}
	r.assertNoToken(t)
}

func TestPrepareRepoNotImported(t *testing.T) {
	r := newPrepareRun(t, "file:///nowhere", &fakePlatformServer{notImported: true})
	err := runPrepare(context.Background(), r.env, testRepo, "")
	if !errors.Is(err, errNotImported) || !strings.Contains(err.Error(), "scripts/import-repo.sh") {
		t.Fatalf("들여오라고 알려야 함: %v", err)
	}
	if len(*r.argv) != 0 || len(r.ai.requests) != 0 {
		t.Fatal("토큰을 못 받으면 클론도 AI도 하지 않아야 함")
	}
}

func TestPushTokenErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"플랫폼에 API 없음", 404, "404 page not found", "ADR 0023"},
		{"레포 이름 형식이 다름", 201, `{"repo":"../x","token":"` + testPushToken + `","expiresAt":"2026-11-05T00:00:00Z"}`, "OWNER/NAME이 아님"},
		{"토큰 없음", 201, `{"repo":"` + testRepo + `"}`, "형식이 다름"},
		{"권한 없음", 403, `{"error":{"code":"forbidden","message":"agent or operator"}}`, "HTTP 403 forbidden"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}))
			defer srv.Close()
			_, err := platformPushTokens{base: srv.URL, token: "agent-token", hc: http.DefaultClient}.PushToken(context.Background(), testRepo)
			if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), testPushToken) {
				t.Fatalf("오류: %v", err)
			}
		})
	}
	// 이름이 바뀐 레포는 GitHub의 지금 이름으로 온다. 받아들인다
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		fmt.Fprint(w, `{"repo":"`+testRepo+`","token":"`+testPushToken+`","expiresAt":"2026-11-05T00:00:00Z"}`)
	}))
	defer srv.Close()
	if got, err := (platformPushTokens{base: srv.URL, token: "agent-token", hc: http.DefaultClient}).PushToken(context.Background(), "2026-Softbank-Hackathon-Charlie/old-shop"); err != nil || got.Repo != testRepo {
		t.Fatalf("이름이 바뀐 레포: %+v %v", got, err)
	}
	tok := pushToken{Token: secret(testPushToken)}
	if s := fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok.Token); strings.Contains(s, testPushToken) {
		t.Fatalf("토큰이 출력에 찍힘: %s", s)
	}
	if b, _ := json.Marshal(tok); strings.Contains(string(b), testPushToken) {
		t.Fatalf("토큰이 JSON에 찍힘: %s", b)
	}
}

func TestLastDeploymentKeepsErrorLinesMaskedAndCapped(t *testing.T) {
	srv := httptest.NewServer(&fakePlatformServer{})
	defer srv.Close()
	d, lines, err := lastDeployment(context.Background(), client.New(srv.URL, "agent-token"), "dep_1")
	if err != nil || d.Id != "dep_1" {
		t.Fatal(d, err)
	}
	for _, want := range []string{"상태 failed", "오류: environment eks", "[log building] npm WARN could not resolve", "\n...\n", "[log syncing] Error: listen tcp :80", "[done failed] environment eks"} {
		if !strings.Contains(lines, want) {
			t.Errorf("%q가 없음:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "Secr3tPw") || strings.Contains(lines, "step 50 ok") || strings.Count(lines, "\n") > maxFailureErrorLines+maxFailureTailLines+4 {
		t.Fatalf("가리지 않았거나 줄이 많음:\n%s", lines)
	}
	big := strings.Repeat("error: 가나다라마바사 "+strings.Repeat("x", 300)+"\n", 100)
	if got := tail(big, maxFailureBytes); len(got) > maxFailureBytes+4 || !strings.HasPrefix(got, "...\nerror:") {
		t.Fatalf("크기 상한: %d바이트, 앞부분 %q", len(got), got[:20])
	}
}

// requireHTTPBackend는 git http-backend가 없으면 건너뛴다. --help로 확인하지 않는다(Git for Windows는 설명서를 브라우저로 연다).
func requireHTTPBackend(t *testing.T) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git이 없음")
	}
	execPath, err := exec.Command(gitPath, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path 실패")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		if _, err := os.Stat(backend + ".exe"); err != nil {
			t.Skip("git http-backend가 없음")
		}
	}
	return gitPath
}

// httpGitServer는 맨 레포를 git http-backend로 HTTP에 내준다. Basic 인증이 x-access-token:<testPushToken>일 때만 통과시키고,
// 받은 사용자 이름을 모은다.
func httpGitServer(t *testing.T, bare string) (*httptest.Server, func() []string) {
	t.Helper()
	gitPath := requireHTTPBackend(t)
	gitT(t, bare, "config", "http.receivepack", "true")
	projectRoot := filepath.Dir(filepath.Dir(bare))
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		mu.Lock()
		auths = append(auths, user)
		mu.Unlock()
		if !ok || user != "x-access-token" || pass != testPushToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="github"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"},
			Env:        []string{"GIT_PROJECT_ROOT=" + projectRoot, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x-access-token", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + hermeticGitConfig},
			InheritEnv: []string{"PATH", "SystemRoot", "TEMP", "TMP"}}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(auths)
	}
}

// operatorConfig는 운영자 컴퓨터처럼 git 전역 설정(가짜 HOME의 .gitconfig와 GIT_CONFIG_GLOBAL)과 시스템 설정(GIT_CONFIG_SYSTEM)을
// 부모 환경에 넣는다. hopai는 이것을 읽지 않아야 한다. 가짜 HOME을 돌려준다.
func operatorConfig(t *testing.T, cfg string) string {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, ".gitconfig")
	system := filepath.Join(t.TempDir(), "gitconfig")
	for _, f := range []string{global, system} {
		if err := os.WriteFile(f, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")
	os.Unsetenv("GIT_CONFIG_NOSYSTEM")
	return home
}

// TestGitUsesTokenOnlyThroughCredentialHelper는 HTTP git 서버(git http-backend)로 토큰이 실제로 자격 증명 도우미를 거쳐서만
// 가는지 본다. 원격 URL, 명령줄, .git/config에는 토큰이 없어야 하고, 운영자 설정의 credential.helper=store가 있어도 저장되지 않아야 한다.
func TestGitUsesTokenOnlyThroughCredentialHelper(t *testing.T) {
	_, bare := localRemote(t)
	srv, auths := httpGitServer(t, bare)
	home := operatorConfig(t, "[credential]\n\thelper = store\n")
	// 운영자 홈의 netrc는 curl이 읽을 수 있다. hopai의 git은 빈 HOME을 쓰므로 읽지 않아야 한다
	for _, n := range []string{".netrc", "_netrc"} {
		_ = os.WriteFile(filepath.Join(home, n), []byte("machine 127.0.0.1\nlogin operator\npassword opsecret\n"), 0o600)
	}

	tmp := t.TempDir()
	var argv [][]string
	g, err := newGitRunner(tmp, srv.URL, secret(testPushToken))
	if err != nil {
		t.Fatal(err)
	}
	g.trace = func(a []string) { argv = append(argv, a) }
	url := srv.URL + "/" + testRepo + ".git"
	ctx := context.Background()
	exists, err := g.remoteHasBranch(ctx, tmp, url, prepareBranch)
	if err != nil || exists {
		t.Fatalf("ls-remote: %v %v", exists, err)
	}
	dir := filepath.Join(tmp, "repo")
	if _, err := g.clone(ctx, tmp, dir, url, prepareBranch, false); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "platform.yaml"), []byte(shopSpec), 0o644)
	if _, _, err := g.commit(ctx, dir, "feat: x\n"); err != nil {
		t.Fatal(err)
	}
	if err := g.push(ctx, dir, prepareBranch); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(auths(), "x-access-token") {
		t.Fatalf("토큰이 자격 증명 도우미로 가지 않음: %v", auths())
	}
	if slices.Contains(auths(), "operator") {
		t.Fatalf("운영자 홈의 netrc를 씀: %v", auths())
	}
	if got := gitT(t, bare, "log", "-1", "--format=%an", "hopcharlie"); got != aiAuthorName {
		t.Fatalf("push한 커밋: %s", got)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	all, _ := json.Marshal(argv)
	if strings.Contains(string(cfg), testPushToken) || strings.Contains(string(all), testPushToken) {
		t.Fatal("토큰이 .git/config나 명령줄에 있음")
	}
	if !strings.Contains(string(cfg), url) {
		t.Fatalf("원격 URL은 토큰 없는 주소여야 함:\n%s", cfg)
	}
	for _, f := range []string{".git-credentials", ".config/git/credentials"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(f))); err == nil {
			t.Fatalf("운영자 설정의 store 도우미가 토큰을 %s에 저장함", f)
		}
	}

	// 틀린 토큰이면 실패하고, 오류에 토큰이 없다
	bad, _ := newGitRunner(t.TempDir(), srv.URL, secret("WRONG"+testPushToken))
	_, err = bad.remoteHasBranch(ctx, tmp, url, prepareBranch)
	if err == nil || strings.Contains(err.Error(), testPushToken) {
		t.Fatalf("틀린 토큰: %v", err)
	}
}

func TestCredentialHelperAnswersOnlyRemoteHost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git이 없음")
	}
	dir := t.TempDir()
	g, err := newGitRunner(dir, "https://github.com", secret(testPushToken))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, err := g.run(ctx, dir, "protocol=https\nhost=github.com\npath=org/shop.git\n\n", "credential", "fill")
	if err != nil || !strings.Contains(out, "username=x-access-token") || !strings.Contains(out, "password="+testPushToken) {
		t.Fatalf("원격 주소에는 토큰을 줘야 함: %v", err)
	}
	for _, other := range []string{
		"protocol=https\nhost=attacker.example\npath=lfs\n\n",
		"protocol=http\nhost=github.com\n\n",
		"protocol=https\nhost=github.com.attacker.example\n\n",
		"protocol=https\nhost=github.com:8443\n\n",
	} {
		out, err := g.run(ctx, dir, other, "credential", "fill")
		if strings.Contains(out, testPushToken) || err != nil && strings.Contains(err.Error(), testPushToken) {
			t.Fatalf("다른 주소에 토큰을 줌: %q", other)
		}
	}
	// 도우미 스크립트도 설정 범위와 별개로 다른 host에는 답하지 않는다
	sh, err := exec.LookPath("sh")
	if err != nil {
		return
	}
	ask := func(input string) string {
		cmd := exec.Command(sh, "-c", strings.TrimPrefix(g.helper(), "!")+" get")
		cmd.Env = append(os.Environ(), gitTokenEnv+"="+testPushToken)
		cmd.Stdin = strings.NewReader(input)
		b, _ := cmd.Output()
		return string(b)
	}
	if !strings.Contains(ask("protocol=https\nhost=github.com\n\n"), testPushToken) || strings.Contains(ask("protocol=https\nhost=attacker.example\n\n"), testPushToken) {
		t.Fatal("도우미는 github.com에만 답해야 함")
	}
}

func TestGitEnvAllowlist(t *testing.T) {
	dropped := map[string]string{
		"GIT_TRACE": "1", "GIT_SSL_NO_VERIFY": "1", "GIT_CONFIG_PARAMETERS": "'core.hookspath'='x'", "GIT_CONFIG_COUNT": "1",
		"GIT_CONFIG_SYSTEM": "x", "GIT_DIR": "x", "GIT_OBJECT_DIRECTORY": "x", "GIT_NAMESPACE": "x", "GIT_ASKPASS": "x",
		"AWS_SECRET_ACCESS_KEY": "x", "GH_TOKEN": "x", "ANTHROPIC_API_KEY": "x", "PLATFORM_TOKEN": "x", "XDG_CONFIG_HOME": "x",
		"HOMEDRIVE": "Z:", "HOMEPATH": `\operator`, "NETRC": "x",
	}
	for k, v := range dropped {
		t.Setenv(k, v)
	}
	t.Setenv("LC_ALL", "C")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	g, err := newGitRunner(t.TempDir(), "https://github.com", secret(testPushToken))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range g.env() {
		k, v, _ := strings.Cut(kv, "=")
		env[strings.ToUpper(k)] = v
	}
	for k := range dropped {
		if _, ok := env[k]; ok {
			t.Errorf("%s를 git에 넘김", k)
		}
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": g.global, "GIT_TERMINAL_PROMPT": "0", "GIT_LFS_SKIP_SMUDGE": "1",
		"LC_ALL": "C", "HTTPS_PROXY": "http://proxy.example:3128", gitTokenEnv: testPushToken, "GIT_AUTHOR_NAME": aiAuthorName,
		"HOME": g.home, "USERPROFILE": g.home, "GIT_CEILING_DIRECTORIES": g.ceiling,
	} {
		if env[k] != v {
			t.Errorf("%s=%q, want %q", k, env[k], v)
		}
	}
	if env["PATH"] == "" {
		t.Error("PATH는 넘겨야 함")
	}
}

func TestGitStopsWhenHooksAppear(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git이 없음")
	}
	dir := t.TempDir()
	g, err := newGitRunner(dir, "https://github.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(context.Background(), dir, "", "version"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(g.hooks, "pre-push"), []byte("#!/bin/sh\n"), 0o755)
	if _, err := g.run(context.Background(), dir, "", "version"); err == nil || !strings.Contains(err.Error(), "훅") {
		t.Fatalf("훅 디렉터리에 파일이 생기면 멈춰야 함: %v", err)
	}
}

// TestPrepareLFSRepoCannotLeakToken은 git-lfs를 쓰게 만든 레포(.gitattributes filter=lfs, .lfsconfig가 공격자 주소)가
// 운영자 컴퓨터의 git-lfs 설정(Git for Windows 기본값)을 타고 토큰을 빼내지 못하는지 본다.
func TestPrepareLFSRepoCannotLeakToken(t *testing.T) {
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs가 없음")
	}
	var mu sync.Mutex
	var hits []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="lfs"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer attacker.Close()
	github, bare := localRemote(t)
	work := filepath.Join(t.TempDir(), "w")
	gitT(t, "", "clone", "--quiet", bare, work)
	gitattributes := "* filter=lfs diff=lfs merge=lfs -text\n"
	_ = os.WriteFile(filepath.Join(work, ".gitattributes"), []byte(gitattributes), 0o644)
	_ = os.WriteFile(filepath.Join(work, ".lfsconfig"), []byte("[lfs]\n\turl = "+attacker.URL+"/lfs\n"), 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "--quiet", "-m", "hostile")
	gitT(t, work, "push", "--quiet", "origin", "main")
	operatorConfig(t, "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n")

	dockerfile := "FROM ghcr.io/example/shop:1.2.3\nUSER 65532\n"
	r := newPrepareRun(t, github, &fakePlatformServer{},
		write(".gitattributes", "* text=auto\n"),
		write(".lfsconfig", "[lfs]\n\turl = "+attacker.URL+"/again\n"),
		write("platform.yaml", shopSpec),
		write("hopcharlie.Dockerfile", dockerfile),
		aiReply{text: shopAnswer},
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := runPrepare(ctx, r.env, testRepo, ""); err != nil {
		t.Fatalf("%v\n%s", err, r.stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 0 {
		t.Fatalf("공격자 주소로 요청이 감: %v", hits)
	}
	for p, want := range map[string]string{"platform.yaml": shopSpec, "hopcharlie.Dockerfile": dockerfile, ".gitattributes": gitattributes} {
		if got := gitT(t, bare, "cat-file", "blob", "hopcharlie:"+p); got != strings.TrimSpace(want) {
			t.Fatalf("push한 %s가 검사한 내용과 다름 (LFS 포인터?): %q", p, got)
		}
	}
	blocked := 0
	for _, e := range r.sink.Sessions[0].Events {
		if e.Type == agentlog.Blocked && e.Tool == "write_file" {
			blocked++
		}
	}
	if blocked != 2 {
		t.Fatalf(".gitattributes와 .lfsconfig 쓰기는 막혀야 함: %d", blocked)
	}
	r.assertNoToken(t)
}

// TestPrepareIgnoresOperatorGitConfig는 운영자 git 설정의 주소 바꾸기(pushInsteadOf, insteadOf), 훅, 서명, store 도우미가
// hopai의 클론과 push에 끼어들지 못하는지 본다.
func TestPrepareIgnoresOperatorGitConfig(t *testing.T) {
	for _, c := range []struct{ name, extra string }{
		{"push 주소를 다른 레포로", "pushInsteadOf"},
		{"주소를 운영자 SSH 키로", "insteadOf"},
	} {
		t.Run(c.name, func(t *testing.T) {
			github, bare := localRemote(t)
			decoy, decoyBare := localRemote(t)
			hooks := t.TempDir()
			marker := filepath.ToSlash(filepath.Join(t.TempDir(), "hook-ran"))
			for _, h := range []string{"pre-commit", "post-commit", "pre-push"} {
				_ = os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\necho "+h+" >> '"+marker+"'\n"), 0o755)
			}
			cfg := "[credential]\n\thelper = store\n[core]\n\thooksPath = " + filepath.ToSlash(hooks) + "\n[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = hopai-no-such-gpg\n"
			if c.extra == "pushInsteadOf" {
				cfg += "[url \"" + decoy + "/\"]\n\tpushInsteadOf = " + github + "/\n"
			} else {
				cfg += "[url \"ssh://git@127.0.0.1:9/\"]\n\tinsteadOf = " + github + "/\n"
			}
			home := operatorConfig(t, cfg)
			r := newPrepareRun(t, github, &fakePlatformServer{},
				write("platform.yaml", shopSpec),
				write("hopcharlie.Dockerfile", "FROM x:1\n"),
				aiReply{text: shopAnswer},
			)
			if err := runPrepare(context.Background(), r.env, testRepo, ""); err != nil {
				t.Fatalf("%v\n%s", err, r.stderr)
			}
			if got := gitT(t, bare, "log", "-1", "--format=%an", "hopcharlie"); got != aiAuthorName {
				t.Fatalf("원래 레포에 push되지 않음: %s", got)
			}
			if got := gitT(t, decoyBare, "branch", "--list", "hopcharlie"); got != "" {
				t.Fatal("운영자 설정의 pushInsteadOf로 다른 레포에 push됨")
			}
			if _, err := os.Stat(filepath.FromSlash(marker)); err == nil {
				t.Fatal("운영자 설정의 훅이 돌았음")
			}
			if _, err := os.Stat(filepath.Join(home, ".git-credentials")); err == nil {
				t.Fatal("운영자 설정의 store 도우미가 무언가를 저장함")
			}
		})
	}
}

func TestPrepareFollowsRenamedRepo(t *testing.T) {
	github, bare := localRemote(t)
	r := newPrepareRun(t, github, &fakePlatformServer{rename: testRepo},
		write("platform.yaml", shopSpec),
		write("hopcharlie.Dockerfile", "FROM x:1\n"),
		aiReply{text: shopAnswer},
	)
	if err := runPrepare(context.Background(), r.env, "2026-Softbank-Hackathon-Charlie/old-shop", ""); err != nil {
		t.Fatalf("%v\n%s", err, r.stderr)
	}
	if got := gitT(t, bare, "log", "-1", "--format=%an", "hopcharlie"); got != aiAuthorName {
		t.Fatalf("지금 이름의 레포에 push해야 함: %s", got)
	}
	if !strings.Contains(r.stderr.String(), "지금 이름은 "+testRepo) || !strings.Contains(r.stdout.String(), "레포: "+testRepo) {
		t.Fatalf("바뀐 이름을 알려야 함:\n%s\n%s", r.stderr, r.stdout)
	}
}

func TestPrepareDeploymentMustBelongToRepo(t *testing.T) {
	for _, c := range []struct {
		name string
		plat *fakePlatformServer
		want string
	}{
		{"앱을 읽지 못함", &fakePlatformServer{appStatus: 404}, "확인할 수 없음"},
		{"배포 정보 없는 앱", &fakePlatformServer{appNoDeploy: true}, "배포 정보가 없어"},
		{"다른 레포의 앱", &fakePlatformServer{appRepo: "x/other"}, "레포 x/other의 앱"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPrepareRun(t, "file:///nowhere", c.plat)
			err := runPrepare(context.Background(), r.env, testRepo, "dep_1")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("멈춰야 함: %v", err)
			}
			if len(*r.argv) != 0 || len(r.ai.requests) != 0 {
				t.Fatal("확인하지 못하면 클론도 AI도 하지 않아야 함")
			}
		})
	}
}

// commitFixture는 localRemote를 클론해 setup으로 인덱스를 바꾸고 커밋한 작업 디렉터리다.
func commitFixture(t *testing.T, setup func(dir string)) string {
	t.Helper()
	_, bare := localRemote(t)
	dir := filepath.Join(t.TempDir(), "w")
	gitT(t, "", "clone", "--quiet", bare, dir)
	setup(dir)
	gitT(t, dir, "commit", "--quiet", "-m", "x")
	return dir
}

func TestVerifyCommitComparesValidatedBytes(t *testing.T) {
	dir := commitFixture(t, func(dir string) {
		_ = os.WriteFile(filepath.Join(dir, "platform.yaml"), []byte(shopSpec), 0o644)
		gitT(t, dir, "add", "-A")
	})
	g, err := newGitRunner(t.TempDir(), "file:///x", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	changes := []gitChange{{"A", "platform.yaml", "100644"}}
	spec := map[string][]byte{"platform.yaml": []byte(shopSpec)}
	files, checked := []string{"platform.yaml"}, []string{"platform.yaml"}
	if err := g.verifyCommit(ctx, dir, changes, spec, files, checked); err != nil {
		t.Fatalf("같은 내용인데 걸림: %v", err)
	}
	if err := g.verifyCommit(ctx, dir, changes, map[string][]byte{"platform.yaml": []byte(shopSpec + "x")}, files, checked); err == nil || !strings.Contains(err.Error(), "검사한 내용과 다름") {
		t.Fatalf("다른 내용을 놓침: %v", err)
	}
	if err := g.verifyCommit(ctx, dir, append(changes, gitChange{"A", "other.txt", "100644"}), spec, files, checked); err == nil || !strings.Contains(err.Error(), "검사하지 않은 파일") {
		t.Fatalf("검사하지 않은 파일을 놓침: %v", err)
	}
	// 검사한 파일이 커밋에 빠짐 (서브모듈 안에 쓴 Dockerfile은 git add가 넣지 않는다)
	withDocker := map[string][]byte{"platform.yaml": []byte(shopSpec), "vendor/lib/Dockerfile": []byte("FROM x\n")}
	if err := g.verifyCommit(ctx, dir, changes, withDocker, []string{"platform.yaml", "vendor/lib/Dockerfile"}, checked); err == nil || !strings.Contains(err.Error(), "커밋에 들어가지 않음") {
		t.Fatalf("커밋에 빠진 검사한 파일을 놓침: %v", err)
	}
	// 바뀌지 않은 Dockerfile이 커밋에 없음
	if err := g.verifyCommit(ctx, dir, changes, withDocker, files, []string{"platform.yaml", "vendor/lib/Dockerfile"}); err == nil || !strings.Contains(err.Error(), "일반 파일로 없음") {
		t.Fatalf("커밋에 없는 Dockerfile을 놓침: %v", err)
	}
}

func TestVerifyCommitRejectsSymlinkAndWrongCase(t *testing.T) {
	g, err := newGitRunner(t.TempDir(), "file:///x", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := []byte(shopSpec)

	// (a) platform.yaml이 심볼릭 링크(120000)로 커밋됨. core.symlinks=false면 작업 트리에서는 일반 파일로 보인다
	link := commitFixture(t, func(dir string) {
		oid := gitTIn(t, dir, shopSpec, "hash-object", "-w", "--stdin")
		gitT(t, dir, "update-index", "--add", "--cacheinfo", "120000,"+oid+",platform.yaml")
	})
	validated := map[string][]byte{"platform.yaml": spec}
	if err := g.verifyCommit(ctx, link, []gitChange{{"A", "platform.yaml", "120000"}}, validated, []string{"platform.yaml"}, []string{"platform.yaml"}); err == nil || !strings.Contains(err.Error(), "일반 파일이 아닌 것") {
		t.Fatalf("심볼릭 링크로 커밋된 것을 놓침: %v", err)
	}
	// 바뀐 목록에 없더라도 platform.yaml은 이름 그대로의 일반 파일이어야 한다
	if err := g.verifyCommit(ctx, link, nil, validated, nil, []string{"platform.yaml"}); err == nil || !strings.Contains(err.Error(), `모드 "120000"`) {
		t.Fatalf("심볼릭 링크인 platform.yaml을 놓침: %v", err)
	}
	if err := g.verifyCommit(ctx, link, []gitChange{{"T", "platform.yaml", "100644"}}, validated, nil, nil); err == nil || !strings.Contains(err.Error(), "일반 파일이 아닌 것") {
		t.Fatalf("종류가 바뀐 항목을 놓침: %v", err)
	}

	// (c) Windows에서 PLATFORM.YAML로 쓰면 검사는 platform.yaml로 읽지만 커밋에는 대문자 이름으로 들어간다
	upper := commitFixture(t, func(dir string) {
		oid := gitTIn(t, dir, shopSpec, "hash-object", "-w", "--stdin")
		gitT(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+oid+",PLATFORM.YAML")
	})
	validated = map[string][]byte{"platform.yaml": spec, "PLATFORM.YAML": spec}
	if err := g.verifyCommit(ctx, upper, []gitChange{{"A", "PLATFORM.YAML", "100644"}}, validated, []string{"PLATFORM.YAML"}, []string{"platform.yaml"}); err == nil || !strings.Contains(err.Error(), "platform.yaml가 그 이름의 일반 파일로 없음") {
		t.Fatalf("대소문자가 다른 platform.yaml을 놓침: %v", err)
	}
}

// gitTIn은 표준 입력을 주는 gitT다.
func gitTIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+hermeticGitConfig)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// TestPrepareRefusesWritesIntoSubmodule은 서브모듈(160000) 항목 아래에 쓰려는 AI를 클론 뒤 읽은 인덱스로 막는지 본다.
func TestPrepareRefusesWritesIntoSubmodule(t *testing.T) {
	github, bare := localRemote(t)
	work := filepath.Join(t.TempDir(), "w")
	gitT(t, "", "clone", "--quiet", bare, work)
	head := gitT(t, work, "rev-parse", "HEAD")
	gitT(t, work, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/lib")
	gitT(t, work, "commit", "--quiet", "-m", "submodule")
	gitT(t, work, "push", "--quiet", "origin", "main")
	r := newPrepareRun(t, github, &fakePlatformServer{},
		write("vendor/lib/Dockerfile", "FROM x:1\n"),
		write("platform.yaml", shopSpec),
		write("hopcharlie.Dockerfile", "FROM x:1\n"),
		aiReply{text: shopAnswer},
	)
	if err := runPrepare(context.Background(), r.env, testRepo, ""); err != nil {
		t.Fatalf("%v\n%s", err, r.stderr)
	}
	blocked := false
	for _, e := range r.sink.Sessions[0].Events {
		if e.Type == agentlog.Blocked && e.Tool == "write_file" && strings.Contains(fmt.Sprint(e.Output), "서브모듈") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("서브모듈 안에 쓰는 것은 막혀야 함")
	}
	if got := gitT(t, bare, "ls-tree", "hopcharlie", "vendor/lib"); !strings.HasPrefix(got, "160000 commit "+head) {
		t.Fatalf("서브모듈 항목은 그대로여야 함: %q", got)
	}
}

// TestGitIgnoresEnclosingRepoConfig는 임시 디렉터리가 다른 git 레포 안에 있어도 그 레포의 설정(주소 바꾸기 등)을 읽지 않는지 본다.
func TestGitIgnoresEnclosingRepoConfig(t *testing.T) {
	github, _ := localRemote(t)
	outer := t.TempDir()
	gitT(t, outer, "init", "--quiet")
	gitT(t, outer, "config", "url.ssh://git@127.0.0.1:9/.insteadOf", github+"/")
	tmp := filepath.Join(outer, "hopai-prepare-1")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	g, err := newGitRunner(tmp, github, "")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := g.remoteHasBranch(context.Background(), tmp, github+"/"+testRepo+".git", "main")
	if err != nil || !ok {
		t.Fatalf("둘러싼 레포의 설정이 끼어듦: %v %v", ok, err)
	}
}

func TestPrepareChecksRepoSizeBeforeClone(t *testing.T) {
	github, _ := localRemote(t)

	// 너무 크면 클론하지 않는다
	r := newPrepareRun(t, github, &fakePlatformServer{})
	r.github.size = 400 << 10 // KB
	err := runPrepare(context.Background(), r.env, testRepo, "")
	if err == nil || !strings.Contains(err.Error(), "상한 300 MB") {
		t.Fatalf("큰 레포는 멈춰야 함: %v", err)
	}
	for _, a := range *r.argv {
		if slices.Contains(a, "clone") {
			t.Fatal("큰 레포를 클론함")
		}
	}
	if auth := r.github.auth(); auth != "Bearer "+testPushToken {
		t.Fatalf("GitHub API는 푸시 토큰으로 읽음: %q", auth)
	}

	// 크기를 모르면 알리고 클론한 뒤 본다
	r = newPrepareRun(t, github, &fakePlatformServer{},
		write("platform.yaml", shopSpec),
		write("hopcharlie.Dockerfile", "FROM x:1\n"),
		aiReply{text: shopAnswer},
	)
	r.github.status = 500
	if err := runPrepare(context.Background(), r.env, testRepo, ""); err != nil {
		t.Fatalf("%v\n%s", err, r.stderr)
	}
	if !strings.Contains(r.stderr.String(), "클론한 뒤 크기를 봅니다") {
		t.Fatalf("크기를 모른다고 알려야 함:\n%s", r.stderr)
	}
}

func TestTreeSizeLimit(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"a", "sub/b"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, p), bytes.Repeat([]byte("x"), 600), 0o644)
	}
	if err := treeSize(dir, 2000); err != nil {
		t.Fatal(err)
	}
	if err := treeSize(dir, 1000); err == nil {
		t.Fatal("상한을 넘으면 멈춰야 함")
	}
}

func TestPrintPreparedNextStep(t *testing.T) {
	app := func(branch string, auto bool, instances ...apiv1.AppInstance) *apiv1.AppList {
		return &apiv1.AppList{Apps: []apiv1.App{
			{Name: "other", Deploy: &apiv1.AppDeploy{Repo: "x/other", Branch: "hopcharlie", AutoDeploy: true}},
			{Name: "shop", Deploy: &apiv1.AppDeploy{Repo: testRepo, Branch: branch, AutoDeploy: auto, Instances: instances}},
		}}
	}
	two := &apiv1.AppList{Apps: []apiv1.App{
		{Name: "shop-main", Deploy: &apiv1.AppDeploy{Repo: testRepo, Branch: "main", AutoDeploy: true}},
		{Name: "shop", Deploy: &apiv1.AppDeploy{Repo: testRepo, Branch: "hopcharlie", AutoDeploy: true}},
	}}
	for _, c := range []struct {
		name string
		apps *apiv1.AppList
		want string
	}{
		{"등록 안 됨", &apiv1.AppList{}, "charlie app add --repo " + testRepo + " --branch hopcharlie --env eks --watch"},
		{"앱 목록을 못 읽음", nil, "확인하지 못했습니다"},
		{"hopcharlie 자동 배포", app("hopcharlie", true), "push가 자동 재배포를 시작했습니다"},
		{"hopcharlie 인스턴스 자동 배포", app("main", false, apiv1.AppInstance{Branch: "hopcharlie", AutoDeploy: true}), "push가 자동 재배포를 시작했습니다"},
		{"자동 배포 꺼짐", app("hopcharlie", false), "charlie deploy shop --branch hopcharlie --watch"},
		{"다른 브랜치로 등록됨", app("main", true), "charlie app branch add shop hopcharlie --watch"},
		{"같은 레포의 앱 둘 중 hopcharlie를 배포하는 앱", two, "앱 shop가 이 레포의 hopcharlie 브랜치를 배포합니다. push가 자동 재배포를 시작했습니다"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			prep := &llm.Preparation{Summary: "s", Spec: &appspec.Spec{Name: "shop"}, Secrets: []llm.Secret{{Name: "ADMIN_PASSWORD", Purpose: "관리자"}}}
			printPrepared(&out, prepared{repo: testRepo, sha: "abc", changes: []gitChange{{"A", "platform.yaml", "100644"}}, prep: prep, apps: c.apps})
			if !strings.Contains(out.String(), c.want) || !strings.Contains(out.String(), "charlie app secret set shop ADMIN_PASSWORD") || !strings.Contains(out.String(), "A platform.yaml") {
				t.Fatalf("%q가 없음:\n%s", c.want, out.String())
			}
		})
	}

	// 등록된 앱 이름과 platform.yaml의 name이 다르면 등록된 이름을 쓰고 알린다. 레포에서 온 글의 제어 문자는 뺀다
	var out bytes.Buffer
	prep := &llm.Preparation{Summary: "s", Spec: &appspec.Spec{Name: "shop"}, Secrets: []llm.Secret{{Name: "ADMIN_PASSWORD", Purpose: "관리자"}}}
	apps := &apiv1.AppList{Apps: []apiv1.App{{Name: "shop-reg", Deploy: &apiv1.AppDeploy{Repo: testRepo, Branch: "hopcharlie", AutoDeploy: true}}}}
	printPrepared(&out, prepared{repo: testRepo, sha: "abc", changes: []gitChange{{"A", "evil‮\x1b[2J.txt", "100644"}}, prep: prep, apps: apps})
	s := out.String()
	if !strings.Contains(s, "앱 이름: shop-reg") || !strings.Contains(s, "charlie app secret set shop-reg ADMIN_PASSWORD") || !strings.Contains(s, "name(shop)") {
		t.Fatalf("등록된 앱 이름을 써야 함:\n%s", s)
	}
	if strings.ContainsAny(s, "‮\x1b") {
		t.Fatalf("제어 문자나 양방향 글자가 찍힘: %q", s)
	}
}

// TestGitReachesGitHubWithoutOperatorConfig는 운영자 git 설정 없이(시스템·전역 설정을 읽지 않고, 허용한 환경 변수만으로)
// 실제 GitHub에 닿는지 본다. 네트워크가 필요해 HOPAI_GITHUB_TEST=1일 때만 돈다.
func TestGitReachesGitHubWithoutOperatorConfig(t *testing.T) {
	if os.Getenv("HOPAI_GITHUB_TEST") != "1" {
		t.Skip("HOPAI_GITHUB_TEST=1일 때만 실제 GitHub에 붙는다")
	}
	dir := t.TempDir()
	g, err := newGitRunner(dir, "https://github.com", "")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := g.remoteHasBranch(context.Background(), dir, "https://github.com/octocat/Hello-World.git", "master")
	if err != nil || !ok {
		t.Fatalf("ls-remote: %v %v", ok, err)
	}
}
