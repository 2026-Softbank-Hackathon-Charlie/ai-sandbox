package plan

import (
	"strings"
	"testing"
)

func validPlan(t *testing.T) *Plan {
	t.Helper()
	p, is := Parse([]byte(validJSON(t)))
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	return p
}

// 데모 실패 시나리오(설계 문서 9장)에서 AI가 내야 하는 진단
const demoDiagnosis = `{
  "cause": "values에 원본 EKS의 StorageClass gp3가 남아 있어 GKE에서 CNPG PVC가 Pending 상태",
  "evidence": ["persistentvolumeclaim/shop-db-1 Pending: storageclass.storage.k8s.io \"gp3\" not found"],
  "fix": { "kind": "values-patch", "file": "values-gcp.yaml",
           "patch": [{ "op": "replace", "path": "/cnpg/storageClass", "value": "standard-rwo" }] }
}`

func TestParseDiagnosisDemoScenario(t *testing.T) {
	d, is := ParseDiagnosis([]byte(demoDiagnosis), validPlan(t))
	if len(is) > 0 {
		t.Fatalf("데모 진단이 걸림:\n%s", is.Error())
	}
	if d.Fix.Patch[0].Path != "/cnpg/storageClass" || string(d.Fix.Patch[0].Value) != `"standard-rwo"` {
		t.Fatalf("수정안이 다름: %+v", d.Fix)
	}
}

func TestParseDiagnosisRejects(t *testing.T) {
	cases := []struct {
		name, old, new, want string
	}{
		{"원인 없음", `"cause": "values에 원본 EKS의 StorageClass gp3가 남아 있어 GKE에서 CNPG PVC가 Pending 상태"`, `"cause": " "`, "원인이 비어"},
		{"근거 없음", `["persistentvolumeclaim/shop-db-1 Pending: storageclass.storage.k8s.io \"gp3\" not found"]`, `[]`, "근거가 하나 이상"},
		{"모르는 종류", `"values-patch"`, `"shell"`, "kind"},
		{"다른 파일", `"values-gcp.yaml"`, `"values-aws.yaml"`, "환경 프로필"},
		{"허용 안 된 키", `"/cnpg/storageClass"`, `"/cnpg/image"`, "고칠 수 없는 values 키"},
		{"비밀값 env", `"/cnpg/storageClass"`, `"/api/env/DB_PASSWORD"`, "고칠 수 없는 values 키"},
		{"삭제 연산", `"op": "replace"`, `"op": "remove"`, `"replace" 또는 "add"`},
		{"값 없음", `, "value": "standard-rwo"`, ``, "값이 있어야"},
		{"경로 형식", `"/cnpg/storageClass"`, `"cnpg.storageClass"`, "고칠 수 없는 values 키"},
		{"모르는 키", `"evidence"`, `"evidence": [], "run"`, "unknown field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(demoDiagnosis, c.old) {
				t.Fatalf("기준 진단에 %q가 없음", c.old)
			}
			raw := strings.Replace(demoDiagnosis, c.old, c.new, 1)
			d, is := ParseDiagnosis([]byte(raw), validPlan(t))
			if len(is) == 0 {
				t.Fatalf("통과하면 안 되는데 통과함: %+v", d)
			}
			if !strings.Contains(is.Error(), c.want) {
				t.Fatalf("%q가 들어간 항목이 없음. 걸린 항목:\n%s", c.want, is.Error())
			}
		})
	}
}

func TestValuesPathAllowed(t *testing.T) {
	for path, want := range map[string]bool{
		"/cnpg/storageClass":             true,
		"/storageClass":                  true,
		"/order/replicas":                true,
		"/order/resources/limits/memory": true,
		"/order/env/PRODUCT_URL":         true,
		"/web/env/API_GATEWAY_HOST":      true,
		"/order/resources":               false, // resources 통째로 바꾸기는 막는다
		"/order/env/DB_PASSWORD":         false,
		"/order/env/PRODUCT_URL/x":       false,
		"/order/image/tag":               false,
		"/order/serviceAccountName":      false,
		"/":                              false,
		"":                               false,
	} {
		if got := valuesPathAllowed(path); got != want {
			t.Errorf("valuesPathAllowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestPlanParamFix(t *testing.T) {
	p := validPlan(t)
	f := Fix{Kind: KindPlanParam, Patch: []Op{{Op: "replace", Path: "/steps/2/timeoutSec", Value: []byte("20")}}}
	if is := f.Check(p); len(is) > 0 {
		t.Fatalf("정상 수정안이 걸림:\n%s", is.Error())
	}
	got, is := f.ApplyToPlan(p)
	if len(is) > 0 {
		t.Fatal(is.Error())
	}
	if *got.Steps[2].TimeoutSec != 20 {
		t.Fatalf("적용이 안 됨: %d", *got.Steps[2].TimeoutSec)
	}
	if *p.Steps[2].TimeoutSec != 30 {
		t.Fatal("원본 Plan이 바뀜")
	}
}

func TestPlanParamFixRejects(t *testing.T) {
	cases := []struct {
		name string
		op   Op
		want string
	}{
		{"고정 검사를 깨는 값", Op{"replace", "/steps/2/timeoutSec", []byte("120")}, "Plan 검사에 걸림"},
		{"원본 바로 삭제로 변경", Op{"replace", "/steps/5/delete", []byte(`"immediate"`)}, "Plan 검사에 걸림"},
		{"단계 이름 변경", Op{"replace", "/steps/0/id", []byte(`"shell"`)}, "고칠 수 없는 키"},
		{"대상 변경", Op{"replace", "/to", []byte(`"onprem"`)}, "/steps/<번호>/<키>"},
		{"다른 단계의 키", Op{"add", "/steps/0/gateway", []byte(`{"gcp":100}`)}, "precopy 단계에서 고칠 수 없는 키"},
		{"없는 단계 번호", Op{"replace", "/steps/6/db", []byte(`"x"`)}, "단계 번호"},
		{"앞에 0 붙은 번호", Op{"replace", "/steps/02/timeoutSec", []byte("20")}, "단계 번호"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := Fix{Kind: KindPlanParam, Patch: []Op{c.op}}
			is := f.Check(validPlan(t))
			if len(is) == 0 {
				t.Fatal("통과하면 안 되는데 통과함")
			}
			if !strings.Contains(is.Error(), c.want) {
				t.Fatalf("%q가 들어간 항목이 없음. 걸린 항목:\n%s", c.want, is.Error())
			}
		})
	}
}

func TestFixPatchLimit(t *testing.T) {
	ops := make([]Op, maxPatchOps+1)
	for i := range ops {
		ops[i] = Op{"replace", "/cnpg/storageClass", []byte(`"x"`)}
	}
	is := Fix{Kind: KindValuesPatch, File: "values-gcp.yaml", Patch: ops}.Check(validPlan(t))
	if !strings.Contains(is.Error(), "한 번에") {
		t.Fatalf("상한에 걸려야 함:\n%s", is.Error())
	}
}
