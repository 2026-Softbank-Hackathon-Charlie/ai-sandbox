package plan

import (
	"os"
	"strings"
	"testing"
)

func validJSON(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/valid.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// replace는 정상 Plan에서 한 부분만 바꾼다. old가 없으면 테스트가 잘못된 것이다.
func replace(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("기준 Plan에 %q가 없음", old)
	}
	return strings.Replace(s, old, new, 1)
}

func TestParseValid(t *testing.T) {
	p, is := Parse([]byte(validJSON(t)))
	if len(is) > 0 {
		t.Fatalf("정상 Plan이 걸림:\n%s", is.Error())
	}
	if p.App != "shop" || p.From != AWS || p.To != GCP || len(p.Steps) != 6 {
		t.Fatalf("읽은 값이 다름: %+v", p)
	}
	if *p.Steps[2].TimeoutSec != 30 || p.Steps[4].Gateway[GCP] != 100 {
		t.Fatalf("단계 값이 다름: %+v", p.Steps)
	}
}

func TestParseRejectsBrokenAIOutput(t *testing.T) {
	v := validJSON(t)
	cases := []struct {
		name string
		raw  string
		want string // 걸린 항목 중 하나에 들어 있어야 하는 글
	}{
		// 형식이 깨진 응답
		{"빈 응답", "", "JSON으로 읽을 수 없음"},
		{"설명 글만", "다음은 이전 계획입니다.", "JSON으로 읽을 수 없음"},
		{"마크다운 코드 블록", "```json\n" + v + "\n```", "JSON으로 읽을 수 없음"},
		{"중간에 끊김", v[:len(v)/2], "JSON으로 읽을 수 없음"},
		{"JSON 뒤에 설명", v + "\n이 계획은 안전합니다.", "JSON 객체 하나만"},
		{"객체 두 개", v + v, "JSON 객체 하나만"},
		{"배열", "[" + v + "]", "JSON으로 읽을 수 없음"},
		{"숫자를 문자열로", replace(t, v, `"timeoutSec": 30`, `"timeoutSec": "30"`), "JSON으로 읽을 수 없음"},
		{"모르는 최상위 키", replace(t, v, `"app": "shop",`, `"app": "shop", "notes": "x",`), "unknown field"},

		// 단계 구성
		{"모르는 단계", replace(t, v, `"id": "verify"`, `"id": "shell"`), `모르는 단계 "shell"`},
		{"다른 단계의 키", replace(t, v, `"holdWrites": true,`, `"holdWrites": true, "gateway": {"gcp": 100},`), "fence 단계에서 쓸 수 없는 키"},
		{"명령어 키 끼워 넣기", replace(t, v, `"files": "rclone sync"`, `"files": "rclone sync", "cmd": "kubectl delete ns prod"`), "쓸 수 없는 키"},
		{"단계 순서 바뀜", swapSteps(t, v), "순서로 정확히 6개"},
		{"단계 빠짐", replace(t, v, `{ "id": "verify",  "rows": "pk-compare", "files": "rclone check" },`, ""), "순서로 정확히 6개"},
		{"단계 이름 없음", replace(t, v, `{ "id": "sync",    "until"`, `{ "until"`), "문자열 단계 이름"},

		// 머리 값
		{"원본과 대상이 같음", replace(t, v, `"to": "gcp"`, `"to": "aws"`), "원본과 대상이 같음"},
		{"모르는 환경", replace(t, v, `"to": "gcp"`, `"to": "azure"`), "aws, gcp, onprem 중 하나"},
		{"앱 이름에 대문자", replace(t, v, `"shop"`, `"Shop"`), "앱 이름"},
		{"프로필 경로 탈출", replace(t, v, `"values-gcp.yaml"`, `"../secrets.yaml"`), ".yaml 파일 이름"},
		{"다른 원복 규칙", replace(t, v, RollbackRule, "never"), "rollback"},

		// 고정 검사
		{"쓰기 보류 30초 초과", replace(t, v, `"timeoutSec": 30`, `"timeoutSec": 60`), "1~30초"},
		{"쓰기 보류 시간 없음", replace(t, v, `"holdWrites": true, "timeoutSec": 30`, `"holdWrites": true`), "보류 시간 상한"},
		{"쓰기 보류 안 함", replace(t, v, `"holdWrites": true`, `"holdWrites": false`), "양쪽 동시 쓰기 금지"},
		{"양쪽으로 나눠 보냄", replace(t, v, `{ "gcp": 100 }`, `{ "gcp": 50, "aws": 50 }`), "쓰기 주체는 한 곳"},
		{"대상 비율 없음", replace(t, v, `{ "gcp": 100 }`, `{ "aws": 0 }`), "대상 gcp 비율이 없음"},
		{"원본 바로 삭제", replace(t, v, `"needs-approval"`, `"immediate"`), "needs-approval"},
		{"동기화 기준 음수", replace(t, v, `"dbLagSec": 1`, `"dbLagSec": -1`), "0 이상인 숫자"},
		{"동기화 기준 빠짐", replace(t, v, `"until": { "dbLagSec": 1, "filesBehind": 0 }`, `"until": { "dbLagSec": 1 }`), "filesBehind"},
		{"모르는 검증 방식", replace(t, v, `"pk-compare"`, `"row-count"`), "pk-compare"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, is := Parse([]byte(c.raw))
			if len(is) == 0 {
				t.Fatalf("통과하면 안 되는데 통과함: %+v", p)
			}
			if p != nil {
				t.Fatal("걸렸는데 Plan도 돌려줌")
			}
			if !strings.Contains(is.Error(), c.want) {
				t.Fatalf("%q가 들어간 항목이 없음. 걸린 항목:\n%s", c.want, is.Error())
			}
		})
	}
}

func swapSteps(t *testing.T, v string) string {
	t.Helper()
	a := `{ "id": "fence",   "holdWrites": true, "timeoutSec": 30 },`
	b := `{ "id": "verify",  "rows": "pk-compare", "files": "rclone check" },`
	return replace(t, replace(t, replace(t, v, a, "@@A@@"), b, a), "@@A@@", b)
}

func TestValidateCollectsAllIssues(t *testing.T) {
	// AI에 한 번에 다 돌려줘야 재요청 횟수가 준다.
	v := validJSON(t)
	v = replace(t, v, `"timeoutSec": 30`, `"timeoutSec": 99`)
	v = replace(t, v, `"needs-approval"`, `"now"`)
	v = replace(t, v, `"to": "gcp"`, `"to": "aws"`)
	_, is := Parse([]byte(v))
	if len(is) < 3 {
		t.Fatalf("세 가지가 모두 걸려야 함:\n%s", is.Error())
	}
}
