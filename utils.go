package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func getHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func expandHome(p string) string {
	if p == "~" {
		return getHomeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(getHomeDir(), p[2:])
	}
	return p
}

func normPath(p string) string {
	expanded := expandHome(p)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		abs = expanded
	}
	if len(abs) > 1 && abs[len(abs)-1] == filepath.Separator {
		abs = abs[:len(abs)-1]
	}
	return abs
}

func pathEqual(a, b string) bool {
	return normPath(a) == normPath(b)
}

func getLocalLocation() *time.Location {
	return time.Local
}

func dtFromISO(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	var lastErr error
	for _, f := range formats {
		t, err := time.Parse(f, s)
		if err == nil {
			if strings.Contains(s, "Z") || strings.Contains(s, "+") || strings.Contains(s, "-") {
				return t.UTC(), nil
			}
			localTime, err := time.ParseInLocation(f, s, getLocalLocation())
			if err == nil {
				return localTime.UTC(), nil
			}
		}
		lastErr = err
	}
	return time.Time{}, lastErr
}

var reUserQueryStart = regexp.MustCompile(`(?i)<user_query>\s*`)
var reUserQueryEnd = regexp.MustCompile(`(?i)\s*</user_query>`)
var reMultipleSpaces = regexp.MustCompile(`\s+`)

func compactOneLine(s string) string {
	s = reUserQueryStart.ReplaceAllString(s, "")
	s = reUserQueryEnd.ReplaceAllString(s, "")
	s = reMultipleSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func isCodexBootstrapText(text string) bool {
	if strings.Contains(text, "<environment_context>") {
		return true
	}
	if strings.Contains(text, "AGENTS.md instructions") && strings.Contains(text, "<INSTRUCTIONS>") {
		return true
	}
	if strings.Contains(text, "Repository Guidelines") && strings.Contains(text, "Recommended Sections") {
		return true
	}
	return false
}

func isTrivialGreeting(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return true
	}
	return t == "hi" || t == "hello" || t == "hey" || t == "test"
}

func fmtLocal(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04")
}

func eastAsianWidth(r rune) int {
	if r < 0x20 || (r >= 0x7F && r < 0xA0) {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Mc, r) {
		return 0
	}
	if (r >= 0x1100 && r <= 0x11FF) || // Hangul Jamo
		(r >= 0x2E80 && r <= 0x2FDF) || // CJK Radicals Supplement & Kangxi Radicals
		(r >= 0x3000 && r <= 0x303F) || // CJK Symbols and Punctuation
		(r >= 0x3040 && r <= 0x309F) || // Hiragana
		(r >= 0x30A0 && r <= 0x30FF) || // Katakana
		(r >= 0x3100 && r <= 0x312F) || // Bopomofo
		(r >= 0x3130 && r <= 0x318F) || // Hangul Compatibility Jamo
		(r >= 0x31A0 && r <= 0x31BF) || // Bopomofo Extended
		(r >= 0x31C0 && r <= 0x31EF) || // CJK Strokes
		(r >= 0x31F0 && r <= 0x31FF) || // Katakana Phonetic Extensions
		(r >= 0x3200 && r <= 0x32FF) || // Enclosed CJK Letters and Months
		(r >= 0x3300 && r <= 0x33FF) || // CJK Compatibility
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Unified Ideographs Extension A
		(r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0xA960 && r <= 0xA97F) || // Hangul Jamo Extended-A
		(r >= 0xAC00 && r <= 0xD7AF) || // Hangul Syllables
		(r >= 0xD7B0 && r <= 0xD7FF) || // Hangul Jamo Extended-B
		(r >= 0xF900 && r <= 0xFAFF) || // CJK Compatibility Ideographs
		(r >= 0xFE10 && r <= 0xFE1F) || // Vertical Forms
		(r >= 0xFE30 && r <= 0xFE4F) || // CJK Compatibility Forms
		(r >= 0xFE50 && r <= 0xFE6F) || // Small Form Variants
		(r >= 0xFF01 && r <= 0xFF60) || // Fullwidth Forms
		(r >= 0xFFE0 && r <= 0xFFE6) || // Fullwidth Forms Symbol
		(r >= 0x1B000 && r <= 0x1B0FF) || // Kana Supplement
		(r >= 0x1B100 && r <= 0x1B12F) || // Kana Extended-A
		(r >= 0x1B130 && r <= 0x1B16F) || // Small Kana Extension
		(r >= 0x1F200 && r <= 0x1F2FF) || // Enclosed Ideographic Supplement
		(r >= 0x20000 && r <= 0x2A6DF) || // CJK Unified Ideographs Extension B
		(r >= 0x2A700 && r <= 0x2B73F) || // CJK Unified Ideographs Extension C
		(r >= 0x2B740 && r <= 0x2B81F) || // CJK Unified Ideographs Extension D
		(r >= 0x2B820 && r <= 0x2CEAF) || // CJK Unified Ideographs Extension E
		(r >= 0x2F800 && r <= 0x2FA1F) { // CJK Compatibility Ideographs Supplement
		return 2
	}
	return 1
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += eastAsianWidth(r)
	}
	return w
}

func truncateByWidth(s string, maxW int, ellipsis bool) string {
	if maxW <= 0 {
		return ""
	}
	if displayWidth(s) <= maxW {
		return s
	}
	ell := ""
	ellW := 0
	if ellipsis {
		ell = "..."
		ellW = 3
		if maxW <= ellW {
			runes := []rune(ell)
			if maxW < len(runes) {
				return string(runes[:maxW])
			}
			return ell
		}
	}
	limit := maxW - ellW
	w := 0
	var sb strings.Builder
	for _, r := range s {
		cw := eastAsianWidth(r)
		if w+cw > limit {
			break
		}
		sb.WriteRune(r)
		w += cw
	}
	sb.WriteString(ell)
	return sb.String()
}

func padCell(s string, width int, ellipsis bool) string {
	s2 := truncateByWidth(s, width, ellipsis)
	pad := width - displayWidth(s2)
	if pad < 0 {
		pad = 0
	}
	return s2 + strings.Repeat(" ", pad)
}

func listFilesDepth(root string, maxDepth int, pred func(string) bool) []string {
	var out []string
	var rec func(string, int)
	rec = func(dir string, depth int) {
		if depth > maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, ent := range entries {
			p := filepath.Join(dir, ent.Name())
			fi, err := ent.Info()
			if err != nil {
				continue
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if fi.IsDir() {
				rec(p, depth+1)
			} else if fi.Mode().IsRegular() {
				if pred(p) {
					out = append(out, p)
				}
			}
		}
	}
	rec(root, 0)
	return out
}

type pathMtime struct {
	path  string
	mtime time.Time
}

func newestByMtime(paths []string, maxCount int) []string {
	var list []pathMtime
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		list = append(list, pathMtime{path: p, mtime: fi.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].mtime.After(list[j].mtime)
	})
	if maxCount > 0 && len(list) > maxCount {
		list = list[:maxCount]
	}
	out := make([]string, len(list))
	for i, item := range list {
		out[i] = item.path
	}
	return out
}

func readTailLines(path string, maxBytes int64) []string {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	size := fi.Size()
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	_, err = f.Seek(start, io.SeekStart)
	if err != nil {
		return nil
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	txt := strings.ToValidUTF8(string(b), string(utf8.RuneError))
	scanner := bufio.NewScanner(strings.NewReader(txt))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	var out []string
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed != "" {
			out = append(out, ln)
		}
	}
	return out
}

func parseJSONLStream(path string, callback func(map[string]any) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			return err
		}
		line := strings.TrimSpace(strings.ToValidUTF8(string(lineBytes), string(utf8.RuneError)))
		if line == "" {
			if err == io.EOF {
				break
			}
			continue
		}
		var obj map[string]any
		if jsonErr := json.Unmarshal([]byte(line), &obj); jsonErr == nil {
			if !callback(obj) {
				break
			}
		}
		if err == io.EOF {
			break
		}
	}
	return nil
}

func renderTableProvider(sessions []Session) string {
	cols := map[string]int{"id": 36, "updated": 16, "project": 26, "title": 60}
	header := fmt.Sprintf("%s  %s  %s  %s",
		padCell("Id", cols["id"], false),
		padCell("Updated", cols["updated"], false),
		padCell("Project", cols["project"], false),
		padCell("Title", cols["title"], false),
	)
	sep := fmt.Sprintf("%s  %s  %s  %s",
		strings.Repeat("-", cols["id"]),
		strings.Repeat("-", cols["updated"]),
		strings.Repeat("-", cols["project"]),
		strings.Repeat("-", cols["title"]),
	)
	lines := []string{header, sep}
	for _, s := range sessions {
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s",
			padCell(s.ID, cols["id"], false),
			padCell(fmtLocal(s.UpdatedAt), cols["updated"], false),
			padCell(s.Project, cols["project"], false),
			padCell(s.Title, cols["title"], true),
		))
	}
	return strings.TrimRightFunc(strings.Join(lines, "\n"), unicode.IsSpace)
}

func renderTableMerged(sessions []Session) string {
	cols := map[string]int{"id": 36, "prov": 7, "updated": 16, "project": 30, "title": 60}
	header := fmt.Sprintf("%s  %s  %s  %s  %s",
		padCell("Id", cols["id"], false),
		padCell("Prov", cols["prov"], false),
		padCell("Updated", cols["updated"], false),
		padCell("Project", cols["project"], false),
		padCell("Title", cols["title"], false),
	)
	sep := fmt.Sprintf("%s  %s  %s  %s  %s",
		strings.Repeat("-", cols["id"]),
		strings.Repeat("-", cols["prov"]),
		strings.Repeat("-", cols["updated"]),
		strings.Repeat("-", cols["project"]),
		strings.Repeat("-", cols["title"]),
	)
	lines := []string{header, sep}
	for _, s := range sessions {
		proj := s.ProjectRoot
		if proj == "" {
			proj = s.Project
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s  %s",
			padCell(s.ID, cols["id"], false),
			padCell(abbrevProvider(s.Provider), cols["prov"], false),
			padCell(fmtLocal(s.UpdatedAt), cols["updated"], false),
			padCell(proj, cols["project"], false),
			padCell(s.Title, cols["title"], true),
		))
	}
	return strings.TrimRightFunc(strings.Join(lines, "\n"), unicode.IsSpace)
}

func renderSearchRow(s Session) string {
	cols := map[string]int{"id": 36, "prov": 7, "updated": 16, "project": 30, "title": 60}
	proj := s.ProjectRoot
	if proj == "" {
		proj = s.Project
	}
	return fmt.Sprintf("%s  %s  %s  %s  %s",
		padCell(s.ID, cols["id"], false),
		padCell(abbrevProvider(s.Provider), cols["prov"], false),
		padCell(fmtLocal(s.UpdatedAt), cols["updated"], false),
		padCell(proj, cols["project"], false),
		padCell(s.Title, cols["title"], true),
	)
}

func copyToClipboard(text string) error {
	text = strings.ToValidUTF8(text, "?")
	var commands [][]string
	commands = append(commands, []string{"pbcopy"})
	for _, name := range []string{"wl-copy", "xclip", "xsel"} {
		path, err := exec.LookPath(name)
		if err == nil {
			if name == "xclip" {
				commands = append(commands, []string{path, "-selection", "clipboard"})
			} else if name == "xsel" {
				commands = append(commands, []string{path, "--clipboard", "--input"})
			} else {
				commands = append(commands, []string{path})
			}
		}
	}

	var lastErr error
	for _, cmdArgs := range commands {
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		cmd.Stdin = strings.NewReader(text)
		err := cmd.Run()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return fmt.Errorf("no clipboard command succeeded, last error: %w", lastErr)
	}
	return errors.New("no clipboard command found")
}

var spinnerFrames = []string{"|", "/", "-", "\\"}

func progressRenderSpinner(frame int) string {
	spin := spinnerFrames[frame%len(spinnerFrames)]
	dots := strings.Repeat(".", frame%4)
	return fmt.Sprintf("\r%s processing%-3s", spin, dots)
}

func progressRender(current, total, hits int, width int) string {
	if total <= 0 {
		return ""
	}
	pct := float64(current) / float64(total)
	filled := int(math.Round(pct * float64(width)))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("#", filled) + strings.Repeat(".", width-filled)
	return fmt.Sprintf("\r[%s] %d/%d (%3d%%) hits=%d", bar, current, total, int(pct*100), hits)
}

func progressClear() {
	cols, _, err := getTermSize()
	if err != nil || cols <= 0 {
		cols = 80
	}
	clearStr := "\r" + strings.Repeat(" ", cols-1) + "\r"
	os.Stderr.WriteString(clearStr)
}

func indexOfRuneSlice(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	if len(haystack) < len(needle) {
		return -1
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func findSnippet(haystack, needle string, caseSensitive bool, radius int) string {
	if haystack == "" || needle == "" {
		return ""
	}
	hayRunes := []rune(haystack)
	neeRunes := []rune(needle)

	var hayLower []rune
	var neeLower []rune
	if !caseSensitive {
		hayLower = make([]rune, len(hayRunes))
		for i, r := range hayRunes {
			hayLower[i] = unicode.ToLower(r)
		}
		neeLower = make([]rune, len(neeRunes))
		for i, r := range neeRunes {
			neeLower[i] = unicode.ToLower(r)
		}
	}

	idx := -1
	if caseSensitive {
		idx = indexOfRuneSlice(hayRunes, neeRunes)
	} else {
		idx = indexOfRuneSlice(hayLower, neeLower)
	}

	if idx < 0 {
		return ""
	}

	start := idx - radius
	if start < 0 {
		start = 0
	}
	end := idx + len(neeRunes) + radius
	if end > len(hayRunes) {
		end = len(hayRunes)
	}

	snippet := string(hayRunes[start:end])
	snippet = strings.ReplaceAll(snippet, "\n", " ")
	snippet = strings.ReplaceAll(snippet, "\r", " ")
	snippet = strings.TrimSpace(snippet)

	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(hayRunes) {
		snippet = snippet + "..."
	}
	return snippet
}

func abbrevProvider(provider string) string {
	switch strings.ToLower(provider) {
	case "codex":
		return "CODX"
	case "claude":
		return "CLAD"
	case "cursor":
		return "CURS"

	case "antigravity":
		return "ANTG"
	default:
		u := strings.ToUpper(provider)
		if len(u) > 4 {
			return u[:4]
		}
		return u
	}
}
