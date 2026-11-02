// Package plan은 AI가 낸 이전 계획(엔진 Plan + 대상 CNPG 매니페스트)을 검사한다.
//
// 엔진의 9단계는 엔진에 고정되어 있고 Plan은 무엇을 어디로 옮길지만 정한다 (migration-engine/pkg/plan).
// AI는 플랫폼의 Plan 초안(POST /v1/plans/draft)에서 출발한다. 데이터가 가는 곳(원본, 대상, DSN,
// 게이트웨이, 파일 위치)은 초안과 같아야 하고, AI는 상한값, 검증 범위, 대상 매니페스트만 정한다.
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// ManifestName은 대상 매니페스트를 Plan 첨부로 올릴 때 쓰는 이름이다.
// Plan의 target.manifest는 "attachment:target.yaml"로 이 첨부를 가리킨다 (platform ADR 0006).
const ManifestName = "target.yaml"

// ManifestRef는 target.manifest에 들어갈 값이다.
const ManifestRef = "attachment:" + ManifestName

// Proposal은 AI의 계획 출력이다.
type Proposal struct {
	Plan eplan.Plan `json:"plan"`
	// Manifest는 대상 클러스터에 만들 CNPG Cluster YAML이다. 원본의 복제본으로 시작한다.
	Manifest string `json:"manifest"`
	// Notes는 AI가 값을 정한 근거다. 승인 화면과 에이전트 기록에 보인다.
	Notes []string `json:"notes"`
}

// Issue는 검사에서 걸린 항목 하나다. 재요청할 때 그대로 AI에 돌려준다.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (i Issue) String() string {
	if i.Path == "" {
		return i.Message
	}
	return i.Path + ": " + i.Message
}

// Issues는 검사 결과 목록이다. 비어 있으면 통과다.
type Issues []Issue

func (is Issues) Error() string {
	s := make([]string, len(is))
	for i, x := range is {
		s[i] = x.String()
	}
	return strings.Join(s, "\n")
}

func (is *Issues) add(path, msg string, a ...any) {
	*is = append(*is, Issue{path, fmt.Sprintf(msg, a...)})
}

// 상한값 범위. 데모 3분 안에 끝나야 하고, 게이트웨이의 쓰기 보류 상한(-max-hold 90s)보다 짧아야 한다.
// 제안값이며 11/6에 확정한다.
var (
	maxDowntimeRange       = [2]time.Duration{5 * time.Second, 60 * time.Second}
	switchoverTimeoutRange = [2]time.Duration{30 * time.Second, 3 * time.Minute}
	catchupTimeoutRange    = [2]time.Duration{1 * time.Minute, 15 * time.Minute}
	maxLagBytesRange       = [2]int64{1 << 10, 64 << 20}
)

// Parse는 AI 응답 원문을 읽고 초안과 비교해 검사한다. Issues가 비어 있을 때만 Proposal을 쓴다.
func Parse(raw []byte, draft *eplan.Plan) (*Proposal, Issues) {
	var p Proposal
	if is := decodeStrict(raw, &p); is != nil {
		return nil, is
	}
	if is := p.Check(draft); len(is) > 0 {
		return nil, is
	}
	return &p, nil
}

// decodeStrict는 모르는 키, 타입 불일치, JSON 뒤에 붙은 글을 모두 오류로 본다.
func decodeStrict(raw []byte, v any) Issues {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Issues{{Message: "JSON으로 읽을 수 없음: " + err.Error()}}
	}
	if _, err := dec.Token(); err != io.EOF {
		return Issues{{Message: "JSON 객체 하나만 보내야 함 (뒤에 다른 내용이 있음)"}}
	}
	return nil
}

// Check는 엔진의 검사, 초안과의 비교, 고정 규칙, 매니페스트 검사를 한 번에 하고 걸린 항목을 모두 돌려준다.
func (p *Proposal) Check(draft *eplan.Plan) Issues {
	var is Issues
	if err := p.Plan.Validate(); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			is.add("plan", "엔진 검사: %s", line)
		}
	}
	is = append(is, CheckAgainstDraft(&p.Plan, draft)...)
	is = append(is, CheckRules(&p.Plan)...)
	if p.Plan.Target.Manifest == ManifestRef {
		for _, x := range CheckManifest(p.Manifest, &p.Plan) {
			x.Path = joinPath("manifest", x.Path)
			is = append(is, x)
		}
	} else if strings.TrimSpace(p.Manifest) != "" {
		is.add("manifest", "target.manifest가 %q가 아니면 매니페스트를 보내지 않음", ManifestRef)
	}
	if len(p.Notes) == 0 {
		is.add("notes", "값을 정한 근거가 하나 이상 있어야 함")
	}
	return is
}

// CheckAgainstDraft는 데이터가 가는 곳이 초안과 같은지 본다. AI가 DSN이나 게이트웨이를 다른 곳으로 돌리지 못하게 한다.
func CheckAgainstDraft(p, draft *eplan.Plan) Issues {
	var is Issues
	if draft == nil {
		is.add("", "비교할 Plan 초안이 없음")
		return is
	}
	same := func(path string, got, want any) {
		if !reflect.DeepEqual(got, want) {
			is.add(path, "초안과 같아야 함 (초안 %v, 받은 값 %v)", show(want), show(got))
		}
	}
	same("plan.id", p.ID, draft.ID)
	same("plan.source", p.Source, draft.Source)
	for _, f := range []struct {
		path      string
		got, want string
	}{
		{"plan.target.env", p.Target.Env, draft.Target.Env},
		{"plan.target.context", p.Target.Context, draft.Target.Context},
		{"plan.target.kubeconfig", p.Target.Kubeconfig, draft.Target.Kubeconfig},
		{"plan.target.namespace", p.Target.Namespace, draft.Target.Namespace},
		{"plan.target.cluster", p.Target.Cluster, draft.Target.Cluster},
		{"plan.target.dsn", p.Target.DSN, draft.Target.DSN},
	} {
		same(f.path, f.got, f.want)
	}
	same("plan.gateway", p.Gateway, draft.Gateway)
	switch {
	case draft.Files == nil && p.Files != nil:
		is.add("plan.files", "초안에 없는 파일 이전을 넣을 수 없음")
	case draft.Files != nil && p.Files != nil:
		same("plan.files.source", p.Files.Source, draft.Files.Source)
		same("plan.files.target", p.Files.Target, draft.Files.Target)
		same("plan.files.rclone", p.Files.Rclone, draft.Files.Rclone)
	}
	same("plan.verify.writtenRecords", p.Verify.WrittenRecords, draft.Verify.WrittenRecords)
	// 검증할 스키마를 줄이면 누락을 못 볼 수 있다. 테이블 필터(verify.tables)도 초안에 없으면 넣지 않는다.
	same("plan.verify.schemas", p.Verify.Schemas, draft.Verify.Schemas)
	if len(draft.Verify.Tables) == 0 && len(p.Verify.Tables) > 0 {
		is.add("plan.verify.tables", "초안이 모든 테이블을 검증하면 범위를 줄일 수 없음")
	}
	return is
}

// CheckRules는 초안과 상관없이 늘 지키는 규칙이다.
func CheckRules(p *eplan.Plan) Issues {
	var is Issues
	if m := p.Target.Manifest; m != "" && m != ManifestRef {
		is.add("plan.target.manifest", "비우거나 %q여야 함 (서버 파일 경로를 직접 쓰지 않음)", ManifestRef)
	}
	if p.Verify.FenceGate != nil && !*p.Verify.FenceGate {
		is.add("plan.verify.fenceGate", "끌 수 없음 (쓰기 보류 중 데이터 비교가 원복의 근거)")
	}
	for _, t := range p.Verify.Tables {
		if strings.ContainsAny(t, " ;\"'`") || t == "" {
			is.add("plan.verify.tables", "테이블 이름 형식이 잘못됨: %q", t)
		}
	}
	th := p.Thresholds
	inDur := func(path string, d eplan.Duration, r [2]time.Duration) {
		if d.Duration != 0 && (d.Duration < r[0] || d.Duration > r[1]) {
			is.add(path, "%s~%s여야 함 (받은 값 %s)", r[0], r[1], d.Duration)
		}
	}
	inDur("plan.thresholds.maxDowntime", th.MaxDowntime, maxDowntimeRange)
	inDur("plan.thresholds.switchoverTimeout", th.SwitchoverTimeout, switchoverTimeoutRange)
	inDur("plan.thresholds.catchupTimeout", th.CatchupTimeout, catchupTimeoutRange)
	if th.MaxLagBytes != 0 && (th.MaxLagBytes < maxLagBytesRange[0] || th.MaxLagBytes > maxLagBytesRange[1]) {
		is.add("plan.thresholds.maxLagBytes", "%d~%d여야 함 (받은 값 %d)", maxLagBytesRange[0], maxLagBytesRange[1], th.MaxLagBytes)
	}
	return is
}

func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func joinPath(a, b string) string {
	if b == "" {
		return a
	}
	return a + "." + b
}
