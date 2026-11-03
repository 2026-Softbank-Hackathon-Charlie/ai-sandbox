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
		{"파일 이전 빼기", replace(t, v, `"files": { "source": "gcs:moimroom-photos-gke", "target": "s3:moimroom-photos-eks" },`, ``), "초안의 파일 이전을 뺄 수 없음"},
		{"rclone 옵션 더하기", replace(t, v, `"target": "s3:moimroom-photos-eks" }`, `"target": "s3:moimroom-photos-eks", "flags": ["--delete-before"] }`), "plan.files.flags"},
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
		{"복제 소스가 원본이 아님", replace(t, v, `primary: moimroom-db-gke, source: moimroom-db-gke}`, `primary: moimroom-db-gke, source: other}`), "manifest.spec.replica.source"},
		{"슈퍼유저 켜기", replace(t, v, `  instances: 2\n`, `  instances: 2\n  enableSuperuserAccess: true\n`), "spec.enableSuperuserAccess: 쓸 수 없는 키"},
		{"백업 대상 넣기", replace(t, v, `  instances: 2\n`, `  instances: 2\n  backup: {barmanObjectStore: {destinationPath: s3://x}}\n`), "spec.backup"},
		{"빈 DB로 시작", replace(t, v, `pg_basebackup: {source`, `initdb: {source`), "pg_basebackup으로만"},
		{"다른 곳에서 basebackup", replace(t, v, `pg_basebackup: {source: moimroom-db-gke`, `pg_basebackup: {source: other`), "pg_basebackup.source"},
		{"다른 계정 시크릿", replace(t, v, `secret: {name: moimroom-db-gke-app}`, `secret: {name: moimroom-db-gke-superuser}`), "pg_basebackup.secret"},
		{"원본 주소 바꾸기", replace(t, v, `{host: moimroom-db-gke-rw.moimroom.gke.ts.net`, `{host: evil.example.com`), "connectionParameters.host"},
		{"원본 비밀번호 넣기", replace(t, v, `sslmode: verify-full}`, `sslmode: verify-full, password: x}`), "connectionParameters.password: 쓸 수 없는 키"},
		{"모르는 externalCluster", replace(t, v, `    - name: moimroom-db-gke\n`, `    - name: other\n`), "externalClusters[0].name"},
		{"다른 인증서 시크릿", replace(t, v, `sslmode: verify-full}\n`, `sslmode: verify-full}\n      sslKey: {name: other-tls, key: tls.key}\n`), "sslKey"},
		{"임의 이미지", replace(t, v, `imageName: ghcr.io/cloudnative-pg/postgresql:17.11`, `imageName: attacker/pg:latest`), "spec.imageName"},
		{"비슷한 이름의 이미지", replace(t, v, `imageName: ghcr.io/cloudnative-pg/postgresql:17.11`, `imageName: ghcr.io/cloudnative-pg/postgresql-evil:17`), "spec.imageName"},
		{"인스턴스 너무 많음", replace(t, v, `instances: 2`, `instances: 50`), "1~5인 정수"},
		{"Cluster 아님", replace(t, v, `kind: Cluster`, `kind: Deployment`), "manifest.kind"},
		{"문서 여러 개", replace(t, v, `"manifest": "apiVersion`, `"manifest": "kind: Secret\n---\napiVersion`), "문서가 여러 개"},
		{"문서 끝 뒤에 다른 문서", replace(t, v, `sslmode: verify-full}\n"`, `sslmode: verify-full}\n...\nkind: Pod\n"`), "문서가 여러 개"},
		{"매니페스트 너무 큼", replace(t, v, `"manifest": "apiVersion`, `"manifest": "# `+strings.Repeat("x", 65<<10)+`\napiVersion`), "KiB 이하"},

		// 엔진이 Force로 적용하므로 대상 DB를 멈추거나 지우는 메타데이터는 받지 않는다
		{"annotations로 DB 멈추기", replace(t, v, `namespace: moimroom\nspec`, `namespace: moimroom\n  annotations: {cnpg.io/hibernation: \"on\"}\nspec`), "metadata.annotations: 쓸 수 없는 키"},
		{"finalizers", replace(t, v, `namespace: moimroom\nspec`, `namespace: moimroom\n  finalizers: [example.com/never]\nspec`), "metadata.finalizers"},
		{"최상위 status", replace(t, v, `"manifest": "apiVersion`, `"manifest": "status: {}\napiVersion`), "manifest.status: 쓸 수 없는 키"},

		// 값의 타입과 범위
		{"인스턴스 수를 문자열로", replace(t, v, `instances: 2`, `instances: \"3\"`), "1~5인 정수"},
		{"종료 대기 문자열", replace(t, v, `smartShutdownTimeout: 5`, `smartShutdownTimeout: x`), "spec.smartShutdownTimeout"},
		{"storage 없음", replace(t, v, `  storage:\n    size: 5Gi\n    storageClass: gp3\n`, ``), "spec.storage: 있어야 함"},
		{"다른 볼륨에서 복제", replace(t, v, `    storageClass: gp3\n`, `    storageClass: gp3\n    pvcTemplate: {dataSource: {kind: PersistentVolumeClaim, name: other}}\n`), "spec.storage.pvcTemplate"},
		{"크기 너무 큼", replace(t, v, `size: 5Gi`, `size: 1Ei`), "spec.storage.size"},
		{"StorageClass를 숫자로", replace(t, v, `storageClass: gp3`, `storageClass: 123`), "StorageClass 이름"},
		{"CPU 음수", replace(t, v, `requests: {cpu: 100m`, `requests: {cpu: \"-1\"`), "spec.resources.requests.cpu"},
		{"GPU 요청", replace(t, v, `limits: {memory: 512Mi}`, `limits: {memory: 512Mi, nvidia.com/gpu: 1}`), "nvidia.com/gpu: 쓸 수 없는 키"},
		{"bootstrap 없음", replace(t, v, `  bootstrap:\n    pg_basebackup: {source: moimroom-db-gke, database: app, owner: app, secret: {name: moimroom-db-gke-app}}\n`, ``), "spec.bootstrap: pg_basebackup으로"},
		{"bootstrap 문자열", replace(t, v, `pg_basebackup: {source: moimroom-db-gke, database: app, owner: app, secret: {name: moimroom-db-gke-app}}`, `pg_basebackup: x`), "pg_basebackup: 객체여야"},

		// 하위 키
		{"복제 늦추기", replace(t, v, `source: moimroom-db-gke}\n  externalClusters`, `source: moimroom-db-gke, minApplyDelay: 1h}\n  externalClusters`), "spec.replica.minApplyDelay"},
		{"SQL을 로그로", replace(t, v, `  instances: 2\n`, `  instances: 2\n  postgresql: {parameters: {log_statement: all}}\n`), "parameters.log_statement: 쓸 수 없는 설정"},
		{"설정 값을 숫자로", replace(t, v, `  instances: 2\n`, `  instances: 2\n  postgresql: {parameters: {max_connections: 200}}\n`), "값은 문자열"},
		{"모니터링 쿼리", replace(t, v, `  instances: 2\n`, `  instances: 2\n  monitoring: {customQueriesSecret: [{name: x, key: q}]}\n`), "spec.monitoring: 쓸 수 없는 키"},
		{"복제 암호화 끄기", replace(t, v, `sslmode: verify-full}`, `sslmode: disable}`), "sslmode"},
		{"복제 접속 DB 바꾸기", replace(t, v, `dbname: postgres`, `dbname: app`), "connectionParameters.dbname"},

		// 근거
		{"빈 근거", replace(t, v, `"notes": ["원본 DB 2.1 GiB, 쓰기 초당 약 20건이라 maxLagBytes 1 MiB로 둠", "대상 EKS의 기본 StorageClass gp3를 씀"]`, `"notes": ["   "]`), "notes[0]: 비어 있음"},
		{"근거 너무 많음", replace(t, v, `"대상 EKS의 기본 StorageClass gp3를 씀"`, strings.TrimSuffix(strings.Repeat(`"x",`, 10), ",")), "10개까지"},
		{"근거 너무 김", replace(t, v, `"대상 EKS의 기본 StorageClass gp3를 씀"`, `"`+strings.Repeat("가", 501)+`"`), "500자 이하"},
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

func TestEquivalentEmptyValues(t *testing.T) {
	// 초안에 schemas가 없고(null) AI가 빈 목록을 내도, rclone 기본값을 적어도 같은 계획이다.
	d := draft(t)
	d.Verify.Schemas = nil
	v := read(t, "proposal.json")
	v = replace(t, v, `"schemas": ["app"], "fenceGate"`, `"schemas": [], "fenceGate"`)
	v = replace(t, v, `"target": "s3:moimroom-photos-eks" }`, `"target": "s3:moimroom-photos-eks", "rclone": "rclone", "flags": [] }`)
	if _, is := Parse([]byte(v), d); len(is) > 0 {
		t.Fatalf("같은 뜻의 값은 통과해야 함:\n%s", is.Error())
	}
}

func TestParseDefaultsFenceGateWithoutExpandingReferences(t *testing.T) {
	raw := replace(t, read(t, "proposal.json"), `"fenceGate": true,`, ``)
	p, is := Parse([]byte(raw), draft(t))
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	if p.Plan.Verify.FenceGate == nil || !*p.Plan.Verify.FenceGate {
		t.Fatal("omitted fenceGate must default to true before execution")
	}
	if !strings.Contains(p.Plan.Source.DSN, "${GKE_MOIMROOM_PGPASSWORD}") {
		t.Fatal("validation must not expand deployment secrets")
	}
}

func TestParseRejectsMissingOrZeroThresholds(t *testing.T) {
	for _, field := range []string{"maxLagBytes", "catchupTimeout", "switchoverTimeout", "maxDowntime"} {
		for _, missing := range []bool{true, false} {
			t.Run(field+map[bool]string{true: "/missing", false: "/zero"}[missing], func(t *testing.T) {
				var raw map[string]any
				_ = json.Unmarshal([]byte(read(t, "proposal.json")), &raw)
				thresholds := raw["plan"].(map[string]any)["thresholds"].(map[string]any)
				if missing {
					delete(thresholds, field)
				} else if field == "maxLagBytes" {
					thresholds[field] = 0
				} else {
					thresholds[field] = "0s"
				}
				b, _ := json.Marshal(raw)
				if _, is := Parse(b, draft(t)); !strings.Contains(is.Error(), "plan.thresholds."+field) {
					t.Fatalf("missing or zero %s must fail: %v", field, is)
				}
			})
		}
	}
}

func TestCheckAgainstDraftPreservesRequiredTables(t *testing.T) {
	d := draft(t)
	d.Verify.Tables = []string{"app.orders", "app.items"}
	for _, c := range []struct {
		name   string
		tables []string
		ok     bool
	}{
		{"same order-independent set", []string{"app.items", "app.orders"}, true},
		{"wider filter", []string{"app.orders", "app.items", "app.extra"}, true},
		{"all tables", nil, true},
		{"reduced filter", []string{"app.orders"}, false},
		{"replaced filter", []string{"app.other"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := *d
			p.Verify.Tables = c.tables
			is := CheckAgainstDraft(&p, d)
			if (len(is) == 0) != c.ok {
				t.Fatalf("unexpected table scope result: %v", is)
			}
		})
	}
}

func TestCheckAgainstDraftDoesNotDiscloseOriginalSecrets(t *testing.T) {
	d := draft(t)
	d.Source.DSN = "postgres://app:unlabelled-original-secret@db:5432/app"
	p := *d
	p.Source.DSN = "postgres://app:replacement-secret@db:5432/app"
	is := CheckAgainstDraft(&p, d)
	if len(is) == 0 || strings.Contains(is.Error(), "unlabelled-original-secret") || strings.Contains(is.Error(), "replacement-secret") {
		t.Fatalf("mismatch issue must identify the path without secret values: %v", is)
	}
}
