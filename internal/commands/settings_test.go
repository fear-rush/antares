package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/config"
)

type fakeGateway struct {
	restarted []string
	fail      map[string]bool
}

func (f *fakeGateway) Restart(platform string) error {
	f.restarted = append(f.restarted, platform)
	if f.fail[platform] {
		return errNoAgent
	}
	return nil
}

func settingsDeps(cfg *config.Config, gw GatewayControls) Deps {
	return Deps{
		Config:  func() *config.Config { return cfg },
		Gateway: gw,
		Reload:  func() error { return nil },
	}
}

func TestSettingsRedactsSecrets(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Default = "m1"
	cfg.Model.Provider = "p1"
	for _, p := range cfg.Providers {
		p.APIKey = "sk-secret-value"
		cfg.Providers["x"] = p
		break
	}
	d := settingsDeps(cfg, &fakeGateway{})
	res, err := cmdSettings(context.Background(), d, Input{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "sk-secret-value") {
		t.Fatalf("secret leaked: %q", res.Output)
	}
	if !strings.Contains(res.Output, "model.default") || !strings.Contains(res.Output, "m1") {
		t.Fatalf("missing model section: %q", res.Output)
	}
}

func TestSettingsSinglePath(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Default = "m1"
	d := settingsDeps(cfg, &fakeGateway{})
	res, err := cmdSettings(context.Background(), d, Input{Args: "model.default"})
	if err != nil || !strings.Contains(res.Output, "m1") {
		t.Fatalf("got %q %v", res.Output, err)
	}
	if _, err := cmdSettings(context.Background(), d, Input{Args: "nope.missing"}); err == nil {
		t.Fatal("bad path accepted")
	}
}

func TestRestartGateway(t *testing.T) {
	gw := &fakeGateway{}
	d := settingsDeps(config.Default(), gw)
	res, err := cmdRestart(context.Background(), d, Input{Args: "gateway"})
	if err != nil || !strings.Contains(res.Output, "reconnect") {
		t.Fatalf("got %q %v", res.Output, err)
	}
	if len(gw.restarted) != 1 || gw.restarted[0] != "" {
		t.Fatalf("got %v", gw.restarted)
	}
	res, err = cmdRestart(context.Background(), d, Input{Args: "gateway telegram"})
	if err != nil || !strings.Contains(res.Output, "telegram") {
		t.Fatalf("got %q %v", res.Output, err)
	}
}

func TestRestartAgent(t *testing.T) {
	d := settingsDeps(config.Default(), &fakeGateway{})
	res, err := cmdRestart(context.Background(), d, Input{Args: "agent"})
	if err != nil || !strings.Contains(res.Output, "reloaded") {
		t.Fatalf("got %q %v", res.Output, err)
	}
}

func TestRestartErrors(t *testing.T) {
	d := settingsDeps(config.Default(), nil)
	if _, err := cmdRestart(context.Background(), d, Input{Args: "gateway"}); err == nil {
		t.Fatal("nil gateway accepted")
	}
	d = settingsDeps(config.Default(), &fakeGateway{})
	if _, err := cmdRestart(context.Background(), d, Input{Args: "bogus"}); err == nil {
		t.Fatal("bogus target accepted")
	}
}
