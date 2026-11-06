package plan

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// CheckSourceRole은 원본으로 잡힌 CNPG 클러스터가 이전의 원본으로 맞는지 본다. 플랫폼 카탈로그(apps.yaml)의
// cnpgCluster는 사람이 SSM에 손으로 넣는 값이라, 이전으로 DB가 바뀐 뒤에도 낡은 채 남을 수 있다.
// 플랫폼의 Draft는 이름 형식만 보고, 엔진 precheck는 승인 뒤에야 본다. 그래서 승인 전에 여기서 막는다.
//
//   - 원본이 replica면 막는다. 이전의 원본은 primary여야 한다.
//   - 같은 네임스페이스에 다른 primary가 있으면, Plan의 원본 DSN 호스트(<클러스터>-rw...)가 어느 클러스터를 가리키는지 본다.
//     호스트가 원본이면 통과하고, 다른 primary를 가리키면 막는다. 2026-11-05 카탈로그의 원본은 낡은 독립 DB
//     moimroom-gcp-db였는데 DSN 호스트는 실제 primary moimroom-db-gke-rw를 가리켰다.
//     호스트로 판단할 수 없으면(프록시 이름 등) 막지 않고 경고만 한다. 이름 관례가 환경마다 달라서(eks는 moimroom-db와
//     moimroom-db-eks-rw) 호스트가 원본과 다르다는 이유만으로는 막지 않는다.
//
// allowOtherPrimaries는 같은 네임스페이스의 다른 primary 검사만 건너뛴다. replica 검사는 건너뛰지 않는다.
// 같은 네임스페이스의 목록을 읽지 못했으면(OthersUnknown) 두 번째 검사는 하지 못한다. 호출하는 쪽이 경고한다.
func CheckSourceRole(sc *SourceCluster, dsn string, allowOtherPrimaries bool) (warnings []string, err error) {
	if sc == nil {
		return nil, nil
	}
	where := sc.Name
	if sc.Namespace != "" {
		where = sc.Namespace + "/" + sc.Name
	}
	var errs []string
	if sc.Replica {
		errs = append(errs, fmt.Sprintf("원본 %s는 replica 클러스터임. 이전의 원본은 primary여야 함. 플랫폼 카탈로그의 원본 cnpgCluster가 실제 primary를 가리키는지 확인", where))
	}
	if len(sc.OtherPrimaries) > 0 && !allowOtherPrimaries {
		others := strings.Join(sc.OtherPrimaries, ", ")
		switch host := hostCluster(dsn); {
		case host == sc.Name:
			// DSN이 원본을 가리킨다. 같은 네임스페이스의 다른 DB는 이번 이전과 상관없다.
		case slices.Contains(sc.OtherPrimaries, host):
			errs = append(errs, fmt.Sprintf("Plan의 원본 DSN 호스트는 %s를 가리키는데 원본 클러스터로 잡힌 것은 %s임. 플랫폼 카탈로그(apps.yaml)가 낡았을 수 있음. 원본이 맞다고 확인했으면 -allow-other-primaries로 진행", host, where))
		default:
			warnings = append(warnings, fmt.Sprintf("같은 네임스페이스에 원본 %s 말고도 primary가 있음: %s. DSN 호스트로는 어느 쪽인지 알 수 없으므로 앱이 실제로 쓰는 DB가 %s가 맞는지 확인할 것", where, others, sc.Name))
		}
	}
	if len(errs) > 0 {
		return warnings, errors.New(strings.Join(errs, "\n"))
	}
	return warnings, nil
}

// hostCluster는 DSN 호스트의 첫 라벨에서 CNPG 서비스 접미사(-rw, -ro, -r)를 뗀 이름이다. 알 수 없으면 빈 문자열이다.
// DSN의 비밀번호 자리는 ${ENV} 참조일 수 있어 net/url로 읽지 않는다.
// 예: postgres://app:${PW}@moimroom-db-gke-rw.moimroom.gke.ts.net:5432/app → moimroom-db-gke
func hostCluster(dsn string) string {
	s := dsn
	if _, after, ok := strings.Cut(s, "://"); ok {
		s = after
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "]") {
		s = s[:i]
	}
	label, _, _ := strings.Cut(s, ".")
	for _, suffix := range []string{"-rw", "-ro", "-r"} {
		if name, ok := strings.CutSuffix(label, suffix); ok {
			return name
		}
	}
	return label
}
