package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/agent"
	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/store"
)

func openTestDB(t *testing.T) store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), "memory", "", 1, 5000, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testDeps(a *agent.Agent) Deps {
	cfg := config.Default()
	return Deps{
		Config: func() *config.Config { return cfg },
		Agent:  a,
		Store:  nil,
		Reload: func() error { return nil },
	}
}

func TestAgentsEmpty(t *testing.T) {
	db := openTestDB(t)
	a := agent.New(config.Default(), db, nil, nil, nil)
	res, err := cmdAgents(context.Background(), testDeps(a), Input{})
	if err != nil || !strings.Contains(res.Output, "No agents running") {
		t.Fatalf("got %q %v", res.Output, err)
	}
}

func TestTasksEmpty(t *testing.T) {
	db := openTestDB(t)
	a := agent.New(config.Default(), db, nil, nil, nil)
	res, err := cmdTasks(context.Background(), testDeps(a), Input{})
	if err != nil || !strings.Contains(res.Output, "No background tasks") {
		t.Fatalf("got %q %v", res.Output, err)
	}
}

func TestVerboseToggle(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Default()
	cfg.Display.ToolProgress = true
	a := agent.New(cfg, db, nil, nil, nil)
	d := testDeps(a)
	d.Config = func() *config.Config { return cfg }
	res, err := cmdVerbose(context.Background(), d, Input{Args: "off"})
	if err != nil || cfg.Display.ToolProgress {
		t.Fatalf("got %q %v prog=%v", res.Output, err, cfg.Display.ToolProgress)
	}
	if _, err := cmdVerbose(context.Background(), d, Input{Args: "bogus"}); err == nil {
		t.Fatal("bogus arg accepted")
	}
}

func TestModelPickerText(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Default = "m1"
	cfg.Model.Provider = "p1"
	cfg.Providers = map[string]config.Provider{"p1": {Models: []string{"m1", "m2"}}}
	out := modelPickerText(cfg)
	if !strings.Contains(out, "m1") || !strings.Contains(out, "m2") {
		t.Fatalf("got %q", out)
	}
}
