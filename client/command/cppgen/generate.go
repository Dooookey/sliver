package cppgen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bishopfox/sliver/client/command/generate"
	"github.com/bishopfox/sliver/client/console"
	"github.com/bishopfox/sliver/protobuf/clientpb"
	"github.com/spf13/cobra"
)

// GenerateCppCmd builds an ImplantConfig from the same flags the official
// `generate` command exposes (subset that the C/C++ runtime supports), then
// hands it to the GenerateCpp RPC. All target/limits/transport semantics match
// upstream because the C2 URI parsing and the units are shared with it.
func GenerateCppCmd(cmd *cobra.Command, con *console.SliverClient, args []string) {
	name, config, ok := parseCppFlags(cmd, con)
	if !ok {
		return
	}

	save, _ := cmd.Flags().GetString("save")
	if save == "" {
		save, _ = os.Getwd()
	}

	if config.IsBeacon {
		con.PrintInfof("Generating new %s/%s C/C++ beacon implant (%v interval)\n",
			config.GOOS, config.GOARCH, time.Duration(config.BeaconInterval))
	} else {
		con.PrintInfof("Generating new %s/%s C/C++ implant (%s)\n",
			config.GOOS, config.GOARCH, config.Format)
	}

	start := time.Now()
	ctrl := make(chan bool)
	con.SpinUntil("Compiling with cmake, please wait ...", ctrl)

	generated, err := con.Rpc.GenerateCpp(context.Background(), &clientpb.GenerateReq{
		Name:   name,
		Config: config,
	})
	ctrl <- true
	<-ctrl
	if err != nil {
		con.PrintErrorf("%s\n", err)
		return
	}
	if generated.GetFile() == nil || len(generated.GetFile().GetData()) == 0 {
		con.PrintErrorf("Build failed, no file data\n")
		return
	}

	data := generated.GetFile().GetData()
	saveTo, err := resolveSavePath(save, generated.GetFile().GetName())
	if err != nil {
		con.PrintErrorf("%s\n", err)
		return
	}
	if err := os.WriteFile(saveTo, data, 0o700); err != nil {
		con.PrintErrorf("Failed to write to %s: %s\n", saveTo, err)
		return
	}

	con.PrintInfof("Build completed in %s\n", time.Since(start).Round(time.Second))
	con.PrintInfof("Implant saved to %s (%d bytes)\n", saveTo, len(data))
	if !config.Debug {
		con.PrintInfof("Artifact is expected to be statically linked; the server rejects >5 MiB builds\n")
	}
}

// parseCppFlags mirrors the relevant part of generate.parseCompileFlags.
func parseCppFlags(cmd *cobra.Command, con *console.SliverClient) (string, *clientpb.ImplantConfig, bool) {
	flags := cmd.Flags()

	name, _ := flags.GetString("name")
	name = strings.ToLower(strings.TrimSpace(name))

	goos, _ := flags.GetString("os")
	goos = strings.ToLower(goos)
	if goos != "windows" && goos != "linux" {
		con.PrintErrorf("C/C++ implants support windows and linux (got %q)\n", goos)
		return "", nil, false
	}

	goarch, _ := flags.GetString("arch")
	formatFlag, _ := flags.GetString("format")
	format := outputFormatFromFlag(formatFlag, con)

	c2s := []*clientpb.ImplantC2{}
	parsers := []struct {
		flag  string
		parse func(string) ([]*clientpb.ImplantC2, error)
	}{
		{"mtls", generate.ParseMTLSc2},
		{"wg", generate.ParseWGc2},
		{"http", generate.ParseHTTPc2},
		{"dns", generate.ParseDNSc2},
		{"named-pipe", generate.ParseNamedPipec2},
		{"tcp-pivot", generate.ParseTCPPivotc2},
	}
	counts := map[string]int{}
	for _, p := range parsers {
		raw, _ := flags.GetString(p.flag)
		parsed, err := p.parse(raw)
		if err != nil {
			con.PrintErrorf("%s\n", err)
			return "", nil, false
		}
		counts[p.flag] = len(parsed)
		c2s = append(c2s, parsed...)
	}
	if len(c2s) == 0 {
		con.PrintErrorf("Must specify at least one of --mtls, --wg, --http, --dns, --named-pipe, or --tcp-pivot\n")
		return "", nil, false
	}

	debug, _ := flags.GetBool("debug")
	debugFile, _ := flags.GetString("debug-file")
	strategy, _ := flags.GetString("strategy")
	reconnect, _ := flags.GetInt64("reconnect")
	pollTimeout, _ := flags.GetInt64("poll-timeout")
	maxErrors, _ := flags.GetUint32("max-errors")
	c2Profile, _ := flags.GetString("c2profile")
	keyExchange, _ := flags.GetUint32("key-exchange")
	tcpComms, _ := flags.GetUint32("tcp-comms")
	allProtocols, _ := flags.GetBool("all-protocols")
	beacon, _ := flags.GetBool("beacon")
	interval, _ := flags.GetInt64("interval")
	jitter, _ := flags.GetInt64("jitter")
	exportsRaw, _ := flags.GetString("exports")
	runAtLoad, _ := flags.GetBool("run-at-load")
	serviceName, _ := flags.GetString("service-name")

	limitDatetime, _ := flags.GetString("limit-datetime")
	limitDomainJoined, _ := flags.GetBool("limit-domainjoined")
	limitUsername, _ := flags.GetString("limit-username")
	limitHostname, _ := flags.GetString("limit-hostname")
	limitFileExists, _ := flags.GetString("limit-fileexists")
	limitLocale, _ := flags.GetString("limit-locale")

	exports := []string{}
	for _, e := range strings.Split(exportsRaw, ",") {
		if trimmed := strings.TrimSpace(e); trimmed != "" {
			exports = append(exports, trimmed)
		}
	}
	if len(exports) == 0 {
		exports = []string{"StartW"}
	}

	config := &clientpb.ImplantConfig{
		GOOS:                goos,
		GOARCH:              goarch,
		Format:              format,
		IsBeacon:            beacon,
		BeaconInterval:      interval * int64(time.Second),
		BeaconJitter:        jitter * int64(time.Second),
		ReconnectInterval:   reconnect * int64(time.Second),
		PollTimeout:         pollTimeout * int64(time.Second),
		MaxConnectionErrors: maxErrors,
		Debug:               debug,
		DebugFile:           debugFile,
		ConnectionStrategy:  strategy,
		C2:                  c2s,
		HTTPC2ConfigName:    c2Profile,
		WGKeyExchangePort:   keyExchange,
		WGTcpCommsPort:      tcpComms,
		CanaryDomains:       nil,
		Exports:             exports,
		RunAtLoad:           runAtLoad,
		ServiceName:         serviceName,

		IncludeMTLS:     counts["mtls"] > 0 || allProtocols,
		IncludeWG:       counts["wg"] > 0 || allProtocols,
		IncludeHTTP:     counts["http"] > 0 || allProtocols,
		IncludeDNS:      counts["dns"] > 0 || allProtocols,
		IncludeNamePipe: counts["named-pipe"] > 0 || allProtocols,
		IncludeTCP:      counts["tcp-pivot"] > 0 || allProtocols,

		LimitDatetime:     limitDatetime,
		LimitDomainJoined: limitDomainJoined,
		LimitUsername:     limitUsername,
		LimitHostname:     limitHostname,
		LimitFileExists:   limitFileExists,
		LimitLocale:       limitLocale,
	}

	switch format {
	case clientpb.OutputFormat_SHARED_LIB:
		config.IsSharedLib = true
	case clientpb.OutputFormat_SHELLCODE:
		config.IsShellcode = true
	case clientpb.OutputFormat_SERVICE:
		config.IsService = true
	}
	if goos == "windows" && format != clientpb.OutputFormat_EXECUTABLE {
		con.PrintWarnf("C/C++ implant: Windows shared/service artifacts are experimental in P0\n")
	}

	return name, config, true
}

// resolveSavePath mirrors the official output path behaviour: an existing
// directory means "save inside it", anything else is taken as the file name.
func resolveSavePath(save, remoteName string) (string, error) {
	info, err := os.Stat(save)
	if err == nil && info.IsDir() {
		return filepath.Join(save, filepath.Base(remoteName)), nil
	}
	if err == nil && !info.IsDir() {
		return save, nil
	}
	if os.IsNotExist(err) {
		// Treat a path without extension as a directory to create.
		if filepath.Ext(save) == "" {
			if mkErr := os.MkdirAll(save, 0o700); mkErr != nil {
				return "", fmt.Errorf("cannot create %s: %w", save, mkErr)
			}
			return filepath.Join(save, filepath.Base(remoteName)), nil
		}
		return save, nil
	}
	return "", errors.New("invalid save path: " + save)
}
