package plan

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SourceCluster는 실행기가 원본 CNPG 클러스터를 읽기 전용으로 읽은 값이다. 대상은 원본의 물리 복제본(hot standby)이라
// 원본과 맞아야 하는 값이 있다. 리허설은 임시 원본을 이 값으로 띄워 실제 원본을 흉내 낸다.
type SourceCluster struct {
	// Image는 원본이 지금 쓰는 이미지다 (status.image, 없으면 spec.imageName).
	Image string `json:"image"`
	// Parameters는 원본 spec.postgresql.parameters 중 StandbyParams에 든 것이다. 없는 설정은 원본도 기본값이다.
	Parameters map[string]string `json:"parameters,omitempty"`
}

// StandbyParams는 hot standby가 primary 이상이어야 시작하는 PostgreSQL 설정이다 (PostgreSQL 문서 "Hot Standby").
// 대상이 이 값을 원본보다 작게 두면 대상이 뜨지 않는다.
var StandbyParams = []string{"max_connections", "max_worker_processes", "max_wal_senders", "max_prepared_transactions", "max_locks_per_transaction"}

// PostgresMajor는 CNPG PostgreSQL 이미지 태그의 메이저 버전이다.
// 예: ghcr.io/cloudnative-pg/postgresql:17.11 → 17. 태그가 없으면(digest만) 0이다.
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

// checkSourceMatch는 대상 매니페스트가 원본과 맞아야 하는 값을 지키는지 본다. 원본을 읽지 못했으면 보지 않는다.
//   - 이미지: 원본과 같은 PostgreSQL 메이저 버전. 다르면 pg_basebackup이 거절되는데, 경고 이벤트가 없어
//     엔진이 catchupTimeout까지 기다린 뒤에야 드러난다. 비우면 대상 클러스터 운영자의 기본 이미지(실제 클러스터는 18.6)를
//     써서 원본(예: moimroom 17.11)과 달라질 수 있으므로 반드시 적게 한다.
//   - hot standby 설정: 원본이 정한 값은 대상도 정하고 원본 이상이어야 한다. 기본값을 짐작하지 않는다.
func checkSourceMatch(p *Proposal) Issues {
	var is Issues
	sc := p.SourceCluster
	if sc == nil || p.Plan.Target.Manifest != ManifestRef {
		return is
	}
	doc, err := parseYAMLObject(p.Manifest)
	if err != nil {
		return is // CheckManifest가 알린다
	}
	spec, _ := doc["spec"].(map[string]any)
	if want := PostgresMajor(sc.Image); want > 0 {
		switch img, _ := spec["imageName"].(string); {
		case img == "":
			is.add("manifest.spec.imageName", "원본과 같은 PostgreSQL %d 이미지를 정해야 함 (원본 %s). 비우면 대상 클러스터의 기본 이미지를 써서 메이저 버전이 달라질 수 있음", want, sc.Image)
		case PostgresMajor(img) != want:
			is.add("manifest.spec.imageName", "PostgreSQL %d 이미지여야 함 (받은 값 %s, 원본 %s). 메이저 버전이 다르면 물리 복제가 되지 않음", want, img, sc.Image)
		}
	}
	pg, _ := spec["postgresql"].(map[string]any)
	params, _ := pg["parameters"].(map[string]any)
	for _, k := range StandbyParams {
		// 원본을 실측했으면 접속 수·워커 수는 CheckCapacity가 실제 설정값으로 본다
		if p.Source != nil && (k == "max_connections" || k == "max_worker_processes") {
			continue
		}
		want, err := strconv.Atoi(sc.Parameters[k])
		if err != nil {
			continue // 원본도 기본값이거나 읽을 수 없는 값
		}
		path := "manifest.spec.postgresql.parameters." + k
		switch n, set, err := paramInt(params, k); {
		case errors.Is(err, errNotString):
			// 문자열이 아닌 값은 CheckManifest가 알린다
		case err != nil:
			is.add(path, "%s", err)
		case !set:
			is.add(path, "원본 값 %d 이상으로 정해야 함 (원본 spec에 정해져 있음. 대상은 hot standby라 작으면 뜨지 않음)", want)
		case n < want:
			is.add(path, "%d이면 부족함. 대상은 hot standby라 원본 값 %d 이상이어야 함", n, want)
		}
	}
	return is
}
