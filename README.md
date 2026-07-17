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

## Battery Runtime Prediction

The `battery.runtime` value is computed differently depending on the power source:

- **On battery**: Victron's own `TimeToGo` (computed by the BMS from the real discharge) is used directly. On grid this value is stale or absent, so it is ignored there.
- **On grid**: the runtime is estimated from the AC load through a **self-learned linear model** `DC watts = a * AC watts + b` (conversion losses plus inverter idle overhead), smoothed with a time-based exponential moving average so short appliance spikes don't make the prediction bounce.
- **Self-learning, zero tunables**: while the system actually discharges, every sample of measured DC battery power trains the model (decayed least-squares fit, regularized toward a sane prior so it stays well-behaved even with a perfectly flat load). Inverter efficiency and idle draw are learned from your own hardware — there is nothing to configure or tune. The average battery voltage used to convert the BMS-discovered Ah capacity into Wh is also measured, so 12/24/48V systems all work unmodified.
- **SoC reserve**: if the ESS minimum SoC limit is published on MQTT (`Settings/CGwacs/BatteryLife/MinimumSocLimit`) it is used as the unusable reserve; otherwise a conservative 10% default applies.
- **Persistence**: the learned model is saved to `/data/cerbo-nut/calibration.json` (configurable via `--calib-file`, empty string disables it) so it survives restarts and firmware updates. The write policy is deliberately flash-friendly: learning only happens while discharging, so the disk is written exactly once per discharge event (right after it ends) plus once on graceful shutdown — normal grid operation never writes to the NAND at all. Writes are atomic (temp file + rename).

## Compilation

> [!IMPORTANT]
> Before compiling, open `main.go` and review the `CONFIGURATION BLOCK` at the top of the file. You should customize variables like `InverterMaxVA`, `BatteryCapacityWh`, and `DeviceModel` to match your specific hardware setup.

Since the Cerbo GX uses an ARM architecture (usually ARMv7), you need to cross-compile the binary if you are developing on a different architecture (like x86_64).

### Local Compilation (Your machine)
To compile for your local machine:
```bash
go build -o cerbo-nut main.go
```

### Cross-Compilation for Cerbo GX (ARM)
To compile for the Cerbo GX (optimized for size and portability):
```bash
env GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o cerbo-nut main.go
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

3. **Enable MQTT on Cerbo GX**:
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
MONITOR ups@<cerbo-ip> 1 upsuser upspass slave
```
*(Note: The server currently accepts any username/password for simplicity in internal networks).*
