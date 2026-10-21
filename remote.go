package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type TelegramUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

type TelegramResponse struct {
	Ok     bool             `json:"ok"`
	Result []TelegramUpdate `json:"result"`
}

type remoteResponseEvent struct {
	kind string
	text string
}

type telegramSender interface {
	sendMessage(chatID int64, text string) error
	sendMessageDraft(chatID int64, draftID int64, text string) error
}

type telegramClient struct {
	token string
	http  *http.Client
}

func newTelegramClient(token string) *telegramClient {
	return &telegramClient{
		token: token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *telegramClient) setMyCommands() error {
	commands := []map[string]string{
		{"command": "start", "description": "Start the remote bridge"},
		{"command": "start_codex", "description": "Start Codex remote session"},
		{"command": "start_agy", "description": "Start Antigravity (agy) remote session"},
		{"command": "start_claude", "description": "Start Claude (ACP) remote session"},
		{"command": "stop", "description": "Stop the current active session"},
	}
	return c.post("setMyCommands", map[string]interface{}{
		"commands": commands,
	})
}

func (c *telegramClient) post(method string, payload map[string]interface{}) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.token, method)
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram %s failed: %s", method, resp.Status)
	}
	return nil
}

// Telegram's own Markdown flavor differs from the CommonMark the agents emit
// (e.g. **bold**, ## headings, ```fences```), so we convert to Telegram HTML,
// which only requires escaping & < > and supports a small, well-defined tag set.
var (
	reFencedCode = regexp.MustCompile("(?s)```[a-zA-Z0-9_+.-]*\\n?(.*?)```")
	reInlineCode = regexp.MustCompile("`([^`\n]+)`")
	reHeader     = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	reBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reStrike     = regexp.MustCompile(`~~(.+?)~~`)
	reItalic     = regexp.MustCompile(`\*([^*\n]+?)\*`)
	reLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
)

func telegramEscapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramMarkdownToHTML converts a useful subset of CommonMark to Telegram
// HTML. Code spans/blocks are stashed first so their contents are never treated
// as markdown. Unbalanced markers (common in streamed drafts) simply stay
// literal, which keeps the output valid HTML.
func telegramMarkdownToHTML(s string) string {
	var keys []string
	var vals []string
	stash := func(html string) string {
		key := fmt.Sprintf("\x00%d\x00", len(keys))
		keys = append(keys, key)
		vals = append(vals, html)
		return key
	}

	s = reFencedCode.ReplaceAllStringFunc(s, func(m string) string {
		sub := reFencedCode.FindStringSubmatch(m)
		return stash("<pre>" + telegramEscapeHTML(sub[1]) + "</pre>")
	})
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		sub := reInlineCode.FindStringSubmatch(m)
		return stash("<code>" + telegramEscapeHTML(sub[1]) + "</code>")
	})

	s = telegramEscapeHTML(s)

	s = reHeader.ReplaceAllString(s, "<b>$1</b>")
	s = reBold.ReplaceAllString(s, "<b>$1</b>")
	s = reStrike.ReplaceAllString(s, "<s>$1</s>")
	s = reItalic.ReplaceAllString(s, "<i>$1</i>")
	s = reLink.ReplaceAllString(s, `<a href="$2">$1</a>`)

	for i, key := range keys {
		s = strings.ReplaceAll(s, key, vals[i])
	}
	return s
}

func (c *telegramClient) sendMessage(chatID int64, text string) error {
	err := c.post("sendMessage", map[string]interface{}{
		"chat_id":    chatID,
		"text":       telegramMarkdownToHTML(text),
		"parse_mode": "HTML",
	})
	if err != nil {
		// Fall back to plain text so a malformed entity never blocks delivery.
		return c.post("sendMessage", map[string]interface{}{
			"chat_id": chatID,
			"text":    text,
		})
	}
	return nil
}

func (c *telegramClient) sendMessageDraft(chatID int64, draftID int64, text string) error {
	if text == "" {
		// Empty text clears the draft; no formatting needed.
		return c.post("sendMessageDraft", map[string]interface{}{
			"chat_id":  chatID,
			"draft_id": draftID,
			"text":     "",
		})
	}
	err := c.post("sendMessageDraft", map[string]interface{}{
		"chat_id":    chatID,
		"draft_id":   draftID,
		"text":       telegramMarkdownToHTML(text),
		"parse_mode": "HTML",
	})
	if err != nil {
		return c.post("sendMessageDraft", map[string]interface{}{
			"chat_id":  chatID,
			"draft_id": draftID,
			"text":     text,
		})
	}
	return nil
}

func (c *telegramClient) fetchUpdates(offset int, timeout int) ([]byte, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d", c.token, offset, timeout)
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func splitTelegramText(text string, maxRunes int) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		n := maxRunes
		if len(runes) < n {
			n = len(runes)
		}
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
	}
	return chunks
}

type remoteOutputState struct {
	chatID          int64
	draftBuffer     strings.Builder
	activeDraftID   int64
	lastDraftSentAt time.Time
	lastDraftText   string
	now             func() time.Time
}

func newRemoteOutputState(chatID int64) *remoteOutputState {
	return &remoteOutputState{
		chatID: chatID,
		now:    time.Now,
	}
}

func (s *remoteOutputState) beginTurn(draftID int64) {
	s.activeDraftID = draftID
	s.draftBuffer.Reset()
	s.lastDraftText = ""
	s.lastDraftSentAt = time.Time{}
}

func (s *remoteOutputState) sendDraft(sender telegramSender, force bool) (bool, error) {
	if s.activeDraftID == 0 || s.draftBuffer.Len() == 0 {
		return false, nil
	}
	now := s.now()
	if !force && !s.lastDraftSentAt.IsZero() && now.Sub(s.lastDraftSentAt) < 700*time.Millisecond {
		return false, nil
	}
	textToSend := s.draftBuffer.String()
	if chunks := splitTelegramText(textToSend, 4096); len(chunks) > 0 {
		textToSend = chunks[0]
	}
	if textToSend == s.lastDraftText {
		return false, nil
	}
	if err := sender.sendMessageDraft(s.chatID, s.activeDraftID, textToSend); err != nil {
		return false, err
	}
	s.lastDraftText = textToSend
	s.lastDraftSentAt = now
	return true, nil
}

func (s *remoteOutputState) sendFinal(sender telegramSender, text string) error {
	if s.activeDraftID != 0 {
		_ = sender.sendMessageDraft(s.chatID, s.activeDraftID, "")
	}
	s.draftBuffer.Reset()
	s.activeDraftID = 0
	s.lastDraftText = ""
	if text == "" {
		return nil
	}
	for _, chunk := range splitTelegramText(text, 4096) {
		if err := sender.sendMessage(s.chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *remoteOutputState) handleEvent(sender telegramSender, event remoteResponseEvent) error {
	switch event.kind {
	case "delta":
		if s.activeDraftID != 0 {
			s.draftBuffer.WriteString(event.text)
		}
	case "final":
		return s.sendFinal(sender, event.text)
	case "turn_completed":
		_, err := s.sendDraft(sender, true)
		return err
	case "status", "error", "tool":
		for _, chunk := range splitTelegramText(event.text, 4096) {
			if err := sender.sendMessage(s.chatID, chunk); err != nil {
				return err
			}
		}
	}
	return nil
}

func augmentedAgyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/agent"
	}
	base := os.Getenv("PATH")
	if base == "" {
		base = "/usr/local/bin:/usr/bin:/bin"
	}
	return fmt.Sprintf("%s/bin:%s/.local/bin:%s/.local/share/fnm/aliases/default/bin:%s", home, home, home, base)
}

func agyTranscriptPath(convID string) string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain", convID, ".system_generated", "logs", "transcript.jsonl")
}

func agyConversationIDFromTranscriptPath(path string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
}

func extractAgyUserRequest(content string) string {
	start := strings.Index(content, "<USER_REQUEST>")
	end := strings.Index(content, "</USER_REQUEST>")
	if start < 0 || end < 0 || end < start {
		return strings.TrimSpace(content)
	}
	start += len("<USER_REQUEST>")
	return strings.TrimSpace(content[start:end])
}

func transcriptMatchesPrompt(path string, prompt string) bool {
	if prompt == "" {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var step map[string]interface{}
		if err := json.Unmarshal([]byte(line), &step); err != nil {
			continue
		}
		source, _ := step["source"].(string)
		typ, _ := step["type"].(string)
		content, _ := step["content"].(string)
		if source == "USER_EXPLICIT" && typ == "USER_INPUT" && extractAgyUserRequest(content) == prompt {
			return true
		}
	}
	return false
}

func isAgyNarration(text string) bool {
	lines := strings.Split(text, "\n")
	found := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		found = true
		if !strings.HasPrefix(trimmed, "I will") {
			return false
		}
	}
	return found
}

func getNewAgyConversationIDFromTranscripts(startedAt time.Time, beforeID string, prompt string) string {
	homeDir, _ := os.UserHomeDir()
	pattern := filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain", "*", ".system_generated", "logs", "transcript.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ""
	}

	var bestPath string
	var bestTime time.Time
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(startedAt.Add(-1 * time.Second)) {
			continue
		}
		convID := agyConversationIDFromTranscriptPath(path)
		if convID == beforeID {
			continue
		}
		if !transcriptMatchesPrompt(path, prompt) {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestTime) {
			bestPath = path
			bestTime = info.ModTime()
		}
	}
	if bestPath == "" {
		return ""
	}
	return agyConversationIDFromTranscriptPath(bestPath)
}

func latestAgyStepIndex(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()

	var latest int64 = -1
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var step map[string]interface{}
		if err := json.Unmarshal([]byte(line), &step); err != nil {
			continue
		}
		idxVal, ok := step["step_index"].(float64)
		if !ok {
			continue
		}
		idx := int64(idxVal)
		if idx > latest {
			latest = idx
		}
	}
	return latest
}

func readNewSteps(path string, lastSentStep *int64, responseChan chan<- remoteResponseEvent) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var step map[string]interface{}
		if err := json.Unmarshal([]byte(line), &step); err == nil {
			idxVal, ok := step["step_index"].(float64)
			if !ok {
				continue
			}
			idx := int64(idxVal)
			if idx > *lastSentStep {
				*lastSentStep = idx
				source, _ := step["source"].(string)
				typ, _ := step["type"].(string)
				content, _ := step["content"].(string)

				if source == "MODEL" {
					if typ == "PLANNER_RESPONSE" {
						if toolCalls, ok := step["tool_calls"].([]interface{}); ok {
							for _, tc := range toolCalls {
								if tcMap, ok := tc.(map[string]interface{}); ok {
									if name, ok := tcMap["name"].(string); ok && name != "" {
										responseChan <- remoteResponseEvent{kind: "tool", text: fmt.Sprintf("*[Tool Call]* `%s`", name)}
									}
								}
							}
						}
						msg := ""
						if !isAgyNarration(content) {
							msg = content
						}
						if msg != "" {
							responseChan <- remoteResponseEvent{kind: "delta", text: msg}
						}
					}
				}
			}
		}
	}
}

func pollAgyTranscript(convID string, lastSentStep *int64, responseChan chan<- remoteResponseEvent, doneChan <-chan struct{}) {
	transcriptPath := agyTranscriptPath(convID)

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-doneChan:
			readNewSteps(transcriptPath, lastSentStep, responseChan)
			return
		case <-ticker.C:
			readNewSteps(transcriptPath, lastSentStep, responseChan)
		}
	}
}

const agyBootstrapPrompt = "Internal remote bridge initialization. This is not a human user request and must not be counted as a previous user question. Start a reusable conversation session and reply with exactly READY."

func createAgySession(timeout time.Duration) (string, int64, error) {
	startedAt := time.Now()
	cmd := exec.Command("agy", "--print", agyBootstrapPrompt)
	cmd.Env = append(os.Environ(), "PATH="+augmentedAgyPath())
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}

	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	var convID string
	var cmdErr error
	cmdFinished := false
	ticker := time.NewTicker(100 * time.Millisecond)
	timer := time.NewTimer(timeout)
	defer ticker.Stop()
	defer timer.Stop()

waitForConversation:
	for {
		select {
		case <-ticker.C:
			convID = getNewAgyConversationIDFromTranscripts(startedAt, "", agyBootstrapPrompt)
			if convID != "" {
				break waitForConversation
			}
		case cmdErr = <-cmdDone:
			cmdFinished = true
			convID = getNewAgyConversationIDFromTranscripts(startedAt, "", agyBootstrapPrompt)
			if convID != "" {
				break waitForConversation
			}
			if cmdErr != nil {
				return "", -1, fmt.Errorf("agy bootstrap exited before transcript was found: %w", cmdErr)
			}
			return "", -1, fmt.Errorf("agy bootstrap exited before transcript was found")
		case <-timer.C:
			_ = cmd.Process.Kill()
			<-cmdDone
			return "", -1, fmt.Errorf("timed out waiting for agy bootstrap transcript")
		}
	}

	if !cmdFinished {
		cmdErr = <-cmdDone
	}
	if cmdErr != nil {
		return "", -1, cmdErr
	}

	return convID, latestAgyStepIndex(agyTranscriptPath(convID)), nil
}

func remoteLockPath() string {
	return filepath.Join(os.TempDir(), "am-remote.pid.lock")
}

// processAlive reports whether a process with the given pid is currently
// running. signal 0 performs error checking without delivering a signal.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// acquireRemoteLock guarantees only one `am remote` runs at a time by writing
// the current pid to a lock file. It returns an error if a live instance
// already holds the lock, and a release func that removes the lock if (and
// only if) it still belongs to this process.
func acquireRemoteLock() (func(), error) {
	path := remoteLockPath()
	if data, err := os.ReadFile(path); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
			if pid != os.Getpid() && processAlive(pid) {
				return nil, fmt.Errorf("another am remote is already running (pid %d); stop it first or remove %s", pid, path)
			}
		}
		// Otherwise the lock is stale (no such process) and we overwrite it.
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, fmt.Errorf("failed to write remote lock %s: %w", path, err)
	}
	release := func() {
		if data, err := os.ReadFile(path); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid != os.Getpid() {
				return // lock was taken over by another process; leave it alone
			}
		}
		_ = os.Remove(path)
	}
	return release, nil
}

// killProcessTree terminates a child (started with Setpgid: true) along with
// its whole process group, so grandchildren are reaped too — notably
// npx -> node -> the actual agent, which npx does not forward signals to.
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// A negative pid targets the entire process group; the child is the group
	// leader because it was started with Setpgid: true.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

func remoteCommand(token string) {
	release, err := acquireRemoteLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	defer release()

	// activeChild points at the currently running agent process (codex
	// app-server / claude ACP). The signal handler kills it so Ctrl-C, SIGTERM,
	// `kill <pid>` and detached runs all leave no orphaned agent behind.
	var activeChildMu sync.Mutex
	var activeChild *exec.Cmd

	// Clean up the agent process and the lock on Ctrl-C / termination so a
	// crash-free exit never leaves an orphan or a stale lock blocking the next run.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		activeChildMu.Lock()
		killProcessTree(activeChild)
		activeChildMu.Unlock()
		release()
		os.Exit(0)
	}()

	fmt.Println("Starting am remote (experimental) with Telegram integration...")
	fmt.Println("Please send a message '/start codex', '/start agy' or '/start claude' to your telegram bot.")

	tg := newTelegramClient(token)
	if err := tg.setMyCommands(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to register telegram commands: %v\n", err)
	}
	offset := 0

	for {
		var chatID int64
		var mode string // "codex", "agy" or "claude"

		for mode == "" {
			out, err := tg.fetchUpdates(offset, 10)
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}

			var resp TelegramResponse
			if err := json.Unmarshal(out, &resp); err != nil {
				time.Sleep(2 * time.Second)
				continue
			}

			for _, up := range resp.Result {
				if up.UpdateID >= offset {
					offset = up.UpdateID + 1
				}
				if up.Message != nil && strings.HasPrefix(up.Message.Text, "/start") {
					cID := up.Message.Chat.ID
					txt := strings.TrimSpace(up.Message.Text)
					if strings.Contains(txt, "agy") {
						chatID = cID
						mode = "agy"
						fmt.Printf("Authenticated chat ID: %d (mode: agy)\n", chatID)
						_ = tg.sendMessage(chatID, "Authenticated! Launching agy CLI remote...")
						break
					} else if strings.Contains(txt, "codex") {
						chatID = cID
						mode = "codex"
						fmt.Printf("Authenticated chat ID: %d (mode: codex)\n", chatID)
						_ = tg.sendMessage(chatID, "Authenticated! Launching codex app-server...")
						break
					} else if strings.Contains(txt, "claude") {
						chatID = cID
						mode = "claude"
						fmt.Printf("Authenticated chat ID: %d (mode: claude)\n", chatID)
						_ = tg.sendMessage(chatID, "Authenticated! Launching Claude ACP agent...")
						break
					} else {
						_ = tg.sendMessage(cID, "Please specify a mode. Use /start_codex, /start_agy or /start_claude to start the remote session.")
					}
				}
			}
			time.Sleep(1 * time.Second)
		}

		var appServerCmd *exec.Cmd
		var stdin io.WriteCloser
		var stdout io.ReadCloser
		var activeThreadID string
		var activeClaudeSessionID string
		var nextRequestID = 2
		var responseChan = make(chan remoteResponseEvent, 100)
		appServerDoneChan := make(chan struct{})

		writeJSONRPCForCodex := func(w io.Writer, method string, id interface{}, params interface{}) {
			msg := map[string]interface{}{
				"method": method,
				"params": params,
			}
			if id != nil {
				msg["id"] = id
			}
			data, _ := json.Marshal(msg)
			fmt.Printf(">> Write app-server: %s\n", string(data))
			_, _ = w.Write(append(data, '\n'))
		}

		// ACP (Agent Client Protocol) is JSON-RPC 2.0 and keeps the
		// "jsonrpc":"2.0" envelope field, unlike codex app-server.
		writeJSONRPCForACP := func(w io.Writer, method string, id interface{}, params interface{}) {
			msg := map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  method,
				"params":  params,
			}
			if id != nil {
				msg["id"] = id
			}
			data, _ := json.Marshal(msg)
			fmt.Printf(">> Write acp: %s\n", string(data))
			_, _ = w.Write(append(data, '\n'))
		}

		// writeACPResponse replies to an agent-initiated request (e.g.
		// session/request_permission) with a result by id.
		writeACPResponse := func(w io.Writer, id interface{}, result interface{}) {
			msg := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  result,
			}
			data, _ := json.Marshal(msg)
			fmt.Printf(">> Write acp response: %s\n", string(data))
			_, _ = w.Write(append(data, '\n'))
		}

		if mode == "codex" {
			appServerCmd = exec.Command("codex", "app-server", "--stdio")
			appServerCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var err error
			stdin, err = appServerCmd.StdinPipe()
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get stdin pipe: %v\n", err)
				_ = tg.sendMessage(chatID, fmt.Sprintf("Error: failed to initialize Codex pipes: %v", err))
				continue
			}
			stdout, err = appServerCmd.StdoutPipe()
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get stdout pipe: %v\n", err)
				_ = tg.sendMessage(chatID, fmt.Sprintf("Error: failed to initialize Codex pipes: %v", err))
				continue
			}

			if err := appServerCmd.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "failed to start codex app-server: %v\n", err)
				_ = tg.sendMessage(chatID, "Error: Failed to launch codex app-server. Make sure codex CLI is installed.")
				continue
			}
			activeChildMu.Lock()
			activeChild = appServerCmd
			activeChildMu.Unlock()

			go func() {
				scanner := bufio.NewScanner(stdout)
				for {
					select {
					case <-appServerDoneChan:
						return
					default:
						if !scanner.Scan() {
							return
						}
						line := scanner.Text()
						fmt.Printf("<< Read app-server: %s\n", line)

						var raw map[string]interface{}
						if err := json.Unmarshal([]byte(line), &raw); err != nil {
							continue
						}

						method, _ := raw["method"].(string)
						if method == "" {
							idVal, ok := raw["id"].(float64)
							if ok {
								if idVal == 0 {
									writeJSONRPCForCodex(stdin, "initialized", nil, map[string]interface{}{})
									writeJSONRPCForCodex(stdin, "thread/start", 1, map[string]interface{}{})
								} else if idVal == 1 {
									if result, exists := raw["result"].(map[string]interface{}); exists {
										if thread, exists := result["thread"].(map[string]interface{}); exists {
											activeThreadID, _ = thread["id"].(string)
											fmt.Printf("Codex active thread: %s\n", activeThreadID)
											responseChan <- remoteResponseEvent{kind: "status", text: fmt.Sprintf("Codex thread started! (ID: %s)", activeThreadID)}
										}
									}
								}
							}
						} else {
							switch method {
							case "item/agentMessage/delta":
								if params, exists := raw["params"].(map[string]interface{}); exists {
									if delta, exists := params["delta"].(string); exists {
										responseChan <- remoteResponseEvent{kind: "delta", text: delta}
									}
								}
							case "item/completed":
								if params, exists := raw["params"].(map[string]interface{}); exists {
									if item, exists := params["item"].(map[string]interface{}); exists {
										itemType, _ := item["type"].(string)
										text, _ := item["text"].(string)
										if itemType == "agentMessage" && text != "" {
											responseChan <- remoteResponseEvent{kind: "final", text: text}
										}
									}
								}
							case "turn/completed":
								responseChan <- remoteResponseEvent{kind: "turn_completed", text: ""}
							case "error":
								if params, exists := raw["params"].(map[string]interface{}); exists {
									if msg, exists := params["message"].(string); exists {
										responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("[Error] %s", msg)}
									}
								}
							}
						}
					}
				}
			}()

			writeJSONRPCForCodex(stdin, "initialize", 0, map[string]interface{}{
				"clientInfo": map[string]interface{}{
					"name":    "agents_manager",
					"title":   "agents-manager",
					"version": "0.1.0",
				},
			})
		}

		if mode == "claude" {
			cwd, _ := os.Getwd()
			appServerCmd = exec.Command("npx", "-y", "@agentclientprotocol/claude-agent-acp")
			appServerCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var err error
			stdin, err = appServerCmd.StdinPipe()
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get stdin pipe: %v\n", err)
				_ = tg.sendMessage(chatID, fmt.Sprintf("Error: failed to initialize Claude pipes: %v", err))
				continue
			}
			stdout, err = appServerCmd.StdoutPipe()
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get stdout pipe: %v\n", err)
				_ = tg.sendMessage(chatID, fmt.Sprintf("Error: failed to initialize Claude pipes: %v", err))
				continue
			}

			if err := appServerCmd.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "failed to start claude acp agent: %v\n", err)
				_ = tg.sendMessage(chatID, "Error: Failed to launch Claude ACP agent. Make sure node/npx is installed.")
				continue
			}
			activeChildMu.Lock()
			activeChild = appServerCmd
			activeChildMu.Unlock()

			go func() {
				scanner := bufio.NewScanner(stdout)
				scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
				for {
					select {
					case <-appServerDoneChan:
						return
					default:
						if !scanner.Scan() {
							return
						}
						line := scanner.Text()
						fmt.Printf("<< Read acp: %s\n", line)

						var raw map[string]interface{}
						if err := json.Unmarshal([]byte(line), &raw); err != nil {
							continue
						}

						method, _ := raw["method"].(string)
						if method != "" {
							switch method {
							case "session/update":
								params, _ := raw["params"].(map[string]interface{})
								update, _ := params["update"].(map[string]interface{})
								if update == nil {
									continue
								}
								switch update["sessionUpdate"] {
								case "agent_message_chunk":
									if content, ok := update["content"].(map[string]interface{}); ok {
										if txt, ok := content["text"].(string); ok && txt != "" {
											responseChan <- remoteResponseEvent{kind: "delta", text: txt}
										}
									}
								case "tool_call":
									title, _ := update["title"].(string)
									if title == "" {
										title, _ = update["kind"].(string)
									}
									if title != "" {
										responseChan <- remoteResponseEvent{kind: "tool", text: fmt.Sprintf("[Tool] %s", title)}
									}
								}
							case "session/request_permission":
								// Auto-approve so the remote session can run unattended.
								id := raw["id"]
								params, _ := raw["params"].(map[string]interface{})
								options, _ := params["options"].([]interface{})
								optionID := ""
								for _, kind := range []string{"allow_always", "allow_once"} {
									for _, optAny := range options {
										if opt, ok := optAny.(map[string]interface{}); ok {
											if opt["kind"] == kind {
												optionID, _ = opt["optionId"].(string)
												break
											}
										}
									}
									if optionID != "" {
										break
									}
								}
								if optionID == "" && len(options) > 0 {
									if opt, ok := options[0].(map[string]interface{}); ok {
										optionID, _ = opt["optionId"].(string)
									}
								}
								writeACPResponse(stdin, id, map[string]interface{}{
									"outcome": map[string]interface{}{
										"outcome":  "selected",
										"optionId": optionID,
									},
								})
							}
						} else if _, hasID := raw["id"]; hasID {
							idVal, _ := raw["id"].(float64)
							result, _ := raw["result"].(map[string]interface{})
							if errObj, ok := raw["error"].(map[string]interface{}); ok {
								msg, _ := errObj["message"].(string)
								responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("[Error] %s", msg)}
								continue
							}
							if idVal == 0 {
								// initialize result -> create a session
								writeJSONRPCForACP(stdin, "session/new", 1, map[string]interface{}{
									"cwd":        cwd,
									"mcpServers": []interface{}{},
								})
							} else if idVal == 1 {
								// session/new result -> store id, force bypass mode
								if result != nil {
									activeClaudeSessionID, _ = result["sessionId"].(string)
									if activeClaudeSessionID != "" {
										writeJSONRPCForACP(stdin, "session/set_mode", 99, map[string]interface{}{
											"sessionId": activeClaudeSessionID,
											"modeId":    "bypassPermissions",
										})
										responseChan <- remoteResponseEvent{kind: "status", text: fmt.Sprintf("Claude session started! (ID: %s)", activeClaudeSessionID)}
									}
								}
							} else if result != nil {
								if _, ok := result["stopReason"]; ok {
									responseChan <- remoteResponseEvent{kind: "turn_completed", text: ""}
								}
							}
						}
					}
				}
			}()

			writeJSONRPCForACP(stdin, "initialize", 0, map[string]interface{}{
				"protocolVersion": 1,
				"clientCapabilities": map[string]interface{}{
					"fs": map[string]interface{}{
						"readTextFile":  false,
						"writeTextFile": false,
					},
					"terminal": false,
				},
			})
		}

		var isGenerating int32
		var activeAgyConversationID string
		var activeAgyLastStep int64 = -1

		if mode == "agy" {
			_ = tg.sendMessage(chatID, "Starting agy session...")
			convID, lastStep, err := createAgySession(30 * time.Second)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to initialize agy session: %v\n", err)
				_ = tg.sendMessage(chatID, fmt.Sprintf("Failed to initialize agy session: %v", err))
				continue
			} else {
				activeAgyConversationID = convID
				activeAgyLastStep = lastStep
				_ = tg.sendMessage(chatID, fmt.Sprintf("Agy session started! (ID: %s)", convID))
			}
		}

		ticker := time.NewTicker(200 * time.Millisecond)
		outputState := newRemoteOutputState(chatID)
		handleResponseEvent := func(event remoteResponseEvent) {
			if event.kind == "final" || event.kind == "status" || event.kind == "error" {
				fmt.Printf("Sent to Telegram:\n%s\n--------------------\n", event.text)
			}
			if err := outputState.handleEvent(tg, event); err != nil {
				fmt.Fprintf(os.Stderr, "failed to handle remote output event %s: %v\n", event.kind, err)
			}
		}
		drainResponseEvents := func() {
			for {
				select {
				case event := <-responseChan:
					handleResponseEvent(event)
				default:
					return
				}
			}
		}

		sessionTerminated := false
		for !sessionTerminated {
			select {
			case <-ticker.C:
				drainResponseEvents()

				out, err := tg.fetchUpdates(offset, 0)
				if err == nil {
					var resp TelegramResponse
					if err := json.Unmarshal(out, &resp); err == nil {
						for _, up := range resp.Result {
							if up.UpdateID >= offset {
								offset = up.UpdateID + 1
							}
							if up.Message != nil && up.Message.Chat.ID == chatID {
								text := strings.TrimSpace(up.Message.Text)
								fmt.Printf("Telegram user input: %s\n", text)

								if text == "/stop" {
									_ = tg.sendMessage(chatID, "Stopping current session...")
									if (mode == "codex" || mode == "claude") && appServerCmd != nil && appServerCmd.Process != nil {
										close(appServerDoneChan)
										killProcessTree(appServerCmd)
										// Reap the killed process so repeated start/stop
										// cycles do not pile up zombie processes.
										go func(p *os.Process) { _, _ = p.Wait() }(appServerCmd.Process)
										activeChildMu.Lock()
										activeChild = nil
										activeChildMu.Unlock()
									}
									_ = tg.sendMessage(chatID, "Session stopped. Use /start_codex, /start_agy or /start_claude to start a new session.")
									sessionTerminated = true
									break
								}

								if strings.HasPrefix(text, "/start") {
									_ = tg.sendMessage(chatID, "Session is already active. Use /stop to terminate the current session before starting a new one.")
									continue
								}

								if mode == "codex" {
									if activeThreadID == "" {
										_ = tg.sendMessage(chatID, "Waiting for Codex thread to initialize...")
										continue
									}

									requestID := nextRequestID
									nextRequestID++
									outputState.beginTurn(time.Now().UnixNano())
									writeJSONRPCForCodex(stdin, "turn/start", requestID, map[string]interface{}{
										"threadId": activeThreadID,
										"input": []interface{}{
											map[string]interface{}{
												"type": "text",
												"text": text,
											},
										},
									})
								} else if mode == "claude" {
									if activeClaudeSessionID == "" {
										_ = tg.sendMessage(chatID, "Waiting for Claude session to initialize...")
										continue
									}

									requestID := nextRequestID
									nextRequestID++
									outputState.beginTurn(time.Now().UnixNano())
									writeJSONRPCForACP(stdin, "session/prompt", requestID, map[string]interface{}{
										"sessionId": activeClaudeSessionID,
										"prompt": []interface{}{
											map[string]interface{}{
												"type": "text",
												"text": text,
											},
										},
									})
								} else if mode == "agy" {
									if atomic.LoadInt32(&isGenerating) == 1 {
										_ = tg.sendMessage(chatID, "Antigravity is busy. Please wait...")
										continue
									}
									atomic.StoreInt32(&isGenerating, 1)
									outputState.beginTurn(time.Now().UnixNano())

									go func(promptText string) {
										defer atomic.StoreInt32(&isGenerating, 0)

										resumeConversationID := activeAgyConversationID
										args := []string{}
										if resumeConversationID != "" {
											args = append(args, "--conversation", resumeConversationID)
										}
										args = append(args, "--print", promptText)
										startedAt := time.Now()

										augmentedPath := augmentedAgyPath()
										cmd := exec.Command("agy", args...)
										cmd.Env = append(os.Environ(), "PATH="+augmentedPath)
										cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

										err := cmd.Start()
										if err != nil {
											responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("Failed to start agy: %v", err)}
											return
										}
										// Expose the in-flight agy process so Ctrl-C / SIGTERM
										// can kill it; clear it again when this prompt finishes.
										activeChildMu.Lock()
										activeChild = cmd
										activeChildMu.Unlock()
										defer func() {
											activeChildMu.Lock()
											if activeChild == cmd {
												activeChild = nil
											}
											activeChildMu.Unlock()
										}()
										cmdDone := make(chan error, 1)
										go func() {
											cmdDone <- cmd.Wait()
										}()

										var tempConvID string
										var cmdErr error
										cmdFinished := false
										transcriptTicker := time.NewTicker(100 * time.Millisecond)
										transcriptTimeout := time.NewTimer(30 * time.Second)
									waitForTranscript:
										for {
											select {
											case <-transcriptTicker.C:
												if resumeConversationID != "" {
													if transcriptMatchesPrompt(agyTranscriptPath(resumeConversationID), promptText) {
														tempConvID = resumeConversationID
													}
												} else {
													tempConvID = getNewAgyConversationIDFromTranscripts(startedAt, "", promptText)
												}
												if tempConvID != "" {
													if resumeConversationID == "" {
														responseChan <- remoteResponseEvent{kind: "status", text: fmt.Sprintf("Agy session created! (ID: %s)", tempConvID)}
													}
													break waitForTranscript
												}
											case cmdErr = <-cmdDone:
												cmdFinished = true
												if resumeConversationID != "" {
													if transcriptMatchesPrompt(agyTranscriptPath(resumeConversationID), promptText) {
														tempConvID = resumeConversationID
													}
												} else {
													tempConvID = getNewAgyConversationIDFromTranscripts(startedAt, "", promptText)
												}
												if tempConvID != "" {
													if resumeConversationID == "" {
														responseChan <- remoteResponseEvent{kind: "status", text: fmt.Sprintf("Agy session created! (ID: %s)", tempConvID)}
													}
													break waitForTranscript
												}
												if cmdErr != nil {
													responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("agy exited before transcript was found: %v", cmdErr)}
												} else {
													responseChan <- remoteResponseEvent{kind: "error", text: "agy exited before transcript was found."}
												}
												return
											case <-transcriptTimeout.C:
												_ = cmd.Process.Kill()
												cmdErr = <-cmdDone
												cmdFinished = true
												responseChan <- remoteResponseEvent{kind: "error", text: "Timed out waiting for agy transcript for this prompt."}
												return
											}
										}
										transcriptTicker.Stop()
										transcriptTimeout.Stop()

										donePoll := make(chan struct{})
										lastSentStep := activeAgyLastStep
										go pollAgyTranscript(tempConvID, &lastSentStep, responseChan, donePoll)

										if !cmdFinished {
											cmdErr = <-cmdDone
										}
										close(donePoll)
										activeAgyConversationID = tempConvID
										activeAgyLastStep = latestAgyStepIndex(agyTranscriptPath(tempConvID))

										if cmdErr != nil {
											responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("agy exited with error: %v", cmdErr)}
										} else {
											responseChan <- remoteResponseEvent{kind: "turn_completed", text: ""}
										}
									}(text)
								}
							}
						}
					}
				}

				drainResponseEvents()
				if sent, err := outputState.sendDraft(tg, false); err != nil {
					fmt.Fprintf(os.Stderr, "failed to send Telegram draft: %v\n", err)
				} else if sent {
					fmt.Printf("Sent Telegram draft:\n%s\n--------------------\n", outputState.lastDraftText)
				}

			case event := <-responseChan:
				handleResponseEvent(event)
			}
		}

		ticker.Stop()

		// The session loop only exits via /stop, which already killed and
		// reaped any agent process. Defensively clear the reference (no pid
		// operations here — the pid may already be reused) so the signal
		// handler never targets a stale pid before the next /start.
		activeChildMu.Lock()
		activeChild = nil
		activeChildMu.Unlock()
	}
}
