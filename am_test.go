package main

import (
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
