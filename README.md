# agents-manager (`am`)

`agents-manager` (CLI 명령: `am`)는 macOS 환경에서 사용 중인 다양한 AI 에이전트들의 대화 세션 기록을 한데 모아 조회하고 검색할 수 있는 Go 기반의 초경량 CLI 도구입니다.

외부 의존성(External Dependencies) 없이 오직 Go 표준 라이브러리만으로 빌드되었으며, 각 에이전트의 로컬 로그 파일을 직접 분석하여 실행되므로 별도의 데몬이나 캐싱 없이 빠르고 가볍게 작동합니다.

---

## 🚀 주요 기능

### 1. 다양한 AI 에이전트 로그 수집
각 에이전트가 디스크 상에 남기는 대화 이력 로그를 스캔하여 세션 목록을 만듭니다.
* **codex**: `~/.codex/session_index.jsonl` 및 개별 세션 롤아웃 로그 분석 (프로젝트 CWD 및 첫 유저 메시지 복구)
* **claude code**: `~/.claude/projects/*/*.jsonl` 프로젝트별 로그 분석 (세션 최종 업데이트 시각 및 CWD 복구)
* **cursor-agent**: `~/.cursor/projects/<projectDir>/agent-transcripts/` 로그 분석 (제목에서 `<user_query>` 태그 필터링 제공)
* **gemini**: `~/.gemini/tmp/*/chats/session-*.json` 세션 데이터 및 `.project_root` 파일 파싱

### 2. 세션 리스트 조회 및 병합 정렬
* 기본적으로 각 프로바이더(에이전트)별로 그룹화된 테이블 형태로 세션들을 조회합니다.
* 디렉토리 경로 필터(`am sessions <dir>`)를 주면, 해당 경로에서 작업한 모든 에이전트의 세션을 하나로 병합하여 가장 최근 순서로 정렬된 단일 테이블을 보여줍니다.

### 3. 세션 본문 텍스트 검색 (`am search`)
* 모든 에이전트 세션의 대화 본문(트랜스크립트), 제목, 프로젝트 루트 전체를 대상으로 키워드 검색을 수행합니다.
* 검색 결과는 일치하는 대화 문맥의 스니펫을 포함하여 한 줄씩 일목요연하게 표시됩니다.

### 4. 대화형 TUI (터미널 사용자 인터페이스)
* TTY 환경에서 세션 목록을 조회할 때, 화면 하단에 최대 10개의 세션을 노출하는 대화형 TUI 모드가 기본으로 실행됩니다.
* 키보드 `↑/↓` (또는 `k/j`) 키로 세션을 탐색하고 `Enter`를 눌러 선택된 세션 ID를 표준 출력(stdout)으로 출력할 수 있습니다.

### 5. Telegram 원격 브리지 (`am remote`)
* `codex app-server --stdio`를 자식 프로세스로 실행하고 Telegram Bot API를 통해 메시지를 주고받습니다.
* 현재는 Telegram 토큰을 직접 전달하는 방식만 지원합니다.

---

## 🛠 설치 및 빌드 방법

### Go 툴체인을 이용한 직접 설치
Go가 설치되어 있다면 아래 명령어로 즉시 바이너리를 빌드하여 시스템 전역에서 사용할 수 있도록 설치할 수 있습니다.
```bash
go install github.com/eunsung-han/agents-manager@latest
```

### 소스코드에서 수동 빌드
저장소를 로컬에 클론한 뒤 직접 빌드할 수 있습니다.
```bash
git clone https://github.com/eunsung-han/agents-manager.git
cd agents-manager
go build -o am am.go
```

---

## 📖 CLI 명령어 명세

### 세션 이력 조회 (`am sessions`)
```bash
# 전체 세션 최근 20개 조회 (프로바이더별 정렬)
am sessions

# 특정 프로젝트 디렉토리 기준 병합 조회
am sessions /Users/eunsung/dev/my-project

# 옵션 조합 (TUI 비활성화, 출력 개수 제한 등)
am sessions --no-tui --limit 10
```

### 대화 내용 검색 (`am search`)
```bash
# 키워드 검색 (기본 대소문자 구분 없음)
am search "오류 메시지"

# 대소문자 구분 검색
am search "NullPointerException" --case-sensitive
```

### Telegram 원격 브리지 (`am remote`)
```bash
am remote --telegram <bot-token>
```

### 지원하는 글로벌 옵션
* `--provider P`: 특정 프로바이더만 조회 (예: `--provider gemini --provider codex`)
* `--limit N`: 각 프로바이더별 또는 병합 조회 시 노출할 최대 세션 개수 (기본값: 20)
* `--json`: 결과를 JSON 포맷으로 출력 (외부 스크립트 연동 시 유용)
* `--debug`: 스캔 및 파싱 과정의 디버그 로그 출력

### `remote` 옵션
* `--telegram <token>`: Telegram Bot API 토큰을 사용해 Codex app-server와 Telegram 메시지를 연결합니다.
