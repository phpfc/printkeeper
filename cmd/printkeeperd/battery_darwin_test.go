package main

import "testing"

// batteryDischargingFixture and batteryPluggedInFixture are trimmed,
// representative excerpts of real `ioreg -rn AppleSmartBattery`
// output, captured by hand on this task's own development machine
// (see this task's report for the untrimmed original). Only the keys
// parseExternalConnected and this test actually care about are kept.
const batteryDischargingFixture = `+-o AppleSmartBattery  <class AppleSmartBattery, id 0x1000007fa, registered, matched, active, busy 0 (43 ms), retain 10>
  | {
  |   "TimeRemaining" = 182
  |   "ExternalConnected" = No
  |   "ExternalChargeCapable" = No
  |   "CurrentCapacity" = 87
  | }
`

const batteryPluggedInFixture = `+-o AppleSmartBattery  <class AppleSmartBattery, id 0x1000007fa, registered, matched, active, busy 0 (12 ms), retain 10>
  | {
  |   "TimeRemaining" = 65535
  |   "ExternalConnected" = Yes
  |   "ExternalChargeCapable" = Yes
  |   "CurrentCapacity" = 100
  | }
`

// TestParseExternalConnected covers the shapes onBattery must get
// right: genuinely on battery, a battery-equipped machine sitting on
// charge (must not be mistaken for "on battery" just because it has a
// battery at all), and the fallback case — no AppleSmartBattery
// service in the dump whatsoever, the ordinary shape of ioreg's output
// on any desktop that has never had a battery — which must default to
// "not on battery" (see parseExternalConnected's own doc comment for
// why that direction, not the other, is the conservative one).
func TestParseExternalConnected(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"discharging", batteryDischargingFixture, true},
		{"plugged in", batteryPluggedInFixture, false},
		{"no battery service at all (desktop)", "", false},
		{"unparseable garbage", "not even close to ioreg output", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseExternalConnected([]byte(tt.in))
			if got != tt.want {
				t.Errorf("parseExternalConnected(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
