package commands

import (
	"fmt"
	"strings"

	"github.com/enowdev/antares/internal/config"
)

// modelChoices returns the configured models worth offering as buttons:
// provider-curated list first, then fallbacks, then auxiliary.
func modelChoices(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	if p, ok := cfg.Providers[cfg.Model.Provider]; ok {
		for _, m := range p.Models {
			add(m)
		}
	}
	for _, m := range cfg.Model.Fallback {
		add(m)
	}
	add(cfg.Model.Auxiliary)
	return out
}

// modelPickerText is the /model reply without args: current model plus how to
// pick. Telegram renders the choices as tappable inline buttons.
func modelPickerText(cfg *config.Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Model `%s` on provider `%s`.",
		orDash(cfg.Model.Default), orDash(cfg.Model.Provider))
	if choices := modelChoices(cfg); len(choices) > 0 {
		b.WriteString("\n\nTap to switch:")
		for _, c := range choices {
			fmt.Fprintf(&b, "\n- `%s`", c)
		}
	} else {
		b.WriteString("\n\nChange it with `/model <id>`.")
	}
	return b.String()
}
