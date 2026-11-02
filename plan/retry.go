package plan

import (
	"context"
	"errors"
	"fmt"
)

// Ask는 AI를 한 번 부른다. issues는 직전 응답에서 걸린 항목이고, 첫 호출에서는 nil이다.
// 호출하는 쪽은 issues를 프롬프트에 넣어 다시 요청한다.
type Ask func(ctx context.Context, issues Issues) ([]byte, error)

// RetryError는 재요청 상한까지 검사를 통과하지 못했을 때 돌려준다.
type RetryError struct {
	Attempts int
	Last     Issues
}

func (e *RetryError) Error() string {
	return fmt.Sprintf("AI 응답이 %d번 모두 검사에 걸림:\n%s", e.Attempts, e.Last.Error())
}

// Retry는 ask의 응답을 parse로 검사하고, 걸리면 걸린 항목을 넣어 최대 maxRetries번 다시 묻는다.
// 총 호출 수는 maxRetries+1이다. ask가 오류(타임아웃 포함)를 내면 다시 묻지 않고 바로 실패한다.
// 두 번째 반환값은 실제 호출 수다. 호출 기록에 남긴다.
func Retry[T any](ctx context.Context, maxRetries int, ask Ask, parse func([]byte) (T, Issues)) (T, int, error) {
	var zero T
	var last Issues
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, attempt - 1, err
		}
		raw, err := ask(ctx, last)
		if err != nil {
			return zero, attempt, fmt.Errorf("AI 호출 %d번째 실패: %w", attempt, err)
		}
		v, is := parse(raw)
		if len(is) == 0 {
			return v, attempt, nil
		}
		last = is
	}
	return zero, maxRetries + 1, &RetryError{Attempts: maxRetries + 1, Last: last}
}

// IsRetryExhausted는 err가 재요청 상한 초과인지 알려준다.
func IsRetryExhausted(err error) bool {
	var re *RetryError
	return errors.As(err, &re)
}
