package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

// git CLI로 클론, 커밋, push를 한다 (hopai prepare).
//
// 레포는 믿을 수 없다. 레포가 git에 명령을 실행시키거나(훅, git-lfs 같은 필터, fsmonitor) 토큰을 다른 곳으로 보내게 하지 못하도록
// git을 운영자 설정 없이 돌린다: 시스템·전역 설정을 읽지 않고(GIT_CONFIG_NOSYSTEM, 빈 GIT_CONFIG_GLOBAL), 환경 변수는 허용 목록만 넘기고,
// 명령마다 훅·필터·fsmonitor·심볼릭 링크를 끄고 원격 주소의 프로토콜만 허용한다. 명령마다 훅 디렉터리가 비어 있는지도 본다.
//
// 푸시 토큰은 git 자식 프로세스의 환경 변수(gitTokenEnv)로만 넘긴다. 자격 증명 도우미는 원격 주소(https://github.com) 하나에만
// 설정하고, 도우미도 git이 묻는 protocol과 host가 그 주소일 때만 답한다. 그래서 토큰은 명령줄, 원격 URL, .git/config, 오류 문자열,
// 다른 호스트에 가지 않는다.

// gitTokenEnv는 푸시 토큰을 git 자식 프로세스에만 넘기는 환경 변수다.
const gitTokenEnv = "HOPAI_GIT_TOKEN"

// AI 커밋의 작성자
const (
	aiAuthorName  = "HopCharlie AI"
	aiAuthorEmail = "ai@hopcharlie.dev"
)

// errPushRejected는 원격 브랜치가 그사이 바뀌어 fast-forward가 아닌 push가 거절됐을 때다.
var errPushRejected = errors.New("push가 거절됨")

// secret은 출력과 JSON에서 가려지는 문자열이다. 토큰이 실수로 로그나 오류에 찍히지 않게 한다.
type secret string

func (secret) String() string               { return mask.Placeholder }
func (secret) GoString() string             { return mask.Placeholder }
func (secret) MarshalJSON() ([]byte, error) { return []byte(`"` + mask.Placeholder + `"`), nil }

// gitRunner는 git 명령을 고정한 설정으로 돌린다.
type gitRunner struct {
	// hooks는 빈 디렉터리다. 명령마다 core.hooksPath로 주고, 명령이 끝날 때마다 비어 있는지 본다.
	hooks string
	// global은 빈 전역 설정 파일이다 (GIT_CONFIG_GLOBAL).
	global string
	// home은 빈 디렉터리다. git과 curl이 운영자 홈의 .netrc, _netrc, ~/.config/git/attributes를 읽지 않게 HOME으로 준다.
	home string
	// ceiling은 임시 디렉터리의 부모다 (GIT_CEILING_DIRECTORIES). 임시 디렉터리가 다른 git 레포 안에 있어도 그 레포 설정을 읽지 않는다.
	ceiling string
	// scheme, host는 원격 주소다. 이 프로토콜만 허용하고, 자격 증명 도우미는 이 주소에만 답한다.
	scheme, host string
	token        secret
	// trace는 테스트가 git 명령줄을 본다.
	trace func(args []string)
}

var gitHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

// newGitRunner는 tmp 아래에 빈 훅 디렉터리와 빈 전역 설정 파일을 만든다. base는 원격 주소의 앞부분이다
// (https://github.com. 테스트는 http://127.0.0.1:<포트>나 file://).
func newGitRunner(tmp, base string, token secret) (*gitRunner, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("GitHub 주소가 틀림: %w", err)
	}
	switch {
	case u.Scheme == "file":
	case (u.Scheme == "https" || u.Scheme == "http") && u.User == nil && gitHost.MatchString(u.Host):
	default:
		return nil, fmt.Errorf("GitHub 주소는 https://<host>여야 함: %q", base)
	}
	abs, err := filepath.Abs(tmp)
	if err != nil {
		return nil, err
	}
	g := &gitRunner{hooks: filepath.Join(abs, "hooks"), global: filepath.Join(abs, "gitconfig"), home: filepath.Join(abs, "home"), ceiling: filepath.Dir(abs),
		scheme: u.Scheme, host: u.Host, token: token}
	for _, d := range []string{g.hooks, g.home} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(g.global, nil, 0o600); err != nil {
		return nil, err
	}
	return g, nil
}

// helper는 자격 증명 도우미다. git이 묻는 protocol과 host가 원격 주소일 때만 GitHub 앱 토큰 형식(x-access-token)으로 답한다.
// store, erase에는 아무것도 하지 않으므로 토큰이 어디에도 저장되지 않는다. 명령줄에는 변수 이름만 있다.
func (g *gitRunner) helper() string {
	return `!f() { test "$1" = get || { cat >/dev/null; return 0; }; p=; h=; ` +
		`while IFS='=' read -r k v; do case "$k" in protocol) p=$v;; host) h=$v;; esac; done; ` +
		`test "$p" = '` + g.scheme + `' && test "$h" = '` + g.host + `' || return 0; ` +
		`echo username=x-access-token; printf 'password=%s\n' "$` + gitTokenEnv + `"; }; f`
}

// config는 모든 git 명령에 붙이는 설정이다.
func (g *gitRunner) config() []string {
	c := []string{
		// 운영자의 자격 증명 도우미는 모두 지운다. 토큰 도우미는 원격 주소에만 둔다(아래)
		"-c", "credential.helper=",
		// 레포가 git에 명령을 실행시키는 길(훅, fsmonitor, git-lfs 필터)과 심볼릭 링크를 끈다
		"-c", "core.hooksPath=" + g.hooks,
		"-c", "core.fsmonitor=false",
		"-c", "core.symlinks=false",
		"-c", "filter.lfs.process=", "-c", "filter.lfs.clean=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.required=false",
		// 원격 주소의 프로토콜만 쓴다
		"-c", "protocol.allow=never", "-c", "protocol." + g.scheme + ".allow=always",
		// AI 커밋은 고정 작성자로 남긴다. 운영자 개인 키로 서명하지 않는다
		"-c", "commit.gpgsign=false",
		// 클론한 바이트를 그대로 AI에 보이고 그대로 커밋한다
		"-c", "core.autocrlf=false",
	}
	if g.scheme != "file" {
		c = append(c, "-c", "credential."+g.scheme+"://"+g.host+".helper="+g.helper())
	}
	return c
}

// gitEnvKeep은 git 자식 프로세스에 넘기는 운영자 환경 변수다 (대소문자 무시, LC_*도). 나머지(GIT_*, 클라우드 키, 다른 토큰,
// 운영자 홈)는 넘기지 않는다. HOME과 USERPROFILE은 빈 디렉터리로 바꿔 준다.
var gitEnvKeep = []string{"PATH", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "LANG"}

func (g *gitRunner) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != "" && (slices.ContainsFunc(gitEnvKeep, func(k string) bool { return strings.EqualFold(k, name) }) || strings.HasPrefix(strings.ToUpper(name), "LC_")) {
			env = append(env, kv)
		}
	}
	env = append(env,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+g.global,
		"HOME="+g.home, "USERPROFILE="+g.home, "GIT_CEILING_DIRECTORIES="+g.ceiling,
		"GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1",
		"GIT_AUTHOR_NAME="+aiAuthorName, "GIT_AUTHOR_EMAIL="+aiAuthorEmail,
		"GIT_COMMITTER_NAME="+aiAuthorName, "GIT_COMMITTER_EMAIL="+aiAuthorEmail,
	)
	if g.token != "" {
		env = append(env, gitTokenEnv+"="+string(g.token))
	}
	return env
}

// run은 dir에서 git을 돌리고 표준 출력을 돌려준다. 오류에는 가린 표준 오류를 붙인다.
// 명령이 끝나면 훅 디렉터리가 비어 있는지 보고, 무언가 생겼으면 멈춘다.
func (g *gitRunner) run(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	full := append(g.config(), args...)
	if g.trace != nil {
		g.trace(full)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = g.env()
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if hooks, herr := os.ReadDir(g.hooks); herr != nil || len(hooks) > 0 {
		return "", fmt.Errorf("git %s 뒤에 빈 훅 디렉터리가 바뀜. 레포가 git에 훅을 심으려 한 것으로 보고 멈춤", args[0])
	}
	if err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, g.scrub(strings.TrimSpace(errb.String())))
	}
	return out.String(), nil
}

// scrub은 git 출력에서 토큰과 비밀처럼 보이는 값을 가린다. 토큰을 넘기지 않으니 나올 일은 없지만 한 번 더 막는다.
func (g *gitRunner) scrub(s string) string {
	if g.token != "" {
		s = strings.ReplaceAll(s, string(g.token), mask.Placeholder)
	}
	return mask.String(s)
}

// remoteHasBranch는 원격에 브랜치가 있는지 본다.
func (g *gitRunner) remoteHasBranch(ctx context.Context, dir, url, branch string) (bool, error) {
	out, err := g.run(ctx, dir, "", "ls-remote", "--heads", url, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// clone은 브랜치가 있으면 그 브랜치를, 없으면 기본 브랜치를 얕게 클론하고 새 브랜치를 만든다. 클론한 커밋을 돌려준다.
func (g *gitRunner) clone(ctx context.Context, parent, dir, url, branch string, exists bool) (string, error) {
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags"}
	if exists {
		args = append(args, "--branch", branch)
	}
	if _, err := g.run(ctx, parent, "", append(args, "--", url, dir)...); err != nil {
		return "", err
	}
	if !exists {
		if _, err := g.run(ctx, dir, "", "switch", "-c", branch); err != nil {
			return "", err
		}
	}
	out, err := g.run(ctx, dir, "", "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// gitChange는 커밋에 들어간 파일 하나다 (A 추가, M 수정, D 삭제, T 종류 바뀜 등). Mode는 커밋 뒤의 모드다
// (100644, 100755 일반 파일, 120000 심볼릭 링크, 160000 서브모듈, 000000 삭제).
type gitChange struct {
	Status, Path, Mode string
}

func (c gitChange) String() string { return c.Status + " " + c.Path }

// commit은 작업 트리 전체를 커밋하고 커밋 SHA와 바뀐 파일을 돌려준다.
// .gitignore에 걸린 파일도 넣는다. 깨끗한 클론에서 무시된 파일은 AI가 쓴 것뿐이기 때문이다.
func (g *gitRunner) commit(ctx context.Context, dir, message string) (string, []gitChange, error) {
	if _, err := g.run(ctx, dir, "", "add", "--all", "--force"); err != nil {
		return "", nil, err
	}
	// --raw -z: ":<옛 모드> <새 모드> <옛 id> <새 id> <상태>\0<경로>\0"
	out, err := g.run(ctx, dir, "", "diff", "--cached", "--raw", "--no-renames", "-z", "HEAD")
	if err != nil {
		return "", nil, err
	}
	var changes []gitChange
	parts := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		f := strings.Fields(strings.TrimPrefix(parts[i], ":"))
		if len(f) != 5 {
			return "", nil, fmt.Errorf("git diff --raw 출력을 읽을 수 없음: %q", parts[i])
		}
		changes = append(changes, gitChange{Status: f[4], Path: parts[i+1], Mode: f[1]})
	}
	if len(changes) == 0 {
		return "", nil, errors.New("커밋할 변경이 없음 (AI가 쓴 내용이 원래 파일과 같음)")
	}
	if _, err := g.run(ctx, dir, message, "commit", "--quiet", "--file", "-"); err != nil {
		return "", nil, err
	}
	sha, err := g.run(ctx, dir, "", "rev-parse", "HEAD")
	return strings.TrimSpace(sha), changes, err
}

// verifyCommit은 커밋이 검사한 것과 같은지 본다. 다르면 push하지 않는다.
//   - 바뀐 파일은 모두 검사한 파일이고(지운 파일 포함), 일반 파일이며(심볼릭 링크, 서브모듈, 종류 바뀜이 아님), 내용이 검사한 바이트와 같다.
//   - AI가 바꾼 파일(files)은 모두 커밋에 들어갔다 (서브모듈 안이나 대소문자가 다른 이름이면 빠진다).
//   - platform.yaml과 서비스 Dockerfile(checked)은 커밋에 그 이름 그대로, 일반 파일로, 검사한 바이트로 있다.
func (g *gitRunner) verifyCommit(ctx context.Context, dir string, changes []gitChange, validated map[string][]byte, files, checked []string) error {
	committed := map[string]bool{}
	for _, c := range changes {
		if strings.HasPrefix(c.Status, "T") || c.Mode == "120000" || c.Mode == "160000" {
			return fmt.Errorf("%q가 일반 파일이 아닌 것(모드 %s, 상태 %s)으로 커밋됨. 심볼릭 링크나 서브모듈은 push하지 않음", c.Path, c.Mode, c.Status)
		}
		want, ok := validated[c.Path]
		if !ok {
			return fmt.Errorf("검사하지 않은 파일 %q가 커밋에 들어감 (%s). push하지 않음", c.Path, c.Status)
		}
		if err := g.sameBlob(ctx, dir, c.Path, want); err != nil {
			return err
		}
		committed[c.Path] = true
	}
	for _, f := range files {
		if !committed[f] {
			return fmt.Errorf("검사한 파일 %q가 커밋에 들어가지 않음 (서브모듈 안이거나 대소문자가 다른 이름). push하지 않음", f)
		}
	}
	for _, p := range checked {
		mode, err := g.treeMode(ctx, dir, p)
		if err != nil {
			return err
		}
		if mode != "100644" && mode != "100755" {
			return fmt.Errorf("커밋에 %s가 그 이름의 일반 파일로 없음 (모드 %q). push하지 않음", p, mode)
		}
		if err := g.sameBlob(ctx, dir, p, validated[p]); err != nil {
			return err
		}
	}
	return nil
}

// sameBlob은 HEAD의 path 내용이 want와 바이트까지 같은지 본다.
func (g *gitRunner) sameBlob(ctx context.Context, dir, path string, want []byte) error {
	got, err := g.run(ctx, dir, "", "cat-file", "blob", "HEAD:"+path)
	if err != nil {
		return fmt.Errorf("커밋한 %s를 읽을 수 없음: %w", path, err)
	}
	if got != string(want) {
		return fmt.Errorf("커밋한 %s가 검사한 내용과 다름 (git 필터나 줄바꿈 변환). push하지 않음", path)
	}
	return nil
}

// treeMode는 HEAD에서 정확히 path인 항목의 모드다. 없으면 ""다 (대소문자만 다른 이름은 없는 것으로 본다).
func (g *gitRunner) treeMode(ctx context.Context, dir, path string) (string, error) {
	out, err := g.run(ctx, dir, "", "ls-tree", "-r", "-z", "HEAD", "--", path)
	if err != nil {
		return "", err
	}
	// "<모드> <종류> <id>\t<경로>\0"
	for _, e := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(e, "\t")
		if f := strings.Fields(meta); ok && name == path && len(f) == 3 {
			return f[0], nil
		}
	}
	return "", nil
}

// index는 클론한 커밋의 git 인덱스에서 심볼릭 링크(120000)와 서브모듈(160000) 항목을 모은다. AI가 거기에 쓰지 못하게 한다.
func (g *gitRunner) index(ctx context.Context, dir string) (llm.RepoIndex, error) {
	var idx llm.RepoIndex
	out, err := g.run(ctx, dir, "", "ls-files", "-s", "-z")
	if err != nil {
		return idx, err
	}
	// "<모드> <id> <단계>\t<경로>\0"
	for _, e := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(e, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "120000":
			idx.Symlinks = append(idx.Symlinks, name)
		case "160000":
			idx.Gitlinks = append(idx.Gitlinks, name)
		}
	}
	return idx, nil
}

// push는 브랜치를 같은 이름의 원격 브랜치에만 fast-forward로 올린다. 강제 push는 하지 않는다.
func (g *gitRunner) push(ctx context.Context, dir, branch string) error {
	ref := "refs/heads/" + branch
	out, err := g.run(ctx, dir, "", "push", "--porcelain", "--no-follow-tags", "origin", ref+":"+ref)
	if err != nil && strings.Contains(out+err.Error(), "[rejected]") {
		return fmt.Errorf("%w: 그사이 누가 %s 브랜치에 push함. hopai prepare를 다시 실행하세요 (%v)", errPushRejected, branch, err)
	}
	return err
}

// maxTreeBytes는 클론한 디렉터리(.git 포함)의 상한이다. 넘으면 AI를 부르지 않는다.
const maxTreeBytes = 300 << 20

// treeSize는 dir 아래 파일 크기의 합이 limit을 넘는지 본다. 넘으면 거기서 멈추고 오류를 돌려준다.
func treeSize(dir string, limit int64) error {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		if total > limit {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return err
	}
	if total > limit {
		return fmt.Errorf("클론한 레포가 %d MB를 넘음. 이렇게 큰 레포는 다루지 않음", limit>>20)
	}
	return nil
}
