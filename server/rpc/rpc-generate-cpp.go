package rpc

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

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	consts "github.com/bishopfox/sliver/client/constants"
	"github.com/bishopfox/sliver/protobuf/clientpb"
	"github.com/bishopfox/sliver/protobuf/commonpb"
	"github.com/bishopfox/sliver/server/codenames"
	"github.com/bishopfox/sliver/server/core"
	"github.com/bishopfox/sliver/server/cppbuild"
	"github.com/bishopfox/sliver/server/db"
	"github.com/bishopfox/sliver/server/db/models"
	"github.com/bishopfox/sliver/server/generate"
	"github.com/bishopfox/sliver/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GenerateCpp - Generate a C/C++ implant.
//
// This mirrors Generate() (rpc-generate.go) statement by statement: same name
// handling, same config/C2 channel resolution, the same generate.GenerateConfig
// call (so keys, certificates, WG peers and the DB record are produced by the
// official code), the same HTTP C2 profile lookup and the same
// generate.ImplantBuildSave + event publication. The only difference is the
// compilation backend: cmake/cppbuild instead of `go build`.
//
// Keeping the official path untouched means a bug in the C/C++ backend can
// never regress the Go implant generation.
func (rpc *Server) GenerateCpp(ctx context.Context, req *clientpb.GenerateReq) (*clientpb.Generate, error) {
	var (
		err    error
		config *clientpb.ImplantConfig
		name   string
	)

	if req.Name == "" {
		name, err = codenames.GetCodename()
		if err != nil {
			return nil, rpcError(err)
		}
	} else if err := util.AllowedName(req.Name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	} else {
		name = req.Name
	}

	if req.Config.ID != "" {
		// if this is a profile reuse existing configuration
		config, err = db.ImplantConfigByID(req.Config.ID)
		if err != nil {
			return nil, rpcError(err)
		}
	} else {
		// configure c2 channels to enable
		config = req.Config
		config.IncludeMTLS = config.IncludeMTLS || models.IsC2Enabled([]string{"mtls"}, config.C2)
		config.IncludeWG = config.IncludeWG || models.IsC2Enabled([]string{"wg"}, config.C2)
		config.IncludeHTTP = config.IncludeHTTP || models.IsC2Enabled([]string{"http", "https"}, config.C2)
		config.IncludeDNS = config.IncludeDNS || models.IsC2Enabled([]string{"dns"}, config.C2)
		config.IncludeNamePipe = config.IncludeNamePipe || models.IsC2Enabled([]string{"namedpipe"}, config.C2)
		config.IncludeTCP = config.IncludeTCP || models.IsC2Enabled([]string{"tcppivot"}, config.C2)
	}

	if len(config.Exports) == 0 {
		config.Exports = []string{"StartW"}
	}

	// Official key/cert/WG material + DB record; unchanged.
	build, err := generate.GenerateConfig(name, config)
	if err != nil {
		return nil, rpcError(err)
	}

	// retrieve http c2 implant config
	httpC2Config, err := db.LoadHTTPC2ConfigByName(req.Config.HTTPC2ConfigName)
	if err != nil {
		return nil, rpcError(err)
	}

	var fPath string
	switch req.Config.Format {
	case clientpb.OutputFormat_SERVICE:
		fallthrough
	case clientpb.OutputFormat_EXECUTABLE:
		fPath, err = cppbuild.CompileExecutable(name, build, config, httpC2Config.ImplantConfig)
	case clientpb.OutputFormat_SHARED_LIB:
		fPath, err = cppbuild.CompileSharedLibrary(name, build, config, httpC2Config.ImplantConfig)
	case clientpb.OutputFormat_SHELLCODE:
		fPath, err = cppbuild.CompileShellcode(name, build, config, httpC2Config.ImplantConfig)
	case clientpb.OutputFormat_GO_ARCHIVE:
		err = status.Error(codes.Unimplemented, "C/C++ implants have no c-archive format; use SHARED_LIB")
	default:
		err = status.Error(codes.InvalidArgument, fmt.Sprintf("invalid output format: %s", req.Config.Format))
	}
	if err != nil {
		return nil, rpcError(err)
	}

	fileName := filepath.Base(fPath)
	fileData, err := os.ReadFile(fPath)
	if err != nil {
		return nil, rpcError(err)
	}

	// Must happen before returning: the server only recognises an implant that
	// has a matching ImplantBuild row (peer key digest lookup).
	err = generate.ImplantBuildSave(build, config, fPath)
	if err != nil {
		rpcLog.Errorf("Failed to save C++ implant build: %s", err)
		return nil, rpcError(err)
	}

	core.EventBroker.Publish(core.Event{
		EventType: consts.BuildCompletedEvent,
		Data:      []byte(fileName),
	})

	return &clientpb.Generate{
		File: &commonpb.File{
			Name: fileName,
			Data: fileData,
		},
		ImplantName:    build.Name,
		ImplantBuildID: build.ID,
	}, nil
}
