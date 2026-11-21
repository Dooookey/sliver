package cppbuild

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

// Package cppbuild is the CMake build backend for C/C++ implants.
//
// It is deliberately a drop-in replacement for the generate.Sliver* family: the
// same inputs (*clientpb.ImplantConfig / *clientpb.ImplantBuild /
// *clientpb.HTTPC2ImplantConfig) and the same output (path to the artifact
// inside the slivers directory), so the official Go generation path stays
// untouched and server/rpc/rpc-generate-cpp.go can mirror rpc-generate.go
// one-to-one.
//
// The package intentionally does not import server/generate or server/assets:
// it only needs the slivers directory layout, which is reproduced here, so the
// backend can be unit tested without the embedded assets being generated.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bishopfox/sliver/protobuf/clientpb"
	"github.com/bishopfox/sliver/server/log"
)

var (
	cppLog = log.NamedLogger("cppbuild", "generate")
)

const (
	// MaxImplantSizeBytes is a hard failure threshold. Anything above this means
	// a build configuration mistake (debug build, dynamic runtime, LTO off).
	MaxImplantSizeBytes = 5 << 20
	// TargetImplantSizeBytes is the "KB 级" goal: builds above it are logged as
	// warnings so size regressions are visible in the server console and in CI.
	TargetImplantSizeBytes = 512 << 10

	envSourceDir  = "SLIVER_CPP_SRC"
	envCMake      = "SLIVER_CMAKE"
	envSliversDir = "SLIVER_SLIVERS_DIR"
)

// Format is the artifact flavour requested by the operator.
type Format string

const (
	// FormatExe is a standalone executable.
	FormatExe Format = "exe"
	// FormatShared is a DLL / .so.
	FormatShared Format = "shared"
)

// Options overrides build environment discovery (used by tests and by the
// server config file in later phases).
type Options struct {
	SourceDir string // implant-cpp source tree
	CMake     string // cmake binary
	BuildType string // Release (default) or Debug
	Jobs      int    // parallel build jobs, 0 = cmake default
}

// CompileExecutable mirrors generate.SliverExecutable.
func CompileExecutable(name string, build *clientpb.ImplantBuild, config *clientpb.ImplantConfig, httpC2 *clientpb.HTTPC2ImplantConfig) (string, error) {
	return compile(FormatExe, Options{}, name, build, config, httpC2)
}

// CompileSharedLibrary mirrors generate.SliverSharedLibrary.
func CompileSharedLibrary(name string, build *clientpb.ImplantBuild, config *clientpb.ImplantConfig, httpC2 *clientpb.HTTPC2ImplantConfig) (string, error) {
	return compile(FormatShared, Options{}, name, build, config, httpC2)
}

// CompileShellcode mirrors generate.SliverShellcode. The C toolchain produces a
// PE/ELF first; the conversion step (donut/malasada equivalent) is wired in a
// later milestone, so this returns an explicit error instead of a fake artifact.
func CompileShellcode(name string, build *clientpb.ImplantBuild, config *clientpb.ImplantConfig, httpC2 *clientpb.HTTPC2ImplantConfig) (string, error) {
	return "", fmt.Errorf("cppbuild: shellcode format is not wired yet (exe/shared only)")
}

func compile(format Format, opts Options, name string, build *clientpb.ImplantBuild, config *clientpb.ImplantConfig, httpC2 *clientpb.HTTPC2ImplantConfig) (string, error) {
	if config == nil || build == nil {
		return "", errors.New("cppbuild: nil implant config or build")
	}
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return "", err
	}

	tgt, err := resolveTarget(config.GOOS, config.GOARCH, format, opts)
	if err != nil {
		return "", err
	}

	buildDir := filepath.Join(sliversDir(), config.GOOS, config.GOARCH, name)
	binDir := filepath.Join(buildDir, "bin")
	cmakeDir := filepath.Join(buildDir, "build")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return "", fmt.Errorf("cppbuild: create bin dir: %w", err)
	}
	if err := os.MkdirAll(cmakeDir, 0700); err != nil {
		return "", fmt.Errorf("cppbuild: create build dir: %w", err)
	}

	cfgJSON := filepath.Join(buildDir, "config.json")
	if err := WriteConfigJSON(cfgJSON, name, build, config, httpC2); err != nil {
		return "", fmt.Errorf("cppbuild: write config: %w", err)
	}
	cppLog.Infof("C++ implant config: %s", cfgJSON)

	configureArgs := []string{
		"-S", opts.SourceDir,
		"-B", cmakeDir,
		"-DCMAKE_BUILD_TYPE=" + opts.BuildType,
		"-DSLIVER_CONFIG_JSON=" + cfgJSON,
		"-DIMPLANT_FORMAT=" + string(format),
		"-DIMPLANT_DEBUG_LOG=" + boolFlag(config.Debug),
		"-DIMPLANT_GUI=" + boolFlag(isGUI(config)),
		"-DBUILD_TESTING=OFF",
	}
	configureArgs = append(configureArgs, tgt.configureArgs...)

	if err := runCMake(opts.CMake, cmakeDir, configureArgs); err != nil {
		return "", err
	}
	buildArgs := []string{"--build", cmakeDir, "--config", opts.BuildType}
	if opts.Jobs > 0 {
		buildArgs = append(buildArgs, "--parallel", fmt.Sprintf("%d", opts.Jobs))
	}
	if err := runCMake(opts.CMake, cmakeDir, buildArgs); err != nil {
		return "", err
	}

	src, err := locateArtifact(cmakeDir, tgt)
	if err != nil {
		return "", err
	}
	finalName := name + tgt.extension(config)
	fPath := filepath.Join(binDir, finalName)
	if err := copyFile(src, fPath); err != nil {
		return "", fmt.Errorf("cppbuild: collect artifact: %w", err)
	}
	if err := ValidateArtifact(fPath, format); err != nil {
		return "", err
	}
	return fPath, nil
}

func (o Options) withDefaults() Options {
	if o.SourceDir == "" {
		o.SourceDir = os.Getenv(envSourceDir)
	}
	if o.SourceDir == "" {
		o.SourceDir = defaultSourceDir()
	}
	if o.CMake == "" {
		o.CMake = os.Getenv(envCMake)
	}
	if o.CMake == "" {
		o.CMake = "cmake"
	}
	if o.BuildType == "" {
		o.BuildType = "Release"
	}
	return o
}

func (o Options) validate() error {
	info, err := os.Stat(o.SourceDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("cppbuild: implant-cpp source dir not found: %s (set %s)", o.SourceDir, envSourceDir)
	}
	if _, err := os.Stat(filepath.Join(o.SourceDir, "CMakeLists.txt")); err != nil {
		return fmt.Errorf("cppbuild: %s is not a CMake project", o.SourceDir)
	}
	if _, err := exec.LookPath(o.CMake); err != nil {
		return fmt.Errorf("cppbuild: cmake not found in PATH: %w", err)
	}
	return nil
}

// sliversDir reproduces generate.GetSliversDir() without importing the assets
// package (which requires the generated embedded FS).
func sliversDir() string {
	if dir := os.Getenv(envSliversDir); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".sliver", "slivers")
}

// defaultSourceDir looks for implant-cpp next to the working directory (server
// runs from the repo root in dev) and next to the executable (installed server).
func defaultSourceDir() string {
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "implant-cpp"),
			filepath.Join(cwd, "..", "implant-cpp"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "implant-cpp"),
			filepath.Join(dir, "..", "implant-cpp"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(filepath.Join(c, "CMakeLists.txt")); err == nil && !info.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return "implant-cpp"
}

func boolFlag(v bool) string {
	if v {
		return "ON"
	}
	return "OFF"
}

// isGUI maps the Go side "hide console window" behaviour (-H=windowsgui) onto
// the CMake option.
func isGUI(config *clientpb.ImplantConfig) bool {
	return config.GOOS == "windows" && !config.Debug
}

// runCMake executes cmake and streams combined output to the server log.
func runCMake(cmakeBin, dir string, args []string) error {
	cmd := exec.Command(cmakeBin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if os.Getenv("SLIVER_CPP_VERBOSE") == "1" {
		// Debug aid: mirror cmake output to stderr (tests, CI, local debugging).
		_, _ = os.Stderr.Write(out)
	}
	if err != nil {
		tail := string(out)
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		cppLog.Errorf("cmake %s failed: %s\n%s", strings.Join(args, " "), err, tail)
		return fmt.Errorf("cppbuild: cmake %s: %w", args[0], err)
	}
	cppLog.Debugf("cmake %s ok", strings.Join(args, " "))
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0700)
}

func init() {
	// Surface obviously wrong host/target combinations early instead of letting
	// cmake fail with a cryptic toolchain error.
	if runtime.GOOS == "darwin" {
		cppLog.Warnf("C++ implant builds on darwin hosts are not supported yet")
	}
}
