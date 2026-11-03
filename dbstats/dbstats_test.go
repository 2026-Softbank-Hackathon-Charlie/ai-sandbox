package dbstats

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMeasureReportsWhyWithoutSecrets(t *testing.T) {
	t.Setenv("TEST_PGPASSWORD", "s3cret-value")
	t.Setenv("UNSET_PGPASSWORD", "")
	for _, c := range []struct {
		dsn  string
		want error
	}{
		{"postgres://app:${UNSET_PGPASSWORD}@db.example.ts.net/app", ErrMissingEnv},
		{"postgres://app:${TEST_PGPASSWORD}@127.0.0.1:1/app?sslmode=disable&connect_timeout=1", ErrUnreachable},
		{"postgres://app:${TEST_PGPASSWORD}@[::1/app", ErrBadDSN},
	} {
		s, err := Measure(context.Background(), c.dsn, 10*time.Millisecond)
		if s != nil || !errors.Is(err, c.want) {
			t.Fatalf("%q: %v여야 함: %v %v", c.dsn, c.want, s, err)
		}
		for _, leak := range []string{"s3cret", "127.0.0.1", "example.ts.net"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("오류에 %q가 들어가면 안 됨: %v", leak, err)
			}
		}
	}
	// 비어 있는 변수 이름은 비밀이 아니므로 알려 준다
	_, err := Measure(context.Background(), "postgres://app:${UNSET_PGPASSWORD}@db/app", 0)
	if !strings.Contains(err.Error(), "UNSET_PGPASSWORD") {
		t.Fatalf("어느 변수가 비었는지 알려야 함: %v", err)
	}
}
