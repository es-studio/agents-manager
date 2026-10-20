package codexlaunch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartupStatusWithoutSessionLog(t *testing.T) {
	directory := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.test'; exit; fi
while IFS= read -r request; do
 case "$request" in
 *'"method":"initialize"'*) echo '{"id":1,"result":{}}';;
 *'"method":"config/read"'*) echo '{"id":2,"result":{"config":{"model":"configured-model","model_reasoning_effort":"high"}}}';;
 *'"method":"account/rateLimits/read"'*) echo '{"id":3,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":31,"windowDurationMins":10080,"resetsAt":2500}}}}';;
 esac
done
`
	if err := os.WriteFile(filepath.Join(directory, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+":"+os.Getenv("PATH"))
	updates := make(chan status, 3)
	startupStatus(context.Background(), startupOptions{directory: directory, configAvailable: true, model: "override-model"}, updates)
	var initial status
	for len(updates) > 0 {
		initial = <-updates
	}
	if initial.Model != "override-model" || initial.Effort != "high" || initial.Version != "0.test" {
		t.Fatalf("initial status: %+v", initial)
	}
	line := initial.line(time.Unix(1000, 0))
	if !strings.Contains(line, "7d:69% (25m)") || strings.Contains(line, "5h:") {
		t.Fatal(line)
	}
	live := status{Model: "live-model", Effort: "low", Usage: &usageInfo{Window: 1000}, Limits: &rateLimits{}}
	merged := live.withStartup(initial)
	if merged.Model != "live-model" || merged.Effort != "low" || merged.Limits != live.Limits || merged.Version != "0.test" {
		t.Fatalf("startup replaced live data: %+v", merged)
	}
}

func TestStartupLimitsSelectCodexBucket(t *testing.T) {
	data := []byte(`{"rateLimits":{"limitId":"other","primary":{"usedPercent":100}},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":31,"windowDurationMins":10080}}}}`)
	result := startupLimits(data)
	if result == nil || result.Primary == nil || result.Primary.Used != 31 || result.Primary.Minutes != 10080 {
		t.Fatalf("wrong bucket: %+v", result)
	}
	if startupLimits([]byte(`{"rateLimits":{"limitId":"other"}}`)) != nil {
		t.Fatal("unrelated bucket accepted")
	}
}

func TestStartupOverridesAndCancellation(t *testing.T) {
	options := startupOptionsFor([]string{"--model=chosen", "--cd", "/tmp", "-c", "model_reasoning_effort=high", "--", "--model=ignored"})
	if options.model != "chosen" || options.directory != "/tmp" || len(options.overrides) != 2 {
		t.Fatalf("options: %+v", options)
	}
	if startupOptionsFor([]string{"resume", "--last"}).configAvailable {
		t.Fatal("resume must not guess session model from global config")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { startupStatus(ctx, options, make(chan status)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startup did not cancel")
	}
}
