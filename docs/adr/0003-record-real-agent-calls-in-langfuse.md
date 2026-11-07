# 0003. 실제 LLM·도구 실행 경로에 전용 Langfuse 기록을 붙인다

- 상태: 채택 — 계획·구체적 실행 범위 승인. 실제 image/Job/trace 검증 결과는 배포 기록과 구분한다.
- 날짜: 2026-11-08
- 관련 결정: 0001 서버 토폴로지 권한 보존, 0002 CI Linux 바이너리 발행

## 배경과 제약

Langfuse 서버의 합성 수집 경로는 확인됐지만 hopai의 실제 요청은 전송되지 않았다. 기존 llm.run, loggedTool.Execute, Anthropic SDK v1.84 middleware를 사용한다. 플랫폼 agentlog와 Writer, 전역 OTel 설정과 기존 업무 동작은 유지해야 한다.

## 결정

전용 OTel v1.33 provider와 OTLP/HTTP exporter를 환경 설정으로 활성화한다. unset이면 disabled이며 부분 설정은 값 없는 경고와 disabled로 처리한다. AGENT root 아래 SDK HTTP 시도를 GENERATION, 실제 실행 도구를 TOOL로 기록한다. 실제 응답 model·usage를 사용하고 platform sessionID·JobID로 연결한다.

middleware는 SDK/transport의 기존 body 읽기를 관찰하는 bounded ReadCloser를 사용한다. 요청·응답 바이트/Close/error를 그대로 전달하고 추가 읽기·재전송을 하지 않는다. 입력·출력은 기존 mask와 알려진 키 제거를 통과하며 최대64KiB다. 잘린 JSON은 IO를 생략하고 truncated를 남긴다. header·env전체·kubeconfig 파일은 수집하지 않는다. 마스킹이 완전한 비식별화를 보장하는 것은 아니다.

exporter timeout1초·retryoff·root종료flush2초다. 실패가 AI·도구의 return/error를 바꾸지 않는 fail-open 방식이다. 업무 error/프로세스 os.Exit 전 llm.run 내부 defer에서 전송한다. 강제 종료·네트워크 장애 중 유실은 가능하다.

실제 모델 검증은 별도 migration/온보딩을 실행하지 않는 hopai telemetry-check로 한다. 같은 llm.run/SDK/tool wrapper를 사용하고 메모리 읽기 도구만 제공한다. HTTP4회·각1024출력토큰·180초 상한이다. 모델이 도구를 호출하지 않으면 실환경 TOOL 검증으로 보고하지 않는다. Config.MaxTokens의 기본값은 기존16000을 유지한다.

## 대안과 후속 작업

- 로그를 변환하기만 하면 실제 HTTP input/time·완전한 응답을 확보하기 어려워 제외했다.
- 전역 exporter 변경은 기존 ClickStack 경로에 영향을 주므로 제외했다.
- CI에서 private 의존성·포맷·vet·단위검사를 수행하고, ADR0002의 검증 Linux artifact로 기존 image의 hopai 바이너리만 교체한다. 노트북 전체 컴파일/DB 컨테이너 통합시험은 하지 않는다.
- infra가 Job에 기존 전용 프로젝트키 Secretrefs와 네트워크를 제공한다. IAM/SSM 정책이나 새 장기키는 추가하지 않는다.
- rollback은 Langfuse 설정 비활성화/이전 image다. 기존 플랫폼 세션과 실행은 유지한다.
