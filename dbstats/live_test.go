package dbstats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestMeasureLive는 실제 PostgreSQL에서 실측 쿼리를 돌린다. DBSTATS_TEST_DSN이 없으면 건너뛴다.
// 테스트용 DB에 표를 만들고 쓰므로 운영 DB를 넣지 않는다.
//
//	docker run --rm -d -p 55432:5432 -e POSTGRES_PASSWORD=test postgres:17
//	DBSTATS_TEST_DSN='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable' go test -run Live -v ./dbstats/
func TestMeasureLive(t *testing.T) {
	dsn := os.Getenv("DBSTATS_TEST_DSN")
	if dsn == "" {
		t.Skip("DBSTATS_TEST_DSN이 없음")
	}
	ctx := context.Background()

	// 세션 하나를 열어 두고, 재는 동안 쓰기를 만들어 WAL이 늘게 한다.
	other, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, "CREATE TABLE IF NOT EXISTS dbstats_live (id bigserial PRIMARY KEY, v text)"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = other.Exec(ctx, "INSERT INTO dbstats_live (v) SELECT repeat('x', 1000) FROM generate_series(1, 20000)")
	}()

	s, err := Measure(ctx, dsn, time.Second)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", *s)
	switch {
	case s.Sessions < 1:
		t.Errorf("열어 둔 세션을 세야 함 (자기 연결은 뺌): %d", s.Sessions)
	case s.MaxConnections <= 0 || s.MaxWorkerProcesses <= 0:
		t.Errorf("설정을 읽어야 함: %+v", *s)
	case s.Bytes <= 0:
		t.Errorf("DB 크기를 읽어야 함: %d", s.Bytes)
	case s.WindowSeconds < 1:
		t.Errorf("window만큼 재야 함: %v", s.WindowSeconds)
	case s.WALBytesPerSecond <= 0:
		t.Errorf("쓰는 동안의 WAL 속도가 0보다 커야 함: %v", s.WALBytesPerSecond)
	}
}

// TestMeasureLiveAuth는 실제 서버가 비밀번호를 거절할 때 ErrAuth로 구분하는지 본다.
func TestMeasureLiveAuth(t *testing.T) {
	dsn := os.Getenv("DBSTATS_TEST_DSN")
	if dsn == "" {
		t.Skip("DBSTATS_TEST_DSN이 없음")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WRONG_PGPASSWORD", "wrong-password")
	bad := fmt.Sprintf("postgres://%s:${WRONG_PGPASSWORD}@%s:%d/%s?sslmode=disable", cfg.User, cfg.Host, cfg.Port, cfg.Database)
	_, err = Measure(context.Background(), bad, time.Second)
	if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "wrong-password") {
		t.Fatalf("인증 거절은 ErrAuth로, 비밀번호 없이: %v", err)
	}
}
