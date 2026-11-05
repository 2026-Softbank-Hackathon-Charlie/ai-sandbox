# 버전 히스토리

ai-sandbox에 main으로 합쳐진 순서대로 무엇이 생겼는지 정리했습니다. 화면이 없는 레포라 스크린샷 대신 버전마다 **무엇이 달라졌나**와 **어떻게 확인했나**를 적었습니다.

| 버전 | PR | 머지 | 내용 | 테스트 |
| --- | --- | --- | --- | --- |
| [v0](#v0--설계-초안) | 직접 커밋 | 11/2 10:06 | 설계 초안 (코드 없음) | - |
| [v1](#v1--설계-문서를-플랫폼-형식에-맞춤) | [#3](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/3) | 11/2 16:27 | 계획 예시를 플랫폼 형식에 맞춤 (코드 변화 없음) | - |
| [v2](#v2--ai-계획-검사) | [#4](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/4) | 11/2 18:39 | AI가 만든 이전 계획을 규칙으로 검사 | 14 |
| [v3](#v3--샌드박스) | [#5](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/5) | 11/2 18:44 | AI 전용 격리 공간(샌드박스)과 격리 검사 | 28 |
| [v4](#v4--ai-에이전트와-활동-기록) | [#6](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/6) | 11/2 18:49 | 직접 조사해 계획을 만드는 AI, 활동 기록 | 40 |
| [v5](#v5--리허설과-전체-실행-명령) | [#7](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/7) | 11/2 18:52 | 승인 전에 미리 돌려 보는 리허설 | 52 |
| [v6](#v6--설계-문서-갱신) | [#8](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/8) | 11/2 18:54 | 구현 내용을 설계 문서에 반영 (코드 변화 없음) | 52 |
| [v7](#v7--maxdowntime-설명) | [#9](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/9) | 11/2 19:05 | `maxDowntime` 뜻 풀어 쓰기 (코드 변화 없음) | 52 |
| [v8](#v8--보안-구멍-3개-막기) | [#10](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/10) | 11/2 19:53 | 보안 구멍 3개 막기 | 53 |
| [v9](#v9--ai-계획과-데이터-검증의-빈틈) | [#14](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/14) | 11/2 20:32 | AI 계획과 데이터 검증의 빈틈 막기 | 65 |
| [v10](#v10--리허설-결과-정리) | [#15](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/15) | 11/2 20:40 | 리허설 결과 표시 정리 | 73 |
| [v11](#v11--리뷰-나머지-4건) | [#11](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/11) | 11/2 20:45 | 리뷰에서 나온 나머지 4건 수정 | 75 |
| [v12](#v12--승인할-때-같은-계획인지-확인) | [#16](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/16) | 11/3 11:19 | 승인할 때 리허설한 계획과 같은지 확인 | 78 |
| [v13](#v13--버전-히스토리-문서) | [#17](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/17) | 11/3 14:33 | 버전 히스토리 문서, kind 재확인 (코드 변화 없음) | 78 |
| [v14](#v14--히스토리를-쉬운-말로) | [#18](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/18) | 11/3 14:55 | 히스토리를 쉬운 말로 다시 씀 (코드 변화 없음) | 78 |
| [v15](#v15--버전-표-정리) | [#19](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/19) | 11/3 15:03 | 버전 표에서 작성자 빼고 칸 비율 정리 (코드 변화 없음) | 78 |
| [v16](#v16--readme를-코드에-맞춤) | [#20](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/20) | 11/3 15:29 | README를 지금 코드에 맞춤 (코드 변화 없음) | 78 |
| [v17](#v17--maxdowntime-상한-50초) | [#22](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/22) | 11/3 17:42 | `maxDowntime` 상한을 60초에서 50초로 | 78 |
| [v18](#v18--ai-출력-검사와-샌드박스-권한-좁힘) | [#21](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/21) | 11/3 21:05 | 이상한 AI 출력 막기, 샌드박스 권한 좁힘 | 85 |
| [v19](#v19--원본-실측으로-용량-검사) | [#23](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/23) | 11/4 10:22 | 원본 실측값으로 접속 수·초기 복사 시간 검사 | 99 |
| [v20](#v20--원본과-같은-postgresql-메이저-버전) | [#24](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/24) | 11/4 13:39 | 원본과 같은 PostgreSQL 메이저 버전, CNPG 1.30.1 확인 | 109 |
| [v21](#v21--버전-히스토리-보충) | [#25](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/25) | 11/4 16:40 | 버전 히스토리에 v15~v20 추가 (코드 변화 없음) | 109 |
| [v22](#v22--데모-실패-시나리오) | [#26](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/26) | 11/4 17:14 | 데모 실패 시나리오 (`hopai -demo-copy-source-storage`) | 114 |
| [v23](#v23--격리-검사-대기) | [#27](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/27) | 11/5 14:20 | 격리 검사 Pod가 정책이 붙은 뒤에 연결, 실패 항목을 메시지에 남김 | 114 |
| [v24](#v24--실제-eks-리허설-수정) | [#28](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/28) | 11/5 14:21 | 실제 EKS에서 리허설 임시 원본이 뜨지 않던 문제 4가지 수정 | 116 |
| [v25](#v25--ai-계획-파일-저장) | [#29](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/29) | 11/5 14:21 | AI 계획을 리포트 옆 파일로 저장 | 117 |
| [v26](#v26--들여온-오픈소스-앱-배포-준비) | [#33](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/33) | 11/5 17:58 | 들여온 오픈소스 레포를 AI가 고쳐 `hopcharlie`로 push | 158 |
| [v29](#v29-배포할-수-없는-앱-알리기) | [#36](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/36) | 머지 전 | `hopai prepare`가 플랫폼이 주지 못하는 것을 찾으면 push하지 않고 알림, 새 `platform.yaml` 계약 | 171 |

- 머지 시각은 한국 시간이고, "테스트"는 그 버전에 들어 있던 테스트 함수 개수입니다.
- 닫힌 PR #1, #2는 플랫폼의 계획 형식이 정해진 뒤 #4~#8로 나눠 다시 올렸습니다.
- 이슈 #12는 #14로, #13은 #15로 닫혔습니다.
- 11/3에 v12 코드로 실제 클러스터 리허설을 다시 돌렸습니다: [11/3 kind 재확인](#113-kind-재확인)

## 먼저 알아 두면 좋은 말

| 말 | 뜻 |
| --- | --- |
| 계획 (Plan) | DB를 어디서 어디로 옮길지 적은 이전 계획. AI가 다듬고 운영자가 승인합니다 |
| 새 DB 설정 (매니페스트) | 대상 클러스터에 만들 DB(CNPG)의 설정 파일 `target.yaml` |
| 엔진 | 실제로 이전을 실행하는 프로그램(migration-engine). 정해진 9단계로 돕니다 |
| 샌드박스 | AI와 리허설만 쓰는 격리된 공간(쿠버네티스 네임스페이스). 운영 쪽으로 나갈 수 없습니다 |
| 리허설 | 승인 전에 샌드박스에서 테스트 데이터로 이전을 끝까지 미리 돌려 보는 것 |
| 쓰기 보류 | 전환하는 몇 초 동안 쓰기 요청을 잠깐 붙잡아 두는 것. 읽기는 계속되고 붙잡힌 요청도 버려지지 않습니다 |

---

## v0 · 설계 초안

직접 커밋 `02212f4` · 11/2 10:06 · README 164줄

D 영역(AI·샌드박스) 설계를 처음 문서로 적었습니다. 중심 원칙은 **"AI는 계획과 수정안만 만들고, 운영을 실제로 바꾸는 일은 사람이 승인한 뒤 엔진만 한다"** 입니다. 이 원칙 아래 계획 검사, AI가 쓸 수 있는 도구 목록, 샌드박스, 리허설, 데모 실패 시나리오를 정리했습니다.

---

## v1 · 설계 문서를 플랫폼 형식에 맞춤

[PR #3](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/3) · 11/2 16:27 · README +97 −31

**무엇이 달라졌나**

초안의 계획 예시가 실제 플랫폼이 받는 형식과 달라서 맞췄습니다.

- 초안을 요청할 때 보내는 값과, 다듬어서 제출하는 계획을 나눠 설명했습니다.
- 계획에 단계를 직접 적던 방식을 없앴습니다. 대신 엔진이 정해진 9단계로 돈다는 것과, 플랫폼이 어디서 사람 승인을 받는지 적었습니다.
- 계획 제출, 파일 첨부, 승인, AI 활동 기록에 쓰는 플랫폼 API를 정리했습니다.

**어떻게 확인했나**

문서의 예시가 실제 플랫폼 스키마를 통과하고, 잘못된 입력 9가지는 거부되는 것을 확인했습니다.

---

## v2 · AI 계획 검사

[PR #4](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/4) · 11/2 18:39 · 14파일 +1,100 · 패키지 `plan`, `mask`

**무엇이 달라졌나**

AI가 만든 계획을 그대로 믿지 않고, 정해진 규칙으로 검사해서 통과한 것만 다음 단계로 넘깁니다. 구현을 작은 PR 5개로 나눈 것 중 첫 번째입니다.

- **바꾸면 안 되는 값은 초안과 비교합니다.** 원본·대상 DB 주소, 게이트웨이, 파일 위치 같은 값입니다. AI가 데이터를 엉뚱한 곳으로 보내지 못하게 하려는 것입니다.
- **안전장치는 끌 수 없습니다.** 전환 직전에 두 DB의 데이터를 비교하는 단계(fence)는 항상 켜져 있어야 하고, 시간·지연 상한값은 정해진 범위 안이어야 합니다.
- **새 DB 설정에는 허용된 항목만 쓸 수 있습니다.** 관리자 계정이나 백업 대상 같은 위험한 설정은 거절합니다.
- **리허설 실패 후 AI가 내는 수정안도 범위가 정해져 있습니다.** 저장소 종류·크기, 자원, 인스턴스 수, 이미지, 상한값만 고칠 수 있고, 고친 뒤 전체 검사를 다시 통과해야 합니다.
- AI에 보내기 전에 비밀번호 같은 비밀값을 가립니다.

**어떻게 확인했나**

형식이 깨진 AI 응답(코드 블록에 싸인 답, 중간에 끊긴 JSON, 뒤에 붙은 설명)과 규칙을 어긴 응답(대상 DB 주소 바꾸기, 안전장치 끄기, 관리자 계정 설정)을 테스트에 넣었습니다.

---

## v3 · 샌드박스

[PR #5](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/5) · 11/2 18:44 · 7파일 +1,493 · 패키지 `sandbox`, `cmd/sandboxctl`

**무엇이 달라졌나**

AI와 리허설이 쓸 격리 공간을 만들고, 정말 밖으로 나갈 수 없는지 검사합니다.

- 리허설마다 전용 공간을 새로 만들고, 1시간이 지나면 만료됩니다. 끝나면 바로 지우고, 남은 것은 만료 정리가 지웁니다. 운영 공간을 실수로 지우지 않도록 이름과 표시(라벨)가 둘 다 맞을 때만 지웁니다.
- 네트워크는 기본으로 전부 막고, 같은 공간 안과 DNS만 엽니다. 자원도 CPU 4, 메모리 8Gi, Pod 20개로 묶습니다.
- AI 계정은 자기 공간 안에서만 움직이고, 네트워크 규칙이나 권한을 고칠 수 없습니다.
- **격리 검사:** AI 계정으로 운영 비밀 읽기, 자기 네트워크 규칙 지우기, 권한 바꾸기, 클라우드 메타데이터 주소나 운영 DB 접속 등을 실제로 시도해서 모두 막히는지 봅니다.
- `sandboxctl`은 이 샌드박스를 손으로 만들고 검사하고 지우는 도구입니다.

**어떻게 확인했나**

로컬 kind 클러스터에서 격리 검사 10개 항목이 모두 기대대로 나왔습니다. 비교를 위해 규칙이 없는 공간에서 같은 시도를 하면 열리는 것도 확인했습니다.

---

## v4 · AI 에이전트와 활동 기록

[PR #6](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/6) · 11/2 18:49 · 13파일 +1,664 · 패키지 `llm`, `agentlog`

**무엇이 달라졌나**

AI가 도구를 써서 직접 조사하고 계획을 만듭니다. 그 과정은 전부 기록되어 콘솔에서 볼 수 있습니다.

- **조사:** 계획을 만들 때 AI는 앱 설정, 저장소 종류, 노드 자원, 기존 DB 설정을 조회할 수 있습니다. 모두 읽기 전용이고, 비밀을 읽는 도구는 없습니다. 허용 범위 밖 요청은 도구가 막고 '막힘'으로 기록합니다.
- **답은 두 번 확인합니다.** 정해진 JSON 형식으로만 답하게 하고, 받은 뒤 v2의 검사를 다시 돌립니다. 걸리면 걸린 이유를 알려 주고 다시 묻습니다(최대 2회).
- **진단:** 리허설이 실패하면 AI가 로그를 보고 원인, 근거, 수정안을 냅니다.
- **기록:** 도구 호출과 결과, 막힌 요청, AI 응답(토큰과 걸린 시간), 오류, 최종 결정을 플랫폼에 남깁니다. 콘솔의 'AI 활동' 화면에 실시간으로 나옵니다. 기록은 프로그램이 남기므로 AI가 빼먹거나 고칠 수 없습니다.
- 모델은 `claude-opus-5-5`입니다. (v28부터 기본값은 `claude-sonnet-5-5` · medium, `-model`로 바꿈)

---

## v5 · 리허설과 전체 실행 명령

[PR #7](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/7) · 11/2 18:52 · 12파일 +1,721 · 패키지 `rehearsal`, `cmd/hopai`

**무엇이 달라졌나**

승인 전에 샌드박스에서 실제 이전 엔진으로 계획을 끝까지 돌려 봅니다. 결과를 흉내만 내는 단계는 없습니다.

1. 샌드박스를 만들고 격리 검사를 합니다. 하나라도 뚫리면 바로 멈춥니다.
2. 임시 원본 DB를 만들고 테스트 데이터를 넣습니다. 운영 데이터는 쓰지 않습니다.
3. AI가 쓴 새 DB 설정을 그대로 적용합니다. 그 설정이 대상 클러스터에서 실제로 되는지가 리허설로 보려는 것입니다.
4. 엔진 9단계를 실행합니다. 엔진도 샌드박스 계정으로만 움직입니다.
5. 같은 경고(예: 저장소를 만들 수 없음)가 25초 넘게 반복되면, 기다려도 풀리지 않는 문제로 보고 멈춥니다.
6. 실패하면 AI가 원인을 찾아 수정안을 내고, 새 샌드박스에서 다시 돌립니다(최대 3회).

`hopai run` 한 번으로 초안 → AI 계획 → 제출 → 리허설 → 결과 첨부까지 하고, 운영자 승인을 기다립니다.

**어떻게 확인했나** (로컬 kind, PR 본문 기준)

| 경우 | 결과 |
| --- | --- |
| 대상 저장소 종류가 맞을 때 (`standard`) | 격리 검사 10/10 → 엔진 9단계 모두 성공. 쓰기 보류 8.8초, 전체 80초 |
| 대상에 없는 저장소 종류를 쓸 때 (`gp3`) | 52초 만에 멈춤. 근거 `storageclass "gp3" not found`. 원본은 그대로 |

돌려 보면서 DB 연결 방식, 복제 인증서, 비밀번호 변수 처리를 고쳤습니다.

---

## v6 · 설계 문서 갱신

[PR #8](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/8) · 11/2 18:54 · README +169 −60

v2~v5에서 만든 내용을 설계 문서(README)에 반영했습니다. v1에서 정리한 플랫폼 형식 설명은 그대로 두고, 전체 흐름, 검사 규칙, 에이전트와 도구, 샌드박스, 리허설, 실행 방법, kind 확인 결과를 고쳤습니다.

---

## v7 · maxDowntime 설명

[PR #9](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/9) · 11/2 19:05 · README +4 −3

`maxDowntime`은 '서비스가 멈추는 시간'으로 오해하기 쉽습니다. 실제로는 **전환할 때 쓰기를 잠깐 붙잡아 두는 시간의 상한**이라고 풀어 썼습니다. 이전은 무중단이고, 그동안 읽기는 계속됩니다.

- 상한을 60초로 둔 이유를 적었습니다. 게이트웨이는 90초 넘게 붙잡힌 요청을 실패시키므로, 그보다 먼저 끝나야 합니다.
- 허용 범위를 벗어나던 예시 값 3분을 30초로 고쳤습니다.

---

## v8 · 보안 구멍 3개 막기

[PR #10](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/10) · 11/2 19:53 · 9파일 +268 −13

**무엇이 달라졌나**

리뷰에서, AI가 낸 값이 검사를 통과한 채 운영이나 샌드박스 밖에 영향을 줄 수 있는 경우가 3가지 나왔습니다.

| 문제 | 이렇게 막았습니다 |
| --- | --- |
| AI가 새 DB 설정에 다른 DB 주소를 적어, 엉뚱한 곳에서 데이터를 복제해 올 수 있었음 | 복제 출처는 계획에 적힌 원본 DB만 허용. 주소, 계정, 인증서 이름을 계획과 맞춰 봄 |
| DB 이미지를 아무거나 쓸 수 있었음 | 공식 CNPG PostgreSQL 이미지(`ghcr.io/cloudnative-pg/postgresql`)만 허용 |
| AI 계정이 노드의 네트워크나 파일을 직접 쓰는 Pod를 만들어 샌드박스를 빠져나갈 수 있었음 | 샌드박스에 Pod Security(`baseline`)를 걸고, 격리 검사에 이 항목을 추가 |

**어떻게 확인했나**

세 경우마다 테스트를 넣었고 모두 통과했습니다. 머지할 때는 실제 클러스터에서 돌려 보지 못했고, [11/3에 kind에서 다시 확인](#113-kind-재확인)했습니다.

---

## v9 · AI 계획과 데이터 검증의 빈틈

[PR #14](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/14) · 11/2 20:32 · 13파일 +602 −41 · 이슈 #12

**무엇이 달라졌나**

- **상한값 누락:** AI가 상한값을 빼먹거나 안전장치 값을 비워 두면 그대로 통과할 수 있었습니다. 이제 상한값 4개는 반드시 있어야 하고, 안전장치는 기본으로 켜집니다. 검증할 테이블 범위도 줄일 수 없습니다.
- **비밀값 가리기:** 가리는 과정에서 설정의 JSON 구조와 `${VAR}` 같은 변수 표기가 망가졌습니다. 구조를 지키도록 고쳤고, 다시 물을 때 보내는 오류 메시지에도 비밀값이 남지 않습니다.
- **비교 없이 통과:** 리허설이 비교할 데이터 없이도 통과할 수 있었습니다. 이제 테스트 데이터의 모든 표가 실제로 비교되고 행 수가 맞아야 통과합니다.

---

## v10 · 리허설 결과 정리

[PR #15](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/15) · 11/2 20:40 · 7파일 +358 −32 · 이슈 #13

**무엇이 달라졌나**

리허설 결과가 실제 상황과 다르게 보일 수 있던 부분을 고쳤습니다.

- 기록 전송이나 샌드박스 정리에 실패해도 리허설 자체의 성공·실패는 바뀌지 않습니다. 대신 '경고'로 따로 보여 줍니다.
- 샌드박스 삭제 결과를 '요청함'과 '실패'로 나눠 남깁니다. 삭제를 요청했다고 다 지워진 것으로 보지 않습니다.
- 도중에 취소돼도 정리와 기록 마무리는 따로 시간을 정해 시도합니다.
- `hopai` 명령의 종료 결과가 리포트의 판정과 같아졌습니다.

---

## v11 · 리뷰 나머지 4건

[PR #11](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/11) · 11/2 20:45 · 8파일 +81 −21

**무엇이 달라졌나**

| 문제 | 수정 |
| --- | --- |
| 격리 검사가 확인할 대상을 찾지 못해도 '막힘'으로 기록돼, 네트워크 규칙이 동작하지 않는 클러스터에서도 통과할 수 있었음 | 대상을 먼저 찾고, 못 찾으면 검사 실패 |
| 리허설에 실패해도 검증되지 않은 수정안을 플랫폼에 올렸음 | 리허설을 통과했을 때만 올림. 리포트는 항상 올림 |
| 일부 DB 주소 형식(사용자 이름이 비었거나 비밀번호에 `@`가 든 경우)에서 비밀번호를 가리지 못했음 | 가리는 규칙을 넓힘 |
| 원본 공간 이름이 32자를 넘으면 샌드박스를 만들지 못했음 | 앞 27자와 짧은 해시로 줄임 |

v9·v10과 같은 파일을 고쳐 충돌이 났고, main을 합쳐 풀었습니다.

**어떻게 확인했나**

4건마다 테스트가 있고 모두 통과했습니다. 첫 번째 수정 때문에 이제 확인 대상을 찾지 못하는 클러스터에서는 격리 검사가 실패합니다. 머지할 때는 실제 클러스터에서 돌려 보지 못했고, [11/3에 kind에서 다시 확인](#113-kind-재확인)했습니다.

---

## v12 · 승인할 때 같은 계획인지 확인

[PR #16](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/16) · 11/3 11:19 · 7파일 +200 −14 · platform #24의 ai-sandbox 쪽

**무엇이 달라졌나**

리허설을 통과한 계획과 운영자가 승인하려는 계획이 같은지 확인할 수 있게 했습니다.

- 리허설 리포트에 **어떤 계획을 리허설했는지**를 지문(SHA-256 해시)으로 남깁니다. 계획 본문과 새 DB 설정 파일 각각의 해시입니다.
- 플랫폼은 승인할 때 저장된 계획과 첨부 파일의 해시를 이 값과 비교하면 됩니다. 중간에 글자 하나라도 바뀌었으면 해시가 달라집니다.
- `hopai`도 리포트를 올리기 전에 같은 비교를 하고, 다르면 "리허설은 통과했지만 계획이 다름"이라고 알리며 실패로 끝냅니다.
- 계획의 revision 번호는 넣지 않았습니다. 리포트를 첨부하는 순간 revision이 올라가서, 리포트가 자기 자신을 가리키게 되기 때문입니다.

```json
{
  "schemaVersion": "hopcharlie.rehearsal/v1",
  "passed": true,
  "candidate": { "planSha256": "…", "manifestSha256": "…" },
  "attempts": [ { "candidate": { "…": "…" }, "coverage": { "…": "…" } } ]
}
```

**남은 일:** 플랫폼이 승인할 때 이 값을 확인하는 부분(platform #24).

---

## v13 · 버전 히스토리 문서

[PR #17](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/17) · 11/3 14:33 · README, `docs/HISTORY.md`

이 문서와 README의 버전 표를 추가했습니다. 리뷰에서 README의 kind 결과가 v8 이전 값이라는 게 나와서, 리허설을 다시 돌려 결과를 반영했습니다(아래 11/3 kind 재확인). 코드는 바뀌지 않았습니다.

---

## v14 · 히스토리를 쉬운 말로

[PR #18](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/18) · 11/3 14:55 · README, `docs/HISTORY.md`

이 문서를 버전마다 "무엇이 달라졌나"와 "어떻게 확인했나"로 나누고, 필드·패키지 이름 대신 하는 일을 풀어 썼습니다. 위의 "먼저 알아 두면 좋은 말" 표도 이때 넣었습니다. 코드는 바뀌지 않았습니다.

---

## v15 · 버전 표 정리

[PR #19](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/19) · 11/3 15:03 · README, `docs/HISTORY.md`

README와 이 문서의 버전 표에서 작성자 열을 빼고, 칸이 두 줄로 꺾이지 않게 내용을 줄였습니다. 코드는 바뀌지 않았습니다.

---

## v16 · README를 코드에 맞춤

[PR #20](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/20) · 11/3 15:29 · README

README 전체를 코드와 대조했습니다. 수정안(고친 새 DB 설정, 새 계획)은 리허설을 통과했을 때만 반영하고 리허설 리포트는 결과와 상관없이 항상 올린다는 것을 v11 이후 동작대로 고쳐 적었습니다. 코드는 바뀌지 않았습니다.

---

## v17 · maxDowntime 상한 50초

[PR #22](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/22) · 11/3 17:42 · 4파일 +8 −5

**무엇이 달라졌나**

계획의 `maxDowntime`(쓰기 보류 예산) 허용 범위를 5~60초에서 **5~50초**로 줄였습니다.

- 게이트웨이가 관리 EKS의 ALB(유휴 타임아웃 60초) 뒤로 가면서, 플랫폼 #41이 게이트웨이의 쓰기 보류 상한을 90초에서 55초로 낮췄습니다.
- 예산이 55초에 가까우면 예산 안에서 끝나도 보류 시작 직후 들어온 쓰기가 503을 받습니다. 그래서 5초 여유를 두었습니다.
- AI 지시문의 범위도 같이 고쳤습니다. 기본값 30초와 리허설 실측(약 9초)에는 영향이 없습니다.

**어떻게 확인했나**

55초 예산을 거절하는 경계 테스트를 넣었고 모두 통과했습니다. 실제 AI 호출로는 확인하지 않았습니다.

---

## v18 · AI 출력 검사와 샌드박스 권한 좁힘

[PR #21](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/21) · 11/3 21:05 · 13파일 +709 −112

**무엇이 달라졌나**

AI가 내는 계획, 새 DB 설정, 진단 수정안과 샌드박스 설정에 이상한 값을 150개쯤 넣어 봤습니다. 멈추거나 죽는 경우는 없었지만 8가지가 검사를 통과해서 막았습니다.

| 통과하던 것 | 이렇게 막았습니다 |
| --- | --- |
| 새 DB 설정에 DB를 멈추거나 지우는 표시(annotations, finalizers 등)와 모르는 키 | 최상위는 4개 키만, metadata는 이름과 네임스페이스만 허용 |
| spec 아래 아무 키(복제 지연, 디스크 복원 출처, 로그 설정 등) | 복제, 디스크, 자원, PostgreSQL 설정을 허용 목록으로만 받음 |
| 복제 접속의 `sslmode: disable`, 아무 DB 이름 | 암호화 접속 3가지와 `postgres`만 |
| 문자열 숫자, 음수, 너무 큰 값, 빠진 필수 항목 | 타입과 범위를 검사(디스크 2Ti, CPU 16, 메모리 64Gi는 제안값) |
| 문서 두 개를 이은 설정, 5MB짜리 설정 | 둘 다 거절, 64 KiB 상한 |
| 비어 있거나 너무 긴 AI 메모·근거 | 1~10개, 길이 상한 |
| AI 계정이 Pod, Service, Secret, Job까지 만들 수 있었음 | 쓰기는 CNPG 클러스터만, 읽기는 Pod·디스크·이벤트와 Secret 조회만 |
| 샌드박스 설정의 음수 CPU·메모리·Pod 수·수명 | 거절 |

**어떻게 확인했나**

- 회귀 테스트 33개를 넣었고 모두 통과했습니다.
- 로컬 kind(CNPG 1.25.1)에서 리허설: 저장소 종류가 맞을 때 격리 검사 10/10 → 엔진 9단계 성공(쓰기 보류 9.7초, 전체 81초), `gp3`일 때 52초 만에 멈춤.
- 줄인 권한으로도 임시 원본 만들기, 새 DB 설정 적용, 엔진 실행, 진단 도구가 모두 동작했습니다.

---

## v19 · 원본 실측으로 용량 검사

[PR #23](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/23) · 11/4 10:22 · 16파일 +657 −27 · platform #38, #47과 짝

**무엇이 달라졌나**

플랫폼은 이전을 시작할 때 원본과 대상을 재서 부족하면 막습니다(용량 검사, 대기 서버 설정 검사). 그런데 AI는 재지 않고 값을 정해서, 리허설과 승인까지 통과한 계획이 시작 단계에서 막힐 수 있었습니다.

- `hopai`가 계획 초안을 받을 때 플랫폼에 **원본 실측값**을 같이 요청합니다(platform #47). 이 실행기는 운영 DB에 직접 붙지 않고 비밀번호도 갖지 않습니다.
- 실측값이 있으면 계획 검사가 대상의 `max_connections`와 `max_worker_processes`를 원본 기준으로 보고, `catchupTimeout`이 원본 전체를 복사하는 최소 시간보다 긴지 봅니다.
- 원본이 너무 크거나 변경이 복사보다 빨라 어떤 계획으로도 안 되면 AI를 부르지 않고 이유를 바로 알립니다.
- 리허설 임시 원본도 대상과 같은 접속 수·워커 수로 띄워, 리허설에서만 실패하는 경우를 없앴습니다.
- 플랫폼이 재지 못하면 경고만 내고 지금처럼 계획합니다.

**어떻게 확인했나**

계획 검사, AI 전달, 플랫폼 응답 변환 테스트를 넣었고 모두 통과했습니다. 실제 플랫폼과 실제 Claude로는 확인하지 못했습니다.

---

## v20 · 원본과 같은 PostgreSQL 메이저 버전

[PR #24](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/24) · 11/4 13:39 · 16파일 +592 −48

**무엇이 달라졌나**

실제 클러스터는 CNPG 1.30.1이고, DB 이미지를 비우면 PostgreSQL 18.6으로 뜹니다. 데모 원본 moimroom은 17.11로 고정돼 있습니다. 대상은 원본의 물리 복제본이라 메이저 버전이 같아야 합니다.

- `hopai`가 원본 CNPG 클러스터에서 이미지와 hot standby 설정 5개를 읽어 AI에 보여 줍니다.
- 원본을 읽었으면 대상 DB 이미지를 반드시 적고 원본과 같은 메이저여야 하며, 원본이 정한 hot standby 설정은 대상도 원본 이상이어야 합니다.
- 리허설 임시 원본을 실제 원본의 이미지와 설정으로 띄웁니다. 전에는 대상 클러스터 기본값(18.6)으로 떠서 리허설에서만 실패했습니다.
- 경고 이벤트 없이 CNPG 작업 Job만 거듭 실패하면(같은 Job 3번) 멈추고, Pod 로그의 오류 줄을 근거로 남깁니다. 전에는 최대 15분을 기다렸습니다.

**어떻게 확인했나**

CNPG 1.30.1, 기본 이미지 18.6인 kind에서 리허설 5건을 돌렸습니다.

| 경우 | 결과 |
| --- | --- |
| 저장소 종류가 맞을 때 (`standard`) | 격리 검사 11/11 → 엔진 9단계 성공. 쓰기 보류 19.5초, 전체 97초 |
| 대상에 없는 저장소 종류 (`gp3`) | 52초 만에 멈춤 |
| 원본을 모르고 대상 설정이 기본값보다 작을 때 | 임시 원본을 대상 값으로 띄워 성공, 88초 |
| 원본 17.11을 읽었을 때 (moimroom과 같음) | 임시 원본을 17.11로 띄워 성공, 92초 |
| 원본 18.6 / 대상 17.11 | 123초 만에 멈춤. 근거 `incompatible server version 18.6` |

**남은 일**

- 쓰기 보류가 측정마다 9~41초로 달랐습니다(단일 노드 kind). 11/6에 실제 클러스터에서 다시 재야 합니다.
- 플랫폼 시작 검사의 메이저 버전 비교는 [platform #50](https://github.com/2026-Softbank-Hackathon-Charlie/platform/issues/50)으로 제안했고 platform #51로 들어갔습니다.

---

## v21 · 버전 히스토리 보충

[PR #25](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/25) · 11/4 16:40 · 2파일 +127 −2

**무엇이 달라졌나**

이 문서와 README의 버전 표가 v14에서 멈춰 있어서 v15~v20을 추가했습니다. README 10장의 날짜별 상태도 11/4 기준으로 고쳤습니다. 코드는 그대로입니다.

**어떻게 확인했나**

- 목차 링크 22개가 모두 제목과 맞는지 GitHub 제목 링크 규칙으로 비교했습니다.
- 테스트 수는 각 머지 커밋의 테스트 함수 개수입니다.

---

## v22 · 데모 실패 시나리오

[PR #26](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/26) · 11/4 17:14 · 5파일 +531 −6

**무엇이 달라졌나**

AI는 조사 도구로 대상 StorageClass를 보고 처음부터 맞게 고르기 때문에, 그대로 두면 리허설이 처음부터 통과해서 데모에서 "실패 → AI 진단 → 수정 → 재통과"를 보여 줄 수 없었습니다. `hopai run -demo-copy-source-storage` 옵션을 넣었습니다.

- AI 계획의 `storageClass`(있으면 `walStorage`도)를 **원본 DB가 실제로 쓰는 값**으로 바꿔 제출합니다. 원본 설정을 그대로 옮긴 계획과 같습니다.
- 대상에 그 StorageClass가 없으면 1차 리허설에서 DB 볼륨이 실제로 Pending이 되고, 멈춤 감지(25초)와 AI 진단이 돕니다. 진단 수정안이 통과하면 고친 `target.yaml`을 첨부해 revision이 올라갑니다.
- 실패가 나지 않을 조건(AI가 이미 원본 값을 골랐거나, 대상에도 그 값이 있거나, 목록을 읽지 못함)이면 시작하지 않고 이유를 알립니다.
- 바꾼 사실은 감추지 않습니다. 표준 오류와 결과 JSON의 `demo`, 그리고 플랫폼 에이전트 기록의 **"운영자 데모 설정"** 세션으로 남아 콘솔에서 리허설 실패보다 먼저 보입니다.

**어떻게 확인했나**

- kind에서 원본 StorageClass `standard-rwo`를 그대로 옮겼을 때 1회차가 prepare에서 55초 만에 멈추고, 수정안(`/spec/storage/storageClass` → `standard`) 뒤 2회차가 통과했습니다(쓰기 보류 19.7초, 96초). 진단은 AI 대신 정해진 수정안을 내는 가짜 진단기로 했습니다.

**남은 일**

- 실제 AI 진단으로 같은 장면이 나오는지는 API 키가 있는 환경에서 확인해야 합니다.

---

## v23 · 격리 검사 대기

[PR #27](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/27) · 11/5 14:20 · 4파일 +45 −8

**무엇이 달라졌나**

실제 Claude, 플랫폼, EKS로 `hopai run`을 처음 돌렸을 때(GKE→EKS) AI 계획과 제출은 46초에 끝났지만, 리허설 1회차에서 격리 검사 12개 중 네트워크 3개(운영 DB, 다른 네임스페이스의 CNPG 웹훅, 메타데이터 주소)가 열린 것으로 나와 멈췄습니다. 실패 메시지도 `<nil>`이라 무엇이 걸렸는지 알 수 없었습니다.

- 원인은 워크로드 EKS의 VPC CNI(v1.22.4) standard 모드입니다. **새 Pod에 정책을 붙이기 전 몇 초 동안 모든 통신이 열립니다.** 검사 Pod가 뜨자마자 연결해서 그 틈에 걸렸습니다. 같은 샌드박스에서 새로 띄워 바로 연결하면 세 곳 다 열렸고, 띄워 두고 0·20·40초 뒤 연결하면 모두 막혔습니다.
- 검사 Pod가 연결 전에 15초 기다립니다(`IsolationConfig.Settle`, 기본 `sandbox.DefaultSettle`, 0~5분).
- 격리 검사가 실패하면 기대와 다른 항목을 메시지에 적습니다(예: `db: blocked 기대, open`).

**어떻게 확인했나**

- 단위 테스트 통과.
- 실제 워크로드 EKS에서 `sandboxctl create -cnpg` 뒤 `check`(운영 DB 대상 포함)가 12/12 통과했습니다. 검사 28초(대기 15초 포함). PR 본문의 기록입니다.

**남은 일**

- 이 틈은 리허설 CNPG Pod에도 있습니다. 없애려면 클러스터를 `NETWORK_POLICY_ENFORCING_MODE=strict`로 바꿔야 합니다(infra #60, #61). GKE와 k3s는 아직 확인하지 않았습니다.

---

## v24 · 실제 EKS 리허설 수정

[PR #28](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/28) · 11/5 14:21 · 5파일 +181 −6

**무엇이 달라졌나**

실제 EKS(GKE→EKS)에서 리허설이 임시 원본 단계에서 실패하던 원인 4가지를 고쳤습니다.

| 증상 | 원인 → 수정 |
| --- | --- |
| 임시 원본 PVC가 Pending, 6분 뒤 실패 | 워크로드 EKS에 기본 StorageClass가 없음 → `TempStorageClass`가 쓸 수 있는 클래스를 고름(Retain은 디스크가 남아 고르지 않음) |
| healthy 직후 첫 접속 실패(간헐) | 첫 접속을 3초 간격으로 60초까지 다시 시도 |
| 엔진 precheck에서 `tls: failed to verify certificate` | 터널(127.0.0.1)로 붙을 때 필요한 `tls-server-name`이 샌드박스 토큰 kubeconfig에서 빠졌음 → 유지 |
| `password authentication failed` / `User app has no password assigned`(간헐) | EKS VPC CNI는 서비스 주소(ClusterIP) 기준으로 판정해서 `kubernetes.default`가 막힘 → `ForCNPG`가 그 ClusterIP도 연다 |

**어떻게 확인했나**

- 단위 테스트 통과(`TestTempStorageClass`, `TestTokenKubeconfigKeepsTLSServerName`, `TestForCNPG`에 ClusterIP 검사 추가).
- 실제 EKS에서 `TestClusterRehearsalPasses` 통과: 격리 검사 25초, 임시 원본과 5,000행 34초, 엔진 9단계 69초(쓰기 중단 16.9초), 전체 128초. PR 본문의 기록입니다.

**남은 일**

- standard 모드에서 새 Pod의 첫 몇 초가 열려 있는 문제의 근본 해결은 인프라의 strict 모드입니다(v23과 같음).

---

## v25 · AI 계획 파일 저장

[PR #29](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/29) · 11/5 14:21 · 2파일 +51 −0

**무엇이 달라졌나**

`hopai run -out <리포트>`일 때 플랫폼에 제출한 AI 계획(Plan, 대상 매니페스트, 근거)을 `<리포트>-proposal.json`으로 함께 저장합니다. 플랫폼에 첨부를 내려받는 API가 없어서, 리허설이 실패하거나 멈추면 AI가 무엇을 만들었는지 볼 방법이 없었습니다. 비밀값은 가리고 0600 권한으로 씁니다.

**어떻게 확인했나**

- `TestSaveProposal`: 파일 이름, DSN 비밀번호 마스킹, 매니페스트 보존을 확인했습니다. 전체 테스트 통과.

---

## v26 · 들여온 오픈소스 앱 배포 준비

[PR #33](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/33) · 11/5 17:58 · 12파일 +3,645 −8 · 패키지 `llm`, `cmd/hopai` · platform ADR 0023(푸시 토큰)과 짝

**무엇이 달라졌나**

운영자가 들여온 오픈소스 레포(조직의 비공개 사본)를 AI가 플랫폼에 배포할 수 있게 고칩니다. `hopai prepare -repo OWNER/NAME` 한 번으로 다음을 합니다.

1. 플랫폼에서 그 레포에만 쓰는 푸시 토큰을 받습니다. 들여온 레포가 아니면 `scripts/import-repo.sh`로 먼저 들여오라고 알립니다. GitHub에서 이름이 바뀐 레포면 지금 이름으로 클론합니다.
2. 레포를 임시 디렉터리에 클론합니다. `hopcharlie` 브랜치가 있으면 그 위에서 이어서 고칩니다. 300 MB가 넘는 레포는 다루지 않습니다. 크기는 받기 전에 GitHub API로 보고, 받은 뒤에도 다시 봅니다.
3. `-deployment`를 주면 지난 배포의 오류 줄을 비밀값을 가리고 줄여서 AI에 보여 줍니다. 그 배포가 이 레포의 앱 것인지 확인하지 못하면 멈춥니다.
4. AI가 클론 안의 파일만 읽고 써서 `platform.yaml`과 Dockerfile을 만듭니다. 명령은 실행하지 않습니다. 지시문에 플랫폼의 `platform.yaml` 규칙과 실행 환경(root 아님, 1024 이상 포트, 볼륨 없음, 메모리 256Mi 등)을 넣었습니다.
5. 플랫폼 서버와 같은 코드로 `platform.yaml`을 검사하고, Dockerfile이 있는지, 포트가 1024 이상인지, 실제로 바꾼 파일이 있는지, env나 Dockerfile의 `ENV`, `ARG`에 비밀값을 적지 않았는지 등을 봅니다. 걸리면 걸린 이유를 알려 주고 다시 묻습니다(최대 2회).
6. 통과하면 고정 작성자(HopCharlie AI)로 커밋하고, 커밋이 검사한 것과 같은지(내용이 바이트까지 같고, 심볼릭 링크나 서브모듈이 아니고, AI가 바꾼 파일이 빠지지 않았고, `platform.yaml`과 Dockerfile이 그 이름 그대로 있는지) 본 뒤 `hopcharlie` 브랜치에만 push합니다. 강제 push는 하지 않아서, 그사이 누가 push했으면 거절되고 다시 실행하라고 알립니다.
7. 넣어야 할 시크릿(`charlie app secret set`)과 다음 단계를 알려 줍니다. 앱이 없으면 등록 명령(`charlie app add`, 운영자만)을, 이미 이 브랜치를 배포하는 앱이 있으면 push가 자동 재배포를 시작했다고 알립니다.

- 레포는 믿을 수 없는 데이터로 다룹니다. 파일 도구는 경로마다 심볼릭 링크를 풀어서 클론 밖이나 `.git` 아래를 가리키면 막고 '막힘'으로 기록합니다. git과 GitHub이 설정이나 실행할 것으로 읽는 파일(`.gitattributes`, `.lfsconfig`, `.gitmodules`, `.github/workflows`, Windows 짧은 이름 `GITATT~1` 같은 것 포함)도, git이 그대로 커밋하지 않는 곳(심볼릭 링크인 파일, 서브모듈 안, 대소문자만 다른 이름)도 AI가 쓰지 못합니다.
- git은 운영자 설정 없이 돌립니다. 시스템, 전역 설정과 운영자 홈(`.netrc` 등)을 읽지 않고, 임시 디렉터리를 둘러싼 레포의 설정도 읽지 않고, 환경 변수는 허용 목록만 넘기고, 명령마다 훅, git-lfs 필터, fsmonitor, 심볼릭 링크를 끄고 https만 허용합니다. 리뷰에서 Git for Windows의 기본 git-lfs 설정과 레포의 `.gitattributes`, `.lfsconfig`로 토큰이 공격자 주소에 가는 것을 재현해서 막았습니다.
- 푸시 토큰은 git 자식 프로세스의 환경 변수로만 넘기고, `https://github.com`에만 답하는 자격 증명 도우미로만 git에 줍니다. 명령줄, 원격 주소, 다른 호스트, 기록, AI 대화, 출력에는 들어가지 않고 어디에도 저장되지 않습니다.

**어떻게 확인했나**

가짜 AI, 가짜 플랫폼, 로컬 git 원격으로 테스트 32개를 넣었고 모두 통과했습니다. 테스트는 개발자 컴퓨터의 git 설정을 읽지 않게 돌립니다.

| 경우 | 결과 |
| --- | --- |
| 레포 밖, 절대 경로, `.git`, 양방향 글자, `.gitattributes` 같은 git 설정 파일, 크기 상한 등 23가지, 링크로 빠져나가기 8가지, 41번째 쓰기 | 모두 막힘, 레포 밖 파일은 그대로 |
| 규칙 위반, Dockerfile 없음, 1024 미만 포트, 바꾼 파일 없음, env와 Dockerfile `ENV`의 비밀값 등 11가지 | 세 번 묻고 실패, 커밋 없음 |
| 처음 실행 / 다시 실행 | `hopcharlie`를 새로 만들어 커밋 1개 / 그 위에 fast-forward, main은 그대로 |
| AI가 일하는 사이 다른 push | push 거절, 남의 커밋 그대로 |
| git-lfs를 쓰게 만든 레포 + git-lfs를 켠 운영자 설정 | 공격자 주소로 요청 0번, push한 내용이 검사한 바이트와 같음 |
| 운영자 설정의 `pushInsteadOf`, `insteadOf`(SSH), 훅, 서명, `store` 도우미 | 모두 무시, 원래 레포에 push |
| 커밋된 내용이 검사한 바이트와 다르거나 검사하지 않은 파일이 들어감 | push하지 않음 |
| `platform.yaml`이 심볼릭 링크로 커밋됨, 서브모듈 안에 써서 커밋에 빠짐, 대문자 `PLATFORM.YAML`로 커밋됨 | push하지 않음. 서브모듈 안과 링크인 파일에는 처음부터 쓰지 못함 |
| 운영자 홈의 `.netrc`, 임시 디렉터리를 둘러싼 레포의 `insteadOf` | 쓰이지 않음 |
| GitHub API가 레포를 300 MB보다 크다고 함 / API를 읽지 못함 | 클론하지 않고 멈춤 / 알리고 클론한 뒤 크기를 봄 |
| `COOKIE_SECURE=true`, `PASSWORD_MIN_LENGTH=8`, `TOKEN_TTL=3600`, `GOOGLE_APPLICATION_CREDENTIALS=/path` | 비밀값으로 보지 않음 (값을 적은 비밀번호, 토큰은 그대로 걸림) |
| 실제 HTTP git 서버(git http-backend) | 토큰이 자격 증명 도우미로만 가고 `.git/config`, 명령줄, `~/.git-credentials`에 없음. 다른 호스트에는 답하지 않음 |
| 토큰 찾기 | 출력, 에이전트 기록, git 명령줄, AI 요청, 플랫폼 요청 어디에도 없음 |

운영자 설정 없이도 실제 GitHub(`git ls-remote https://github.com/octocat/Hello-World.git`)에 닿는 것을 Git for Windows에서 확인했습니다. 실제 Claude와 플랫폼 푸시 토큰 API로는 돌려 보지 못했습니다.

---

## v29 배포할 수 없는 앱 알리기

[PR #36](https://github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/pull/36), 머지 전, 9파일 +493 −49, 패키지 `llm`, `cmd/hopai`, `mask`

**무엇이 달라졌나**

MySQL, Redis, 볼륨 같은 리소스 종류를 늘리려면 eks, gke, 온프레미스 모두에서 만드는 기능과 이전 엔진의 데이터 이전이 함께 있어야 합니다. 그래서 종류를 늘리지 않고, `hopai prepare`가 그런 것이 꼭 필요한 앱을 옮기기 전에 찾아 알리게 했습니다.

1. AI 답에 `deployable`과 `blockers`(`{need, reason}`, 5개까지)가 생겼습니다.
2. 우회할 수 없으면 AI가 `deployable: false`로 답합니다. 예: MySQL이나 MongoDB만 지원함, 남아야 하는 볼륨이 꼭 필요함, HTTP가 아닌 TCP나 UDP 포트를 밖에 열어야 함, SMTP 서버에 붙지 못하면 시작하지 않음. 그러면 파일 검사, 커밋, push를 하지 않고, 플랫폼이 주지 못하는 것과 근거를 출력하고, 에이전트 세션(콘솔)에 `decision` 이벤트와 요약 `배포할 수 없음: MySQL`로 남기고, 종료 코드 3으로 끝납니다.
3. 우회할 수 있으면 우회하고 메모에 적습니다. 캐시로만 쓰는 Redis는 앱의 서비스 하나로(따로 Dockerfile, 1024 이상 port, root 아님, 볼륨 없음), 따로 뜨는 프런트엔드와 백엔드는 공개 프록시 서비스 하나로, 로컬 디스크의 업로드 파일은 `s3` 리소스로 바꿉니다.
4. AI가 정하는 것은 "배포할 수 없음"뿐입니다. `deployable: true`여도 배포할 수 있는지는 지금과 같은 검사가 정하고, 걸리면 지금처럼 다시 묻습니다. `true`인데 `blockers`가 있거나 `false`인데 `blockers`가 없으면 걸립니다.
5. 지시문을 새 `platform.yaml` 계약에 맞췄습니다. `postgres` 리소스가 넣는 `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`, `JDBC_DATABASE_URL`을 앱이 읽는 이름에 `$(이름)`으로 잇습니다(예: `SPRING_DATASOURCE_URL: "$(JDBC_DATABASE_URL)"`). 시작 스크립트로 `DATABASE_URL`을 나누라는 문장은 뺐습니다. S3도 앱이 `AWS_*` 이름만 읽을 때 `$(S3_ACCESS_KEY)`처럼 잇습니다. 요청을 받지 않는 워커는 port를 비웁니다. 고정 256Mi 대신 서비스마다 `memory`(256Mi~1536Mi)와 `cpu`(50m~500m)를 정하고, 서비스마다 (replicas + 1) × 값을 더한 합은 memory 3Gi, cpu 1500m 이하입니다(+1은 롤링 업데이트 중에 하나 더 뜨는 Pod). 값은 platform #69와 같고 `llm/prepare.go`의 상수 블록 한 곳에 두어 지시문과 검사가 같이 씁니다.
6. env와 시크릿 이름에 새 플랫폼 이름 6개를 쓰면 걸립니다. env 값의 `$(PGPASSWORD)` 같은 참조는 비밀값으로 보지 않고, `mask`도 `${VAR}`처럼 그대로 둡니다. 대신 참조는 컨테이너에 실제로 있는 이름(리소스가 있을 때 플랫폼 변수, port가 있을 때 `PORT`, 답의 시크릿, 같은 서비스 env에서 이름순으로 앞선 키)만 가리킬 수 있습니다. 없는 이름은 Kubernetes가 글자 그대로 넣기 때문에, 이렇게 막지 않으면 `$(hunter2)`처럼 비밀값을 감싸 커밋할 수 있었습니다(독립 리뷰에서 찾음).

**어떻게 확인했나**

가짜 AI, 가짜 플랫폼, 로컬 git 원격으로 확인했습니다.

| 경우 | 결과 |
| --- | --- |
| AI가 MySQL과 TCP 25 수신을 이유로 `deployable: false` (쓰다 만 `platform.yaml`, 전에 만든 `hopcharlie` 브랜치 있음) | 다시 묻지 않음, 커밋과 push 없음, `hopcharlie`는 그대로, 이유 출력, 플랫폼 세션 기록에 `need`와 요약, 종료 코드 3 |
| docker-doom처럼 서비스 하나짜리 앱에 `deployable: true` | 지금처럼 커밋하고 push, 종료 코드 0 |
| `false`인데 `blockers` 없음, `deployable` 키 없음, `true`인데 `blockers` 있음, 여러 줄 `need`, env `PGHOST`, 시크릿 `PGPASSWORD` | 세 번 묻고 실패 |
| env의 없는 참조(`$(ZX8KQ2LMN4PR7ST9VW1Y)`), postgres 없는 `$(PGHOST)`, 이름순으로 뒤에 오는 키 참조 | 세 번 묻고 실패 |
| postgres 변수, `$(PORT)`, 답의 시크릿, 앞선 키를 가리키는 참조, `postgresql://$(PGUSER):$(PGPASSWORD)@...` | 통과 |
| env `KC_DB_PASSWORD: "$(PGPASSWORD)"`, `jdbc:postgresql://$(PGHOST):$(PGPORT)/$(PGDATABASE)` | 비밀값으로 보지 않음 (`$(PGPASSWORD)hunter2`, `$(hunter2)`는 걸림) |
| `memory`, `cpu` 범위 밖, 수량이 아닌 값(`512MB`), 앱 전체 합 초과(1536Mi replicas 2, 비운 memory 서비스 5개 replicas 2) | 걸림. 1536Mi replicas 1(딱 3Gi), 500m replicas 2(딱 1500m)는 통과 |

`go vet`, gofmt, `go test ./...`가 통과했습니다. 실제 Claude와 플랫폼으로는 돌려 보지 못했습니다.

**남은 일**

- 고정한 platform 모듈(`d138f93`)의 `appspec`은 아직 port를 필수로 보고 `memory`, `cpu`를 모르는 키로 거절합니다. platform #69가 머지되면 이 PR에 모듈을 올리는 커밋을 더합니다. 그 전에는 port 없는 워커와 자원 값이 hopai 검사를 통과하지 못합니다.

---

## 11/3 kind 재확인

v8과 v11은 격리 검사를 바꿨지만, 머지할 때 실제 클러스터에서 돌려 보지 못했습니다. 그래서 v12 코드로 로컬 kind 클러스터에서 리허설을 다시 돌렸습니다.

| 경우 | 결과 |
| --- | --- |
| 대상 저장소 종류가 맞을 때 (`standard`) | 격리 검사 10/10 → 엔진 9단계 모두 성공. 쓰기 보류 9.1초, 전체 82초 |
| 대상에 없는 저장소 종류를 쓸 때 (`gp3`) | 55초 만에 멈춤. 근거 `storageclass "gp3" not found`. 원본은 그대로 |

- v8에서 막은 '노드를 직접 쓰는 Pod'는 Pod Security가 실제로 막았습니다. 이 제한 아래에서도 DB Pod는 정상으로 떴습니다.
- v11에서 바꾼 확인 대상(CNPG 웹훅)은 이름으로 찾은 뒤 막힌 것을 확인했습니다.
- 운영 DB 접속 검사는 이 테스트에 대상 주소를 넣지 않아서 빠졌습니다. `hopai`에서는 `-target`으로 넣습니다.
