package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err == nil {
		t.Fatal("explicit missing config file should be an error")
	}

	// Default path missing is fine: point the resolver at a clean env
	t.Setenv("CERBO_NUT_CONFIG", "")
	c, err = loadConfig("")
	if err != nil {
		t.Fatalf("defaults should load: %v", err)
	}
	if c.Server.Listen != "0.0.0.0:3493" || c.UPS.Name != "ups" || c.Power.InverterMaxVA != 1600.0 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if len(c.Users) != 0 {
		t.Fatalf("no users expected by default, got %d", len(c.Users))
	}
}

func TestLoadConfigFileAndEnvPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[server]
listen = "127.0.0.1:13493"

[power]
inverter_max_va = 3000.0

[[users]]
username = "synology"
password = "filepass"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CERBO_NUT_INVERTER_MAX_VA", "2400")
	t.Setenv("CERBO_NUT_USERNAME", "synology")
	t.Setenv("CERBO_NUT_PASSWORD", "envpass")

	c, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Server.Listen != "127.0.0.1:13493" {
		t.Errorf("file value not applied: %s", c.Server.Listen)
	}
	if c.Power.InverterMaxVA != 2400 {
		t.Errorf("env must override file: %v", c.Power.InverterMaxVA)
	}
	if c.Power.BatteryCapacityWh != 5120.0 {
		t.Errorf("untouched default lost: %v", c.Power.BatteryCapacityWh)
	}
	if len(c.Users) != 1 || c.Users[0].Password != "envpass" {
		t.Errorf("env user must override file user with same name: %+v", c.Users)
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[server]\nlisten = \"0.0.0.0:3493\"\nlissten_typo = true\n"), 0o644)

	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("expected unknown-keys error, got: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"empty listen":      func(c *Config) { c.Server.Listen = "" },
		"zero inverter":     func(c *Config) { c.Power.InverterMaxVA = 0 },
		"ups name space":    func(c *Config) { c.UPS.Name = "my ups" },
		"user without pass": func(c *Config) { c.Users = []UserConfig{{Username: "a"}} },
		"duplicate username": func(c *Config) {
			c.Users = []UserConfig{{Username: "a", Password: "x"}, {Username: "a", Password: "y"}}
		},
	}
	for name, mutate := range cases {
		c := defaultConfig()
		mutate(c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestValidateRejectsBadNetworks(t *testing.T) {
	c := defaultConfig()
	c.Users = []UserConfig{{Username: "a", Password: "x", AllowedNetworks: []string{"not-a-network"}}}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "allowed_networks") {
		t.Fatalf("expected allowed_networks error, got: %v", err)
	}
}

func TestEnvUserWithNetworks(t *testing.T) {
	t.Setenv("CERBO_NUT_CONFIG", "")
	t.Setenv("CERBO_NUT_USERNAME", "envuser")
	t.Setenv("CERBO_NUT_PASSWORD", "envpass")
	t.Setenv("CERBO_NUT_ALLOWED_NETWORKS", "192.168.1.0/24, 10.0.0.1")

	c, err := loadConfig("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Users) != 1 || len(c.Users[0].AllowedNetworks) != 2 {
		t.Fatalf("env user networks not applied: %+v", c.Users)
	}
	if !c.Users[0].ipAllowed("192.168.1.9:1") || c.Users[0].ipAllowed("172.16.0.1:1") {
		t.Fatal("env-configured networks not enforced")
	}
}
