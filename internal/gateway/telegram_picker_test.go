package gateway

import (
	"testing"

	"github.com/enowdev/antares/internal/config"
)

func testPickerConfig() *config.Config {
	cfg := config.Default()
	cfg.Model.Default = "m1"
	cfg.Model.Provider = "p1"
	cfg.Providers = map[string]config.Provider{
		"p1": {Models: []string{"m1", "m2"}},
		"p2": {},
	}
	cfg.Model.Fallback = []string{"m3"}
	return cfg
}

func TestKeyboardForModel(t *testing.T) {
	kb := keyboardFor(testPickerConfig(), "model")
	if len(kb) != 3 {
		t.Fatalf("want 3 rows, got %d", len(kb))
	}
	if kb[0][0].CallbackData != "model:m1" || kb[0][0].Text != "● m1" {
		t.Fatalf("active model not marked: %+v", kb[0][0])
	}
}

func TestKeyboardForProvider(t *testing.T) {
	kb := keyboardFor(testPickerConfig(), "provider")
	if len(kb) != 2 || kb[0][0].CallbackData != "provider:p1" {
		t.Fatalf("got %+v", kb)
	}
}

func TestValidChoices(t *testing.T) {
	cfg := testPickerConfig()
	for _, id := range []string{"m1", "m2", "m3"} {
		if !validModelChoice(cfg, id) {
			t.Fatalf("reject %s", id)
		}
	}
	if validModelChoice(cfg, "nope") || !validProviderChoice(cfg, "p2") || validProviderChoice(cfg, "nope") {
		t.Fatal("choice validation wrong")
	}
}

func TestParseCommand(t *testing.T) {
	n, a, ok := parseCommand("/model foo")
	if !ok || n != "model" || a != "foo" {
		t.Fatalf("got %q %q %v", n, a, ok)
	}
	if _, _, ok := parseCommand("hello"); ok {
		t.Fatal("plain text parsed as command")
	}
	if _, _, ok := parseCommand("/etc/hosts"); !ok {
		t.Fatal("slash input must still parse; routing decides")
	}
}
