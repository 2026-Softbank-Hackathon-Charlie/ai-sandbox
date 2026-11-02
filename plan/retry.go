package plan

import (
	"errors"
	"fmt"
)

// RetryError는 재요청 상한까지 검사를 통과하지 못했을 때 돌려준다.
// 재요청 자체는 대화를 이어 가야 하므로 에이전트(llm)가 한다.
type RetryError struct {
	Attempts int
	Last     Issues
}

func (e *RetryError) Error() string {
	return fmt.Sprintf("AI 응답이 %d번 모두 검사에 걸림:\n%s", e.Attempts, e.Last.Error())
}

// IsRetryExhausted는 err가 재요청 상한 초과인지 알려준다.
func IsRetryExhausted(err error) bool {
	var re *RetryError
	return errors.As(err, &re)
}
