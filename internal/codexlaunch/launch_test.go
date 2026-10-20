package codexlaunch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTmuxLifecycleAndProcessScopedStatus(t *testing.T) {
	for _, tool := range []string{"tmux", "lsof"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "am")
	build := exec.Command("go", "build", "-o", binary, "../..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	fake := `#!/bin/sh
if [ "$1" = "--version" ]; then printf 'codex-cli test\n'; exit; fi
if [ "$1" = "app-server" ]; then exit; fi
printf '%s\n' "$@" > "$AM_TEST_ARGS"
exec 3> "$AM_TEST_LOG"
printf '%s\n' '{"type":"session_meta","payload":{"cwd":"/test/misc","cli_version":"test","source":"cli"}}' >&3
printf '%s\n' '{"type":"turn_context","payload":{"model":"test-model","effort":"high"}}' >&3
printf '%s\n' '{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":123,"output_tokens":45},"last_token_usage":{"total_tokens":100},"model_context_window":1000}}}' >&3
read -r answer
`
	if err := os.WriteFile(filepath.Join(directory, "codex"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	socket := fmt.Sprintf("am-codex-test-%d", time.Now().UnixNano())
	otherSocket := socket + "-other"
	t.Cleanup(func() {
		_ = tmuxCommand(context.Background(), socket, "kill-server").Run()
		_ = tmuxCommand(context.Background(), otherSocket, "kill-server").Run()
	})
	run := func(target string, args ...string) string {
		t.Helper()
		output, err := tmuxCommand(context.Background(), target, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v %s", args, err, output)
		}
		return string(output)
	}
	run(otherSocket, "-f", "/dev/null", "new-session", "-d", "-s", "unrelated", "sleep", "60")
	logPath := filepath.Join(directory, "rollout-test.jsonl")
	argPath := filepath.Join(directory, "args")
	prompt := "literal 'quotes' $(touch unwanted); #value"
	configDir := filepath.Join(directory, "config")
	t.Setenv("AM_CONFIG_DIR", configDir)
	if err := saveStatusConfig(statusConfig{FastMode: true}); err != nil {
		t.Fatal(err)
	}
	run(socket, "-f", "/dev/null", "new-session", "-d", "-x", "180", "-s", "codex", "env", "PATH="+directory+":"+os.Getenv("PATH"), "AM_CONFIG_DIR="+configDir, "AM_TEST_LOG="+logPath, "AM_TEST_ARGS="+argPath, binary, "_launch-codex", socket, "--", prompt)
	waitFor(t, func() bool {
		output, err := tmuxCommand(context.Background(), socket, "show-options", "-gv", "status-left").Output()
		return err == nil && strings.Contains(string(output), "#[fg=colour81,bold]test-model (High)#[default]") && strings.Contains(string(output), "#[fg=colour114]ctx:90.0%#[default]") && !strings.Contains(string(output), "5h:")
	})
	data, err := os.ReadFile(argPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "--no-daemon\n-c\nservice_tier=\"fast\"\n-c\ntui.status_line=[]\n--\n"+prompt+"\n" {
		t.Fatalf("arguments changed: %q", data)
	}
	if mouse := strings.TrimSpace(run(socket, "show-options", "-gv", "mouse")); mouse != "on" {
		t.Fatalf("mouse scrolling disabled: %q", mouse)
	}
	run(socket, "new-window", "-d", "-n", "extra", "sleep", "60")
	run(socket, "send-keys", "-t", "codex:0", "Enter")
	waitFor(t, func() bool { return tmuxCommand(context.Background(), socket, "has-session").Run() != nil })
	run(otherSocket, "has-session")
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timed out waiting for tmux lifecycle/status")
}
