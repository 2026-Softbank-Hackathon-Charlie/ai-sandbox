package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryableConnectErr(t *testing.T) {
	pgErr := func(code string) error { return &pgconn.PgError{Code: code} }
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"healthy 직후 비밀번호 인증 실패(28P01)", pgErr("28P01"), true},
		{"서버가 아직 연결을 받지 못함(57P03)", pgErr("57P03"), true},
		{"접속 수 초과(53300)", pgErr("53300"), true},
		{"DB 없음(3D000)은 영구 오류", pgErr("3D000"), false},
		{"권한 없음(42501)은 영구 오류", pgErr("42501"), false},
		{"프록시 연결 거부", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"연결이 끊김", io.EOF, true},
		{"연결이 도중에 끊김", io.ErrUnexpectedEOF, true},
		{"감싼 오류도 같은 판정", fmt.Errorf("connect: %w", pgErr("28P01")), true},
		{"잘못된 DSN은 영구 오류", errors.New("cannot parse `postgres://...`: invalid port"), false},
		{"오류 없음", nil, false},
	} {
		if got := retryableConnectErr(c.err); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// 영구 오류는 60초를 기다리지 않고 바로 돌려준다.
func TestConnectRetryReturnsPermanentErrorAtOnce(t *testing.T) {
	start := time.Now()
	_, err := connectRetry(context.Background(), "postgres://app@127.0.0.1:1/app?connect_timeout=1&sslmode=bogus", time.Minute)
	if err == nil {
		t.Fatal("잘못된 DSN은 오류여야 함")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("영구 오류인데 %s나 기다림", d)
	}
}
