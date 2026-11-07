package rehearsal

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
	// Explicit engine thresholds survive sandbox adaptation; omitted fence
	// budgets are covered separately against the engine's Parse defaults.
	p.Plan.Thresholds.MaxFenceGate.Duration = 7 * time.Second
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

func TestSandboxApp(t *testing.T) {
	if got := SandboxApp("moimroom"); got != "moimroom" {
		t.Fatalf("짧은 이름은 그대로: %s", got)
	}
	long := "moimroom-reservation-service-prod-01"
	got := SandboxApp(long)
	if len(got) > 32 || got != SandboxApp(long) || got == SandboxApp(long+"x") {
		t.Fatalf("긴 이름: %q", got)
	}
	if _, err := sandbox.Build(sandbox.Config{App: got, ID: "ab12"}, time.Now()); err != nil {
		t.Fatalf("샌드박스 이름 규칙에 맞아야 함: %v", err)
	}
}

func TestTargetManifestFillsLimitsFromRequests(t *testing.T) {
	// requests만 적은 CPU는 샌드박스에서 limits도 같게 둔다. LimitRange 기본 limits(500m)가 더 작으면 Pod가 거절된다.
	p := proposal(t)
	out, err := TargetManifest(p.Manifest, &p.Plan, "sandbox-moimroom-ab12")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal([]byte(out), &doc)
	lim := doc["spec"].(map[string]any)["resources"].(map[string]any)["limits"].(map[string]any)
	if lim["cpu"] != "100m" || lim["memory"] != "512Mi" {
		t.Fatalf("limits: %+v", lim)
	}
	if !strings.Contains(p.Manifest, "limits: {memory: 512Mi}") {
		t.Fatal("운영 매니페스트가 바뀜")
	}
}

func TestSandboxConfigFitsTargetManifest(t *testing.T) {
	// 인스턴스 2개 + 작업 Pod 1개 = 3개 몫을 기본값에 더한다. CPU는 requests 100m(limits 없음), 메모리는 limits 512Mi.
	c, err := SandboxConfig("moimroom", proposal(t).Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if c.CPU != "4300m" || c.Memory != "9728Mi" {
		t.Fatalf("Quota: CPU %s, 메모리 %s", c.CPU, c.Memory)
	}
	// 운영에 맞춘 큰 자원도 샌드박스에서 돌 수 있어야 한다
	big := "spec:\n  instances: 3\n  resources:\n    requests: {cpu: \"4\", memory: 16Gi}\n"
	c, err = SandboxConfig("moimroom", big)
	if err != nil {
		t.Fatal(err)
	}
	if c.CPU != "20" || c.Memory != "72Gi" {
		t.Fatalf("큰 매니페스트 Quota: CPU %s, 메모리 %s", c.CPU, c.Memory)
	}
	if _, err := sandbox.Build(c, time.Now()); err != nil {
		t.Fatalf("샌드박스 설정으로 쓸 수 있어야 함: %v", err)
	}
}

func TestLogIsolationNamesPodSecurity(t *testing.T) {
	mem := &agentlog.Memory{}
	s, _ := mem.Start(context.Background(), agentlog.Start{Kind: agentlog.KindRehearsal})
	logIsolation(context.Background(), s, []sandbox.CheckResult{
		{Name: "host-pod-create", Command: "kubectl run", Expect: "blocked", Got: "blocked", Pass: true, Detail: sandbox.HostPodByRBAC},
		{Name: "host-pod-admission", Command: "kubectl run", Expect: "blocked", Got: "blocked", Pass: true, Detail: sandbox.HostPodByPodSecurity},
	})
	ev := mem.Sessions[0].Events
	if ev[0].Output != sandbox.HostPodByRBAC || ev[1].Output != sandbox.HostPodByPodSecurity {
		t.Fatalf("무엇이 막았는지 남겨야 함: %+v %+v", ev[0], ev[1])
	}
}

func TestPrepareTempSource(t *testing.T) {
	p := proposal(t)
	newSrc := func() *unstructured.Unstructured {
		return SourceManifest(&p.Plan, "sandbox-moimroom-ab12", DBInfo{Name: "app", User: "app"})
	}
	image := func(u *unstructured.Unstructured) string {
		s, _, _ := unstructured.NestedString(u.Object, "spec", "imageName")
		return s
	}
	params := func(u *unstructured.Unstructured) map[string]string {
		m, _, _ := unstructured.NestedStringMap(u.Object, "spec", "postgresql", "parameters")
		return m
	}
	target := "spec:\n  imageName: ghcr.io/cloudnative-pg/postgresql:17.11\n  postgresql:\n    parameters:\n      max_connections: \"60\"\n      max_worker_processes: \"8\"\n      work_mem: \"8MB\"\n"

	// 원본을 읽었으면 실제 원본처럼: 이미지와 hot standby 설정을 원본 값으로 (원본에 없는 설정은 원본도 기본값)
	src := newSrc()
	sc := &plan.SourceCluster{Image: "ghcr.io/cloudnative-pg/postgresql:17.6", Parameters: map[string]string{"max_wal_senders": "20"}}
	if err := PrepareTempSource(src, sc, target); err != nil {
		t.Fatal(err)
	}
	if image(src) != "ghcr.io/cloudnative-pg/postgresql:17.6" || !reflect.DeepEqual(params(src), map[string]string{"max_wal_senders": "20"}) {
		t.Fatalf("실제 원본처럼 떠야 함: %s %v", image(src), params(src))
	}
	// 원본 이미지가 공식 이미지가 아니면(샌드박스가 받지 못할 수 있음) 대상 이미지, 설정은 원본 그대로
	src = newSrc()
	if err := PrepareTempSource(src, &plan.SourceCluster{Image: "123456789012.dkr.ecr.ap-northeast-2.amazonaws.com/mirror/postgresql:17.11"}, target); err != nil {
		t.Fatal(err)
	}
	if image(src) != "ghcr.io/cloudnative-pg/postgresql:17.11" || len(params(src)) != 0 {
		t.Fatalf("미러 원본: %s %v", image(src), params(src))
	}
	// 원본을 모르면 대상의 이미지와 hot standby 설정 (대상이 자기 설정 이상인 원본의 복제본으로 뜨는지만 본다)
	src = newSrc()
	if err := PrepareTempSource(src, nil, target); err != nil {
		t.Fatal(err)
	}
	if image(src) != "ghcr.io/cloudnative-pg/postgresql:17.11" || !reflect.DeepEqual(params(src), map[string]string{"max_connections": "60", "max_worker_processes": "8"}) {
		t.Fatalf("원본을 모를 때: %s %v", image(src), params(src))
	}
	// 대상에도 없으면 건드리지 않는다
	src = newSrc()
	if err := PrepareTempSource(src, nil, "spec: {}\n"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedMap(src.Object, "spec", "postgresql"); found || image(src) != "" {
		t.Fatalf("없는 값을 넣음: %v", src.Object["spec"])
	}
}

func TestTempStorageClass(t *testing.T) {
	retain, del := corev1.PersistentVolumeReclaimRetain, corev1.PersistentVolumeReclaimDelete
	sc := func(name, prov string, policy *corev1.PersistentVolumeReclaimPolicy, def bool) storagev1.StorageClass {
		s := storagev1.StorageClass{Provisioner: prov, ReclaimPolicy: policy}
		s.Name = name
		if def {
			s.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
		}
		return s
	}
	// 워크로드 EKS와 같은 모양: 기본값 없음
	eks := []storagev1.StorageClass{
		sc("cnpg-gp3-retain", "ebs.csi.aws.com", &retain, false),
		sc("gp2", "kubernetes.io/aws-ebs", &del, false),
		sc("gp3", "ebs.csi.aws.com", &del, false),
	}
	for _, c := range []struct {
		name, manifest, want string
		scs                  []storagev1.StorageClass
	}{
		{"기본값이 있으면 비움", "spec: {storage: {storageClass: gp3}}", "", append(eks, sc("standard", "x", nil, true))},
		{"대상 값이 있고 Delete면 그것", "spec: {storage: {storageClass: gp2}}", "gp2", eks},
		{"대상 값이 Retain이면 고르지 않음", "spec: {storage: {storageClass: cnpg-gp3-retain}}", "gp3", eks},
		{"대상 값이 이 클러스터에 없으면(데모 실패) CSI Delete", "spec: {storage: {storageClass: standard-rwo}}", "gp3", eks},
		{"대상 값이 없어도", "spec: {}", "gp3", eks},
		// 기본값이 Retain이면 샌드박스를 지워도 디스크가 남는다: Delete인 CSI 클래스를 고른다
		{"기본값이 Retain이면 Delete인 것", "spec: {}", "gp3", append(slices.Clone(eks), sc("retain-default", "ebs.csi.aws.com", &retain, true))},
		{"기본값이 Retain이어도 대상 값이 Delete로 있으면 그것", "spec: {storage: {storageClass: gp2}}", "gp2", append(slices.Clone(eks), sc("retain-default", "ebs.csi.aws.com", &retain, true))},
		{"Delete인 것이 없으면 Retain 기본값이라도", "spec: {}", "", []storagev1.StorageClass{sc("retain-default", "ebs.csi.aws.com", &retain, true)}},
	} {
		got, err := TempStorageClass(c.scs, c.manifest)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	if _, err := TempStorageClass([]storagev1.StorageClass{sc("r", "ebs.csi.aws.com", &retain, false)}, "spec: {}"); err == nil {
		t.Error("쓸 수 있는 것이 없으면 오류여야 함")
	}
}

func TestTokenKubeconfigKeepsTLSServerName(t *testing.T) {
	admin := &rest.Config{Host: "https://127.0.0.1:11443", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ca"), ServerName: "abc.gr7.ap-northeast-2.eks.amazonaws.com"}}
	path, err := TokenKubeconfig(t.TempDir(), admin, "tok")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters["sandbox"].TLSServerName; got != admin.ServerName {
		t.Fatalf("터널로 붙을 때 인증서 이름이 남아야 함: %q", got)
	}
}

func TestSandboxStorageClasses(t *testing.T) {
	retain, del := corev1.PersistentVolumeReclaimRetain, corev1.PersistentVolumeReclaimDelete
	sc := func(name, prov, typ string, policy *corev1.PersistentVolumeReclaimPolicy) storagev1.StorageClass {
		s := storagev1.StorageClass{Provisioner: prov, ReclaimPolicy: policy, Parameters: map[string]string{"type": typ}}
		s.Name = name
		return s
	}
	// 워크로드 EKS와 같은 모양
	eks := []storagev1.StorageClass{
		sc("cnpg-gp3-retain", "ebs.csi.aws.com", "gp3", &retain),
		sc("gp2", "kubernetes.io/aws-ebs", "gp2", &del),
		sc("aaa-io2", "ebs.csi.aws.com", "io2", &del),
		sc("gp3", "ebs.csi.aws.com", "gp3", &del),
	}
	get := func(t *testing.T, manifest, key string) string {
		t.Helper()
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
			t.Fatal(err)
		}
		st, _ := doc["spec"].(map[string]any)[key].(map[string]any)
		v, _ := st["storageClass"].(string)
		return v
	}
	for _, c := range []struct {
		name, manifest, wantStorage, wantWAL string
		notes                                int
	}{
		{"Retain은 같은 프로비저너·같은 종류의 Delete로", "spec: {instances: 2, storage: {size: 10Gi, storageClass: cnpg-gp3-retain}}", "gp3", "", 1},
		{"walStorage도", "spec: {storage: {storageClass: cnpg-gp3-retain}, walStorage: {storageClass: cnpg-gp3-retain}}", "gp3", "gp3", 2},
		{"Delete면 그대로", "spec: {storage: {storageClass: gp2}}", "gp2", "", 0},
		{"이 클러스터에 없는 클래스는 그대로 (리허설이 잡을 것)", "spec: {storage: {storageClass: standard-rwo}}", "standard-rwo", "", 0},
		{"비어 있으면 그대로", "spec: {storage: {size: 1Gi}}", "", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, notes, err := SandboxStorageClasses(c.manifest, eks)
			if err != nil {
				t.Fatal(err)
			}
			if len(notes) != c.notes {
				t.Fatalf("notes = %v, want %d", notes, c.notes)
			}
			if got := get(t, out, "storage"); got != c.wantStorage {
				t.Fatalf("storage = %q, want %q", got, c.wantStorage)
			}
			if c.wantWAL != "" {
				if got := get(t, out, "walStorage"); got != c.wantWAL {
					t.Fatalf("walStorage = %q, want %q", got, c.wantWAL)
				}
			}
		})
	}
	// 같은 프로비저너의 Delete가 없으면 디스크가 남으니 리허설 전에 멈춘다
	only := []storagev1.StorageClass{sc("cnpg-gp3-retain", "ebs.csi.aws.com", "gp3", &retain), sc("gp2", "kubernetes.io/aws-ebs", "gp2", &del)}
	if _, _, err := SandboxStorageClasses("spec: {storage: {storageClass: cnpg-gp3-retain}}", only); err == nil || !strings.Contains(err.Error(), "Delete") {
		t.Fatalf("err = %v, want no Delete class error", err)
	}
}

// 대체 후보는 같은 volumeBindingMode가 먼저다. 이름순으로는 앞서도 Immediate는 운영(WaitForFirstConsumer)에 없는 AZ 문제를 만든다.
func TestSandboxStorageClassesBindingMode(t *testing.T) {
	retain, del := corev1.PersistentVolumeReclaimRetain, corev1.PersistentVolumeReclaimDelete
	wffc, imm := storagev1.VolumeBindingWaitForFirstConsumer, storagev1.VolumeBindingImmediate
	sc := func(name string, policy *corev1.PersistentVolumeReclaimPolicy, mode *storagev1.VolumeBindingMode) storagev1.StorageClass {
		s := storagev1.StorageClass{Provisioner: "ebs.csi.aws.com", ReclaimPolicy: policy, VolumeBindingMode: mode, Parameters: map[string]string{"type": "gp3", "encrypted": "true"}}
		s.Name = name
		return s
	}
	scs := []storagev1.StorageClass{sc("cnpg-gp3-retain", &retain, &wffc), sc("aaa-gp3-immediate", &del, &imm), sc("gp3", &del, &wffc)}
	out, notes, err := SandboxStorageClasses("spec: {storage: {storageClass: cnpg-gp3-retain}}", scs)
	if err != nil || len(notes) != 1 || !strings.Contains(out, "storageClass: gp3") {
		t.Fatalf("out = %q, notes = %v, err = %v, want gp3", out, notes, err)
	}
}

// 진단 AI가 샌드박스 대체를 운영 매니페스트 문제로 고치지 않게 실패 로그에 남긴다.
func TestFailureLogSandboxNotes(t *testing.T) {
	a := Attempt{N: 1, Namespace: "sandbox-x", SandboxNotes: []string{"storage.storageClass cnpg-gp3-retain(Retain) → gp3(Delete, 같은 프로비저너)"}}
	got := failureLog(a, engine.State{}, nil)
	if !strings.Contains(got, "sandbox-only substitutions") || !strings.Contains(got, "cnpg-gp3-retain(Retain) → gp3") {
		t.Fatalf("failureLog = %q", got)
	}
}

// 두 환경에서 같은 이름을 쓰는 DB 짝(infra ADR 0063, moimroom-mig)은 원본과 대상을 한 샌드박스에 두려면
// 임시 원본 이름을 바꿔야 한다. 이름이 같으면 CNPG가 externalClusters 중복으로 임시 원본을 거부했다.
func TestPairedSameNameGetsDistinctSandboxSource(t *testing.T) {
	const ns = "sandbox-moimroom-mig-ab12"
	p := &eplan.Plan{
		Source: eplan.Endpoint{Env: "gke", Cluster: "moimroom-mig-db"},
		Target: eplan.Endpoint{Env: "eks", Cluster: "moimroom-mig-db"},
	}
	if got := SandboxSource(p); got != "moimroom-mig-db-source" {
		t.Fatalf("임시 원본 이름: %s", got)
	}
	src := SourceManifest(p, ns, DBInfo{Name: "app", User: "app"}).Object
	if name, _, _ := unstructured.NestedString(src, "metadata", "name"); name != "moimroom-mig-db-source" {
		t.Fatalf("임시 원본 metadata.name: %s", name)
	}
	rep, _, _ := unstructured.NestedStringMap(src, "spec", "replica")
	if rep["self"] != "moimroom-mig-db-source" || rep["primary"] != "moimroom-mig-db-source" || rep["source"] != "moimroom-mig-db" {
		t.Fatalf("임시 원본 토폴로지: %+v", rep)
	}
	srcExt, _, _ := unstructured.NestedSlice(src, "spec", "externalClusters")
	if names := extNames(srcExt); !slices.Equal(names, []string{"moimroom-mig-db-source", "moimroom-mig-db"}) {
		t.Fatalf("임시 원본 externalClusters: %v", names)
	}

	// AI가 이 앱에 실제로 쓴 모양: 모든 자리가 같은 이름이고, externalClusters는 한 항목뿐이다(plan 검사가 중복을 거절함).
	// 같은 이름 항목이 두 개여도 결과는 같아야 한다.
	dup := "    - name: moimroom-mig-db\n      connectionParameters: {host: moimroom-mig-db-rw.moimroom-mig.svc, user: streaming_replica, dbname: postgres, sslmode: verify-full}\n"
	for _, extra := range []string{"", dup} {
		checkPairedTarget(t, p, ns, pairedTarget+extra)
	}
	if sp := SandboxPlan(p, ns, "k", "m", "s", "d"); sp.Source.Cluster != "moimroom-mig-db-source" || sp.Target.Cluster != "moimroom-mig-db" {
		t.Fatalf("샌드박스 Plan: 원본 %s, 대상 %s", sp.Source.Cluster, sp.Target.Cluster)
	}
}

const pairedTarget = `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: moimroom-mig-db, namespace: moimroom-mig}
spec:
  instances: 1
  replica: {self: moimroom-mig-db, primary: moimroom-mig-db, source: moimroom-mig-db}
  bootstrap:
    pg_basebackup: {source: moimroom-mig-db, database: app, owner: app, secret: {name: moimroom-mig-db-app}}
  externalClusters:
    - name: moimroom-mig-db
      connectionParameters: {host: db-eks-moimroom-mig.hopcharlie-db-network.svc.cluster.local, user: streaming_replica, dbname: postgres, sslmode: verify-full}
`

func checkPairedTarget(t *testing.T, p *eplan.Plan, ns, manifest string) {
	t.Helper()
	out, err := TargetManifest(manifest, p, ns)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if name, _, _ := unstructured.NestedString(doc, "metadata", "name"); name != "moimroom-mig-db" {
		t.Fatalf("대상 이름은 그대로여야 함: %s", name)
	}
	trep, _, _ := unstructured.NestedStringMap(doc, "spec", "replica")
	if trep["self"] != "moimroom-mig-db" || trep["primary"] != "moimroom-mig-db-source" || trep["source"] != "moimroom-mig-db-source" {
		t.Fatalf("대상 토폴로지: %+v", trep)
	}
	pb, _, _ := unstructured.NestedMap(doc, "spec", "bootstrap", "pg_basebackup")
	if pb["source"] != "moimroom-mig-db-source" || pb["secret"].(map[string]any)["name"] != "moimroom-mig-db-source-app" {
		t.Fatalf("pg_basebackup: %+v", pb)
	}
	ext, _, _ := unstructured.NestedSlice(doc, "spec", "externalClusters")
	if names := extNames(ext); !slices.Equal(names, []string{"moimroom-mig-db-source", "moimroom-mig-db"}) {
		t.Fatalf("대상 externalClusters: %v", names)
	}
	for _, e := range ext {
		h := e.(map[string]any)["connectionParameters"].(map[string]any)["host"].(string)
		if !strings.HasSuffix(h, "."+ns+".svc") {
			t.Fatalf("운영 주소가 남음: %s", h)
		}
	}
}

func extNames(ext []any) []string {
	var out []string
	for _, e := range ext {
		out = append(out, e.(map[string]any)["name"].(string))
	}
	return out
}
