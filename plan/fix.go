package plan

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// 수정안 종류. 이 두 가지만 허용한다 (설계 문서 4장).
const (
	KindValuesPatch = "values-patch" // 환경 프로필(values) 사본을 고친다
	KindPlanParam   = "plan-param"   // Plan 사본의 단계 값을 고친다
)

// Fix는 AI가 낸 수정안이다. 원본이 아니라 사본에 적용하고 승인 화면에 diff로 보여준다.
type Fix struct {
	Kind  string `json:"kind"`
	File  string `json:"file,omitempty"`
	Patch []Op   `json:"patch"`
}

// Op는 JSON Patch(RFC 6902) 연산 중 replace와 add만 쓴다.
type Op struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// Diagnosis는 리허설 실패를 진단한 AI 응답이다.
type Diagnosis struct {
	Cause    string   `json:"cause"`
	Evidence []string `json:"evidence"`
	Fix      Fix      `json:"fix"`
}

// 리허설 한 번에 고칠 수 있는 줄 수 상한. 큰 수정은 사람이 보는 게 맞다.
const maxPatchOps = 5

// 엔드포인트 환경 변수로 볼 이름. 제안값이며 샘플 앱의 변수 이름이 정해지면 맞춘다.
var endpointEnv = regexp.MustCompile(`^[A-Z][A-Z0-9_]*_(HOST|URL|ENDPOINT|ADDR|PORT)$`)

// ParseDiagnosis는 진단 응답을 읽고 수정안이 p에 대해 허용되는지까지 검사한다.
func ParseDiagnosis(raw []byte, p *Plan) (*Diagnosis, Issues) {
	var d Diagnosis
	if is := decodeStrict(raw, &d); is != nil {
		return nil, is
	}
	var is Issues
	if strings.TrimSpace(d.Cause) == "" {
		is = append(is, Issue{"cause", "원인이 비어 있음"})
	}
	if len(d.Evidence) == 0 {
		is = append(is, Issue{"evidence", "로그에서 찾은 근거가 하나 이상 있어야 함"})
	}
	for _, x := range d.Fix.Check(p) {
		x.Path = joinPath("fix", x.Path)
		is = append(is, x)
	}
	if len(is) > 0 {
		return nil, is
	}
	return &d, nil
}

// Check는 수정안 형식과 고칠 수 있는 범위를 검사한다.
// plan-param이면 실제로 사본에 적용해 본 뒤 Plan 검사를 다시 통과하는지도 본다.
func (f Fix) Check(p *Plan) Issues {
	var is Issues
	add := func(path, msg string, a ...any) { is = append(is, Issue{path, fmt.Sprintf(msg, a...)}) }

	switch {
	case len(f.Patch) == 0:
		add("patch", "고칠 내용이 없음")
	case len(f.Patch) > maxPatchOps:
		add("patch", "한 번에 %d개까지만 고칠 수 있음 (받은 수 %d)", maxPatchOps, len(f.Patch))
	}
	for i, op := range f.Patch {
		path := fmt.Sprintf("patch[%d]", i)
		if op.Op != "replace" && op.Op != "add" {
			add(path+".op", `"replace" 또는 "add"만 허용 (받은 값 %q)`, op.Op)
		}
		if len(op.Value) == 0 || string(op.Value) == "null" {
			add(path+".value", "값이 있어야 함")
		}
	}

	switch f.Kind {
	case KindValuesPatch:
		if f.File != p.Profile {
			add("file", "Plan의 환경 프로필 %q만 고칠 수 있음 (받은 값 %q)", p.Profile, f.File)
		}
		for i, op := range f.Patch {
			if !valuesPathAllowed(op.Path) {
				add(fmt.Sprintf("patch[%d].path", i), "고칠 수 없는 values 키 %q (허용: storageClass, replicas, resources 아래, env의 엔드포인트 변수)", op.Path)
			}
		}
	case KindPlanParam:
		if f.File != "" {
			add("file", "plan-param에는 file을 쓰지 않음")
		}
		for i, op := range f.Patch {
			if _, _, err := planParamPath(op.Path); err != nil {
				add(fmt.Sprintf("patch[%d].path", i), "%s", err)
			}
		}
		if len(is) == 0 {
			if _, perr := f.ApplyToPlan(p); perr != nil {
				for _, x := range perr {
					add("patch", "적용하면 Plan 검사에 걸림: %s", x)
				}
			}
		}
	default:
		add("kind", "%q 또는 %q만 허용 (받은 값 %q)", KindValuesPatch, KindPlanParam, f.Kind)
	}
	return is
}

// ApplyToPlan은 plan-param 수정안을 p의 사본에 적용하고, 결과를 다시 검사해서 돌려준다. p는 바꾸지 않는다.
func (f Fix) ApplyToPlan(p *Plan) (*Plan, Issues) {
	if f.Kind != KindPlanParam {
		return nil, Issues{{"kind", "plan-param 수정안만 Plan에 적용할 수 있음"}}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, Issues{{Message: err.Error()}}
	}
	var doc map[string]json.RawMessage
	var steps []map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	_ = json.Unmarshal(doc["steps"], &steps)

	for i, op := range f.Patch {
		path := fmt.Sprintf("patch[%d].path", i)
		n, key, err := planParamPath(op.Path)
		if err != nil {
			return nil, Issues{{path, err.Error()}}
		}
		if n >= len(steps) {
			return nil, Issues{{path, "없는 단계 번호"}}
		}
		if _, exists := steps[n][key]; op.Op == "replace" && !exists {
			return nil, Issues{{path, "replace는 이미 있는 키에만 쓸 수 있음"}}
		}
		steps[n][key] = op.Value
	}

	doc["steps"], _ = json.Marshal(steps)
	out, _ := json.Marshal(doc)
	return Parse(out)
}

// planParamPath는 /steps/<번호>/<키> 형태만 받는다. 단계 이름(id)과 Plan 머리(app, from, to 등)는 못 고친다.
func planParamPath(path string) (int, string, error) {
	seg, err := splitPointer(path)
	if err != nil {
		return 0, "", err
	}
	if len(seg) != 3 || seg[0] != "steps" {
		return 0, "", fmt.Errorf("/steps/<번호>/<키> 형태만 고칠 수 있음 (받은 값 %q)", path)
	}
	n, err := strconv.Atoi(seg[1])
	if err != nil || n < 0 || n >= len(Order) || seg[1] != strconv.Itoa(n) {
		return 0, "", fmt.Errorf("단계 번호는 0~%d여야 함 (받은 값 %q)", len(Order)-1, seg[1])
	}
	if !slices.Contains(stepKeys[Order[n]][1:], seg[2]) {
		return 0, "", fmt.Errorf("%s 단계에서 고칠 수 없는 키 %q", Order[n], seg[2])
	}
	return n, seg[2], nil
}

// valuesPathAllowed는 설계 문서 3장의 허용 목록을 따른다.
func valuesPathAllowed(path string) bool {
	seg, err := splitPointer(path)
	if err != nil || len(seg) == 0 {
		return false
	}
	last := seg[len(seg)-1]
	if last == "storageClass" || last == "replicas" {
		return true
	}
	for i, s := range seg {
		if s == "resources" && i < len(seg)-1 {
			return true
		}
		if s == "env" && i == len(seg)-2 && endpointEnv.MatchString(last) {
			return true
		}
	}
	return false
}

// splitPointer는 JSON Pointer(RFC 6901)를 조각으로 나눈다.
func splitPointer(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("경로는 /로 시작해야 함 (받은 값 %q)", path)
	}
	seg := strings.Split(path[1:], "/")
	for i, s := range seg {
		if s == "" {
			return nil, fmt.Errorf("빈 경로 조각이 있음 (받은 값 %q)", path)
		}
		seg[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return seg, nil
}

func joinPath(a, b string) string {
	if b == "" {
		return a
	}
	return a + "." + b
}
