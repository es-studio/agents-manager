package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func wantTUI(noTUI, tui bool) bool {
	if noTUI {
		return false
	}
	if tui {
		return true
	}
	return isatty(0) && isatty(1)
}

func isatty(fd int) bool {
	var term syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&term)))
	return err == 0
}

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func getTermSize() (int, int, error) {
	var ws winsize
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(syscall.Stdout), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if err != 0 {
		return 120, 24, err
	}
	w := int(ws.Col)
	h := int(ws.Row)
	if w <= 0 {
		w = 120
	}
	if h <= 0 {
		h = 24
	}
	return w, h, nil
}

type tuiModel struct {
	items      []Session
	selected   int
	dirFilter  string
	width      int
	height     int
	action     string // "id", "copy-context"
	selectedID string
	ok         bool
	quitting   bool

	// Tabs support
	tabs       []string
	currentTab int
	allItems   []Session
}

func (m tuiModel) Init() tea.Cmd {
	return nil
}

func (m tuiModel) filterItemsByTab() tuiModel {
	if len(m.tabs) == 0 {
		return m
	}
	tabProvider := m.tabs[m.currentTab]
	if strings.EqualFold(tabProvider, "all") {
		m.items = m.allItems
		m.selected = 0
		return m
	}
	var filtered []Session
	for _, it := range m.allItems {
		if strings.EqualFold(it.Provider, tabProvider) {
			filtered = append(filtered, it)
		}
	}
	m.items = filtered
	m.selected = 0
	return m
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "left", "h", "shift+tab":
			if len(m.tabs) > 0 {
				m.currentTab = (m.currentTab - 1 + len(m.tabs)) % len(m.tabs)
				m = m.filterItemsByTab()
			}
		case "right", "l", "tab":
			if len(m.tabs) > 0 {
				m.currentTab = (m.currentTab + 1) % len(m.tabs)
				m = m.filterItemsByTab()
			}
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.items)-1 {
				m.selected++
			}
		case "enter":
			if len(m.items) > 0 && m.selected >= 0 && m.selected < len(m.items) {
				m.action = "id"
				m.selectedID = m.items[m.selected].ID
				m.ok = true
				m.quitting = true
				return m, tea.Quit
			}
		case "c":
			if len(m.items) > 0 && m.selected >= 0 && m.selected < len(m.items) {
				m.action = "copy-context"
				m.selectedID = m.items[m.selected].ID
				m.ok = true
				m.quitting = true
				return m, tea.Quit
			}
		case "q", "esc", "ctrl+c":
			m.ok = false
			m.quitting = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}
	return m, nil
}

var (
	// Accent colors
	accentColor = lipgloss.Color("#4F46E5") // Indigo
	cyanColor   = lipgloss.Color("#06B6D4") // Cyan
	grayColor   = lipgloss.Color("#6B7280") // Gray
	yellowColor = lipgloss.Color("#F59E0B") // Yellow/Amber

	titleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(accentColor).
		Padding(0, 1)

	filterStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(cyanColor)

	helpStyle = lipgloss.NewStyle().
		Foreground(grayColor)

	selectedRowStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(accentColor)

	normalRowStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#D1D5DB"))

	providerStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(yellowColor)

	projectStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#38BDF8")) // Sky Blue

	previewHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(cyanColor)

	previewBodyStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#9CA3AF"))

	activeTabStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(accentColor).
		Padding(0, 2).
		MarginRight(1)

	inactiveTabStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#9CA3AF")).
		Background(lipgloss.Color("#374151")).
		Padding(0, 2).
		MarginRight(1)
)

func (m tuiModel) View() string {
	if m.quitting {
		return ""
	}

	if m.width == 0 {
		w, h, _ := getTermSize()
		m.width = w
		m.height = h
	}

	width := m.width - 2
	if width < 1 {
		width = 1
	}

	height := m.height
	if height <= 0 {
		height = 24
	}

	var sb strings.Builder

	// 1. Header Line
	headerTitle := titleStyle.Render("agents-manager")
	headerFilter := ""
	if m.dirFilter != "" {
		headerFilter = " | " + filterStyle.Render(m.dirFilter)
	}
	sb.WriteString(headerTitle + headerFilter + "\n")

	// 2. Help Instructions Line
	helpDesc := "Up/Down (or k/j) navigate | Enter select ID | C copy context | Q/Esc quit"
	if len(m.tabs) > 0 {
		helpDesc = "Up/Down (or k/j) navigate | Left/Right (or h/l, Tab) switch tabs | Enter select ID | C copy context | Q/Esc quit"
	}
	helpText := helpStyle.Render(helpDesc)
	sb.WriteString(helpText + "\n\n")

	// Tabs Line
	if len(m.tabs) > 0 {
		var tabsStr strings.Builder
		for idx, t := range m.tabs {
			tabLabel := strings.ToUpper(t)
			if idx == m.currentTab {
				tabsStr.WriteString(activeTabStyle.Render(tabLabel))
			} else {
				tabsStr.WriteString(inactiveTabStyle.Render(tabLabel))
			}
		}
		sb.WriteString(tabsStr.String() + "\n\n")
	}

	// Column widths
	selW := 2
	idW := 36
	provW := 4
	updW := 16
	projW := 26
	minTitleW := 10

	totalW := func(iW, pW, tW int) int {
		return selW + iW + 1 + provW + 1 + updW + 1 + pW + 1 + tW
	}

	titleW := width - totalW(idW, projW, 0)
	if titleW < minTitleW {
		need := minTitleW - titleW
		projW = projW - need
		if projW < 0 {
			projW = 0
		}
		titleW = width - totalW(idW, projW, 0)
	}
	if titleW < minTitleW {
		need := minTitleW - titleW
		idW = idW - need
		if idW < 8 {
			idW = 8
		}
		titleW = width - totalW(idW, projW, 0)
	}
	if idW < 8 {
		idW = 8
	}
	if projW < 0 {
		projW = 0
	}
	if titleW < 0 {
		titleW = 0
	}

	// 3. Viewport calculation (paging)
	maxItems := 10
	startIdx := 0
	if m.selected >= maxItems {
		startIdx = m.selected - maxItems + 1
	}
	endIdx := startIdx + maxItems
	if endIdx > len(m.items) {
		endIdx = len(m.items)
	}

	// Draw table rows
	if len(m.items) == 0 {
		sb.WriteString("  " + normalRowStyle.Render("(no sessions found)") + "\n")
		// Pad blank lines to keep viewport height static
		for i := 1; i < maxItems; i++ {
			sb.WriteString("\n")
		}
	} else {
		for i := startIdx; i < endIdx; i++ {
			s := m.items[i]
			prefix := "  "
			isSelected := (i == m.selected)
			if isSelected {
				prefix = "> "
			}

			proj := s.ProjectRoot
			if proj == "" {
				proj = s.Project
			}
			// Extract base dir name to make it look clean
			projBase := filepath.Base(proj)
			if projBase == "." || projBase == "/" {
				projBase = proj
			}

			colPrefix := padCell(prefix, selW, false)
			colID := padCell(s.ID, idW, false)
			colProv := padCell(abbrevProvider(s.Provider), provW, false)
			colUpd := padCell(fmtLocal(s.UpdatedAt), updW, false)
			colProj := padCell(projBase, projW, false)
			colTitle := padCell(s.Title, titleW, true)

			var rowStr string
			if isSelected {
				unstyledRow := fmt.Sprintf("%s%s %s %s %s %s",
					colPrefix, colID, colProv, colUpd, colProj, colTitle)
				rowStr = selectedRowStyle.Render(truncateByWidth(unstyledRow, width, false))
			} else {
				rowStr = fmt.Sprintf("%s%s %s %s %s %s",
					colPrefix,
					normalRowStyle.Render(colID),
					providerStyle.Render(colProv),
					normalRowStyle.Render(colUpd),
					projectStyle.Render(colProj),
					normalRowStyle.Render(colTitle),
				)
			}
			sb.WriteString(rowStr + "\n")
		}

		// Pad blank lines to keep viewport height static
		renderedCount := endIdx - startIdx
		for i := renderedCount; i < maxItems; i++ {
			sb.WriteString("\n")
		}
	}

	// 4. Preview / Transcript Snippet Area
	if m.selected >= 0 && m.selected < len(m.items) && m.items[m.selected].Snippet != "" {
		sb.WriteString(previewHeaderStyle.Render(strings.Repeat("-", width)) + "\n")
		snip := m.items[m.selected].Snippet
		snipLines := strings.Split(snip, "\n")
		for _, sl := range snipLines {
			sb.WriteString(previewBodyStyle.Render(truncateByWidth(sl, width, false)) + "\n")
		}
	} else {
		// Padding lines
		sb.WriteString("\n\n")
	}

	// Render anchored to the bottom of the screen
	return lipgloss.PlaceVertical(height, lipgloss.Bottom, sb.String())
}

func tuiSelectID(items []Session, dirFilter string, providers []string) (string, string, bool) {
	if len(items) == 0 {
		return "", "", false
	}
	if !wantTUI(false, true) {
		return "", "", false
	}

	m := tuiModel{
		allItems:  items,
		selected:  0,
		dirFilter: dirFilter,
	}

	if len(providers) > 0 {
		m.tabs = append([]string{"ALL"}, providers...)
		m.currentTab = 0
		m = m.filterItemsByTab()
	} else {
		m.items = items
	}

	// Run inline (without AltScreen) to keep the command output history visible
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return "", "", false
	}

	fm := finalModel.(tuiModel)
	return fm.action, fm.selectedID, fm.ok
}
