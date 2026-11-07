package main

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// moimroom-legacy 같은 레포(app, postgres, caddy compose)와 조사 결과, 가짜 AI 응답은 llm/testdata/onboard에 있다.
const (
	onboardRepo    = "2026-Softbank-Hackathon-Charlie/moimroom-legacy"
	onboardFixture = "../../llm/testdata/onboard"
	testOnboarding = "onb-3f9a1c2b7d4e"
)

func onboardFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(onboardFixture, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// onboardRemote는 fixture 레포를 main에 올린 맨 레포를 만든다. GitHub 대신 쓸 file:// 주소를 돌려준다.
func onboardRemote(t *testing.T) (github, bare, work string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git이 없음")
	}
	root := t.TempDir()
	bare = filepath.Join(root, "remote", filepath.FromSlash(onboardRepo)+".git")
	gitT(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	work = filepath.Join(root, "work")
	gitT(t, root, "init", "--quiet", "-b", "main", work)
	src := filepath.Join(onboardFixture, "repo")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		full := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "--quiet", "-m", "legacy")
	gitT(t, work, "push", "--quiet", bare, "main")
	p := filepath.ToSlash(filepath.Join(root, "remote"))
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p, bare, work
}

func onboardArgsFor(t *testing.T, checkLog string) onboardArgs {
	t.Helper()
	a := onboardArgs{repo: onboardRepo, factsFile: filepath.Join(onboardFixture, "facts.json"), onboarding: testOnboarding}
	if checkLog != "" {
		a.checkLogFile = filepath.Join(t.TempDir(), "check.log")
		if err := os.WriteFile(a.checkLogFile, []byte(checkLog), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// pushArgs는 git push 명령의 인자들이다 (-c 설정 뒤).
func pushArgs(argv [][]string) []string {
	var out []string
	for _, a := range argv {
		i := 0
		for i+1 < len(a) && a[i] == "-c" {
			i += 2
		}
		if a[i] == "push" {
			out = append(out, strings.Join(a[i+1:], " "))
		}
	}
	return out
}

func decodeOnboardResult(t *testing.T, out string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("표준 출력은 결과 JSON 한 줄이어야 함:\n%s", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("결과 JSON: %v\n%s", err, out)
	}
	return m
}

// TestOnboardEndToEnd는 기본 브랜치 클론에 새 파일만 더해 hopcharlie/onboard에만 push하고, 결과 JSON의 head가 그 커밋인지 본다.
// 시험 배포가 실패한 뒤 다시 돌리면 그 브랜치 위에서 fast-forward로 이어 가고, 기록이 AI에 간다. main은 그대로다.
func TestOnboardEndToEnd(t *testing.T) {
	github, bare, _ := onboardRemote(t)
	plat := &fakePlatformServer{}
	main := gitT(t, bare, "rev-parse", "main")

	r := newPrepareRun(t, github, plat,
		aiReply{tool: "read_file", input: `{"path":"compose.yaml"}`},
		write("compose.yaml", "services: {}\n"),
		write("platform.yaml", onboardFile(t, "reply/platform.yaml")),
		write("hopcharlie.Dockerfile", onboardFile(t, "reply/hopcharlie.Dockerfile")),
		aiReply{text: onboardFile(t, "reply/answer.json")},
	)
	if err := runOnboard(context.Background(), r.env, onboardArgsFor(t, "")); err != nil {
		t.Fatalf("%v\n%s", err, r.stderr)
	}
	head := gitT(t, bare, "rev-parse", "hopcharlie/onboard")
	if gitT(t, bare, "rev-parse", "hopcharlie/onboard^") != main || gitT(t, bare, "rev-parse", "main") != main {
		t.Fatal("hopcharlie/onboard는 main 위에 커밋 하나여야 하고 main은 그대로여야 함")
	}
	if got := gitT(t, bare, "diff", "--name-status", "main", "hopcharlie/onboard"); got != "A\thopcharlie.Dockerfile\nA\tplatform.yaml" {
		t.Fatalf("기본 브랜치 대비 새 파일만: %q", got)
	}
	if body := gitT(t, bare, "log", "-1", "--format=%s|%b", "hopcharlie/onboard"); !strings.Contains(body, "feat: HopCharlie 온보딩 계약 추가|") || !strings.Contains(body, "hopai onboard "+testOnboarding+", AI 세션 ags_1") {
		t.Fatalf("커밋 메시지: %s", body)
	}
	if got := pushArgs(*r.argv); len(got) != 1 || got[0] != "--porcelain --no-follow-tags origin refs/heads/hopcharlie/onboard:refs/heads/hopcharlie/onboard" {
		t.Fatalf("push는 hopcharlie/onboard에만 한 번, 강제 없이: %v", got)
	}
	res := decodeOnboardResult(t, r.stdout.String())
	if res["kind"] != "onboard" || res["head"] != head || res["base"] != main || res["branch"] != "hopcharlie/onboard" || res["deployable"] != true || res["onboarding"] != testOnboarding {
		t.Fatalf("결과: %v", res)
	}
	if m, _ := res["mapping"].([]any); len(m) != 4 {
		t.Fatalf("mapping: %v", res["mapping"])
	}
	if f, _ := json.Marshal(res["files"]); string(f) != `["hopcharlie.Dockerfile","platform.yaml"]` {
		t.Fatalf("files: %s", f)
	}
	r.assertNoToken(t)

	// 시험 배포 실패 뒤 재시도: 있던 브랜치 위에서 더한 파일을 고친다
	r2 := newPrepareRun(t, github, plat,
		write("hopcharlie.Dockerfile", strings.Replace(onboardFile(t, "reply/hopcharlie.Dockerfile"), "USER 65532:65532", "USER 65532:65532\nENV HOME=/tmp", 1)),
		aiReply{text: strings.Replace(onboardFile(t, "reply/answer.json"), "feat: HopCharlie 온보딩 계약 추가", "fix: HOME을 /tmp로", 1)},
	)
	if err := runOnboard(context.Background(), r2.env, onboardArgsFor(t, "Error: /root/.cache: permission denied\npassword=Secr3tPw")); err != nil {
		t.Fatalf("%v\n%s", err, r2.stderr)
	}
	if gitT(t, bare, "rev-parse", "hopcharlie/onboard^") != head || gitT(t, bare, "rev-parse", "main") != main {
		t.Fatal("재시도는 전 커밋 위에 fast-forward여야 함")
	}
	prompt := r2.ai.requests[0]
	if !strings.Contains(prompt, "permission denied") || strings.Contains(prompt, "Secr3tPw") || !strings.Contains(prompt, "전에 만든 hopcharlie/onboard 브랜치") {
		t.Fatal("시험 배포 기록이 가려져 AI에 가야 하고, 이어서 고친다고 알려야 함")
	}
	res2 := decodeOnboardResult(t, r2.stdout.String())
	if res2["head"] != gitT(t, bare, "rev-parse", "hopcharlie/onboard") {
		t.Fatalf("결과 head: %v", res2["head"])
	}
	if f, _ := json.Marshal(res2["files"]); string(f) != `["hopcharlie.Dockerfile","platform.yaml"]` {
		t.Fatalf("files는 브랜치 전체가 더한 파일: %s", f)
	}
	r2.assertNoToken(t)
}

// TestOnboardRefusesBranchThatModifies는 있던 hopcharlie/onboard가 기본 브랜치의 파일을 고쳤으면 AI를 부르지 않고 push하지 않는지 본다.
func TestOnboardRefusesBranchThatModifies(t *testing.T) {
	github, bare, work := onboardRemote(t)
	gitT(t, work, "switch", "--quiet", "-c", "hopcharlie/onboard")
	if err := os.WriteFile(filepath.Join(work, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "commit", "--quiet", "-am", "tamper")
	gitT(t, work, "push", "--quiet", bare, "hopcharlie/onboard")
	before := gitT(t, bare, "rev-parse", "hopcharlie/onboard")
	r := newPrepareRun(t, github, &fakePlatformServer{})
	err := runOnboard(context.Background(), r.env, onboardArgsFor(t, ""))
	if err == nil || !strings.Contains(err.Error(), `"compose.yaml"가 바뀜`) {
		t.Fatalf("멈춰야 함: %v", err)
	}
	if len(r.ai.requests) != 0 || len(pushArgs(*r.argv)) != 0 || gitT(t, bare, "rev-parse", "hopcharlie/onboard") != before {
		t.Fatal("AI 호출과 push가 없어야 함")
	}
}

// TestOnboardExistingPlatformYAML은 기본 브랜치에 platform.yaml이 있으면 AI 없이 계약을 더할 수 없다고 답하고 종료 코드 3인지 본다.
func TestOnboardExistingPlatformYAML(t *testing.T) {
	github, bare, work := onboardRemote(t)
	if err := os.WriteFile(filepath.Join(work, "platform.yaml"), []byte("name: other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "--quiet", "-m", "spec")
	gitT(t, work, "push", "--quiet", bare, "main")
	r := newPrepareRun(t, github, &fakePlatformServer{})
	err := runOnboard(context.Background(), r.env, onboardArgsFor(t, ""))
	if code := prepareExit(io.Discard, err); code != exitNotDeployable {
		t.Fatalf("종료 코드 %d: %v", code, err)
	}
	res := decodeOnboardResult(t, r.stdout.String())
	if res["deployable"] != false || res["head"] != nil || len(res["blockers"].([]any)) != 1 {
		t.Fatalf("결과: %v", res)
	}
	if len(r.ai.requests) != 0 || len(pushArgs(*r.argv)) != 0 {
		t.Fatal("AI 호출과 push가 없어야 함")
	}
}

// TestOnboardNotDeployableDoesNotPush는 AI가 기존 파일을 고쳐야 한다고 답하면 커밋, push 없이 결과를 쓰고 종료 코드 3인지 본다.
func TestOnboardNotDeployableDoesNotPush(t *testing.T) {
	github, bare, _ := onboardRemote(t)
	r := newPrepareRun(t, github, &fakePlatformServer{},
		aiReply{text: `{"deployable":false,"blockers":[{"need":"기존 Dockerfile 수정","reason":"앱이 포트 80을 코드에 박아 둠"}],"summary":"앱 코드를 고쳐야 한다.","commitMessage":"","secrets":[],"notes":[],"mapping":[]}`},
	)
	err := runOnboard(context.Background(), r.env, onboardArgsFor(t, ""))
	if code := prepareExit(io.Discard, err); code != exitNotDeployable {
		t.Fatalf("종료 코드 %d: %v", code, err)
	}
	if res := decodeOnboardResult(t, r.stdout.String()); res["deployable"] != false || res["session"] != "ags_1" {
		t.Fatalf("결과: %v", res)
	}
	if len(pushArgs(*r.argv)) != 0 || strings.Contains(gitT(t, bare, "branch", "--list"), "hopcharlie/onboard") {
		t.Fatal("push하지 않아야 함")
	}
}

// TestOnboardChangesOK는 기본 브랜치 대비 바뀐 것이 허용된 새 일반 파일일 때만 push하는지 본다.
func TestOnboardChangesOK(t *testing.T) {
	ok := []gitChange{{"A", "platform.yaml", "100644"}, {"A", "app/hopcharlie.Dockerfile", "100644"}, {"A", "hopcharlie.entrypoint.sh", "100755"}}
	if files, err := onboardChangesOK(ok, "커밋"); err != nil || len(files) != 3 {
		t.Fatalf("%v %v", files, err)
	}
	for _, c := range []gitChange{
		{"M", "compose.yaml", "100644"}, {"D", "Dockerfile", "000000"}, {"T", "platform.yaml", "120000"},
		{"A", "hopcharlie.Dockerfile", "120000"}, {"A", ".dockerignore", "100644"}, {"A", ".github/workflows/x.yml", "100644"},
		{"A", "compose.override.yaml", "100644"}, {"A", "app/platform.yaml", "100644"},
	} {
		if _, err := onboardChangesOK([]gitChange{c}, "커밋"); err == nil {
			t.Errorf("%v: 막아야 함", c)
		}
	}
}

// TestReadFacts는 이 레포의 것이 아니거나 읽을 수 없는 버전이거나 차단 사유가 있는 조사 결과를 받지 않는지 본다.
func TestReadFacts(t *testing.T) {
	file := filepath.Join(onboardFixture, "facts.json")
	if _, err := readFacts(file, strings.ToUpper(onboardRepo)); err != nil {
		t.Fatal(err)
	}
	if _, err := readFacts(file, "org/other"); err == nil {
		t.Error("다른 레포의 조사 결과")
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(onboardFile(t, "facts.json")), &m)
	for name, mod := range map[string]func(){
		"버전": func() { m["version"] = 2 },
		"차단 사유": func() {
			m["report"] = map[string]any{"blockers": []any{map[string]any{"code": "storage_out_of_scope", "message": "MinIO"}}}
		},
		"GitHub": func() { m["github"] = map[string]any{"installed": false} },
	} {
		_ = json.Unmarshal([]byte(onboardFile(t, "facts.json")), &m)
		mod()
		b, _ := json.Marshal(m)
		p := filepath.Join(t.TempDir(), "facts.json")
		_ = os.WriteFile(p, b, 0o644)
		if _, err := readFacts(p, onboardRepo); err == nil {
			t.Errorf("%s: 받지 않아야 함", name)
		}
	}
}

// TestOnboardRerunUnchanged는 이미 맞는 계약이 있는 브랜치에서 다시 돌 때(크래시 뒤 재실행) 고칠 것이 없으면 커밋도 push도 없이
// 종료 코드 0으로 그 head를 결과로 쓰는지 본다. 브랜치의 platform.yaml이 검사에 걸리면 고치지 않은 채로는 끝나지 않는다.
func TestOnboardRerunUnchanged(t *testing.T) {
	github, bare, _ := onboardRemote(t)
	plat := &fakePlatformServer{}
	answer := onboardFile(t, "reply/answer.json")
	r := newPrepareRun(t, github, plat,
		write("platform.yaml", onboardFile(t, "reply/platform.yaml")),
		write("hopcharlie.Dockerfile", onboardFile(t, "reply/hopcharlie.Dockerfile")),
		aiReply{text: answer},
	)
	if err := runOnboard(context.Background(), r.env, onboardArgsFor(t, "")); err != nil {
		t.Fatalf("%v\n%s", err, r.stderr)
	}
	head := gitT(t, bare, "rev-parse", "hopcharlie/onboard")

	r2 := newPrepareRun(t, github, plat, aiReply{tool: "read_file", input: `{"path":"platform.yaml"}`}, aiReply{text: answer})
	err := runOnboard(context.Background(), r2.env, onboardArgsFor(t, ""))
	if code := prepareExit(io.Discard, err); code != 0 {
		t.Fatalf("종료 코드 %d: %v\n%s", code, err, r2.stderr)
	}
	res := decodeOnboardResult(t, r2.stdout.String())
	if res["head"] != head || res["deployable"] != true || gitT(t, bare, "rev-parse", "hopcharlie/onboard") != head {
		t.Fatalf("있던 head를 그대로 돌려줘야 함: %v", res)
	}
	if f, _ := json.Marshal(res["files"]); string(f) != `["hopcharlie.Dockerfile","platform.yaml"]` {
		t.Fatalf("files: %s", f)
	}
	if len(pushArgs(*r2.argv)) != 0 {
		t.Fatal("고칠 것이 없으면 push하지 않음")
	}
	r2.assertNoToken(t)

	// 브랜치의 platform.yaml이 검사에 걸리면(앱 이름이 다름) 고치지 않은 답은 세 번 걸려 실패한다
	github2, bare2, work := onboardRemote(t)
	gitT(t, work, "switch", "--quiet", "-c", "hopcharlie/onboard")
	_ = os.WriteFile(filepath.Join(work, "platform.yaml"), []byte(strings.Replace(onboardFile(t, "reply/platform.yaml"), "name: moimroom-legacy", "name: moimroom", 1)), 0o644)
	_ = os.WriteFile(filepath.Join(work, "hopcharlie.Dockerfile"), []byte(onboardFile(t, "reply/hopcharlie.Dockerfile")), 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "--quiet", "-m", "bad")
	gitT(t, work, "push", "--quiet", bare2, "hopcharlie/onboard")
	before := gitT(t, bare2, "rev-parse", "hopcharlie/onboard")
	r3 := newPrepareRun(t, github2, plat, aiReply{text: answer}, aiReply{text: answer}, aiReply{text: answer})
	if err := runOnboard(context.Background(), r3.env, onboardArgsFor(t, "")); err == nil || !strings.Contains(err.Error(), "온보딩 앱 이름") {
		t.Fatalf("검사에 걸려야 함: %v", err)
	}
	if len(pushArgs(*r3.argv)) != 0 || gitT(t, bare2, "rev-parse", "hopcharlie/onboard") != before || r3.stdout.Len() != 0 {
		t.Fatal("push하지 않고 결과도 쓰지 않아야 함")
	}
}
