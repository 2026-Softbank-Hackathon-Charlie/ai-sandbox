# ai-sandbox

HopCharlie의 AI(Plan 작성, 리허설 실패 진단, 수정안)와 샌드박스 격리를 맡는 레포입니다. 아직 코드는 없고, 아래 설계 초안부터 올립니다.

# HopCharlie AI·샌드박스 사용 설계 (D 영역 초안)

작성: 태훈 · 2026-11-02 · 근거: HopCharlie 주제 정리의 2장(공통 결론), 3장 '확정된 내용' 열, 4-6·4-7장

## 1. 원칙

- AI가 하는 일은 네 가지다. 리소스 분석, Plan 작성, 리허설 실패 진단, 수정 제안.
- AI에게는 운영 클러스터를 바꾸는 권한이 없다. 운영 변경은 사람이 승인한 Plan을 엔진(결정적 코드)이 실행할 때만 일어난다.
- AI 출력은 항상 JSON이다. 플랫폼이 스키마와 고정 규칙으로 검사하고, 통과한 것만 다음 단계로 넘긴다.
- AI가 쓰는 도구는 허용 목록으로 묶는다. 운영 쪽은 읽기 전용, 샌드박스 쪽은 자기 네임스페이스 하나만 쓸 수 있다.

## 2. 전체 흐름

| # | 단계 | 누가 | 입력 | 출력 | 실패하면 |
|---|---|---|---|---|---|
| 1 | 스캔 | AI (읽기 전용 도구) | 앱 이름, 원본·대상 클러스터 | 리소스 요약 | — |
| 2 | Plan 작성 | AI | 스캔 결과, 대상 환경 프로필 | Plan JSON | — |
| 3 | Plan 검사 | 플랫폼 | Plan JSON | 통과 / 오류 목록 | 오류를 넣어 AI에 다시 요청 (상한 있음) |
| 4 | 리허설 | 엔진, 샌드박스 안 | Plan, 테스트 데이터 | 단계별 결과, 로그 | 5단계로 |
| 5 | 진단·수정안 | AI | 이번 리허설 로그만 (마스킹 후) | 원인, 근거, 수정안 JSON | 수정안 검사 후 4단계 재실행 (상한 있음) |
| 6 | 승인 | 운영자 | Plan, 검사 결과, 리허설 결과, 수정안 diff | 승인 / 반려 | 반려 시 설정 화면으로 |
| 7 | 실행 | 엔진 | 승인된 Plan | 이전 결과, 증거 | 원복 경계 규칙에 따라 원복 또는 정지 (8장) |
| 8 | 결과 설명 | AI | 이번 실행 로그 | 원인 설명 (실행 권한 없음) | — |

콘솔의 이전 화면이 이 순서를 그대로 보여준다 ([frontend PR #2](https://github.com/2026-Softbank-Hackathon-Charlie/frontend/pull/2)).

## 3. Plan JSON

AI는 단계를 새로 만들지 못한다. 단계 이름은 2장의 이전 순서로 고정하고, AI는 각 단계의 값만 채운다. 엔진은 단계 이름마다 미리 짠 구현을 실행한다.

```json
{
  "app": "shop",
  "from": "aws",
  "to": "gcp",
  "profile": "values-gcp.yaml",
  "steps": [
    { "id": "precopy", "db": "initial-copy", "files": "rclone sync" },
    { "id": "sync",    "until": { "dbLagSec": 1, "filesBehind": 0 } },
    { "id": "fence",   "holdWrites": true, "timeoutSec": 30 },
    { "id": "verify",  "rows": "pk-compare", "files": "rclone check" },
    { "id": "cutover", "gateway": { "gcp": 100 } },
    { "id": "cleanup", "source": "scale-to-zero", "delete": "needs-approval" }
  ],
  "rollback": "auto-until-target-accepts-writes"
}
```

- `steps[].id`는 enum이고 순서도 고정이다: precopy → sync → fence → verify → cutover → cleanup.
- `db` 값은 DB 이전 방식(3장 2번)이 정해지면 enum으로 바꾼다. 지금은 비워 둔다.

### 고정 검사 (스키마 다음에 플랫폼이 확인)

- 원본과 대상이 다르다.
- 쓰기 주체는 항상 한 곳이다. 양쪽 동시 쓰기 설정이 없어야 한다.
- fence 단계에 `timeoutSec`가 있고 30 이하다.
- cleanup의 원본 삭제는 `needs-approval`이다.
- 수정안이 고치는 values 키가 허용 목록 안에 있다: `storageClass`, `resources`, `replicas`, 엔드포인트 환경 변수.
- 정책이 늘면 OPA로 옮긴다 (선택 사항).

## 4. AI 호출

Claude API, Go SDK(`anthropic-sdk-go`)의 Tool Runner를 쓴다 (4-6장 확정 기술).

| 호출 | 도구 | 출력 스키마 | 재요청 상한 |
|---|---|---|---|
| plan | 운영 읽기 전용 도구 | Plan | 스키마 오류 2회 |
| diagnose | 없음 (로그만 받음) | `{cause, evidence[], fix}` | 2회 |
| explain | 없음 | `{summary, cause, nextAction}` | 1회 |

- 모델은 별칭이 아닌 고정 버전 ID로 지정한다.
- 호출마다 단계 이름, 입력·출력 토큰, 걸린 시간을 기록한다. 발표에서 AI 비용 대비 효과를 설명할 근거다.
- 모든 호출과 반복에 타임아웃을 둔다. 값은 데모 3분 안에서 정한다.
- 로그는 AI에 보내기 전에 비밀값을 마스킹한다. 해당 실행의 로그만 보낸다.

`fix`의 형식:

```json
{ "kind": "values-patch", "file": "values-gcp.yaml", "patch": [{ "op": "replace", "path": "/cnpg/storageClass", "value": "standard-rwo" }] }
```

`kind`는 `values-patch`와 `plan-param` 두 가지만 허용한다. 수정안은 Plan과 values의 사본에 적용하고, 승인 화면에 diff로 보여준다.

## 5. 도구 허용 목록

Go 함수로 구현하고, 함수 안에서 허용 범위를 다시 검사한다. AI가 임의의 셸 명령을 실행하는 도구는 만들지 않는다.

| 도구 | 범위 | 자격 증명 |
|---|---|---|
| `k8s_get` | 운영 클러스터, Deployment·Service·PVC·StorageClass·Node·CNPG Cluster 조회. Secret 제외 | 읽기 전용 ServiceAccount |
| `db_stats` | 테이블 수, 크기, 기본 키 유무 | 통계 전용 DB 계정 |
| `bucket_stats` | 객체 수, 크기 (`rclone size`) | 읽기 전용 키 |
| `metrics_query` | Prometheus 조회 | 읽기 전용 |
| `sandbox_apply` | 샌드박스 네임스페이스 안에서 Helm 설치·삭제 | 샌드박스 전용 SA, 토큰 1시간 |
| `sandbox_smoke` | 샌드박스 안에서 스모크 테스트, k6 짧게 실행 | 샌드박스 전용 SA |

## 6. 샌드박스

리허설은 **대상 클러스터**에 만든다. 대상 환경의 차이(StorageClass, 노드 자원, 네트워크)를 미리 드러내는 것이 목적이다.

| 항목 | 설정 |
|---|---|
| 네임스페이스 | `sandbox-<app>-<4자리 id>`, 라벨 `hopcharlie/sandbox=true`, 주석 `expires-at` |
| RBAC | 이 네임스페이스에만 바인딩한 Role. TokenRequest로 1시간짜리 토큰 발급 |
| NetworkPolicy | 기본 전부 차단. 같은 네임스페이스와 kube-dns만 허용. 운영망, Tailscale 대역(100.64.0.0/10), 169.254.169.254 차단 |
| ResourceQuota, LimitRange | CPU 4, 메모리 8 GiB, Pod 20개 (데모 값) |
| 데이터 | 임시 PostgreSQL(CNPG)와 임시 MinIO에 테스트 데이터만. 운영 데이터는 넣지 않는다 |
| 외부 호출 | mock 서버로 돌린다 |
| 삭제 | 리허설이 끝나면 바로 지운다. 남은 것은 `expires-at`이 지난 네임스페이스를 지우는 CronJob이 정리한다 |

**NetworkPolicy가 실제로 동작하는지 확인해야 한다.** EKS는 VPC CNI의 네트워크 정책 기능을, GKE는 Dataplane V2를 켜야 한다. k3s는 기본 정책 컨트롤러를 끄지 않았는지 본다. 11/4 A 영역 목표("NetworkPolicy 동작 확인")와 같이 진행한다.

### 격리 검증 (차단 테스트)

리허설마다 고정 스크립트가 AI용 샌드박스 토큰으로 다음 명령을 시도하고, 모두 막히는지 확인한다. AI가 스스로 이 명령을 실행하길 기대하지 않는다. 결과는 콘솔의 '막힌 명령' 표에 나온다.

- `kubectl get secrets -n <운영 네임스페이스>` → RBAC가 막아야 한다
- `curl http://169.254.169.254/` → NetworkPolicy가 막아야 한다
- 운영 DB 주소로 연결 → NetworkPolicy가 막아야 한다

## 7. 리허설 내용

1. 샌드박스에 임시 원본(DB와 버킷)과 임시 대상을 만들고 테스트 데이터를 넣는다.
2. Plan 6단계를 그대로 실행한다. 이때 짧은 k6 부하를 같이 준다.
3. 통과 기준은 세 가지다. 데이터 일치(PK 비교, `rclone check`), 스모크 테스트 통과, 원본 차단 테스트 통과.
4. 실패하면 4장의 diagnose를 호출하고, 수정안이 3장 검사를 통과하면 다시 실행한다. 최대 3회까지 실행한다.

## 8. 실행 중 실패: 원복 경계

엔진이 처리하고, AI는 판단하지 않는다 (2장 복구 경계).

- 대상 DB가 쓰기를 받기 **전**에 실패하면 원본으로 자동 원복한다.
- 대상 DB가 쓰기를 받은 **뒤**에 실패하면 원복하지 않고 멈춘다(split-brain 방지). 그다음 AI explain을 호출한다.
- 콘솔은 `rolled_back`과 `halted`, `failed`를 따로 표시한다.

## 9. 데모 실패 시나리오 (고정)

11/6 목표 "데모 실패 시나리오 고정"에 쓸 안이다. 콘솔에 들어간 시나리오와 같다.

- 원인: values에 원본 클러스터의 StorageClass(예: EKS의 `gp3`)가 남아 있어서, 대상에서 CNPG PVC가 Pending이 된다.
- 고르는 이유: 경로 6개 모두에서 재현된다(gp3, standard-rwo, local-path가 서로 다르다). 원인이 로그에 분명히 남고, 수정이 values 한 줄이다.
- 흐름: 1차 실패 → AI 진단 → values diff 수정안 → 2차 통과 → 승인 화면에 diff 표시.

## 10. 날짜별 (D 영역)

| 날짜 | 목표 |
|---|---|
| 11/2 (월) | 스캔 도구 4개(`k8s_get`, `db_stats`, `bucket_stats`, `metrics_query`), plan 프롬프트 초안 |
| 11/3 (화) | Plan JSON 스키마, 검증과 재요청 루프, 고정 검사 |
| 11/4 (수) | 샌드박스 생성·삭제, RBAC·NetworkPolicy·Quota, 리허설 1회 |
| 11/5 (목) | 실패 → 진단 → 수정 → 재통과 루프, 격리 검증 스크립트 |
| 11/6 (금) | 데모 실패 시나리오 고정, 타임아웃·상한 값 확정 |

## 11. 팀 결정이 필요한 것

- **DB 이전 방식 (3장 2번):** CNPG replica인지 논리 복제인지에 따라 Plan의 `precopy`·`sync` 값과 리허설 구성이 달라진다.
- **트래픽 게이트웨이 (3장 3번):** 확정란이 "그때 AI가 하라는 대로"인데, AI는 실행 중에 인프라를 고르지 않는다. 쓰기 보류가 필요하므로 Go 직접 구현이나 Envoy 중 하나를 팀이 정해야 한다.
- **AI 범위 (3장 7번):** 온보딩(Dockerfile·PR 생성)을 넣을지. 콘솔에서는 일단 뺐다.
- **리허설 범위:** 대상 클러스터 하나에서만 할지, 원본과 대상 양쪽에 샌드박스를 만들어 클러스터 간 복제까지 시험할지. 이 문서는 대상 하나로 가정했다.
- **타임아웃·재요청 상한 값:** 이 문서의 숫자(재요청 2회, 리허설 3회, 쓰기 보류 30초)는 제안값이다.

## 12. 구현 현황

| 패키지 | 내용 | 설계 문서 |
|---|---|---|
| `plan` | Plan JSON 읽기, 스키마·고정 검사, 진단(`{cause, evidence, fix}`)과 수정안 검사, 재요청 루프 | 3장, 4장 |
| `sandbox` | 샌드박스 생성·삭제, AI 토큰 발급(1시간), 만료 정리, 격리 검증(차단 테스트) | 6장 |
| `cmd/sandboxctl` | 샌드박스를 손으로 만들고 검사하고 지우는 도구 | 6장 |

실행:

```bash
go test ./...
```

- `plan.Parse(raw)`는 AI 응답 원문을 받아 통과하면 Plan을, 아니면 걸린 항목 목록(`Issues`)을 돌려준다. 걸린 항목은 한 번에 모두 모아서 돌려준다.
- `plan.ParseDiagnosis(raw, p)`는 진단 응답을 읽고, 수정안이 허용 범위 안인지 본다. `plan-param` 수정안은 Plan 사본에 적용한 뒤 Plan 검사를 다시 통과해야 한다.
- `plan.Retry(ctx, 2, ask, parse)`는 걸린 항목을 `ask`에 넘겨 다시 요청한다. 실제 Claude 호출은 `ask` 함수로 붙인다. 호출 오류(타임아웃 포함)는 다시 묻지 않고 바로 실패한다.
- 테스트에는 형식이 깨진 AI 응답(코드 블록, 중간에 끊김, 뒤에 붙은 설명, 숫자를 문자열로)과 규칙을 어긴 응답 사례가 들어 있다.
- 제안값: 쓰기 보류 상한 30초, 수정안 한 번에 5줄, 엔드포인트 변수 이름 규칙(`_HOST`, `_URL`, `_ENDPOINT`, `_ADDR`, `_PORT`로 끝남). 샘플 앱 변수 이름과 11/6 상한 값이 정해지면 맞춘다.

### 샌드박스 (`sandbox`)

- `Create`는 네임스페이스 다음 바로 NetworkPolicy를 만든다. 다른 리소스가 열린 상태로 뜨지 않게 하기 위해서다. 중간에 실패하면 네임스페이스를 지운다.
- `Delete`는 이름이 `sandbox-`로 시작하고 `hopcharlie/sandbox=true` 라벨이 있을 때만 지운다. 운영 네임스페이스는 거절한다.
- `Token`은 TokenRequest로 1시간짜리 토큰을 발급하고, 샌드박스 만료 시각을 넘기지 않는다.
- `Sweep`은 `hopcharlie/expires-at`이 지난 샌드박스를 지운다. CronJob에서 부를 함수다 (CronJob 매니페스트는 아직 없음).
- AI 계정 Role에는 Role, RoleBinding, NetworkPolicy, ResourceQuota, LimitRange, `serviceaccounts/token`, `pods/exec` 권한이 없다. 격리를 스스로 풀 수 없게 하기 위해서다. Helm이 릴리스 기록을 secret에 남기므로 자기 네임스페이스의 secret 권한은 있다.

격리 검증(`CheckIsolation`)이 확인하는 것:

| 항목 | 명령 (같은 뜻) | 기대 | 방법 |
|---|---|---|---|
| prod-secrets | `kubectl get secrets -n <운영>` | 막힘 | AI 토큰으로 실제 시도 |
| own-netpol-delete | `kubectl delete networkpolicy ...` | 막힘 | 권한 확인 요청 (실제로 지우지 않음) |
| own-role-escalate | `kubectl edit role ...` | 막힘 | 권한 확인 요청 |
| own-token-create | `kubectl create token ...` | 막힘 | 권한 확인 요청 |
| prod-deploy-delete | `kubectl delete deployment -n <운영> --all` | 막힘 | 권한 확인 요청 |
| own-pods-list | `kubectl get pods -n <샌드박스>` | 열림 | 리허설이 가능한지 확인 |
| dns | `nslookup kubernetes.default` | 열림 | 검사 Pod. 막히면 검사 자체를 믿을 수 없음 |
| metadata | `curl http://169.254.169.254/` | 막힘 | 검사 Pod |
| apiserver | `nc kubernetes.default.svc 443` | 막힘 | 검사 Pod. 정책이 없으면 열리는 곳이라 정책 동작의 증거 |
| (추가) | `-target db=호스트:포트` | 막힘 | 검사 Pod. 운영 DB 주소를 넣는다 |

로컬 kind 클러스터에서 확인한 결과 (2026-11-02, kind 0.33, kindnet):

- 정책이 없는 네임스페이스에서는 apiserver와 다른 네임스페이스의 서비스가 **열림** (대조군)
- 샌드박스에서는 10개 항목 모두 기대대로 (`rehearse-check` 종료 코드 0)
- 샌드박스의 NetworkPolicy를 지우면 apiserver와 db가 열림으로 나와 검사가 **실패** (종료 코드 1)

```bash
kind create cluster --name hop-sandbox --kubeconfig ./kind.kubeconfig
go run ./cmd/sandboxctl -kubeconfig ./kind.kubeconfig rehearse-check -app shop -prod-ns shop -target db=<운영 DB 호스트>:5432
```

`-kubeconfig`는 필수다. 현재 컨텍스트(운영 클러스터일 수 있음)를 실수로 쓰지 않도록 기본값을 두지 않았다.

주의:

- CNPG를 샌드박스에 띄우려면 `AllowIngressNamespaces`에 operator 네임스페이스를, `AllowEgressCIDRs`에 API 서버 주소를 넣어야 한다. 이때는 기본 대상의 apiserver 항목을 빼야 한다.
- EKS(VPC CNI 네트워크 정책), GKE(Dataplane V2), k3s에서는 아직 돌려 보지 않았다. 11/4 A 영역 작업과 같이 확인한다.
