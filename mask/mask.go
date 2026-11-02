// Package mask는 AI에 보내기 전에 로그와 설정에서 비밀값을 지운다 (설계 문서 4장).
// 원문은 이 패키지 밖으로 내보내지 않는다. 지운 자리는 Placeholder로 바꾼다.
package mask

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// Placeholder는 지운 값 자리에 남기는 표시다.
const Placeholder = "***"

type rule struct {
	re   *regexp.Regexp
	repl string
}

// 순서가 중요하다. 넓은 덩어리(PEM)를 먼저 지우고, 키=값 규칙은 마지막에 둔다.
var rules = []rule{
	// PEM 개인 키, 인증서
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----[\s\S]*?-----END [A-Z0-9 ]+-----`), Placeholder},
	// Authorization 헤더
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + Placeholder},
	// JWT (Kubernetes ServiceAccount 토큰 포함)
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`), Placeholder},
	// 알려진 키 형식
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), Placeholder},   // AWS 액세스 키
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{10,}`), Placeholder},   // Anthropic
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), Placeholder},  // GitHub
	{regexp.MustCompile(`\bxox[abpr]-[A-Za-z0-9-]{10,}`), Placeholder}, // Slack
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), Placeholder},     // Google API
	{regexp.MustCompile(`\btskey-[A-Za-z0-9-]{10,}`), Placeholder},     // Tailscale
}

var (
	variable    = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
	dsnPassword = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]+:)([^@\s]+)@`)
	assignment  = regexp.MustCompile(`(?i)("?[A-Za-z0-9_.-]*(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)[A-Za-z0-9_.-]*"?\s*[:=]\s*)("[^"]*"|'[^']*'|\$\{[A-Za-z_][A-Za-z0-9_]*\}|[^\s,}]+)`)
)

var secretKey = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|authorization|cookie)`)

// SecretKey는 키 이름이 비밀값을 담는 이름인지 알려준다 (JSON, YAML 키).
// input_tokens처럼 숫자를 담는 키는 이름이 걸려도 값이 문자열이 아니면 호출하는 쪽에서 그대로 둔다.
func SecretKey(name string) bool { return secretKey.MatchString(name) }

// String은 s에서 비밀값을 지운 결과를 돌려준다.
func String(s string) string {
	s = dsnPassword.ReplaceAllStringFunc(s, func(match string) string {
		parts := dsnPassword.FindStringSubmatch(match)
		if variable.MatchString(parts[2]) {
			return match
		}
		return parts[1] + Placeholder + "@"
	})
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return assignment.ReplaceAllStringFunc(s, func(match string) string {
		parts := assignment.FindStringSubmatch(match)
		value := parts[3]
		quote := ""
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
			quote = value[:1]
			value = value[1 : len(value)-1]
		}
		if variable.MatchString(value) {
			return match
		}
		return parts[1] + quote + Placeholder + quote
	})
}

// JSON masks string values without changing JSON structure, numbers, or symbolic secret references.
func JSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return json.Marshal(maskValue(doc))
}

func maskValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if s, ok := value.(string); ok && SecretKey(k) && !variable.MatchString(s) {
				x[k] = Placeholder
			} else {
				x[k] = maskValue(value)
			}
		}
	case []any:
		for i, value := range x {
			x[i] = maskValue(value)
		}
	case string:
		return String(x)
	}
	return v
}
