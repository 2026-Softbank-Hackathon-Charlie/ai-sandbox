package plan

import (
	"fmt"
	"math"
	"strconv"
	"time"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// SourceStats는 계획 전에 원본 DB를 실측한 값이다 (dbstats.Measure).
//
// 플랫폼은 이전을 시작할 때 원본과 대상을 같은 방식으로 재고, 부족하면 409 capacity_insufficient로 막는다
// (platform internal/preflight/capacity). 그때는 이미 승인된 뒤이므로, 계획 단계에서 같은 기준으로 먼저 막는다.
type SourceStats struct {
	// Sessions는 원본의 클라이언트 세션 수다 (pg_stat_activity의 client backend).
	Sessions int `json:"sessions"`
	// MaxConnections는 원본의 max_connections다.
	MaxConnections int `json:"maxConnections"`
	// Bytes는 원본 DB 전체 크기다. 대상은 pg_basebackup으로 이만큼을 처음 복사한다.
	Bytes int64 `json:"bytes"`
	// WALBytesPerSecond는 원본의 변경(WAL) 생성 속도다.
	WALBytesPerSecond float64 `json:"walBytesPerSecond"`
	// Window는 WAL 속도를 잰 시간이다.
	Window time.Duration `json:"window"`
}

// ConnectionHeadroom은 대상이 원본보다 더 받아야 하는 접속 비율이다. 전환 직후 앱이 다시 붙으면서
// 잠깐 평소보다 많아진다. 플랫폼 용량 검사와 같은 값이다.
const ConnectionHeadroom = 1.2

// MaxCopyBytesPerSecond는 클러스터 사이 복사 속도를 넉넉하게 잡은 상한이다 (제안값, 11/6 확정).
// 이 속도로도 시간 안에 못 끝나는 계획만 막는다. 실제 대역폭은 이보다 느릴 수 있다.
const MaxCopyBytesPerSecond = 100 << 20

// RequiredConnections는 대상의 max_connections가 이 값 이상이어야 한다.
func (s *SourceStats) RequiredConnections() int {
	return int(math.Ceil(float64(s.Sessions) * ConnectionHeadroom))
}

// MinCopyTime은 MaxCopyBytesPerSecond로 원본 전체를 복사하는 데 걸리는 시간이다. 실제로는 이보다 오래 걸린다.
func (s *SourceStats) MinCopyTime() time.Duration {
	return time.Duration(float64(s.Bytes) / MaxCopyBytesPerSecond * float64(time.Second))
}

// CheckCapacity는 실측값으로 접속 수와 초기 복사 시간을 본다. s가 없으면(실측 실패) 보지 않는다.
//   - 대상을 새로 만들면(매니페스트 있음) max_connections를 정해야 하고, 원본 세션 수 × 1.2 이상이어야 한다.
//     플랫폼은 대상이 없을 때 이 매니페스트 값으로 판정하고, 값이 없으면 판정하지 못한 채 시작한다.
//   - 엔진은 prepare에서 catchupTimeout 안에 대상이 원본을 다 복사하고 따라잡기를 기다린다.
//     넉넉한 복사 속도로도 그 안에 못 끝나거나, 원본 변경 속도가 복사 속도보다 빠르면 끝나지 않는다.
func CheckCapacity(p *eplan.Plan, manifest string, s *SourceStats) Issues {
	var is Issues
	if s == nil {
		return is
	}
	if p.Target.Manifest == ManifestRef {
		need := s.RequiredConnections()
		switch n, ok := manifestMaxConnections(manifest); {
		case !ok:
			is.add("manifest.spec.postgresql.parameters.max_connections",
				"원본 세션 %d개를 받으려면 %d 이상으로 정해야 함 (원본 max_connections %d)", s.Sessions, need, s.MaxConnections)
		case n < need:
			is.add("manifest.spec.postgresql.parameters.max_connections",
				"%d이면 부족함. 원본 세션 %d개 × %.1f = %d 이상이어야 함 (원본 max_connections %d)", n, s.Sessions, ConnectionHeadroom, need, s.MaxConnections)
		}
	}
	if s.WALBytesPerSecond >= MaxCopyBytesPerSecond {
		is.add("plan.thresholds.catchupTimeout", "원본 변경 속도 %s/s가 복사 속도 상한 %s/s 이상이라 따라잡을 수 없음",
			humanBytes(int64(s.WALBytesPerSecond)), humanBytes(MaxCopyBytesPerSecond))
	} else if min, got := s.MinCopyTime(), p.Thresholds.CatchupTimeout.Duration; got < min {
		is.add("plan.thresholds.catchupTimeout", "%s이면 짧음. 원본 %s를 %s/s로 복사해도 최소 %s 걸림",
			got, humanBytes(s.Bytes), humanBytes(MaxCopyBytesPerSecond), min.Round(time.Second))
	}
	return is
}

// manifestMaxConnections는 spec.postgresql.parameters.max_connections를 읽는다. 플랫폼과 같이 기본값을 가정하지 않는다.
func manifestMaxConnections(manifest string) (int, bool) {
	doc, err := parseYAMLObject(manifest)
	if err != nil {
		return 0, false
	}
	spec, _ := doc["spec"].(map[string]any)
	pg, _ := spec["postgresql"].(map[string]any)
	params, _ := pg["parameters"].(map[string]any)
	v, ok := params["max_connections"].(string)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n > 0
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), 0
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[i-1])
}
