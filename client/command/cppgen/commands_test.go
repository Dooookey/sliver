package cppgen

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestGenerateCppBeaconIsSubcommand locks the command shape to match the
// official `generate` command: session is `generate-cpp ...`, beacon is
// `generate-cpp beacon ...`. A --beacon flag (or a stray positional argument)
// previously produced a session while the operator expected a beacon.
func TestGenerateCppBeaconIsSubcommand(t *testing.T) {
	cmds := Commands(nil)
	if len(cmds) != 1 {
		t.Fatalf("expected exactly one top-level command, got %d", len(cmds))
	}
	root := cmds[0]

	if root.Flags().Lookup("beacon") != nil {
		t.Fatalf("generate-cpp must not expose a --beacon flag; beacon is a subcommand")
	}

	var beacon *cobra.Command
	for _, sub := range root.Commands() {
		if sub.Name() == "beacon" {
			beacon = sub
		}
	}
	if beacon == nil {
		t.Fatalf("missing `beacon` subcommand")
	}

	for _, name := range []string{"interval", "jitter"} {
		if root.Flags().Lookup(name) != nil {
			t.Fatalf("session command must not expose --%s", name)
		}
		if beacon.Flags().Lookup(name) == nil {
			t.Fatalf("beacon subcommand must expose --%s", name)
		}
	}

	// The session command must carry the implant flags too.
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
