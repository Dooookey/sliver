package cppbuild

import (
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/bishopfox/sliver/protobuf/clientpb"
)

// target describes how to configure CMake for one (goos, goarch, format) tuple.
type target struct {
	// configureArgs are extra cmake -D / -G / -A arguments.
	configureArgs []string
	// artifact names to look for, in order of preference.
	names []string
	// ext is the extension appended to the collected artifact name.
	ext string
}

func (t target) extension(config *clientpb.ImplantConfig) string {
	if config.Extension != "" {
		return config.Extension
	}
	return t.ext
}

// resolveTarget maps the Go-side GOOS/GOARCH/Format onto a CMake toolchain.
//
// Host constraints are explicit on purpose: cross-compiling linux binaries from
// a Windows host needs WSL, and pretending otherwise produces confusing cmake
// failures. Mingw (windows target from a linux host) is fully supported.
func resolveTarget(goos, goarch string, format Format, opts Options) (target, error) {
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	artifact := artifactName(format)
	switch goos {
	case "windows":
		ext := ".exe"
		if format == FormatShared {
			ext = ".dll"
		}
		switch runtime.GOOS {
		case "windows":
			arch := map[string]string{
				"amd64": "x64",
				"386":   "Win32",
				"arm64": "ARM64",
			}[goarch]
			if arch == "" {
				return target{}, fmt.Errorf("cppbuild: unsupported windows arch %q", goarch)
			}
			return target{
				configureArgs: []string{"-G", "Visual Studio 17 2022", "-A", arch},
				names:         []string{artifact + ext},
				ext:           ext,
			}, nil
		case "linux":
			toolchain, err := mingwToolchain(goarch)
			if err != nil {
				return target{}, err
			}
			return target{
				configureArgs: []string{"-G", "Ninja", "-DCMAKE_TOOLCHAIN_FILE=" + filepath.Join(opts.SourceDir, "cmake", "toolchains", toolchain)},
				names:         []string{artifact + ext},
				ext:           ext,
			}, nil
		default:
			return target{}, fmt.Errorf("cppbuild: windows target from %s host is not supported", runtime.GOOS)
		}

	case "linux":
		if runtime.GOOS != "linux" {
			return target{}, fmt.Errorf("cppbuild: linux target requires a linux host (run the server inside WSL): host=%s", runtime.GOOS)
		}
		if goarch != "amd64" && goarch != "arm64" {
			return target{}, fmt.Errorf("cppbuild: unsupported linux arch %q", goarch)
		}
		if goarch != runtime.GOARCH {
			return target{}, fmt.Errorf("cppbuild: cross-arch linux builds need a dedicated toolchain (host=%s target=%s)", runtime.GOARCH, goarch)
		}
		ext := ""
		if format == FormatShared {
			ext = ".so"
		}
		return target{
			configureArgs: []string{"-G", "Ninja"},
			names:         []string{artifact + ext},
			ext:           ext,
		}, nil

	case "darwin":
		return target{}, fmt.Errorf("cppbuild: darwin targets are not supported yet")
	default:
		return target{}, fmt.Errorf("cppbuild: unsupported GOOS %q", goos)
	}
}

func artifactName(format Format) string {
	// CMake target name is "implant" for both formats.
	return "implant"
}

func mingwToolchain(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "windows-mingw.cmake", nil
	case "386":
		return "windows-mingw-386.cmake", nil
	case "arm64":
		return "windows-mingw-arm64.cmake", nil
	default:
		return "", fmt.Errorf("cppbuild: unsupported mingw arch %q", goarch)
	}
}

// locateArtifact finds the freshly built binary inside the CMake build tree.
func locateArtifact(cmakeDir string, tgt target) (string, error) {
	searchDirs := []string{
		filepath.Join(cmakeDir, "bin"),
		filepath.Join(cmakeDir, "bin", "Release"),
		filepath.Join(cmakeDir, "bin", "Debug"),
		cmakeDir,
		filepath.Join(cmakeDir, "Release"),
	}
	for _, dir := range searchDirs {
		for _, name := range tgt.names {
			candidate := filepath.Join(dir, name)
			if info, err := statFile(candidate); err == nil && info > 0 {
				return candidate, nil
			}
		}
	}
	// Fall back to a glob so renamed outputs still resolve.
	for _, dir := range searchDirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "implant*"))
		for _, m := range matches {
			if info, err := statFile(m); err == nil && info > 0 {
				return m, nil
			}
		}
	}
	return "", fmt.Errorf("cppbuild: artifact not found under %s", cmakeDir)
}
