package dbstats

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMeasureHidesConnectionDetails(t *testing.T) {
	t.Setenv("TEST_PGPASSWORD", "s3cret-value")
	for _, dsn := range []string{
		"postgres://app:${TEST_PGPASSWORD}@127.0.0.1:1/app?sslmode=disable&connect_timeout=1",
		"not a dsn ://",
	} {
		s, err := Measure(context.Background(), dsn, 10*time.Millisecond)
		if s != nil || !errors.Is(err, ErrUnreachable) {
			t.Fatalf("%q: 접속 실패여야 함: %v %v", dsn, s, err)
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("오류에 주소나 비밀번호가 들어가면 안 됨: %v", err)
		}
	}
}
