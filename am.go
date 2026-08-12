package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const usageStr = `am (agents-manager): local agent session history viewer

Usage:
  am --help
  am sessions [options]
  am sessions <dir> [options]
  am search <keyword> [<dir>] [options]
  am remote --telegram <token>

Options (sessions):
  --provider <name>   Filter providers (repeatable). One of: codex, claude, cursor, antigravity
  -n, --limit <n>     Limit rows (default: 20). In <dir> mode this applies to merged results.
  -p, --period <days> Search recent days (default: 7). Ignored if --all is set.
  --all               Disable limit and period filter
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
  <dir>               Optional directory to filter sessions (exact match)
  --provider <name>   Filter providers (repeatable)
  -n, --limit <n>     Limit matched rows (default: 20). Use --all to disable.
  -p, --period <days> Search recent days (default: 7). Ignored if --all is set.
  --all               Disable limit and period filter
  --case-sensitive    Case-sensitive match (default: case-insensitive)
  --project <substr>  Additional substring filter on project/cwd/title/id
  --snippet <lines>   Number of transcript lines to show in snippet (default: 10)
  --tui / --no-tui    Same selector behavior as ` + "`" + `sessions <dir>` + "`" + `
  --json              Output JSON (includes a short snippet around the first hit)
  --debug             Print debug info to stderr

Options (remote):
  --telegram <token>  Bridge codex / agy / claude over stdio through a Telegram bot
                      (pick the agent with /start_codex, /start_agy or /start_claude)`

func usage() string {
	return usageStr
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
	PeriodDays  int
}

func parseSessionsArgs(args []string) (*SessionsArgs, error) {
	res := &SessionsArgs{
		Limit:      20,
		PeriodDays: 7,
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
			res.PeriodDays = 0
		case "-p", "--period":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for -p/--period")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid period value: %s", args[i+1])
			}
			res.PeriodDays = val
			i++
		case "--copy-context", "-cc":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for --copy-context / -cc")
			}
			res.CopyContext = args[i+1]
			i++
		case "-n", "--limit":
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

	validProviders := map[string]bool{"codex": true, "claude": true, "cursor": true, "antigravity": true}
	for _, pr := range providerFlags {
		if !validProviders[pr] {
			return nil, fmt.Errorf("unknown provider: %s", pr)
		}
	}
	if len(providerFlags) > 0 {
		res.Providers = providerFlags
	} else {
		res.Providers = []string{"codex", "claude", "cursor", "antigravity"}
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
	DirFilter     string
	SnippetLines  int
	PeriodDays    int
}

func parseSearchArgs(args []string) (*SearchArgs, error) {
	res := &SearchArgs{
		Limit:        20,
		SnippetLines: 10,
		PeriodDays:   7,
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
			res.PeriodDays = 0
		case "-p", "--period":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for -p/--period")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid period value: %s", args[i+1])
			}
			res.PeriodDays = val
			i++
		case "--case-sensitive", "-s":
			res.CaseSensitive = true
		case "-n", "--limit":
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
		case "--snippet":
			if i+1 >= len(args) {
				return nil, errors.New("missing argument for -n")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid -n value: %s", args[i+1])
			}
			res.SnippetLines = val
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
			} else if res.DirFilter == "" {
				res.DirFilter = arg
			} else {
				return nil, fmt.Errorf("extra positional argument: %s", arg)
			}
		}
	}

	if res.Limit < 0 {
		return nil, errors.New("invalid --limit (must be >= 0)")
	}

	validProviders := map[string]bool{"codex": true, "claude": true, "cursor": true, "antigravity": true}
	for _, pr := range providerFlags {
		if !validProviders[pr] {
			return nil, fmt.Errorf("unknown provider: %s", pr)
		}
	}
	if len(providerFlags) > 0 {
		res.Providers = providerFlags
	} else {
		res.Providers = []string{"codex", "claude", "cursor", "antigravity"}
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

	if ns.PeriodDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -ns.PeriodDays)
		var recent []Session
		for _, s := range allSessions {
			if s.UpdatedAt.After(cutoff) {
				recent = append(recent, s)
			}
		}
		allSessions = recent
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
			action, sid, ok := tuiSelectID(merged, normPath(dirFilter), nil)
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

	if wantTUI(ns.NoTUI, ns.TUI) {
		if len(allSessions) == 0 {
			fmt.Println("(no sessions found)")
			return
		}
		action, sid, ok := tuiSelectID(allSessions, "", ns.Providers)
		if ok {
			if action == "copy-context" {
				session, err := findSessionByID(allSessions, sid)
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

	first := true
	for _, pr := range []string{"codex", "claude", "cursor", "antigravity"} {
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
			byProvider["codex"] = getCodexSessions(0, "", ns.Debug, ns.DirFilter, onLoadProgress)
		case "claude":
			byProvider["claude"] = getClaudeSessions(0, "", ns.Debug, onLoadProgress)
		case "cursor":
			byProvider["cursor"] = getCursorSessions(0, "", ns.Debug, onLoadProgress)
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
	if ns.DirFilter != "" {
		allSessions = applyDirFilterExact(allSessions, ns.DirFilter)
	}
	allSessions = applyProjectFilter(allSessions, ns.Project)

	if ns.PeriodDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -ns.PeriodDays)
		var recent []Session
		for _, s := range allSessions {
			if s.UpdatedAt.After(cutoff) {
				recent = append(recent, s)
			}
		}
		allSessions = recent
	}

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
		ok, snip := matchSession(s, keyword, caseSensitive, ns.SnippetLines)
		if ok {
			s.Snippet = snip
			matched = append(matched, s)
			if snip != "" {
				snippets[s.ID] = snip
			}
		}
		if progressOn && (i%updateEvery == 0 || i == total-1) {
			os.Stderr.WriteString(progressRender(i+1, total, len(matched), 28))
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
		tuiTitle := fmt.Sprintf("search: %s", keyword)
		if ns.DirFilter != "" {
			tuiTitle = fmt.Sprintf("search: %s | dir: %s", keyword, ns.DirFilter)
		}
		action, sid, ok := tuiSelectID(matched, tuiTitle, nil)
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

	for _, s := range matched {
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

		fmt.Printf("[%-4s] %s > %s\n", abbrevProvider(s.Provider), projBase, cleanTitle)
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
