package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

type progressFn func(provider string, current, total int)

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
	roots := make(map[string]bool)

	// 1. Check CLAUDE_CONFIG_DIR
	if envDir := os.Getenv("CLAUDE_CONFIG_DIR"); envDir != "" {
		roots[filepath.Join(envDir, "projects")] = true
	}

	// 2. Scan home directory for all .claude* directories
	if matches, err := filepath.Glob(filepath.Join(homeDir, ".claude*")); err == nil {
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err == nil && fi.IsDir() {
				roots[filepath.Join(m, "projects")] = true
			}
		}
	}

	// Ensure default path is included
	roots[filepath.Join(homeDir, ".claude", "projects")] = true

	var allJSONL []string
	for root := range roots {
		if debug {
			fmt.Fprintf(os.Stderr, "[claude] root: %s\n", root)
		}
		if _, err := os.Stat(root); err != nil {
			continue
		}
		jsonls := listFilesDepth(root, 2, func(p string) bool {
			return filepath.Ext(p) == ".jsonl"
		})
		allJSONL = append(allJSONL, jsonls...)
	}

	if len(allJSONL) == 0 {
		return nil
	}

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

func getLastLines(text string, n int) string {
	if text == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(text, "\n")
	var filtered []string
	for _, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			filtered = append(filtered, ln)
		}
	}
	if len(filtered) > n {
		filtered = filtered[len(filtered)-n:]
	}
	return strings.Join(filtered, "\n")
}

func matchSession(session Session, needle string, caseSensitive bool, snippetLines int) (bool, string) {
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
	if transcript == "" {
		return true, ""
	}
	return true, getLastLines(transcript, snippetLines)
}
