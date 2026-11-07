package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
)

// moimroom-legacy처럼 app, postgres, caddy를 compose로 돌리고 파일은 외부 S3에 두는 레포와 조사 결과, 가짜 AI 응답(testdata/onboard).

func onboardFixture(t *testing.T) (dir string, existing []string, f *facts.Facts) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "repo")
	src := filepath.Join("testdata", "onboard", "repo")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		existing = append(existing, filepath.ToSlash(rel))
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("testdata", "onboard", "facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	f = &facts.Facts{}
	if err := json.Unmarshal(b, f); err != nil {
		t.Fatal(err)
	}
	return dir, existing, f
}

func onboardReply(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "onboard", "reply", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func onboardInput(dir string, existing []string, f *facts.Facts) OnboardInput {
	return OnboardInput{Repo: f.Repo, Branch: "hopcharlie/onboard", Onboarding: "onb-3f9a1c2b7d4e", Dir: dir, Facts: f, Existing: existing}
}

// TestOnboardRefusesExistingFiles는 쓰기 도구가 기본 브랜치에 있는 파일(대소문자 무시)과 원본 배포가 읽는 새 이름을 거절하고,
// platform.yaml과 hopcharlie. 파일만 새로 쓰게 하는지 본다. 거절한 파일은 그대로다.
func TestOnboardRefusesExistingFiles(t *testing.T) {
	dir, existing, _ := onboardFixture(t)
	r, err := openRepo(dir, RepoIndex{})
	if err != nil {
		t.Fatal(err)
	}
	r.existingFiles = map[string]bool{}
	for _, p := range existing {
		r.existingFiles[strings.ToLower(p)] = true
	}
	before, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	for _, p := range []string{
		"compose.yaml", "Dockerfile", "Caddyfile", "README.md", "./compose.yaml", "COMPOSE.yaml",
		".github/workflows/deploy.yml", ".github/dependabot.yml", ".github/hopcharlie.yml",
		".dockerignore", ".env", "compose.override.yaml", "docker-compose.yml", "app/Dockerfile", "sub/platform.yaml", "hopcharlie.",
		// 원본 빌드나 설정 glob이 집어 갈 수 있는 곳
		"hopcharlie.go", "conf.d/hopcharlie.conf", "initdb/hopcharlie.sql", "app/hopcharlie.Dockerfile", "hopcharlie.entrypoint.sh",
		"hopcharlie/main.go", "hopcharlie/app.js", "hopcharlie/.env", ".hopcharlie/x.sh", "hopcharlie.Web.Dockerfile",
	} {
		if _, err := r.write(p, "x\n"); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: 막아야 함: %v", p, err)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "compose.yaml")); string(after) != string(before) {
		t.Fatal("compose.yaml이 바뀜")
	}
	if _, err := os.Stat(filepath.Join(dir, ".dockerignore")); err == nil {
		t.Fatal(".dockerignore가 생김")
	}
	for _, p := range []string{"platform.yaml", "hopcharlie.Dockerfile", "hopcharlie.web.Dockerfile", "hopcharlie.Dockerfile.dockerignore", "hopcharlie.web.Dockerfile.dockerignore", "hopcharlie/entrypoint.sh", "hopcharlie/conf/nginx.conf"} {
		if _, err := r.write(p, "x\n"); err != nil {
			t.Errorf("%s: 새 파일은 써야 함: %v", p, err)
		}
	}
	// 이 브랜치에서 더한 새 파일은 다시 고칠 수 있다 (시험 배포 실패 뒤)
	if _, err := r.write("hopcharlie.Dockerfile", "y\n"); err != nil {
		t.Fatalf("더한 파일 다시 쓰기: %v", err)
	}
}

// TestOnboardMoimroomLegacy는 app, postgres, caddy compose(파일은 외부 S3)를 가짜 AI 응답 그대로 옮기면 플랫폼 검사(appspec)를 통과하는
// platform.yaml이 되고, 기존 파일에 쓰려던 시도는 거절된 채 끝나는지 본다.
func TestOnboardMoimroomLegacy(t *testing.T) {
	dir, existing, fx := onboardFixture(t)
	spec, dockerfile := onboardReply(t, "platform.yaml"), onboardReply(t, "hopcharlie.Dockerfile")
	f := &fakeAPI{replies: []reply{
		{tool: "read_file", toolInput: `{"path":"compose.yaml"}`},
		{tool: "write_file", toolInput: writeInput("Dockerfile", "FROM scratch\n")},
		{tool: "write_file", toolInput: writeInput("platform.yaml", spec)},
		{tool: "write_file", toolInput: writeInput("hopcharlie.Dockerfile", dockerfile)},
		{text: onboardReply(t, "answer.json")},
	}}
	a, sink := newAgent(t, f)
	in := onboardInput(dir, existing, fx)
	in.CheckLog = "Error: connect ECONNREFUSED <script>"
	o, sid, err := a.Onboard(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if o.Spec == nil || o.Spec.Name != "moimroom-legacy" || sid != "mem_1" {
		t.Fatalf("결과: %+v", o)
	}
	if _, errs := appspec.Check([]byte(spec)); len(errs) > 0 {
		t.Fatalf("platform.yaml이 플랫폼 검사를 통과해야 함: %v", errs)
	}
	var types []string
	for _, r := range o.Spec.Resources {
		types = append(types, r.Name+"="+r.Type)
	}
	if strings.Join(types, ",") != "db=postgres,files=s3" || !o.Spec.Services[0].Public {
		t.Fatalf("리소스와 public: %v %+v", types, o.Spec.Services)
	}
	if strings.Join(o.Files, ",") != "hopcharlie.Dockerfile,platform.yaml" || len(o.Mapping) != 4 || o.Secrets[0].Name != "SESSION_SECRET" {
		t.Fatalf("파일 %v, mapping %v, secrets %v", o.Files, o.Mapping, o.Secrets)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Dockerfile")); strings.Contains(string(b), "scratch") {
		t.Fatal("기존 Dockerfile이 바뀜")
	}
	user, _ := json.Marshal(f.requests[0]["messages"])
	for _, want := range []string{"moimroom-legacy", `\"publicService\": \"app\"`, "compose_services", `[\"app\",\"caddy\",\"db\"]`} {
		if !strings.Contains(string(user), want) {
			t.Errorf("AI 입력에 %q가 없음", want)
		}
	}
	if !strings.Contains(fmt.Sprint(f.requests[0]["messages"]), "ECONNREFUSED &lt;script&gt;") {
		t.Error("시험 배포 기록은 <, >를 바꿔 AI에 넣음")
	}
	if strings.Contains(string(user), "public.users\"") {
		t.Error("테이블 목록은 AI에 넣지 않음")
	}
	system, _ := json.Marshal(f.requests[0]["system"])
	for _, want := range []string{"새 파일만", "hopcharlie.Dockerfile.dockerignore", "mapping", "$(PGHOST)", "256Mi~1536Mi"} {
		if !strings.Contains(string(system), want) {
			t.Errorf("지시문에 %q가 없음", want)
		}
	}
	blocked := 0
	for _, e := range sink.Sessions[0].Events {
		if e.Type == "blocked" {
			blocked++
		}
	}
	if blocked != 1 || !strings.Contains(sink.Sessions[0].Info.Title, "onb-3f9a1c2b7d4e") {
		t.Fatalf("기존 Dockerfile 쓰기 한 번이 막혀 기록돼야 함: %d, %s", blocked, sink.Sessions[0].Info.Title)
	}
}

// TestOnboardContractChecks는 조사 결과와 맞지 않는 답(앱 이름, 빠진 리소스, 빠진 compose 서비스, 조사에 없는 시크릿,
// public이 아닌 프록시 대상)이 세 번 묻고 실패하는지 본다.
func TestOnboardContractChecks(t *testing.T) {
	answer := func(mod func(map[string]any)) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(onboardReply(t, "answer.json")), &m)
		mod(m)
		b, _ := json.Marshal(m)
		return string(b)
	}
	good := onboardReply(t, "answer.json")
	for _, c := range []struct {
		name, spec, answer, want string
	}{
		{"앱 이름", "name: moimroom\n", good, "온보딩 앱 이름 moimroom-legacy"},
		{"postgres 없음", "-db-postgres", good, "postgres 리소스가 있어야 함"},
		{"s3 없음", "-files-s3", good, "s3 리소스가 있어야 함"},
		{"caddy 빠짐", "", answer(func(m map[string]any) { m["mapping"] = m["mapping"].([]any)[:3] }), "compose 서비스 caddy가 없음"},
		{"조사에 없는 시크릿", "", answer(func(m map[string]any) {
			m["secrets"] = []map[string]string{{"name": "ADMIN_PASSWORD", "purpose": "관리자"}}
		}), "secrets[0].name"},
		{"옮기지 않는 이름을 시크릿으로", "", answer(func(m map[string]any) {
			m["secrets"] = []map[string]string{{"name": "POSTGRES_PASSWORD", "purpose": "DB"}}
		}), "excluded"},
		{"없는 서비스로 옮김", "", answer(func(m map[string]any) {
			m["mapping"].([]any)[0].(map[string]any)["platformService"] = "web"
		}), "mapping[0].platformService"},
		{"둘 다 씀", "", answer(func(m map[string]any) {
			m["mapping"].([]any)[0].(map[string]any)["resource"] = "db"
		}), "하나만 쓴다"},
		{"compose 서비스가 아님", "", answer(func(m map[string]any) {
			m["mapping"].([]any)[3].(map[string]any)["composeService"] = "nginx"
		}), "compose 서비스가 아님"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, existing, fx := onboardFixture(t)
			spec := onboardReply(t, "platform.yaml")
			switch {
			case strings.HasPrefix(c.spec, "name:"):
				spec = strings.Replace(spec, "name: moimroom-legacy\n", c.spec, 1)
			case c.spec == "-db-postgres":
				spec = strings.Replace(spec, "  - name: db\n    type: postgres\n", "", 1)
				spec = strings.NewReplacer(`"$(PGHOST)"`, `"db"`, `"$(PGDATABASE)"`, `"app"`, `"$(PGPASSWORD)"`, `"$(SESSION_SECRET)"`, `"$(PGPORT)"`, `"5432"`, `"$(PGUSER)"`, `"app"`).Replace(spec)
			case c.spec == "-files-s3":
				spec = strings.Replace(spec, "  - name: files\n    type: s3\n", "", 1)
				spec = strings.NewReplacer(`"$(S3_ACCESS_KEY)"`, `"$(AWS_SECRET_ACCESS_KEY)"`, `"$(S3_REGION)"`, `"ap-northeast-2"`, `"$(S3_SECRET_KEY)"`, `"$(SESSION_SECRET)"`).Replace(spec)
			}
			replies := []reply{
				{tool: "write_file", toolInput: writeInput("platform.yaml", spec)},
				{tool: "write_file", toolInput: writeInput("hopcharlie.Dockerfile", onboardReply(t, "hopcharlie.Dockerfile"))},
				{text: c.answer}, {text: c.answer}, {text: c.answer},
			}
			f := &fakeAPI{replies: replies}
			a, _ := newAgent(t, f)
			_, _, err := a.Onboard(context.Background(), onboardInput(dir, existing, fx))
			if !plan.IsRetryExhausted(err) || len(f.requests) != len(replies) {
				t.Fatalf("세 번 묻고 포기해야 함: %v, 요청 %d번", err, len(f.requests))
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("걸린 항목에 %q가 없음: %v", c.want, err)
			}
		})
	}
}

// TestOnboardNotDeployable은 기존 파일을 고쳐야만 하면 AI가 deployable false로 답하고, 파일 검사 없이 그 답을 받는지 본다.
func TestOnboardNotDeployable(t *testing.T) {
	dir, existing, fx := onboardFixture(t)
	f := &fakeAPI{replies: []reply{{text: `{"deployable":false,"blockers":[{"need":"기존 Dockerfile 수정","reason":"앱이 포트 80을 코드에 박아 둠 (src/Main.java)"}],"summary":"앱 코드를 고쳐야 한다.","commitMessage":"","secrets":[],"notes":[],"mapping":[]}`}}}
	a, sink := newAgent(t, f)
	o, _, err := a.Onboard(context.Background(), onboardInput(dir, existing, fx))
	if err != nil || o.Deployable || o.Spec != nil || len(o.Blockers) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
	if !strings.Contains(sink.Sessions[0].Summary, "기존 Dockerfile 수정") {
		t.Fatalf("세션 요약: %q", sink.Sessions[0].Summary)
	}
}

// TestOnboardReadRefusesSecretFiles는 onboard의 read_file이 커밋된 비밀 파일(.env 등)은 이름만 보이고, 다른 파일의 비밀값은 가려 보이는지 본다.
func TestOnboardReadRefusesSecretFiles(t *testing.T) {
	dir, existing, _ := onboardFixture(t)
	for p, c := range map[string]string{".env": "SESSION_SECRET=Zx81kQp2LmN4rT7v\n", "deploy/id_ed25519": "key\n", "config/app.yml": "db:\n  password: hunter2pw\n"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644)
		existing = append(existing, p)
	}
	r, _ := openRepo(dir, RepoIndex{})
	r.existingFiles, r.touched = map[string]bool{}, map[string]bool{}
	for _, p := range existing {
		r.existingFiles[strings.ToLower(p)] = true
	}
	for _, p := range []string{".env", "deploy/id_ed25519", ".ENV.production", "certs/tls.pem", "secrets.yaml", "gcp-credentials.json"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			_ = os.WriteFile(filepath.Join(dir, p), []byte("Zx81kQp2LmN4rT7v\n"), 0o644)
		}
		out, err := r.read(p)
		b, _ := json.Marshal(out)
		if err != nil || !strings.Contains(string(b), `"secretFile":true`) || strings.Contains(string(b), "Zx81") {
			t.Errorf("%s: 이름만 보여야 함: %s %v", p, b, err)
		}
	}
	out, err := r.read("config/app.yml")
	if b, _ := json.Marshal(out); err != nil || strings.Contains(string(b), "hunter2pw") || !strings.Contains(string(b), "password: ***") {
		t.Fatalf("비밀값은 가려 보여야 함: %s %v", b, err)
	}
	if !r.touched["config/app.yml"] {
		t.Fatal("읽은 파일은 touched에 남아야 함")
	}
}
