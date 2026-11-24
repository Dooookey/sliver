package cppgen

/*
	Sliver Implant Framework
	Copyright (C) 2019  Bishop Fox

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU General Public License for more details.

	You should have received a copy of the GNU General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

// Package cppgen implements `generate-cpp`: a standalone command that generates
// the C/C++ implant through the GenerateCpp RPC.
//
// It is deliberately a separate command/package instead of a flag on the
// official `generate` command: the upstream generation logic stays untouched,
// so a bug in the C/C++ path can never change Go implant behaviour, and the two
// commands can be compared side by side (same flags, same target resolution,
// different build backend).
//
// The C2 URI parsers are reused from the generate package (they are exported and
// pure parsing helpers), so address syntax is identical to upstream.

import (
	"strings"

	"github.com/bishopfox/sliver/client/command/flags"
	"github.com/bishopfox/sliver/client/command/generate"
	"github.com/bishopfox/sliver/client/console"
	consts "github.com/bishopfox/sliver/client/constants"
	"github.com/bishopfox/sliver/protobuf/clientpb"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Commands returns the generate-cpp command tree.
func Commands(con *console.SliverClient) []*cobra.Command {
	generateCppCmd := &cobra.Command{
		Use:     consts.GenerateStr + "-cpp",
		Aliases: []string{"gcpp"},
		Short:   "Generate a C/C++ implant binary (cmake build backend)",
		Long: "Generate a C/C++ implant binary.\n\n" +
			"Same flags and target resolution as `generate`, but the artifact is built by the\n" +
			"C/C++ project (cmake) instead of the Go toolchain, and it speaks the standard\n" +
			"Sliver wire protocol.",
		// The official `generate` exposes a `beacon` subcommand; a stray positional
		// arg here (e.g. `generate-cpp beacon`) used to be silently ignored and
		// produce a session, so reject unexpected args instead of guessing.
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			GenerateCppCmd(cmd, con, args)
		},
		GroupID: consts.PayloadsHelpGroup,
	}

	flags.Bind("generate-cpp", true, generateCppCmd, func(f *pflag.FlagSet) {
		f.Int64P("timeout", "t", flags.DefaultTimeout, "grpc timeout in seconds")
	})
	cppImplantFlags(generateCppCmd)

	// `generate-cpp beacon` mirrors `generate beacon`: same flags, but IsBeacon is
	// forced true. The `--beacon` flag on the parent stays as a convenience alias.
	generateCppBeaconCmd := &cobra.Command{
		Use:   consts.BeaconStr,
		Short: "Generate a C/C++ beacon binary (cmake build backend)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Flags().Set("beacon", "true")
			GenerateCppCmd(cmd, con, args)
		},
		GroupID: consts.PayloadsHelpGroup,
	}
	// Local flags only: persistent flags (e.g. --timeout) are inherited from the
	// parent generate-cpp command.
	cppImplantFlags(generateCppBeaconCmd)
	generateCppCmd.AddCommand(generateCppBeaconCmd)

	return []*cobra.Command{generateCppCmd}
}

// cppImplantFlags mirrors the subset of the official implant flags that the
// C/C++ runtime understands. Go-only options (symbol obfuscation, netgo,
// traffic encoders, shellcode encoders, external builders) are intentionally
// absent instead of being silently ignored.
func cppImplantFlags(cmd *cobra.Command) {
	f := cmd.Flags()

	// Target
	f.StringP("os", "o", "windows", "operating system (windows, linux)")
	f.StringP("arch", "a", "amd64", "cpu architecture")
	f.StringP("name", "N", "", "implant name (default: random codename)")
	f.StringP("format", "f", "exe", "output format: exe or shared")
	f.StringP("save", "s", "", "directory/file to save the binary to")
	f.BoolP("debug", "d", false, "enable debug features (debug log, no strip)")
	f.StringP("debug-file", "O", "", "path to debug output")

	// C2 channels
	f.StringP("mtls", "m", "", "mtls connection string(s)")
	f.StringP("wg", "g", "", "wireguard connection string(s)")
	f.StringP("http", "b", "", "http(s) connection string(s)")
	f.StringP("dns", "n", "", "dns connection string(s)")
	f.StringP("named-pipe", "p", "", "named-pipe connection string(s)")
	f.StringP("tcp-pivot", "i", "", "tcp-pivot connection string(s)")
	f.Uint32P("key-exchange", "X", generate.DefaultWGKeyExPort, "wg key-exchange port")
	f.Uint32P("tcp-comms", "T", generate.DefaultWGNPort, "wg c2 comms port")
	f.Bool("all-protocols", false, "force include all transport protocols")

	// Behaviour
	f.StringP("strategy", "Z", "", "connection strategy (r = random, rd = random domain, s = sequential)")
	f.Int64P("reconnect", "j", generate.DefaultReconnect, "attempt to reconnect every n second(s)")
	f.Int64P("poll-timeout", "P", generate.DefaultPollTimeout, "long poll request timeout in second(s)")
	f.Uint32P("max-errors", "k", generate.DefaultMaxErrors, "max number of connection errors")
	f.StringP("c2profile", "C", consts.DefaultC2Profile, "HTTP C2 profile to use")
	f.Bool("beacon", false, "generate a beacon instead of an interactive session")
	f.Int64("interval", 60, "beacon interval in second(s)")
	f.Int64("jitter", 0, "beacon jitter in second(s)")

	// Limits
	f.StringP("limit-datetime", "w", "", "limit execution to before datetime")
	f.BoolP("limit-domainjoined", "x", false, "limit execution to domain joined machines")
	f.StringP("limit-username", "y", "", "limit execution to specified username")
	f.StringP("limit-hostname", "z", "", "limit execution to specified hostname")
	f.StringP("limit-fileexists", "F", "", "limit execution to hosts with this file in the filesystem")
	f.StringP("limit-locale", "L", "", "limit execution to hosts that match this locale")

	// Artifact details
	f.StringP("exports", "v", "StartW", "comma separated list of exports (shared format)")
	f.BoolP("run-at-load", "R", false, "run the implant entrypoint from DllMain/Constructor (shared only)")
	f.String("service-name", "", "service name to register the implant under (windows service format)")
}

// outputFormatFromFlag maps the CLI string onto clientpb.OutputFormat.
func outputFormatFromFlag(raw string, con *console.SliverClient) clientpb.OutputFormat {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "exe", "executable":
		return clientpb.OutputFormat_EXECUTABLE
	case "shared", "shared_lib", "dll", "so":
		return clientpb.OutputFormat_SHARED_LIB
	case "service":
		return clientpb.OutputFormat_SERVICE
	case "shellcode":
		return clientpb.OutputFormat_SHELLCODE
	case "archive":
		con.PrintErrorf("C/C++ implants have no Go archive format; use --format shared\n")
		return clientpb.OutputFormat_GO_ARCHIVE
	default:
		con.PrintErrorf("Invalid format %q (valid: exe, shared, service, shellcode)\n", raw)
		return clientpb.OutputFormat_EXECUTABLE
	}
}
