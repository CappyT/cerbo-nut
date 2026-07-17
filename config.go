package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultConfigPath is used when no --config flag or CERBO_NUT_CONFIG
// environment variable is provided. A missing file at this path is not an
// error: the built-in defaults apply.
const DefaultConfigPath = "/data/cerbo-nut/config.toml"

// Config holds every runtime setting. Values are resolved in order of
// precedence: built-in defaults < TOML config file < CERBO_NUT_* environment
// variables.
type Config struct {
	Server struct {
		Listen    string `toml:"listen"`
		StateFile string `toml:"state_file"`
		Verbose   bool   `toml:"verbose"`
	} `toml:"server"`

	MQTT struct {
		Broker   string `toml:"broker"`
		ClientID string `toml:"client_id"`
	} `toml:"mqtt"`

	UPS struct {
		Name        string `toml:"name"`
		Description string `toml:"description"`
	} `toml:"ups"`

	Device struct {
		Manufacturer string `toml:"manufacturer"`
		Model        string `toml:"model"`
		Serial       string `toml:"serial"`
		BatteryType  string `toml:"battery_type"`
	} `toml:"device"`

	Power struct {
		InverterMaxVA     float64 `toml:"inverter_max_va"`
		BatteryCapacityWh float64 `toml:"battery_capacity_wh"`
	} `toml:"power"`

	Thresholds struct {
		BatteryChargeLow  float64 `toml:"battery_charge_low"`
		BatteryRuntimeLow float64 `toml:"battery_runtime_low"`
		LowBatterySoc     float64 `toml:"low_battery_soc"`
		LowBatteryRuntime float64 `toml:"low_battery_runtime"`
		GridLostVoltage   float64 `toml:"grid_lost_voltage"`
	} `toml:"thresholds"`

	// Users enables NUT authentication. With no users configured the server
	// accepts any client, preserving the historical open behavior.
	Users []UserConfig `toml:"users"`
}

// UserConfig is one NUT account allowed to LOGIN (and use SET/INSTCMD).
type UserConfig struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// defaultConfig returns the built-in defaults, matching the values this
// project historically had as compile-time constants.
func defaultConfig() *Config {
	c := &Config{}
	c.Server.Listen = "0.0.0.0:3493"
	c.Server.StateFile = "/data/cerbo-nut/calibration.json"
	c.MQTT.Broker = "tcp://127.0.0.1:1883"
	c.MQTT.ClientID = "nut-server-go"
	c.UPS.Name = "ups"
	c.UPS.Description = "Victron System"
	c.Device.Manufacturer = "Victron Energy"
	c.Device.Model = "MultiPlus 48/1600 (Cerbo GX)"
	c.Device.Serial = "CerboGX-MK2"
	c.Device.BatteryType = "Li-Ion"
	c.Power.InverterMaxVA = 1600.0
	c.Power.BatteryCapacityWh = 5120.0
	c.Thresholds.BatteryChargeLow = 20
	c.Thresholds.BatteryRuntimeLow = 600
	c.Thresholds.LowBatterySoc = 20.0
	c.Thresholds.LowBatteryRuntime = 300.0
	c.Thresholds.GridLostVoltage = 180.0
	return c
}

// loadConfig resolves the config file path (flag > env > default), decodes it,
// applies environment overrides, and validates the result. A missing file is
// only an error when its path was requested explicitly.
func loadConfig(flagPath string) (*Config, error) {
	c := defaultConfig()

	path, explicit := flagPath, flagPath != ""
	if !explicit {
		if envPath, ok := os.LookupEnv("CERBO_NUT_CONFIG"); ok && envPath != "" {
			path, explicit = envPath, true
		} else {
			path = DefaultConfigPath
		}
	}

	if _, err := os.Stat(path); err != nil {
		if explicit {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
	} else {
		meta, err := toml.DecodeFile(path, c)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if undecoded := meta.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, len(undecoded))
			for i, k := range undecoded {
				keys[i] = k.String()
			}
			return nil, fmt.Errorf("unknown keys in %s: %s", path, strings.Join(keys, ", "))
		}
	}

	if err := c.applyEnv(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// applyEnv overrides config values from CERBO_NUT_* environment variables.
func (c *Config) applyEnv() error {
	var errs []string

	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	num := func(key string, dst *float64) {
		if v, ok := os.LookupEnv(key); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %q is not a number", key, v))
				return
			}
			*dst = f
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(key); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %q is not a boolean", key, v))
				return
			}
			*dst = b
		}
	}

	str("CERBO_NUT_LISTEN", &c.Server.Listen)
	str("CERBO_NUT_STATE_FILE", &c.Server.StateFile)
	boolean("CERBO_NUT_VERBOSE", &c.Server.Verbose)
	str("CERBO_NUT_MQTT_BROKER", &c.MQTT.Broker)
	str("CERBO_NUT_MQTT_CLIENT_ID", &c.MQTT.ClientID)
	str("CERBO_NUT_UPS_NAME", &c.UPS.Name)
	str("CERBO_NUT_UPS_DESCRIPTION", &c.UPS.Description)
	str("CERBO_NUT_DEVICE_MANUFACTURER", &c.Device.Manufacturer)
	str("CERBO_NUT_DEVICE_MODEL", &c.Device.Model)
	str("CERBO_NUT_DEVICE_SERIAL", &c.Device.Serial)
	str("CERBO_NUT_BATTERY_TYPE", &c.Device.BatteryType)
	num("CERBO_NUT_INVERTER_MAX_VA", &c.Power.InverterMaxVA)
	num("CERBO_NUT_BATTERY_CAPACITY_WH", &c.Power.BatteryCapacityWh)
	num("CERBO_NUT_BATTERY_CHARGE_LOW", &c.Thresholds.BatteryChargeLow)
	num("CERBO_NUT_BATTERY_RUNTIME_LOW", &c.Thresholds.BatteryRuntimeLow)
	num("CERBO_NUT_LOW_BATTERY_SOC", &c.Thresholds.LowBatterySoc)
	num("CERBO_NUT_LOW_BATTERY_RUNTIME", &c.Thresholds.LowBatteryRuntime)
	num("CERBO_NUT_GRID_LOST_VOLTAGE", &c.Thresholds.GridLostVoltage)

	// CERBO_NUT_USERNAME/PASSWORD add one account (or override the account
	// with the same username), handy for container-style deployments.
	envUser, hasUser := os.LookupEnv("CERBO_NUT_USERNAME")
	envPass, hasPass := os.LookupEnv("CERBO_NUT_PASSWORD")
	if hasUser != hasPass {
		errs = append(errs, "CERBO_NUT_USERNAME and CERBO_NUT_PASSWORD must be set together")
	} else if hasUser {
		replaced := false
		for i := range c.Users {
			if c.Users[i].Username == envUser {
				c.Users[i].Password = envPass
				replaced = true
				break
			}
		}
		if !replaced {
			c.Users = append(c.Users, UserConfig{Username: envUser, Password: envPass})
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid environment overrides: %s", strings.Join(errs, "; "))
	}
	return nil
}

// validate rejects configurations that cannot work.
func (c *Config) validate() error {
	var errs []string

	if c.Server.Listen == "" {
		errs = append(errs, "server.listen must not be empty")
	}
	if c.MQTT.Broker == "" {
		errs = append(errs, "mqtt.broker must not be empty")
	}
	if c.UPS.Name == "" || strings.ContainsAny(c.UPS.Name, " \t\"") {
		errs = append(errs, "ups.name must be non-empty and contain no spaces or quotes")
	}
	if c.Power.InverterMaxVA <= 0 {
		errs = append(errs, "power.inverter_max_va must be > 0")
	}
	if c.Power.BatteryCapacityWh <= 0 {
		errs = append(errs, "power.battery_capacity_wh must be > 0")
	}
	if c.Thresholds.GridLostVoltage <= 0 {
		errs = append(errs, "thresholds.grid_lost_voltage must be > 0")
	}

	seen := map[string]bool{}
	for i, u := range c.Users {
		switch {
		case u.Username == "" || strings.ContainsAny(u.Username, " \t\""):
			errs = append(errs, fmt.Sprintf("users[%d].username must be non-empty and contain no spaces or quotes", i))
		case seen[u.Username]:
			errs = append(errs, fmt.Sprintf("users[%d]: duplicate username %q", i, u.Username))
		default:
			seen[u.Username] = true
		}
		if u.Password == "" {
			errs = append(errs, fmt.Sprintf("users[%d].password must not be empty", i))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return nil
}
