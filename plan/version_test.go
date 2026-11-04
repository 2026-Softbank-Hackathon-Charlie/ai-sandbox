package plan

import (
	"strings"
	"testing"
)

// moimroom 원본(moimroom/deploy/cnpg/cluster.yaml)은 PostgreSQL 17.11로 고정돼 있다.
// 18.6은 실제 클러스터 CNPG 운영자의 기본 이미지다. imageName을 비우면 이것으로 뜬다.
const (
	pg17 = "ghcr.io/cloudnative-pg/postgresql:17.11"
	pg18 = "ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie@sha256:64abc98a4387361afd6821a3a99f975763bbd87bbdde9449e75ad6002528a318"
)

func TestPostgresMajor(t *testing.T) {
	for img, want := range map[string]int{
		pg18:                                   18,
		pg17:                                   17,
		"ghcr.io/cloudnative-pg/postgresql:16": 16,
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

func TestMajorLabel(t *testing.T) {
	if got := MajorLabel(pg18); got != "PostgreSQL 18" {
		t.Fatalf("태그 있음: %s", got)
	}
	// 태그가 없으면 "PostgreSQL 0"이 아니라 모른다고 보여 준다
	if got := MajorLabel("ghcr.io/cloudnative-pg/postgresql@sha256:64abc98a4387361afd6821a3a99f975763bbd87bbdde9449e75ad6002528a318"); !strings.Contains(got, "모름") {
		t.Fatalf("태그 없음: %s", got)
	}
}

func TestCheckSourceMatch(t *testing.T) {
	d := draft(t)
	for _, c := range []struct {
		name     string
		source   *SourceCluster
		old, new string
		want     string
	}{
		{"같은 메이저", &SourceCluster{Image: pg17}, "", "", ""},
		{"원본을 모름", nil, "postgresql:17.11", "postgresql:18.6", ""},
		{"대상이 18", &SourceCluster{Image: pg17}, "postgresql:17.11", "postgresql:18.6", "PostgreSQL 17 이미지여야 함"},
		// 비우면 대상 클러스터 운영자 기본 이미지(18.6)로 떠서 원본 17.11과 달라진다
		{"대상 이미지 없음", &SourceCluster{Image: pg17}, "  imageName: ghcr.io/cloudnative-pg/postgresql:17.11\n", "", "원본과 같은 PostgreSQL 17 이미지를 정해야 함"},
		{"원본 태그 없음(digest만)", &SourceCluster{Image: "ghcr.io/cloudnative-pg/postgresql@sha256:64abc98a4387361afd6821a3a99f975763bbd87bbdde9449e75ad6002528a318"}, "postgresql:17.11", "postgresql:18.6", ""},
		// 원본이 정한 hot standby 설정은 대상도 원본 이상으로 정해야 한다
		{"원본이 정한 설정을 대상이 안 정함", &SourceCluster{Image: pg17, Parameters: map[string]string{"max_wal_senders": "20"}}, "", "", "max_wal_senders: 원본 값 20 이상으로 정해야 함"},
		{"원본보다 작음", &SourceCluster{Image: pg17, Parameters: map[string]string{"max_locks_per_transaction": "256"}}, "  instances: 2\n", "  instances: 2\n  postgresql:\n    parameters:\n      max_locks_per_transaction: \"64\"\n", "64이면 부족함"},
		{"원본 이상", &SourceCluster{Image: pg17, Parameters: map[string]string{"max_wal_senders": "20"}}, "  instances: 2\n", "  instances: 2\n  postgresql:\n    parameters:\n      max_wal_senders: \"20\"\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := proposal(t)
			p.SourceCluster = c.source
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

func TestFixKeepsSourceMatch(t *testing.T) {
	// 진단 수정안으로 메이저 버전을 바꾸면 사본 검사에서 걸린다
	p := proposal(t)
	p.SourceCluster = &SourceCluster{Image: pg17}
	_, is := Fix{Kind: KindManifestPatch, Patch: []Op{{"replace", "/spec/imageName", []byte(`"ghcr.io/cloudnative-pg/postgresql:18.6"`)}}}.Apply(p, draft(t))
	if !strings.Contains(is.Error(), "PostgreSQL 17 이미지여야 함") {
		t.Fatalf("메이저 버전 변경이 걸려야 함:\n%s", is.Error())
	}
	if _, is := (Fix{Kind: KindManifestPatch, Patch: []Op{{"replace", "/spec/imageName", []byte(`"ghcr.io/cloudnative-pg/postgresql:17.6"`)}}}).Apply(p, draft(t)); len(is) > 0 {
		t.Fatalf("같은 메이저의 버전 변경은 통과해야 함:\n%s", is.Error())
	}
}
