package codexlaunch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type segmentKey string

const (
	segmentModel          segmentKey = "model"
	segmentDirectory      segmentKey = "directory"
	segmentBranch         segmentKey = "branch"
	segmentTokens         segmentKey = "tokens"
	segmentContext        segmentKey = "context"
	segmentContextUsed    segmentKey = "context_used"
	segmentCache          segmentKey = "cache"
	segmentReasoning      segmentKey = "reasoning_tokens"
	segmentLastTurnTokens segmentKey = "last_turn_tokens"
	segmentTotalTokens    segmentKey = "total_tokens"
	segmentTasks          segmentKey = "tasks"
	segmentTurns          segmentKey = "turns"
	segmentTurnTime       segmentKey = "turn_time"
	segmentUptime         segmentKey = "uptime"
	segmentTools          segmentKey = "tools"
	segmentLastCommand    segmentKey = "last_command"
	segmentLimit5h        segmentKey = "limit_5h"
	segmentLimit7d        segmentKey = "limit_7d"
	segmentApproval       segmentKey = "approval"
	segmentSandbox        segmentKey = "sandbox"
	segmentMode           segmentKey = "mode"
	segmentPersonality    segmentKey = "personality"
	segmentServiceTier    segmentKey = "service_tier"
	segmentSessionID      segmentKey = "session_id"
	segmentClock          segmentKey = "clock"
	segmentVersion        segmentKey = "version"
)

type segmentSpec struct {
	key     segmentKey
	label   string
	enabled bool
}

// segmentSpecs is the default order; enabled marks the items shown when no config file exists.
var segmentSpecs = []segmentSpec{
	{segmentModel, "Model (reasoning effort)", true},
	{segmentDirectory, "Working directory", true},
	{segmentBranch, "Git branch (* = uncommitted changes)", false},
	{segmentTokens, "Input / output tokens", true},
	{segmentContext, "Remaining context %", true},
	{segmentContextUsed, "Context used / window", false},
	{segmentCache, "Cached input ratio", false},
	{segmentReasoning, "Reasoning tokens", false},
	{segmentLastTurnTokens, "Last turn in / out tokens", false},
	{segmentTotalTokens, "Total tokens", false},
	{segmentTasks, "Active tasks", true},
	{segmentTurns, "Turn count", false},
	{segmentTurnTime, "Turn time (running or last)", false},
	{segmentUptime, "Launch uptime", false},
	{segmentTools, "Tool call count", false},
	{segmentLastCommand, "Last shell command", false},
	{segmentLimit5h, "5-hour limit", true},
	{segmentLimit7d, "Weekly limit", true},
	{segmentApproval, "Approval policy", false},
	{segmentSandbox, "Sandbox policy", false},
	{segmentMode, "Collaboration mode", false},
	{segmentPersonality, "Personality", false},
	{segmentServiceTier, "Service tier", false},
	{segmentSessionID, "Session id (short)", false},
	{segmentClock, "Current time", false},
	{segmentVersion, "Codex version", true},
}

var segmentLabels = func() map[segmentKey]string {
	labels := map[segmentKey]string{}
	for _, spec := range segmentSpecs {
		labels[spec.key] = spec.label
	}
	return labels
}()

type statusItem struct {
	Key     segmentKey `json:"key"`
	Enabled bool       `json:"enabled"`
}

type statusConfig struct {
	StatusLine []statusItem `json:"status_line"`
	FastMode   bool         `json:"fast_mode"`
}

func defaultStatusConfig() statusConfig {
	config := statusConfig{}
	for _, spec := range segmentSpecs {
		config.StatusLine = append(config.StatusLine, statusItem{Key: spec.key, Enabled: spec.enabled})
	}
	return config
}

func configPath() (string, error) {
	if dir := os.Getenv("AM_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "launch.json"), nil
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "am", "launch.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "am", "launch.json"), nil
}

func loadStatusConfig() statusConfig {
	path, err := configPath()
	if err != nil {
		return defaultStatusConfig()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultStatusConfig()
	}
	var saved statusConfig
	if json.Unmarshal(data, &saved) != nil {
		return defaultStatusConfig()
	}
	return saved.normalized()
}

func saveStatusConfig(config statusConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config.normalized(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

// normalized drops unknown keys and duplicates, then appends missing segments with their default visibility.
func (c statusConfig) normalized() statusConfig {
	seen := map[segmentKey]bool{}
	result := statusConfig{FastMode: c.FastMode}
	for _, item := range c.StatusLine {
		if _, known := segmentLabels[item.Key]; !known || seen[item.Key] {
			continue
		}
		seen[item.Key] = true
		result.StatusLine = append(result.StatusLine, item)
	}
	for _, item := range defaultStatusConfig().StatusLine {
		if !seen[item.Key] {
			result.StatusLine = append(result.StatusLine, item)
		}
	}
	return result
}

func (c statusConfig) validate() error {
	for _, item := range c.StatusLine {
		if item.Enabled {
			return nil
		}
	}
	return errors.New("at least one status line item must be enabled")
}

func (c statusConfig) wants(key segmentKey) bool {
	for _, item := range c.StatusLine {
		if item.Key == key {
			return item.Enabled
		}
	}
	return false
}
