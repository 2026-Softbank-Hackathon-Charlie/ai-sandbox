package plan

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	eplan "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// SourceStats는 계획 전에 원본 DB를 실측한 값이다. 플랫폼이 초안과 함께 잰다 (POST /v1/plans/draft의 sourceStats, platform ADR 0017).
//
// 플랫폼은 이전을 시작할 때 원본과 대상을 같은 방식으로 재고, 부족하면 409 capacity_insufficient로 막는다
// (platform internal/preflight/capacity). 그때는 이미 승인된 뒤이므로, 계획 단계에서 같은 기준으로 먼저 막는다.
type SourceStats struct {
	// Sessions는 원본의 클라이언트 세션 수다 (pg_stat_activity의 client backend).
	Sessions int `json:"sessions"`
	// MaxConnections, MaxWorkerProcesses는 원본 설정이다. 대상은 원본을 따라가는 hot standby라서
	// PostgreSQL이 이 두 값을 원본 이상으로 요구한다. 작으면 대상이 뜨지 않는다.
	MaxConnections     int `json:"maxConnections"`
	MaxWorkerProcesses int `json:"maxWorkerProcesses"`
	// Bytes는 원본 DB 전체 크기다. 대상은 pg_basebackup으로 이만큼을 처음 복사한다.
	Bytes int64 `json:"bytes"`
	// WALBytesPerSecond는 원본의 변경(WAL) 생성 속도다.
	WALBytesPerSecond float64 `json:"walBytesPerSecond"`
	// WindowSeconds는 WAL 속도를 잰 시간(초)이다.
	WindowSeconds float64 `json:"windowSeconds"`
}

// ConnectionHeadroom은 대상이 원본보다 더 받아야 하는 접속 비율이다. 전환 직후 앱이 다시 붙으면서
// 잠깐 평소보다 많아진다. 플랫폼 용량 검사와 같은 값이다.
const ConnectionHeadroom = 1.2

// MaxCopyBytesPerSecond는 클러스터 사이 복사 속도를 넉넉하게 잡은 상한이다 (제안값, 11/6 확정).
// 이 속도로도 시간 안에 못 끝나는 계획만 막는다. 실제 대역폭은 이보다 느릴 수 있다.
const MaxCopyBytesPerSecond = 100 << 20

// cnpgDefaultMaxWorkerProcesses는 매니페스트에 없을 때 CNPG가 쓰는 max_worker_processes다.
const cnpgDefaultMaxWorkerProcesses = 32

// RequiredConnections는 대상 max_connections의 최솟값이다. 원본 max_connections 이상(hot standby 조건)이고
// 원본 세션 수 × 1.2 이상(플랫폼 용량 검사)이어야 한다. 원본 설정을 기준으로 하므로 계획한 뒤 세션이
// 늘어도 시작할 때 막히지 않는다.
func (s *SourceStats) RequiredConnections() int {
	return max(s.MaxConnections, int(math.Ceil(float64(s.Sessions)*ConnectionHeadroom)))
}

// MinCopyTime은 MaxCopyBytesPerSecond로 원본 전체를 복사하는 데 걸리는 시간이다. 실제로는 이보다 오래 걸린다.
func (s *SourceStats) MinCopyTime() time.Duration {
	return time.Duration(float64(s.Bytes) / MaxCopyBytesPerSecond * float64(time.Second))
}

// CheckFeasible은 AI에 묻기 전에, 어떤 Plan으로도 통과할 수 없는 원본인지 본다.
// 변경 속도가 복사 속도 이상이거나, 초기 복사가 catchupTimeout 상한보다 오래 걸리면 상한값 정책을 바꾸기 전에는 계획할 수 없다.
func CheckFeasible(s *SourceStats) error {
	if s == nil {
		return nil
	}
	if s.WALBytesPerSecond >= MaxCopyBytesPerSecond {
		return fmt.Errorf("원본 변경 속도 %s/s가 복사 속도 상한 %s/s 이상이라 따라잡을 수 없음",
			humanBytes(int64(s.WALBytesPerSecond)), humanBytes(MaxCopyBytesPerSecond))
	}
	if copyTime, limit := s.MinCopyTime(), catchupTimeoutRange[1]; copyTime > limit {
		return fmt.Errorf("원본 %s는 %s/s로 복사해도 최소 %s 걸려 catchupTimeout 상한 %s 안에 끝나지 않음 (상한값은 제안값, 11/6 확정)",
			humanBytes(s.Bytes), humanBytes(MaxCopyBytesPerSecond), copyTime.Round(time.Second), limit)
	}
	return nil
}

// CheckCapacity는 실측값으로 대상 설정과 초기 복사 시간을 본다. s가 없으면(실측 실패) 보지 않는다.
//   - 대상을 새로 만들면(매니페스트 있음) max_connections를 정해야 하고 RequiredConnections 이상이어야 한다.
//     max_worker_processes도 정해야 하고 원본 이상이어야 한다.
//   - 엔진은 prepare에서 catchupTimeout 안에 대상이 원본을 다 복사하고 따라잡기를 기다린다.
//     넉넉한 복사 속도로도 그 안에 못 끝나면 끝나지 않는다.
func CheckCapacity(p *eplan.Plan, manifest string, s *SourceStats) Issues {
	var is Issues
	if s == nil {
		return is
	}
	if p.Target.Manifest == ManifestRef {
		const path = "manifest.spec.postgresql.parameters."
		params := manifestParams(manifest)
		need := s.RequiredConnections()
		// 값이 있는데 정수 문자열이 아니면 형식 오류만 알린다. "정해야 함"까지 같이 나가면 AI가 이미 적은 값을 또 적는다.
		switch n, set, err := paramInt(params, "max_connections"); {
		case errors.Is(err, errNotString): // CheckManifest가 알린다
		case err != nil:
			is.add(path+"max_connections", "%s", err)
		case !set:
			is.add(path+"max_connections", "%d 이상으로 정해야 함 (원본 max_connections %d, 세션 %d × %.1f)",
				need, s.MaxConnections, s.Sessions, ConnectionHeadroom)
		case n < need:
			is.add(path+"max_connections", "%d이면 부족함. %d 이상이어야 함 (대상은 hot standby라 원본 max_connections %d 이상, 플랫폼 기준 세션 %d × %.1f 이상)",
				n, need, s.MaxConnections, s.Sessions, ConnectionHeadroom)
		}
		// 플랫폼 시작 검사(standby_settings)는 대상이 없으면 매니페스트에서 이 값을 읽고, 없으면 짐작하지 않고 건너뛴다.
		// 그래서 기본값에 기대지 않고 적게 한다 (platform #47).
		switch n, set, err := paramInt(params, "max_worker_processes"); {
		case errors.Is(err, errNotString): // CheckManifest가 알린다
		case err != nil:
			is.add(path+"max_worker_processes", "%s", err)
		case !set:
			is.add(path+"max_worker_processes", "원본 값 %d 이상으로 정해야 함 (플랫폼 시작 검사가 매니페스트에서 이 값을 읽음. CNPG 기본값은 %d)",
				s.MaxWorkerProcesses, cnpgDefaultMaxWorkerProcesses)
		case n < s.MaxWorkerProcesses:
			is.add(path+"max_worker_processes", "%d이면 부족함. 대상은 hot standby라 원본 값 %d 이상이어야 함",
				n, s.MaxWorkerProcesses)
		}
	}
	// 어떤 값으로도 통과할 수 없으면 그 이유 하나만 알린다. "짧음"이 같이 나가면 catchupTimeout을 늘리면 될 것처럼 읽힌다.
	if err := CheckFeasible(s); err != nil {
		is.add("plan.thresholds.catchupTimeout", "%s", err)
	} else if got := p.Thresholds.CatchupTimeout.Duration; got > 0 && got < s.MinCopyTime() { // 0은 CheckRules가 이미 거절한다
		is.add("plan.thresholds.catchupTimeout", "%s이면 짧음. 원본 %s를 %s/s로 복사해도 최소 %s 걸림",
			got, humanBytes(s.Bytes), humanBytes(MaxCopyBytesPerSecond), s.MinCopyTime().Round(time.Second))
	}
	return is
}

// errNotString은 설정 값이 문자열이 아닐 때다. CheckManifest가 이미 형식 오류로 알린다.
var errNotString = errors.New("문자열이 아님")

// manifestParams는 spec.postgresql.parameters를 한 번 읽는다. 읽을 수 없으면 비어 있다 (형식 오류는 CheckManifest가 알린다).
func manifestParams(manifest string) map[string]any {
	doc, err := parseYAMLObject(manifest)
	if err != nil {
		return nil
	}
	spec, _ := doc["spec"].(map[string]any)
	pg, _ := spec["postgresql"].(map[string]any)
	params, _ := pg["parameters"].(map[string]any)
	return params
}

// paramInt는 정수 설정을 읽는다. 플랫폼과 같이 기본값을 가정하지 않는다.
// set은 값이 있는지, err는 값이 있는데 양의 정수 문자열이 아닌지다. 문자열이 아니면 errNotString이다 (CheckManifest가 알림).
func paramInt(params map[string]any, name string) (n int, set bool, err error) {
	v, ok := params[name]
	if !ok {
		return 0, false, nil
	}
	s, isStr := v.(string)
	if !isStr {
		return 0, true, errNotString
	}
	n, convErr := strconv.Atoi(s)
	if convErr != nil || n <= 0 {
		return 0, true, fmt.Errorf("양의 정수여야 함 (받은 값 %q)", s)
	}
	return n, true, nil
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
