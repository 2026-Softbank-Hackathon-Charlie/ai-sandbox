# ai-sandbox

HopCharlie의 AI(Plan 작성, 리허설 실패 진단, 수정안)와 샌드박스 격리를 맡는 레포입니다. 아직 코드는 없고, 아래 설계 초안부터 올립니다.

# HopCharlie AI·샌드박스 사용 설계 (D 영역 초안)

작성: 태훈 · 2026-11-02 · 근거: HopCharlie 주제 정리의 2장(공통 결론), 3장 '확정된 내용' 열, 4-6·4-7장

Plan 계약 갱신: 2026-11-02 · [platform OpenAPI](https://github.com/2026-Softbank-Hackathon-Charlie/platform/blob/e802f54467f7b921d7583bdca985537688c20a3f/api/openapi.yaml)의 `EnginePlan` 기준. AI·샌드박스 기능은 설계 초안이며, 아래 플랫폼 API 계약과 구분한다.

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

AI가 플랫폼에 제출하는 본문은 **`EnginePlan` 자체**다. 필수 필드는 `id`, `source`, `target`이고, 두 endpoint에는 각각 `env`, `namespace`, `cluster`, `dsn`이 필요하다. `cluster`는 Kubernetes 클러스터 이름이 아니라 **CNPG Cluster 리소스 이름**이며, Kubernetes 접속 대상은 `context`로 고른다.

기존 초안의 최상위 `app`, `from`, `to`, `profile`, `steps`, `rollback`은 이 계약에 없다. 알 수 없는 필드는 거부된다. 공통 계약은 [OpenAPI의 EnginePlan·EnginePlanEndpoint](https://github.com/2026-Softbank-Hackathon-Charlie/platform/blob/e802f54467f7b921d7583bdca985537688c20a3f/api/openapi.yaml#L973)와 플랫폼이 고정한 [엔진 Plan 파서](https://github.com/2026-Softbank-Hackathon-Charlie/migration-engine/blob/f953d2612ffb/pkg/plan/plan.go)를 따른다.

### 초안 생성 요청과 제출 본문

앱 설정에서 출발할 때는 agent 토큰으로 `POST /v1/plans/draft`에 **`PlanDraftRequest`**를 보낸다. 이 요청의 `source`, `target`은 환경 이름 문자열이다.

```json
{
  "app": "moimroom",
  "source": "gke",
  "target": "eks",
  "writtenRecordsStream": "moimroom-gke-to-eks-demo-01"
}
```

응답은 저장되지 않은 `EnginePlan`이다. AI는 이 응답을 검토·수정하고 `POST /v1/plans`에 그대로 제출한다. **제출 본문의 `source`, `target`은 endpoint 객체**다. 아래는 [앱 설정 예제](https://github.com/2026-Softbank-Hackathon-Charlie/platform/blob/e802f54467f7b921d7583bdca985537688c20a3f/examples/apps.yaml)에 매니페스트 첨부와 임계값을 더한 구조 예제다. 주소·context·버킷·비밀번호 변수는 실제 서버 설정과 맞춰야 하며, 게이트웨이 URL은 draft 응답의 값을 사용한다.

```json
{
  "id": "moimroom-gke-to-eks",
  "source": {
    "env": "gke",
    "context": "gke-hopcharlie",
    "namespace": "moimroom",
    "cluster": "moimroom-db-gke",
    "dsn": "postgres://app:${GKE_MOIMROOM_PGPASSWORD}@moimroom-db-gke-rw.moimroom.gke.ts.net:5432/app?sslmode=require"
  },
  "target": {
    "env": "eks",
    "context": "eks-hopcharlie",
    "namespace": "moimroom",
    "cluster": "moimroom-db-eks",
    "dsn": "postgres://app:${EKS_MOIMROOM_PGPASSWORD}@moimroom-db-eks-rw.moimroom.eks.ts.net:5432/app?sslmode=require",
    "manifest": "attachment:target-cnpg.yaml"
  },
  "files": {
    "source": "gcs:moimroom-photos-gke",
    "target": "s3:moimroom-photos-eks"
  },
  "gateway": {
    "mode": "http",
    "url": "http://gateway:9901",
    "token": "${GATEWAY_TOKEN}",
    "sourceBackend": "gke",
    "targetBackend": "eks"
  },
  "verify": {
    "schemas": ["app"],
    "fenceGate": true,
    "writtenRecords": "stream:moimroom-gke-to-eks-demo-01"
  },
  "thresholds": {
    "maxLagBytes": 16777216,
    "catchupTimeout": "10m",
    "switchoverTimeout": "2m",
    "maxDowntime": "3m"
  }
}
```

- `files`, `gateway`, `verify`, `thresholds`는 선택 필드다. `files`를 쓰면 `source`, `target` 둘 다 필요하며, 값은 runner에 설정한 rclone remote 경로다. 접속 키는 Plan에 넣지 않는다.
- `gateway.mode: "none"`은 쓰기 보류·트래픽 전환을 수행하지 않는다. 운영 이전에서는 설정된 HTTP 게이트웨이를 사용한다.
- timeout은 `"10m"`, `"2m"` 같은 duration 문자열이다. `maxDowntime`은 전환 후 실측값과 비교하는 예산이며, 초과만으로 자동 중단·원복하지 않는다. catchup 대기시간과 이전 다운타임은 다른 지표다.

### 실행 순서와 플랫폼 연동

단계 목록은 Plan에 넣지 않는다. [엔진의 고정 순서](https://github.com/2026-Softbank-Hackathon-Charlie/migration-engine/blob/f953d2612ffb/pkg/engine/engine.go)는 다음 9단계다.

```text
precheck → prepare → presync → delta → fence → switchover → cutover → verify → finalize
```

플랫폼은 `RunStep`으로 단계를 호출하며, `delta`와 `fence` 사이에 읽기 카나리와 전환 승인을 넣는다. AI는 실행 단계나 순서를 추가하지 않는다.

1. agent 토큰으로 `POST /v1/plans`에 `EnginePlan`을 제출하면 `pending_approval` 상태의 `PlanRecord`를 받는다. 이후 API에는 Plan의 `id` 대신 응답의 `ref`를 사용한다.
2. `target.manifest`를 쓸 때는 승인 전에 `PUT /v1/plans/{ref}/attachments/target-cnpg.yaml`로 파일을 올린다. 첨부가 바뀔 때마다 `PlanRecord.revision`이 증가한다.
3. `verify.writtenRecords`의 스트림에는 부하 생성기가 `POST /v1/written-records/{stream}`으로 성공한 쓰기를 보낸다. 실행마다 새 스트림 이름을 쓰고, `verify` 직전에 플랫폼이 기록을 엔진에 전달한다.
4. operator가 검토한 `revision`으로 `POST /v1/plans/{ref}/approve`를 호출하고, 승인된 `ref`를 `POST /v1/migrations`의 `planRef`로 넘겨 실행한다. 첨부가 있는 Plan은 revision 없이 승인할 수 없고, 그사이 첨부가 바뀌면 409다. agent는 승인·운영 실행을 할 수 없다.
5. AI의 계획·리허설·진단 활동은 기존 `POST /v1/agent/sessions`, `.../{id}/events`, `.../{id}/finish`로 기록한다. 비밀번호·토큰은 이벤트에도 넣지 않는다.

### 검증: 현재 계약과 D 영역 추가 검사

현재 플랫폼·엔진은 JSON 계약 외에 다음을 검사한다. 자세한 제한은 [플랫폼 Plan 정책](https://github.com/2026-Softbank-Hackathon-Charlie/platform/blob/e802f54467f7b921d7583bdca985537688c20a3f/internal/plans/policy.go)을 따른다.

- `id`는 1~63자의 소문자·숫자·하이픈이며, 시작과 끝은 소문자 또는 숫자다. `source.env`와 `target.env`는 달라야 하고 `source.manifest`는 허용하지 않는다.
- 제출 Plan의 `kubeconfig` 경로는 허용하지 않는다. 서버의 kubeconfig와 등록된 context를 사용한다.
- DSN은 `postgres://` 또는 `postgresql://` URL이다. `${VAR}`는 비밀번호 자리 전체에서만 쓰며, 서버가 허용한 변수·DB 호스트와 포트를 따라야 한다. 앱 비밀번호 변수는 `_PGPASSWORD`로 끝나고 `PLATFORM_`·`AWS_`로 시작하지 않는다.
- `gateway.token`은 비어 있거나 `${GATEWAY_TOKEN}`이고, HTTP URL은 서버에 설정된 게이트웨이여야 한다. 그 밖의 필드에는 `$`를 쓰지 않는다.
- 매니페스트와 쓰기 기록은 각각 `attachment:<name>`, `stream:<name>`을 권장한다. `files.rclone`을 명시하면 `rclone`만 가능하고 flags는 서버 허용 목록을 따른다.

D 영역에서 구현할 추가 검사는 별도다. 수정 가능한 values 키를 `storageClass`, `resources`, `replicas`, 승인된 엔드포인트 환경 변수로 제한하고, 리허설 Plan의 DB·버킷·게이트웨이가 모두 테스트 대상을 가리키는지 확인한다. 원본 삭제나 원복 정책을 새 Plan 필드로 추가하지 않는다. 정책이 늘면 OPA 도입을 검토한다.

## 4. AI 호출

Claude API, Go SDK(`anthropic-sdk-go`)의 Tool Runner를 쓴다 (4-6장 확정 기술).

| 호출 | 도구 | 출력 스키마 | 재요청 상한 |
|---|---|---|---|
| plan | 운영 읽기 전용 도구 | `EnginePlan` | 스키마 오류 2회 |
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

`kind`는 `values-patch`와 `plan-param` 두 가지만 허용한다. 이 JSON은 AI 워커 내부의 수정 제안 형식이며 플랫폼 API의 제출 본문이 아니다. 수정안은 EnginePlan과 values의 사본에 적용하고, 승인 화면에 diff로 보여준다. 수정한 매니페스트는 첨부로 다시 올리고 새 revision으로 검토·승인한다. EnginePlan 본문을 바꾸면 새 Plan으로 제출한다.

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
2. 같은 EnginePlan 형식으로 엔진의 고정 9단계를 실행한다. 원본·대상의 namespace, context, CNPG Cluster, DSN과 파일·게이트웨이 설정을 모두 샌드박스용으로 바꾸고, 서로 다른 `env` 식별자를 쓴다. 운영용 접속 정보와 토큰은 재사용하지 않는다. 이때 짧은 k6 부하를 같이 준다.
3. 통과 기준은 세 가지다. 데이터 일치(PK 비교, `rclone check`), 스모크 테스트 통과, 원본 차단 테스트 통과.
4. 실패하면 4장의 diagnose를 호출하고, 수정안이 3장 검사를 통과하면 다시 실행한다. 최대 3회까지 실행한다.

리허설 실행기와 샌드박스 발급 API는 아직 구현 전이다. 운영용 `/v1/plans`의 호스트·비밀번호 정책을 우회하지 않고, 테스트 환경용 접속 설정과 제한된 권한을 연결해야 한다. 대상 클러스터 하나에서의 리허설만으로 실제 클러스터 간 연결·복제가 검증되는 것은 아니다.

## 8. 실행 중 실패: 원복 경계

엔진이 처리하고, AI는 판단하지 않는다 (2장 복구 경계).

- 쓰기 보류 또는 원본 demote 이후이면서 대상 DB **승격 요청 전**의 실패는 자동 원복한다. 그 이전 실패는 `failed`로 종료한다. 승격 요청 이후에는 결과가 불확실할 수 있어 자동 원복하지 않고 `halted`로 멈춘다(split-brain 방지). 그다음 AI explain을 호출한다.
- 이 경계는 엔진이 처리하며 Plan에 `rollback` 필드를 넣지 않는다. 성공한 이전을 되돌리는 operator 전용 `POST /v1/migrations/{runId}/rollback`은 별도의 역방향 이전이다.
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

- **DB 지원 범위:** 현재 엔진은 CNPG replica·demote·promote를 사용한다. 다른 DB 이전 방식을 지원하려면 엔진 변경과 별도 합의가 필요하다. AI가 Plan에 이전 방식 enum을 새로 만들지 않는다.
- **트래픽 게이트웨이 연결:** 현재 플랫폼의 Go 게이트웨이 계약을 사용한다. 각 환경 backend 주소와 샌드박스용 게이트웨이를 인계받아야 한다.
- **AI 범위 (3장 7번):** 온보딩(Dockerfile·PR 생성)을 넣을지. 콘솔에서는 일단 뺐다.
- **리허설 범위:** 대상 클러스터 하나에서만 할지, 원본과 대상 양쪽에 샌드박스를 만들어 클러스터 간 복제까지 시험할지. 이 문서는 대상 하나로 가정했다.
- **타임아웃·재요청 상한 값:** 재요청 2회와 리허설 3회는 제안값이다. 이전 대기는 EnginePlan의 `thresholds`를 쓰고, 게이트웨이의 쓰기 보류 설정은 별도로 관리한다. `steps[].timeoutSec` 같은 필드는 추가하지 않는다.
