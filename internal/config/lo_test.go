package config_test

import (
	"testing"

	"github.com/duckbugio/flock/internal/config"
)

func TestLOConfigRequiresIndependentIdentity(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		LOAPIURL: "https://lo.example", LOBotToken: "123:test", LOAllowedUsers: []int64{77}, AllowedUsers: []int64{88},
	}
	if err := cfg.ValidateLO(); err != nil {
		t.Fatal(err)
	}
	if !cfg.IsLOAllowed(77) || cfg.IsLOAllowed(88) || cfg.IsLOAllowed(0) {
		t.Fatal("LO inherited another transport's identity")
	}
	for _, test := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"no token", func(c *config.Config) { c.LOBotToken = "" }},
		{"no endpoint", func(c *config.Config) { c.LOAPIURL = "" }},
		{"no users", func(c *config.Config) { c.LOAllowedUsers = nil }},
		{"negative user", func(c *config.Config) { c.LOAllowedUsers = []int64{-1} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := cfg
			test.change(&candidate)
			if err := candidate.ValidateLO(); err == nil {
				t.Fatal("accepted invalid LO config")
			}
		})
	}
}
