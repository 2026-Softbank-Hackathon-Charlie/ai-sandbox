package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

// 레포 도구 (hopai prepare). 들여온 오픈소스 레포를 클론한 디렉터리 하나만 읽고 쓴다.
// 레포 내용은 믿을 수 없으므로 경로마다 심볼릭 링크를 풀어 클론 안인지, .git 밖인지 다시 보고, 밖이면 ErrBlocked로 막는다.
// 명령을 실행하는 도구는 없다. 커밋과 push는 검사를 통과한 뒤 실행기(hopai)가 한다.

// 레포 도구의 상한
const (
	// MaxListEntries는 list_files 한 번에 돌려주는 항목 수다.
	MaxListEntries = 400
	// MaxReadBytes는 read_file이 돌려주는 내용의 크기다. 이보다 큰 파일은 덮어쓸 수 없다(AI가 전체를 본 적이 없다).
	MaxReadBytes = 48 << 10
	// MaxWriteBytes는 write_file 한 번에 쓰는 내용의 크기다.
	MaxWriteBytes = 64 << 10
	// MaxWrites는 작업 한 번에 write_file로 쓸 수 있는 횟수다.
	MaxWrites = 40
)

// RepoIndex는 클론한 커밋의 git 인덱스에서 일반 파일이 아닌 항목이다 (git ls-files -s). 레포 루트 기준 / 경로다.
// 작업 트리에서는 일반 파일이나 빈 디렉터리로 보여도 git은 다르게 커밋하므로 AI가 쓰지 못하게 한다.
type RepoIndex struct {
	// Symlinks는 심볼릭 링크 항목(모드 120000)이다. core.symlinks=false로 클론하면 링크 대상 경로를 담은 파일로 보이고,
	// 거기에 쓰면 내용이 링크 대상으로 커밋된다.
	Symlinks []string
	// Gitlinks는 서브모듈 항목(모드 160000)이다. 그 아래에 쓴 파일은 커밋되지 않는다.
	Gitlinks []string
}

// repoDir는 클론 디렉터리다. root는 심볼릭 링크를 푼 절대 경로다.
type repoDir struct {
	root   string
	index  RepoIndex
	writes int
	// orig는 처음 쓰기 전 파일의 SHA-256이다 (없던 파일은 nil). 작업 트리가 바뀌었는지 볼 때 쓴다.
	orig map[string]*[32]byte
	// existingFiles가 nil이 아니면(onboard) 새 파일만 쓸 수 있다. 기본 브랜치에 있는 경로(소문자로 바꾼 것)는 고칠 수 없고,
	// 새 파일도 OnboardWritable이 받는 이름만 된다.
	existingFiles map[string]bool
	// allowUnchanged면 바꾼 파일이 없어도 된다 (onboard가 이미 맞는 계약이 있는 브랜치에서 이어서 돌 때).
	allowUnchanged bool
	// touched는 onboard에서 이번 작업에 읽거나 쓴 파일이다 (정리한 경로).
	touched map[string]bool
}

func openRepo(dir string, index RepoIndex) (*repoDir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("클론 디렉터리를 열 수 없음: %w", err)
	}
	return &repoDir{root: root, index: index, orig: map[string]*[32]byte{}}, nil
}

// RepoTools는 클론 디렉터리 dir 하나만 보는 도구(list_files, read_file, write_file)다.
func RepoTools(dir string, index RepoIndex) ([]Tool, error) {
	r, err := openRepo(dir, index)
	if err != nil {
		return nil, err
	}
	return r.tools(), nil
}

// cleanPath는 AI가 준 경로를 검사하고 정리한다. 레포 루트 기준, /로 나눈 상대 경로만 받는다. 빈 값은 루트(".")다.
func cleanPath(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if strings.ContainsAny(p, "\\:") || strings.ContainsFunc(p, controlOrFormat) {
		return "", fmt.Errorf("%w: 경로에 \\, :, 제어 문자, 서식 문자를 쓸 수 없음 (/로 나눈 레포 안 상대 경로만): %q", ErrBlocked, p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: 절대 경로는 쓸 수 없음 (레포 루트 기준 상대 경로만): %q", ErrBlocked, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: ..로 레포 밖에 나갈 수 없음: %q", ErrBlocked, p)
		}
		// Windows의 8.3 짧은 이름(GITATT~1 등)은 다른 파일을 가리킬 수 있어 막는다
		if shortName.MatchString(seg) {
			return "", fmt.Errorf("%w: ~숫자가 든 이름(Windows 짧은 이름)은 쓸 수 없음: %q", ErrBlocked, p)
		}
		if isGitName(seg) {
			return "", fmt.Errorf("%w: .git 아래는 볼 수도 고칠 수도 없음: %q", ErrBlocked, p)
		}
	}
	clean := path.Clean(p)
	// Windows의 장치 이름(NUL, CON, COM1 등)처럼 파일이 아닌 곳도 막는다
	if clean != "." && !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", fmt.Errorf("%w: 레포 안의 파일 경로가 아님: %q", ErrBlocked, p)
	}
	return clean, nil
}

var shortName = regexp.MustCompile(`~[0-9]`)

// controlOrFormat은 제어 문자와 서식 문자(양방향 글자, 폭 없는 글자 등)다. 경로와 터미널에 찍히는 글에서 막는다.
func controlOrFormat(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }

// isGitName은 경로 조각이 .git인지 본다. Windows는 대소문자를 가리지 않고 끝의 점과 공백을 버리므로 그것도 막는다.
func isGitName(seg string) bool { return sameName(seg, ".git") }

// sameName은 경로 조각이 name인지 대소문자와 끝의 점, 공백을 가리지 않고 본다.
func sameName(seg, name string) bool { return strings.EqualFold(strings.TrimRight(seg, ". "), name) }

// gitControlled는 git이 설정처럼 읽거나(필터·속성, 서브모듈, git-lfs 서버 주소) GitHub이 실행하는(워크플로) 파일이다.
// 레포에 원래 있던 것은 그대로 두지만 AI는 쓸 수 없다.
func gitControlled(p string) bool {
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if sameName(seg, ".gitattributes") || sameName(seg, ".lfsconfig") || sameName(seg, ".gitmodules") {
			return true
		}
	}
	return len(segs) >= 2 && sameName(segs[0], ".github") && sameName(segs[1], "workflows")
}

// check는 심볼릭 링크를 푼 실제 경로가 클론 안이고 .git 밖인지 본다.
func (r *repoDir) check(real, asked string) error {
	rel, err := filepath.Rel(r.root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: 레포 밖을 가리킴 (심볼릭 링크): %q", ErrBlocked, asked)
	}
	if slices.ContainsFunc(strings.Split(filepath.ToSlash(rel), "/"), isGitName) {
		return fmt.Errorf("%w: .git 아래를 가리킴 (심볼릭 링크): %q", ErrBlocked, asked)
	}
	return nil
}

// existing은 있는 경로를 심볼릭 링크까지 풀어 실제 경로와 정리한 경로를 돌려준다.
func (r *repoDir) existing(asked string) (string, string, error) {
	clean, err := cleanPath(asked)
	if err != nil {
		return "", "", err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(r.root, filepath.FromSlash(clean)))
	if err != nil {
		// 끊긴 링크의 대상 경로를 AI에 보이지 않게 오류 원문은 넘기지 않는다.
		return "", clean, fmt.Errorf("%q가 없음", clean)
	}
	return real, clean, r.check(real, clean)
}

// writeTarget은 쓸 파일의 실제 경로와 만들어야 할 상위 디렉터리를 돌려준다.
// 있는 가장 깊은 상위 디렉터리의 심볼릭 링크를 풀어 범위를 보고, 없는 디렉터리는 그 아래에 만든다.
func (r *repoDir) writeTarget(asked string) (target, parent, clean string, err error) {
	if clean, err = cleanPath(asked); err != nil {
		return "", "", "", err
	}
	if clean == "." {
		return "", "", "", errors.New("파일 경로가 필요함")
	}
	full := filepath.Join(r.root, filepath.FromSlash(clean))
	dir, rest := filepath.Dir(full), []string{}
	for {
		if _, err := os.Lstat(dir); err == nil || dir == r.root {
			break
		}
		rest = append([]string{filepath.Base(dir)}, rest...)
		dir = filepath.Dir(dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", clean, fmt.Errorf("%q의 상위 디렉터리를 열 수 없음", clean)
	}
	if err := r.check(real, clean); err != nil {
		return "", "", clean, err
	}
	parent = filepath.Join(append([]string{real}, rest...)...)
	return filepath.Join(parent, filepath.Base(full)), parent, clean, nil
}

func (r *repoDir) tools() []Tool {
	return []Tool{
		{
			Name:        "list_files",
			Description: fmt.Sprintf("레포 디렉터리 아래의 파일과 디렉터리를 얕은 것부터 본다(.git 제외, %d개까지). 디렉터리는 /로, 심볼릭 링크는 @로 끝나고 파일 뒤 괄호는 바이트 수다.", MaxListEntries),
			Properties: map[string]any{
				"path": map[string]any{"type": "string", "description": "레포 루트 기준 디렉터리 (/로 나눔, 비우면 루트)"},
			},
			Run: func(_ context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Path string }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				return r.list(in.Path)
			},
		},
		{
			Name:        "read_file",
			Description: fmt.Sprintf("레포의 텍스트 파일 하나를 읽는다. %d KiB까지만 돌려주고 넘으면 truncated가 true다. 바이너리는 읽지 않는다.", MaxReadBytes>>10),
			Properties: map[string]any{
				"path": map[string]any{"type": "string", "description": "레포 루트 기준 파일 경로 (/로 나눔)"},
			},
			Required: []string{"path"},
			Run: func(_ context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Path string }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				return r.read(in.Path)
			},
		},
		{
			Name: "write_file",
			Description: fmt.Sprintf("레포에 텍스트 파일 하나를 새로 쓰거나 덮어쓴다. 없는 디렉터리는 만든다. 내용은 %d KiB까지, 작업 한 번에 %d번까지. "+
				".git 아래, 심볼릭 링크, %d KiB보다 큰 기존 파일은 쓸 수 없다.", MaxWriteBytes>>10, MaxWrites, MaxReadBytes>>10),
			Properties: map[string]any{
				"path":    map[string]any{"type": "string", "description": "레포 루트 기준 파일 경로 (/로 나눔)"},
				"content": map[string]any{"type": "string", "description": "파일 전체 내용"},
			},
			Required: []string{"path", "content"},
			Run: func(_ context.Context, raw json.RawMessage) (any, error) {
				var in struct{ Path, Content string }
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, err
				}
				return r.write(in.Path, in.Content)
			},
		},
	}
}

// list는 디렉터리 아래를 얕은 것부터 MaxListEntries개까지 본다. 심볼릭 링크는 따라가지 않는다.
func (r *repoDir) list(asked string) (any, error) {
	real, clean, err := r.existing(asked)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%q는 디렉터리가 아님", clean)
	}
	type dir struct{ real, rel string }
	queue := []dir{{real, clean}}
	entries, truncated := []string{}, false
	for len(queue) > 0 && !truncated {
		d := queue[0]
		queue = queue[1:]
		list, err := os.ReadDir(d.real)
		if err != nil {
			return nil, fmt.Errorf("%q를 읽을 수 없음", d.rel)
		}
		for _, e := range list {
			if isGitName(e.Name()) {
				continue
			}
			if len(entries) == MaxListEntries {
				truncated = true
				break
			}
			rel := path.Join(d.rel, e.Name())
			switch {
			case e.Type()&fs.ModeSymlink != 0:
				entries = append(entries, rel+"@")
			case e.IsDir():
				entries = append(entries, rel+"/")
				queue = append(queue, dir{filepath.Join(d.real, e.Name()), rel})
			default:
				size := int64(0)
				if fi, err := e.Info(); err == nil {
					size = fi.Size()
				}
				entries = append(entries, fmt.Sprintf("%s (%d)", rel, size))
			}
		}
	}
	out := map[string]any{"path": clean, "entries": entries, "truncated": truncated}
	if truncated {
		out["note"] = fmt.Sprintf("%d개까지만 보임. 하위 디렉터리를 path로 다시 불러라", MaxListEntries)
	}
	return out, nil
}

// read는 텍스트 파일을 MaxReadBytes까지 읽는다.
func (r *repoDir) read(asked string) (any, error) {
	real, clean, err := r.existing(asked)
	if err != nil {
		return nil, err
	}
	if clean == "." {
		return nil, errors.New("파일 경로가 필요함")
	}
	// onboard는 실제 운영 앱의 레포다. 커밋된 비밀 파일은 열지 않고 이름만 보인다
	if r.existingFiles != nil && secretFile(path.Base(clean)) {
		return map[string]any{"path": clean, "secretFile": true, "note": "비밀 파일이라 이름만 보임. 내용은 읽지 않는다"}, nil
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%q는 일반 파일이 아님", clean)
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, fmt.Errorf("%q를 열 수 없음", clean)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%q를 읽을 수 없음", clean)
	}
	truncated := len(b) > MaxReadBytes
	if truncated {
		b = b[:MaxReadBytes]
		// 잘린 자리의 반쪽 글자는 버린다
		for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return nil, fmt.Errorf("%q는 UTF-8 텍스트가 아님 (바이너리는 읽지 않음)", clean)
	}
	content := string(b)
	if r.existingFiles != nil {
		content = mask.String(content)
		r.touched[clean] = true
	}
	out := map[string]any{"path": clean, "size": fi.Size(), "content": content}
	if truncated {
		out["truncated"] = true
		out["note"] = fmt.Sprintf("앞의 %d KiB만 보임. 이 파일은 덮어쓸 수 없다", MaxReadBytes>>10)
	}
	return out, nil
}

// write는 텍스트 파일 하나를 쓴다. 상한을 넘거나 범위 밖이면 ErrBlocked다.
func (r *repoDir) write(asked, content string) (any, error) {
	if r.writes >= MaxWrites {
		return nil, fmt.Errorf("%w: 쓰기는 작업 한 번에 %d번까지", ErrBlocked, MaxWrites)
	}
	if len(content) > MaxWriteBytes {
		return nil, fmt.Errorf("%w: 내용이 %d바이트임 (%d KiB까지)", ErrBlocked, len(content), MaxWriteBytes>>10)
	}
	if strings.IndexByte(content, 0) >= 0 {
		return nil, errors.New("내용에 NUL이 있음 (텍스트만 쓴다)")
	}
	target, parent, clean, err := r.writeTarget(asked)
	if err != nil {
		return nil, err
	}
	// 링크를 푼 실제 위치로도 본다 (링크로 .github/workflows 아래를 가리키는 경우). 이미 있는 파일은 마지막 이름까지 풀어서 본다
	rel, _ := filepath.Rel(r.root, target)
	refused := gitControlled(clean) || gitControlled(filepath.ToSlash(rel))
	if real, err := filepath.EvalSymlinks(target); err == nil {
		if rel, err := filepath.Rel(r.root, real); err == nil && gitControlled(filepath.ToSlash(rel)) {
			refused = true
		}
	}
	if refused {
		return nil, fmt.Errorf("%w: .gitattributes, .lfsconfig, .gitmodules, .github/workflows 아래는 쓰지 않음 (git과 GitHub이 설정이나 실행할 것으로 읽음): %q", ErrBlocked, clean)
	}
	if r.existingFiles != nil {
		for _, p := range []string{clean, filepath.ToSlash(rel)} {
			if r.existingFiles[strings.ToLower(p)] {
				return nil, fmt.Errorf("%w: 기본 브랜치에 있는 파일은 고치지 않음 (원본 배포가 쓰는 파일일 수 있음). 새 파일만 더한다: %q", ErrBlocked, clean)
			}
			if err := OnboardWritable(p); err != nil {
				return nil, err
			}
		}
	}
	if err := r.checkIndex(clean); err != nil {
		return nil, err
	}
	if p := r.caseMismatch(clean); p != "" {
		return nil, fmt.Errorf("%w: 레포에는 %q가 다른 대소문자로 있음 (git은 대소문자를 가린다): %q", ErrBlocked, p, clean)
	}
	if sameName(clean, appspec.FileName) && clean != appspec.FileName {
		return nil, fmt.Errorf("%w: 레포 루트의 %s는 소문자 이름 그대로 써야 함: %q", ErrBlocked, appspec.FileName, clean)
	}
	mode, created := fs.FileMode(0o644), true
	fi, err := os.Lstat(target)
	switch {
	case err == nil && fi.Mode()&fs.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: 심볼릭 링크에는 쓰지 않음: %q", ErrBlocked, clean)
	case err == nil && !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%q는 일반 파일이 아님", clean)
	case err == nil && fi.Size() > MaxReadBytes:
		return nil, fmt.Errorf("%w: %q는 %d KiB보다 커서 전체를 읽을 수 없으므로 덮어쓰지 않음", ErrBlocked, clean, MaxReadBytes>>10)
	case err == nil:
		mode, created = fi.Mode().Perm(), false
	}
	if _, seen := r.orig[clean]; !seen {
		r.orig[clean] = nil
		if !created {
			if b, err := os.ReadFile(target); err == nil {
				sum := sha256.Sum256(b)
				r.orig[clean] = &sum
			}
		}
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("%q의 디렉터리를 만들 수 없음", clean)
	}
	if err := os.WriteFile(target, []byte(content), mode); err != nil {
		return nil, fmt.Errorf("%q를 쓸 수 없음", clean)
	}
	r.writes++
	if r.touched != nil {
		r.touched[clean] = true
	}
	return map[string]any{"path": clean, "bytes": len(content), "created": created, "writesLeft": MaxWrites - r.writes}, nil
}

// secretFile은 onboard에서 읽지 않는 비밀 파일 이름인지 본다 (.env, .env.*, *.pem, *.key, id_*, secrets.*, *credentials*).
func secretFile(base string) bool {
	b := strings.ToLower(base)
	return b == ".env" || strings.HasPrefix(b, ".env.") || strings.HasSuffix(b, ".pem") || strings.HasSuffix(b, ".key") ||
		strings.HasPrefix(b, "id_") || strings.HasPrefix(b, "secrets.") || strings.Contains(b, "credentials")
}

// checkIndex는 경로가 git 인덱스의 심볼릭 링크이거나 서브모듈 안이면 막는다 (대소문자 무시).
func (r *repoDir) checkIndex(clean string) error {
	for _, l := range r.index.Symlinks {
		if strings.EqualFold(clean, l) {
			return fmt.Errorf("%w: git에서 심볼릭 링크인 파일에는 쓰지 않음 (내용이 링크 대상으로 커밋됨): %q", ErrBlocked, clean)
		}
	}
	for _, g := range r.index.Gitlinks {
		if strings.EqualFold(clean, g) || len(clean) > len(g) && strings.EqualFold(clean[:len(g)+1], g+"/") {
			return fmt.Errorf("%w: 서브모듈 %q 안에는 쓰지 않음 (커밋되지 않음): %q", ErrBlocked, g, clean)
		}
	}
	return nil
}

// caseMismatch는 경로의 앞부분 중 대소문자만 다른 이름으로 이미 있는 것을 돌려준다 (없으면 "").
// Windows와 macOS는 대소문자를 가리지 않아 그 파일에 쓰게 되지만, git과 플랫폼(리눅스)은 다른 파일로 본다.
func (r *repoDir) caseMismatch(clean string) string {
	dir := r.root
	segs := strings.Split(clean, "/")
	for i, seg := range segs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return ""
		}
		found := ""
		for _, e := range entries {
			if e.Name() == seg {
				found = seg
				break
			}
			if strings.EqualFold(e.Name(), seg) {
				found = e.Name()
			}
		}
		switch {
		case found == "":
			return ""
		case found != seg:
			return path.Join(append(segs[:i:i], found)...)
		}
		dir = filepath.Join(dir, seg)
	}
	return ""
}

// snapshot은 paths의 지금 내용이다. 검사를 통과한 바이트로 남겨, 실행기가 커밋된 내용과 비교한다.
func (r *repoDir) snapshot(paths []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, p := range paths {
		real, clean, err := r.existing(p)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(real)
		if err != nil {
			return nil, fmt.Errorf("%q를 읽을 수 없음", clean)
		}
		out[clean] = b
	}
	return out, nil
}

// changed는 도구로 쓴 파일 중 처음과 내용이 달라진 것을 돌려준다. 클론을 바꾸는 길은 이 도구뿐이다.
func (r *repoDir) changed() []string {
	var out []string
	for p, sum := range r.orig {
		real, _, err := r.existing(p)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(real)
		if err != nil {
			continue
		}
		if now := sha256.Sum256(b); sum == nil || now != *sum {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}
