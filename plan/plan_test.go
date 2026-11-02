package plan

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func draft(t *testing.T) *eplan.Plan {
	t.Helper()
	var p eplan.Plan
	if is := decodeStrict([]byte(read(t, "draft.json")), &p); is != nil {
		t.Fatal(is.Error())
	}
	return &p
}

// replace는 정상 출력에서 한 부분만 바꾼다. old가 없으면 테스트가 잘못된 것이다.
func replace(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("기준 출력에 %q가 없음", old)
	}
	return strings.Replace(s, old, new, 1)
}

func TestParseValid(t *testing.T) {
	p, is := Parse([]byte(read(t, "proposal.json")), draft(t))
	if len(is) > 0 {
		t.Fatalf("정상 출력이 걸림:\n%s", is.Error())
	}
	if p.Plan.Target.Manifest != ManifestRef || p.Plan.Thresholds.MaxDowntime.Seconds() != 30 {
		t.Fatalf("읽은 값: %+v", p.Plan)
	}
	if !strings.Contains(p.Manifest, "storageClass: gp3") {
		t.Fatal("매니페스트가 비어 있음")
	}
}

func TestParseRejectsBrokenAIOutput(t *testing.T) {
	v := read(t, "proposal.json")
	cases := []struct {
		name, raw, want string
	}{
		// 형식이 깨진 응답
		{"빈 응답", "", "JSON으로 읽을 수 없음"},
		{"설명 글만", "다음은 이전 계획입니다.", "JSON으로 읽을 수 없음"},
		{"마크다운 코드 블록", "```json\n" + v + "\n```", "JSON으로 읽을 수 없음"},
		{"중간에 끊김", v[:len(v)/2], "JSON으로 읽을 수 없음"},
		{"JSON 뒤에 설명", v + "\n이 계획은 안전합니다.", "JSON 객체 하나만"},
		{"모르는 키", replace(t, v, `"notes":`, `"steps": [], "notes":`), "unknown field"},
		{"엔진 Plan에 없는 키", replace(t, v, `"id": "moimroom-gke-to-eks",`, `"id": "moimroom-gke-to-eks", "rollback": "auto",`), "unknown field"},
		{"시간을 숫자로", replace(t, v, `"maxDowntime": "30s"`, `"maxDowntime": 30`), "JSON으로 읽을 수 없음"},

		// 데이터가 가는 곳은 초안과 같아야 한다
		{"대상 DSN 바꾸기", replace(t, v, "moimroom-db-eks-rw.moimroom.eks.ts.net:5432", "evil.example.com:5432"), "plan.target.dsn: 초안과 같아야 함"},
		{"원본 바꾸기", replace(t, v, `"cluster": "moimroom-db-gke"`, `"cluster": "other-db"`), "plan.source: 초안과 같아야 함"},
		{"게이트웨이 끄기", replace(t, v, `"mode": "http"`, `"mode": "none"`), "plan.gateway: 초안과 같아야 함"},
		{"파일 위치 바꾸기", replace(t, v, `"target": "s3:moimroom-photos-eks"`, `"target": "s3:attacker"`), "plan.files.target"},
		{"쓰기 기록 빼기", replace(t, v, `, "writtenRecords": "stream:moimroom-demo"`, ``), "plan.verify.writtenRecords"},
		{"검증 스키마 줄이기", replace(t, v, `"schemas": ["app"], "fenceGate"`, `"schemas": [], "fenceGate"`), "plan.verify.schemas"},
		{"검증 테이블만", replace(t, v, `"fenceGate": true,`, `"fenceGate": true, "tables": ["app.orders"],`), "범위를 줄일 수 없음"},

		// 고정 규칙
		{"fence 비교 끄기", replace(t, v, `"fenceGate": true`, `"fenceGate": false`), "끌 수 없음"},
		{"쓰기 보류 너무 김", replace(t, v, `"maxDowntime": "30s"`, `"maxDowntime": "5m"`), "5s~1m0s"},
		{"전환 시간 너무 김", replace(t, v, `"switchoverTimeout": "2m"`, `"switchoverTimeout": "30m"`), "switchoverTimeout"},
		{"복제 지연 기준 너무 큼", replace(t, v, `"maxLagBytes": 1048576`, `"maxLagBytes": 999999999999`), "maxLagBytes"},
		{"서버 파일 경로", replace(t, v, `"manifest": "attachment:target.yaml"`, `"manifest": "/etc/passwd"`), "서버 파일 경로"},
		{"근거 없음", replace(t, v, `"notes": ["원본 DB 2.1 GiB, 쓰기 초당 약 20건이라 maxLagBytes 1 MiB로 둠", "대상 EKS의 기본 StorageClass gp3를 씀"]`, `"notes": []`), "근거가 하나 이상"},

		// 매니페스트
		{"다른 클러스터 이름", replace(t, v, `name: moimroom-db-eks\n  namespace`, `name: other\n  namespace`), "manifest.metadata.name"},
		{"다른 네임스페이스", replace(t, v, `namespace: moimroom\nspec`, `namespace: kube-system\nspec`), "manifest.metadata.namespace"},
		{"복제본 아님", replace(t, v, `  replica: {self: moimroom-db-eks, primary: moimroom-db-gke, source: moimroom-db-gke}\n`, ``), "manifest.spec.replica"},
		{"엉뚱한 원본에서 복제", replace(t, v, `primary: moimroom-db-gke`, `primary: other-db`), "manifest.spec.replica.primary"},
		{"슈퍼유저 켜기", replace(t, v, `  instances: 2\n`, `  instances: 2\n  enableSuperuserAccess: true\n`), "spec.enableSuperuserAccess: 쓸 수 없는 키"},
		{"백업 대상 넣기", replace(t, v, `  instances: 2\n`, `  instances: 2\n  backup: {barmanObjectStore: {destinationPath: s3://x}}\n`), "spec.backup"},
		{"빈 DB로 시작", replace(t, v, `pg_basebackup: {source`, `initdb: {source`), "pg_basebackup으로만"},
		{"인스턴스 너무 많음", replace(t, v, `instances: 2`, `instances: 50`), "1~5인 정수"},
		{"Cluster 아님", replace(t, v, `kind: Cluster`, `kind: Deployment`), "manifest.kind"},
		{"문서 여러 개", replace(t, v, `"manifest": "apiVersion`, `"manifest": "kind: Secret\n---\napiVersion`), "문서가 여러 개"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, is := Parse([]byte(c.raw), draft(t))
			if len(is) == 0 {
				t.Fatalf("통과하면 안 되는데 통과함: %+v", p)
			}
			if p != nil {
				t.Fatal("걸렸는데 결과도 돌려줌")
			}
			if !strings.Contains(is.Error(), c.want) {
				t.Fatalf("%q가 들어간 항목이 없음. 걸린 항목:\n%s", c.want, is.Error())
			}
		})
	}
}

func TestCheckCollectsAllIssues(t *testing.T) {
	// AI에 한 번에 다 돌려줘야 재요청 횟수가 준다.
	v := read(t, "proposal.json")
	v = replace(t, v, `"maxDowntime": "30s"`, `"maxDowntime": "5m"`)
	v = replace(t, v, `"fenceGate": true`, `"fenceGate": false`)
	v = replace(t, v, `kind: Cluster`, `kind: Deployment`)
	_, is := Parse([]byte(v), draft(t))
	if len(is) < 3 {
		t.Fatalf("세 가지가 모두 걸려야 함:\n%s", is.Error())
	}
}

func TestPlanWithoutManifest(t *testing.T) {
	// 대상 클러스터가 이미 있으면 매니페스트 없이도 된다.
	var p Proposal
	_ = json.Unmarshal([]byte(read(t, "proposal.json")), &p)
	p.Plan.Target.Manifest = ""
	p.Manifest = ""
	if is := p.Check(draft(t)); len(is) > 0 {
		t.Fatal(is.Error())
	}
	p.Manifest = "kind: Cluster"
	if is := p.Check(draft(t)); !strings.Contains(is.Error(), "매니페스트를 보내지 않음") {
		t.Fatalf("매니페스트만 따로 오면 걸려야 함: %v", is)
	}
}
