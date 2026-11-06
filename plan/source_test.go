package plan

import (
	"strings"
	"testing"
)

func TestCheckSourceRole(t *testing.T) {
	base := func(mut func(*SourceCluster)) *SourceCluster {
		sc := &SourceCluster{Name: "moimroom-db-gke", Namespace: "charlie-apps"}
		if mut != nil {
			mut(sc)
		}
		return sc
	}
	for _, c := range []struct {
		name   string
		sc     *SourceCluster
		allow  bool
		wantIn []string // 오류 메시지에 모두 들어 있어야 함. 비어 있으면 통과여야 함
	}{
		{"원본을 읽지 못했으면 보지 않음", nil, false, nil},
		{"primary 하나뿐", base(nil), false, nil},
		{"원본이 replica", base(func(s *SourceCluster) { s.Replica = true }), false, []string{"charlie-apps/moimroom-db-gke", "replica"}},
		{"같은 네임스페이스에 다른 primary (2026-11-05 사고)", base(func(s *SourceCluster) {
			s.Name = "moimroom-gcp-db"
			s.OtherPrimaries = []string{"moimroom-db-gke"}
		}), false, []string{"moimroom-gcp-db", "moimroom-db-gke", "-allow-other-primaries"}},
		{"다른 primary를 알고 넘김", base(func(s *SourceCluster) { s.OtherPrimaries = []string{"moimroom-gcp-db"} }), true, nil},
		{"replica는 다른 primary를 허용해도 막힘", base(func(s *SourceCluster) {
			s.Replica = true
			s.OtherPrimaries = []string{"moimroom-gcp-db"}
		}), true, []string{"replica"}},
		{"둘 다 걸리면 모두 알림", base(func(s *SourceCluster) {
			s.Replica = true
			s.OtherPrimaries = []string{"moimroom-gcp-db"}
		}), false, []string{"replica", "moimroom-gcp-db"}},
		{"목록을 읽지 못함은 막지 않음", base(func(s *SourceCluster) { s.OthersUnknown = true }), false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckSourceRole(c.sc, c.allow)
			if len(c.wantIn) == 0 {
				if err != nil {
					t.Fatalf("통과여야 함: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("걸려야 함 (%v)", c.wantIn)
			}
			for _, w := range c.wantIn {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("오류에 %q가 없음: %v", w, err)
				}
			}
		})
	}
}
