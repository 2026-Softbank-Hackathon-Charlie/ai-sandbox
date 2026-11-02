// Package plan은 AI가 낸 Plan JSON을 읽고, 스키마와 고정 규칙으로 검사한다.
// AI는 단계를 새로 만들 수 없고 정해진 6단계의 값만 채운다 (설계 문서 3장).
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Env는 이전 원본·대상이 될 수 있는 클러스터다.
type Env string

const (
	AWS    Env = "aws"
	GCP    Env = "gcp"
	OnPrem Env = "onprem"
)

var envs = []Env{AWS, GCP, OnPrem}

// StepID는 엔진이 구현을 미리 가진 단계 이름이다. 순서도 고정이다.
type StepID string

const (
	Precopy StepID = "precopy"
	Sync    StepID = "sync"
	Fence   StepID = "fence"
	Verify  StepID = "verify"
	Cutover StepID = "cutover"
	Cleanup StepID = "cleanup"
)

// Order는 Plan의 steps가 따라야 하는 순서다.
var Order = []StepID{Precopy, Sync, Fence, Verify, Cutover, Cleanup}

// 단계마다 쓸 수 있는 키. 다른 단계의 키가 섞이면 거절한다.
var stepKeys = map[StepID][]string{
	Precopy: {"id", "db", "files"},
	Sync:    {"id", "until"},
	Fence:   {"id", "holdWrites", "timeoutSec"},
	Verify:  {"id", "rows", "files"},
	Cutover: {"id", "gateway"},
	Cleanup: {"id", "source", "delete"},
}

const (
	// RollbackRule은 지금 허용하는 유일한 원복 규칙이다 (설계 문서 8장).
	RollbackRule = "auto-until-target-accepts-writes"
	// MaxFenceSec은 쓰기 보류 시간의 상한이다. 제안값이며 11/6에 확정한다.
	MaxFenceSec = 30
	// DeleteNeedsApproval은 원본 삭제에 허용하는 유일한 값이다.
	DeleteNeedsApproval = "needs-approval"
)

type Plan struct {
	App      string `json:"app"`
	From     Env    `json:"from"`
	To       Env    `json:"to"`
	Profile  string `json:"profile"`
	Steps    []Step `json:"steps"`
	Rollback string `json:"rollback"`
}

// Step은 6단계의 값을 한 구조체에 담는다. 단계별로 어떤 키가 허용되는지는 stepKeys가 정한다.
type Step struct {
	ID StepID `json:"id"`

	// precopy
	// DB는 DB 이전 방식(주제 문서 3장 2번)이 정해지면 enum으로 바꾼다. 지금은 문자열 그대로 둔다.
	DB    string `json:"db,omitempty"`
	Files string `json:"files,omitempty"` // precopy, verify

	// sync
	Until *Until `json:"until,omitempty"`

	// fence
	HoldWrites *bool `json:"holdWrites,omitempty"`
	TimeoutSec *int  `json:"timeoutSec,omitempty"`

	// verify
	Rows string `json:"rows,omitempty"`

	// cutover
	Gateway map[Env]int `json:"gateway,omitempty"`

	// cleanup
	Source string `json:"source,omitempty"`
	Delete string `json:"delete,omitempty"`
}

type Until struct {
	DBLagSec    *float64 `json:"dbLagSec"`
	FilesBehind *int     `json:"filesBehind"`
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

// Parse는 AI 응답 원문을 읽어 검사까지 마친다. Issues가 비어 있을 때만 Plan을 쓴다.
func Parse(raw []byte) (*Plan, Issues) {
	// 먼저 단계별 키를 본다. 한 구조체로 바로 읽으면 다른 단계의 키가 섞여도 통과하기 때문이다.
	var head struct {
		App      json.RawMessage   `json:"app"`
		From     json.RawMessage   `json:"from"`
		To       json.RawMessage   `json:"to"`
		Profile  json.RawMessage   `json:"profile"`
		Steps    []json.RawMessage `json:"steps"`
		Rollback json.RawMessage   `json:"rollback"`
	}
	if is := decodeStrict(raw, &head); is != nil {
		return nil, is
	}

	var issues Issues
	for i, s := range head.Steps {
		issues = append(issues, checkStepKeys(i, s)...)
	}
	if len(issues) > 0 {
		return nil, issues
	}

	var p Plan
	if is := decodeStrict(raw, &p); is != nil {
		return nil, is
	}
	if is := p.Validate(); len(is) > 0 {
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

func checkStepKeys(i int, raw json.RawMessage) Issues {
	path := fmt.Sprintf("steps[%d]", i)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return Issues{{path, "객체여야 함"}}
	}
	var id StepID
	if err := json.Unmarshal(m["id"], &id); err != nil || m["id"] == nil {
		return Issues{{path + ".id", "문자열 단계 이름이 있어야 함"}}
	}
	allowed, ok := stepKeys[id]
	if !ok {
		return Issues{{path + ".id", fmt.Sprintf("모르는 단계 %q (허용: %s)", id, joinIDs(Order))}}
	}
	var issues Issues
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			issues = append(issues, Issue{path + "." + k, fmt.Sprintf("%s 단계에서 쓸 수 없는 키 (허용: %s)", id, strings.Join(allowed[1:], ", "))})
		}
	}
	return issues
}

var appName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Validate는 스키마 값 검사와 고정 검사를 함께 한다 (설계 문서 3장).
func (p *Plan) Validate() Issues {
	var is Issues
	add := func(path, msg string, a ...any) { is = append(is, Issue{path, fmt.Sprintf(msg, a...)}) }

	if !appName.MatchString(p.App) {
		add("app", "소문자, 숫자, 하이픈으로 된 앱 이름이어야 함 (받은 값 %q)", p.App)
	}
	if !slices.Contains(envs, p.From) {
		add("from", "aws, gcp, onprem 중 하나여야 함 (받은 값 %q)", p.From)
	}
	if !slices.Contains(envs, p.To) {
		add("to", "aws, gcp, onprem 중 하나여야 함 (받은 값 %q)", p.To)
	}
	if p.From != "" && p.From == p.To {
		add("to", "원본과 대상이 같음")
	}
	if !strings.HasSuffix(p.Profile, ".yaml") || strings.ContainsAny(p.Profile, `/\`) {
		add("profile", "같은 폴더의 .yaml 파일 이름이어야 함 (받은 값 %q)", p.Profile)
	}
	if p.Rollback != RollbackRule {
		add("rollback", "%q만 허용 (받은 값 %q)", RollbackRule, p.Rollback)
	}

	got := make([]StepID, len(p.Steps))
	for i, s := range p.Steps {
		got[i] = s.ID
	}
	if !slices.Equal(got, Order) {
		add("steps", "단계는 %s 순서로 정확히 6개여야 함 (받은 순서: %s)", joinIDs(Order), joinIDs(got))
		return is
	}

	for i, s := range p.Steps {
		path := fmt.Sprintf("steps[%d]", i)
		switch s.ID {
		case Precopy:
			if s.Files != "rclone sync" {
				add(path+".files", `"rclone sync"만 허용 (받은 값 %q)`, s.Files)
			}
		case Sync:
			switch {
			case s.Until == nil:
				add(path+".until", "dbLagSec와 filesBehind가 있어야 함")
			default:
				if s.Until.DBLagSec == nil || *s.Until.DBLagSec < 0 {
					add(path+".until.dbLagSec", "0 이상인 숫자가 있어야 함")
				}
				if s.Until.FilesBehind == nil || *s.Until.FilesBehind < 0 {
					add(path+".until.filesBehind", "0 이상인 정수가 있어야 함")
				}
			}
		case Fence:
			// 쓰기 주체를 한 곳으로 유지하려면 전환 동안 쓰기를 붙잡아야 한다.
			if s.HoldWrites == nil || !*s.HoldWrites {
				add(path+".holdWrites", "true여야 함 (전환 중 양쪽 동시 쓰기 금지)")
			}
			if s.TimeoutSec == nil {
				add(path+".timeoutSec", "쓰기 보류 시간 상한이 있어야 함")
			} else if *s.TimeoutSec < 1 || *s.TimeoutSec > MaxFenceSec {
				add(path+".timeoutSec", "1~%d초여야 함 (받은 값 %d)", MaxFenceSec, *s.TimeoutSec)
			}
		case Verify:
			if s.Rows != "pk-compare" {
				add(path+".rows", `"pk-compare"만 허용 (받은 값 %q)`, s.Rows)
			}
			if s.Files != "rclone check" {
				add(path+".files", `"rclone check"만 허용 (받은 값 %q)`, s.Files)
			}
		case Cutover:
			is = append(is, checkGateway(path+".gateway", s.Gateway, p.To)...)
		case Cleanup:
			if s.Source != "scale-to-zero" {
				add(path+".source", `"scale-to-zero"만 허용 (받은 값 %q)`, s.Source)
			}
			if s.Delete != DeleteNeedsApproval {
				add(path+".delete", "원본 삭제는 %q여야 함 (받은 값 %q)", DeleteNeedsApproval, s.Delete)
			}
		}
	}
	return is
}

// checkGateway는 전환이 끝났을 때 트래픽이 대상 한 곳으로만 가는지 본다.
func checkGateway(path string, gw map[Env]int, to Env) Issues {
	if len(gw) == 0 {
		return Issues{{path, fmt.Sprintf("대상 비율이 있어야 함 (예: {%q: 100})", to)}}
	}
	var is Issues
	for env, w := range gw {
		switch {
		case !slices.Contains(envs, env):
			is = append(is, Issue{path, fmt.Sprintf("모르는 환경 %q", env)})
		case env == to && w != 100:
			is = append(is, Issue{path, fmt.Sprintf("대상 %s는 100이어야 함 (받은 값 %d)", env, w)})
		case env != to && w != 0:
			is = append(is, Issue{path, fmt.Sprintf("대상이 아닌 %s는 0이어야 함 (받은 값 %d, 쓰기 주체는 한 곳)", env, w)})
		}
	}
	if _, ok := gw[to]; !ok {
		is = append(is, Issue{path, fmt.Sprintf("대상 %s 비율이 없음", to)})
	}
	sort.Slice(is, func(a, b int) bool { return is[a].Message < is[b].Message })
	return is
}

func joinIDs(ids []StepID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = string(id)
	}
	return strings.Join(s, " → ")
}
