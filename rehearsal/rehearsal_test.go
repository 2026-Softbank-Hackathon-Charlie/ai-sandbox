package rehearsal

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/sandbox"
)

func proposal(t *testing.T) *plan.Proposal {
	t.Helper()
	b, err := os.ReadFile("../plan/testdata/draft.json")
	if err != nil {
		t.Fatal(err)
	}
	var d eplan.Plan
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	pb, _ := os.ReadFile("../plan/testdata/proposal.json")
	p, is := plan.Parse(pb, &d)
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	return p
}

func TestParseDB(t *testing.T) {
	db, err := ParseDB("postgres://app:${GKE_MOIMROOM_PGPASSWORD}@moimroom-db-gke-rw:5432/app?sslmode=require")
	if err != nil || db.Name != "app" || db.User != "app" {
		t.Fatalf("%+v %v", db, err)
	}
	for _, bad := range []string{"mysql://a@b/c", "postgres://host:5432/app", "postgres://app@host:5432/"} {
		if _, err := ParseDB(bad); err == nil {
			t.Errorf("거절해야 함: %s", bad)
		}
	}
}

func TestSourceManifestMatchesEngineTopology(t *testing.T) {
	p := proposal(t)
	m := SourceManifest(&p.Plan, "sandbox-moimroom-ab12", DBInfo{Name: "app", User: "app"}).Object
	spec := m["spec"].(map[string]any)
	rep := spec["replica"].(map[string]any)
	// 엔진 checkTopology: 원본은 자기 자신이 primary, source는 대상
	if rep["self"] != "moimroom-db-gke" || rep["primary"] != "moimroom-db-gke" || rep["source"] != "moimroom-db-eks" {
		t.Fatalf("원본 토폴로지: %+v", rep)
	}
	ext := spec["externalClusters"].([]any)
	host := ext[1].(map[string]any)["connectionParameters"].(map[string]any)["host"]
	if host != "moimroom-db-eks-rw.sandbox-moimroom-ab12.svc" {
		t.Fatalf("샌드박스 안 주소여야 함: %v", host)
	}
}

func TestTargetManifestKeepsWhatIsRehearsed(t *testing.T) {
	p := proposal(t)
	out, err := TargetManifest(p.Manifest, &p.Plan, "sandbox-moimroom-ab12")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["metadata"].(map[string]any)["namespace"] != "sandbox-moimroom-ab12" {
		t.Fatal("네임스페이스가 샌드박스로 바뀌어야 함")
	}
	spec := doc["spec"].(map[string]any)
	// AI가 정한 값(StorageClass, 크기, 인스턴스)은 그대로여야 리허설이 의미가 있다
	if spec["storage"].(map[string]any)["storageClass"] != "gp3" || spec["instances"] != float64(2) {
		t.Fatalf("리허설할 값이 바뀜: %+v", spec)
	}
	ext := spec["externalClusters"].([]any)
	if len(ext) != 2 {
		t.Fatalf("원본과 대상 항목 둘 다 있어야 함: %d", len(ext))
	}
	for _, e := range ext {
		h := e.(map[string]any)["connectionParameters"].(map[string]any)["host"].(string)
		if !strings.HasSuffix(h, ".sandbox-moimroom-ab12.svc") {
			t.Fatalf("운영 주소가 남음: %s", h)
		}
	}
	for _, e := range ext {
		m := e.(map[string]any)
		name := m["name"].(string)
		if m["sslRootCert"].(map[string]any)["name"] != name+"-ca" || m["sslCert"].(map[string]any)["name"] != name+"-replication" {
			t.Fatalf("샌드박스의 복제용 인증서 시크릿을 가리켜야 함: %+v", m)
		}
	}
	if strings.Contains(out, "ts.net") {
		t.Fatal("운영(Tailscale) 주소가 샌드박스 매니페스트에 남음")
	}
}

func TestSandboxPlanDropsProductionPaths(t *testing.T) {
	p := proposal(t)
	sp := SandboxPlan(&p.Plan, "sandbox-moimroom-ab12", "/tmp/k", "/tmp/target.yaml", "postgres://a:b@127.0.0.1:1/app", "postgres://a:b@127.0.0.1:2/app")
	if err := sp.Validate(); err != nil {
		t.Fatal(err)
	}
	if sp.Gateway.Mode != "none" || sp.Files != nil || sp.Verify.WrittenRecords != "" {
		t.Fatalf("샌드박스에는 게이트웨이, 파일, k6 기록이 없음: %+v", sp)
	}
	if sp.Source.Namespace != "sandbox-moimroom-ab12" || sp.Target.Kubeconfig != "/tmp/k" {
		t.Fatalf("샌드박스와 샌드박스 토큰으로 가야 함: %+v", sp.Target)
	}
	if sp.Thresholds != p.Plan.Thresholds || *sp.Verify.FenceGate != true {
		t.Fatal("상한값과 fence 비교는 AI Plan 그대로여야 함")
	}
	if p.Plan.Gateway.Mode != "http" {
		t.Fatal("원본 Plan이 바뀜")
	}
}

func TestSandboxPlanVerifiesFixturesWithoutChangingProductionScope(t *testing.T) {
	p := proposal(t)
	p.Plan.Verify.Schemas = []string{"app", "audit", "app"}
	p.Plan.Verify.Tables = []string{"app.orders"}
	p.Plan.Verify.FenceGate = nil
	sp := SandboxPlan(&p.Plan, "sandbox-test", "/tmp/k", "/tmp/target.yaml", "source", "target")
	want := []string{"app.rehearsal_orders", "app.rehearsal_items", "audit.rehearsal_orders", "audit.rehearsal_items"}
	if !reflect.DeepEqual(sp.Verify.Tables, want) {
		t.Fatalf("fixture verification scope = %v, want %v", sp.Verify.Tables, want)
	}
	if !reflect.DeepEqual(sp.Verify.Schemas, []string{"app", "audit"}) {
		t.Fatalf("schemas must be unique: %v", sp.Verify.Schemas)
	}
	if !reflect.DeepEqual(p.Plan.Verify.Tables, []string{"app.orders"}) || len(p.Plan.Verify.Schemas) != 3 {
		t.Fatal("production verification scope changed")
	}
	if sp.Verify.FenceGate == nil || !*sp.Verify.FenceGate || p.Plan.Verify.FenceGate != nil {
		t.Fatal("default fence gate must be enabled without changing production plan")
	}
	p.Plan.Verify.Schemas = nil
	sp = SandboxPlan(&p.Plan, "sandbox-test", "/tmp/k", "/tmp/target.yaml", "source", "target")
	if !reflect.DeepEqual(sp.Verify.Schemas, []string{"public"}) || sp.Verify.Tables[0] != "public.rehearsal_orders" {
		t.Fatalf("default fixture schema = %+v", sp.Verify)
	}
}

func TestRehearseRejectsInvalidLimitsBeforeOpeningSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs int
		rows int
	}{
		{"negative runs", -1, 20},
		{"negative rows", 1, -1},
		{"empty items fixture", 1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := proposal(t)
			mem := &agentlog.Memory{}
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("invalid limits panicked instead of returning an error: %v", r)
				}
			}()
			_, err := Rehearse(context.Background(), Config{Proposal: p, Draft: &p.Plan, Sink: mem, MaxRuns: tc.runs, Rows: tc.rows})
			if err == nil || len(mem.Sessions) != 0 {
				t.Fatalf("invalid limits opened a session: error=%v, sessions=%d", err, len(mem.Sessions))
			}
		})
	}
}

func fixtureState() engine.State {
	return engine.State{Outcome: engine.OutcomeSucceeded, Steps: map[string]*engine.StepRecord{
		engine.StepSwitchover: {Status: engine.StatusSucceeded, Evidence: map[string]any{"fenceGate": map[string]any{
			"ok": true,
			"tables": []verify.TableResult{
				{Table: "app.rehearsal_orders", SourceCount: 20, TargetCount: 20, Match: true},
				{Table: "app.rehearsal_items", SourceCount: 15, TargetCount: 15, Match: true},
			},
		}}},
		engine.StepVerify: {Status: engine.StatusSucceeded, Evidence: map[string]any{"report": &engine.Report{OK: true, Tables: []verify.MissingResult{
			{Table: "app.rehearsal_orders", Checked: 20},
			{Table: "app.rehearsal_items", Checked: 15},
		}}}},
	}}
}

func TestFixtureCoverageRequiresActualNonemptyEngineChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(engine.State)
	}{
		{"missing fence gate", func(s engine.State) { delete(s.Steps[engine.StepSwitchover].Evidence, "fenceGate") }},
		{"empty fence tables", func(s engine.State) {
			s.Steps[engine.StepSwitchover].Evidence["fenceGate"].(map[string]any)["tables"] = []verify.TableResult{}
		}},
		{"missing fence table", func(s engine.State) {
			g := s.Steps[engine.StepSwitchover].Evidence["fenceGate"].(map[string]any)
			g["tables"] = g["tables"].([]verify.TableResult)[:1]
		}},
		{"duplicate fence table", func(s engine.State) {
			rs := s.Steps[engine.StepSwitchover].Evidence["fenceGate"].(map[string]any)["tables"].([]verify.TableResult)
			rs[1] = rs[0]
		}},
		{"unexpected fence table", func(s engine.State) {
			s.Steps[engine.StepSwitchover].Evidence["fenceGate"].(map[string]any)["tables"].([]verify.TableResult)[1].Table = "app.orders"
		}},
		{"empty source and target", func(s engine.State) {
			r := &s.Steps[engine.StepSwitchover].Evidence["fenceGate"].(map[string]any)["tables"].([]verify.TableResult)[0]
			r.SourceCount, r.TargetCount = 0, 0
		}},
		{"fence mismatch", func(s engine.State) {
			s.Steps[engine.StepSwitchover].Evidence["fenceGate"].(map[string]any)["tables"].([]verify.TableResult)[0].Match = false
		}},
		{"missing report", func(s engine.State) { delete(s.Steps[engine.StepVerify].Evidence, "report") }},
		{"empty report tables", func(s engine.State) { s.Steps[engine.StepVerify].Evidence["report"].(*engine.Report).Tables = nil }},
		{"duplicate report table", func(s engine.State) {
			rs := s.Steps[engine.StepVerify].Evidence["report"].(*engine.Report).Tables
			rs[1] = rs[0]
		}},
		{"unexpected report table", func(s engine.State) {
			s.Steps[engine.StepVerify].Evidence["report"].(*engine.Report).Tables[1].Table = "app.orders"
		}},
		{"unchecked rows", func(s engine.State) {
			s.Steps[engine.StepVerify].Evidence["report"].(*engine.Report).Tables[0].Checked = 0
		}},
		{"skipped check", func(s engine.State) {
			s.Steps[engine.StepVerify].Evidence["report"].(*engine.Report).Tables[0].Skipped = "no primary key"
		}},
		{"missing rows", func(s engine.State) {
			s.Steps[engine.StepVerify].Evidence["report"].(*engine.Report).Tables[0].Missing = 1
		}},
		{"verification error", func(s engine.State) {
			s.Steps[engine.StepVerify].Evidence["report"].(*engine.Report).Tables[0].Error = "query failed"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := fixtureState()
			tc.edit(st)
			if _, err := fixtureCoverage(st, []string{"app"}, 20); err == nil {
				t.Fatal("incomplete fixture verification accepted")
			}
		})
	}
	got, err := fixtureCoverage(fixtureState(), []string{"app"}, 20)
	if err != nil || !got.Synthetic || !reflect.DeepEqual(got.Tables, []TableCoverage{
		{Table: "app.rehearsal_orders", SourceRows: 20, TargetRows: 20, CheckedRows: 20},
		{Table: "app.rehearsal_items", SourceRows: 15, TargetRows: 15, CheckedRows: 15},
	}) {
		t.Fatalf("actual fixture coverage = %+v, error=%v", got, err)
	}
}

func TestDSNEncodesPassword(t *testing.T) {
	dsn := DSN(DBInfo{Name: "app", User: "app"}, "p@ss/w:rd", 5555)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if pw != "p@ss/w:rd" || u.Host != "127.0.0.1:5555" {
		t.Fatalf("%s", dsn)
	}
}

func TestDiffMasksAndOrders(t *testing.T) {
	d := Diff("a: 1\nstorageClass: gp3\npassword: hunter2hunter2\n", "a: 1\nstorageClass: standard\npassword: hunter2hunter2\n")
	if strings.Contains(d, "hunter2") {
		t.Fatal("diff에 비밀값이 남음")
	}
	if !strings.Contains(d, "- storageClass: gp3\n+ storageClass: standard") {
		t.Fatalf("지운 줄 다음에 더한 줄: \n%s", d)
	}
}

func TestLogIsolation(t *testing.T) {
	mem := &agentlog.Memory{}
	s, _ := mem.Start(context.Background(), agentlog.Start{Kind: agentlog.KindRehearsal})
	logIsolation(context.Background(), s, []sandbox.CheckResult{
		{Name: "prod-secrets", Command: "kubectl get secrets -n moimroom", Expect: "blocked", Got: "blocked", Pass: true},
		{Name: "metadata", Command: "curl http://169.254.169.254/", Expect: "blocked", Got: "blocked", Pass: true},
		{Name: "dns", Command: "nslookup kubernetes.default", Expect: "open", Got: "open", Pass: true},
	})
	ev := mem.Sessions[0].Events
	if ev[0].Type != agentlog.Blocked || ev[0].Tool != "kubectl" || !strings.Contains(ev[0].Output.(string), "RBAC") {
		t.Fatalf("RBAC 차단: %+v", ev[0])
	}
	if ev[1].Type != agentlog.Blocked || ev[1].Tool != "curl" || !strings.Contains(ev[1].Output.(string), "NetworkPolicy") {
		t.Fatalf("네트워크 차단: %+v", ev[1])
	}
	if ev[2].Type != agentlog.ToolResult {
		t.Fatalf("열려야 하는 항목은 차단으로 남기지 않음: %+v", ev[2])
	}
}
