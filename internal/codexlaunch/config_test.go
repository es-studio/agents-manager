package codexlaunch

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestStatusConfigRoundTripAndOrdering(t *testing.T) {
	t.Setenv("AM_CONFIG_DIR", t.TempDir())
	total := len(segmentSpecs)
	if got := loadStatusConfig(); len(got.StatusLine) != total || !got.StatusLine[0].Enabled || got.StatusLine[0].Key != segmentModel {
		t.Fatalf("missing config should yield defaults: %+v", got)
	}
	custom := statusConfig{FastMode: true, StatusLine: []statusItem{
		{Key: segmentVersion, Enabled: true},
		{Key: segmentTokens, Enabled: false},
		{Key: "bogus", Enabled: true},
		{Key: segmentVersion, Enabled: false},
		{Key: segmentModel, Enabled: true},
	}}
	if err := saveStatusConfig(custom); err != nil {
		t.Fatal(err)
	}
	loaded := loadStatusConfig()
	if len(loaded.StatusLine) != total {
		t.Fatalf("unknown or duplicate keys retained, missing keys not appended: %+v", loaded)
	}
	if !loaded.FastMode {
		t.Fatalf("fast mode lost: %+v", loaded)
	}
	if loaded.StatusLine[0].Key != segmentVersion || loaded.StatusLine[1].Key != segmentTokens || loaded.StatusLine[2].Key != segmentModel {
		t.Fatalf("order lost: %+v", loaded)
	}
	if loaded.StatusLine[3].Key != segmentDirectory || !loaded.StatusLine[3].Enabled || loaded.StatusLine[4].Key != segmentBranch || loaded.StatusLine[4].Enabled {
		t.Fatalf("appended missing keys should follow default order and visibility: %+v", loaded)
	}
	var s status
	s.apply([]byte(`{"type":"session_meta","payload":{"cwd":"/work/misc","cli_version":"0.161.0","source":"cli"}}`))
	s.apply([]byte(`{"type":"turn_context","payload":{"model":"gpt-test","effort":"high"}}`))
	only := statusConfig{StatusLine: []statusItem{{Key: segmentVersion, Enabled: true}, {Key: segmentLimit5h, Enabled: true}, {Key: segmentModel, Enabled: true}}}
	if got := s.lineWith(only, time.Unix(1000, 0)); got != "v0.161.0 · gpt-test (High)" {
		t.Fatalf("got %q", got)
	}
	if got := s.tmuxLine(only, time.Unix(1000, 0)); got != "#[fg=colour245]v0.161.0#[default]#[fg=colour245] · #[default]#[fg=colour81,bold]gpt-test (High)#[default]" {
		t.Fatalf("got %q", got)
	}
}

func TestExtendedSegments(t *testing.T) {
	var s status
	for _, event := range []string{
		`{"timestamp":"2026-10-13T07:28:52.573Z","type":"session_meta","payload":{"id":"01a1342a-0941-79a3-a79f-3b55dc6fb2ea","cwd":"/work/misc","cli_version":"0.162.0","source":"cli"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1","root_turn_id":"t1","model":"gpt-test","effort":"medium","approval_policy":"on-request","sandbox_policy":{"type":"workspace-write"},"personality":"pragmatic","collaboration_mode":{"mode":"plan"}}}`,
		`{"timestamp":"2026-10-13T07:30:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1","root_turn_id":"t1"}}`,
		`{"timestamp":"2026-10-13T07:30:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"sub","root_turn_id":"t1"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec"}}`,
		`{"type":"response_item","payload":{"type":"message"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/zsh","-lc","go   test ./... -run TestSomethingVeryLongName -timeout 60s"]}}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":40000,"cached_input_tokens":30000,"output_tokens":5000,"reasoning_output_tokens":1500,"total_tokens":45000},"last_token_usage":{"input_tokens":20000,"output_tokens":500,"total_tokens":20500},"model_context_window":100000}}}`,
	} {
		s.apply([]byte(event))
	}
	s.Branch, s.Dirty = "main", true
	s.LaunchedAt = time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 13, 7, 31, 30, 0, time.UTC)
	config := statusConfig{}
	for _, spec := range segmentSpecs {
		config.StatusLine = append(config.StatusLine, statusItem{Key: spec.key, Enabled: true})
	}
	want := "gpt-test (Medium) · misc · main* · in:40.0k / out:5.0k · ctx:79.5% · ctx:20.5k/100.0k · cache:75% · reason:1.5k · last:20.0k / 500 · total:45.0k · tasks:1 · turns:1 · turn:1m 30s · up:1h 31m · tools:2 · $ go test ./... -run TestSometh... · approval:on-request · sandbox:workspace-write · mode:plan · persona:pragmatic · tier:priority · 01a1342a · " + now.Local().Format("15:04") + " · v0.162.0"
	if got := s.lineWith(config, now); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	s.apply([]byte(`{"timestamp":"2026-10-13T07:32:10Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","root_turn_id":"t1"}}`))
	if line := s.lineWith(config, now); !strings.Contains(line, "turn:2m 10s") || !strings.Contains(line, "tasks:0") {
		t.Fatal(line)
	}
	if got := shortCommand("ls"); got != "ls" {
		t.Fatal(got)
	}
}

func TestConfigEditorKeys(t *testing.T) {
	press := func(m configModel, keys ...string) configModel {
		for _, key := range keys {
			var msg tea.KeyMsg
			switch key {
			case " ":
				msg = tea.KeyMsg{Type: tea.KeySpace}
			case "enter":
				msg = tea.KeyMsg{Type: tea.KeyEnter}
			case "shift+down":
				msg = tea.KeyMsg{Type: tea.KeyShiftDown}
			default:
				msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
			}
			next, _ := m.Update(msg)
			m = next.(configModel)
		}
		return m
	}
	m := press(newConfigModel(defaultStatusConfig()), "j", " ", "shift+down", "J")
	items := m.config.StatusLine
	if items[0].Key != segmentModel || items[1].Key != segmentBranch || items[2].Key != segmentTokens || items[3].Key != segmentDirectory || items[3].Enabled {
		t.Fatalf("toggle/reorder wrong: %+v", items)
	}
	if m.cursor != 3 {
		t.Fatalf("cursor should follow moved item, got %d", m.cursor)
	}
	all := newConfigModel(defaultStatusConfig())
	for index := range all.config.StatusLine {
		if all.config.StatusLine[index].Enabled {
			all = press(all, " ")
		}
		all = press(all, "j")
	}
	all = press(all, "enter")
	if all.saved || all.message == "" {
		t.Fatalf("saving with nothing enabled must be refused: %+v", all)
	}
	all = press(all, "r", "enter")
	if !all.saved || !all.config.StatusLine[0].Enabled {
		t.Fatalf("reset then save failed: %+v", all)
	}
	fast := press(newConfigModel(defaultStatusConfig()), "f")
	if !fast.config.FastMode || !strings.Contains(fast.View(), "[x] Fast mode") {
		t.Fatalf("fast mode toggle failed: %+v", fast)
	}
	if fast = press(fast, "f"); fast.config.FastMode {
		t.Fatalf("fast mode should toggle off: %+v", fast)
	}
	if view := newConfigModel(defaultStatusConfig()).View(); !strings.Contains(view, "[x] Model (reasoning effort)") || !strings.Contains(view, "[ ] Fast mode") || !strings.Contains(view, "Preview: gpt-5-codex (High) · project · in:48.2k / out:6.1k · ctx:79.8% · tasks:1 · 5h:82% (2h 15m) · 7d:59% (3d 4h) · v0.161.0") {
		t.Fatalf("unexpected view:\n%s", view)
	}
}
