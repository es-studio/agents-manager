package codexlaunch

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

type gitState struct {
	directory string
	checked   time.Time
	branch    string
	dirty     bool
}

const gitRefreshInterval = 5 * time.Second

func (g *gitState) refresh(ctx context.Context, directory string, now time.Time) {
	if directory == "" {
		return
	}
	if directory == g.directory && now.Sub(g.checked) < gitRefreshInterval {
		return
	}
	g.directory, g.checked = directory, now
	g.branch, g.dirty = "", false
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	branch, err := exec.CommandContext(ctx, "git", "-C", directory, "symbolic-ref", "--short", "-q", "HEAD").Output()
	if err != nil {
		short, err := exec.CommandContext(ctx, "git", "-C", directory, "rev-parse", "--short", "HEAD").Output()
		if err != nil {
			return
		}
		branch = short
	}
	g.branch = strings.TrimSpace(string(branch))
	changes, err := exec.CommandContext(ctx, "git", "-C", directory, "status", "--porcelain", "--untracked-files=no").Output()
	g.dirty = err == nil && len(strings.TrimSpace(string(changes))) > 0
}
