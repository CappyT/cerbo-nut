# Victron-NUT Server for Cerbo GX

This project provides a lightweight bridge between a Victron Energy system (via Venus OS / Cerbo GX) and the Network UPS Tools (NUT) protocol. It allows the Victron system to appear as a standard UPS to NUT clients, such as Synology NAS, Home Assistant, or other servers that need to monitor power status and perform graceful shutdowns.

## Features
- **MQTT Integration**: Connects to the local Victron MQTT broker to fetch real-time data.
- **NUT Protocol Support**: Implements the NUT network protocol (TCP port 3493).
- **Automatic Discovery**: Dynamically identifies the Victron Portal ID for keepalive notifications.
- **Real-time Metrics**: Provides battery SoC, voltage, current, runtime, input/output voltage, and system status (OL, OB, CHRG, DISCHRG, LB).
- **Low Overhead**: Written in Go, designed to run directly on the Cerbo GX (ARM architecture).

## Scope
The server listens for MQTT messages from Venus OS and translates them into NUT variables. It emulates a `upsd` server, allowing any NUT client to query the state of the Victron system as if it were a physical UPS.

## Configuration

Everything is configured through a TOML file, with optional environment variable overrides. Resolution order is: **built-in defaults < config file < `CERBO_NUT_*` environment variables**. All values are optional: with no config file at all, the server runs entirely on built-in defaults.

- Default config path: `/data/cerbo-nut/config.toml` (missing file = defaults apply)
- Override the path with `--config /path/to/config.toml` or `CERBO_NUT_CONFIG=/path/to/config.toml`
- See [`config.example.toml`](config.example.toml) for the full annotated reference
- Unknown keys in the config file are rejected at startup, so typos fail loudly instead of being silently ignored

| Environment variable | Config key |
|---|---|
| `CERBO_NUT_LISTEN` | `server.listen` |
| `CERBO_NUT_STATE_FILE` | `server.state_file` |
| `CERBO_NUT_VERBOSE` | `server.verbose` |
| `CERBO_NUT_MQTT_BROKER` | `mqtt.broker` |
| `CERBO_NUT_MQTT_CLIENT_ID` | `mqtt.client_id` |
| `CERBO_NUT_UPS_NAME` | `ups.name` |
| `CERBO_NUT_UPS_DESCRIPTION` | `ups.description` |
| `CERBO_NUT_DEVICE_MANUFACTURER` | `device.manufacturer` |
| `CERBO_NUT_DEVICE_MODEL` | `device.model` |
| `CERBO_NUT_DEVICE_SERIAL` | `device.serial` |
| `CERBO_NUT_BATTERY_TYPE` | `device.battery_type` |
| `CERBO_NUT_INVERTER_MAX_VA` | `power.inverter_max_va` |
| `CERBO_NUT_BATTERY_CAPACITY_WH` | `power.battery_capacity_wh` |
| `CERBO_NUT_BATTERY_CHARGE_LOW` | `thresholds.battery_charge_low` |
| `CERBO_NUT_BATTERY_RUNTIME_LOW` | `thresholds.battery_runtime_low` |
| `CERBO_NUT_LOW_BATTERY_SOC` | `thresholds.low_battery_soc` |
| `CERBO_NUT_LOW_BATTERY_RUNTIME` | `thresholds.low_battery_runtime` |
| `CERBO_NUT_GRID_LOST_VOLTAGE` | `thresholds.grid_lost_voltage` |
| `CERBO_NUT_USERNAME` + `CERBO_NUT_PASSWORD` | adds (or overrides) one `[[users]]` account |
| `CERBO_NUT_ALLOWED_NETWORKS` | comma-separated `allowed_networks` for the env-defined account |

## Authentication

NUT authentication is enabled by adding one or more `[[users]]` blocks to the config (or setting `CERBO_NUT_USERNAME`/`CERBO_NUT_PASSWORD`):

```toml
[[users]]
username = "synology"
password = "changeme"
allowed_networks = ["192.168.1.0/24"]  # optional per-user ACL

[[users]]
username = "homeassistant"
password = "changeme-too"              # no ACL: valid from anywhere
```

Behavior follows the NUT protocol (RFC 9271), matching what a real `upsd` does:

- Read-only queries (`LIST`, `GET`) never require authentication.
- `LOGIN`, `SET`, and `INSTCMD` require a valid `USERNAME` + `PASSWORD`, with the proper protocol errors (`USERNAME-REQUIRED`, `PASSWORD-REQUIRED`, `ACCESS-DENIED`, `ALREADY-SET-USERNAME`, `ALREADY-LOGGED-IN`, `UNKNOWN-UPS`, ...).
- Quoted arguments are supported, so passwords may contain spaces (`PASSWORD "my secret"`).
- Password checks run in constant time.
- `GET NUMLOGINS` and `LIST CLIENT` report the clients actually logged in.
- **Per-user network ACL**: `allowed_networks` limits an account to a list of CIDR ranges or bare IPs (IPv4 and IPv6). Authenticating from outside the list fails with the same `ACCESS-DENIED` as a wrong password, so probing reveals nothing. Accounts without the key work from anywhere.

Authentication is opt-in: with no users configured the server runs in open mode (any client accepted), which is a perfectly fine choice on a trusted LAN. The startup log states which mode is active.

## Battery Runtime Prediction

The `battery.runtime` value is computed differently depending on the power source:

- **On battery**: Victron's own `TimeToGo` (computed by the BMS from the real discharge) is used directly. On grid this value is stale or absent, so it is ignored there.
- **On grid**: the runtime is estimated from the AC load through a **self-learned linear model** `DC watts = a * AC watts + b` (conversion losses plus inverter idle overhead), smoothed with a time-based exponential moving average so short appliance spikes don't make the prediction bounce.
- **Self-learning, zero tunables**: while the system actually discharges, every sample of measured DC battery power trains the model (decayed least-squares fit, regularized toward a sane prior so it stays well-behaved even with a perfectly flat load). Inverter efficiency and idle draw are learned from your own hardware — there is nothing to configure or tune. The average battery voltage used to convert the BMS-discovered Ah capacity into Wh is also measured, so 12/24/48V systems all work unmodified.
- **SoC reserve**: if the ESS minimum SoC limit is published on MQTT (`Settings/CGwacs/BatteryLife/MinimumSocLimit`) it is used as the unusable reserve; otherwise a conservative 10% default applies.
- **Persistence**: the learned model is saved to `/data/cerbo-nut/calibration.json` (configurable via `server.state_file` / `CERBO_NUT_STATE_FILE`, empty string disables it) so it survives restarts and firmware updates. The write policy is deliberately flash-friendly: learning only happens while discharging, so the disk is written exactly once per discharge event (right after it ends) plus once on graceful shutdown — normal grid operation never writes to the NAND at all. Writes are atomic (temp file + rename).

### Limitations

- **Single-phase only**: AC load and grid-loss detection read the L1 phase exclusively. On a three-phase system the model would learn the DC draw against a third of the real load (over-reporting the runtime), and a grid failure on L2 or L3 would not raise `OB`.
- **Sized for small inverters**: the learned idle overhead is clamped to 100 W, which covers single units up to roughly 8 kVA. Larger or multi-unit setups idle above that clamp, so on-grid predictions stay accurate near the learned operating point but skew when extrapolating far from it.
- Battery voltage is **not** a limitation: 12/24/48 V systems all work unmodified, since the average discharge voltage is measured rather than assumed.

## Releases

Prebuilt binaries are published on the [GitHub Releases page](../../releases) for `linux-armv7` (Cerbo GX), `linux-arm64`, and `linux-amd64`, along with SHA-256 checksums. If you just want to run the server, download the `armv7` binary from the latest release and skip the compilation section.

Releases follow [semantic versioning](https://semver.org/) and are cut by pushing a tag:

```bash
git tag v1.0.0
git push origin v1.0.0
```

The `release` workflow then cross-compiles the binaries (stamping the version into the build), generates release notes, and attaches everything to the GitHub release. Every push and pull request is also built and vetted by the `ci` workflow.

## Compilation

Since the Cerbo GX uses an ARM architecture (usually ARMv7), you need to cross-compile the binary if you are developing on a different architecture (like x86_64).

### Local Compilation (Your machine)
To compile for your local machine:
```bash
go build -o cerbo-nut .
```

### Cross-Compilation for Cerbo GX (ARM)
To compile for the Cerbo GX (optimized for size and portability):
```bash
env GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o cerbo-nut .
```
*Note: Using `CGO_ENABLED=0` ensures a statically-linked binary, and `-ldflags="-s -w"` reduces the binary size by removing debug information.*

## Installation on Cerbo GX

1. **Transfer the binary**:
   Copy the compiled `cerbo-nut` binary to your Cerbo GX using `scp`:
   ```bash
   scp cerbo-nut root@<cerbo-ip>:/data/
   ```

2. **Set up the service directory**:
   SSH into the Cerbo GX and move the binary to a dedicated folder:
   ```bash
   ssh root@<cerbo-ip>
   mkdir -p /data/cerbo-nut
   mv /data/cerbo-nut-bin /data/cerbo-nut/cerbo-nut # If you named it differently
   # OR just:
   mv /data/cerbo-nut /data/cerbo-nut/cerbo-nut
   chmod +x /data/cerbo-nut/cerbo-nut
   ```

3. **Add the configuration (optional)**:
   Copy [`config.example.toml`](config.example.toml) to `/data/cerbo-nut/config.toml` and adjust it to your hardware. Without it, the built-in defaults apply.

4. **Enable MQTT on Cerbo GX**:
   Ensure that "MQTT on LAN (SSL)" or "MQTT on LAN (Plain)" is enabled in the Venus OS settings under **Settings -> Services**. This tool connects to the local broker at `127.0.0.1:1883`.

## Running as a Service (Persistence)

Venus OS uses `daemontools` to manage services. To run `cerbo-nut` as a persistent service that starts automatically on boot, follow these steps:

1. **Create the `run` script**:
   ```bash
   cat << 'EOF' > /data/cerbo-nut/run
   #!/bin/sh
   exec /data/cerbo-nut/cerbo-nut 2>&1
   EOF
   chmod +x /data/cerbo-nut/run
   ```

2. **Configure persistence across reboots**:
   Add the service symlink to `/data/rc.local` so it is recreated after firmware updates:
   ```bash
   if [ ! -f /data/rc.local ]; then 
       echo "#!/bin/sh" > /data/rc.local
       chmod +x /data/rc.local
   fi
   grep -q "cerbo-nut" /data/rc.local || echo "ln -s /data/cerbo-nut /service/cerbo-nut" >> /data/rc.local
   ```

3. **Start the service now**:
   ```bash
   ln -s /data/cerbo-nut /service/cerbo-nut
   ```

4. **Verify the status**:
   ```bash
   sv stat /service/cerbo-nut
   ```

### Service Management

Use the following commands to manage the service once it is installed:

- **Check status**: `sv stat /service/cerbo-nut`
- **Start service**: `svc -u /service/cerbo-nut`
- **Stop service**: `svc -d /service/cerbo-nut`
- **Restart service**: `svc -t /service/cerbo-nut`
- **View logs**: `tail -f /data/cerbo-nut/log/current` (If logging is configured) or check the standard output of the process.

## Integration with NUT Clients

Configure your NUT client (e.g., Synology or another Linux box) to point to the Cerbo GX IP address on port 3493.

**Example `upsmon.conf` entry:**
```text
MONITOR ups@<cerbo-ip> 1 upsuser changeme slave
```
The credentials must match a `[[users]]` account from the config file (see [Authentication](#authentication)). If no users are configured, any username/password is accepted.
