# ai-sandbox

HopCharlie의 AI(Plan 작성, 리허설 실패 진단, 수정안)와 샌드박스 격리를 맡는 레포입니다. 구현 현황과 실행 방법은 12장에 있습니다.

## 버전 히스토리

머지된 PR마다 무엇이 생기고 무엇으로 확인했는지는 [docs/HISTORY.md](docs/HISTORY.md)에 있습니다. 머지 시각은 한국 시간입니다.

| 버전 | PR | 머지 | 내용 |
| --- | --- | --- | --- |
| v0 | 직접 커밋 | 11/2 10:06 | 설계 초안 (코드 없음) |
| v1 | [#3](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/3) | 11/2 16:27 | 계획 예시를 플랫폼 형식에 맞춤 (코드 변화 없음) |
| v2 | [#4](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/4) | 11/2 18:39 | AI가 만든 이전 계획을 규칙으로 검사 |
| v3 | [#5](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/5) | 11/2 18:44 | AI 전용 격리 공간(샌드박스)과 격리 검사 |
| v4 | [#6](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/6) | 11/2 18:49 | 직접 조사해 계획을 만드는 AI, 활동 기록 |
| v5 | [#7](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/7) | 11/2 18:52 | 승인 전에 미리 돌려 보는 리허설 |
| v6 | [#8](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/8) | 11/2 18:54 | 구현 내용을 설계 문서에 반영 (코드 변화 없음) |
| v7 | [#9](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/9) | 11/2 19:05 | `maxDowntime` 뜻 풀어 쓰기 (코드 변화 없음) |
| v8 | [#10](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/10) | 11/2 19:53 | 보안 구멍 3개 막기 |
| v9 | [#14](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/14) | 11/2 20:32 | AI 계획과 데이터 검증의 빈틈 막기 |
| v10 | [#15](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/15) | 11/2 20:40 | 리허설 결과 표시 정리 |
| v11 | [#11](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/11) | 11/2 20:45 | 리뷰에서 나온 나머지 4건 수정 |
| v12 | [#16](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/16) | 11/3 11:19 | 승인할 때 리허설한 계획과 같은지 확인 |
| v13 | [#17](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/17) | 11/3 14:33 | 버전 히스토리 문서, kind 재확인 (코드 변화 없음) |
| v14 | [#18](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/18) | 11/3 14:55 | 히스토리를 쉬운 말로 다시 씀 (코드 변화 없음) |

# HopCharlie AI·샌드박스 사용 설계 (D 영역 초안)

작성: 태훈 · 2026-11-02 · 근거: HopCharlie 주제 정리의 2장(공통 결론), 3장 '확정된 내용' 열, 4-6·4-7장

Plan 계약 갱신: 2026-11-02 · [platform OpenAPI](https://github.com/2026-Softbank-Hackathon-Charlie/platform/blob/e802f54467f7b921d7583bdca985537688c20a3f/api/openapi.yaml)의 `EnginePlan` 기준. AI·샌드박스 기능은 설계 초안이며, 아래 플랫폼 API 계약과 구분한다.

## 1. 원칙

- AI가 하는 일은 네 가지다. 리소스 분석, Plan 작성, 리허설 실패 진단, 수정 제안.
- AI에게는 운영 클러스터를 바꾸는 권한이 없다. 운영 변경은 사람이 승인한 Plan을 엔진(결정적 코드)이 실행할 때만 일어난다.
- AI 출력은 항상 JSON이다. 플랫폼이 스키마와 고정 규칙으로 검사하고, 통과한 것만 다음 단계로 넘긴다.
- AI가 쓰는 도구는 허용 목록으로 묶는다. 운영 쪽은 읽기 전용, 샌드박스 쪽은 자기 네임스페이스 하나만 쓸 수 있다.

## 2. 전체 흐름

`hopai run`이 1~6을 한 번에 돌린다. 사람의 승인 전까지만 하고 실행은 하지 않는다.

| # | 단계 | 누가 | 입력 | 출력 | 실패하면 |
|---|---|---|---|---|---|
| 1 | Plan 초안 | 플랫폼 (`POST /v1/plans/draft`) | 앱, 원본·대상 환경 | 엔진 Plan 초안 | — |
| 2 | 계획 | AI (읽기 전용 도구, `plan` 세션) | 초안, 두 클러스터 조사 결과, 원본 실측값(플랫폼 `sourceStats`) | Plan + 대상 매니페스트 + 근거 | 검사 오류를 넣어 다시 요청 (2회) |
| 3 | 제출 | `hopai` (agent 토큰) | 검사를 통과한 Plan | `pending_approval` Plan, 첨부 `target.yaml` | — |
| 4 | 리허설 | 엔진, 대상 클러스터 샌드박스 (`rehearsal` 세션) | Plan, 매니페스트, 테스트 데이터 | 단계별 결과, 격리 검사, 근거 | 5로 |
| 5 | 진단·수정안 | AI (샌드박스 조회 도구, `diagnose` 세션) | 이번 리허설 로그만 (가린 뒤) | 원인, 근거, 수정안 | 사본에 적용해 4를 새 샌드박스에서 다시 (최대 3회) |
| 6 | 첨부 | `hopai` | 리허설 리포트, 통과했으면 고친 매니페스트 | 첨부 갱신 (revision 오름) | 통과하지 못하면 리포트만 올리고 종료 코드 1 |
| 7 | 승인 | 운영자 (콘솔) | Plan, 첨부, AI 세션 기록 | 화면의 revision으로 승인 / 반려 | — |
| 8 | 실행 | 플랫폼과 엔진 | 승인된 Plan | 이전 결과, 증거 | 원복 경계에 따라 원복 또는 정지 (8장) |

콘솔의 이전 화면과 'AI 활동'이 이 기록을 그대로 보여준다 ([frontend PR #2](https://github.com/2026-Softbank-Hackathon-Charlie/frontend/pull/2)).

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
    "manifest": "attachment:target.yaml"
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
    "maxDowntime": "30s"
  }
}
```

- `files`, `gateway`, `verify`, `thresholds`는 선택 필드다. `files`를 쓰면 `source`, `target` 둘 다 필요하며, 값은 runner에 설정한 rclone remote 경로다. 접속 키는 Plan에 넣지 않는다.
- `gateway.mode: "none"`은 쓰기 보류·트래픽 전환을 수행하지 않는다. 운영 이전에서는 설정된 HTTP 게이트웨이를 사용한다.
- timeout은 `"10m"`, `"2m"` 같은 duration 문자열이다.
- `maxDowntime`은 서비스 중단 시간이 아니라 **쓰기 보류 시간**의 예산이다. 이전은 무중단이다. 전환 순간 두 DB가 함께 쓰기를 받지 않도록 게이트웨이가 쓰기를 잠깐 붙잡는다(fence부터 cutover까지). 그동안 읽기는 계속되고, 붙잡힌 쓰기는 거절되지 않고 새 primary로 넘어간다. 엔진은 전환 후 실측값과 이 예산을 비교하며, 초과만으로 자동 중단·원복하지 않는다. catchup 대기시간과는 다른 지표다.

### 실행 순서와 플랫폼 연동

단계 목록은 Plan에 넣지 않는다. [엔진의 고정 순서](https://github.com/2026-Softbank-Hackathon-Charlie/migration-engine/blob/f953d2612ffb/pkg/engine/engine.go)는 다음 9단계다.

```text
precheck → prepare → presync → delta → fence → switchover → cutover → verify → finalize
```

플랫폼은 `RunStep`으로 단계를 호출하며, `delta`와 `fence` 사이에 읽기 카나리와 전환 승인을 넣는다. AI는 실행 단계나 순서를 추가하지 않는다.

1. agent 토큰으로 `POST /v1/plans`에 `EnginePlan`을 제출하면 `pending_approval` 상태의 `PlanRecord`를 받는다. 이후 API에는 Plan의 `id` 대신 응답의 `ref`를 사용한다.
2. `target.manifest`를 쓸 때는 승인 전에 `PUT /v1/plans/{ref}/attachments/target.yaml`로 파일을 올린다. 첨부가 바뀔 때마다 `PlanRecord.revision`이 증가한다.
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

AI의 출력은 엔진 Plan, 대상 CNPG 매니페스트, 근거를 묶은 JSON이다 (`plan.Proposal`). `plan`은 그대로 `POST /v1/plans`에 제출하고, `manifest`는 Plan 첨부 `target.yaml`로 올려 `target.manifest: "attachment:target.yaml"`로 가리킨다. 첨부 이름은 플랫폼 OpenAPI 예시와 `charlie plan attach` 안내를 따랐다.

#### D 영역 추가 검사 (`plan` 패키지)

엔진의 `Validate`를 먼저 돌리고, 다음을 한 번에 모아 AI에 돌려준다.

| 구분 | 규칙 |
|---|---|
| 초안과 같아야 함 | id, source 전체, target의 env·context·namespace·cluster·dsn, gateway, files 전체(위치, rclone, flags. 빼거나 더할 수 없음), verify의 schemas·writtenRecords (없음과 빈 목록은 같게 봄). AI가 데이터나 비밀번호를 다른 곳으로 보내지 못하게 한다 |
| 고정 규칙 | `verify.fenceGate` 생략은 `true`로 정규화하고 `false`는 거절. `target.manifest`는 비우거나 `attachment:target.yaml`. 검증 테이블은 초안보다 줄일 수 없음(기존 필터의 테이블도 유지) |
| 상한값 범위 (제안값) | 네 값 모두 필수이며 0은 거절. maxDowntime(쓰기 보류) 5s~50s (게이트웨이는 55s(`-max-hold`, 앞단 ALB의 유휴 타임아웃 60s보다 짧게) 넘게 붙잡힌 쓰기에 503을 돌려주므로, 그 전에 끝나 실패한 요청이 없게 한다), switchoverTimeout 30s~3m, catchupTimeout 1m~15m, maxLagBytes 1 KiB~64 MiB |
| 매니페스트 | `postgresql.cnpg.io/v1` Cluster 하나(64 KiB 이하, `---`나 `...`로 문서를 더 붙일 수 없음). 엔진이 Force로 적용하므로 최상위는 apiVersion·kind·metadata·spec만, metadata는 name·namespace만 (annotations·finalizers·ownerReferences로 대상 DB를 멈추거나 지우지 못하게). 이름·네임스페이스는 Plan의 대상과 같음 |
| 매니페스트 spec | 키는 허용 목록만 (슈퍼유저, 백업 대상, 관리 계정, monitoring 금지). `replica`는 self·primary·source만(primary·source는 원본). 시작은 `bootstrap.pg_basebackup` 필수(source는 원본, secret은 `<원본>-app`, database·owner는 원본 DSN과 같음). `storage`(필수)·`walStorage`는 size(문자열, 0보다 크고 2Ti 이하)·storageClass만. `affinity`는 DB 전용 노드용 nodeSelector·tolerations(key 필수)·Pod 분산 설정만, control-plane 노드는 금지. `resources`는 requests·limits의 cpu(16 이하)·memory(64Gi 이하)만. `postgresql`은 성능·WAL 보존 설정(`max_connections`, `shared_buffers`, `wal_keep_size` 등)만, 값은 문자열. instances 1~5 정수, smartShutdownTimeout 0~600 정수. `imageName`은 `ghcr.io/cloudnative-pg/postgresql` 이미지만 |
| 복제 접속 | `externalClusters`는 원본·대상 이름만 쓰고 host·port는 Plan의 해당 DSN과 같음, user는 `streaming_replica`, sslmode는 require·verify-ca·verify-full만, dbname은 postgres만, 인증서 시크릿은 `<이름>-replication`·`<이름>-ca`만(비밀번호 키 금지) |
| 근거 | notes는 1~10개, 각 500자 이하, 빈 글 금지 (승인 화면과 AI 활동 기록에 그대로 보임) |
| 용량 (원본을 실측했을 때) | 대상을 새로 만들면 `postgresql.parameters.max_connections`를 정해야 하고, 원본 max_connections 이상(대상은 원본을 따라가는 hot standby라 작으면 뜨지 않음)이면서 원본 클라이언트 세션 수 × 1.2 이상(플랫폼 기준)이어야 함. `max_worker_processes`도 정해야 하고 원본 이상이어야 함 (플랫폼 시작 검사 `standby_settings`는 대상이 없으면 매니페스트에서 두 값을 읽고, 없으면 건너뜀). `catchupTimeout`은 원본 전체를 100 MiB/s(제안값)로 복사하는 시간보다 길어야 함. 원본 변경 속도가 이 복사 속도 이상이거나 복사가 catchupTimeout 상한(15m)보다 오래 걸리면 AI를 부르지 않고 바로 멈춤(`llm.ErrInfeasible`). 플랫폼이 이전을 시작할 때 같은 기준(platform `internal/preflight/capacity`)으로 재고 막으므로, 승인 뒤에 막히지 않게 계획 단계에서 먼저 본다. 진단 수정안에도 같은 실측값으로 다시 본다 |

수정안(진단 결과)은 두 종류다.

| kind | 고칠 수 있는 곳 |
|---|---|
| `manifest-patch` | `spec.storage`·`spec.walStorage`의 storageClass·size, `spec.resources` 아래, `spec.instances`, `spec.imageName`, `spec.smartShutdownTimeout` |
| `plan-param` | `thresholds`의 네 값, `verify.tables` |

수정안은 사본에 적용한 뒤 위 검사를 모두 다시 통과해야 한다. 한 번에 5줄까지다. 진단의 원인은 1000자, 근거(evidence)는 1~10개·각 1000자 이하다.

원본 삭제나 원복 정책을 새 Plan 필드로 추가하지 않는다. 정책이 늘면 OPA 도입을 검토한다.

## 4. AI 에이전트 (`llm`, `agentlog`)

도구를 쓰는 에이전트다 (본선 규칙: tool-using agent). Claude API Go SDK(`anthropic-sdk-go`)의 Tool Runner로 도구 호출 반복을 돌린다.

| 작업 | 세션 종류 | 도구 | 출력 | 재요청 상한 |
|---|---|---|---|---|
| 계획 | `plan` | 읽기 전용 조사 도구 (5장) | Plan + 대상 매니페스트 + 근거 (3장) | 검사 실패 2회 |
| 진단 | `diagnose` | 샌드박스 조회 도구 (리허설이 만듦) | `{cause, evidence[], fix}` | 2회 |

- 모델은 `claude-opus-5-5`(고정 ID), 생각 깊이(effort)는 `medium`이다. 데모 시간 안에 끝나도록 정했다.
- 응답은 두 겹으로 막는다. API에 JSON 스키마(`llm/schema/*.json`)를 넘겨 형식을 강제하고, 받은 뒤 `plan` 검사를 다시 돌린다.
- 검사에 걸리면 대화를 이어서 걸린 항목을 붙여 다시 묻는다. 거절, 출력 잘림, API 오류, 타임아웃(호출당 90초)은 다시 묻지 않는다. 답 한 번에 AI 호출은 10번까지다.
- 안전 분류기가 거절하면 서버가 권장 대체 모델로 다시 돌리도록 `fallbacks: "default"`를 켰다.
- AI에 보내는 Plan, 매니페스트, 로그는 비밀값을 가린 뒤 보낸다. Plan은 JSON 구조와 `${VAR}` 참조를 보존한다. 평문 비밀값은 가린 초안과 응답의 고정 필드를 비교한 뒤에만 원래 값으로 복원하며, 재요청 오류에도 원문 비밀값을 넣지 않는다.

### 기록 (플랫폼 세션)

작업마다 플랫폼에 에이전트 세션을 열고(`POST /v1/agent/sessions`), 아래를 이벤트로 남긴 뒤 닫는다 (platform ADR 0008). 콘솔의 'AI 활동'과 이전 화면, `charlie agent watch`에서 실시간으로 보인다.

| 이벤트 | 언제 |
|---|---|
| `tool_call`, `tool_result` | 도구를 부를 때와 결과 (걸린 시간 포함) |
| `blocked` | 도구가 허용 범위 밖 요청을 막았을 때 |
| `message` | AI 응답 한 번 (입력·출력 토큰, 걸린 시간) |
| `error` | 검사 실패(`status: check`), 호출 실패 |
| `decision` | 검사를 통과한 결과 |

- 기록은 에이전트 코드가 남긴다. AI가 기록을 고치거나 빼먹을 수 없다.
- 플랫폼에 보내기 전에 문자열의 비밀값과 비밀처럼 보이는 키(token, password 등) 아래 값을 가린다. 플랫폼도 저장 전에 가린다.
- 기록 요청이 실패해도 작업은 멈추지 않는다. 검사를 통과한 결과는 그대로 쓰고, 남기지 못한 기록은 경고로 알린다(`OnLogError`).
- 계획 세션은 Plan 제출 전에 열리므로 planRef가 없다. 그래서 리허설 세션(planRef 있음)의 첫 기록에 planRef와 계획 세션 ID를 남겨, 승인 화면에서 계획 세션을 찾아갈 수 있게 한다. 리허설과 진단 세션은 planRef와 함께 열린다.
- Anthropic API 키(`ANTHROPIC_API_KEY`)와 플랫폼 agent 토큰은 환경 변수로만 읽는다.

## 5. 도구 허용 목록

Go 함수로 구현하고, 함수 안에서 허용 범위를 다시 검사한다. 범위 밖이면 `blocked`로 남기고 오류로 돌려준다. AI가 임의의 명령을 실행하는 도구는 없다. Secret을 읽는 도구도 없다.

| 도구 | 범위 | 자격 증명 |
|---|---|---|
| `get_app` | 플랫폼의 앱 설정 (`GET /v1/apps`). 비밀번호 없음 | agent 토큰 |
| `list_storage_classes` | 이번 이전의 두 환경만. StorageClass 이름, 프로비저너, 기본 여부 | 환경별 읽기 전용 kubeconfig |
| `node_capacity` | 이번 이전의 두 환경만. 노드 할당 가능 CPU, 메모리 | 같음 |
| `get_cnpg_cluster` | 이번 이전의 두 환경, 앱 네임스페이스만. spec 중 instances, image, storage, resources, bootstrap, replica만 돌려줌 | 같음 |
| 진단 도구 | 리허설 샌드박스 네임스페이스만 (7장) | 샌드박스 토큰 |

원본 실측은 AI 도구가 아니다. `hopai`가 Plan 초안을 받을 때 플랫폼에 같이 재 달라고 하고(`POST /v1/plans/draft`에 `"sourceStats": true`, platform #47·ADR 0017), 숫자만 AI에 보여 준다. 플랫폼은 시작 전 용량 검사와 같은 쿼리로 클라이언트 세션 수, max_connections, max_worker_processes, DB 크기, 5초 동안의 WAL 생성 속도를 잰다. 그래서 이 실행기는 운영 DB에 붙지 않고 비밀번호도 갖지 않으며, 계획 검사와 시작 검사가 같은 숫자를 본다. 플랫폼이 재지 못하면(`sourceStatsReason: not_measured`: 비밀번호 변수 없음, 닿지 못함, 쿼리 실패) 경고만 내고 용량 검사 없이 계획한다(`-no-measure`로 요청하지 않을 수 있음). 크기나 WAL 속도만 빠지면 접속 수는 검사하고 초기 복사 검사는 하지 않는다.

## 6. 샌드박스

리허설은 **대상 클러스터**에 만든다. 대상 환경의 차이(StorageClass, 노드 자원, 네트워크)를 미리 드러내는 것이 목적이다.

| 항목 | 설정 |
|---|---|
| 네임스페이스 | `sandbox-<app>-<4자리 id>`, 라벨 `hopcharlie/sandbox=true`, 주석 `expires-at` |
| Pod Security | `pod-security.kubernetes.io/enforce=baseline`. AI 계정은 Pod를 만들 수 없지만, 권한이 넓어지는 경우를 대비해 hostNetwork(NetworkPolicy를 받지 않음), hostPath, privileged Pod를 막는다. 격리 검사가 관리자 계정의 dry-run으로 실제로 막히는지 따로 확인한다. CNPG Pod가 뜰 수 있게 restricted가 아니라 baseline |
| RBAC | 이 네임스페이스에만 바인딩한 Role. 쓰기는 CNPG Cluster만(리허설과 엔진), 읽기는 Pod·PVC·이벤트(진단 도구, 멈춤 감지)와 secret 이름 조회(임시 원본 앱 계정)만. Pod·Service·Job은 만들 수 없다(Service externalIPs로 클러스터 트래픽을 가로채는 길, CVE-2020-8554). TokenRequest로 1시간짜리 토큰 발급 |
| NetworkPolicy | 기본 전부 차단. 같은 네임스페이스와 kube-dns만 허용. 운영망, Tailscale 대역(100.64.0.0/10), 169.254.169.254 차단 |
| ResourceQuota, LimitRange | 기본 CPU 4, 메모리 8 GiB, Pod 20개 (데모 값). 리허설은 대상 매니페스트 몫(인스턴스와 붙일 때 뜨는 작업 Pod 하나 × limits, 없으면 requests)을 더해 잡는다. 운영에 맞게 정한 자원을 샌드박스 때문에 줄이지 않게 하려는 것이다. 샌드박스 사본에서는 requests만 적은 자원의 limits를 requests와 같게 둔다 (LimitRange 기본 limits가 더 작으면 Pod가 거절됨) |
| 데이터 | 임시 PostgreSQL(CNPG)에 테스트 데이터만. 운영 데이터는 넣지 않는다 (파일 리허설은 아직 없음) |
| 삭제 | 리허설이 끝나면 바로 지운다. 남은 것은 `expires-at`이 지난 네임스페이스를 `Sweep`이 지운다 (CronJob 매니페스트는 아직 없음) |

**CNPG 모드 (`ForCNPG`, `sandboxctl -cnpg`):** 리허설은 샌드박스에 CNPG 클러스터를 띄우므로, 오퍼레이터(`cnpg-system`)가 들어오고 인스턴스가 API 서버로 나가는 길만 더 연다. API 서버 주소는 `default/kubernetes`의 EndpointSlice에서 읽는다. 이때 격리 검사는 API 서버 대신 다른 네임스페이스의 서비스(`cnpg-webhook-service.cnpg-system`)가 막히는지로 정책 동작을 증명한다. 로컬 kind(kindnet, CNPG 1.25.1)에서 정책 없는 네임스페이스에서는 열리고 샌드박스에서는 막히는 것을 확인했다.

엔진은 대상 네임스페이스의 `clusters.postgresql.cnpg.io`만 조회·적용·패치하므로, 리허설 때 엔진도 샌드박스 토큰(최소 권한)으로 돌린다.

**NetworkPolicy가 실제로 동작하는지 확인해야 한다.** EKS는 VPC CNI의 네트워크 정책 기능을, GKE는 Dataplane V2를 켜야 한다. k3s는 기본 정책 컨트롤러를 끄지 않았는지 본다. 11/4 A 영역 목표("NetworkPolicy 동작 확인")와 같이 진행한다.

### 격리 검증 (차단 테스트)

리허설마다 고정 스크립트가 AI용 샌드박스 토큰으로 다음 명령을 시도하고, 모두 막히는지 확인한다. AI가 스스로 이 명령을 실행하길 기대하지 않는다. 결과는 콘솔의 '막힌 명령' 표에 나온다.

- `kubectl get secrets -n <운영 네임스페이스>` → RBAC가 막아야 한다
- `curl http://169.254.169.254/` → NetworkPolicy가 막아야 한다
- 운영 DB 주소로 연결 → NetworkPolicy가 막아야 한다
- 자기 NetworkPolicy 삭제, 자기 Role 수정, 토큰 발급, 운영 Deployment 삭제 → 권한 확인 요청으로 막혔는지 본다 (실제로 지우지 않음)
- API 서버(CNPG 모드에서는 다른 네임스페이스 서비스) → 정책이 없으면 열리는 곳이라 정책 동작의 증거
- hostNetwork·hostPath Pod 생성 (dry-run) → AI 계정은 RBAC(권한 확인 요청으로 Pod 생성 권한이 없는지)가, 권한이 있다면 Pod Security가 막아야 한다. Quota 거절처럼 다른 이유로 막힌 것은 통과로 치지 않는다
- 같은 호스트 Pod를 관리자 계정으로 dry-run → RBAC를 통과하므로 Pod Security만 막을 수 있다. 샌드박스의 baseline이 실제로 동작하는지의 증거
- 자기 네임스페이스 조회와 DNS는 열려야 한다 (리허설이 가능한지, 검사 자체를 믿을 수 있는지)

결과는 리허설 세션의 `blocked` 이벤트로도 남는다.

## 7. 리허설 (`rehearsal`)

대상 클러스터의 샌드박스에서 **실제 migration-engine**으로 Plan을 끝까지 돌린다. 고정값을 돌려주는 단계는 없다.

1. 샌드박스를 CNPG 모드로 만들고(6장) 격리 검사를 돌린다. 막혀야 하는 명령은 `blocked` 이벤트로 남는다. 격리가 깨지면 그 샌드박스에서 아무것도 하지 않고 리허설 전체를 멈춘다.
2. 임시 원본 CNPG 클러스터를 만든다. 이름은 Plan의 원본 클러스터와 같고, 엔진의 분산 토폴로지(`replica.source`가 대상)를 따른다. 검증할 스키마마다 테스트 데이터를 넣는다. 운영 데이터는 넣지 않는다.
3. AI가 쓴 대상 매니페스트를 샌드박스로 옮긴다. 네임스페이스와 접속 주소만 바꾸고 StorageClass, 크기, 자원, 인스턴스 수, 이미지는 그대로 둔다. 그 값이 대상 클러스터에서 되는지가 리허설이 보려는 것이다.
4. 엔진을 라이브러리로 부른다. 쿠버네티스는 **샌드박스 토큰**(최소 권한)으로, DB는 로컬 프록시(연결마다 현재 primary Pod로 port-forward)로 붙는다. 게이트웨이, 파일, k6 기록은 샌드박스에 없으므로 뺀다. 상한값과 스키마는 AI의 Plan을 따르며, 테이블 필터는 샌드박스에 만든 합성 fixture 이름으로 바꾼 사본을 사용한다.
5. 같은 경고(PVC ProvisioningFailed, FailedScheduling, 이미지 풀 실패 등)가 25초 넘게 **계속 되풀이되면** 기다려도 풀리지 않는 환경 문제로 보고 엔진을 멈춘다. 이 실행 전부터 있던 경고와, 한 번 나고 다시 일어나지 않은 경고는 보지 않는다. 엔진이 끝나면 감시를 먼저 멈추므로 성공한 실행이 늦은 판정으로 실패가 되지 않는다. 엔진의 catchupTimeout(분 단위)까지 기다리지 않는다.
6. 엔진 단계 이벤트는 `engine.<단계>` 도구 호출·결과로 리허설 세션에 남는다.
7. 통과 기준: 엔진 결과 `succeeded`와 실제 합성 데이터 검증 증거가 모두 필요하다. 각 스키마의 `rehearsal_orders/items`를 명시적으로 비교하고, fence 비교와 verify 결과에서 모든 fixture의 행 수가 예상값과 일치하며 0보다 커야 한다. 누락·중복·빈 검증은 실패다. 각 시도의 `coverage`에 실제 원본·대상·검사 행 수를 남긴다. 운영 Plan의 테이블 필터는 유지되며, 이 coverage는 합성 데이터에 대한 것이다.
8. 실패하면 샌드박스가 살아 있는 동안 AI가 진단한다(샌드박스 조회 도구: `sandbox_events`, `sandbox_cnpg_status`, `sandbox_pvcs`, 대상 환경의 `list_storage_classes`, `node_capacity`). 수정안이 검사를 통과하면 새 샌드박스에서 다시 돌린다. 최대 3회다.
9. 끝나면 샌드박스 삭제를 요청한다. 각 시도의 `cleanup.status`는 `delete_requested`(요청 수락) 또는 `failed`이며, 요청 수락을 namespace 삭제 완료로 간주하지 않는다. 삭제 실패의 원인은 마스킹하여 보고서와 경고에 남긴다. 리허설 리포트(`rehearsal-report.json`)는 결과와 상관없이 Plan 첨부로 올린다. 수정안은 **리허설을 통과했을 때만** 반영한다. 고친 매니페스트는 `target.yaml` 첨부로 올리고, Plan 값(`plan-param`)이 바뀌었으면 Plan을 새로 제출한다. 앞의 Plan은 운영자가 반려한다(agent 토큰은 반려할 수 없다). 통과하지 못한 수정안이 승인 대상 revision이 되지 않게 하기 위해서다.
10. 리포트에는 형식 버전 `schemaVersion: hopcharlie.rehearsal/v1`과 리허설한 후보 `candidate`가 들어간다(각 시도에도 `candidate`가 있다). `candidate.planSha256`은 `POST /v1/plans`로 보낸 본문의 SHA-256이고, `candidate.manifestSha256`은 `target.yaml` 첨부 내용의 SHA-256이다(매니페스트가 없는 Plan이면 비어 있다). 플랫폼 승인 게이트(platform #24)는 이 값을 저장된 Plan 본문과 첨부의 sha256과 비교한다. revision은 넣지 않는다. 리포트를 첨부하면 revision이 올라가서 자기 참조가 되기 때문이다. `hopai`는 리포트를 올리기 전에 같은 비교를 하고, 다르면 종료 코드 1로 끝난다. 해시는 입력이 같다는 것만 보장하며, 리허설을 실제로 했다는 것은 이 runner를 믿는 데 기댄다.

`passed`는 엔진 실행과 합성 데이터 검증 결과다. 이벤트 전송·세션 종료 기록·정리 실패는 별도의 `warnings`에 남기며 성공한 실행 결과를 뒤집지 않는다. CLI는 경고를 출력하고 같은 실행 판정으로 종료한다. 실행 실패·취소나 필수 Plan/리포트 첨부 실패는 종료 코드 1이다. 세션을 처음 열지 못하면 실행하지 않는다. 취소 후에도 정리는 최대 60초, 최종 세션 기록은 별도의 제한 시간(기본 5초) 안에서 시도한다. 기록 실패 시 플랫폼 세션이 갱신되지 않았을 수 있으므로 보고서의 경고를 확인한다. `cleanup.status: failed`인 namespace는 운영자가 확인하고 `sandboxctl sweep` 등으로 정리해야 한다.

다루지 않는 것: 클러스터 사이 네트워크(원본과 대상이 한 클러스터 안), 게이트웨이와 카나리, 파일(rclone), 부하(k6). 이것들은 실제 이전에서 엔진과 플랫폼이 다룬다.

운영용 `/v1/plans`의 호스트·비밀번호 정책을 우회하지 않는다. 리허설 Plan은 플랫폼에 제출하지 않고 엔진에만 넘기며, DB·게이트웨이·파일이 모두 샌드박스(또는 없음)를 가리킨다. 대상 클러스터 하나에서의 리허설만으로 실제 클러스터 간 연결·복제가 검증되는 것은 아니다.

## 8. 실행 중 실패: 원복 경계

엔진이 처리하고, AI는 판단하지 않는다 (2장 복구 경계).

- 쓰기 보류 또는 원본 demote 이후이면서 대상 DB **승격 요청 전**의 실패는 자동 원복한다. 그 이전 실패는 `failed`로 종료한다. 승격 요청 이후에는 결과가 불확실할 수 있어 자동 원복하지 않고 `halted`로 멈춘다(split-brain 방지). 그다음 AI explain을 호출한다.
- 이 경계는 엔진이 처리하며 Plan에 `rollback` 필드를 넣지 않는다. 성공한 이전을 되돌리는 operator 전용 `POST /v1/migrations/{runId}/rollback`은 별도의 역방향 이전이다.
- 콘솔은 `rolled_back`과 `halted`, `failed`를 따로 표시한다.

## 9. 데모 실패 시나리오

- 원인 예: 대상 매니페스트에 원본 클러스터의 StorageClass(예: EKS의 `gp3`)가 남아 있으면 대상에서 CNPG PVC가 Pending이 된다.
- 리허설은 이것을 실제로 잡는다. 로컬 kind에서 `gp3`로 돌리면 `storageclass.storage.k8s.io "gp3" not found` 경고가 근거로 남고, 엔진은 쓰기 보류 전에 멈춘다(원본 그대로).
- 흐름: 1차 실패 → AI 진단(샌드박스 조회 도구로 StorageClass 확인) → `manifest-patch` 수정안 → 2차 통과 → 고친 `target.yaml` 첨부 → 승인 화면에 첨부와 세션 기록.
- 남은 결정: AI가 처음부터 대상 StorageClass를 맞게 고르면 실패가 나오지 않는다. 데모에서 실패를 보이려면 어떻게 할지(예: 원본 설정을 그대로 옮긴 매니페스트로 시작) 11/6에 정한다.

## 10. 날짜별 (D 영역)

목표일보다 먼저 끝난 항목도 상태에 완료로 적는다 (11/3 기준).

| 목표일 | 목표 | 상태 |
|---|---|---|
| 11/2 (월) | 조사 도구, 계획 프롬프트 | 완료 (`get_app`, `list_storage_classes`, `node_capacity`, `get_cnpg_cluster`) |
| 11/3 (화) | Plan 검사, 재요청, 고정 검사 | 완료 (엔진 Plan 기준) |
| 11/4 (수) | 샌드박스, RBAC·NetworkPolicy·Quota, 리허설 1회 | 완료 (kind) |
| 11/5 (목) | 실패 → 진단 → 수정 → 재통과, 격리 검증 | 코드 완료. AI 진단은 API 키로 실제 확인 필요 |
| 11/6 (금) | 데모 실패 시나리오, 타임아웃·상한 값 확정 | 남음 |

## 11. 팀 결정이 필요한 것

- **DB 지원 범위:** 현재 엔진은 CNPG replica·demote·promote를 사용한다. 다른 DB 이전 방식을 지원하려면 엔진 변경과 별도 합의가 필요하다. AI가 Plan에 이전 방식 enum을 새로 만들지 않는다.
- **트래픽 게이트웨이 연결:** 현재 플랫폼의 Go 게이트웨이 계약을 사용한다. 각 환경 backend 주소와 샌드박스용 게이트웨이를 인계받아야 한다.
- **AI 범위 (3장 7번):** 온보딩(Dockerfile·PR 생성)을 넣을지. 콘솔에서는 일단 뺐다.
- **승인 게이트 (platform #24):** 리포트가 리허설한 후보를 `candidate`(Plan 본문과 `target.yaml`의 SHA-256)로 남기는 쪽은 끝났다([#16](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/16), 7장 10번). 플랫폼이 승인할 때 이 값과 `passed`를 확인하는 쪽이 남았다.
- **계획 세션의 planRef:** 지금은 리허설 세션에 계획 세션 ID를 남겨 연결한다. 세션에 planRef를 나중에 붙이는 API가 플랫폼에 생기면 계획 세션을 Plan에 직접 연결할 수 있다 (platform 담당과 상의).
- **리허설 범위:** 지금은 대상 클러스터 하나에 원본과 대상을 모두 만든다. 클러스터 사이 복제까지 시험하려면 양쪽에 샌드박스가 필요하다.
- **샌드박스 관리 자격 증명:** 리허설은 대상 클러스터에서 네임스페이스 생성·삭제, 검사 Pod, port-forward 권한이 필요하다. 어느 계정으로 할지 A 영역과 정한다.
- **타임아웃·재요청 상한 값:** 이 문서의 숫자(재요청 2회, 리허설 3회, 쓰기 보류 상한 범위, 멈춤 판단 25초)는 제안값이다.

## 12. 구현 현황과 실행 방법

| 패키지 | 내용 | 장 |
|---|---|---|
| `plan` | AI 출력(엔진 Plan + 매니페스트 + 근거) 검사, 진단 수정안 검사와 사본 적용, 제출 본문과 후보 digest | 3, 7 |
| `mask` | AI와 플랫폼에 보내기 전 비밀값 가리기 | 4 |
| `agentlog` | 플랫폼 에이전트 세션 기록 (`/v1/agent/sessions`) | 4 |
| `llm` | 도구를 쓰는 에이전트 (계획, 진단, 검사에 걸리면 대화를 이어 재요청), 읽기 전용 조사 도구 | 4, 5 |
| `sandbox` | 샌드박스 생성·삭제, 1시간 토큰, 만료 정리, CNPG 모드, 격리 검사 | 6 |
| `rehearsal` | 샌드박스에서 실제 엔진으로 리허설, 실패 시 진단과 재실행, 리포트(`candidate`, `coverage`, `cleanup`, `warnings`) | 7 |
| `cmd/hopai` | 전체 흐름 (초안 → 계획 → 제출 → 리허설 → 후보 일치 확인 → 첨부) | 2 |
| `cmd/sandboxctl` | 샌드박스를 손으로 만들고 검사하고 지우는 도구 | 6 |

### 실행

비밀값은 환경 변수로만 받는다. 명령줄 옵션은 셸 기록과 프로세스 목록에 남기 때문이다.

```bash
export ANTHROPIC_API_KEY=...   PLATFORM_URL=...   PLATFORM_TOKEN=...   # agent 토큰
go run ./cmd/hopai run -app moimroom -from gke -to eks \
  -env gke=gke-readonly.kubeconfig -env eks=eks-readonly.kubeconfig \
  -sandbox-kubeconfig eks-sandbox-admin.kubeconfig \
  -target db=moimroom-db-eks-rw.moimroom.svc.cluster.local:5432
```

- `-env`는 AI 조사 도구가 쓰는 환경별 읽기 전용 kubeconfig다.
- `-sandbox-kubeconfig`는 대상 클러스터에서 샌드박스를 만들고 지우는 관리용이다. 엔진은 이것이 아니라 샌드박스 토큰으로 돈다.
- 끝나면 Plan ref와 revision, 세션 ID, 리허설 리포트를 출력한다. 리허설을 통과하지 못하면 종료 코드 1이고 승인하지 말라고 알린다.
- 비공개 모듈(platform, migration-engine)을 받으려면 `go env -w GOPRIVATE=github.com/2026-Softbank-Hackathon-Charlie`가 필요하다.

### 테스트

```bash
go test ./...
```

- 가짜 Anthropic API와 가짜 플랫폼으로 요청 형태, 도구 사용과 기록, 재요청, 거절·잘림 처리, 비밀값 가리기를 확인한다.
- 형식이 깨진 AI 응답과 규칙을 어긴 응답(대상 DSN 바꾸기, fence 비교 끄기, 슈퍼유저 키 등) 사례가 들어 있다.

실제 클러스터(CNPG 설치)에서 리허설을 돌리는 테스트는 따로 있다. AI 없이 예시 계획으로 엔진만 실제로 돌린다.

```bash
REHEARSAL_KUBECONFIG=<kubeconfig> REHEARSAL_SC=standard go test -tags cluster -run Cluster -v -timeout 40m ./rehearsal/
```

로컬 kind(kindnet, CNPG 1.25.1)에서 확인한 결과 (2026-11-03, 매니페스트 검사 강화, AI 계정 권한 축소, Quota를 매니페스트에 맞춘 뒤):

| 경우 | 결과 |
|---|---|
| 대상 StorageClass `standard` | 격리 검사 11/11 → 임시 원본과 테스트 데이터(5,000행) → 엔진 9단계 모두 성공, 쓰기 보류 20.6초, 전체 96초 |
| 대상 StorageClass `gp3` (kind에 없음) | prepare에서 52초 만에 멈춤, 근거 `storageclass.storage.k8s.io "gp3" not found`, 쓰기 보류 전이라 원본 그대로 |

- 격리 검사 11개: 열려야 하는 2개(자기 네임스페이스 조회, DNS)와 막혀야 하는 9개(운영 Secret 읽기, 자기 NetworkPolicy 삭제, Role 수정, 토큰 발급, 운영 Deployment 삭제, AI 계정의 호스트 Pod, 관리자 계정의 호스트 Pod, 메타데이터 주소, CNPG 웹훅)가 모두 기대대로 나왔다. AI 계정의 호스트 Pod는 권한 확인 요청에서 RBAC가 막았고, 관리자 계정의 같은 Pod는 Pod Security가 막았다. 권한을 줄인 계정으로도 임시 원본 생성, 대상 DB 적용, 엔진 9단계가 모두 됐고, CNPG Pod는 `baseline` 아래에서 정상으로 떴다. 증거 대상인 CNPG 웹훅은 이름을 찾은 뒤 막혔다.
- 샌드박스 Quota는 매니페스트(인스턴스 2개 + 작업 Pod 1개)만큼 더해 CPU 4200m, 메모리 9Gi로 잡혔다.
- 쓰기 보류는 이전 측정(9.7초)보다 길었다. 상한(30초) 안이지만 한 번 잰 값이다.
- 운영 DB 접속 검사는 이 테스트에 대상을 넣지 않아 빠졌다. `hopai`는 `-target`으로 넣는다.
- 두 샌드박스 모두 끝나고 삭제 요청(`delete_requested`)이 들어갔다.
- 이전 결과(11/2, [#8](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/8) 시점, Pod Security와 `host-pod-create` 전): 쓰기 보류 8.7초, 전체 77초 / `gp3` 51초.

알려진 한계:

- 실제 클러스터 확인은 로컬 kind뿐이다. EKS·GKE·k3s에서 NetworkPolicy와 Pod Security가 실제로 동작하는지는 A 영역과 같이 확인해야 한다(6장).
- 쿠버네티스 경고 이벤트로 드러나지 않는 실패(예: 복제 인증서 오류)는 Plan의 `catchupTimeout`까지 기다린다.
- AI가 실제로 계획과 진단을 하는 흐름(`hopai run`)과 플랫폼 세션 기록은 가짜 API로만 확인했다. 실제 API 키와 플랫폼으로 돌려 봐야 한다.
- 파일(rclone), 게이트웨이와 카나리, 부하(k6)는 리허설하지 않는다.
