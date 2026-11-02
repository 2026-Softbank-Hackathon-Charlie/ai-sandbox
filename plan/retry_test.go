package plan

import (
	"fmt"
	"strings"
	"testing"
)

func TestRetryError(t *testing.T) {
	err := fmt.Errorf("계획 실패: %w", &RetryError{Attempts: 3, Last: Issues{{"plan.id", "초안과 같아야 함"}}})
	if !IsRetryExhausted(err) {
		t.Fatal("감싼 오류에서도 알아봐야 함")
	}
	if !strings.Contains(err.Error(), "3번 모두") || !strings.Contains(err.Error(), "plan.id") {
		t.Fatalf("마지막 걸린 항목이 메시지에 있어야 함: %v", err)
	}
	if IsRetryExhausted(fmt.Errorf("다른 오류")) {
		t.Fatal("다른 오류는 아님")
	}
}
