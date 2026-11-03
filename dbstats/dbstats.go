// Package dbstats는 계획 전에 원본 DB를 실측한다. 플랫폼의 시작 전 용량 검사(platform internal/preflight/capacity)와
// 같은 읽기 전용 조회를 쓴다. 값은 숫자만 돌려주고 접속 주소, 비밀번호, 서버 메시지는 내보내지 않는다.
package dbstats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/plan"
)

// 기본값
const (
	DefaultWindow  = 5 * time.Second
	DefaultTimeout = 20 * time.Second
	// appName은 이 측정의 연결 이름이다. 세션 수를 셀 때 자기 연결은 뺀다.
	appName = "hopcharlie-ai-capacity"
)

// 실측 실패 이유. 주소, 비밀번호, 서버 메시지는 담지 않는다. 환경 변수 이름은 비밀이 아니라서 담는다.
var (
	// ErrMissingEnv는 DSN의 ${VAR}가 이 프로세스에 없을 때다.
	ErrMissingEnv = errors.New("원본 DSN의 비밀번호 환경 변수가 비어 있음")
	// ErrBadDSN은 DSN을 읽을 수 없을 때다.
	ErrBadDSN = errors.New("원본 DSN 형식이 잘못됨")
	// ErrAuth는 접속했지만 인증이 거절됐을 때다 (SQLSTATE 28xxx).
	ErrAuth = errors.New("원본 DB가 인증을 거절함 (비밀번호 확인)")
	// ErrUnreachable은 원본에 닿지 못했을 때다. DB 주소는 *.ts.net이라 이 실행기가 tailnet에 붙어 있어야 한다.
	ErrUnreachable = errors.New("원본 DB에 닿지 못함 (tailnet 연결 확인)")
	// ErrIncomplete는 접속했지만 일부 값을 읽지 못했을 때다.
	ErrIncomplete = errors.New("원본 DB 값을 다 읽지 못함")
)

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Measure는 dsn의 DB를 읽기 전용으로 재고 window 동안의 WAL 생성 속도를 구한다.
// dsn의 ${VAR}는 이 프로세스의 환경 변수로 채운다 (엔진, 플랫폼과 같음).
func Measure(ctx context.Context, dsn string, window time.Duration) (*plan.SourceStats, error) {
	if window <= 0 {
		window = DefaultWindow
	}
	ctx, cancel := context.WithTimeout(ctx, window+DefaultTimeout)
	defer cancel()

	for _, m := range envRef.FindAllStringSubmatch(dsn, -1) {
		if os.Getenv(m[1]) == "" {
			return nil, fmt.Errorf("%w: %s", ErrMissingEnv, m[1])
		}
	}
	cfg, err := pgx.ParseConfig(os.ExpandEnv(dsn))
	if err != nil {
		return nil, ErrBadDSN
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
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "28") {
			return nil, ErrAuth
		}
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
	// 대상(hot standby)이 원본 이상이어야 하는 설정
	for name, dst := range map[string]*int{"max_connections": &s.MaxConnections, "max_worker_processes": &s.MaxWorkerProcesses} {
		var v string
		if conn.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&v) != nil {
			return nil, ErrIncomplete
		}
		if *dst, err = strconv.Atoi(v); err != nil {
			return nil, ErrIncomplete
		}
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
	s.WindowSeconds = time.Since(start).Seconds()
	var diff int64
	if conn.QueryRow(ctx, "SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)::bigint", lsn1, lsn0).Scan(&diff) != nil || diff < 0 {
		return nil, ErrIncomplete
	}
	s.WALBytesPerSecond = float64(diff) / s.WindowSeconds
	return s, nil
}
