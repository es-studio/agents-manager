# agents-manager (`am`)

[English](README.md) | [한국어](README.ko.md)

`agents-manager` (CLI 명령: `am`)는 macOS 환경에서 사용 중인 다양한 AI 에이전트들의 대화 세션 기록을 한데 모아 조회하고 검색할 수 있는 Go 기반의 초경량 CLI 도구입니다.

외부 의존성(External Dependencies) 없이 오직 Go 표준 라이브러리만으로 빌드되었으며, 각 에이전트의 로컬 로그 파일을 직접 분석하여 실행되므로 별도의 데몬이나 캐싱 없이 빠르고 가볍게 작동합니다.

---

## 주요 기능

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

### 5. Telegram 원격 브리지 (실험적 기능) (`am remote`)
* `codex app-server --stdio`를 자식 프로세스로 실행하고 Telegram Bot API를 통해 메시지를 주고받습니다.
* *주의: 현재 실험적 기능으로 제공됩니다.*

---

## 설치 및 빌드 방법

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

## CLI 명령어 명세

> [!TIP]
> **명령어 단축 및 퍼지(Fuzzy) 매칭**
> 모든 명령어는 접두사(prefix) 및 부분 시퀀스(subsequence) 퍼지 매칭을 지원하므로 전체 명령어를 다 입력하지 않고도 빠르게 실행할 수 있습니다.
> * `am l codex` 또는 `am lch codex` → `am launch codex`
> * `am ses` → `am sessions`
> * `am sea <키워드>` → `am search <키워드>`
> * `am hi` → `am history`
> * `am rem` → `am remote`
> *(단, `am s`처럼 여러 명령어(`sessions`, `search`)와 일치하여 모호한 경우 후보 목록이 출력됩니다)*

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

독립된 tmux 세션에서 Codex를 실행하고 하단에 모델, 토큰 사용량, 잔여 컨텍스트, 쿼터 한도 등을 실시간으로 표시하는 커스텀 상태줄을 제공합니다.

**상태줄 표시 예시 (기본값)**:
```text
gpt-5-codex (High) · my-project · in:48.2k / out:6.1k · ctx:79.8% · tasks:1 · 5h:82% (2h 15m) · 7d:59% (3d 4h) · v0.161.0
```
* `gpt-5-codex (High)`: 모델 및 추론 수준
* `my-project`: 작업 디렉토리
* `in:48.2k / out:6.1k`: 누적 입력/출력 토큰
* `ctx:79.8%`: 남은 컨텍스트 비율
* `tasks:1`: 진행 중인 작업 수
* `5h:82% (2h 15m)` / `7d:59% (3d 4h)`: 5시간 / 주간 잔여 쿼터 및 리셋 카운트다운
* `v0.161.0`: Codex CLI 버전

* **요구사항**: `tmux`, `lsof`, `--no-daemon`을 지원하는 Codex CLI
* **화면 제어**: `Ctrl-b d`로 Codex 종료 없이 분리(detach), `Ctrl-b [`로 스크롤 모드, 마우스 휠 스크롤 지원
* **상태줄 설정 (`am launch config`)**: 대화형 TUI에서 상태줄 항목(토큰, 한도, git 브랜치 등)을 켜고 끄거나 순서를 변경할 수 있으며, `f` 키로 fast 모드를 켤 수 있습니다.

### Telegram 원격 브리지 (실험적 기능) (`am remote`)
```bash
am remote --telegram <bot-token>
```
*주의: 이 기능은 실험적 기능입니다.*

### 지원하는 글로벌 옵션
* `--provider P`: 특정 프로바이더만 조회 (예: `--provider claude --provider codex`)
* `-n, --limit N`: 각 프로바이더별 또는 병합 조회 시 노출할 최대 세션 개수 (기본값: 20)
* `-p, --period D`: 최근 D일 동안의 세션만 필터링 (기본값: 7일)
* `--all`: 개수 제한(-n)과 기간 제한(-p)을 모두 무시하고 전체 표시
* `--json`: 결과를 JSON 포맷으로 출력 (외부 스크립트 연동 시 유용)
* `--debug`: 스캔 및 파싱 과정의 디버그 로그 출력

### `remote` 옵션 (실험적 기능)
* `--telegram <token>`: Telegram Bot API 토큰을 사용해 Codex app-server와 Telegram 메시지를 연결합니다.
