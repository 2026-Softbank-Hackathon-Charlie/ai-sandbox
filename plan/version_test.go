package plan

import (
	"strings"
	"testing"
)

const pg18 = "ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie@sha256:64abc98a4387361afd6821a3a99f975763bbd87bbdde9449e75ad6002528a318"

func TestPostgresMajor(t *testing.T) {
	for img, want := range map[string]int{
		pg18: 18,
		"ghcr.io/cloudnative-pg/postgresql:17.11": 17,
		"ghcr.io/cloudnative-pg/postgresql:16":    16,
		"ghcr.io/cloudnative-pg/postgresql@sha256:64abc98a4387361afd6821a3a99f975763bbd87bbdde9449e75ad6002528a318": 0,
		"ghcr.io/cloudnative-pg/postgresql": 0,
		"localhost:5000/postgresql":         0,
		"":                                  0,
	} {
		if got := PostgresMajor(img); got != want {
			t.Errorf("%s: got %d want %d", img, got, want)
		}
	}
}

func TestCheckMajorVersion(t *testing.T) {
	d := draft(t)
	for _, c := range []struct {
		name, source, old, new, want string
	}{
		{"같은 메이저", pg18, "", "", ""},
		// 실제 클러스터의 기본 이미지는 18.6이다. 대상만 17이면 물리 복제가 되지 않는다
		{"대상이 17", pg18, "postgresql:18.6-minimal-trixie", "postgresql:17.11", "PostgreSQL 18 이미지여야 함"},
		{"대상 이미지 없음", pg18, "  imageName: ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie\n", "", "원본과 같은 PostgreSQL 18 이미지를 정해야 함"},
		{"원본 이미지를 모름", "", "postgresql:18.6-minimal-trixie", "postgresql:17.11", ""},
		{"원본 태그 없음(digest만)", "ghcr.io/cloudnative-pg/postgresql@sha256:64abc98a4387361afd6821a3a99f975763bbd87bbdde9449e75ad6002528a318", "postgresql:18.6-minimal-trixie", "postgresql:17.11", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := proposal(t)
			p.SourceImage = c.source
			if c.old != "" {
				if !strings.Contains(p.Manifest, c.old) {
					t.Fatalf("기준 매니페스트에 %q가 없음", c.old)
				}
				p.Manifest = strings.Replace(p.Manifest, c.old, c.new, 1)
			}
			is := p.Check(d)
			if c.want == "" {
				if len(is) > 0 {
					t.Fatalf("통과해야 함:\n%s", is.Error())
				}
				return
			}
			if !strings.Contains(is.Error(), c.want) {
				t.Fatalf("%q가 없음:\n%s", c.want, is.Error())
			}
		})
	}
}

func TestFixKeepsMajorVersion(t *testing.T) {
	// 진단 수정안으로 메이저 버전을 바꾸면 사본 검사에서 걸린다
	p := proposal(t)
	p.SourceImage = pg18
	_, is := Fix{Kind: KindManifestPatch, Patch: []Op{{"replace", "/spec/imageName", []byte(`"ghcr.io/cloudnative-pg/postgresql:17.6"`)}}}.Apply(p, draft(t))
	if !strings.Contains(is.Error(), "PostgreSQL 18 이미지여야 함") {
		t.Fatalf("메이저 버전 변경이 걸려야 함:\n%s", is.Error())
	}
	if _, is := (Fix{Kind: KindManifestPatch, Patch: []Op{{"replace", "/spec/imageName", []byte(`"ghcr.io/cloudnative-pg/postgresql:18.7"`)}}}).Apply(p, draft(t)); len(is) > 0 {
		t.Fatalf("같은 메이저의 버전 변경은 통과해야 함:\n%s", is.Error())
	}
}
