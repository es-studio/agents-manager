# Codex app-server 도입 메모

이 문서는 `agents-manager`(`am`)에서 Codex app-server를 사용할 때 필요한 공식 문서 기반 요약과 1차 도입 방향을 정리한다.

공식 문서 기준:

- Codex App Server: <https://developers.openai.com/codex/app-server>
- CLI reference: <https://developers.openai.com/codex/cli/reference#codex-app-server>
- Open-source implementation: <https://github.com/openai/codex/tree/main/codex-rs/app-server>

## 결론

`codex app-server`는 Codex의 rich client를 만들기 위한 로컬 프로토콜 서버다. 단순한 배치 자동화나 CI 실행에는 Codex SDK가 더 적합하고, `am`처럼 로컬 세션/대화/승인/스트리밍 이벤트를 제품 기능으로 통합하려는 경우에 app-server를 검토할 가치가 있다.

`am`의 1차 사용처는 "Codex 세션 목록을 디스크에서 읽기"를 대체하는 것이 아니라, 다음 기능을 app-server 기반으로 추가하는 쪽이 적절하다.

- 새 Codex thread 시작
- 기존 thread resume/fork/archive 같은 상태 조작
- turn 실행 후 streamed event를 받아 CLI/TUI에 표시
- 승인 요청이나 tool 진행 상태를 사용자에게 중계

기존 로그 스캔 방식은 캐시나 서버 상태에 의존하지 않는 장점이 있으므로 유지한다.

## 언제 쓰는가

공식 문서는 app-server를 다음 목적에 맞는 인터페이스로 설명한다.

- 자체 제품 안에 Codex를 깊게 통합한다.
- 인증, conversation history, approvals, streamed agent events를 다룬다.
- VS Code extension 같은 rich client를 구현한다.

반대로 자동화 작업이나 CI에서 Codex를 실행하는 목적이면 app-server가 아니라 Codex SDK를 우선 검토한다.

## 프로토콜 개요

app-server는 JSON-RPC 2.0 스타일의 양방향 메시지를 사용한다. 단, wire format에서는 `"jsonrpc":"2.0"` 필드를 생략한다.

요청:

```json
{ "method": "thread/start", "id": 10, "params": { "model": "gpt-5.4" } }
```

응답:

```json
{ "id": 10, "result": { "thread": { "id": "thr_123" } } }
```

알림(notification):

```json
{ "method": "turn/started", "params": { "turn": { "id": "turn_456" } } }
```

핵심 객체:

- Thread: 사용자와 Codex agent 사이의 대화 단위
- Turn: thread 안에서 한 번의 사용자 요청과 그에 따른 agent 작업
- Item: user message, agent message, command run, file change, tool call 같은 입출력 단위

## Transport

지원 transport:

- `stdio://`: 기본값. newline-delimited JSON(JSONL).
- `ws://IP:PORT`: WebSocket. 공식 문서상 experimental/unsupported.
- `unix://`: Codex 기본 app-server control socket에 WebSocket handshake로 연결.
- `unix://PATH`: 지정한 Unix socket 경로 사용.
- `off`: 로컬 transport 비활성화.

1차 구현은 `stdio://`가 가장 단순하다. `am`이 `codex app-server` 프로세스를 직접 spawn하고 stdin/stdout으로 JSONL을 주고받으면 된다. 별도 포트 관리나 auth가 필요 없고, 로컬 CLI 도구라는 `am`의 성격에도 맞는다.

WebSocket은 원격 또는 별도 프로세스 클라이언트가 필요할 때만 사용한다. `ws://127.0.0.1:PORT`는 localhost/SSH port-forwarding 용도로만 두고, non-loopback listener를 열 때는 반드시 auth를 설정한다.

## 실행 명령

기본 stdio:

```bash
codex app-server
```

명시적 stdio:

```bash
codex app-server --listen stdio://
codex app-server --stdio
```

localhost WebSocket:

```bash
codex app-server --listen ws://127.0.0.1:4500
```

Unix socket:

```bash
codex app-server --listen unix://
codex app-server --listen unix:///absolute/path.sock
```

스키마 생성:

```bash
codex app-server generate-ts --out ./schemas
codex app-server generate-json-schema --out ./schemas
```

스키마는 실행한 Codex 버전에 맞춰 생성되므로, `am`에서 타입/검증을 자동 생성한다면 생성물의 Codex 버전을 함께 기록해야 한다.

## 연결 lifecycle

클라이언트는 transport 연결 직후 한 번만 초기화해야 한다.

1. `initialize` 요청 전송
2. `initialized` notification 전송
3. `thread/start`, `thread/resume`, `thread/fork` 중 하나로 thread 준비
4. `turn/start`로 사용자 입력 실행
5. `item/started`, `item/completed`, `item/agentMessage/delta`, tool progress 같은 notification stream 처리
6. `turn/completed` 또는 `turn/interrupt` 결과 처리

`initialize` 전에 다른 요청을 보내면 `Not initialized` 오류가 나고, 같은 연결에서 `initialize`를 반복하면 `Already initialized` 오류가 난다.

예시 초기화:

```json
{
  "method": "initialize",
  "id": 0,
  "params": {
    "clientInfo": {
      "name": "agents_manager",
      "title": "agents-manager",
      "version": "0.1.0"
    }
  }
}
```

`clientInfo.name`은 OpenAI Compliance Logs Platform에서 클라이언트를 식별하는 값으로 사용된다. enterprise 용도로 배포할 통합이라면 공식 문서 안내처럼 OpenAI에 known client 등록을 문의해야 한다.

## Experimental API

일부 method와 field는 `experimentalApi` capability가 있어야 사용할 수 있다.

```json
{
  "method": "initialize",
  "id": 1,
  "params": {
    "clientInfo": {
      "name": "agents_manager",
      "title": "agents-manager",
      "version": "0.1.0"
    },
    "capabilities": {
      "experimentalApi": true
    }
  }
}
```

원칙:

- 기본 구현은 stable API만 사용한다.
- experimental 기능이 꼭 필요할 때만 명시적으로 opt-in한다.
- opt-in이 필요한 기능은 코드와 문서에서 별도 표시한다.

## WebSocket 보안 주의

WebSocket transport는 experimental/unsupported다. 특히 non-loopback listener는 rollout 중 기본적으로 unauthenticated connection을 허용할 수 있으므로, 외부에 노출할 때는 반드시 auth를 설정한다.

지원 auth:

```bash
codex app-server \
  --listen ws://0.0.0.0:4500 \
  --ws-auth capability-token \
  --ws-token-file /absolute/path/to/token
```

또는:

```bash
codex app-server \
  --listen ws://0.0.0.0:4500 \
  --ws-auth capability-token \
  --ws-token-sha256 HEX
```

또는 signed bearer token:

```bash
codex app-server \
  --listen ws://0.0.0.0:4500 \
  --ws-auth signed-bearer-token \
  --ws-shared-secret-file /absolute/path/to/secret
```

원칙:

- raw bearer token을 command line argument로 넘기지 않는다.
- 가능하면 `--ws-token-file`을 사용한다.
- `--ws-token-sha256`은 client가 원본 high-entropy token을 별도 secret store에 보관할 때만 사용한다.
- `wss://` 연결은 app-server가 직접 받는 것이 아니라 TLS termination 또는 secure proxy를 앞에 둔다.

## Backpressure와 retry

WebSocket mode에서는 bounded queue를 사용한다. ingress queue가 꽉 차면 JSON-RPC error code `-32001`과 `"Server overloaded; retry later."` 메시지로 요청이 거절될 수 있다.

클라이언트는 이 오류를 transient error로 보고 exponential backoff와 jitter를 적용해 재시도해야 한다.

## 현재 로컬 확인

2026-06-21 기준 이 repo에서 확인한 로컬 Codex CLI는 `codex-cli 0.141.0`이고, `codex app-server --help`에서 다음 command가 노출된다.

- `daemon`
- `proxy`
- `generate-ts`
- `generate-json-schema`
- `help`

문서화된 기본 transport 옵션(`stdio://`, `unix://`, `unix://PATH`, `ws://IP:PORT`, `off`)과 WebSocket auth 옵션도 현재 help에 표시된다.

## `am` 도입 방향

### 유지할 것

현재 `am sessions`와 `am search`의 기본값은 계속 로컬 로그 파일 스캔으로 둔다.

이유:

- codex/claude/cursor/gemini/antigravity를 같은 방식으로 병합할 수 있다.
- 서버 실행 여부와 무관하게 동작한다.
- 과거 세션 조회와 검색은 app-server보다 파일 스캔이 더 단순하고 견고하다.

### 추가 후보

app-server 기반 기능은 별도 subcommand로 추가하는 것이 안전하다.

```bash
am codex start [--cwd DIR] [--message TEXT]
am codex resume <thread-id> [--message TEXT]
am codex fork <thread-id>
am codex archive <thread-id>
```

초기 구현 범위:

- stdio transport만 지원
- `initialize`/`initialized` handshake 구현
- `thread/start`
- `turn/start`
- notification stream 출력
- `turn/completed`까지 대기

나중에 검토할 범위:

- WebSocket remote endpoint 연결
- approval request를 TUI에서 처리
- generated JSON Schema 기반 message validation
- `am sessions <dir>` TUI에서 선택한 Codex session을 app-server로 resume

## 구현 체크리스트

1. 현재 설치된 Codex CLI가 `codex app-server`를 지원하는지 확인한다.
2. `codex app-server generate-json-schema --out ./schemas/codex-app-server`로 현재 버전 스키마를 생성한다.
3. Go에서 stdio child process wrapper를 만든다.
4. JSON-RPC id allocator와 pending response map을 구현한다.
5. notification handler를 별도 channel로 분리한다.
6. `initialize` 전 요청 금지, repeated initialize 금지 케이스를 테스트한다.
7. `thread/start` -> `turn/start` -> `turn/completed` happy path를 테스트한다.
8. process 종료, malformed JSONL, response error, interrupted turn을 처리한다.

## 주의할 점

- app-server는 공식 문서상 development/debugging 성격이 강하고 변경 가능성이 있다.
- `codex app-server` CLI 옵션과 schema는 Codex 버전에 묶인다.
- approval/sandbox 정책은 app-server를 써도 사라지지 않는다. 클라이언트는 승인 요청을 사용자에게 보여주거나 실패로 처리할 수 있어야 한다.
- `CODEX_HOME`을 바꾸면 config, auth, logs, sessions, skills, package state의 루트가 바뀐다.
- `CODEX_SQLITE_HOME`은 CLI/app-server state 위치에 영향을 준다.
