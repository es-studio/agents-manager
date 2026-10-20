# Telegram Bot API 메시지 포맷팅 및 제한 사항 스펙 문서

이 문서는 `agents-manager`(`am`)의 Telegram 원격 모드(`am remote`) 등에서 에이전트와 통신하고 사용자에게 진행 상황을 출력할 때 참고할 Telegram Bot API 메시지 포맷 및 전송 제한 사항을 정리한다.

---

## 1. 메시지 포맷팅 스타일 (HTML vs MarkdownV2)

Telegram Bot API는 메시지 텍스트를 파싱하여 서식을 지정할 수 있도록 `parse_mode` 파라미터를 제공한다. 주요 모드로는 **HTML**과 **MarkdownV2**가 있다.

`agents-manager`(`am`)는 에이전트가 출력하는 CommonMark(Markdown) 형식을 Telegram **HTML**로 변환하여 전송하는 방식을 취하고 있다. 이는 MarkdownV2에 비해 이스케이프해야 하는 특수 문자의 종류가 적어 더 단순하고 안정적으로 작동하기 때문이다.

### A. HTML 모드 (`parse_mode: "HTML"`)
HTML 모드 사용 시 아래 특수 문자들은 반드시 HTML 엔티티로 이스케이프해야 한다. 그렇지 않으면 `400 Bad Request` 에러가 발생하거나 메시지가 잘릴 수 있다.

#### 필수 이스케이프 규칙
| 문자 | HTML 엔티티 |
|:---:|:---|
| `<` | `&lt;` |
| `>` | `&gt;` |
| `&` | `&amp;` |

#### 지원하는 주요 HTML 태그
* **굵게 (Bold):** `<b>굵은 텍스트</b>` 또는 `<strong>굵은 텍스트</strong>`
* **기울임 (Italic):** `<i>기울인 텍스트</i>` 또는 `<em>기울인 텍스트</em>`
* **밑줄 (Underline):** `<u>밑줄 텍스트</u>`
* **취소선 (Strikethrough):** `<s>취소선</s>`, `<strike>취소선</strike>` 또는 `<del>취소선</del>`
* **스포일러 (Spoiler):** `<span class="tg-spoiler">스포일러</span>` 또는 `<tg-spoiler>스포일러</tg-spoiler>`
* **링크 (Inline Link):** `<a href="https://example.com">링크 텍스트</a>`
* **코드 (Inline Code):** `<code>인라인 코드</code>`
* **코드 블록 (Pre-formatted block):** `<pre>코드 블록 내용</pre>` 또는 언어 지정 포함 `<pre><code class="language-python"># python code...</code></pre>`
* **인용 블록 (Blockquote):** `<blockquote>인용구</blockquote>`

---

### B. MarkdownV2 모드 (`parse_mode: "MarkdownV2"`)
MarkdownV2 모드는 Telegram에서 권장하는 확장 마크다운 문법이지만, 매우 엄격한 이스케이프 규칙을 적용한다.

#### 필수 이스케이프 규칙
텍스트 내에 서식 지정용 문자가 아닌 일반 텍스트로 표시되어야 하는 아래의 문자들은 반드시 백슬래시(`\`)로 이스케이프해야 한다.
`_`, `*`, `[`, `]`, `(`, `)`, `~`, `` ` ``, `>`, `#`, `+`, `-`, `=`, `|`, `{`, `}`, `.`, `!`

#### 주요 MarkdownV2 문법
* **굵게:** `*텍스트*`
* **기울임:** `_텍스트_`
* **밑줄:** `__텍스트__`
* **취소선:** `~텍스트~`
* **스포일러:** `||텍스트||`
* **링크:** `[링크 텍스트](url)`
* **코드:** `` `인라인 코드` ``
* **코드 블록:** 
  ```
  \`\`\`language
  코드 블록 내용
  \`\`\`
  ```

---

## 2. API 전송 제한 및 제약 조건

Telegram Bot API를 통해 메시지를 송신할 때 주의해야 할 글자 수 제한 등 제약 사항이다.

* **텍스트 메시지 본문 (`sendMessage` 등):** 최대 **4,096자** (UTF-8/UTF-16 기준)
* **미디어 캡션 (`caption` 필드):** 최대 **1,024자**
* **메시지 전송 빈도 제한 (Rate Limits):**
  * 동일 대화방(Chat) 대상: 초당 최대 1개 메시지
  * 전체 대화방 대상 (다수 사용자): 초당 최대 30개 메시지
  * 이 제한을 초과하면 `429 Too Many Requests` 에러를 반환받으며, 응답의 `retry_after` 헤더에 명시된 시간(초) 동안 대기해야 한다.

> [!IMPORTANT]
> **글자 수 초과 시 대처 방안**
> 에이전트의 출력이 4,096자를 초과할 가능성이 있는 경우, 메시지를 최대 길이 단위(예: 4,000자 내외)로 나누어 분할 전송하는 로직을 마련해야 한다.
> 또한, HTML 모드에서 태그가 열린 상태에서 메시지가 분할되면 HTML 문법 오류(`400 Bad Request`)가 발생할 수 있으므로, 태그가 닫힌 뒤에 쪼개거나 이스케이프 처리 상태를 신중히 검사해야 한다.

---

## 3. `agents-manager`(`am`)의 메시지 처리 구현체

`am` 프로젝트의 [remote.go](file:///Users/eunsung/dev/agents/remote.go) 파일에서는 CommonMark 형식의 마크다운을 Telegram HTML 형식으로 변환하여 전송한다.

### 구현 상세

1. **정규식 매핑**: [remote.go](file:///Users/eunsung/dev/agents/remote.go#L96-L104)에 정의된 다음 정규식들을 사용하여 마크다운 요소를 매핑한다.
   * `reFencedCode`: `(?s)```[a-zA-Z0-9_+.-]*\n?(.*?)``` -> `<pre>` 태그로 변환 및 코드 내부 문자 이스케이프
   * `reInlineCode`: `` `([^`\n]+)` `` -> `<code>` 태그로 변환 및 코드 내부 문자 이스케이프
   * `reHeader`: `(?m)^[ \t]{0,3}#{1,6}[ \t]+(.*?)[ \t]*#*[ \t]*$` -> `<b>` 태그로 헤더 변환
   * `reBold`: `\*\*(.+?)\*\*` -> `<b>` 태그로 변환
   * `reStrike`: `~~(.+?)~~` -> `<s>` 태그로 변환
   * `reItalic`: `\*([^*\n]+?)\*` -> `<i>` 태그로 변환
   * `reLink`: `\[([^\]]+)\]\(([^)\s]+)\)` -> `<a href="...">` 태그로 변환

2. **안정성 강화 장치**:
   * **코드 블록 보호 (Stashing)**: 마크다운 파싱 중 코드 블록 이내의 마크다운 문자(예: `*`, `_` 등)가 중복 파싱되거나 깨지는 것을 방지하기 위해, 코드 블록을 먼저 특수한 임시 마커 키(예: `\x00index\x00`)로 치환하여 보관(stash)해 두고 일반 텍스트 변환을 완료한 뒤 최종적으로 복원한다.
   * **HTML Fallback**: 만약 잘못 구성된 HTML 엔티티 등으로 인해 Telegram 전송 중 에러가 발생하는 경우, `am`은 포맷팅이 없는 순수 평문(Plain Text)으로 메시지를 재시도(`Fallback`)하여 메시지 전송 자체가 누락되지 않도록 한다.
