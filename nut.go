package main

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"sync"
)

// session tracks the authentication state of one NUT client connection
type session struct {
	remoteAddr string
	username   string
	password   string
	loggedIn   bool
}

// nutClients tracks clients that issued a successful LOGIN, backing the
// NUMLOGINS variable and the LIST CLIENT command.
var nutClients = struct {
	sync.Mutex
	addrs map[string]bool
}{addrs: map[string]bool{}}

func registerLogin(addr string) {
	nutClients.Lock()
	defer nutClients.Unlock()
	nutClients.addrs[addr] = true
}

func unregisterLogin(addr string) {
	nutClients.Lock()
	defer nutClients.Unlock()
	delete(nutClients.addrs, addr)
}

func loggedInClients() []string {
	nutClients.Lock()
	defer nutClients.Unlock()
	addrs := make([]string, 0, len(nutClients.addrs))
	for a := range nutClients.addrs {
		addrs = append(addrs, a)
	}
	return addrs
}

// credentialsValid checks a username/password pair against the configured
// users in constant time.
func credentialsValid(user, pass string) bool {
	valid := false
	for _, u := range cfg.Users {
		userOk := subtle.ConstantTimeCompare([]byte(u.Username), []byte(user)) == 1
		passOk := subtle.ConstantTimeCompare([]byte(u.Password), []byte(pass)) == 1
		if userOk && passOk {
			valid = true
		}
	}
	return valid
}

// authErr returns the NUT error line for a session that is not (yet) allowed
// to perform an authenticated action, or "" when the session may proceed.
// With no users configured, authentication is disabled.
func (s *session) authErr() string {
	if len(cfg.Users) == 0 {
		return ""
	}
	if s.username == "" {
		return "ERR USERNAME-REQUIRED\n"
	}
	if s.password == "" {
		return "ERR PASSWORD-REQUIRED\n"
	}
	if !credentialsValid(s.username, s.password) {
		return "ERR ACCESS-DENIED\n"
	}
	return ""
}

// splitNUTLine tokenizes a NUT protocol line, honoring double-quoted arguments
// with backslash escapes (e.g. PASSWORD "my secret"), as upsd does.
func splitNUTLine(line string) []string {
	var args []string
	var cur strings.Builder
	inQuote, escaped, started := false, false, false

	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case inQuote && r == '\\':
			escaped = true
		case r == '"':
			inQuote = !inQuote
			started = true
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	return args
}

func generateNUTVars() map[string]string {
	state.RLock()
	defer state.RUnlock()

	vars := make(map[string]string)

	vars["driver.name"] = "dummy-victron"
	vars["driver.version"] = version
	vars["driver.version.internal"] = version
	vars["driver.parameter.port"] = "mqtt"

	vars["device.mfr"] = cfg.Device.Manufacturer
	vars["device.model"] = cfg.Device.Model
	vars["device.type"] = "ups"
	vars["device.serial"] = cfg.Device.Serial
	vars["ups.mfr"] = cfg.Device.Manufacturer
	vars["ups.model"] = cfg.Device.Model
	vars["ups.serial"] = cfg.Device.Serial

	vars["battery.type"] = cfg.Device.BatteryType
	vars["battery.charge"] = fmt.Sprintf("%.1f", state.BatterySoc)
	vars["battery.voltage"] = fmt.Sprintf("%.2f", state.BatteryVolts)
	vars["battery.current"] = fmt.Sprintf("%.2f", state.BatteryAmps)
	vars["battery.charge.low"] = fmt.Sprintf("%.0f", cfg.Thresholds.BatteryChargeLow)
	vars["battery.runtime.low"] = fmt.Sprintf("%.0f", cfg.Thresholds.BatteryRuntimeLow)

	// Victron's TimeToGo is only meaningful while actually discharging; on grid
	// it is stale or absent, so there we rely on our own load-based estimate.
	onBattery := state.GridLost || state.AcInVolts < cfg.Thresholds.GridLostVoltage
	var runtimeSeconds float64
	if onBattery && state.BatteryTimeToGo > 0 {
		runtimeSeconds = state.BatteryTimeToGo
	} else {
		calcWatts := state.RuntimeWatts
		if calcWatts < 1 {
			calcWatts = 1 // Div-by-zero guard; the real floor is the learned idle draw
		}

		capacityWh := cfg.Power.BatteryCapacityWh
		if state.BatteryCapacityAh > 0 && state.AvgBatteryVolts > 0 {
			capacityWh = state.BatteryCapacityAh * state.AvgBatteryVolts
		}

		// Unusable reserve: the ESS minimum SoC limit when the system publishes
		// one, a conservative default otherwise
		socFloor := defaultSocFloor
		if state.SocFloorMin >= 0 {
			socFloor = state.SocFloorMin
		}
		usableSoc := state.BatterySoc - socFloor
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

	loadPercent := (state.AcOutWatts / cfg.Power.InverterMaxVA) * 100
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

	if state.GridLost || state.AcInVolts < cfg.Thresholds.GridLostVoltage {
		status = "OB"
	}

	if state.BatteryAmps > 1.0 {
		status = "OL CHRG"
	} else if state.BatteryAmps < -1.0 {
		status += " DISCHRG"
	}

	if strings.Contains(status, "OB") {
		if state.BatterySoc <= cfg.Thresholds.LowBatterySoc || runtimeSeconds <= cfg.Thresholds.LowBatteryRuntime {
			status += " LB"
		}
	}

	return status
}

func handleNUTConnection(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	writer := bufio.NewWriter(conn)

	sess := &session{remoteAddr: conn.RemoteAddr().String()}
	defer func() {
		if sess.loggedIn {
			unregisterLogin(sess.remoteAddr)
		}
	}()

	debugLog("[NUT] New connection from %s", sess.remoteAddr)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		debugLog("[NUT Client %s] -> %s", sess.remoteAddr, line)

		parts := splitNUTLine(line)
		if len(parts) == 0 {
			continue
		}
		command := strings.ToUpper(parts[0])

		switch command {
		case "HELP":
			writer.WriteString("BEGIN HELP\nHELP\nVER\nNETVER\nLIST\nGET\nSET\nINSTCMD\nLOGIN\nLOGOUT\nUSERNAME\nPASSWORD\nSTARTTLS\nEND HELP\n")

		case "VER":
			writer.WriteString("Network UPS Tools upsd 2.8.0\n")

		case "NETVER":
			writer.WriteString("1.2\n")

		case "STARTTLS":
			writer.WriteString("ERR FEATURE-NOT-SUPPORTED\n")

		case "TRACKING":
			writer.WriteString("OK\n")

		case "USERNAME":
			switch {
			case len(parts) != 2:
				writer.WriteString("ERR INVALID-ARGUMENT\n")
			case sess.username != "":
				writer.WriteString("ERR ALREADY-SET-USERNAME\n")
			default:
				sess.username = parts[1]
				writer.WriteString("OK\n")
			}

		case "PASSWORD":
			switch {
			case len(parts) != 2:
				writer.WriteString("ERR INVALID-ARGUMENT\n")
			case sess.password != "":
				writer.WriteString("ERR ALREADY-SET-PASSWORD\n")
			default:
				sess.password = parts[1]
				writer.WriteString("OK\n")
			}

		case "LOGIN":
			switch {
			case len(parts) != 2:
				writer.WriteString("ERR INVALID-ARGUMENT\n")
			case parts[1] != cfg.UPS.Name:
				writer.WriteString("ERR UNKNOWN-UPS\n")
			case sess.loggedIn:
				writer.WriteString("ERR ALREADY-LOGGED-IN\n")
			default:
				if errLine := sess.authErr(); errLine != "" {
					writer.WriteString(errLine)
					break
				}
				sess.loggedIn = true
				registerLogin(sess.remoteAddr)
				writer.WriteString("OK\n")
			}

		case "LOGOUT":
			if sess.loggedIn {
				unregisterLogin(sess.remoteAddr)
				sess.loggedIn = false
			}
			writer.WriteString("OK Goodbye\n")
			writer.Flush()
			return

		case "SET":
			// Authenticated command; no RW variables are exposed, so a valid
			// user still gets VAR-NOT-SUPPORTED (LIST RW is empty).
			if errLine := sess.authErr(); errLine != "" {
				writer.WriteString(errLine)
				break
			}
			writer.WriteString("ERR VAR-NOT-SUPPORTED\n")

		case "INSTCMD":
			if errLine := sess.authErr(); errLine != "" {
				writer.WriteString(errLine)
				break
			}
			writer.WriteString("ERR CMD-NOT-SUPPORTED\n")

		case "LIST":
			handleList(writer, parts)

		case "GET":
			handleGet(writer, parts)

		default:
			writer.WriteString("ERR UNKNOWN-COMMAND\n")
		}

		writer.Flush()
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[NUT] Error reading from %s: %v", sess.remoteAddr, err)
	}
	debugLog("[NUT] Connection closed by %s", sess.remoteAddr)
}

func handleList(writer *bufio.Writer, parts []string) {
	if len(parts) < 2 {
		writer.WriteString("ERR INVALID-ARGUMENT\n")
		return
	}
	sub := strings.ToUpper(parts[1])

	if sub == "UPS" {
		writer.WriteString(fmt.Sprintf("BEGIN LIST UPS\nUPS %s \"%s\"\nEND LIST UPS\n", cfg.UPS.Name, cfg.UPS.Description))
		return
	}

	if len(parts) < 3 {
		writer.WriteString("ERR INVALID-ARGUMENT\n")
		return
	}
	upsName := parts[2]
	if upsName != cfg.UPS.Name {
		writer.WriteString("ERR UNKNOWN-UPS\n")
		return
	}

	switch sub {
	case "VAR":
		writer.WriteString(fmt.Sprintf("BEGIN LIST VAR %s\n", upsName))
		for k, v := range generateNUTVars() {
			writer.WriteString(fmt.Sprintf("VAR %s %s \"%s\"\n", upsName, k, v))
		}
		writer.WriteString(fmt.Sprintf("END LIST VAR %s\n", upsName))
	case "RW":
		writer.WriteString(fmt.Sprintf("BEGIN LIST RW %s\nEND LIST RW %s\n", upsName, upsName))
	case "CMD":
		writer.WriteString(fmt.Sprintf("BEGIN LIST CMD %s\nEND LIST CMD %s\n", upsName, upsName))
	case "CLIENT":
		writer.WriteString(fmt.Sprintf("BEGIN LIST CLIENT %s\n", upsName))
		for _, addr := range loggedInClients() {
			writer.WriteString(fmt.Sprintf("CLIENT %s %s\n", upsName, addr))
		}
		writer.WriteString(fmt.Sprintf("END LIST CLIENT %s\n", upsName))
	default:
		writer.WriteString("ERR INVALID-ARGUMENT\n")
	}
}

func handleGet(writer *bufio.Writer, parts []string) {
	if len(parts) < 3 {
		writer.WriteString("ERR INVALID-ARGUMENT\n")
		return
	}
	sub := strings.ToUpper(parts[1])
	upsName := parts[2]
	if upsName != cfg.UPS.Name {
		writer.WriteString("ERR UNKNOWN-UPS\n")
		return
	}

	switch {
	case sub == "VAR" && len(parts) == 4:
		if val, ok := generateNUTVars()[parts[3]]; ok {
			writer.WriteString(fmt.Sprintf("VAR %s %s \"%s\"\n", upsName, parts[3], val))
		} else {
			writer.WriteString("ERR VAR-NOT-SUPPORTED\n")
		}
	case sub == "UPSDESC" && len(parts) == 3:
		writer.WriteString(fmt.Sprintf("UPSDESC %s \"%s\"\n", upsName, cfg.UPS.Description))
	case sub == "DESC" && len(parts) == 4:
		writer.WriteString(fmt.Sprintf("DESC %s %s \"Generic Description\"\n", upsName, parts[3]))
	case sub == "TYPE" && len(parts) == 4:
		writer.WriteString(fmt.Sprintf("TYPE %s %s STRING\n", upsName, parts[3]))
	case sub == "CMDDESC" && len(parts) == 4:
		writer.WriteString(fmt.Sprintf("CMDDESC %s %s \"Command description\"\n", upsName, parts[3]))
	case sub == "NUMLOGINS" && len(parts) == 3:
		writer.WriteString(fmt.Sprintf("NUMLOGINS %s %d\n", upsName, len(loggedInClients())))
	default:
		writer.WriteString("ERR INVALID-ARGUMENT\n")
	}
}
