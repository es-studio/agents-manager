package codexlaunch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type tokenUsage struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
	Total     int64 `json:"total_tokens"`
}

type usageInfo struct {
	Total  tokenUsage `json:"total_token_usage"`
	Last   tokenUsage `json:"last_token_usage"`
	Window int64      `json:"model_context_window"`
}

type rateWindow struct {
	Used    float64 `json:"used_percent"`
	Minutes int     `json:"window_minutes"`
	Reset   int64   `json:"resets_at"`
}

type rateLimits struct {
	ID        string      `json:"limit_id"`
	Primary   *rateWindow `json:"primary"`
	Secondary *rateWindow `json:"secondary"`
	Plan      string      `json:"plan_type"`
}

type logEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type       string          `json:"type"`
		Source     json.RawMessage `json:"source"`
		SessionID  string          `json:"id"`
		Directory  string          `json:"cwd"`
		Version    string          `json:"cli_version"`
		Model      string          `json:"model"`
		Effort     string          `json:"effort"`
		Info       *usageInfo      `json:"info"`
		Limits     *rateLimits     `json:"rate_limits"`
		TurnID     string          `json:"turn_id"`
		RootTurnID string          `json:"root_turn_id"`
		Approval   string          `json:"approval_policy"`
		Sandbox    struct {
			Type string `json:"type"`
		} `json:"sandbox_policy"`
		FileSandbox struct {
			Kind string `json:"kind"`
		} `json:"file_system_sandbox_policy"`
		Personality   string `json:"personality"`
		Collaboration struct {
			Mode string `json:"mode"`
		} `json:"collaboration_mode"`
		CollaborationKind string `json:"collaboration_mode_kind"`
		ThreadSettings    struct {
			ServiceTier string `json:"service_tier"`
		} `json:"thread_settings"`
		Item struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
		} `json:"item"`
	} `json:"payload"`
}

type status struct {
	Directory, Version, Model, Effort string
	SessionID                         string
	Approval, Sandbox, Personality    string
	Mode, ServiceTier                 string
	Usage                             *usageInfo
	Limits                            *rateLimits
	Active                            bool
	Turns, Tools                      int
	TurnStarted                       time.Time
	LastTurn                          time.Duration
	LastCommand                       string

	// Filled by the launcher, not by the rollout log.
	Branch     string
	Dirty      bool
	LaunchedAt time.Time
}

func (s *status) apply(data []byte) {
	var event logEvent
	if json.Unmarshal(data, &event) != nil {
		return
	}
	p := event.Payload
	at, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
	topLevel := p.RootTurnID == "" || p.RootTurnID == p.TurnID
	switch event.Type {
	case "session_meta":
		s.Directory, s.Version, s.SessionID = p.Directory, p.Version, p.SessionID
	case "turn_context":
		s.Model, s.Effort = p.Model, p.Effort
		if p.Directory != "" {
			s.Directory = p.Directory
		}
		if p.Approval != "" {
			s.Approval = p.Approval
		}
		if p.Sandbox.Type != "" {
			s.Sandbox = p.Sandbox.Type
		} else if p.FileSandbox.Kind != "" {
			s.Sandbox = p.FileSandbox.Kind
		}
		if p.Personality != "" {
			s.Personality = p.Personality
		}
		if p.Collaboration.Mode != "" {
			s.Mode = p.Collaboration.Mode
		}
	case "response_item":
		switch p.Type {
		case "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
			s.Tools++
		}
	case "event_msg":
		switch p.Type {
		case "task_started":
			s.Active = true
			if p.CollaborationKind != "" {
				s.Mode = p.CollaborationKind
			}
			if topLevel {
				s.Turns++
				s.TurnStarted = at
			}
		case "task_complete", "turn_aborted":
			s.Active = false
			if topLevel && !s.TurnStarted.IsZero() && !at.IsZero() {
				s.LastTurn = at.Sub(s.TurnStarted)
				s.TurnStarted = time.Time{}
			}
		case "token_count":
			if p.Info != nil {
				s.Usage = p.Info
			}
			if p.Limits != nil && (p.Limits.ID == "" || p.Limits.ID == "codex") {
				s.Limits = p.Limits
			}
		case "thread_settings_applied":
			if p.ThreadSettings.ServiceTier != "" {
				s.ServiceTier = p.ThreadSettings.ServiceTier
			}
		case "item_completed":
			if p.Item.Type == "CommandExecution" && len(p.Item.Command) > 0 {
				s.LastCommand = p.Item.Command[len(p.Item.Command)-1]
			}
		}
	}
}

func known(value string) string {
	if value == "" {
		return "--"
	}
	return value
}

type statusSegment struct {
	key   segmentKey
	text  string
	style string
}

func formatTokenCount(count int64) string {
	if count >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	}
	if count >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(count)/1_000)
	}
	return fmt.Sprintf("%d", count)
}

func formatDuration(duration time.Duration) string {
	seconds := int(duration.Round(time.Second).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds >= 3600:
		return fmt.Sprintf("%dh %dm", seconds/3600, seconds%3600/60)
	case seconds >= 60:
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func shortCommand(command string) string {
	command = strings.Join(strings.Fields(command), " ")
	if len(command) > 32 {
		return command[:29] + "..."
	}
	return command
}

const dimStyle = "fg=colour245"

func (s status) segments(now time.Time) []statusSegment {
	model := known(s.Model)
	if s.Effort != "" {
		model += " (" + strings.ToUpper(s.Effort[:1]) + s.Effort[1:] + ")"
	}
	tokens, context, contextUsed := "in:-- / out:--", "ctx:--", "ctx:--"
	cache, reasoning, lastTurnTokens, totalTokens := "cache:--", "reason:--", "last:--", "total:--"
	contextStyle := dimStyle
	if s.Usage != nil {
		tokens = fmt.Sprintf("in:%s / out:%s", formatTokenCount(s.Usage.Total.Input), formatTokenCount(s.Usage.Total.Output))
		reasoning = "reason:" + formatTokenCount(s.Usage.Total.Reasoning)
		totalTokens = "total:" + formatTokenCount(s.Usage.Total.Total)
		lastTurnTokens = fmt.Sprintf("last:%s / %s", formatTokenCount(s.Usage.Last.Input), formatTokenCount(s.Usage.Last.Output))
		if s.Usage.Total.Input > 0 {
			cache = fmt.Sprintf("cache:%.0f%%", 100*float64(s.Usage.Total.Cached)/float64(s.Usage.Total.Input))
		}
		if s.Usage.Window > 0 {
			percent := remaining(100 * float64(s.Usage.Last.Total) / float64(s.Usage.Window))
			context = fmt.Sprintf("ctx:%.1f%%", percent)
			contextUsed = fmt.Sprintf("ctx:%s/%s", formatTokenCount(s.Usage.Last.Total), formatTokenCount(s.Usage.Window))
			contextStyle = remainingStyle(percent)
		}
	}
	tasks, taskStyle := 0, dimStyle
	turnTime, turnStyle := "turn:--", dimStyle
	if s.Active {
		tasks, taskStyle = 1, "fg=colour221,bold"
		if !s.TurnStarted.IsZero() {
			turnTime, turnStyle = "turn:"+formatDuration(now.Sub(s.TurnStarted)), "fg=colour221"
		}
	} else if s.LastTurn > 0 {
		turnTime = "turn:" + formatDuration(s.LastTurn)
	}
	directory := "--"
	if s.Directory != "" {
		directory = filepath.Base(s.Directory)
	}
	branch := "--"
	if s.Branch != "" {
		branch = s.Branch
		if s.Dirty {
			branch += "*"
		}
	}
	uptime := "up:--"
	if !s.LaunchedAt.IsZero() {
		uptime = "up:" + formatDuration(now.Sub(s.LaunchedAt))
	}
	sessionID := "--"
	if len(s.SessionID) >= 8 {
		sessionID = s.SessionID[:8]
	}
	lastCommand := "$ --"
	if s.LastCommand != "" {
		lastCommand = "$ " + shortCommand(s.LastCommand)
	}
	segments := []statusSegment{
		{segmentModel, model, "fg=colour81,bold"},
		{segmentDirectory, directory, "fg=colour141"},
		{segmentBranch, branch, "fg=colour175"},
		{segmentTokens, tokens, "fg=colour109"},
		{segmentContext, context, contextStyle},
		{segmentContextUsed, contextUsed, contextStyle},
		{segmentCache, cache, "fg=colour109"},
		{segmentReasoning, reasoning, "fg=colour109"},
		{segmentLastTurnTokens, lastTurnTokens, "fg=colour109"},
		{segmentTotalTokens, totalTokens, "fg=colour109"},
		{segmentTasks, fmt.Sprintf("tasks:%d", tasks), taskStyle},
		{segmentTurns, fmt.Sprintf("turns:%d", s.Turns), dimStyle},
		{segmentTurnTime, turnTime, turnStyle},
		{segmentUptime, uptime, dimStyle},
		{segmentTools, fmt.Sprintf("tools:%d", s.Tools), dimStyle},
		{segmentLastCommand, lastCommand, "fg=colour180"},
	}
	var fiveHour, weekly *rateWindow
	if s.Limits != nil {
		for _, window := range []*rateWindow{s.Limits.Primary, s.Limits.Secondary} {
			if window == nil {
				continue
			}
			switch window.Minutes {
			case 300:
				fiveHour = window
			case 10080:
				weekly = window
			}
		}
	}
	if fiveHour != nil {
		segments = append(segments, statusSegment{segmentLimit5h, "5h:" + windowText(fiveHour, now), remainingStyle(remaining(fiveHour.Used))})
	}
	if weekly != nil {
		segments = append(segments, statusSegment{segmentLimit7d, "7d:" + windowText(weekly, now), remainingStyle(remaining(weekly.Used))})
	}
	return append(segments,
		statusSegment{segmentApproval, "approval:" + known(s.Approval), "fg=colour146"},
		statusSegment{segmentSandbox, "sandbox:" + known(s.Sandbox), sandboxStyle(s.Sandbox)},
		statusSegment{segmentMode, "mode:" + known(s.Mode), "fg=colour146"},
		statusSegment{segmentPersonality, "persona:" + known(s.Personality), "fg=colour146"},
		statusSegment{segmentServiceTier, "tier:" + known(s.ServiceTier), "fg=colour146"},
		statusSegment{segmentSessionID, sessionID, dimStyle},
		statusSegment{segmentClock, now.Local().Format("15:04"), dimStyle},
		statusSegment{segmentVersion, "v" + known(s.Version), dimStyle},
	)
}

func sandboxStyle(sandbox string) string {
	if sandbox == "danger-full-access" {
		return "fg=colour203,bold"
	}
	return "fg=colour146"
}

// arranged keeps only enabled segments, in the configured order; unavailable segments (e.g. missing quota windows) stay omitted.
func (s status) arranged(config statusConfig, now time.Time) []statusSegment {
	available := map[segmentKey]statusSegment{}
	for _, segment := range s.segments(now) {
		available[segment.key] = segment
	}
	var result []statusSegment
	for _, item := range config.StatusLine {
		segment, ok := available[item.Key]
		if !item.Enabled || !ok {
			continue
		}
		result = append(result, segment)
	}
	return result
}

func (s status) line(now time.Time) string {
	return s.lineWith(defaultStatusConfig(), now)
}

func (s status) lineWith(config statusConfig, now time.Time) string {
	var parts []string
	for _, segment := range s.arranged(config, now) {
		parts = append(parts, segment.text)
	}
	return strings.Join(parts, " · ")
}

func (s status) tmuxLine(config statusConfig, now time.Time) string {
	var parts []string
	for _, segment := range s.arranged(config, now) {
		parts = append(parts, "#["+segment.style+"]"+escapeStatus(segment.text)+"#[default]")
	}
	return strings.Join(parts, "#[fg=colour245] · #[default]")
}

func remainingStyle(percent float64) string {
	if percent <= 10 {
		return "fg=colour203,bold"
	}
	if percent <= 25 {
		return "fg=colour221"
	}
	return "fg=colour114"
}

func remaining(used float64) float64 { return math.Max(0, math.Min(100, 100-used)) }

func windowText(window *rateWindow, now time.Time) string {
	countdown := "--"
	if window.Reset > 0 {
		seconds := math.Max(0, float64(window.Reset-now.Unix()))
		minutes := int(math.Ceil(seconds / 60))
		switch {
		case minutes >= 1440:
			countdown = fmt.Sprintf("%dd %dh", minutes/1440, minutes%1440/60)
		case minutes >= 60 && minutes%60 != 0:
			countdown = fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
		case minutes >= 60:
			countdown = fmt.Sprintf("%dh", minutes/60)
		default:
			countdown = fmt.Sprintf("%dm", minutes)
		}
	}
	return fmt.Sprintf("%.0f%% (%s)", remaining(window.Used), countdown)
}

type logReader struct {
	path   string
	offset int64
	status status
}

func (r *logReader) read(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return
	}
	if r.path != path || info.Size() < r.offset {
		r.path, r.offset = path, 0
		r.status = status{}
	}
	if _, err := file.Seek(r.offset, io.SeekStart); err != nil {
		return
	}
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		r.offset += int64(len(line))
		r.status.apply(line)
	}
}

func isCLIRollout(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	line, err := bufio.NewReader(io.LimitReader(file, 1024*1024)).ReadBytes('\n')
	if err != nil {
		return false
	}
	var event logEvent
	if json.Unmarshal(line, &event) != nil || event.Type != "session_meta" {
		return false
	}
	return string(event.Payload.Source) == `"cli"`
}
