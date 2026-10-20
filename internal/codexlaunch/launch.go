package codexlaunch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
)

const Usage = `Usage: am launch codex [codex arguments...]
       am launch config

Launch Codex in a dedicated tmux server with a custom bottom status line.
Requires tmux, lsof, and Codex with --no-daemon support.
Codex arguments (including resume, --model and --cd) are forwarded.
Exit Codex to close the entire dedicated tmux server.
Ctrl-b d detaches; use the printed command to reattach.
Mouse wheel scrolls; Ctrl-b [ opens tmux scrollback (q to exit).

am launch config opens an interactive editor for the status line:
toggle each item on/off and reorder them. It also toggles fast mode,
which launches Codex with -c service_tier="fast". Saved to
~/.config/am/launch.json (or $XDG_CONFIG_HOME/am/launch.json) and applied
on the next launch.`

func Run(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(Usage)
		return nil
	}
	if args[0] == "config" {
		return runConfigEditor(args[1:])
	}
	if args[0] != "codex" {
		return fmt.Errorf("unsupported launch provider %q; expected codex or config", args[0])
	}
	for _, name := range []string{"tmux", "codex", "lsof"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("am launch requires %s: %w", name, err)
		}
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("am launch requires an interactive terminal")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	socket := fmt.Sprintf("am-codex-%d-%d", os.Getpid(), time.Now().UnixNano())
	command := []string{"-f", "/dev/null", "new-session", "-d", "-s", "codex", "-c", cwd, executable, "_launch-codex", socket}
	command = append(command, args[1:]...)
	if output, err := tmuxCommand(context.Background(), socket, command...).CombinedOutput(); err != nil {
		return fmt.Errorf("create tmux session: %w: %s", err, output)
	}
	fmt.Fprintf(os.Stderr, "Reattach: tmux -L %s attach-session -t codex\n", socket)
	attach := tmuxCommand(context.Background(), socket, "attach-session", "-t", "codex")
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := attach.Run(); err != nil {
		if tmuxCommand(context.Background(), socket, "has-session", "-t", "codex").Run() != nil {
			return nil
		}
		_ = tmuxCommand(context.Background(), socket, "kill-server").Run()
		return fmt.Errorf("attach tmux: %w", err)
	}
	return nil
}

func tmuxCommand(ctx context.Context, socket string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "tmux", append([]string{"-L", socket}, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TMUX=") && !strings.HasPrefix(entry, "TMUX_PANE=") {
			command.Env = append(command.Env, entry)
		}
	}
	return command
}

const fastModeOverride = `service_tier="fast"`

func codexArgs(args []string, fastMode bool) []string {
	result := []string{"--no-daemon"}
	if fastMode {
		result = append(result, "-c", fastModeOverride)
	}
	for index, arg := range args {
		if arg == "--" {
			result = append(result, "-c", "tui.status_line=[]")
			return append(result, args[index:]...)
		}
		result = append(result, arg)
	}
	return append(result, "-c", "tui.status_line=[]")
}

func RunPane(args []string) error {
	if len(args) == 0 || !strings.HasPrefix(args[0], "am-codex-") {
		return fmt.Errorf("invalid launch socket")
	}
	socket := args[0]
	options := startupOptionsFor(args[1:])
	config := loadStatusConfig()
	initial := status{Directory: options.directory, Model: options.model, LaunchedAt: time.Now()}
	defer func() { _ = tmuxCommand(context.Background(), socket, "kill-server").Run() }()
	settings := []string{
		"set-option", "-g", "mouse", "on", ";",
		"set-option", "-g", "status-left", initial.tmuxLine(config, time.Now()), ";",
		"set-option", "-g", "status-position", "bottom", ";",
		"set-option", "-g", "status", "on", ";",
		"set-option", "-g", "status-style", "bg=default,fg=default", ";",
		"set-option", "-g", "status-left-length", "10000", ";",
		"set-option", "-g", "status-format[0]", "#{status-left}", ";",
		"set-window-option", "-g", "remain-on-exit", "off",
	}
	if output, err := tmuxCommand(context.Background(), socket, settings...).CombinedOutput(); err != nil {
		return fmt.Errorf("configure tmux: %w: %s", err, output)
	}
	command := exec.Command("codex", codexArgs(args[1:], config.FastMode)...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		updateStatus(ctx, socket, command.Process.Pid, options, config)
	}()
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	var err error
	select {
	case err = <-exited:
	case <-ctx.Done():
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case err = <-exited:
		case <-time.After(3 * time.Second):
			_ = command.Process.Kill()
			err = <-exited
		}
	}
	stop()
	<-done
	return err
}

func updateStatus(ctx context.Context, socket string, pid int, options startupOptions, config statusConfig) {
	launchedAt := time.Now()
	initial := status{Directory: options.directory, Model: options.model}
	updates := make(chan status, 3)
	startupDone := make(chan struct{})
	go func() { defer close(startupDone); startupStatus(ctx, options, updates) }()
	defer func() { <-startupDone }()
	reader := logReader{}
	git := gitState{}
	render := func() {
		poll, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		now := time.Now()
		display := reader.status.withStartup(initial)
		if config.wants(segmentBranch) {
			git.refresh(poll, display.Directory, now)
		}
		display.Branch, display.Dirty, display.LaunchedAt = git.branch, git.dirty, launchedAt
		_ = tmuxCommand(poll, socket, "set-option", "-g", "status-left", display.tmuxLine(config, now)).Run()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		poll, cancel := context.WithTimeout(ctx, 2*time.Second)
		path := processRollout(poll, pid)
		cancel()
		if path != "" {
			reader.read(path)
		}
		render()
		select {
		case <-ctx.Done():
			return
		case initial = <-updates:
			render()
		case <-ticker.C:
		}
	}
}

func processRollout(ctx context.Context, pid int) string {
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return ""
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		child, _ := strconv.Atoi(fields[0])
		parent, _ := strconv.Atoi(fields[1])
		children[parent] = append(children[parent], child)
	}
	pids := []int{pid}
	for index := 0; index < len(pids); index++ {
		pids = append(pids, children[pids[index]]...)
	}
	ids := make([]string, len(pids))
	for index, id := range pids {
		ids[index] = strconv.Itoa(id)
	}
	output, _ = exec.CommandContext(ctx, "lsof", "-nP", "-a", "-p", strings.Join(ids, ","), "-Fn").Output()
	var selected string
	var newest time.Time
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "n/") {
			continue
		}
		path := line[1:]
		if !strings.HasPrefix(filepath.Base(path), "rollout-") || !strings.HasSuffix(path, ".jsonl") {
			continue
		}
		if !isCLIRollout(path) {
			continue
		}
		info, err := os.Stat(path)
		if err == nil && info.ModTime().After(newest) {
			selected, newest = path, info.ModTime()
		}
	}
	return selected
}

func escapeStatus(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
	return strings.ReplaceAll(value, "#", "##")
}
