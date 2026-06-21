package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

type Session struct {
	Provider    string    `json:"provider"`
	ID          string    `json:"id"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Title       string    `json:"title"`
	Project     string    `json:"project"`
	ProjectRoot string    `json:"projectRoot"`
	SourcePath  string    `json:"sourcePath"`
	Snippet     string    `json:"snippet"`
}

const usageStr = `am (agents-manager): local agent session history viewer

Usage:
  am --help
  am sessions [options]
  am sessions <dir> [options]
  am search <keyword> [options]
  am remote --telegram <token>

Options (sessions):
  --provider <name>   Filter providers (repeatable). One of: codex, claude, cursor, gemini, antigravity
  --limit <n>         Limit rows (default: 20). In <dir> mode this applies to merged results.
  --all               Disable limit (equivalent to --limit 0)
  --project <substr>  Filter by substring on project/cwd/projectRoot/title
  <dir>               Filter by directory (exact match). When provided, output is merged across providers.
  --tui               Force interactive selector (TTY only)
  --no-tui            Disable interactive selector
  --copy-context, -cc <id> Copy one session context to clipboard by exact id or unique id prefix
  --json              Output JSON to stdout
  --debug             Print debug info to stderr
  --help              Show help

Options (search):
  <keyword>           Substring to find in session transcript / title / project
  --provider <name>   Filter providers (repeatable)
  --limit <n>         Limit matched rows (default: 20). Use --all to disable.
  --all               Disable limit
  --case-sensitive    Case-sensitive match (default: case-insensitive)
  --project <substr>  Additional substring filter on project/cwd/title/id
  --tui / --no-tui    Same selector behavior as ` + "`" + `sessions <dir>` + "`" + `
  --json              Output JSON (includes a short snippet around the first hit)
  --debug             Print debug info to stderr

Options (remote):
  --telegram <token>  Run Codex app-server over stdio and bridge messages through a Telegram bot`

func usage() string {
	return usageStr
}

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
	// Normalizing standard formats that time.Parse handles
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
			// If layout has no time zone representation and input has no indicator,
			// time.Parse returns UTC but it was actually a local datetime representation.
			if strings.Contains(s, "Z") || strings.Contains(s, "+") || strings.Contains(s, "-") {
				return t.UTC(), nil
			}
			// Parse as local
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
	// Wide CJK blocks & Fullwidth characters
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
			// Skip symlinks
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

func applyProjectFilter(sessions []Session, projectSubstr string) []Session {
	if projectSubstr == "" {
		return sessions
	}
	needle := strings.ToLower(projectSubstr)
	var out []Session
	for _, s := range sessions {
		hay := strings.ToLower(strings.Join([]string{
			s.Project,
			s.ProjectRoot,
			s.Title,
			s.ID,
			s.SourcePath,
		}, " "))
		if strings.Contains(hay, needle) {
			out = append(out, s)
		}
	}
	return out
}

func projectDirToPathHint(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	if strings.HasPrefix(projectDir, "Users-") {
		return "/" + strings.ReplaceAll(projectDir, "-", "/")
	}
	return strings.ReplaceAll(projectDir, "-", "/")
}

func applyDirFilterExact(sessions []Session, dirFilter string) []Session {
	if dirFilter == "" {
		return sessions
	}
	d := normPath(dirFilter)
	var out []Session
	for _, s := range sessions {
		var candidates []string
		if s.Project != "" {
			candidates = append(candidates, s.Project)
		}
		if s.ProjectRoot != "" {
			candidates = append(candidates, s.ProjectRoot)
		}
		if s.Provider == "cursor" && s.Project != "" {
			candidates = append(candidates, projectDirToPathHint(s.Project))
		}
		ok := false
		for _, c := range candidates {
			if c == "" {
				continue
			}
			if !strings.Contains(c, string(filepath.Separator)) && !strings.HasPrefix(c, "~") {
				continue
			}
			if pathEqual(d, c) {
				ok = true
				break
			}
		}
		if ok {
			out = append(out, s)
		}
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
		lines = lines[1:] // drop partial first line
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

func parseCodexRolloutMeta(path string) (string, string, string) {
	var threadID, cwd, title string
	linesCount := 0

	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		linesCount++
		if linesCount > 3000 {
			return false
		}

		typ, _ := obj["type"].(string)
		payload, _ := obj["payload"].(map[string]any)

		if typ == "session_meta" && payload != nil {
			if threadID == "" {
				if idVal, ok := payload["id"].(string); ok {
					threadID = idVal
				}
			}
			if cwd == "" {
				if cwdVal, ok := payload["cwd"].(string); ok {
					cwd = cwdVal
				}
			}
		}

		if typ == "event_msg" && payload != nil {
			pType, _ := payload["type"].(string)
			if pType == "thread_name_updated" {
				if tName, ok := payload["thread_name"].(string); ok {
					cand := compactOneLine(tName)
					if cand != "" && !isCodexBootstrapText(cand) && !isTrivialGreeting(cand) {
						title = cand
					}
				}
			}
			if title == "" && pType == "user_message" {
				if msg, ok := payload["message"].(string); ok {
					cand := compactOneLine(msg)
					if cand != "" && !isCodexBootstrapText(cand) && !isTrivialGreeting(cand) {
						title = cand
					}
				}
			}
		}

		if title == "" && typ == "response_item" && payload != nil {
			pType, _ := payload["type"].(string)
			if pType == "message" && payload["role"] == "user" {
				if content, ok := payload["content"].([]any); ok {
					for _, itemAny := range content {
						if item, ok := itemAny.(map[string]any); ok {
							if text, ok := item["text"].(string); ok {
								cand := compactOneLine(text)
								if cand != "" && !isCodexBootstrapText(cand) && !isTrivialGreeting(cand) {
									title = cand
									break
								}
							}
							if text, ok := item["input_text"].(string); ok {
								cand := compactOneLine(text)
								if cand != "" && !isCodexBootstrapText(cand) && !isTrivialGreeting(cand) {
									title = cand
									break
								}
							}
						}
					}
				}
			}
		}

		if threadID != "" && cwd != "" && title != "" {
			return false
		}
		return true
	})
	return threadID, cwd, compactOneLine(title)
}

type progressFn func(provider string, current, total int)

func getCodexSessions(limit int, projectSubstr string, debug bool, dirFilter string, onProgress progressFn) []Session {
	homeDir := getHomeDir()
	idxPath := filepath.Join(homeDir, ".codex", "session_index.jsonl")
	if debug {
		fmt.Fprintf(os.Stderr, "[codex] index: %s\n", idxPath)
	}

	mp := make(map[string]Session)

	if _, err := os.Stat(idxPath); err == nil {
		_ = parseJSONLStream(idxPath, func(obj map[string]any) bool {
			sid, _ := obj["id"].(string)
			u, _ := obj["updated_at"].(string)
			if sid == "" || u == "" {
				return true
			}
			updated, err := dtFromISO(u)
			if err != nil {
				return true
			}
			tName, _ := obj["thread_name"].(string)
			title := compactOneLine(tName)
			if title == "" {
				title = "(no title)"
			}

			prev, exists := mp[sid]
			if !exists || prev.UpdatedAt.Before(updated) {
				mp[sid] = Session{
					Provider:   "codex",
					ID:         sid,
					UpdatedAt:  updated,
					Title:      title,
					SourcePath: idxPath,
				}
			}
			return true
		})
	}

	rolloutDir := filepath.Join(homeDir, ".codex", "sessions")
	if fi, err := os.Stat(rolloutDir); err == nil && fi.IsDir() {
		allRollouts := listFilesDepth(rolloutDir, 4, func(p string) bool {
			name := filepath.Base(p)
			return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
		})
		scanCount := 2000
		if dirFilter != "" {
			scanCount = 5000
		} else if limit > 0 {
			scanCount = limit * 30
			if scanCount < 400 {
				scanCount = 400
			}
		}
		pre := newestByMtime(allRollouts, scanCount)
		if debug {
			fmt.Fprintf(os.Stderr, "[codex] rollouts scanned: %d/%d\n", len(pre), len(allRollouts))
		}

		for i, fp := range pre {
			if onProgress != nil {
				onProgress("codex", i, len(pre))
			}
			fi, err := os.Stat(fp)
			if err != nil {
				continue
			}
			updated := fi.ModTime().UTC()
			sid, cwd, title := parseCodexRolloutMeta(fp)
			if sid == "" {
				continue
			}

			prev, exists := mp[sid]
			if !exists {
				if title == "" {
					title = "(no title)"
				}
				mp[sid] = Session{
					Provider:    "codex",
					ID:          sid,
					UpdatedAt:   updated,
					Title:       title,
					Project:     cwd,
					ProjectRoot: cwd,
					SourcePath:  fp,
				}
			} else {
				if prev.UpdatedAt.Before(updated) {
					prev.UpdatedAt = updated
				}
				if (prev.Project == "" || !strings.Contains(prev.Project, string(filepath.Separator))) && cwd != "" {
					prev.Project = cwd
					prev.ProjectRoot = cwd
					prev.SourcePath = fp
				}
				if (prev.Title == "" || prev.Title == "(no title)") && title != "" {
					prev.Title = title
				}
				mp[sid] = prev
			}
		}
		if onProgress != nil {
			onProgress("codex", len(pre), len(pre))
		}
	}

	var sessions []Session
	for _, s := range mp {
		sessions = append(sessions, s)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	sessions = applyProjectFilter(sessions, projectSubstr)
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}

	if debug {
		fmt.Fprintf(os.Stderr, "[codex] sessions: %d\n", len(sessions))
	}
	return sessions
}

func getClaudeSessions(limit int, projectSubstr string, debug bool, onProgress progressFn) []Session {
	homeDir := getHomeDir()
	root := filepath.Join(homeDir, ".claude", "projects")
	if debug {
		fmt.Fprintf(os.Stderr, "[claude] root: %s\n", root)
	}
	if _, err := os.Stat(root); err != nil {
		return nil
	}
	allJSONL := listFilesDepth(root, 2, func(p string) bool {
		return filepath.Ext(p) == ".jsonl"
	})
	maxCount := 0
	if limit > 0 {
		maxCount = limit * 10
		if maxCount < 200 {
			maxCount = 200
		}
	}
	pre := newestByMtime(allJSONL, maxCount)

	var sessions []Session
	for i, fp := range pre {
		if onProgress != nil {
			onProgress("claude", i, len(pre))
		}
		fi, err := os.Stat(fp)
		if err != nil {
			continue
		}
		updated := fi.ModTime().UTC()
		var cwd, title, sessionID string

		lines := readTailLines(fp, 256*1024)
		var lastTS, lastPrompt, lastUser, lastCWD, lastSession *string

		for i := len(lines) - 1; i >= 0; i-- {
			var obj map[string]any
			if err := json.Unmarshal([]byte(lines[i]), &obj); err != nil {
				continue
			}
			if lastTS == nil {
				if ts, ok := obj["timestamp"].(string); ok {
					lastTS = &ts
				}
			}
			if lastCWD == nil {
				if c, ok := obj["cwd"].(string); ok {
					lastCWD = &c
				}
			}
			if lastSession == nil {
				if sid, ok := obj["sessionId"].(string); ok {
					lastSession = &sid
				}
			}
			if lastPrompt == nil {
				if obj["type"] == "last-prompt" {
					if lp, ok := obj["lastPrompt"].(string); ok {
						lastPrompt = &lp
					}
				}
			}
			if lastUser == nil {
				if obj["type"] == "user" {
					if msg, ok := obj["message"].(map[string]any); ok {
						if content, ok := msg["content"].(string); ok {
							lastUser = &content
						}
					}
				}
			}
			if lastTS != nil && lastCWD != nil && lastSession != nil && lastPrompt != nil && lastUser != nil {
				break
			}
		}

		if lastTS != nil {
			if t, err := dtFromISO(*lastTS); err == nil {
				updated = t
			}
		}
		if lastCWD != nil {
			cwd = *lastCWD
		}
		if lastSession != nil {
			sessionID = *lastSession
		}
		if lastPrompt != nil {
			title = compactOneLine(*lastPrompt)
		} else if lastUser != nil {
			title = compactOneLine(*lastUser)
		}

		projectDir := filepath.Base(filepath.Dir(fp))
		project := cwd
		if project == "" {
			project = projectDir
		}
		sid := sessionID
		if sid == "" {
			sid = strings.TrimSuffix(filepath.Base(fp), ".jsonl")
		}
		if title == "" {
			title = "(no title)"
		}

		sessions = append(sessions, Session{
			Provider:    "claude",
			ID:          sid,
			UpdatedAt:   updated,
			Title:       title,
			Project:     project,
			ProjectRoot: project,
			SourcePath:  fp,
		})
	}
	if onProgress != nil {
		onProgress("claude", len(pre), len(pre))
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	sessions = applyProjectFilter(sessions, projectSubstr)
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[claude] sessions: %d (scanned %d/%d)\n", len(sessions), len(pre), len(allJSONL))
	}
	return sessions
}

func getCursorSessions(limit int, projectSubstr string, debug bool, onProgress progressFn) []Session {
	homeDir := getHomeDir()
	root := filepath.Join(homeDir, ".cursor", "projects")
	if debug {
		fmt.Fprintf(os.Stderr, "[cursor] root: %s\n", root)
	}
	if _, err := os.Stat(root); err != nil {
		return nil
	}

	files := listFilesDepth(root, 5, func(p string) bool {
		return filepath.Ext(p) == ".jsonl" && strings.Contains(p, string(filepath.Separator)+"agent-transcripts"+string(filepath.Separator))
	})
	maxCount := 0
	if limit > 0 {
		maxCount = limit * 10
		if maxCount < 200 {
			maxCount = 200
		}
	}
	pre := newestByMtime(files, maxCount)

	var sessions []Session
	for i, fp := range pre {
		if onProgress != nil {
			onProgress("cursor", i, len(pre))
		}
		fi, err := os.Stat(fp)
		if err != nil {
			continue
		}
		updated := fi.ModTime().UTC()
		var title string

		linesScanned := 0
		_ = parseJSONLStream(fp, func(obj map[string]any) bool {
			linesScanned++
			if linesScanned >= 200 {
				return false
			}
			if obj["role"] == "user" {
				if msg, ok := obj["message"].(map[string]any); ok {
					if content, ok := msg["content"].([]any); ok {
						var firstText string
						for _, itemAny := range content {
							if item, ok := itemAny.(map[string]any); ok {
								if item["type"] == "text" {
									if txt, ok := item["text"].(string); ok {
										firstText = txt
										break
									}
								}
							}
						}
						if firstText != "" {
							title = compactOneLine(firstText)
							return false
						}
					}
				}
			}
			return true
		})

		parts := strings.Split(fp, string(filepath.Separator))
		projectDir := ""
		for idx, part := range parts {
			if part == "projects" && idx+1 < len(parts) {
				projectDir = parts[idx+1]
				break
			}
		}
		projHint := projectDirToPathHint(projectDir)
		sid := strings.TrimSuffix(filepath.Base(fp), ".jsonl")
		if title == "" {
			title = "(no title)"
		}

		sessions = append(sessions, Session{
			Provider:    "cursor",
			ID:          sid,
			UpdatedAt:   updated,
			Title:       title,
			Project:     projectDir,
			ProjectRoot: projHint,
			SourcePath:  fp,
		})
	}
	if onProgress != nil {
		onProgress("cursor", len(pre), len(pre))
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	sessions = applyProjectFilter(sessions, projectSubstr)
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[cursor] sessions: %d (scanned %d/%d)\n", len(sessions), len(pre), len(files))
	}
	return sessions
}

func geminiTextFromContent(value any) string {
	if s, ok := value.(string); ok {
		return compactOneLine(s)
	}
	if l, ok := value.([]any); ok {
		var parts []string
		for _, itemAny := range l {
			if m, ok := itemAny.(map[string]any); ok {
				if txt, ok := m["text"].(string); ok {
					parts = append(parts, txt)
				}
			} else if s, ok := itemAny.(string); ok {
				parts = append(parts, s)
			}
		}
		return compactOneLine(strings.Join(parts, " "))
	}
	return ""
}

func parseGeminiJSONSession(fp string) (*Session, error) {
	data, err := os.ReadFile(fp)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}

	updatedStr, _ := obj["lastUpdated"].(string)
	if updatedStr == "" {
		updatedStr, _ = obj["startTime"].(string)
	}
	if updatedStr == "" {
		return nil, errors.New("no updated/start time found")
	}
	updated, err := dtFromISO(updatedStr)
	if err != nil {
		return nil, err
	}

	sid, _ := obj["sessionId"].(string)
	if sid == "" {
		sid = strings.TrimSuffix(filepath.Base(fp), filepath.Ext(fp))
	}

	var title string
	if msgs, ok := obj["messages"].([]any); ok {
		for _, mAny := range msgs {
			if m, ok := mAny.(map[string]any); ok {
				if m["type"] == "user" {
					title = geminiTextFromContent(m["content"])
					if title != "" {
						break
					}
				}
			}
		}
	}

	s := geminiSessionFromParts(fp, sid, updated, title)
	return &s, nil
}

func parseGeminiJSONLSession(fp string) (*Session, error) {
	var sid, title string
	var updated time.Time
	var updatedSet bool

	err := parseJSONLStream(fp, func(obj map[string]any) bool {
		if sid == "" {
			if s, ok := obj["sessionId"].(string); ok {
				sid = s
			}
		}
		if lastUpd, ok := obj["lastUpdated"].(string); ok {
			if t, err := dtFromISO(lastUpd); err == nil {
				if !updatedSet || updated.Before(t) {
					updated = t
					updatedSet = true
				}
			}
		}
		if tsStr, ok := obj["timestamp"].(string); ok {
			if t, err := dtFromISO(tsStr); err == nil {
				if !updatedSet || updated.Before(t) {
					updated = t
					updatedSet = true
				}
			}
		}
		if setObj, ok := obj["$set"].(map[string]any); ok {
			if sid == "" {
				if s, ok := setObj["sessionId"].(string); ok {
					sid = s
				}
			}
			if lastUpd, ok := setObj["lastUpdated"].(string); ok {
				if t, err := dtFromISO(lastUpd); err == nil {
					if !updatedSet || updated.Before(t) {
						updated = t
						updatedSet = true
					}
				}
			}
		}
		if title == "" && obj["type"] == "user" {
			title = geminiTextFromContent(obj["displayContent"])
			if title == "" {
				title = geminiTextFromContent(obj["content"])
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	if sid == "" {
		sid = strings.TrimSuffix(filepath.Base(fp), filepath.Ext(fp))
	}
	if !updatedSet {
		fi, err := os.Stat(fp)
		if err != nil {
			return nil, err
		}
		updated = fi.ModTime().UTC()
	}

	s := geminiSessionFromParts(fp, sid, updated, title)
	return &s, nil
}

func geminiSessionFromParts(fp string, sid string, updated time.Time, title string) Session {
	projectDir := filepath.Base(filepath.Dir(filepath.Dir(fp)))
	projectRoot := ""
	prFile := filepath.Join(filepath.Dir(filepath.Dir(fp)), ".project_root")
	if fi, err := os.Stat(prFile); err == nil && !fi.IsDir() {
		data, err := os.ReadFile(prFile)
		if err == nil {
			projectRoot = strings.TrimSpace(string(data))
		}
	}
	if title == "" {
		title = "(no title)"
	}
	return Session{
		Provider:    "gemini",
		ID:          sid,
		UpdatedAt:   updated,
		Title:       title,
		Project:     projectDir,
		ProjectRoot: projectRoot,
		SourcePath:  fp,
	}
}

func getGeminiSessions(limit int, projectSubstr string, debug bool, onProgress progressFn) []Session {
	homeDir := getHomeDir()
	root := filepath.Join(homeDir, ".gemini", "tmp")
	if debug {
		fmt.Fprintf(os.Stderr, "[gemini] root: %s\n", root)
	}
	if _, err := os.Stat(root); err != nil {
		return nil
	}

	files := listFilesDepth(root, 6, func(p string) bool {
		ext := filepath.Ext(p)
		name := filepath.Base(p)
		chatsSub := string(filepath.Separator) + "chats" + string(filepath.Separator)
		return (ext == ".json" || ext == ".jsonl") && strings.HasPrefix(name, "session-") && strings.Contains(p, chatsSub)
	})

	maxCount := 0
	if limit > 0 {
		maxCount = limit * 10
		if maxCount < 200 {
			maxCount = 200
		}
	}
	pre := newestByMtime(files, maxCount)

	byID := make(map[string]Session)
	for i, fp := range pre {
		if onProgress != nil {
			onProgress("gemini", i, len(pre))
		}
		var s *Session
		var err error
		if filepath.Ext(fp) == ".jsonl" {
			s, err = parseGeminiJSONLSession(fp)
		} else {
			s, err = parseGeminiJSONSession(fp)
		}
		if err != nil || s == nil {
			continue
		}
		prev, exists := byID[s.ID]
		if !exists || prev.UpdatedAt.Before(s.UpdatedAt) {
			byID[s.ID] = *s
		}
	}
	if onProgress != nil {
		onProgress("gemini", len(pre), len(pre))
	}

	var sessions []Session
	for _, s := range byID {
		sessions = append(sessions, s)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	sessions = applyProjectFilter(sessions, projectSubstr)
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[gemini] sessions: %d (scanned %d/%d)\n", len(sessions), len(pre), len(files))
	}
	return sessions
}

func parseAntigravityHistory(fp string) map[string]string {
	m := make(map[string]string)
	_ = parseJSONLStream(fp, func(obj map[string]any) bool {
		cid, _ := obj["conversationId"].(string)
		workspace, _ := obj["workspace"].(string)
		if cid != "" && workspace != "" {
			m[cid] = workspace
		}
		return true
	})
	return m
}

func extractUserRequest(content string) string {
	startTag := "<USER_REQUEST>"
	endTag := "</USER_REQUEST>"
	startIdx := strings.Index(content, startTag)
	if startIdx >= 0 {
		endIdx := strings.Index(content, endTag)
		if endIdx > startIdx {
			return strings.TrimSpace(content[startIdx+len(startTag) : endIdx])
		}
	}
	return strings.TrimSpace(content)
}

func getAntigravitySessions(limit int, projectSubstr string, debug bool, onProgress progressFn) []Session {
	homeDir := getHomeDir()
	root := filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain")
	if debug {
		fmt.Fprintf(os.Stderr, "[antigravity] root: %s\n", root)
	}
	if _, err := os.Stat(root); err != nil {
		return nil
	}

	historyPath := filepath.Join(homeDir, ".gemini", "antigravity-cli", "history.jsonl")
	workspaceMap := make(map[string]string)
	if _, err := os.Stat(historyPath); err == nil {
		workspaceMap = parseAntigravityHistory(historyPath)
	}

	files := listFilesDepth(root, 5, func(p string) bool {
		return filepath.Base(p) == "transcript.jsonl" && strings.Contains(p, ".system_generated"+string(filepath.Separator)+"logs")
	})

	maxCount := 0
	if limit > 0 {
		maxCount = limit * 10
		if maxCount < 200 {
			maxCount = 200
		}
	}
	pre := newestByMtime(files, maxCount)

	var sessions []Session
	for i, fp := range pre {
		if onProgress != nil {
			onProgress("antigravity", i, len(pre))
		}
		fi, err := os.Stat(fp)
		if err != nil {
			continue
		}
		updated := fi.ModTime().UTC()

		parts := strings.Split(fp, string(filepath.Separator))
		var sid string
		if len(parts) >= 4 {
			sid = parts[len(parts)-4]
		}
		if sid == "" {
			continue
		}

		var title string
		_ = parseJSONLStream(fp, func(obj map[string]any) bool {
			typ, _ := obj["type"].(string)
			if typ == "USER_INPUT" {
				content, _ := obj["content"].(string)
				title = extractUserRequest(content)
				if title != "" {
					title = compactOneLine(title)
					return false // stop
				}
			}
			return true
		})
		if title == "" {
			title = "(no title)"
		}

		project := workspaceMap[sid]

		sessions = append(sessions, Session{
			Provider:    "antigravity",
			ID:          sid,
			UpdatedAt:   updated,
			Title:       title,
			Project:     project,
			ProjectRoot: project,
			SourcePath:  fp,
		})
	}

	if onProgress != nil {
		onProgress("antigravity", len(pre), len(pre))
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	sessions = applyProjectFilter(sessions, projectSubstr)
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[antigravity] sessions: %d (scanned %d/%d)\n", len(sessions), len(pre), len(files))
	}
	return sessions
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
			padCell(strings.ToUpper(s.Provider), cols["prov"], false),
			padCell(fmtLocal(s.UpdatedAt), cols["updated"], false),
			padCell(proj, cols["project"], false),
			padCell(s.Title, cols["title"], true),
		))
	}
	return strings.TrimRightFunc(strings.Join(lines, "\n"), unicode.IsSpace)
}

var toolPartTypes = map[string]bool{
	"tool_use":               true,
	"tool_result":            true,
	"tool_use_input":         true,
	"function_call":          true,
	"function_call_output":   true,
	"server_tool_use":        true,
	"web_search_tool_result": true,
}

var consecutiveNewlines = regexp.MustCompile(`\n{3,}`)

func readableText(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return consecutiveNewlines.ReplaceAllString(strings.TrimSpace(s), "\n\n")
	}
	if l, ok := value.([]any); ok {
		var parts []string
		for _, v := range l {
			if rText := readableText(v); rText != "" {
				parts = append(parts, rText)
			}
		}
		return strings.Join(parts, "\n")
	}
	if m, ok := value.(map[string]any); ok {
		if typ, ok := m["type"].(string); ok && toolPartTypes[typ] {
			switch typ {
			case "tool_use":
				name, _ := m["name"].(string)
				var inputStr string
				if inputVal, ok := m["input"]; ok {
					if inputMap, ok := inputVal.(map[string]any); ok {
						if cmd, ok := inputMap["command"].(string); ok {
							inputStr = "command: " + cmd
						} else {
							data, err := json.MarshalIndent(inputMap, "", "  ")
							if err == nil {
								inputStr = string(data)
							}
						}
					} else if inputS, ok := inputVal.(string); ok {
						inputStr = inputS
					}
				}
				if name != "" {
					return fmt.Sprintf("[TOOL_USE: %s]\n%s", name, strings.TrimSpace(inputStr))
				}
				return fmt.Sprintf("[TOOL_USE]\n%s", strings.TrimSpace(inputStr))
			case "tool_result":
				var contentStr string
				toolUseID, _ := m["tool_use_id"].(string)
				toolName := currentSessionTools[toolUseID]
				bashCmd := currentSessionCommands[toolUseID]

				isRead := false
				if toolName != "" {
					nameLower := strings.ToLower(toolName)
					if strings.Contains(nameLower, "view") || strings.Contains(nameLower, "read") || strings.Contains(nameLower, "show") {
						isRead = true
					}
					if nameLower == "bash" && bashCmd != "" {
						cmdLower := strings.ToLower(bashCmd)
						words := strings.Fields(cmdLower)
						for _, w := range words {
							if w == "cat" || w == "grep" || w == "tail" || w == "head" || w == "less" || w == "more" {
								isRead = true
								break
							}
						}
					}
				}

				if isRead {
					contentStr = "(READ CODE OMITTED)"
				} else if contentVal, ok := m["content"]; ok {
					if contentS, ok := contentVal.(string); ok {
						contentStr = contentS
					} else if contentL, ok := contentVal.([]any); ok {
						var blockTexts []string
						for _, item := range contentL {
							if itemMap, ok := item.(map[string]any); ok {
								if text, ok := itemMap["text"].(string); ok {
									blockTexts = append(blockTexts, text)
								} else {
									data, _ := json.Marshal(itemMap)
									blockTexts = append(blockTexts, string(data))
								}
							} else if itemS, ok := item.(string); ok {
								blockTexts = append(blockTexts, itemS)
							}
						}
						contentStr = strings.Join(blockTexts, "\n")
					} else {
						data, err := json.Marshal(contentVal)
						if err == nil {
							contentStr = string(data)
						}
					}
				}
				isErr, _ := m["is_error"].(bool)
				errStr := ""
				if isErr {
					errStr = " (ERROR)"
				}
				return fmt.Sprintf("[TOOL_RESULT]%s\n%s", errStr, strings.TrimSpace(contentStr))
			default:
				var detail string
				for _, k := range []string{"content", "text", "message", "command", "arguments"} {
					if val, ok := m[k]; ok {
						detail = readableText(val)
						if detail != "" {
							break
						}
					}
				}
				if detail == "" {
					data, _ := json.Marshal(m)
					detail = string(data)
				}
				return fmt.Sprintf("[%s]\n%s", strings.ToUpper(typ), strings.TrimSpace(detail))
			}
		}
		keys := []string{"text", "input_text", "content", "message", "lastPrompt"}
		for _, key := range keys {
			if val, ok := m[key]; ok {
				if rText := readableText(val); rText != "" {
					return rText
				}
			}
		}
	}
	return ""
}

func appendContextPart(parts *[]string, role string, textVal any, maxChars int) bool {
	text := readableText(textVal)
	if text == "" {
		return true
	}
	block := fmt.Sprintf("\n[%s]\n%s\n", role, strings.TrimSpace(text))
	current := 0
	for _, p := range *parts {
		current += len(p)
	}
	if current+len(block) > maxChars {
		remain := maxChars - current
		if remain > 80 {
			*parts = append(*parts, block[:remain]+"\n\n[TRUNCATED]\n")
		}
		return false
	}
	*parts = append(*parts, block)
	return true
}

const codexEnvContextEndTag = "</environment_context>"

func contextFromCodexJSONL(path string, maxChars int) string {
	// User session content begins after the first </environment_context>.
	// If the tag is absent (e.g. older rollouts), do not filter.
	bootstrapDone := true
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), codexEnvContextEndTag) {
		bootstrapDone = false
	}

	var parts []string
	tryAppend := func(role string, text string) bool {
		if text == "" {
			return true
		}
		if !bootstrapDone {
			idx := strings.Index(text, codexEnvContextEndTag)
			if idx < 0 {
				return true
			}
			bootstrapDone = true
			text = strings.TrimSpace(text[idx+len(codexEnvContextEndTag):])
			if text == "" {
				return true
			}
		}
		return appendContextPart(&parts, role, text, maxChars)
	}

	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		payload, _ := obj["payload"].(map[string]any)
		if typ == "event_msg" && payload != nil {
			pType, _ := payload["type"].(string)
			if pType == "user_message" {
				if !tryAppend("USER", readableText(payload["message"])) {
					return false
				}
			}
		} else if typ == "response_item" && payload != nil {
			pType, _ := payload["type"].(string)
			if pType == "message" {
				role := "MESSAGE"
				if rStr, ok := payload["role"].(string); ok && rStr != "" {
					role = strings.ToUpper(rStr)
				}
				if role == "SYSTEM" {
					return true
				}
				if !tryAppend(role, readableText(payload["content"])) {
					return false
				}
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, ""))
}

func contextFromClaudeJSONL(path string, maxChars int) string {
	var parts []string
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		if typ == "user" {
			if !appendContextPart(&parts, "USER", obj["message"], maxChars) {
				return false
			}
		} else if typ == "assistant" {
			if !appendContextPart(&parts, "ASSISTANT", obj["message"], maxChars) {
				return false
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, ""))
}

func contextFromCursorJSONL(path string, maxChars int) string {
	var parts []string
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		role, _ := obj["role"].(string)
		if role != "" {
			if !appendContextPart(&parts, strings.ToUpper(role), obj["message"], maxChars) {
				return false
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, ""))
}

func contextFromGeminiJSON(path string, maxChars int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, mAny := range msgs {
		m, ok := mAny.(map[string]any)
		if !ok {
			continue
		}
		role := "MESSAGE"
		if typ, ok := m["type"].(string); ok && typ != "" {
			role = strings.ToUpper(typ)
		} else if rStr, ok := m["role"].(string); ok && rStr != "" {
			role = strings.ToUpper(rStr)
		}
		if !appendContextPart(&parts, role, m["content"], maxChars) {
			break
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

func contextFromGeminiJSONL(path string, maxChars int) string {
	var parts []string
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		if typ == "user" {
			text := readableText(obj["displayContent"])
			if text == "" {
				text = readableText(obj["content"])
			}
			if !appendContextPart(&parts, "USER", text, maxChars) {
				return false
			}
		} else if typ == "gemini" {
			if !appendContextPart(&parts, "GEMINI", obj["content"], maxChars) {
				return false
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, ""))
}

func contextFromAntigravityJSONL(path string, maxChars int) string {
	var parts []string
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		if typ == "USER_INPUT" {
			content, _ := obj["content"].(string)
			req := extractUserRequest(content)
			if req != "" {
				if !appendContextPart(&parts, "USER", req, maxChars) {
					return false
				}
			}
		} else if typ == "PLANNER_RESPONSE" {
			content, _ := obj["content"].(string)
			if content != "" {
				if !appendContextPart(&parts, "ANTIGRAVITY", content, maxChars) {
					return false
				}
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, ""))
}

var currentSessionTools = make(map[string]string)
var currentSessionCommands = make(map[string]string)

func preScanClaudeTools(path string) {
	_ = parseJSONLStream(path, func(obj map[string]any) bool {
		typ, _ := obj["type"].(string)
		if typ == "assistant" {
			msg, _ := obj["message"].(map[string]any)
			if msg != nil {
				content, _ := msg["content"].([]any)
				for _, item := range content {
					itemMap, _ := item.(map[string]any)
					if itemMap != nil {
						itemType, _ := itemMap["type"].(string)
						if itemType == "tool_use" {
							id, _ := itemMap["id"].(string)
							name, _ := itemMap["name"].(string)
							if id != "" && name != "" {
								currentSessionTools[id] = name
								if name == "Bash" {
									if inputMap, ok := itemMap["input"].(map[string]any); ok {
										if cmd, ok := inputMap["command"].(string); ok {
											currentSessionCommands[id] = cmd
										}
									}
								}
							}
						}
					}
				}
			}
		}
		return true
	})
}

func sessionTranscriptContext(session Session, maxChars int) string {
	currentSessionTools = make(map[string]string)
	currentSessionCommands = make(map[string]string)

	if session.SourcePath == "" {
		return ""
	}
	if _, err := os.Stat(session.SourcePath); err != nil {
		return ""
	}
	ext := filepath.Ext(session.SourcePath)
	if session.Provider == "codex" && ext == ".jsonl" {
		return contextFromCodexJSONL(session.SourcePath, maxChars)
	}
	if session.Provider == "claude" && ext == ".jsonl" {
		preScanClaudeTools(session.SourcePath)
		return contextFromClaudeJSONL(session.SourcePath, maxChars)
	}
	if session.Provider == "cursor" && ext == ".jsonl" {
		return contextFromCursorJSONL(session.SourcePath, maxChars)
	}
	if session.Provider == "gemini" {
		if ext == ".jsonl" {
			return contextFromGeminiJSONL(session.SourcePath, maxChars)
		}
		if ext == ".json" {
			return contextFromGeminiJSON(session.SourcePath, maxChars)
		}
	}
	if session.Provider == "antigravity" && ext == ".jsonl" {
		return contextFromAntigravityJSONL(session.SourcePath, maxChars)
	}
	return ""
}

func buildSessionContext(session Session) string {
	project := session.ProjectRoot
	if project == "" {
		project = session.Project
	}
	// Note: output time format should be UTC ISO8601 ending with Z
	updatedISO := session.UpdatedAt.UTC().Format("2006-01-02T15:04:05") + "Z"
	header := []string{
		"# Agent Session Context",
		"",
		fmt.Sprintf("Provider: %s", session.Provider),
		fmt.Sprintf("Session ID: %s", session.ID),
		fmt.Sprintf("Updated: %s", updatedISO),
		fmt.Sprintf("Project: %s", project),
		fmt.Sprintf("Title: %s", session.Title),
		fmt.Sprintf("Source: %s", session.SourcePath),
		"",
		"## Transcript",
		"",
	}
	transcript := sessionTranscriptContext(session, 120000)
	if transcript == "" {
		transcript = "(transcript unavailable)"
	}
	return strings.Join(header, "\n") + transcript + "\n"
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

func findSessionByID(sessions []Session, query string) (Session, error) {
	var exact []Session
	for _, s := range sessions {
		if s.ID == query {
			exact = append(exact, s)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}

	var matches []Session
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, query) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return Session{}, fmt.Errorf("no session matches: %s", query)
	}
	return Session{}, fmt.Errorf("ambiguous session prefix: %s (%d matches)", query, len(matches))
}

func wantTUI(noTUI, tui bool) bool {
	if noTUI {
		return false
	}
	if tui {
		return true
	}
	return isatty(0) && isatty(1)
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

func renderTUIBlock(items []Session, selected int, dirFilter string, width int) []string {
	maxItems := 10
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	width = width - 1
	if width < 1 {
		width = 1
	}

	help1 := padCell(truncateByWidth(fmt.Sprintf("dir: %s", dirFilter), width, false), width, false)
	help2 := padCell(truncateByWidth("Up/Down (or k/j) move, Enter prints id, c copies context, q quits", width, false), width, false)
	lines := []string{help1, help2}

	selW := 2
	provW := 7
	updW := 16
	minTitleW := 10

	idW := 36
	projW := 30

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

	for i, s := range items {
		prefix := "  "
		if i == selected {
			prefix = "> "
		}
		proj := s.ProjectRoot
		if proj == "" {
			proj = s.Project
		}
		row := padCell(prefix, selW, false) +
			padCell(s.ID, idW, false) + " " +
			padCell(strings.ToUpper(s.Provider), provW, false) + " " +
			padCell(fmtLocal(s.UpdatedAt), updW, false) + " " +
			padCell(proj, projW, false) + " " +
			padCell(s.Title, titleW, true)

		lines = append(lines, truncateByWidth(row, width, false))
	}

	for len(lines) < 2+maxItems {
		lines = append(lines, strings.Repeat(" ", width))
	}

	// Preview area (2 lines)
	var previewLines []string
	if selected >= 0 && selected < len(items) && items[selected].Snippet != "" {
		snip := items[selected].Snippet
		snip = strings.ReplaceAll(snip, "\n", " ")
		previewLines = append(previewLines, strings.Repeat("-", width))
		previewLines = append(previewLines, truncateByWidth("Snippet: "+snip, width, false))
	} else {
		previewLines = append(previewLines, strings.Repeat(" ", width))
		previewLines = append(previewLines, strings.Repeat(" ", width))
	}
	lines = append(lines, previewLines...)

	var out []string
	for _, ln := range lines {
		out = append(out, padCell(truncateByWidth(ln, width, false), width, false))
	}
	return out
}

func selectRead(fd int, timeout time.Duration) (bool, error) {
	var rfd syscall.FdSet
	rfd.Bits[fd/64] |= 1 << (uint(fd) % 64)

	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	err := syscall.Select(fd+1, &rfd, nil, nil, &tv)
	if err != nil {
		return false, err
	}
	ready := (rfd.Bits[fd/64] & (1 << (uint(fd) % 64))) != 0
	return ready, nil
}

func readByte(fd int, pending *[]byte, timeout time.Duration) (byte, error) {
	if len(*pending) > 0 {
		b := (*pending)[0]
		*pending = (*pending)[1:]
		return b, nil
	}
	if timeout > 0 {
		ready, err := selectRead(fd, timeout)
		if err != nil || !ready {
			return 0, io.EOF
		}
	}
	var buf [1]byte
	n, err := syscall.Read(fd, buf[:])
	if err != nil || n == 0 {
		return 0, io.EOF
	}
	return buf[0], nil
}

func readTUIKey(fd int, pending *[]byte) string {
	b, err := readByte(fd, pending, 0)
	if err != nil {
		return "quit"
	}

	switch b {
	case 0x03:
		return "quit"
	case 0x0D, 0x0A:
		return "enter"
	case 'q', 'Q':
		return "quit"
	case 'c', 'C':
		return "copy"
	case 'j', 'J':
		return "down"
	case 'k', 'K':
		return "up"
	case 0x1B: // Escape
		b2, err := readByte(fd, pending, 80*time.Millisecond)
		if err != nil {
			return "unknown"
		}
		b3, err := readByte(fd, pending, 80*time.Millisecond)
		if err != nil {
			*pending = append([]byte{b2}, *pending...)
			return "unknown"
		}
		if b2 == '[' || b2 == 'O' {
			if b3 == 'A' {
				return "up"
			}
			if b3 == 'B' {
				return "down"
			}
		}
		*pending = append([]byte{b2, b3}, *pending...)
		return "unknown"
	}
	return "unknown"
}

func isatty(fd int) bool {
	var term syscall.Termios
	// On darwin and linux, Tcgetattr is standard syscall wrapper
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&term)))
	return err == 0
}

func makeRaw(fd int) (*syscall.Termios, error) {
	var old syscall.Termios
	// Get term attributes
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&old)))
	if err != 0 {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	_, _, err = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&raw)))
	if err != 0 {
		return nil, err
	}
	return &old, nil
}

func restoreRaw(fd int, old *syscall.Termios) error {
	if old == nil {
		return nil
	}
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(old)))
	if err != 0 {
		return err
	}
	return nil
}

func tuiSelectID(items []Session, dirFilter string) (string, string, bool) {
	if len(items) == 0 {
		return "", "", false
	}
	if !isatty(0) || !isatty(1) {
		return "", "", false
	}

	maxItems := 10
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	selected := 0
	lastStartRow := 1
	lastLines := 0

	ansiHide := "\x1b[?25l"
	ansiShow := "\x1b[?25h"
	ansiNoWrap := "\x1b[?7l"
	ansiWrap := "\x1b[?7h"

	draw := func() {
		width, height, _ := getTermSize()
		block := renderTUIBlock(items, selected, dirFilter, width)
		blockLines := len(block)
		startRow := height - blockLines + 1
		if startRow < 1 {
			startRow = 1
		}
		lastStartRow = startRow
		lastLines = blockLines

		var sb strings.Builder
		sb.WriteString("\x1b[s") // save cursor
		sb.WriteString(ansiHide)
		sb.WriteString(ansiNoWrap)
		for i, ln := range block {
			sb.WriteString(fmt.Sprintf("\x1b[%d;1H", startRow+i))
			sb.WriteString("\x1b[2K") // clear line
			sb.WriteString(ln)
		}
		sb.WriteString("\x1b[u") // restore cursor
		os.Stdout.WriteString(sb.String())
	}

	cleanup := func(clearOnly, keepBlock bool) {
		if keepBlock {
			_, height, _ := getTermSize()
			endRow := lastStartRow + lastLines - 1
			if endRow >= height {
				os.Stdout.WriteString(fmt.Sprintf("\x1b[%d;1H\n", height))
			} else {
				os.Stdout.WriteString(fmt.Sprintf("\x1b[%d;1H", endRow+1))
			}
			os.Stdout.WriteString(ansiWrap + ansiShow)
			return
		}
		width, _, _ := getTermSize()
		clearWidth := width - 1
		if clearWidth < 1 {
			clearWidth = 1
		}
		var sb strings.Builder
		sb.WriteString("\x1b[s")
		sb.WriteString(ansiWrap)
		if !clearOnly {
			sb.WriteString(ansiShow)
		}
		for i := 0; i < lastLines; i++ {
			sb.WriteString(fmt.Sprintf("\x1b[%d;1H", lastStartRow+i))
			sb.WriteString("\x1b[2K")
			sb.WriteString(strings.Repeat(" ", clearWidth))
		}
		sb.WriteString("\x1b[u")
		os.Stdout.WriteString(sb.String())
	}

	clamp := func() {
		if selected < 0 {
			selected = 0
		}
		if selected >= len(items) {
			selected = len(items) - 1
		}
	}

	oldState, err := makeRaw(0)
	if err != nil {
		return "", "", false
	}
	defer func() {
		_ = restoreRaw(0, oldState)
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	defer signal.Stop(sigChan)

	draw()

	// Redraw winch updates concurrently
	go func() {
		for range sigChan {
			draw()
		}
	}()

	pendingInput := []byte{}

	for {
		key := readTUIKey(0, &pendingInput)
		if key == "quit" {
			cleanup(false, false)
			return "", "", false
		}
		if key == "enter" {
			cleanup(false, false)
			return "id", items[selected].ID, true
		}
		if key == "copy" {
			cleanup(false, true)
			return "copy-context", items[selected].ID, true
		}
		if key == "up" {
			selected--
			clamp()
			draw()
		}
		if key == "down" {
			selected++
			clamp()
			draw()
		}
	}
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

func matchSession(session Session, needle string, caseSensitive bool) (bool, string) {
	parts := []string{session.Title, session.Project, session.ProjectRoot}
	transcript := sessionTranscriptContext(session, 120000)
	if transcript != "" {
		parts = append(parts, transcript)
	}
	haystack := strings.Join(parts, "\n")
	hit := false
	if caseSensitive {
		hit = strings.Contains(haystack, needle)
	} else {
		hit = strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
	}
	if !hit {
		return false, ""
	}
	return true, findSnippet(haystack, needle, caseSensitive, 60)
}

type SessionsArgs struct {
	Help        bool
	Debug       bool
	JSON        bool
	TUI         bool
	NoTUI       bool
	CopyContext string
	Limit       int
	All         bool
	Project     string
	Providers   []string
	DirFilter   string
}

func parseSessionsArgs(args []string) (*SessionsArgs, error) {
	res := &SessionsArgs{
		Limit: 20,
	}
	var providerFlags []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--help", "-h":
			res.Help = true
		case "--debug":
			res.Debug = true
		case "--json":
			res.JSON = true
		case "--tui":
			res.TUI = true
		case "--no-tui":
			res.NoTUI = true
		case "--all":
			res.All = true
			res.Limit = 0
		case "--copy-context", "-cc":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for --copy-context / -cc")
			}
			res.CopyContext = args[i+1]
			i++
		case "--limit":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for --limit")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid limit value: %s", args[i+1])
			}
			res.Limit = val
			i++
		case "--project":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for --project")
			}
			res.Project = args[i+1]
			i++
		case "--provider":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for --provider")
			}
			providerFlags = append(providerFlags, strings.ToLower(args[i+1]))
			i++
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, fmt.Errorf("unknown option: %s", arg)
			}
			if res.DirFilter == "" {
				res.DirFilter = arg
			} else {
				return nil, fmt.Errorf("multiple positional arguments provided for sessions: %s and %s", res.DirFilter, arg)
			}
		}
	}

	if res.Limit < 0 {
		return nil, errors.New("invalid --limit (must be >= 0)")
	}

	validProviders := map[string]bool{"codex": true, "claude": true, "cursor": true, "gemini": true, "antigravity": true}
	for _, pr := range providerFlags {
		if !validProviders[pr] {
			return nil, fmt.Errorf("unknown provider: %s", pr)
		}
	}
	if len(providerFlags) > 0 {
		res.Providers = providerFlags
	} else {
		res.Providers = []string{"codex", "claude", "cursor", "gemini", "antigravity"}
	}

	if res.TUI && res.NoTUI {
		return nil, errors.New("use only one of --tui / --no-tui")
	}

	return res, nil
}

type SearchArgs struct {
	Help          bool
	Debug         bool
	JSON          bool
	TUI           bool
	NoTUI         bool
	Limit         int
	All           bool
	CaseSensitive bool
	Project       string
	Providers     []string
	Keyword       string
}

func parseSearchArgs(args []string) (*SearchArgs, error) {
	res := &SearchArgs{
		Limit: 20,
	}
	var providerFlags []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--help", "-h":
			res.Help = true
		case "--debug":
			res.Debug = true
		case "--json":
			res.JSON = true
		case "--tui":
			res.TUI = true
		case "--no-tui":
			res.NoTUI = true
		case "--all":
			res.All = true
			res.Limit = 0
		case "--case-sensitive", "-s":
			res.CaseSensitive = true
		case "--limit":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for --limit")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid limit value: %s", args[i+1])
			}
			res.Limit = val
			i++
		case "--project":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for --project")
			}
			res.Project = args[i+1]
			i++
		case "--provider":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for --provider")
			}
			providerFlags = append(providerFlags, strings.ToLower(args[i+1]))
			i++
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, fmt.Errorf("unknown option: %s", arg)
			}
			if res.Keyword == "" {
				res.Keyword = arg
			} else {
				return nil, fmt.Errorf("extra positional argument: %s", arg)
			}
		}
	}

	if res.Limit < 0 {
		return nil, errors.New("invalid --limit (must be >= 0)")
	}

	validProviders := map[string]bool{"codex": true, "claude": true, "cursor": true, "gemini": true, "antigravity": true}
	for _, pr := range providerFlags {
		if !validProviders[pr] {
			return nil, fmt.Errorf("unknown provider: %s", pr)
		}
	}
	if len(providerFlags) > 0 {
		res.Providers = providerFlags
	} else {
		res.Providers = []string{"codex", "claude", "cursor", "gemini", "antigravity"}
	}

	if res.TUI && res.NoTUI {
		return nil, errors.New("use only one of --tui / --no-tui")
	}

	return res, nil
}

func sessionsCommand(ns *SessionsArgs) {
	dirFilter := ns.DirFilter
	scanLimit := 0
	if dirFilter == "" {
		scanLimit = ns.Limit
	}

	var onProgress progressFn
	if isatty(2) {
		var lastUpdate time.Time
		frame := 0
		onProgress = func(provider string, current, total int) {
			now := time.Now()
			if now.Sub(lastUpdate) < 80*time.Millisecond {
				return
			}
			lastUpdate = now
			os.Stderr.WriteString(progressRenderSpinner(frame))
			frame++
		}
		defer progressClear()
	}

	byProvider := make(map[string][]Session)
	for _, pr := range ns.Providers {
		switch pr {
		case "codex":
			byProvider["codex"] = getCodexSessions(scanLimit, ns.Project, ns.Debug, dirFilter, onProgress)
		case "claude":
			byProvider["claude"] = getClaudeSessions(scanLimit, ns.Project, ns.Debug, onProgress)
		case "cursor":
			byProvider["cursor"] = getCursorSessions(scanLimit, ns.Project, ns.Debug, onProgress)
		case "gemini":
			byProvider["gemini"] = getGeminiSessions(scanLimit, ns.Project, ns.Debug, onProgress)
		case "antigravity":
			byProvider["antigravity"] = getAntigravitySessions(scanLimit, ns.Project, ns.Debug, onProgress)
		}
	}
	if onProgress != nil {
		progressClear()
	}

	var allSessions []Session
	for _, arr := range byProvider {
		allSessions = append(allSessions, arr...)
	}
	sort.Slice(allSessions, func(i, j int) bool {
		return allSessions[i].UpdatedAt.After(allSessions[j].UpdatedAt)
	})

	if ns.CopyContext != "" {
		filtered := allSessions
		if dirFilter != "" {
			filtered = applyDirFilterExact(filtered, dirFilter)
		}
		filtered = applyProjectFilter(filtered, ns.Project)
		session, err := findSessionByID(filtered, ns.CopyContext)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ctxText := buildSessionContext(session)
		err = copyToClipboard(ctxText)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("copied context: %s\n", session.ID)
		return
	}

	if ns.JSON {
		filtered := allSessions
		if dirFilter != "" {
			filtered = applyDirFilterExact(filtered, dirFilter)
		}
		filtered = applyProjectFilter(filtered, ns.Project)
		if dirFilter != "" && ns.Limit > 0 && len(filtered) > ns.Limit {
			filtered = filtered[:ns.Limit]
		}
		type JSONSession struct {
			Provider    string  `json:"provider"`
			ID          string  `json:"id"`
			UpdatedAt   string  `json:"updatedAt"`
			Title       string  `json:"title"`
			Project     *string `json:"project"`
			ProjectRoot *string `json:"projectRoot"`
			SourcePath  *string `json:"sourcePath"`
		}
		var out []JSONSession
		for _, s := range filtered {
			var proj *string
			if s.Project != "" {
				p := s.Project
				proj = &p
			}
			var projRoot *string
			if s.ProjectRoot != "" {
				pr := s.ProjectRoot
				projRoot = &pr
			}
			var srcPath *string
			if s.SourcePath != "" {
				sp := s.SourcePath
				srcPath = &sp
			}
			out = append(out, JSONSession{
				Provider:    s.Provider,
				ID:          s.ID,
				UpdatedAt:   s.UpdatedAt.UTC().Format("2006-01-02T15:04:05") + "Z",
				Title:       s.Title,
				Project:     proj,
				ProjectRoot: projRoot,
				SourcePath:  srcPath,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	if dirFilter != "" {
		merged := applyDirFilterExact(allSessions, dirFilter)
		merged = applyProjectFilter(merged, ns.Project)
		if ns.Limit > 0 && len(merged) > ns.Limit {
			merged = merged[:ns.Limit]
		}
		if len(merged) == 0 {
			fmt.Println("(no sessions found)")
			return
		}
		if wantTUI(ns.NoTUI, ns.TUI) {
			action, sid, ok := tuiSelectID(merged, normPath(dirFilter))
			if ok {
				if action == "copy-context" {
					session, err := findSessionByID(merged, sid)
					if err != nil {
						fmt.Fprintln(os.Stderr, err)
						os.Exit(1)
					}
					err = copyToClipboard(buildSessionContext(session))
					if err != nil {
						fmt.Fprintf(os.Stderr, "failed to copy context: %v\n", err)
						os.Exit(1)
					}
					fmt.Printf("copied context: %s\n", session.ID)
				} else {
					fmt.Println(sid)
				}
			}
			return
		}
		fmt.Println(renderTableMerged(merged))
		return
	}

	first := true
	for _, pr := range []string{"codex", "claude", "cursor", "gemini", "antigravity"} {
		inProviders := false
		for _, p := range ns.Providers {
			if p == pr {
				inProviders = true
				break
			}
		}
		if !inProviders {
			continue
		}
		sessions := byProvider[pr]
		if !first {
			fmt.Println()
		}
		first = false
		fmt.Printf("== %s ==\n", strings.ToUpper(pr))
		if len(sessions) == 0 {
			fmt.Println("(no sessions found)")
			continue
		}
		fmt.Println(renderTableProvider(sessions))
	}
}

func renderSearchRow(s Session) string {
	cols := map[string]int{"id": 36, "prov": 7, "updated": 16, "project": 30, "title": 60}
	proj := s.ProjectRoot
	if proj == "" {
		proj = s.Project
	}
	return fmt.Sprintf("%s  %s  %s  %s  %s",
		padCell(s.ID, cols["id"], false),
		padCell(strings.ToUpper(s.Provider), cols["prov"], false),
		padCell(fmtLocal(s.UpdatedAt), cols["updated"], false),
		padCell(proj, cols["project"], false),
		padCell(s.Title, cols["title"], true),
	)
}

func searchCommand(ns *SearchArgs) {
	keyword := ns.Keyword
	caseSensitive := ns.CaseSensitive

	var onLoadProgress progressFn
	if isatty(2) {
		var lastUpdate time.Time
		frame := 0
		onLoadProgress = func(provider string, current, total int) {
			now := time.Now()
			if now.Sub(lastUpdate) < 80*time.Millisecond {
				return
			}
			lastUpdate = now
			os.Stderr.WriteString(progressRenderSpinner(frame))
			frame++
		}
	}

	byProvider := make(map[string][]Session)
	for _, pr := range ns.Providers {
		switch pr {
		case "codex":
			byProvider["codex"] = getCodexSessions(0, "", ns.Debug, "", onLoadProgress)
		case "claude":
			byProvider["claude"] = getClaudeSessions(0, "", ns.Debug, onLoadProgress)
		case "cursor":
			byProvider["cursor"] = getCursorSessions(0, "", ns.Debug, onLoadProgress)
		case "gemini":
			byProvider["gemini"] = getGeminiSessions(0, "", ns.Debug, onLoadProgress)
		case "antigravity":
			byProvider["antigravity"] = getAntigravitySessions(0, "", ns.Debug, onLoadProgress)
		}
	}
	if onLoadProgress != nil {
		progressClear()
	}

	var allSessions []Session
	for _, arr := range byProvider {
		allSessions = append(allSessions, arr...)
	}
	allSessions = applyProjectFilter(allSessions, ns.Project)
	sort.Slice(allSessions, func(i, j int) bool {
		return allSessions[i].UpdatedAt.After(allSessions[j].UpdatedAt)
	})

	if ns.Debug {
		fmt.Fprintf(os.Stderr, "search: scanning %d sessions for keyword=%q (case_sensitive=%t)\n",
			len(allSessions), keyword, caseSensitive)
	}

	total := len(allSessions)
	progressOn := isatty(2) && total > 0
	updateEvery := total / 200
	if updateEvery < 1 {
		updateEvery = 1
	}

	var matched []Session
	snippets := make(map[string]string)

	for i, s := range allSessions {
		ok, snip := matchSession(s, keyword, caseSensitive)
		if ok {
			s.Snippet = snip
			matched = append(matched, s)
			if snip != "" {
				snippets[s.ID] = snip
			}

			if !ns.JSON {
				if progressOn {
					progressClear()
				}
				proj := s.ProjectRoot
				if proj == "" {
					proj = s.Project
				}
				projBase := filepath.Base(proj)
				if projBase == "." || projBase == "/" {
					projBase = proj
				}

				cleanTitle := s.Title
				if len(cleanTitle) > 40 {
					cleanTitle = cleanTitle[:40] + "..."
				}

				cleanSnip := strings.ReplaceAll(snip, "\n", " ")
				cleanSnip = strings.TrimSpace(cleanSnip)
				if len(cleanSnip) > 80 {
					cleanSnip = cleanSnip[:80] + "..."
				}

				fmt.Printf("[%s] %s > %s | %s\n", strings.ToUpper(s.Provider), projBase, cleanTitle, cleanSnip)
				if progressOn {
					os.Stderr.WriteString(progressRender(i+1, total, len(matched), 28))
				}
			}
		} else {
			if progressOn && (i%updateEvery == 0 || i == total-1) {
				os.Stderr.WriteString(progressRender(i+1, total, len(matched), 28))
			}
		}
	}
	if progressOn {
		progressClear()
	}

	if ns.Limit > 0 && len(matched) > ns.Limit {
		matched = matched[:ns.Limit]
	}

	if ns.JSON {
		type JSONSearchSession struct {
			Provider    string  `json:"provider"`
			ID          string  `json:"id"`
			UpdatedAt   string  `json:"updatedAt"`
			Title       string  `json:"title"`
			Project     *string `json:"project"`
			ProjectRoot *string `json:"projectRoot"`
			SourcePath  *string `json:"sourcePath"`
			Snippet     string  `json:"snippet"`
		}
		var out []JSONSearchSession
		for _, s := range matched {
			var proj *string
			if s.Project != "" {
				p := s.Project
				proj = &p
			}
			var projRoot *string
			if s.ProjectRoot != "" {
				pr := s.ProjectRoot
				projRoot = &pr
			}
			var srcPath *string
			if s.SourcePath != "" {
				sp := s.SourcePath
				srcPath = &sp
			}
			out = append(out, JSONSearchSession{
				Provider:    s.Provider,
				ID:          s.ID,
				UpdatedAt:   s.UpdatedAt.UTC().Format("2006-01-02T15:04:05") + "Z",
				Title:       s.Title,
				Project:     proj,
				ProjectRoot: projRoot,
				SourcePath:  srcPath,
				Snippet:     snippets[s.ID],
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	if len(matched) == 0 {
		fmt.Println("(no matching sessions)")
		return
	}

	if wantTUI(ns.NoTUI, ns.TUI) {
		action, sid, ok := tuiSelectID(matched, fmt.Sprintf("search: %s", keyword))
		if ok {
			if action == "copy-context" {
				session, err := findSessionByID(matched, sid)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				err = copyToClipboard(buildSessionContext(session))
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to copy context: %v\n", err)
					os.Exit(1)
				}
				fmt.Printf("copied context: %s\n", session.ID)
			} else {
				fmt.Println(sid)
			}
		}
		return
	}
}

type TelegramUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

type TelegramResponse struct {
	Ok     bool             `json:"ok"`
	Result []TelegramUpdate `json:"result"`
}

type remoteResponseEvent struct {
	kind string
	text string
}

type telegramSender interface {
	sendMessage(chatID int64, text string) error
	sendMessageDraft(chatID int64, draftID int64, text string) error
}

type telegramClient struct {
	token string
	http  *http.Client
}

func newTelegramClient(token string) *telegramClient {
	return &telegramClient{
		token: token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *telegramClient) post(method string, payload map[string]interface{}) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.token, method)
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram %s failed: %s", method, resp.Status)
	}
	return nil
}

func (c *telegramClient) sendMessage(chatID int64, text string) error {
	return c.post("sendMessage", map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	})
}

func (c *telegramClient) sendMessageDraft(chatID int64, draftID int64, text string) error {
	return c.post("sendMessageDraft", map[string]interface{}{
		"chat_id":  chatID,
		"draft_id": draftID,
		"text":     text,
	})
}

func (c *telegramClient) fetchUpdates(offset int, timeout int) ([]byte, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d", c.token, offset, timeout)
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func splitTelegramText(text string, maxRunes int) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		n := maxRunes
		if len(runes) < n {
			n = len(runes)
		}
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
	}
	return chunks
}

type remoteOutputState struct {
	chatID          int64
	draftBuffer     strings.Builder
	activeDraftID   int64
	lastDraftSentAt time.Time
	lastDraftText   string
	now             func() time.Time
}

func newRemoteOutputState(chatID int64) *remoteOutputState {
	return &remoteOutputState{
		chatID: chatID,
		now:    time.Now,
	}
}

func (s *remoteOutputState) beginTurn(draftID int64) {
	s.activeDraftID = draftID
	s.draftBuffer.Reset()
	s.lastDraftText = ""
	s.lastDraftSentAt = time.Time{}
}

func (s *remoteOutputState) sendDraft(sender telegramSender, force bool) (bool, error) {
	if s.activeDraftID == 0 || s.draftBuffer.Len() == 0 {
		return false, nil
	}
	now := s.now()
	if !force && !s.lastDraftSentAt.IsZero() && now.Sub(s.lastDraftSentAt) < 700*time.Millisecond {
		return false, nil
	}
	textToSend := s.draftBuffer.String()
	if chunks := splitTelegramText(textToSend, 4096); len(chunks) > 0 {
		textToSend = chunks[0]
	}
	if textToSend == s.lastDraftText {
		return false, nil
	}
	if err := sender.sendMessageDraft(s.chatID, s.activeDraftID, textToSend); err != nil {
		return false, err
	}
	s.lastDraftText = textToSend
	s.lastDraftSentAt = now
	return true, nil
}

func (s *remoteOutputState) sendFinal(sender telegramSender, text string) error {
	if s.activeDraftID != 0 {
		_ = sender.sendMessageDraft(s.chatID, s.activeDraftID, "")
	}
	s.draftBuffer.Reset()
	s.activeDraftID = 0
	s.lastDraftText = ""
	if text == "" {
		return nil
	}
	for _, chunk := range splitTelegramText(text, 4096) {
		if err := sender.sendMessage(s.chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *remoteOutputState) handleEvent(sender telegramSender, event remoteResponseEvent) error {
	switch event.kind {
	case "delta":
		if s.activeDraftID != 0 {
			s.draftBuffer.WriteString(event.text)
		}
	case "final":
		return s.sendFinal(sender, event.text)
	case "turn_completed":
		_, err := s.sendDraft(sender, true)
		return err
	case "status", "error", "tool":
		for _, chunk := range splitTelegramText(event.text, 4096) {
			if err := sender.sendMessage(s.chatID, chunk); err != nil {
				return err
			}
		}
	}
	return nil
}

func augmentedAgyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/agent"
	}
	base := os.Getenv("PATH")
	if base == "" {
		base = "/usr/local/bin:/usr/bin:/bin"
	}
	return fmt.Sprintf("%s/bin:%s/.local/bin:%s/.local/share/fnm/aliases/default/bin:%s", home, home, home, base)
}

func agyTranscriptPath(convID string) string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain", convID, ".system_generated", "logs", "transcript.jsonl")
}

func agyConversationIDFromTranscriptPath(path string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
}

func extractAgyUserRequest(content string) string {
	start := strings.Index(content, "<USER_REQUEST>")
	end := strings.Index(content, "</USER_REQUEST>")
	if start < 0 || end < 0 || end < start {
		return strings.TrimSpace(content)
	}
	start += len("<USER_REQUEST>")
	return strings.TrimSpace(content[start:end])
}

func transcriptMatchesPrompt(path string, prompt string) bool {
	if prompt == "" {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var step map[string]interface{}
		if err := json.Unmarshal([]byte(line), &step); err != nil {
			continue
		}
		source, _ := step["source"].(string)
		typ, _ := step["type"].(string)
		content, _ := step["content"].(string)
		if source == "USER_EXPLICIT" && typ == "USER_INPUT" && extractAgyUserRequest(content) == prompt {
			return true
		}
	}
	return false
}

func isAgyNarration(text string) bool {
	lines := strings.Split(text, "\n")
	found := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		found = true
		if !strings.HasPrefix(trimmed, "I will") {
			return false
		}
	}
	return found
}

func getNewAgyConversationIDFromTranscripts(startedAt time.Time, beforeID string, prompt string) string {
	homeDir, _ := os.UserHomeDir()
	pattern := filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain", "*", ".system_generated", "logs", "transcript.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ""
	}

	var bestPath string
	var bestTime time.Time
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(startedAt.Add(-1 * time.Second)) {
			continue
		}
		convID := agyConversationIDFromTranscriptPath(path)
		if convID == beforeID {
			continue
		}
		if !transcriptMatchesPrompt(path, prompt) {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestTime) {
			bestPath = path
			bestTime = info.ModTime()
		}
	}
	if bestPath == "" {
		return ""
	}
	return agyConversationIDFromTranscriptPath(bestPath)
}

func latestAgyStepIndex(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()

	var latest int64 = -1
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var step map[string]interface{}
		if err := json.Unmarshal([]byte(line), &step); err != nil {
			continue
		}
		idxVal, ok := step["step_index"].(float64)
		if !ok {
			continue
		}
		idx := int64(idxVal)
		if idx > latest {
			latest = idx
		}
	}
	return latest
}

func readNewSteps(path string, lastSentStep *int64, responseChan chan<- remoteResponseEvent) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var step map[string]interface{}
		if err := json.Unmarshal([]byte(line), &step); err == nil {
			idxVal, ok := step["step_index"].(float64)
			if !ok {
				continue
			}
			idx := int64(idxVal)
			if idx > *lastSentStep {
				*lastSentStep = idx
				source, _ := step["source"].(string)
				typ, _ := step["type"].(string)
				content, _ := step["content"].(string)

				if source == "MODEL" {
					if typ == "PLANNER_RESPONSE" {
						var toolCallsMsg string
						if tCalls, ok := step["tool_calls"].([]interface{}); ok && len(tCalls) > 0 {
							for _, tcAny := range tCalls {
								if tc, ok := tcAny.(map[string]interface{}); ok {
									name, _ := tc["name"].(string)
									toolCallsMsg += fmt.Sprintf("\n*[Tool Call]* `%s`\n", name)
								}
							}
						}
						if toolCallsMsg != "" {
							responseChan <- remoteResponseEvent{kind: "tool", text: strings.TrimSpace(toolCallsMsg)}
						}
						msg := ""
						if !isAgyNarration(content) {
							msg = content
						}
						if msg != "" {
							responseChan <- remoteResponseEvent{kind: "delta", text: msg}
						}
					}
				}
			}
		}
	}
}

func pollAgyTranscript(convID string, lastSentStep *int64, responseChan chan<- remoteResponseEvent, doneChan <-chan struct{}) {
	transcriptPath := agyTranscriptPath(convID)

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-doneChan:
			readNewSteps(transcriptPath, lastSentStep, responseChan)
			return
		case <-ticker.C:
			readNewSteps(transcriptPath, lastSentStep, responseChan)
		}
	}
}

const agyBootstrapPrompt = "Internal remote bridge initialization. This is not a human user request and must not be counted as a previous user question. Start a reusable conversation session and reply with exactly READY."

func createAgySession(timeout time.Duration) (string, int64, error) {
	startedAt := time.Now()
	cmd := exec.Command("agy", "--print", agyBootstrapPrompt)
	cmd.Env = append(os.Environ(), "PATH="+augmentedAgyPath())
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}

	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	var convID string
	var cmdErr error
	cmdFinished := false
	ticker := time.NewTicker(100 * time.Millisecond)
	timer := time.NewTimer(timeout)
	defer ticker.Stop()
	defer timer.Stop()

waitForConversation:
	for {
		select {
		case <-ticker.C:
			convID = getNewAgyConversationIDFromTranscripts(startedAt, "", agyBootstrapPrompt)
			if convID != "" {
				break waitForConversation
			}
		case cmdErr = <-cmdDone:
			cmdFinished = true
			convID = getNewAgyConversationIDFromTranscripts(startedAt, "", agyBootstrapPrompt)
			if convID != "" {
				break waitForConversation
			}
			if cmdErr != nil {
				return "", -1, fmt.Errorf("agy bootstrap exited before transcript was found: %w", cmdErr)
			}
			return "", -1, fmt.Errorf("agy bootstrap exited before transcript was found")
		case <-timer.C:
			_ = cmd.Process.Kill()
			<-cmdDone
			return "", -1, fmt.Errorf("timed out waiting for agy bootstrap transcript")
		}
	}

	if !cmdFinished {
		cmdErr = <-cmdDone
	}
	if cmdErr != nil {
		return "", -1, cmdErr
	}

	return convID, latestAgyStepIndex(agyTranscriptPath(convID)), nil
}

func remoteCommand(token string) {
	fmt.Println("Starting am remote with Telegram integration...")
	fmt.Println("Please send a message '/start codex' or '/start agy' to your telegram bot.")

	tg := newTelegramClient(token)
	var chatID int64
	offset := 0
	var mode string // "codex" or "agy"

	// 1. Get Chat ID (auth) and Mode selection
	for chatID == 0 {
		out, err := tg.fetchUpdates(offset, 10)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		var resp TelegramResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		for _, up := range resp.Result {
			if up.UpdateID >= offset {
				offset = up.UpdateID + 1
			}
			if up.Message != nil && strings.HasPrefix(up.Message.Text, "/start") {
				chatID = up.Message.Chat.ID
				fmt.Printf("Authenticated chat ID: %d\n", chatID)
				txt := strings.TrimSpace(up.Message.Text)
				if strings.Contains(txt, "agy") {
					mode = "agy"
					_ = tg.sendMessage(chatID, "Authenticated! Launching agy CLI remote...")
				} else {
					mode = "codex"
					_ = tg.sendMessage(chatID, "Authenticated! Launching codex app-server...")
				}
				break
			}
		}
		time.Sleep(1 * time.Second)
	}

	var appServerCmd *exec.Cmd
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	var activeThreadID string
	var nextRequestID = 2
	var responseChan = make(chan remoteResponseEvent, 100)

	writeJSONRPCForCodex := func(w io.Writer, method string, id interface{}, params interface{}) {
		msg := map[string]interface{}{
			"method": method,
			"params": params,
		}
		if id != nil {
			msg["id"] = id
		}
		data, _ := json.Marshal(msg)
		fmt.Printf(">> Write app-server: %s\n", string(data))
		_, _ = w.Write(append(data, '\n'))
	}

	if mode == "codex" {
		appServerCmd = exec.Command("codex", "app-server", "--stdio")
		var err error
		stdin, err = appServerCmd.StdinPipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get stdin pipe: %v\n", err)
			os.Exit(1)
		}
		stdout, err = appServerCmd.StdoutPipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get stdout pipe: %v\n", err)
			os.Exit(1)
		}

		if err := appServerCmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start codex app-server: %v\n", err)
			_ = tg.sendMessage(chatID, "Error: Failed to launch codex app-server. Make sure codex CLI is installed.")
			os.Exit(1)
		}

		defer func() {
			_ = appServerCmd.Process.Kill()
		}()

		// Goroutine to read stdout from app-server
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				fmt.Printf("<< Read app-server: %s\n", line)

				var raw map[string]interface{}
				if err := json.Unmarshal([]byte(line), &raw); err != nil {
					continue
				}

				// Handle responses and notifications
				method, _ := raw["method"].(string)
				if method == "" {
					// This is a response.
					idVal, ok := raw["id"].(float64)
					if ok {
						if idVal == 0 { // Response to 'initialize'
							// 1. Send initialized notification (client -> server)
							writeJSONRPCForCodex(stdin, "initialized", nil, map[string]interface{}{})
							// 2. Start thread immediately
							writeJSONRPCForCodex(stdin, "thread/start", 1, map[string]interface{}{})
						} else if idVal == 1 { // Response to 'thread/start'
							if result, exists := raw["result"].(map[string]interface{}); exists {
								if thread, exists := result["thread"].(map[string]interface{}); exists {
									activeThreadID, _ = thread["id"].(string)
									fmt.Printf("Codex active thread: %s\n", activeThreadID)
									responseChan <- remoteResponseEvent{kind: "status", text: fmt.Sprintf("Codex thread started! (ID: %s)", activeThreadID)}
								}
							}
						}
					}
				} else {
					// This is a notification
					switch method {
					case "item/agentMessage/delta":
						if params, exists := raw["params"].(map[string]interface{}); exists {
							if delta, exists := params["delta"].(string); exists {
								responseChan <- remoteResponseEvent{kind: "delta", text: delta}
							}
						}
					case "item/completed":
						if params, exists := raw["params"].(map[string]interface{}); exists {
							if item, exists := params["item"].(map[string]interface{}); exists {
								itemType, _ := item["type"].(string)
								text, _ := item["text"].(string)
								if itemType == "agentMessage" && text != "" {
									responseChan <- remoteResponseEvent{kind: "final", text: text}
								}
							}
						}
					case "turn/completed":
						responseChan <- remoteResponseEvent{kind: "turn_completed", text: ""}
					case "error":
						if params, exists := raw["params"].(map[string]interface{}); exists {
							if msg, exists := params["message"].(string); exists {
								responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("[Error] %s", msg)}
							}
						}
					}
				}
			}
		}()

		// 3. Initialize app-server protocol handshake
		writeJSONRPCForCodex(stdin, "initialize", 0, map[string]interface{}{
			"clientInfo": map[string]interface{}{
				"name":    "agents_manager",
				"title":   "agents-manager",
				"version": "0.1.0",
			},
		})
	}

	var isGenerating int32 // 0 = idle, 1 = busy
	var activeAgyConversationID string
	var activeAgyLastStep int64 = -1

	if mode == "agy" {
		_ = tg.sendMessage(chatID, "Starting agy session...")
		convID, lastStep, err := createAgySession(30 * time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to initialize agy session: %v\n", err)
			_ = tg.sendMessage(chatID, fmt.Sprintf("Failed to initialize agy session: %v", err))
		} else {
			activeAgyConversationID = convID
			activeAgyLastStep = lastStep
			_ = tg.sendMessage(chatID, fmt.Sprintf("Agy session started! (ID: %s)", convID))
		}
	}

	// 4. Polling loop for Telegram messages & streaming response output
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	outputState := newRemoteOutputState(chatID)
	handleResponseEvent := func(event remoteResponseEvent) {
		if event.kind == "final" || event.kind == "status" || event.kind == "error" {
			fmt.Printf("Sent to Telegram:\n%s\n--------------------\n", event.text)
		}
		if err := outputState.handleEvent(tg, event); err != nil {
			fmt.Fprintf(os.Stderr, "failed to handle remote output event %s: %v\n", event.kind, err)
		}
	}
	drainResponseEvents := func() {
		for {
			select {
			case event := <-responseChan:
				handleResponseEvent(event)
			default:
				return
			}
		}
	}

	for {
		select {
		case <-ticker.C:
			drainResponseEvents()

			// A. Poll for new messages from Telegram
			out, err := tg.fetchUpdates(offset, 0)
			if err == nil {
				var resp TelegramResponse
				if err := json.Unmarshal(out, &resp); err == nil {
					for _, up := range resp.Result {
						if up.UpdateID >= offset {
							offset = up.UpdateID + 1
						}
						if up.Message != nil && up.Message.Chat.ID == chatID {
							text := up.Message.Text
							fmt.Printf("Telegram user input: %s\n", text)
							if mode == "codex" {
								if activeThreadID == "" {
									_ = tg.sendMessage(chatID, "Waiting for Codex thread to initialize...")
									continue
								}

								requestID := nextRequestID
								nextRequestID++
								outputState.beginTurn(time.Now().UnixNano())
								// Send turn/start to start processing
								writeJSONRPCForCodex(stdin, "turn/start", requestID, map[string]interface{}{
									"threadId": activeThreadID,
									"input": []interface{}{
										map[string]interface{}{
											"type": "text",
											"text": text,
										},
									},
								})
							} else if mode == "agy" {
								if atomic.LoadInt32(&isGenerating) == 1 {
									_ = tg.sendMessage(chatID, "Antigravity is busy. Please wait...")
									continue
								}
								atomic.StoreInt32(&isGenerating, 1)
								outputState.beginTurn(time.Now().UnixNano())

								go func(promptText string) {
									defer atomic.StoreInt32(&isGenerating, 0)

									resumeConversationID := activeAgyConversationID
									args := []string{}
									if resumeConversationID != "" {
										args = append(args, "--conversation", resumeConversationID)
									}
									args = append(args, "--print", promptText)
									startedAt := time.Now()

									augmentedPath := augmentedAgyPath()
									cmd := exec.Command("agy", args...)
									cmd.Env = append(os.Environ(), "PATH="+augmentedPath)

									err := cmd.Start()
									if err != nil {
										responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("Failed to start agy: %v", err)}
										return
									}
									cmdDone := make(chan error, 1)
									go func() {
										cmdDone <- cmd.Wait()
									}()

									var tempConvID string
									var cmdErr error
									cmdFinished := false
									transcriptTicker := time.NewTicker(100 * time.Millisecond)
									transcriptTimeout := time.NewTimer(30 * time.Second)
								waitForTranscript:
									for {
										select {
										case <-transcriptTicker.C:
											if resumeConversationID != "" {
												if transcriptMatchesPrompt(agyTranscriptPath(resumeConversationID), promptText) {
													tempConvID = resumeConversationID
												}
											} else {
												tempConvID = getNewAgyConversationIDFromTranscripts(startedAt, "", promptText)
											}
											if tempConvID != "" {
												if resumeConversationID == "" {
													responseChan <- remoteResponseEvent{kind: "status", text: fmt.Sprintf("Agy session created! (ID: %s)", tempConvID)}
												}
												break waitForTranscript
											}
										case cmdErr = <-cmdDone:
											cmdFinished = true
											if resumeConversationID != "" {
												if transcriptMatchesPrompt(agyTranscriptPath(resumeConversationID), promptText) {
													tempConvID = resumeConversationID
												}
											} else {
												tempConvID = getNewAgyConversationIDFromTranscripts(startedAt, "", promptText)
											}
											if tempConvID != "" {
												if resumeConversationID == "" {
													responseChan <- remoteResponseEvent{kind: "status", text: fmt.Sprintf("Agy session created! (ID: %s)", tempConvID)}
												}
												break waitForTranscript
											}
											if cmdErr != nil {
												responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("agy exited before transcript was found: %v", cmdErr)}
											} else {
												responseChan <- remoteResponseEvent{kind: "error", text: "agy exited before transcript was found."}
											}
											return
										case <-transcriptTimeout.C:
											_ = cmd.Process.Kill()
											cmdErr = <-cmdDone
											cmdFinished = true
											responseChan <- remoteResponseEvent{kind: "error", text: "Timed out waiting for agy transcript for this prompt."}
											return
										}
									}
									transcriptTicker.Stop()
									transcriptTimeout.Stop()

									donePoll := make(chan struct{})
									lastSentStep := activeAgyLastStep
									go pollAgyTranscript(tempConvID, &lastSentStep, responseChan, donePoll)

									if !cmdFinished {
										cmdErr = <-cmdDone
									}
									close(donePoll)
									activeAgyConversationID = tempConvID
									activeAgyLastStep = latestAgyStepIndex(agyTranscriptPath(tempConvID))

									if cmdErr != nil {
										responseChan <- remoteResponseEvent{kind: "error", text: fmt.Sprintf("agy exited with error: %v", cmdErr)}
									} else {
										responseChan <- remoteResponseEvent{kind: "turn_completed", text: ""}
									}
								}(text)
							}
						}
					}
				}
			}

			drainResponseEvents()
			if sent, err := outputState.sendDraft(tg, false); err != nil {
				fmt.Fprintf(os.Stderr, "failed to send Telegram draft: %v\n", err)
			} else if sent {
				fmt.Printf("Sent Telegram draft:\n%s\n--------------------\n", outputState.lastDraftText)
			}

		case event := <-responseChan:
			handleResponseEvent(event)
		}
	}
}

func main() {
	argv := os.Args[1:]

	if len(argv) == 0 || argv[0] == "--help" || argv[0] == "-h" {
		fmt.Println(usage())
		return
	}

	debug := false
	for _, arg := range argv {
		if arg == "--debug" {
			debug = true
			break
		}
	}

	cmd, candidates, err := resolveCommand(argv[0], []string{"sessions", "search", "remote"})
	if err != nil {
		if len(candidates) > 0 {
			for _, c := range candidates {
				fmt.Println(c)
			}
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "\nTry: am sessions | am search <keyword> | am remote --telegram <token>")
		os.Exit(1)
	}
	rest := argv[1:]

	if cmd == "sessions" {
		ns, err := parseSessionsArgs(rest)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if ns.Help {
			fmt.Println(usage())
			return
		}
		if debug {
			ns.Debug = true
		}
		sessionsCommand(ns)
		return
	}

	if cmd == "search" {
		ns, err := parseSearchArgs(rest)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if ns.Help {
			fmt.Println(usage())
			return
		}
		if debug {
			ns.Debug = true
		}
		if ns.Keyword == "" {
			fmt.Fprintln(os.Stderr, "Missing <keyword>. Try: am search <keyword>")
			os.Exit(1)
		}
		searchCommand(ns)
		return
	}

	loadTokenFromEnvTest := func() string {
		f, err := os.Open(".env.test")
		if err != nil {
			return ""
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "TELEGRAM_BOT_TOKEN=") {
				return strings.TrimPrefix(line, "TELEGRAM_BOT_TOKEN=")
			}
		}
		return ""
	}

	if cmd == "remote" {
		var token string
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--telegram" {
				if i+1 < len(rest) {
					token = rest[i+1]
					i++
				}
			}
		}
		if token == "" {
			token = loadTokenFromEnvTest()
		}
		if token == "" {
			fmt.Fprintln(os.Stderr, "Error: --telegram <token> is required for remote command or configure it in .env.test")
			os.Exit(1)
		}
		remoteCommand(token)
		return
	}
}

func resolveCommand(input string, commands []string) (string, []string, error) {
	in := strings.ToLower(strings.TrimSpace(input))
	if in == "" {
		return "", nil, errors.New("missing command")
	}
	for _, c := range commands {
		if c == in {
			return c, nil, nil
		}
	}
	var prefixMatches []string
	for _, c := range commands {
		if strings.HasPrefix(c, in) {
			prefixMatches = append(prefixMatches, c)
		}
	}
	if len(prefixMatches) == 1 {
		return prefixMatches[0], nil, nil
	}
	if len(prefixMatches) > 1 {
		return "", prefixMatches, fmt.Errorf("ambiguous command %q: matches %s", input, strings.Join(prefixMatches, ", "))
	}
	var subseqMatches []string
	for _, c := range commands {
		if isSubsequence(in, c) {
			subseqMatches = append(subseqMatches, c)
		}
	}
	if len(subseqMatches) == 1 {
		return subseqMatches[0], nil, nil
	}
	if len(subseqMatches) > 1 {
		return "", subseqMatches, fmt.Errorf("ambiguous command %q: matches %s", input, strings.Join(subseqMatches, ", "))
	}
	return "", nil, fmt.Errorf("unknown command: %s", input)
}

func isSubsequence(needle, haystack string) bool {
	if needle == "" {
		return true
	}
	n := []rune(strings.ToLower(needle))
	h := []rune(strings.ToLower(haystack))
	i := 0
	for _, r := range h {
		if r == n[i] {
			i++
			if i == len(n) {
				return true
			}
		}
	}
	return false
}
