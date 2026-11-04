package plan

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// 수정안 종류. 이 두 가지만 허용한다.
const (
	KindManifestPatch = "manifest-patch" // 대상 매니페스트(target.yaml) 사본을 고친다
	KindPlanParam     = "plan-param"     // Plan 사본의 상한값과 검증 범위를 고친다
)

// Fix는 AI가 낸 수정안이다. 원본이 아니라 사본에 적용하고, 다시 첨부로 올려 승인 화면에 diff로 보여준다.
type Fix struct {
	Kind  string `json:"kind"`
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

// Plan에서 plan-param으로 고칠 수 있는 경로
var planParamPaths = []string{
	"/thresholds/maxLagBytes", "/thresholds/catchupTimeout", "/thresholds/switchoverTimeout", "/thresholds/maxDowntime",
	"/verify/tables",
}

// ParseDiagnosis는 진단 응답을 읽고, 수정안을 p 사본에 적용해도 모든 검사를 통과하는지까지 본다.
func ParseDiagnosis(raw []byte, p *Proposal, draft *eplan.Plan) (*Diagnosis, Issues) {
	var d Diagnosis
	if is := decodeStrict(raw, &d); is != nil {
		return nil, is
	}
	var is Issues
	if strings.TrimSpace(d.Cause) == "" {
		is.add("cause", "원인이 비어 있음")
	} else if n := utf8.RuneCountInString(d.Cause); n > maxCauseLen {
		is.add("cause", "%d자 이하여야 함 (받은 길이 %d자)", maxCauseLen, n)
	}
	is = append(is, checkTexts("evidence", "로그에서 찾은 근거", d.Evidence, maxEvidenceLen)...)
	if _, fis := d.Fix.Apply(p, draft); len(fis) > 0 {
		for _, x := range fis {
			x.Path = joinPath("fix", x.Path)
			is = append(is, x)
		}
	}
	if len(is) > 0 {
		return nil, is
	}
	return &d, nil
}

// Apply는 수정안을 p의 사본에 적용하고, 결과가 모든 검사를 통과하면 돌려준다. p는 바꾸지 않는다.
func (f Fix) Apply(p *Proposal, draft *eplan.Plan) (*Proposal, Issues) {
	var is Issues
	switch {
	case len(f.Patch) == 0:
		is.add("patch", "고칠 내용이 없음")
	case len(f.Patch) > maxPatchOps:
		is.add("patch", "한 번에 %d개까지만 고칠 수 있음 (받은 수 %d)", maxPatchOps, len(f.Patch))
	}
	for i, op := range f.Patch {
		path := fmt.Sprintf("patch[%d]", i)
		if op.Op != "replace" && op.Op != "add" {
			is.add(path+".op", `"replace" 또는 "add"만 허용 (받은 값 %q)`, op.Op)
		}
		if len(op.Value) == 0 || string(op.Value) == "null" {
			is.add(path+".value", "값이 있어야 함")
		}
		seg, err := splitPointer(op.Path)
		if err != nil {
			is.add(path+".path", "%s", err)
			continue
		}
		switch f.Kind {
		case KindManifestPatch:
			if !manifestPathAllowed(seg) {
				is.add(path+".path", "고칠 수 없는 매니페스트 경로 %q (허용: spec.storage·walStorage의 storageClass·size, spec.resources 아래, spec.instances, spec.imageName, spec.smartShutdownTimeout)", op.Path)
			}
		case KindPlanParam:
			if !slices.Contains(planParamPaths, op.Path) {
				is.add(path+".path", "고칠 수 없는 Plan 경로 %q (허용: %s)", op.Path, strings.Join(planParamPaths, ", "))
			}
		}
	}
	if f.Kind != KindManifestPatch && f.Kind != KindPlanParam {
		is.add("kind", "%q 또는 %q만 허용 (받은 값 %q)", KindManifestPatch, KindPlanParam, f.Kind)
	}
	if f.Kind == KindManifestPatch && p.Plan.Target.Manifest != ManifestRef {
		is.add("kind", "이 Plan에는 대상 매니페스트가 없음")
	}
	if len(is) > 0 {
		return nil, is
	}

	next := &Proposal{Plan: p.Plan, Manifest: p.Manifest, Notes: append([]string(nil), p.Notes...), Source: p.Source, SourceImage: p.SourceImage}
	switch f.Kind {
	case KindManifestPatch:
		m, err := patchYAML(p.Manifest, f.Patch)
		if err != nil {
			return nil, Issues{{"patch", err.Error()}}
		}
		next.Manifest = m
	case KindPlanParam:
		np, err := patchPlan(&p.Plan, f.Patch)
		if err != nil {
			return nil, Issues{{"patch", err.Error()}}
		}
		next.Plan = *np
	}
	if cis := next.Check(draft); len(cis) > 0 {
		for _, x := range cis {
			is.add("patch", "적용하면 검사에 걸림: %s", x)
		}
		return nil, is
	}
	return next, nil
}

// patchPlan은 Plan을 JSON으로 바꿔 연산을 적용하고 엔진 형식으로 다시 읽는다.
func patchPlan(p *eplan.Plan, ops []Op) (*eplan.Plan, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	m := doc.(map[string]any)
	for i, op := range ops {
		seg, _ := splitPointer(op.Path)
		var v any
		if err := json.Unmarshal(op.Value, &v); err != nil {
			return nil, fmt.Errorf("patch[%d] 값을 읽을 수 없음: %w", i, err)
		}
		// 엔진은 thresholds·verify를 omitempty 없이 내보내지만, 혹시 없으면 만든다
		if _, ok := m[seg[0]]; !ok {
			m[seg[0]] = map[string]any{}
		}
		if err := setPointer(doc, seg, v, false); err != nil {
			return nil, fmt.Errorf("patch[%d] %s: %w", i, op.Path, err)
		}
	}
	out, _ := json.Marshal(doc)
	var np eplan.Plan
	if is := decodeStrict(out, &np); is != nil {
		return nil, fmt.Errorf("고친 Plan을 읽을 수 없음: %s", is.Error())
	}
	return &np, nil
}
