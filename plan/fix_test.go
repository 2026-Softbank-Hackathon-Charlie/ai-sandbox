package plan

import (
	"strings"
	"testing"
)

func proposal(t *testing.T) *Proposal {
	t.Helper()
	p, is := Parse([]byte(read(t, "proposal.json")), draft(t))
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	return p
}

// 데모 실패 시나리오에서 AI가 내야 하는 진단: 원본 클러스터의 StorageClass가 대상에 없음
const demoDiagnosis = `{
  "cause": "매니페스트의 StorageClass gp3가 대상 클러스터에 없어 moimroom-db-eks의 PVC가 Pending",
  "evidence": ["Warning  ProvisioningFailed  persistentvolumeclaim/moimroom-db-eks-1  storageclass.storage.k8s.io \"gp3\" not found"],
  "fix": { "kind": "manifest-patch", "patch": [{ "op": "replace", "path": "/spec/storage/storageClass", "value": "standard" }] }
}`

func TestParseDiagnosisDemoScenario(t *testing.T) {
	p := proposal(t)
	d, is := ParseDiagnosis([]byte(demoDiagnosis), p, draft(t))
	if len(is) > 0 {
		t.Fatalf("데모 진단이 걸림:\n%s", is.Error())
	}
	next, is := d.Fix.Apply(p, draft(t))
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	if !strings.Contains(next.Manifest, "storageClass: standard") || strings.Contains(next.Manifest, "gp3") {
		t.Fatalf("수정이 적용되지 않음:\n%s", next.Manifest)
	}
	if !strings.Contains(p.Manifest, "gp3") {
		t.Fatal("원본 매니페스트가 바뀜")
	}
}

func TestParseDiagnosisRejects(t *testing.T) {
	cases := []struct{ name, old, new, want string }{
		{"원인 없음", `"cause": "매니페스트의 StorageClass gp3가 대상 클러스터에 없어 moimroom-db-eks의 PVC가 Pending"`, `"cause": " "`, "원인이 비어"},
		{"근거 없음", `["Warning  ProvisioningFailed  persistentvolumeclaim/moimroom-db-eks-1  storageclass.storage.k8s.io \"gp3\" not found"]`, `[]`, "근거가 하나 이상"},
		{"모르는 종류", `"manifest-patch"`, `"shell"`, "kind"},
		{"복제 원본 바꾸기", `"/spec/storage/storageClass"`, `"/spec/replica/primary"`, "고칠 수 없는 매니페스트 경로"},
		{"슈퍼유저 켜기", `"/spec/storage/storageClass"`, `"/spec/enableSuperuserAccess"`, "고칠 수 없는 매니페스트 경로"},
		{"이름 바꾸기", `"/spec/storage/storageClass"`, `"/metadata/name"`, "고칠 수 없는 매니페스트 경로"},
		{"삭제 연산", `"op": "replace"`, `"op": "remove"`, `"replace" 또는 "add"`},
		{"값 없음", `, "value": "standard"`, ``, "값이 있어야"},
		{"없는 키 replace", `"/spec/storage/storageClass"`, `"/spec/walStorage/size"`, "중간 키"},
		{"모르는 키", `"evidence"`, `"evidence": [], "run"`, "unknown field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(demoDiagnosis, c.old) {
				t.Fatalf("기준 진단에 %q가 없음", c.old)
			}
			raw := strings.Replace(demoDiagnosis, c.old, c.new, 1)
			d, is := ParseDiagnosis([]byte(raw), proposal(t), draft(t))
			if len(is) == 0 {
				t.Fatalf("통과하면 안 되는데 통과함: %+v", d)
			}
			if !strings.Contains(is.Error(), c.want) {
				t.Fatalf("%q가 들어간 항목이 없음. 걸린 항목:\n%s", c.want, is.Error())
			}
		})
	}
}

func TestPlanParamFix(t *testing.T) {
	p := proposal(t)
	f := Fix{Kind: KindPlanParam, Patch: []Op{{Op: "replace", Path: "/thresholds/catchupTimeout", Value: []byte(`"12m"`)}}}
	next, is := f.Apply(p, draft(t))
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	if next.Plan.Thresholds.CatchupTimeout.Minutes() != 12 || p.Plan.Thresholds.CatchupTimeout.Minutes() != 10 {
		t.Fatalf("사본에만 적용돼야 함: %v / %v", next.Plan.Thresholds.CatchupTimeout, p.Plan.Thresholds.CatchupTimeout)
	}
}

func TestPlanParamFixRejects(t *testing.T) {
	for name, c := range map[string]struct {
		op   Op
		want string
	}{
		"고정 규칙을 깨는 값": {Op{"replace", "/thresholds/maxDowntime", []byte(`"10m"`)}, "적용하면 검사에 걸림"},
		"DSN 바꾸기":     {Op{"replace", "/target/dsn", []byte(`"postgres://x"`)}, "고칠 수 없는 Plan 경로"},
		"게이트웨이 바꾸기":   {Op{"replace", "/gateway/mode", []byte(`"none"`)}, "고칠 수 없는 Plan 경로"},
		"fence 끄기":    {Op{"replace", "/verify/fenceGate", []byte(`false`)}, "고칠 수 없는 Plan 경로"},
		"형식 틀린 시간":    {Op{"replace", "/thresholds/maxDowntime", []byte(`"soon"`)}, "고친 Plan을 읽을 수 없음"},
	} {
		t.Run(name, func(t *testing.T) {
			_, is := Fix{Kind: KindPlanParam, Patch: []Op{c.op}}.Apply(proposal(t), draft(t))
			if !strings.Contains(is.Error(), c.want) {
				t.Fatalf("%q가 들어간 항목이 없음. 걸린 항목:\n%s", c.want, is.Error())
			}
		})
	}
}

func TestFixPatchLimit(t *testing.T) {
	ops := make([]Op, maxPatchOps+1)
	for i := range ops {
		ops[i] = Op{"replace", "/spec/storage/storageClass", []byte(`"x"`)}
	}
	_, is := Fix{Kind: KindManifestPatch, Patch: ops}.Apply(proposal(t), draft(t))
	if !strings.Contains(is.Error(), "한 번에") {
		t.Fatalf("상한에 걸려야 함:\n%s", is.Error())
	}
}

func TestManifestPathAllowed(t *testing.T) {
	for path, want := range map[string]bool{
		"/spec/storage/storageClass":                         true,
		"/spec/storage/size":                                 true,
		"/spec/walStorage/storageClass":                      true,
		"/spec/resources/limits/memory":                      true,
		"/spec/instances":                                    true,
		"/spec/imageName":                                    true,
		"/spec/resources":                                    false,
		"/spec/storage":                                      false,
		"/spec/replica/primary":                              false,
		"/spec/externalClusters/0/connectionParameters/host": false,
		"/metadata/namespace":                                false,
		"/spec/bootstrap/pg_basebackup/source":               false,
	} {
		seg, _ := splitPointer(path)
		if got := manifestPathAllowed(seg); got != want {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
}
