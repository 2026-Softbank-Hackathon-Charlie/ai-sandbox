package plan

import (
	"strings"
	"testing"
)

const (
	dsnGKE = "postgres://app:${GKE_MOIMROOM_PGPASSWORD}@moimroom-db-gke-rw.moimroom.gke.ts.net:5432/app?sslmode=require"
	dsnEKS = "postgres://app:${EKS_MOIMROOM_PGPASSWORD}@moimroom-db-eks-rw.moimroom.eks.ts.net:5432/app?sslmode=require"
)

func TestCheckSourceRole(t *testing.T) {
	base := func(name string, mut func(*SourceCluster)) *SourceCluster {
		sc := &SourceCluster{Name: name, Namespace: "charlie-apps"}
		if mut != nil {
			mut(sc)
		}
		return sc
	}
	for _, c := range []struct {
		name     string
		sc       *SourceCluster
		dsn      string
		allow    bool
		wantErr  []string // 오류 메시지에 모두 들어 있어야 함. 비어 있으면 오류가 없어야 함
		wantWarn int
	}{
		{"원본을 읽지 못했으면 보지 않음", nil, dsnGKE, false, nil, 0},
		{"primary 하나뿐", base("moimroom-db-gke", nil), dsnGKE, false, nil, 0},
		{"원본이 replica", base("moimroom-db-gke", func(s *SourceCluster) { s.Replica = true }), dsnGKE, false, []string{"charlie-apps/moimroom-db-gke", "replica"}, 0},
		// 2026-11-05: 카탈로그의 원본은 낡은 독립 DB인데 DSN 호스트는 실제 primary를 가리켰다
		{"DSN이 다른 primary를 가리킴 (2026-11-05 사고)", base("moimroom-gcp-db", func(s *SourceCluster) { s.OtherPrimaries = []string{"moimroom-db-gke"} }),
			dsnGKE, false, []string{"moimroom-db-gke", "moimroom-gcp-db", "-allow-other-primaries"}, 0},
		// 카탈로그를 고친 뒤에도 독립 DB는 남아 있다. 올바른 원본은 막히면 안 된다
		{"DSN이 원본을 가리킴, 무관한 DB가 남아 있음", base("moimroom-db-gke", func(s *SourceCluster) { s.OtherPrimaries = []string{"moimroom-gcp-db"} }), dsnGKE, false, nil, 0},
		{"DSN으로 판단 못함은 경고만", base("moimroom-db-gke", func(s *SourceCluster) { s.OtherPrimaries = []string{"moimroom-gcp-db"} }),
			"postgres://app:${PW}@db-gke.hopcharlie-db-network.svc.cluster.local:5432/app", false, nil, 1},
		{"DSN이 비어 있으면 경고만", base("moimroom-db-gke", func(s *SourceCluster) { s.OtherPrimaries = []string{"moimroom-gcp-db"} }), "", false, nil, 1},
		{"다른 primary를 알고 넘김", base("moimroom-gcp-db", func(s *SourceCluster) { s.OtherPrimaries = []string{"moimroom-db-gke"} }), dsnGKE, true, nil, 0},
		{"replica는 허용 플래그로 넘길 수 없음", base("moimroom-db-gke", func(s *SourceCluster) {
			s.Replica = true
			s.OtherPrimaries = []string{"moimroom-gcp-db"}
		}), dsnGKE, true, []string{"replica"}, 0},
		{"둘 다 걸리면 모두 알림", base("moimroom-gcp-db", func(s *SourceCluster) {
			s.Replica = true
			s.OtherPrimaries = []string{"moimroom-db-gke"}
		}), dsnGKE, false, []string{"replica", "moimroom-db-gke"}, 0},
		{"목록을 읽지 못함은 막지 않음", base("moimroom-db-gke", func(s *SourceCluster) { s.OthersUnknown = true }), dsnGKE, false, nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			warns, err := CheckSourceRole(c.sc, c.dsn, c.allow)
			if len(warns) != c.wantWarn {
				t.Fatalf("경고 %d개여야 함: %v", c.wantWarn, warns)
			}
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("통과여야 함: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("걸려야 함 (%v)", c.wantErr)
			}
			for _, w := range c.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("오류에 %q가 없음: %v", w, err)
				}
			}
		})
	}
}

func TestHostCluster(t *testing.T) {
	for dsn, want := range map[string]string{
		dsnGKE: "moimroom-db-gke",
		dsnEKS: "moimroom-db-eks", // 클러스터 이름(moimroom-db)과 다르다. 호스트 이름만으로 원본을 단정하면 안 되는 이유
		"postgres://app:${PW}@moimroom-db-onprem-ro.moimroom.onprem.ts.net:5432/app":   "moimroom-db-onprem",
		"postgres://app:${PW}@moimroom-db-gke-r:5432/app":                              "moimroom-db-gke",
		"postgres://app:pw@db-gke.hopcharlie-db-network.svc.cluster.local/app":         "db-gke",
		"postgres://app:p@ss@moimroom-db-gke-rw.ns.svc:5432/app?host=other-rw.example": "moimroom-db-gke",
		"postgres://[::1]:5432/app":                                                    "[::1]",
		"":                                                                             "",
	} {
		if got := hostCluster(dsn); got != want {
			t.Errorf("hostCluster(%q) = %q, want %q", dsn, got, want)
		}
	}
}
