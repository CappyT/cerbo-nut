package main

import (
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

// --- INTERNAL MODEL CONSTANTS ---
// The runtime prediction is self-calibrating: the AC->DC load model below is
// only a prior that real discharge data progressively overrides. These are
// algorithm constants, not tunables — there is nothing to configure here.
// Everything meant to be configured lives in the config file (see config.go).
const (
	seedSlope       = 1.0 / 0.92 // Prior DC/AC slope (inverter conversion efficiency)
	seedIdleWatts   = 12.0       // Prior inverter idle overhead (W)
	priorWeight     = 120.0      // Weight of each prior anchor, in seconds of real data
	learnForgetTau  = 6 * 3600.0 // Forgetting time constant of learned data (discharge seconds)
	runtimeEmaTau   = 300.0      // Time constant (s) of the EMA feeding the runtime estimate
	voltsEmaTau     = 6 * 3600.0 // Time constant (s) of the average battery voltage estimate
	defaultSocFloor = 10.0       // SoC reserve when ESS does not publish a minimum SoC limit (%)
	modelSlopeMin   = 0.95       // Sanity clamps for the learned model coefficients
	modelSlopeMax   = 1.8
	modelIdleMin    = 0.0
	modelIdleMax    = 100.0
)

// version is stamped at build time by the release workflow via
// -ldflags "-X main.version=vX.Y.Z"
var version = "dev"

// cfg holds the resolved runtime configuration (defaults < file < env)
var cfg *Config

// verboseMode enables debug logging (--verbose flag or config)
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
	AvgBatteryVolts   float64   // Slow EMA of measured battery voltage, converts discovered Ah to Wh
	SocFloorMin       float64   // ESS minimum SoC limit discovered via MQTT, <0 = not published
	LastPowerSample   time.Time // Timestamp of the last power sample, for time-based EMA
	PortalID          string    // Extracted dynamically from the first MQTT message

	// Learned AC->DC load model: DC watts = ModelA*AC + ModelB. The coefficients
	// come from a decayed least-squares fit of (AC, measured DC) pairs collected
	// while discharging, regularized toward a prior so the fit stays sane even
	// when the observed load never varies.
	ModelA, ModelB                     float64
	RegN, RegSx, RegSy, RegSxx, RegSxy float64
	ModelDirty                         bool // Learned data not yet persisted to disk
}

var state = &VictronData{
	AcInVolts:   230.0, // Initialize to 230V to avoid false 'OB' status at app startup
	AcOutVolts:  230.0, // Initialize to 230V by default
	AcOutFreq:   50.0,  // Initialize to 50Hz by default
	SocFloorMin: -1.0,  // Unknown until (and unless) ESS publishes it
	ModelA:      seedSlope,
	ModelB:      seedIdleWatts,
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
	// Command line parsing
	var verboseFlag bool
	var configPath string
	flag.BoolVar(&verboseFlag, "verbose", false, "Enable detailed logs (debug)")
	flag.StringVar(&configPath, "config", "", "Path to the TOML config file (default "+DefaultConfigPath+", or $CERBO_NUT_CONFIG)")
	flag.Parse()

	var err error
	cfg, err = loadConfig(configPath)
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}
	verboseMode = verboseFlag || cfg.Server.Verbose

	log.Printf("Starting Victron-NUT Server for Cerbo GX (%s)...", version)
	if verboseMode {
		log.Println("Verbose mode ACTIVE: connection and debug logs enabled.")
	}
	if len(cfg.Users) == 0 {
		log.Println("NUT authentication disabled (no users configured); add [[users]] to the config to enable it.")
	} else {
		log.Printf("NUT authentication enabled (%d user(s) configured).", len(cfg.Users))
	}

	// Restore the load model learned in previous runs
	if cfg.Server.StateFile != "" {
		loadModel(cfg.Server.StateFile)
		go modelSaver(cfg.Server.StateFile)
	}

	// 1. MQTT Client Configuration (points to localhost if running on Cerbo)
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	opts.SetClientID(cfg.MQTT.ClientID)
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
	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		log.Fatalf("Error starting NUT TCP server: %v", err)
	}
	defer listener.Close()
	log.Printf("NUT Server listening on %s...", cfg.Server.Listen)

	// Graceful shutdown management
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Closing server...")
		state.RLock()
		dirty := state.ModelDirty
		state.RUnlock()
		if cfg.Server.StateFile != "" && dirty {
			if err := persistModel(cfg.Server.StateFile); err != nil {
				log.Printf("Error saving model on shutdown: %v", err)
			}
		}
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
		// Long-horizon average voltage: converts discovered Ah capacity to Wh
		// without hardcoding the system's nominal voltage (12/24/48V all work)
		if valFloat > 0 {
			if state.AvgBatteryVolts == 0 {
				state.AvgBatteryVolts = valFloat
			} else {
				state.AvgBatteryVolts = emaStep(state.AvgBatteryVolts, valFloat, 1.0, voltsEmaTau)
			}
		}
	} else if strings.HasSuffix(topic, "/Settings/CGwacs/BatteryLife/MinimumSocLimit") {
		state.SocFloorMin = valFloat
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
// While discharging, the battery meter is the ground truth and every sample
// also trains the learned AC->DC model. On grid, the learned model translates
// the AC load into the DC draw the battery would see. Smoothing uses a
// time-based EMA so the result is independent of how often Venus OS publishes.
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

	var sample float64
	if state.BatteryAmps < -0.5 && state.BatteryVolts > 0 {
		sample = math.Abs(state.BatteryAmps * state.BatteryVolts)
		learnLoadModelLocked(state.AcOutWatts, sample, dt)
	} else {
		sample = state.ModelA*state.AcOutWatts + state.ModelB
	}

	if state.RuntimeWatts == 0 {
		state.RuntimeWatts = sample
	} else {
		state.RuntimeWatts = emaStep(state.RuntimeWatts, sample, dt, runtimeEmaTau)
	}
}

// learnLoadModelLocked feeds one (AC watts, measured DC watts) observation into
// the decayed least-squares statistics and refreshes the model coefficients.
// Sample weight is its time span dt, so the fit is publish-rate independent.
// Must be called with state.Lock held.
func learnLoadModelLocked(ac, dc, dt float64) {
	decay := math.Exp(-dt / learnForgetTau)
	state.RegN = state.RegN*decay + dt
	state.RegSx = state.RegSx*decay + dt*ac
	state.RegSy = state.RegSy*decay + dt*dc
	state.RegSxx = state.RegSxx*decay + dt*ac*ac
	state.RegSxy = state.RegSxy*decay + dt*ac*dc
	solveLoadModelLocked()
	state.ModelDirty = true
}

// solveLoadModelLocked computes ModelA/ModelB from the learned statistics plus
// two prior anchor points (at AC=0 and AC=priorAnchorAC). The anchors keep the
// 2x2 system well conditioned even when the observed load barely varies: a
// flat load only pins the total draw at one operating point, and the prior
// decides how to split the correction between slope and idle overhead.
func solveLoadModelLocked() {
	// Second prior anchor point sits at 75% of the inverter rating
	anchorAC := cfg.Power.InverterMaxVA * 0.75

	n := state.RegN + 2*priorWeight
	sx := state.RegSx + priorWeight*anchorAC
	sy := state.RegSy + priorWeight*(2*seedIdleWatts+seedSlope*anchorAC)
	sxx := state.RegSxx + priorWeight*anchorAC*anchorAC
	sxy := state.RegSxy + priorWeight*anchorAC*(seedSlope*anchorAC+seedIdleWatts)

	det := n*sxx - sx*sx
	if det <= 0 {
		return
	}
	a := (n*sxy - sx*sy) / det
	b := (sy*sxx - sx*sxy) / det

	state.ModelA = math.Min(math.Max(a, modelSlopeMin), modelSlopeMax)
	state.ModelB = math.Min(math.Max(b, modelIdleMin), modelIdleMax)
}

// emaStep advances an exponential moving average by dt seconds toward sample,
// using a time constant tau. Equivalent alpha = 1 - e^(-dt/tau).
func emaStep(prev, sample, dt, tau float64) float64 {
	alpha := 1.0 - math.Exp(-dt/tau)
	return prev + alpha*(sample-prev)
}

// modelFile is the on-disk format of the persisted learned state
type modelFile struct {
	RegN            float64 `json:"reg_n"`
	RegSx           float64 `json:"reg_sx"`
	RegSy           float64 `json:"reg_sy"`
	RegSxx          float64 `json:"reg_sxx"`
	RegSxy          float64 `json:"reg_sxy"`
	AvgBatteryVolts float64 `json:"avg_battery_volts"`
}

// loadModel restores the learned load model from a previous run. A missing or
// invalid file is not an error: the model simply restarts from its prior.
func loadModel(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		debugLog("No model file loaded (%v), starting from prior", err)
		return
	}
	var mf modelFile
	if err := json.Unmarshal(data, &mf); err != nil {
		log.Printf("Model file %s is corrupted, ignoring: %v", path, err)
		return
	}
	for _, v := range []float64{mf.RegN, mf.RegSx, mf.RegSy, mf.RegSxx, mf.RegSxy, mf.AvgBatteryVolts} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			log.Printf("Model file %s contains invalid values, ignoring", path)
			return
		}
	}
	if mf.RegN < 0 || mf.RegSxx < 0 {
		log.Printf("Model file %s contains inconsistent statistics, ignoring", path)
		return
	}

	state.Lock()
	state.RegN, state.RegSx, state.RegSy = mf.RegN, mf.RegSx, mf.RegSy
	state.RegSxx, state.RegSxy = mf.RegSxx, mf.RegSxy
	if mf.AvgBatteryVolts > 0 {
		state.AvgBatteryVolts = mf.AvgBatteryVolts
	}
	solveLoadModelLocked()
	a, b := state.ModelA, state.ModelB
	state.Unlock()

	log.Printf("Restored load model from %s: DC = %.3f*AC + %.1fW (%.0fs of discharge data)", path, a, b, mf.RegN)
}

// persistModel atomically writes the learned state to disk. The dirty flag is
// cleared before writing and restored on failure, so learning that happens
// mid-write is picked up by the next persist.
func persistModel(path string) error {
	state.Lock()
	mf := modelFile{
		RegN: state.RegN, RegSx: state.RegSx, RegSy: state.RegSy,
		RegSxx: state.RegSxx, RegSxy: state.RegSxy,
		AvgBatteryVolts: state.AvgBatteryVolts,
	}
	state.ModelDirty = false
	state.Unlock()

	data, err := json.Marshal(mf)
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		state.Lock()
		state.ModelDirty = true
		state.Unlock()
	}
	return err
}

// modelSaver persists the learned model in a flash-friendly way: learning only
// happens while discharging, so the single write happens once the discharge is
// over. Normal grid operation never touches the disk.
func modelSaver(path string) {
	for {
		time.Sleep(time.Minute)

		state.RLock()
		dirty := state.ModelDirty
		discharging := state.BatteryAmps < -0.5
		state.RUnlock()

		if !dirty || discharging {
			continue
		}
		if err := persistModel(path); err != nil {
			log.Printf("Error saving model to %s: %v", path, err)
			continue
		}
		debugLog("Learned load model persisted to %s", path)
	}
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
