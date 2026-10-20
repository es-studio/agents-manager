package codexlaunch

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type startupOptions struct {
	directory       string
	model           string
	overrides       []string
	configAvailable bool
}

func startupOptionsFor(args []string) startupOptions {
	cwd, _ := os.Getwd()
	options := startupOptions{directory: cwd, configAvailable: true}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		key, value, hasValue := strings.Cut(arg, "=")
		switch key {
		case "-c", "--config", "-m", "--model", "-C", "--cd", "-p", "--profile", "--remote":
			if !hasValue && index+1 < len(args) {
				index++
				value = args[index]
			}
			switch key {
			case "-c", "--config":
				options.overrides = append(options.overrides, "-c", value)
			case "-m", "--model":
				options.model = value
			case "-C", "--cd":
				if filepath.IsAbs(value) {
					options.directory = value
				} else {
					options.directory = filepath.Join(cwd, value)
				}
			case "-p", "--profile", "--remote":
				options.configAvailable = false
			}
		case "resume", "fork", "--oss":
			options.configAvailable = false
		}
	}
	return options
}

func startupStatus(ctx context.Context, options startupOptions, updates chan<- status) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	snapshot := status{Directory: options.directory, Model: options.model}
	publish := func() {
		select {
		case updates <- snapshot:
		case <-ctx.Done():
		}
	}
	version, err := exec.CommandContext(ctx, "codex", "--version").Output()
	if err == nil {
		snapshot.Version = strings.TrimPrefix(strings.TrimSpace(string(version)), "codex-cli ")
	}
	publish()
	if !options.configAvailable {
		return
	}
	command := exec.CommandContext(ctx, "codex", append([]string{"app-server", "--stdio"}, options.overrides...)...)
	command.Dir = options.directory
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	input, err := command.StdinPipe()
	if err != nil {
		return
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return
	}
	if command.Start() != nil {
		input.Close()
		output.Close()
		return
	}
	defer func() { input.Close(); _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL); _ = command.Wait() }()
	encoder := json.NewEncoder(input)
	send := func(id int, method string, params any) error {
		return encoder.Encode(map[string]any{"id": id, "method": method, "params": params})
	}
	if send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "am_statusline", "version": "1"}}) != nil {
		return
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	pending := 2
	for scanner.Scan() {
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) != nil {
			continue
		}
		if response.ID == 1 {
			if len(response.Error) > 0 {
				return
			}
			if encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}) != nil {
				return
			}
			if send(2, "config/read", map[string]any{"cwd": options.directory, "includeLayers": false}) != nil {
				return
			}
			if send(3, "account/rateLimits/read", map[string]any{}) != nil {
				return
			}
			continue
		}
		switch response.ID {
		case 2:
			var result struct {
				Config struct {
					Model  string `json:"model"`
					Effort string `json:"model_reasoning_effort"`
				} `json:"config"`
			}
			if json.Unmarshal(response.Result, &result) == nil {
				if options.model == "" {
					snapshot.Model = result.Config.Model
				}
				snapshot.Effort = result.Config.Effort
			}
		case 3:
			snapshot.Limits = startupLimits(response.Result)
		default:
			continue
		}
		publish()
		pending--
		if pending == 0 {
			return
		}
	}
}

func startupLimits(data []byte) *rateLimits {
	type apiWindow struct {
		Used    float64 `json:"usedPercent"`
		Minutes int     `json:"windowDurationMins"`
		Reset   int64   `json:"resetsAt"`
	}
	type apiLimits struct {
		ID        string     `json:"limitId"`
		Primary   *apiWindow `json:"primary"`
		Secondary *apiWindow `json:"secondary"`
	}
	var result struct {
		Limits  *apiLimits            `json:"rateLimits"`
		Buckets map[string]*apiLimits `json:"rateLimitsByLimitId"`
	}
	if json.Unmarshal(data, &result) != nil {
		return nil
	}
	limits := result.Limits
	if result.Buckets != nil {
		limits = result.Buckets["codex"]
	}
	if limits == nil || (limits.ID != "" && limits.ID != "codex") {
		return nil
	}
	convert := func(window *apiWindow) *rateWindow {
		if window == nil {
			return nil
		}
		return &rateWindow{Used: window.Used, Minutes: window.Minutes, Reset: window.Reset}
	}
	return &rateLimits{ID: limits.ID, Primary: convert(limits.Primary), Secondary: convert(limits.Secondary)}
}

func (s status) withStartup(initial status) status {
	if s.Directory == "" {
		s.Directory = initial.Directory
	}
	if s.Model == "" {
		s.Model, s.Effort = initial.Model, initial.Effort
	}
	if s.Version == "" {
		s.Version = initial.Version
	}
	if s.Limits == nil {
		s.Limits = initial.Limits
	}
	return s
}
