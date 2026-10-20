package codexlaunch

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

var (
	configTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#4F46E5")).Padding(0, 1)
	configCursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#4F46E5"))
	configEnabledStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#D1D5DB"))
	configDisabledStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
	configSampleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#9CA3AF"))
	configPreviewStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#06B6D4"))
	configHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
)

func runConfigEditor(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("am launch config takes no arguments")
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("am launch config requires an interactive terminal")
	}
	model := newConfigModel(loadStatusConfig())
	result, err := tea.NewProgram(model).Run()
	if err != nil {
		return err
	}
	final := result.(configModel)
	if !final.saved {
		fmt.Fprintln(os.Stderr, "Status line config unchanged.")
		return nil
	}
	if err := saveStatusConfig(final.config); err != nil {
		return err
	}
	path, _ := configPath()
	fmt.Fprintf(os.Stderr, "Saved status line config to %s\n", path)
	return nil
}

type configModel struct {
	config  statusConfig
	cursor  int
	message string
	saved   bool
}

func newConfigModel(config statusConfig) configModel {
	return configModel{config: config.normalized()}
}

func (m configModel) Init() tea.Cmd { return nil }

func (m configModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	items := m.config.StatusLine
	m.message = ""
	switch key.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(items)-1 {
			m.cursor++
		}
	case " ", "x":
		items[m.cursor].Enabled = !items[m.cursor].Enabled
	case "shift+up", "K":
		if m.cursor > 0 {
			items[m.cursor-1], items[m.cursor] = items[m.cursor], items[m.cursor-1]
			m.cursor--
		}
	case "shift+down", "J":
		if m.cursor < len(items)-1 {
			items[m.cursor+1], items[m.cursor] = items[m.cursor], items[m.cursor+1]
			m.cursor++
		}
	case "f":
		m.config.FastMode = !m.config.FastMode
	case "r":
		m.config = defaultStatusConfig()
		m.message = "Reset to defaults (not saved yet)."
	case "enter":
		if err := m.config.validate(); err != nil {
			m.message = err.Error()
			return m, nil
		}
		m.saved = true
		return m, tea.Quit
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func (m configModel) View() string {
	var b strings.Builder
	b.WriteString(configTitleStyle.Render("am launch codex - status line"))
	b.WriteString("\n\n")
	sample := sampleStatus()
	samples := map[segmentKey]string{}
	for _, segment := range sample.segments(sampleNow) {
		samples[segment.key] = segment.text
	}
	for index, item := range m.config.StatusLine {
		check := "[ ]"
		if item.Enabled {
			check = "[x]"
		}
		row := fmt.Sprintf(" %s %-38s", check, segmentLabels[item.Key])
		style := configDisabledStyle
		if item.Enabled {
			style = configEnabledStyle
		}
		if index == m.cursor {
			row = "> " + row
			style = configCursorStyle
		} else {
			row = "  " + row
		}
		b.WriteString(style.Render(row))
		b.WriteString(configSampleStyle.Render("  " + samples[item.Key]))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(configPreviewStyle.Render("Preview: " + sample.lineWith(m.config, sampleNow)))
	b.WriteString("\n\n")
	b.WriteString(m.fastModeRow())
	b.WriteString("\n")
	if m.message != "" {
		b.WriteString(m.message)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(configHelpStyle.Render("up/down move  space toggle  shift+up/down (K/J) reorder  f fast mode  r reset  enter save  q cancel"))
	b.WriteString("\n")
	return b.String()
}

func (m configModel) fastModeRow() string {
	check, style := "[ ]", configDisabledStyle
	if m.config.FastMode {
		check, style = "[x]", configEnabledStyle
	}
	return style.Render(fmt.Sprintf("  %s %-38s", check, "Fast mode")) + configSampleStyle.Render("  -c "+fastModeOverride)
}

var sampleNow = time.Unix(1_700_000_000, 0)

func sampleStatus() status {
	return status{
		Directory:   "/work/project",
		Version:     "0.161.0",
		Model:       "gpt-5-codex",
		Effort:      "high",
		SessionID:   "01a1342a-0941-79a3-a79f-3b55dc6fb2ea",
		Approval:    "on-request",
		Sandbox:     "workspace-write",
		Personality: "pragmatic",
		Mode:        "default",
		ServiceTier: "priority",
		Active:      true,
		Turns:       12,
		Tools:       87,
		TurnStarted: sampleNow.Add(-72 * time.Second),
		LastCommand: "go test ./... -timeout 60s",
		Branch:      "main",
		Dirty:       true,
		LaunchedAt:  sampleNow.Add(-(1*time.Hour + 23*time.Minute)),
		Usage: &usageInfo{
			Total:  tokenUsage{Input: 48_200, Cached: 31_000, Output: 6_100, Reasoning: 2_400, Total: 54_300},
			Last:   tokenUsage{Input: 50_000, Output: 2_000, Total: 52_000},
			Window: 258_000,
		},
		Limits: &rateLimits{
			Primary:   &rateWindow{Used: 18, Minutes: 300, Reset: sampleNow.Unix() + 2*3600 + 15*60},
			Secondary: &rateWindow{Used: 41, Minutes: 10080, Reset: sampleNow.Unix() + 3*86400 + 4*3600},
		},
	}
}
