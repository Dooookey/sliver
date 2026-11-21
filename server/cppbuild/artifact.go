package cppbuild

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"strings"
)

// statFile returns the file size, or an error if it is not a regular non-empty file.
func statFile(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return 0, errors.New("is a directory")
	}
	return info.Size(), nil
}

// ValidateArtifact enforces the two hard requirements for a C/C++ implant:
//  1. it is a PE/ELF image of the requested shape;
//  2. it is statically linked (no dynamic loader, no VC runtime dependency);
//  3. it fits the size budget (<= 5 MiB hard limit, target <= 512 KiB).
func ValidateArtifact(path string, format Format) error {
	size, err := statFile(path)
	if err != nil {
		return fmt.Errorf("cppbuild: artifact %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cppbuild: read artifact: %w", err)
	}

	if err := checkImage(path, data, format); err != nil {
		return err
	}
	if err := checkStatic(path, data); err != nil {
		return err
	}

	if size > MaxImplantSizeBytes {
		return fmt.Errorf("cppbuild: artifact %s is %d bytes, exceeds hard limit of %d bytes",
			path, size, MaxImplantSizeBytes)
	}
	if size > TargetImplantSizeBytes {
		cppLog.Warnf("artifact %s is %d bytes (target: <= %d bytes); check IMPLANT_LTO/IMPLANT_STRIP",
			path, size, TargetImplantSizeBytes)
	}
	cppLog.Infof("implant artifact: %s (%d bytes)", path, size)
	return nil
}

func checkImage(path string, data []byte, format Format) error {
	switch {
	case len(data) >= 2 && data[0] == 'M' && data[1] == 'Z':
		// PE: a DLL must export its entry symbol.
		if format == FormatShared && !bytes.Contains(data, []byte("StartW")) {
			return fmt.Errorf("cppbuild: %s looks like a DLL but does not export StartW", path)
		}
		return nil
	case len(data) >= 4 && data[0] == 0x7f && data[1] == 'E' && data[2] == 'L' && data[3] == 'F':
		return nil
	default:
		if el, err := elf.Open(path); err == nil {
			_ = el.Close()
			return nil
		}
		return fmt.Errorf("cppbuild: %s is neither PE nor ELF", path)
	}
}

// checkStatic rejects dynamically linked artifacts.
//
//   - ELF: presence of PT_INTERP means the loader is required.
//   - PE: presence of the MSVC/UCRT runtime DLL names means /MT was not used,
//     or MinGW linked against the shared libgcc/msvcrt runtime.
func checkStatic(path string, data []byte) error {
	if len(data) >= 4 && data[0] == 0x7f && data[1] == 'E' && data[2] == 'L' && data[3] == 'F' {
		f, err := elf.Open(path)
		if err != nil {
			return fmt.Errorf("cppbuild: open elf: %w", err)
		}
		defer f.Close()
		for _, prog := range f.Progs {
			if prog.Type == elf.PT_INTERP {
				return fmt.Errorf("cppbuild: %s is dynamically linked (PT_INTERP present); link with -static/musl", path)
			}
		}
		return nil
	}
	// PE: crude but effective import-name scan.
	for _, runtime := range []string{"VCRUNTIME140", "MSVCP140", "ucrtbase.dll", "api-ms-win-crt", "libgcc_s_", "libwinpthread"} {
		if bytes.Contains(data, []byte(runtime)) {
			return fmt.Errorf("cppbuild: %s imports %s; rebuild with static CRT (-DIMPLANT_STATIC_CRT=ON)", path, runtime)
		}
	}
	return nil
}

// SizeReport is a convenience for CLI output and tests.
func SizeReport(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	kb := float64(info.Size()) / 1024.0
	status := "ok"
	switch {
	case info.Size() > MaxImplantSizeBytes:
		status = "OVER LIMIT"
	case info.Size() > TargetImplantSizeBytes:
		status = "above target"
	}
	return fmt.Sprintf("%s: %.1f KiB (%s)", path, kb, status)
}

// stripCRLF keeps log lines tidy when cmake emits CRLF on Windows.
func stripCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "")
}
