# agents-manager (`am`)

[🇺🇸 English](README.md) | [🇰🇷 한국어](README.ko.md)
`agents-manager` (CLI 명령: `am`)는 macOS 환경에서 사용 중인 다양한 AI 에이전트들의 대화 세션 기록을 한데 모아 조회하고 검색할 수 있는 Go 기반의 초경량 CLI 도구입니다.

외부 의존성(External Dependencies) 없이 오직 Go 표준 라이브러리만으로 빌드되었으며, 각 에이전트의 로컬 로그 파일을 직접 분석하여 실행되므로 별도의 데몬이나 캐싱 없이 빠르고 가볍게 작동합니다.

---

## 🚀 주요 기능

### 1. 다양한 AI 에이전트 로그 수집
각 에이전트가 디스크 상에 남기는 대화 이력 로그를 스캔하여 세션 목록을 만듭니다.
* **codex**: `~/.codex/session_index.jsonl` 및 개별 세션 롤아웃 로그 분석 (프로젝트 CWD 및 첫 유저 메시지 복구)
* **claude code**: `~/.claude/projects/*/*.jsonl` 프로젝트별 로그 분석 (세션 최종 업데이트 시각 및 CWD 복구)
* **cursor-agent**: `~/.cursor/projects/<projectDir>/agent-transcripts/` 로그 분석 (제목에서 `<user_query>` 태그 필터링 제공)

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
go install github.com/es-studio/agents-manager@latest
```

### 소스코드에서 수동 빌드
저장소를 로컬에 클론한 뒤 직접 빌드할 수 있습니다.
```bash
git clone https://github.com/es-studio/agents-manager.git
cd agents-manager
go build -o am .
```

---

## 📖 CLI 명령어 명세

### 세션 이력 조회 (`am sessions` / `am history`)
```bash
# 전체 세션 최근 20개 조회 (am history는 am sessions의 별칭)
am history

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

### 커스텀 상태줄로 Codex 실행 (`am launch codex`)
```bash
am launch codex
am launch codex --model <model>
am launch codex resume --last
am launch config
```

`tmux`, `lsof`, `--no-daemon`을 지원하는 Codex CLI가 필요합니다(0.161.0 검증). 실행마다 별도 tmux 서버를 만들고, 해당 실행에서만 Codex의 설정된 상태줄 항목을 끕니다. Codex를 종료하면 추가로 연 창까지 전용 서버가 종료되며 다른 tmux 세션에는 영향을 주지 않습니다. `Ctrl-b d`는 Codex를 종료하지 않고 분리합니다. 시작할 때 출력한 명령으로 다시 연결할 수 있습니다. 기존 tmux 안에서는 prefix를 두 번 눌러 내부 tmux에 전달합니다. 마우스 휠 스크롤이 활성화됩니다. `Ctrl-b [`로 tmux 스크롤 모드에 들어가고 `q`로 돌아올 수 있습니다.

하단에는 모델(추론 수준), 폴더, 누적 입력/출력 토큰, 남은 컨텍스트, 진행 중 응답 수(`tasks:0` 또는 `tasks:1`), 5시간/주간 잔여 한도와 초기화까지의 시간, Codex 버전을 표시하며 플랜 식별자는 제외합니다. 제공되지 않는 한도 항목은 숨기고 나머지 미확인 값은 `--`로 표시합니다. 모델은 청록색, 폴더는 보라색, 토큰은 청회색, 버전은 회색입니다. 컨텍스트·한도 잔여량은 25% 초과면 초록색, 25% 이하면 노란색, 10% 이하면 빨간색이며 진행 중 작업은 노란색입니다. 컨텍스트는 로그의 최근 토큰 수/컨텍스트 크기로 계산하므로 Codex 내부 보정 비율과 다를 수 있습니다. 터미널 폭을 넘는 내용은 잘립니다.

`am launch config`는 상태줄 항목을 편집하는 TUI를 엽니다. `space`로 항목을 켜고 끄고, `shift+up/down`(또는 `K`/`J`)으로 순서를 바꾸고, `r`로 기본값을 되돌리고, `enter`로 저장하고, `q`로 취소합니다. 결과 줄을 미리보기로 보여줍니다. 기본 항목 외에 선택할 수 있는 항목은 git 브랜치(커밋하지 않은 변경이 있으면 `*`, 5초마다 갱신), 사용 컨텍스트/창 크기, 캐시 입력 비율, 추론 토큰, 마지막 턴 토큰, 총 토큰, 턴 수, 턴 시간(진행 중이면 경과, 아니면 마지막 턴 소요), 실행 후 경과 시간, 도구 호출 수, 마지막 셸 명령, 승인 정책, 샌드박스 정책(`danger-full-access`는 빨간색), 협업 모드, 성격, 서비스 등급, 짧은 세션 id, 현재 시각입니다. `f`로 fast 모드를 켜고 끕니다. 켜면 `am launch codex`가 전달 인자 앞에 `-c service_tier="fast"`를 붙이므로, 명령줄에 직접 쓴 `-c service_tier=...`가 우선합니다. 끄면 아무것도 붙이지 않고 Codex 설정을 그대로 따릅니다. 설정은 `~/.config/am/launch.json`(또는 `$XDG_CONFIG_HOME/am/launch.json`)에 저장하며 다음 `am launch codex`를 시작할 때 읽습니다. 이미 실행 중인 세션의 상태줄은 바뀌지 않습니다.

실행한 Codex 프로세스 트리가 연 rollout 로그만 1초마다 읽습니다. 같은 폴더의 다른 세션이나 전역 최신 세션과 혼동하지 않으며 resume도 지원합니다. 상태줄은 Codex 시작 전에 설정합니다. 버전·유효 모델/추론 설정·계정 한도는 임시 Codex app-server로 비동기 조회하며 프롬프트를 보내지 않습니다(8초 제한). CLI의 모델·설정·작업 폴더 옵션을 반영합니다. 토큰·컨텍스트는 실측 전까지 `--`입니다. resume/fork, 프로필, OSS, 원격 실행은 다른 세션 설정을 잘못 표시하지 않도록 모델·한도 초기 조회를 생략하며, 이후 로컬 rollout 값이 우선합니다. 로컬 rollout이 없는 원격 서버 세션에서는 해당 항목을 표시할 수 없습니다.

### Telegram 원격 브리지 (`am remote`)
```bash
am remote --telegram <bot-token>
```

### 지원하는 글로벌 옵션
* `--provider P`: 특정 프로바이더만 조회 (예: `--provider claude --provider codex`)
* `-n, --limit N`: 각 프로바이더별 또는 병합 조회 시 노출할 최대 세션 개수 (기본값: 20)
* `-p, --period D`: 최근 D일 동안의 세션만 필터링 (기본값: 7일)
* `--all`: 개수 제한(-n)과 기간 제한(-p)을 모두 무시하고 전체 표시
* `--json`: 결과를 JSON 포맷으로 출력 (외부 스크립트 연동 시 유용)
* `--debug`: 스캔 및 파싱 과정의 디버그 로그 출력

### `remote` 옵션
* `--telegram <token>`: Telegram Bot API 토큰을 사용해 Codex app-server와 Telegram 메시지를 연결합니다.
