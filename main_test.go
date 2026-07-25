package main

import (
	"math"
	"testing"
	"time"
)

func TestSampleDt(t *testing.T) {
	now := time.Unix(1000000, 0)
	cases := []struct {
		name string
		last time.Time
		want float64
	}{
		{"zero last seeds at 1s", time.Time{}, 1.0},
		{"out-of-order clamps low", now.Add(5 * time.Second), 0.1},
		{"duplicate clamps low", now, 0.1},
		{"stale clamps high", now.Add(-90 * time.Second), 60},
		{"normal passes through", now.Add(-9 * time.Second), 9},
	}
	for _, c := range cases {
		if got := sampleDt(now, c.last); got != c.want {
			t.Errorf("%s: sampleDt = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAvgVoltsOnlyLearnsWhileDischarging(t *testing.T) {
	state.Lock()
	defer state.Unlock()
	oldAmps, oldAvg, oldLast := state.BatteryAmps, state.AvgBatteryVolts, state.LastVoltsSample
	defer func() {
		state.BatteryAmps, state.AvgBatteryVolts, state.LastVoltsSample = oldAmps, oldAvg, oldLast
	}()
	state.BatteryAmps, state.AvgBatteryVolts, state.LastVoltsSample = 0, 0, time.Time{}

	now := time.Unix(1000000, 0)

	// Idle on grid: no learning, but the sample clock still advances so the
	// first discharging sample sees a publish-interval dt, not hours.
	updateAvgVoltsLocked(56.8, now)
	if state.AvgBatteryVolts != 0 {
		t.Fatalf("avg volts learned while idle: %v", state.AvgBatteryVolts)
	}
	if !state.LastVoltsSample.Equal(now) {
		t.Fatalf("sample clock not advanced while idle")
	}

	// Charging: still no learning.
	state.BatteryAmps = 19
	updateAvgVoltsLocked(57.2, now.Add(10*time.Second))
	if state.AvgBatteryVolts != 0 {
		t.Fatalf("avg volts learned while charging: %v", state.AvgBatteryVolts)
	}

	// First discharging sample seeds the average directly.
	state.BatteryAmps = -5
	updateAvgVoltsLocked(53.5, now.Add(20*time.Second))
	if state.AvgBatteryVolts != 53.5 {
		t.Fatalf("first discharge sample did not seed: %v", state.AvgBatteryVolts)
	}

	// Later samples advance by elapsed time, not by message count.
	updateAvgVoltsLocked(53.0, now.Add(30*time.Second))
	want := 53.5 + (1-math.Exp(-10.0/voltsEmaTau))*(53.0-53.5)
	if math.Abs(state.AvgBatteryVolts-want) > 1e-9 {
		t.Fatalf("EMA step wrong: got %v, want %v", state.AvgBatteryVolts, want)
	}
	frozen := state.AvgBatteryVolts

	// Zero/invalid voltage is ignored even while discharging.
	updateAvgVoltsLocked(0, now.Add(40*time.Second))
	if state.AvgBatteryVolts != frozen {
		t.Fatalf("zero voltage moved the average: %v", state.AvgBatteryVolts)
	}

	// Back on grid: the average freezes at the discharge value.
	state.BatteryAmps = 19
	updateAvgVoltsLocked(57.6, now.Add(50*time.Second))
	if state.AvgBatteryVolts != frozen {
		t.Fatalf("avg volts moved while charging: got %v, want %v", state.AvgBatteryVolts, frozen)
	}
}
