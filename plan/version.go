package plan

import (
	"fmt"
	"strconv"
	"strings"
)

// PostgresMajor는 CNPG PostgreSQL 이미지 태그의 메이저 버전이다.
// 예: ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie@sha256:… → 18. 태그가 없으면(digest만) 0이다.
func PostgresMajor(image string) int {
	name, _, _ := strings.Cut(image, "@")
	i := strings.LastIndex(name, ":")
	if i < 0 || strings.Contains(name[i:], "/") {
		return 0
	}
	tag := name[i+1:]
	end := strings.IndexFunc(tag, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(tag)
	}
	n, _ := strconv.Atoi(tag[:end])
	return n
}

// MajorLabel은 사람과 AI에 보여 줄 버전 표시다. 태그가 없어 메이저 버전을 모르면 그렇게 적는다.
func MajorLabel(image string) string {
	if n := PostgresMajor(image); n > 0 {
		return fmt.Sprintf("PostgreSQL %d", n)
	}
	return "PostgreSQL 메이저 버전 모름 (태그 없음)"
}

// checkMajorVersion은 대상 매니페스트의 PostgreSQL이 원본과 같은 메이저 버전인지 본다.
// 대상은 원본의 물리 복제본(pg_basebackup, streaming)이라 메이저 버전이 다르면 시작하지 못한다.
// 이 실패는 쿠버네티스 경고 이벤트를 남기지 않아, 엔진이 catchupTimeout까지 기다린 뒤에야 드러난다.
// 원본 이미지를 모르면(실행기가 원본을 읽지 못함) 보지 않는다.
func checkMajorVersion(p *Proposal) Issues {
	var is Issues
	want := PostgresMajor(p.SourceImage)
	if want == 0 || p.Plan.Target.Manifest != ManifestRef {
		return is
	}
	doc, err := parseYAMLObject(p.Manifest)
	if err != nil {
		return is // CheckManifest가 알린다
	}
	spec, _ := doc["spec"].(map[string]any)
	img, _ := spec["imageName"].(string)
	switch {
	case img == "":
		is.add("manifest.spec.imageName", "원본과 같은 PostgreSQL %d 이미지를 정해야 함 (원본 %s). 비우면 대상 클러스터의 기본 이미지를 써서 메이저 버전이 달라질 수 있음", want, p.SourceImage)
	case PostgresMajor(img) != want:
		is.add("manifest.spec.imageName", "PostgreSQL %d 이미지여야 함 (받은 값 %s, 원본 %s). 메이저 버전이 다르면 물리 복제가 되지 않음", want, img, p.SourceImage)
	}
	return is
}
