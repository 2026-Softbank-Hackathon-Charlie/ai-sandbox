package rehearsal

import (
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

// Diff는 줄 단위 비교 결과다. 지운 줄 앞에 "- ", 더한 줄 앞에 "+ ", 같은 줄 앞에 공백 두 칸을 붙인다.
// 승인 화면에 그대로 보여주므로 비밀값은 가린 뒤 비교한다.
func Diff(before, after string) string {
	a := strings.Split(strings.TrimRight(mask.String(before), "\n"), "\n")
	b := strings.Split(strings.TrimRight(mask.String(after), "\n"), "\n")
	// LCS 표. 매니페스트는 작아서 단순한 방법으로 충분하다.
	l := make([][]int, len(a)+1)
	for i := range l {
		l[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				l[i][j] = l[i+1][j+1] + 1
			} else {
				l[i][j] = max(l[i+1][j], l[i][j+1])
			}
		}
	}
	var sb strings.Builder
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			sb.WriteString("  " + a[i] + "\n")
			i, j = i+1, j+1
		case i < len(a) && (j == len(b) || l[i+1][j] >= l[i][j+1]):
			sb.WriteString("- " + a[i] + "\n")
			i++
		default:
			sb.WriteString("+ " + b[j] + "\n")
			j++
		}
	}
	return sb.String()
}
