package main

import (
	"bufio"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitNUTLine(t *testing.T) {
	cases := map[string][]string{
		"GET VAR ups battery.charge": {"GET", "VAR", "ups", "battery.charge"},
		`PASSWORD "my secret"`:       {"PASSWORD", "my secret"},
		`PASSWORD "with \"quotes\""`: {"PASSWORD", `with "quotes"`},
		`USERNAME ""`:                {"USERNAME", ""},
		"  LIST   UPS  ":             {"LIST", "UPS"},
		"":                           nil,
	}
	for line, want := range cases {
		if got := splitNUTLine(line); !reflect.DeepEqual(got, want) {
			t.Errorf("splitNUTLine(%q) = %#v, want %#v", line, got, want)
		}
	}
}

// nutSession runs handleNUTConnection over an in-memory pipe and returns a
// helper that sends one command and returns the first response line.
func nutSession(t *testing.T) func(string) string {
	t.Helper()
	server, client := net.Pipe()
	go handleNUTConnection(server)
	t.Cleanup(func() { client.Close() })

	reader := bufio.NewReader(client)
	return func(cmd string) string {
		client.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := client.Write([]byte(cmd + "\n")); err != nil {
			t.Fatalf("write %q: %v", cmd, err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read reply to %q: %v", cmd, err)
		}
		return strings.TrimSpace(line)
	}
}

func TestAuthFlow(t *testing.T) {
	cfg = defaultConfig()
	cfg.Users = []UserConfig{{Username: "upsuser", Password: "s3cret"}}

	send := nutSession(t)

	if got := send("LOGIN ups"); got != "ERR USERNAME-REQUIRED" {
		t.Fatalf("LOGIN without username: %s", got)
	}
	if got := send("PASSWORD s3cret"); got != "OK" {
		t.Fatalf("PASSWORD: %s", got)
	}
	if got := send("LOGIN ups"); got != "ERR USERNAME-REQUIRED" {
		t.Fatalf("LOGIN without username: %s", got)
	}
	if got := send("USERNAME upsuser"); got != "OK" {
		t.Fatalf("USERNAME: %s", got)
	}
	if got := send("USERNAME again"); got != "ERR ALREADY-SET-USERNAME" {
		t.Fatalf("second USERNAME: %s", got)
	}
	if got := send("LOGIN nope"); got != "ERR UNKNOWN-UPS" {
		t.Fatalf("LOGIN unknown ups: %s", got)
	}
	if got := send("LOGIN ups"); got != "OK" {
		t.Fatalf("LOGIN: %s", got)
	}
	if got := send("LOGIN ups"); got != "ERR ALREADY-LOGGED-IN" {
		t.Fatalf("second LOGIN: %s", got)
	}
	if got := send("GET NUMLOGINS ups"); got != "NUMLOGINS ups 1" {
		t.Fatalf("NUMLOGINS: %s", got)
	}
	if got := send("LOGOUT"); got != "OK Goodbye" {
		t.Fatalf("LOGOUT: %s", got)
	}
	if n := len(loggedInClients()); n != 0 {
		t.Fatalf("client not unregistered after LOGOUT: %d", n)
	}
}

func TestAuthRejectsBadCredentials(t *testing.T) {
	cfg = defaultConfig()
	cfg.Users = []UserConfig{{Username: "upsuser", Password: "s3cret"}}

	send := nutSession(t)

	send("USERNAME upsuser")
	send("PASSWORD wrong")
	if got := send("LOGIN ups"); got != "ERR ACCESS-DENIED" {
		t.Fatalf("LOGIN with wrong password: %s", got)
	}
	if got := send("SET VAR ups battery.charge 50"); got != "ERR ACCESS-DENIED" {
		t.Fatalf("SET with wrong password: %s", got)
	}
	if got := send("INSTCMD ups shutdown.return"); got != "ERR ACCESS-DENIED" {
		t.Fatalf("INSTCMD with wrong password: %s", got)
	}
}

func TestAuthDisabledWithoutUsers(t *testing.T) {
	cfg = defaultConfig()

	send := nutSession(t)

	if got := send("LOGIN ups"); got != "OK" {
		t.Fatalf("LOGIN with auth disabled: %s", got)
	}
	if got := send("GET VAR ups battery.charge"); !strings.HasPrefix(got, "VAR ups battery.charge") {
		t.Fatalf("GET VAR: %s", got)
	}
	send("LOGOUT")
}

func TestUnknownUpsAndReadOnlyAccess(t *testing.T) {
	cfg = defaultConfig()
	cfg.Users = []UserConfig{{Username: "upsuser", Password: "s3cret"}}

	send := nutSession(t)

	// Read-only queries must work without authentication, per upsd behavior
	if got := send("GET VAR ups ups.status"); !strings.HasPrefix(got, "VAR ups ups.status") {
		t.Fatalf("unauthenticated GET VAR should work: %s", got)
	}
	if got := send("GET VAR wrong ups.status"); got != "ERR UNKNOWN-UPS" {
		t.Fatalf("GET VAR on unknown ups: %s", got)
	}
	if got := send("LIST VAR wrong"); got != "ERR UNKNOWN-UPS" {
		t.Fatalf("LIST VAR on unknown ups: %s", got)
	}
	if got := send("GET VAR ups no.such.var"); got != "ERR VAR-NOT-SUPPORTED" {
		t.Fatalf("GET VAR unknown var: %s", got)
	}
	if got := send("BOGUS"); got != "ERR UNKNOWN-COMMAND" {
		t.Fatalf("unknown command: %s", got)
	}
}

func TestPerUserNetworkACL(t *testing.T) {
	cfg = defaultConfig()
	cfg.Users = []UserConfig{
		{Username: "lan", Password: "pw", AllowedNetworks: []string{"192.168.1.0/24", "10.0.0.5"}},
		{Username: "anywhere", Password: "pw"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	cases := []struct {
		user, addr string
		want       bool
	}{
		{"lan", "192.168.1.42:50123", true},
		{"lan", "192.168.2.42:50123", false},    // outside CIDR
		{"lan", "10.0.0.5:1", true},             // bare IP entry
		{"lan", "10.0.0.6:1", false},            // neighbor of bare IP
		{"lan", "[::ffff:192.168.1.9]:5", true}, // IPv4-mapped IPv6
		{"lan", "pipe", false},                  // unparsable source
		{"anywhere", "203.0.113.7:9", true},     // unrestricted account
		{"anywhere", "pipe", true},              // unrestricted, weird addr
	}
	for _, c := range cases {
		if got := credentialsValid(c.user, "pw", c.addr); got != c.want {
			t.Errorf("credentialsValid(%s from %s) = %v, want %v", c.user, c.addr, got, c.want)
		}
	}

	// Wrong password never passes, even from an allowed network
	if credentialsValid("lan", "wrong", "192.168.1.42:50123") {
		t.Error("wrong password accepted")
	}
}

func TestNetworkACLEndToEnd(t *testing.T) {
	cfg = defaultConfig()
	cfg.Users = []UserConfig{{Username: "lan", Password: "pw", AllowedNetworks: []string{"192.168.1.0/24"}}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// net.Pipe has no source IP, so a restricted account must be denied
	send := nutSession(t)
	send("USERNAME lan")
	send("PASSWORD pw")
	if got := send("LOGIN ups"); got != "ERR ACCESS-DENIED" {
		t.Fatalf("restricted user from unknown source: %s", got)
	}
}
