package codexlaunch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStatusFromRollout(t *testing.T) {
	var s status
	for _, event := range []string{
		`{"type":"session_meta","payload":{"cwd":"/work/misc","cli_version":"0.161.0","source":"cli"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-test","effort":"high"}}`,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1200,"output_tokens":300},"last_token_usage":{"total_tokens":250},"model_context_window":1000},"rate_limits":{"limit_id":"codex","primary":{"used_percent":32,"window_minutes":10080,"resets_at":2500},"secondary":{"used_percent":0,"window_minutes":300,"resets_at":19000},"plan_type":"pro"}}}`,
	} {
		s.apply([]byte(event))
	}
	want := "gpt-test (High) · misc · in:1.2k / out:300 · ctx:75.0% · tasks:1 · 5h:100% (5h) · 7d:68% (25m) · v0.161.0"
	if got := s.line(time.Unix(1000, 0)); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	s.apply([]byte(`{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"other","primary":{"used_percent":100}}}}`))
	if got := s.line(time.Unix(1000, 0)); got != want {
		t.Fatalf("unrelated bucket overwrote status: %s", got)
	}
	s.apply([]byte(`{"type":"event_msg","payload":{"type":"turn_aborted"}}`))
	if s.Active {
		t.Fatal("aborted task still active")
	}
	s.apply([]byte(`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"window_minutes":10080,"used_percent":10}}}}`))
	if strings.Contains(s.line(time.Now()), "5h:") {
		t.Fatal("stale limit retained")
	}
}

func TestLogReaderPartialAppendAndSessionChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	first := `{"type":"turn_context","payload":{"model":"first"}}` + "\n"
	partial := `{"type":"event_msg","payload":{"type":"task_started"`
	if err := os.WriteFile(path, []byte(first+partial), 0600); err != nil {
		t.Fatal(err)
	}
	var reader logReader
	reader.read(path)
	if reader.status.Model != "first" || reader.status.Active {
		t.Fatalf("bad partial read: %+v", reader.status)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("}}\n")
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	reader.read(path)
	if !reader.status.Active {
		t.Fatal("partial record was lost")
	}
	second := filepath.Join(t.TempDir(), "rollout-second.jsonl")
	if err := os.WriteFile(second, []byte(`{"type":"turn_context","payload":{"model":"second"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reader.read(second)
	if reader.status.Model != "second" || reader.status.Active {
		t.Fatal("state leaked between sessions")
	}
}

func TestArgumentsAndStatusEscaping(t *testing.T) {
	args := []string{"resume", "abc", "--", "a prompt with 'quotes' $(touch unwanted)"}
	want := []string{"--no-daemon", "resume", "abc", "-c", "tui.status_line=[]", "--", args[3]}
	if got := codexArgs(args, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	fast := []string{"--no-daemon", "-c", `service_tier="fast"`, "resume", "abc", "-c", "tui.status_line=[]", "--", args[3]}
	if got := codexArgs(args, true); !reflect.DeepEqual(got, fast) {
		t.Fatalf("%q", got)
	}
	if got := escapeStatus("dir#[fg=red]#(echo bad)\n"); got != "dir##[fg=red]##(echo bad)" {
		t.Fatalf("%q", got)
	}
}
