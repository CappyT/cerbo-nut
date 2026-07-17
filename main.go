package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// --- CONFIGURATION BLOCK ---
const (
	// UPS Metadata
	UpsName            = "ups"                        // Standard name for compatibility (Synology/HA)
	UpsDescription     = "Victron System"             // Description shown in NUT clients
	DeviceManufacturer = "Victron Energy"
	DeviceModel        = "MultiPlus 48/1600 (Cerbo GX)"
	DeviceSerial       = "CerboGX-MK2"

	// Power & Battery Specs
	InverterMaxVA     = 1600.0 // Max VA for calculating ups.load percentage
	BatteryCapacityWh = 5120.0 // Default Wh if capacity cannot be discovered via MQTT
	BatteryNominalV   = 51.2   // Nominal battery voltage, used to convert discovered Ah to Wh
	BatteryType       = "Li-Ion"

	// Runtime Prediction Model
	InverterEfficiency = 0.92  // AC->DC conversion efficiency of the inverter
	InverterIdleWatts  = 12.0  // Constant DC overhead of the inverter while inverting (W)
	SocReservePercent  = 10.0  // SoC kept as unusable reserve when estimating runtime (%)
	MinCalcWatts       = 50.0  // Floor for the load used in the estimate, avoids absurd runtimes
	RuntimeEmaTau      = 300.0 // Time constant (s) of the slow EMA feeding the runtime estimate
	CalibEmaTau        = 600.0 // Time constant (s) of the learned AC->DC calibration factor
	CalibMin           = 0.6   // Clamp for the learned calibration factor
	CalibMax           = 1.8   // Clamp for the learned calibration factor

	// Thresholds & Status
	BatteryChargeLow     = "20"  // NUT variable (string) for low battery warning
	BatteryRuntimeLow    = "600" // NUT variable (string) for low runtime warning (seconds)
	LowBatterySocLimit   = 20.0  // Threshold for "LB" (Low Battery) status (%)
	LowBatteryRunLimit   = 300.0 // Threshold for "LB" status (seconds)
	GridLostVoltageLimit = 180.0 // Voltage below which we consider the grid lost (OB status)
)

// ---------------------------

// Global variable for verbose mode
var verboseMode bool

// VictronData contains real-time state read from MQTT
type VictronData struct {
	sync.RWMutex
	GridLost          bool
	BatterySoc        float64
	BatteryVolts      float64
	BatteryAmps       float64
	BatteryTimeToGo   float64
	BatteryCapacityAh float64 // Discovered dynamically from the BMS
	AcInVolts         float64
	AcInAmps          float64 // Input current
	AcOutVolts        float64 // Output voltage
	AcOutFreq         float64 // Output frequency
	AcOutAmps         float64 // Output current
	AcOutWatts        float64
	AcOutVA           float64   // Apparent power (VA)
	RuntimeWatts      float64   // Slow EMA of the DC-equivalent draw, feeds the runtime estimate
	LoadCalib         float64   // Learned measured/modeled DC power ratio, updated while discharging
	LastPowerSample   time.Time // Timestamp of the last power sample, for time-based EMA
	PortalID          string    // Extracted dynamically from the first MQTT message
}

var state = &VictronData{
	AcInVolts:  230.0, // Initialize to 230V to avoid false 'OB' status at app startup
	AcOutVolts: 230.0, // Initialize to 230V by default
	AcOutFreq:  50.0,  // Initialize to 50Hz by default
	LoadCalib:  1.0,   // Start with a neutral calibration until a real discharge is observed
}

// VictronPayload is the standard JSON structure used by Venus OS on MQTT
type VictronPayload struct {
	Value interface{} `json:"value"`
}

// debugLog prints logs only if the --verbose flag is active
func debugLog(format string, v ...interface{}) {
	if verboseMode {
		log.Printf(format, v...)
	}
}

func main() {
	// Command line parsing for --verbose flag
	flag.BoolVar(&verboseMode, "verbose", false, "Enable detailed logs (debug)")
	flag.Parse()

	log.Println("Starting Victron-NUT Server for Cerbo GX...")
	if verboseMode {
		log.Println("Verbose mode ACTIVE: connection and debug logs enabled.")
	}

	// 1. MQTT Client Configuration (points to localhost if running on Cerbo)
	opts := mqtt.NewClientOptions()
	opts.AddBroker("tcp://127.0.0.1:1883")
	opts.SetClientID("nut-server-go")
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(true)
	opts.SetDefaultPublishHandler(messageHandler)

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("MQTT connection error: %v", token.Error()) // Fatal stays to exit on crash
	}
	log.Println("Connected to local MQTT broker.")

	// Subscription to 'N/#' topics (Generic notifications from all portals)
	if token := client.Subscribe("N/#", 0, nil); token.Wait() && token.Error() != nil {
		log.Fatalf("Subscription error: %v", token.Error())
	}

	// 2. Keepalive Loop for Venus OS MQTT
	go func() {
		for {
			state.RLock()
			portalID := state.PortalID
			state.RUnlock()

			if portalID != "" {
				topic := fmt.Sprintf("R/%s/keepalive", portalID)
				client.Publish(topic, 0, false, "")
			}
			time.Sleep(30 * time.Second)
		}
	}()

	// 3. Start NUT TCP Server
	listener, err := net.Listen("tcp", "0.0.0.0:3493")
	if err != nil {
		log.Fatalf("Error starting NUT TCP server: %v", err)
	}
	defer listener.Close()
	log.Println("NUT Server listening on port 3493...")

	// Graceful shutdown management
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Closing server...")
		client.Disconnect(250)
		os.Exit(0)
	}()

	// Accept client connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Error accepting connection: %v", err) // Real errors are always printed
			continue
		}
		go handleNUTConnection(conn)
	}
}

// messageHandler processes incoming MQTT messages and updates the in-memory state
func messageHandler(client mqtt.Client, msg mqtt.Message) {
	topic := msg.Topic()
	parts := strings.Split(topic, "/")

	if len(parts) < 5 {
		return
	}

	// Save the Portal ID for keepalive
	state.Lock()
	if state.PortalID == "" {
		state.PortalID = parts[1]
		debugLog("Portal ID intercepted: %s", state.PortalID)
	}
	state.Unlock()

	var payload VictronPayload
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		return
	}

	// Safe extraction of numerical value
	var valFloat float64
	if payload.Value != nil {
		vf, ok := getFloat(payload.Value)
		if !ok {
			return
		}
		valFloat = vf
	} else {
		valFloat = 0
	}

	state.Lock()
	defer state.Unlock()

	// Topic Mapping
	if strings.HasSuffix(topic, "/Dc/Battery/Soc") {
		state.BatterySoc = valFloat
	} else if strings.HasSuffix(topic, "/Dc/Battery/Voltage") {
		state.BatteryVolts = valFloat
	} else if strings.HasSuffix(topic, "/Dc/Battery/Current") {
		state.BatteryAmps = valFloat
	} else if strings.HasSuffix(topic, "/Dc/Battery/TimeToGo") {
		state.BatteryTimeToGo = valFloat
	} else if (strings.Contains(topic, "/battery/") || strings.Contains(topic, "/Dc/Battery/")) && strings.HasSuffix(topic, "/Capacity") {
		state.BatteryCapacityAh = valFloat
	} else if strings.Contains(topic, "/Ac/Consumption/L1/Power") || strings.Contains(topic, "/Ac/Out/L1/P") {
		state.AcOutWatts = valFloat
		updatePowerEstimateLocked(time.Now())
	} else if strings.Contains(topic, "/Ac/Consumption/L1/Current") || strings.Contains(topic, "/Ac/Out/L1/I") {
		state.AcOutAmps = valFloat
	} else if strings.HasSuffix(topic, "/Ac/Out/L1/S") {
		state.AcOutVA = valFloat
	} else if strings.HasSuffix(topic, "/Ac/Out/L1/V") {
		state.AcOutVolts = valFloat
	} else if strings.HasSuffix(topic, "/Ac/Out/L1/F") {
		state.AcOutFreq = valFloat
	} else if strings.Contains(topic, "/Ac/Grid/L1/Voltage") || strings.Contains(topic, "/Ac/ActiveIn/L1/V") {
		state.AcInVolts = valFloat
	} else if strings.Contains(topic, "/Ac/Grid/L1/Current") || strings.Contains(topic, "/Ac/ActiveIn/L1/Current") {
		state.AcInAmps = valFloat
	} else if strings.Contains(topic, "/Alarms/GridLost") {
		state.GridLost = valFloat > 0
	}
}

// updatePowerEstimateLocked refreshes the DC-equivalent power estimate used for
// the runtime prediction. Must be called with state.Lock held.
//
// The estimate models the real battery draw (inverter idle overhead + AC load
// divided by conversion efficiency) and is corrected by a calibration factor
// learned by comparing the model against the measured DC power whenever the
// system actually discharges. Smoothing uses a time-based EMA so the result is
// independent of how often Venus OS publishes power updates.
func updatePowerEstimateLocked(now time.Time) {
	dt := 1.0
	if !state.LastPowerSample.IsZero() {
		dt = now.Sub(state.LastPowerSample).Seconds()
		if dt <= 0 {
			dt = 0.1
		} else if dt > 60 {
			dt = 60
		}
	}
	state.LastPowerSample = now

	// DC power the inverter would need from the battery to serve the current AC load
	modelWatts := InverterIdleWatts + state.AcOutWatts/InverterEfficiency

	sample := modelWatts * state.LoadCalib
	if state.BatteryAmps < -0.5 {
		measured := math.Abs(state.BatteryAmps * state.BatteryVolts)
		if measured > 0 {
			// While discharging the battery meter is the ground truth
			sample = measured
			// Learn how far the model is from reality, so estimates made while
			// on grid match what actually happens on battery. Skip tiny loads
			// where the ratio is dominated by measurement noise.
			if modelWatts > 25 {
				ratio := measured / modelWatts
				if ratio < CalibMin {
					ratio = CalibMin
				} else if ratio > CalibMax {
					ratio = CalibMax
				}
				state.LoadCalib = emaStep(state.LoadCalib, ratio, dt, CalibEmaTau)
			}
		}
	}

	if state.RuntimeWatts == 0 {
		state.RuntimeWatts = sample
	} else {
		state.RuntimeWatts = emaStep(state.RuntimeWatts, sample, dt, RuntimeEmaTau)
	}
}

// emaStep advances an exponential moving average by dt seconds toward sample,
// using a time constant tau. Equivalent alpha = 1 - e^(-dt/tau).
func emaStep(prev, sample, dt, tau float64) float64 {
	alpha := 1.0 - math.Exp(-dt/tau)
	return prev + alpha*(sample-prev)
}

func getFloat(unk interface{}) (float64, bool) {
	switch v := unk.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}

func generateNUTVars() map[string]string {
	state.RLock()
	defer state.RUnlock()

	vars := make(map[string]string)

	vars["driver.name"] = "dummy-victron"
	vars["driver.version"] = "1.0"
	vars["driver.version.internal"] = "1.0"
	vars["driver.parameter.port"] = "mqtt"

	vars["device.mfr"] = DeviceManufacturer
	vars["device.model"] = DeviceModel
	vars["device.type"] = "ups"
	vars["device.serial"] = DeviceSerial
	vars["ups.mfr"] = DeviceManufacturer
	vars["ups.model"] = DeviceModel
	vars["ups.serial"] = DeviceSerial

	vars["battery.type"] = BatteryType
	vars["battery.charge"] = fmt.Sprintf("%.1f", state.BatterySoc)
	vars["battery.voltage"] = fmt.Sprintf("%.2f", state.BatteryVolts)
	vars["battery.current"] = fmt.Sprintf("%.2f", state.BatteryAmps)
	vars["battery.charge.low"] = BatteryChargeLow
	vars["battery.runtime.low"] = BatteryRuntimeLow

	// Victron's TimeToGo is only meaningful while actually discharging; on grid
	// it is stale or absent, so there we rely on our own load-based estimate.
	onBattery := state.GridLost || state.AcInVolts < GridLostVoltageLimit
	var runtimeSeconds float64
	if onBattery && state.BatteryTimeToGo > 0 {
		runtimeSeconds = state.BatteryTimeToGo
	} else {
		calcWatts := state.RuntimeWatts
		if calcWatts < MinCalcWatts {
			calcWatts = MinCalcWatts
		}

		capacityWh := BatteryCapacityWh
		if state.BatteryCapacityAh > 0 {
			capacityWh = state.BatteryCapacityAh * BatteryNominalV
		}

		usableSoc := state.BatterySoc - SocReservePercent
		if usableSoc < 0 {
			usableSoc = 0
		}

		remainingWh := capacityWh * (usableSoc / 100.0)
		runtimeSeconds = (remainingWh / calcWatts) * 3600.0
	}
	// Report in whole minutes: sub-minute jitter carries no information for NUT
	// clients and only adds noise to history graphs.
	runtimeSeconds = math.Round(runtimeSeconds/60.0) * 60.0
	vars["battery.runtime"] = fmt.Sprintf("%.0f", runtimeSeconds)

	vars["ups.status"] = generateStatus(runtimeSeconds)

	vars["input.voltage"] = fmt.Sprintf("%.1f", state.AcInVolts)
	vars["input.current"] = fmt.Sprintf("%.2f", state.AcInAmps)

	vars["output.voltage"] = fmt.Sprintf("%.1f", state.AcOutVolts)
	vars["output.frequency"] = fmt.Sprintf("%.2f", state.AcOutFreq)
	vars["output.current"] = fmt.Sprintf("%.2f", state.AcOutAmps)

	loadPercent := (state.AcOutWatts / InverterMaxVA) * 100
	if loadPercent > 100 {
		loadPercent = 100
	}
	vars["ups.load"] = fmt.Sprintf("%.1f", loadPercent)
	vars["ups.realpower"] = fmt.Sprintf("%.0f", state.AcOutWatts)
	vars["ups.power"] = fmt.Sprintf("%.0f", state.AcOutVA)

	return vars
}

func generateStatus(runtimeSeconds float64) string {
	status := "OL"

	if state.GridLost || state.AcInVolts < GridLostVoltageLimit {
		status = "OB"
	}

	if state.BatteryAmps > 1.0 {
		status = "OL CHRG"
	} else if state.BatteryAmps < -1.0 {
		status += " DISCHRG"
	}

	if strings.Contains(status, "OB") {
		if state.BatterySoc <= LowBatterySocLimit || runtimeSeconds <= LowBatteryRunLimit {
			status += " LB"
		}
	}

	return status
}

func handleNUTConnection(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	writer := bufio.NewWriter(conn)

	remoteAddr := conn.RemoteAddr().String()
	debugLog("[NUT] New connection from %s", remoteAddr)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		debugLog("[NUT Client %s] -> %s", remoteAddr, line)

		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		command := strings.ToUpper(parts[0])

		reqUpsName := UpsName
		if len(parts) > 2 {
			reqUpsName = parts[2]
		}

		isMyUps := true

		switch command {
		case "HELP":
			resp := "BEGIN HELP\nHELP\nVER\nNETVER\nLIST\nGET\nSET\nLOGIN\nLOGOUT\nUSERNAME\nPASSWORD\nSTARTTLS\nEND HELP\n"
			debugLog("[NUT Server] <- BEGIN/END HELP")
			writer.WriteString(resp)

		case "VER":
			resp := "Network UPS Tools upsd 2.8.0\n"
			debugLog("[NUT Server] <- %q", resp)
			writer.WriteString(resp)

		case "NETVER":
			resp := "1.2\n"
			debugLog("[NUT Server] <- %q", resp)
			writer.WriteString(resp)

		case "STARTTLS":
			resp := "ERR FEATURE-NOT-SUPPORTED\n"
			debugLog("[NUT Server] <- %q", resp)
			writer.WriteString(resp)

		case "TRACKING":
			writer.WriteString("OK\n")

		case "LIST":
			if len(parts) >= 2 && strings.ToUpper(parts[1]) == "UPS" {
				resp := fmt.Sprintf("BEGIN LIST UPS\nUPS %s \"%s\"\nEND LIST UPS\n", UpsName, UpsDescription)
				debugLog("[NUT Server] <- %q", resp)
				writer.WriteString(resp)
			} else if len(parts) >= 3 && strings.ToUpper(parts[1]) == "VAR" && isMyUps {
				writer.WriteString(fmt.Sprintf("BEGIN LIST VAR %s\n", reqUpsName))
				for k, v := range generateNUTVars() {
					writer.WriteString(fmt.Sprintf("VAR %s %s \"%s\"\n", reqUpsName, k, v))
				}
				writer.WriteString(fmt.Sprintf("END LIST VAR %s\n", reqUpsName))
				debugLog("[NUT Server] <- Full VAR list sent")
			} else if len(parts) >= 3 && strings.ToUpper(parts[1]) == "RW" && isMyUps {
				resp := fmt.Sprintf("BEGIN LIST RW %s\nEND LIST RW %s\n", reqUpsName, reqUpsName)
				debugLog("[NUT Server] <- %q", resp)
				writer.WriteString(resp)
			} else if len(parts) >= 3 && strings.ToUpper(parts[1]) == "CMD" && isMyUps {
				resp := fmt.Sprintf("BEGIN LIST CMD %s\nEND LIST CMD %s\n", reqUpsName, reqUpsName)
				debugLog("[NUT Server] <- %q", resp)
				writer.WriteString(resp)
			} else if len(parts) >= 3 && strings.ToUpper(parts[1]) == "CLIENT" && isMyUps {
				resp := fmt.Sprintf("BEGIN LIST CLIENT %s\nCLIENT %s %s\nEND LIST CLIENT %s\n", reqUpsName, reqUpsName, remoteAddr, reqUpsName)
				debugLog("[NUT Server] <- CLIENT list sent")
				writer.WriteString(resp)
			} else {
				debugLog("[NUT Server] <- ERR INVALID-ARGUMENT")
				writer.WriteString("ERR INVALID-ARGUMENT\n")
			}

		case "GET":
			if len(parts) >= 2 {
				subCmd := strings.ToUpper(parts[1])
				if subCmd == "VAR" && len(parts) == 4 && isMyUps {
					vars := generateNUTVars()
					if val, ok := vars[parts[3]]; ok {
						resp := fmt.Sprintf("VAR %s %s \"%s\"\n", reqUpsName, parts[3], val)
						debugLog("[NUT Server] <- %q", resp)
						writer.WriteString(resp)
					} else {
						debugLog("[NUT Server] <- ERR VAR-NOT-SUPPORTED (%s)", parts[3])
						writer.WriteString("ERR VAR-NOT-SUPPORTED\n")
					}
				} else if subCmd == "UPSDESC" && len(parts) == 3 && isMyUps {
					resp := fmt.Sprintf("UPSDESC %s \"%s\"\n", reqUpsName, UpsDescription)
					debugLog("[NUT Server] <- %q", resp)
					writer.WriteString(resp)
				} else if subCmd == "DESC" && len(parts) == 4 && isMyUps {
					resp := fmt.Sprintf("DESC %s %s \"Generic Description\"\n", reqUpsName, parts[3])
					debugLog("[NUT Server] <- %q", resp)
					writer.WriteString(resp)
				} else if subCmd == "TYPE" && len(parts) == 4 && isMyUps {
					resp := fmt.Sprintf("TYPE %s %s STRING\n", reqUpsName, parts[3])
					debugLog("[NUT Server] <- %q", resp)
					writer.WriteString(resp)
				} else if subCmd == "CMDDESC" && len(parts) == 4 && isMyUps {
					resp := fmt.Sprintf("CMDDESC %s %s \"Command description\"\n", reqUpsName, parts[3])
					debugLog("[NUT Server] <- %q", resp)
					writer.WriteString(resp)
				} else if subCmd == "NUMLOGINS" && len(parts) == 3 && isMyUps {
					resp := fmt.Sprintf("NUMLOGINS %s 1\n", reqUpsName)
					debugLog("[NUT Server] <- %q", resp)
					writer.WriteString(resp)
				} else {
					debugLog("[NUT Server] <- ERR INVALID-ARGUMENT")
					writer.WriteString("ERR INVALID-ARGUMENT\n")
				}
			} else {
				debugLog("[NUT Server] <- ERR INVALID-ARGUMENT")
				writer.WriteString("ERR INVALID-ARGUMENT\n")
			}

		case "LOGOUT":
			debugLog("[NUT Server] <- OK Goodbye")
			writer.WriteString("OK Goodbye\n")
			writer.Flush()
			return

		case "USERNAME", "PASSWORD", "LOGIN", "SET":
			debugLog("[NUT Server] <- OK")
			writer.WriteString("OK\n")

		default:
			debugLog("[NUT Server] <- ERR UNKNOWN-COMMAND")
			writer.WriteString("ERR UNKNOWN-COMMAND\n")
		}

		writer.Flush()
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[NUT] Error reading from %s: %v", remoteAddr, err) // Net error log remains
	}
	debugLog("[NUT] Connection closed by %s", remoteAddr)
}
