package plan

import (
	"errors"
	"fmt"
	"strings"
)

// CheckSourceRole은 원본으로 잡힌 CNPG 클러스터가 이전의 원본으로 맞는지 본다. 플랫폼 카탈로그(apps.yaml)의
// cnpgCluster는 사람이 SSM에 손으로 넣는 값이라, 이전으로 DB가 바뀐 뒤에도 낡은 채 남을 수 있다.
// 플랫폼의 Draft는 이름 형식만 보고, 엔진 precheck는 승인 뒤에야 본다. 그래서 승인 전에 여기서 막는다.
//
//   - 원본이 replica면 막는다. 이전의 원본은 primary여야 한다.
//   - 같은 네임스페이스에 다른 primary가 있으면 막는다. 앱이 실제로 쓰는 DB가 어느 쪽인지 이름만으로는 알 수 없다.
//     2026-11-05 카탈로그가 낡은 독립 DB moimroom-gcp-db를 가리켰고 실제 primary는 moimroom-db-gke였다.
//     맞는 원본이 확실하면 allowOtherPrimaries로 넘긴다.
//
// 같은 네임스페이스의 목록을 읽지 못했으면(OthersUnknown) 두 번째 검사는 하지 못한다. 호출하는 쪽이 경고한다.
func CheckSourceRole(sc *SourceCluster, allowOtherPrimaries bool) error {
	if sc == nil {
		return nil
	}
	where := sc.Name
	if sc.Namespace != "" {
		where = sc.Namespace + "/" + sc.Name
	}
	var msgs []string
	if sc.Replica {
		msgs = append(msgs, fmt.Sprintf("원본 %s는 replica 클러스터(spec.replica.enabled)임. 이전의 원본은 primary여야 함. 플랫폼 카탈로그의 원본 cnpgCluster가 실제 primary를 가리키는지 확인", where))
	}
	if len(sc.OtherPrimaries) > 0 && !allowOtherPrimaries {
		msgs = append(msgs, fmt.Sprintf("같은 네임스페이스에 원본 %s 말고도 primary가 있음: %s. 앱이 실제로 쓰는 DB가 %s가 맞는지 확인해야 함 (카탈로그가 낡은 DB를 가리키는 사고가 있었음). 맞다면 -allow-other-primaries로 진행",
			where, strings.Join(sc.OtherPrimaries, ", "), sc.Name))
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "\n"))
}
