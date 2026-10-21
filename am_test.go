package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fakeTelegramSender struct {
	messages []string
	drafts   []string
}

func (f *fakeTelegramSender) sendMessage(chatID int64, text string) error {
	f.messages = append(f.messages, text)
	return nil
}

func (f *fakeTelegramSender) sendMessageDraft(chatID int64, draftID int64, text string) error {
	f.drafts = append(f.drafts, text)
	return nil
}

func TestRemoteOutputFinalSendsOneMessageAndNoDeltaMessages(t *testing.T) {
	sender := &fakeTelegramSender{}
	state := newRemoteOutputState(42)
	state.beginTurn(100)

	for _, event := range []remoteResponseEvent{
		{kind: "delta", text: "저"},
		{kind: "delta", text: "는"},
		{kind: "final", text: "저는 Codex입니다."},
	} {
		if err := state.handleEvent(sender, event); err != nil {
			t.Fatalf("handleEvent(%s): %v", event.kind, err)
		}
	}

	if len(sender.messages) != 1 {
		t.Fatalf("messages len = %d, want 1: %#v", len(sender.messages), sender.messages)
	}
	if sender.messages[0] != "저는 Codex입니다." {
		t.Fatalf("message = %q", sender.messages[0])
	}
	if len(sender.drafts) != 1 || sender.drafts[0] != "" {
		t.Fatalf("drafts = %#v, want only clear draft", sender.drafts)
	}
}

func TestRemoteOutputDraftIsThrottledAndCoalesced(t *testing.T) {
	sender := &fakeTelegramSender{}
	now := time.Unix(1000, 0)
	state := newRemoteOutputState(42)
	state.now = func() time.Time { return now }
	state.beginTurn(100)

	if err := state.handleEvent(sender, remoteResponseEvent{kind: "delta", text: "a"}); err != nil {
		t.Fatal(err)
	}
	if sent, err := state.sendDraft(sender, false); err != nil || !sent {
		t.Fatalf("first sendDraft sent=%v err=%v", sent, err)
	}

	if err := state.handleEvent(sender, remoteResponseEvent{kind: "delta", text: "b"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(100 * time.Millisecond)
	if sent, err := state.sendDraft(sender, false); err != nil || sent {
		t.Fatalf("throttled sendDraft sent=%v err=%v", sent, err)
	}

	now = now.Add(700 * time.Millisecond)
	if sent, err := state.sendDraft(sender, false); err != nil || !sent {
		t.Fatalf("coalesced sendDraft sent=%v err=%v", sent, err)
	}

	want := []string{"a", "ab"}
	if len(sender.drafts) != len(want) {
		t.Fatalf("drafts = %#v, want %#v", sender.drafts, want)
	}
	for i := range want {
		if sender.drafts[i] != want[i] {
			t.Fatalf("drafts[%d] = %q, want %q", i, sender.drafts[i], want[i])
		}
	}
}

func TestRemoteOutputIgnoresDeltaAfterFinal(t *testing.T) {
	sender := &fakeTelegramSender{}
	state := newRemoteOutputState(42)
	state.beginTurn(100)

	events := []remoteResponseEvent{
		{kind: "delta", text: "partial"},
		{kind: "final", text: "final"},
		{kind: "delta", text: " stale"},
		{kind: "turn_completed"},
	}
	for _, event := range events {
		if err := state.handleEvent(sender, event); err != nil {
			t.Fatalf("handleEvent(%s): %v", event.kind, err)
		}
	}

	if len(sender.messages) != 1 || sender.messages[0] != "final" {
		t.Fatalf("messages = %#v, want final only", sender.messages)
	}
	if len(sender.drafts) != 1 || sender.drafts[0] != "" {
		t.Fatalf("drafts = %#v, want only clear draft", sender.drafts)
	}
}

func TestReadNewStepsFromAgyTranscript(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "transcript_*.jsonl")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	steps := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"hello"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"I am Antigravity.","tool_calls":[{"name":"view_file","args":{}}]}`,
	}
	for _, step := range steps {
		if _, err := tmpFile.WriteString(step + "\n"); err != nil {
			t.Fatalf("failed to write step: %v", err)
		}
	}
	_ = tmpFile.Sync()

	responseChan := make(chan remoteResponseEvent, 10)
	var lastSentStep int64 = -1

	readNewSteps(tmpFile.Name(), &lastSentStep, responseChan)

	if lastSentStep != 1 {
		t.Errorf("lastSentStep = %d, want 1", lastSentStep)
	}

	close(responseChan)
	var events []remoteResponseEvent
	for ev := range responseChan {
		events = append(events, ev)
	}

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2. Events: %#v", len(events), events)
	}

	if events[0].kind != "tool" || events[0].text != "*[Tool Call]* `view_file`" {
		t.Errorf("tool event = %#v", events[0])
	}
	if events[1].kind != "delta" || events[1].text != "I am Antigravity." {
		t.Errorf("delta event = %#v", events[1])
	}
}

func TestRemoteOutputToolEventSendsSingleMessage(t *testing.T) {
	sender := &fakeTelegramSender{}
	state := newRemoteOutputState(42)
	state.beginTurn(100)

	if err := state.handleEvent(sender, remoteResponseEvent{kind: "tool", text: "*[Tool Call]* `view_file`"}); err != nil {
		t.Fatal(err)
	}
	if err := state.handleEvent(sender, remoteResponseEvent{kind: "delta", text: "answer"}); err != nil {
		t.Fatal(err)
	}
	if sent, err := state.sendDraft(sender, true); err != nil || !sent {
		t.Fatalf("sendDraft sent=%v err=%v", sent, err)
	}

	if len(sender.messages) != 1 || sender.messages[0] != "*[Tool Call]* `view_file`" {
		t.Fatalf("messages = %#v, want one tool message", sender.messages)
	}
	if len(sender.drafts) != 1 || sender.drafts[0] != "answer" {
		t.Fatalf("drafts = %#v, want answer-only draft", sender.drafts)
	}
}

func TestTranscriptMatchesPromptUsesUserRequestOnly(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "transcript_*.jsonl")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	steps := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\n1+1=?\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nignored\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"stale response from another task"}`,
	}
	for _, step := range steps {
		if _, err := tmpFile.WriteString(step + "\n"); err != nil {
			t.Fatalf("failed to write step: %v", err)
		}
	}
	_ = tmpFile.Sync()

	if !transcriptMatchesPrompt(tmpFile.Name(), "1+1=?") {
		t.Fatal("expected transcript to match its USER_REQUEST prompt")
	}
	if transcriptMatchesPrompt(tmpFile.Name(), "different prompt") {
		t.Fatal("expected transcript not to match a different prompt")
	}
}

func TestReadNewStepsSuppressesAgyNarration(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "transcript_*.jsonl")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	steps := []string{
		`{"step_index":0,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"I will check the status."}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"2"}`,
	}
	for _, step := range steps {
		if _, err := tmpFile.WriteString(step + "\n"); err != nil {
			t.Fatalf("failed to write step: %v", err)
		}
	}
	_ = tmpFile.Sync()

	responseChan := make(chan remoteResponseEvent, 10)
	var lastSentStep int64 = -1
	readNewSteps(tmpFile.Name(), &lastSentStep, responseChan)
	close(responseChan)

	var got []string
	for ev := range responseChan {
		got = append(got, ev.text)
	}
	if len(got) != 1 || got[0] != "2" {
		t.Fatalf("events = %#v, want only final answer", got)
	}
}

func TestReadNewStepsStartsAfterLastSentStep(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "transcript_*.jsonl")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	steps := []string{
		`{"step_index":0,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"old"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"new"}`,
	}
	for _, step := range steps {
		if _, err := tmpFile.WriteString(step + "\n"); err != nil {
			t.Fatalf("failed to write step: %v", err)
		}
	}
	_ = tmpFile.Sync()

	responseChan := make(chan remoteResponseEvent, 10)
	lastSentStep := int64(0)
	readNewSteps(tmpFile.Name(), &lastSentStep, responseChan)
	close(responseChan)

	var got []string
	for ev := range responseChan {
		got = append(got, ev.text)
	}
	if len(got) != 1 || got[0] != "new" {
		t.Fatalf("events = %#v, want only new step", got)
	}
	if lastSentStep != 1 {
		t.Fatalf("lastSentStep = %d, want 1", lastSentStep)
	}
}

func TestAgyRemoteCommunicationFlow(t *testing.T) {
	promptText := "1+1=? reply with ONLY the number 2 and nothing else"

	args := []string{"--print", promptText}
	augmentedPath := augmentedAgyPath()
	startedAt := time.Now()
	cmd := exec.Command("agy", args...)
	cmd.Env = append(os.Environ(), "PATH="+augmentedPath)

	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf

	err := cmd.Start()
	if err != nil {
		t.Skipf("skipping integration test; agy binary start failed: %v", err)
	}

	var tempConvID string
	for i := 0; i < 300; i++ { // Poll for up to 30 seconds
		time.Sleep(100 * time.Millisecond)
		newID := getNewAgyConversationIDFromTranscripts(startedAt, "", promptText)
		if newID != "" {
			tempConvID = newID
			break
		}
	}
	if tempConvID == "" {
		_ = cmd.Process.Kill()
		t.Fatal("new conversation ID not detected from history or matching transcript")
	}

	responseChan := make(chan remoteResponseEvent, 100)
	_ = cmd.Wait()
	var lastSentStep int64 = -1
	readNewSteps(agyTranscriptPath(tempConvID), &lastSentStep, responseChan)

	var deltaText string
	close(responseChan)
	for ev := range responseChan {
		if ev.kind == "delta" {
			deltaText += ev.text
		}
	}

	combinedOutput := deltaText + "\n" + stdoutBuf.String()
	if !strings.Contains(combinedOutput, "2") {
		t.Errorf("communication test failed; expected output to contain '2', got combined: %q", combinedOutput)
	}
}

func TestAgyConversationResumeFlow(t *testing.T) {
	codeWord := fmt.Sprintf("ORBITAL%d", time.Now().UnixNano())
	firstPrompt := "Remember this code word for the next turn and reply only OK: " + codeWord
	secondPrompt := "What code word did I ask you to remember? Reply with only the code word."

	augmentedPath := augmentedAgyPath()
	startedAt := time.Now()
	first := exec.Command("agy", "--print", firstPrompt)
	first.Env = append(os.Environ(), "PATH="+augmentedPath)
	if err := first.Start(); err != nil {
		t.Skipf("skipping integration test; agy binary start failed: %v", err)
	}

	var convID string
	for i := 0; i < 300; i++ {
		time.Sleep(100 * time.Millisecond)
		convID = getNewAgyConversationIDFromTranscripts(startedAt, "", firstPrompt)
		if convID != "" {
			break
		}
	}
	if convID == "" {
		_ = first.Process.Kill()
		t.Fatal("first conversation ID not detected from matching transcript")
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first agy prompt failed: %v", err)
	}
	lastStep := latestAgyStepIndex(agyTranscriptPath(convID))

	secondStartedAt := time.Now()
	second := exec.Command("agy", "--conversation", convID, "--print", secondPrompt)
	second.Env = append(os.Environ(), "PATH="+augmentedPath)
	var stdoutBuf bytes.Buffer
	second.Stdout = &stdoutBuf
	if err := second.Start(); err != nil {
		t.Fatalf("failed to start second agy prompt: %v", err)
	}
	for i := 0; i < 300; i++ {
		time.Sleep(100 * time.Millisecond)
		if transcriptMatchesPrompt(agyTranscriptPath(convID), secondPrompt) {
			break
		}
		if time.Since(secondStartedAt) > 60*time.Second {
			_ = second.Process.Kill()
			t.Fatal("second prompt not detected in resumed transcript")
		}
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("second agy prompt failed: %v", err)
	}

	responseChan := make(chan remoteResponseEvent, 100)
	readNewSteps(agyTranscriptPath(convID), &lastStep, responseChan)
	close(responseChan)

	var deltaText string
	for ev := range responseChan {
		if ev.kind == "delta" {
			deltaText += ev.text
		}
	}
	combinedOutput := deltaText + "\n" + stdoutBuf.String()
	if !strings.Contains(combinedOutput, codeWord) {
		t.Fatalf("resumed conversation did not recall code word %q; got %q", codeWord, combinedOutput)
	}
}

func TestParseSearchArgs(t *testing.T) {
	tests := []struct {
		args      []string
		wantKw    string
		wantDir   string
		wantN     int
		wantLimit int
		wantErr   bool
	}{
		{
			args:      []string{"mykeyword"},
			wantKw:    "mykeyword",
			wantDir:   "",
			wantN:     10,
			wantLimit: 20,
			wantErr:   false,
		},
		{
			args:      []string{"mykeyword", "/some/path"},
			wantKw:    "mykeyword",
			wantDir:   "/some/path",
			wantN:     10,
			wantLimit: 20,
			wantErr:   false,
		},
		{
			args:      []string{"mykeyword", "/some/path", "--case-sensitive"},
			wantKw:    "mykeyword",
			wantDir:   "/some/path",
			wantN:     10,
			wantLimit: 20,
			wantErr:   false,
		},
		{
			args:      []string{"--case-sensitive", "mykeyword", "/some/path"},
			wantKw:    "mykeyword",
			wantDir:   "/some/path",
			wantN:     10,
			wantLimit: 20,
			wantErr:   false,
		},
		{
			args:      []string{"mykeyword", "--snippet", "5"},
			wantKw:    "mykeyword",
			wantDir:   "",
			wantN:     5,
			wantLimit: 20,
			wantErr:   false,
		},
		{
			args:      []string{"mykeyword", "/some/path", "--snippet", "20"},
			wantKw:    "mykeyword",
			wantDir:   "/some/path",
			wantN:     20,
			wantLimit: 20,
			wantErr:   false,
		},
		{
			args:      []string{"mykeyword", "-n", "15"},
			wantKw:    "mykeyword",
			wantDir:   "",
			wantN:     10,
			wantLimit: 15,
			wantErr:   false,
		},
		{
			args:      []string{"mykeyword", "--limit", "30"},
			wantKw:    "mykeyword",
			wantDir:   "",
			wantN:     10,
			wantLimit: 30,
			wantErr:   false,
		},
		{
			args:    []string{"mykeyword", "--snippet"},
			wantErr: true,
		},
		{
			args:    []string{"mykeyword", "--snippet", "invalid"},
			wantErr: true,
		},
		{
			args:    []string{"mykeyword", "-n"},
			wantErr: true,
		},
		{
			args:    []string{"mykeyword", "-n", "invalid"},
			wantErr: true,
		},
		{
			args:    []string{"mykeyword", "/some/path", "extra"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		res, err := parseSearchArgs(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSearchArgs(%v) expected error, got nil", tc.args)
			}
		} else {
			if err != nil {
				t.Errorf("parseSearchArgs(%v) unexpected error: %v", tc.args, err)
				continue
			}
			if res.Keyword != tc.wantKw {
				t.Errorf("parseSearchArgs(%v) keyword = %q, want %q", tc.args, res.Keyword, tc.wantKw)
			}
			if res.DirFilter != tc.wantDir {
				t.Errorf("parseSearchArgs(%v) dirFilter = %q, want %q", tc.args, res.DirFilter, tc.wantDir)
			}
			if res.SnippetLines != tc.wantN {
				t.Errorf("parseSearchArgs(%v) SnippetLines = %d, want %d", tc.args, res.SnippetLines, tc.wantN)
			}
			if res.Limit != tc.wantLimit {
				t.Errorf("parseSearchArgs(%v) Limit = %d, want %d", tc.args, res.Limit, tc.wantLimit)
			}
		}
	}
}

func TestTUIFilterItemsByTab(t *testing.T) {
	all := []Session{
		{ID: "1", Provider: "codex"},
		{ID: "2", Provider: "claude"},
		{ID: "3", Provider: "cursor"},
		{ID: "4", Provider: "antigravity"},
	}

	m := tuiModel{
		allItems:   all,
		tabs:       []string{"ALL", "codex", "claude", "cursor", "antigravity"},
		currentTab: 0,
	}

	m = m.filterItemsByTab()
	if len(m.items) != 4 {
		t.Fatalf("ALL tab items count = %d, want 4", len(m.items))
	}

	m.currentTab = 1
	m = m.filterItemsByTab()
	if len(m.items) != 1 || m.items[0].Provider != "codex" {
		t.Fatalf("codex tab items = %#v, want 1 codex session", m.items)
	}

	m.currentTab = 2
	m = m.filterItemsByTab()
	if len(m.items) != 1 || m.items[0].Provider != "claude" {
		t.Fatalf("claude tab items = %#v, want 1 claude session", m.items)
	}
}

func TestResolveCommand(t *testing.T) {
	commands := []string{"sessions", "history", "search", "remote", "launch", "completion", "help"}

	tests := []struct {
		input       string
		wantCmd     string
		wantCandLen int
		wantErr     bool
	}{
		{input: "launch", wantCmd: "launch"},
		{input: "l", wantCmd: "launch"},
		{input: "lch", wantCmd: "launch"},
		{input: "LCH", wantCmd: "launch"},
		{input: "ses", wantCmd: "sessions"},
		{input: "sea", wantCmd: "search"},
		{input: "hi", wantCmd: "history"},
		{input: "rem", wantCmd: "remote"},
		{input: "s", wantCmd: "", wantCandLen: 2, wantErr: true},
		{input: "h", wantCmd: "", wantCandLen: 2, wantErr: true}, // history, help
		{input: "xyz", wantCmd: "", wantCandLen: 0, wantErr: true},
		{input: "", wantCmd: "", wantCandLen: 0, wantErr: true},
	}

	for _, tt := range tests {
		cmd, candidates, err := resolveCommand(tt.input, commands)
		if tt.wantErr && err == nil {
			t.Errorf("resolveCommand(%q) expected error, got nil", tt.input)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("resolveCommand(%q) unexpected error: %v", tt.input, err)
		}
		if cmd != tt.wantCmd {
			t.Errorf("resolveCommand(%q) cmd = %q, want %q", tt.input, cmd, tt.wantCmd)
		}
		if len(candidates) != tt.wantCandLen {
			t.Errorf("resolveCommand(%q) candidates len = %d, want %d", tt.input, len(candidates), tt.wantCandLen)
		}
	}
}

