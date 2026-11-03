// Package dbstats는 계획 전에 원본 DB를 실측한다. 플랫폼의 시작 전 용량 검사(platform internal/preflight/capacity)와
// 같은 읽기 전용 조회를 쓴다. 값은 숫자만 돌려주고 접속 주소, 비밀번호, 서버 메시지는 내보내지 않는다.
package dbstats

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
)

// 기본값
const (
	DefaultWindow  = 5 * time.Second
	DefaultTimeout = 20 * time.Second
	// appName은 이 측정의 연결 이름이다. 세션 수를 셀 때 자기 연결은 뺀다.
	appName = "hopcharlie-ai-capacity"
)

// ErrUnreachable은 원본에 접속하지 못했을 때다. 원인(주소, 인증 오류)은 비밀이 섞일 수 있어 담지 않는다.
var ErrUnreachable = errors.New("원본 DB에 접속하지 못함")

// ErrIncomplete는 접속했지만 일부 값을 읽지 못했을 때다.
var ErrIncomplete = errors.New("원본 DB 값을 다 읽지 못함")

// Measure는 dsn의 DB를 읽기 전용으로 재고 window 동안의 WAL 생성 속도를 구한다.
// dsn의 ${VAR}는 이 프로세스의 환경 변수로 채운다 (엔진, 플랫폼과 같음).
func Measure(ctx context.Context, dsn string, window time.Duration) (*plan.SourceStats, error) {
	if window <= 0 {
		window = DefaultWindow
	}
	ctx, cancel := context.WithTimeout(ctx, window+DefaultTimeout)
	defer cancel()

	cfg, err := pgx.ParseConfig(os.ExpandEnv(dsn))
	if err != nil {
		return nil, ErrUnreachable
	}
	if cfg.ConnectTimeout == 0 || cfg.ConnectTimeout > 5*time.Second {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["statement_timeout"] = "5000"
	cfg.RuntimeParams["application_name"] = appName
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, ErrUnreachable
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = conn.Close(cctx)
	}()

	s := &plan.SourceStats{}
	if conn.QueryRow(ctx, `SELECT coalesce(sum(pg_database_size(datname)), 0)::bigint
		FROM pg_catalog.pg_database WHERE datallowconn`).Scan(&s.Bytes) != nil {
		return nil, ErrIncomplete
	}
	if conn.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE backend_type = 'client backend' AND application_name <> $1`, appName).Scan(&s.Sessions) != nil {
		return nil, ErrIncomplete
	}
	var maxConn string
	if conn.QueryRow(ctx, "SELECT current_setting('max_connections')").Scan(&maxConn) != nil {
		return nil, ErrIncomplete
	}
	if s.MaxConnections, err = strconv.Atoi(maxConn); err != nil {
		return nil, ErrIncomplete
	}

	var lsn0, lsn1 string
	if conn.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn0) != nil {
		return nil, ErrIncomplete
	}
	start := time.Now()
	select {
	case <-ctx.Done():
		return nil, ErrIncomplete
	case <-time.After(window):
	}
	if conn.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn1) != nil {
		return nil, ErrIncomplete
	}
	s.Window = time.Since(start)
	var diff int64
	if conn.QueryRow(ctx, "SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)::bigint", lsn1, lsn0).Scan(&diff) != nil || diff < 0 {
		return nil, ErrIncomplete
	}
	s.WALBytesPerSecond = float64(diff) / s.Window.Seconds()
	return s, nil
}
