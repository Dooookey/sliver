package cppgen

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func beaconSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmds := Commands(nil)
	if len(cmds) != 1 {
		t.Fatalf("expected exactly one top-level command, got %d", len(cmds))
	}
	for _, sub := range cmds[0].Commands() {
		if sub.Name() == "beacon" {
			return sub
		}
	}
	t.Fatalf("missing `beacon` subcommand")
	return nil
}

// TestGenerateCppBeaconIsSubcommand locks the command shape to match the
// official `generate` command: session is `generate-cpp ...`, beacon is
// `generate-cpp beacon ...`. A --beacon flag (or a stray positional argument)
// previously produced a session while the operator expected a beacon.
func TestGenerateCppBeaconIsSubcommand(t *testing.T) {
	root := Commands(nil)[0]

	if root.Flags().Lookup("beacon") != nil {
		t.Fatalf("generate-cpp must not expose a --beacon flag; beacon is a subcommand")
	}

	beacon := beaconSubcommand(t)

	// Official beacon flag surface: days/hours/minutes/seconds/jitter.
	type flagSpec struct{ name, shorthand string }
	beaconOnly := []flagSpec{
		{"days", "D"}, {"hours", "H"}, {"minutes", "M"},
		{"seconds", "S"}, {"jitter", "J"},
	}
	for _, spec := range beaconOnly {
		if root.Flags().Lookup(spec.name) != nil {
			t.Fatalf("session command must not expose --%s", spec.name)
		}
		f := beacon.Flags().Lookup(spec.name)
		if f == nil {
			t.Fatalf("beacon subcommand missing --%s", spec.name)
		}
		if f.Shorthand != spec.shorthand {
			t.Fatalf("--%s shorthand = %q, want %q", spec.name, f.Shorthand, spec.shorthand)
		}
	}
	// The old interval flag is gone (official parity).
	if beacon.Flags().Lookup("interval") != nil {
		t.Fatalf("beacon subcommand must not expose --interval; use --seconds")
	}
	// Official defaults.
	if f := beacon.Flags().Lookup("seconds"); f.DefValue != "60" {
		t.Fatalf("--seconds default = %q, want 60", f.DefValue)
	}
	if f := beacon.Flags().Lookup("jitter"); f.DefValue != "30" {
		t.Fatalf("--jitter default = %q, want 30", f.DefValue)
	}

	for _, name := range []string{"mtls", "os", "arch", "format", "debug"} {
		if root.Flags().Lookup(name) == nil {
			t.Fatalf("session command missing --%s", name)
		}
		if beacon.Flags().Lookup(name) == nil {
			t.Fatalf("beacon subcommand missing --%s", name)
		}
	}

	// Positional args must be rejected so `generate-cpp beacon` can never be
	// silently swallowed as an argument again.
	if root.Args == nil {
		t.Fatalf("generate-cpp must set Args")
	}
	if err := root.Args(root, []string{"beacon"}); err == nil {
		t.Fatalf("stray positional arguments must be rejected")
	}
}

// TestParseCppBeaconFlags pins the official interval/jitter semantics.
func TestParseCppBeaconFlags(t *testing.T) {
	cases := []struct {
		name     string
		set      map[string]string
		wantSecs int64
		wantJit  int64
	}{
		{name: "defaults", set: nil, wantSecs: 60, wantJit: 30},
		{name: "seconds only", set: map[string]string{"seconds": "10"}, wantSecs: 10, wantJit: 30},
		{name: "minutes suppress default seconds", set: map[string]string{"minutes": "2"}, wantSecs: 120, wantJit: 30},
		{name: "minutes plus explicit seconds", set: map[string]string{"minutes": "1", "seconds": "30"}, wantSecs: 90, wantJit: 30},
		{name: "hours and jitter", set: map[string]string{"hours": "1", "jitter": "5"}, wantSecs: 3600, wantJit: 5},
		{name: "days", set: map[string]string{"days": "1"}, wantSecs: 86400, wantJit: 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beacon := beaconSubcommand(t)
			for k, v := range tc.set {
				if err := beacon.Flags().Set(k, v); err != nil {
					t.Fatalf("set --%s: %v", k, err)
				}
			}
			interval, jitter, ok := parseCppBeaconFlags(beacon, nil, true)
			if !ok {
				t.Fatalf("parseCppBeaconFlags reported failure")
			}
			if got := int64(time.Duration(interval) / time.Second); got != tc.wantSecs {
				t.Fatalf("interval = %ds, want %ds", got, tc.wantSecs)
			}
			if got := int64(time.Duration(jitter) / time.Second); got != tc.wantJit {
				t.Fatalf("jitter = %ds, want %ds", got, tc.wantJit)
			}
		})
	}
}

// TestParseCppBeaconFlagsSessionIsZero ensures the session command carries no
// beacon timing values.
func TestParseCppBeaconFlagsSessionIsZero(t *testing.T) {
	root := Commands(nil)[0]
	interval, jitter, ok := parseCppBeaconFlags(root, nil, false)
	if !ok || interval != 0 || jitter != 0 {
		t.Fatalf("session config must have zero beacon timing, got %d/%d ok=%v", interval, jitter, ok)
	}
}
