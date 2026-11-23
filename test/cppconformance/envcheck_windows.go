//go:build windows

package main

// envcheck_windows.go - environment attribution helpers.
//
// The suite must be able to say "this failed because the audit sandbox denies
// it" rather than "the implant is broken". Both probes run in the *harness*
// process token, which is the token the implant inherits.

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modadvapi32 = syscall.NewLazyDLL("advapi32.dll")

	procRegCreateKeyExA = modadvapi32.NewProc("RegCreateKeyExA")
	procRegCloseKey     = modadvapi32.NewProc("RegCloseKey")
	procRegDeleteKeyA   = modadvapi32.NewProc("RegDeleteKeyA")
)

const (
	keyAllAccess    = 0xF003F
	hkeyCurrentUser = 0x80000001
)

type tokenMandatoryLabel struct {
	Label struct {
		Sid        *windows.SID
		Attributes uint32
	}
}

// selfIntegrity reports the integrity level of the harness process token, which
// is the token every child (including the implant) inherits.
func selfIntegrity() string {
	tok := windows.GetCurrentProcessToken()
	var size uint32
	_ = windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, nil, 0, &size)
	if size == 0 {
		return "unknown (no size)"
	}
	buf := make([]byte, size)
	if err := windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, &buf[0], size, &size); err != nil {
		return fmt.Sprintf("unknown (%v)", err)
	}
	label := (*tokenMandatoryLabel)(unsafe.Pointer(&buf[0]))
	sid := label.Label.Sid
	if sid == nil || !sid.IsValid() {
		return "unknown (bad sid)"
	}
	n := sid.SubAuthorityCount()
	if n == 0 {
		return "unknown (no subauth)"
	}
	rid := sid.SubAuthority(uint32(n - 1))
	name := "Untrusted"
	switch {
	case rid >= 0x4000:
		name = "System"
	case rid >= 0x3000:
		name = "High"
	case rid >= 0x2000:
		name = "Medium"
	case rid >= 0x1000:
		name = "Low"
	}
	return fmt.Sprintf("%s(S-1-16-%d)", name, rid)
}

// regCreateProbe tries the exact registry operation the implant performs, with
// the harness's own token. A failure here means the suite cannot attribute a
// registry write failure to the implant.
func regCreateProbe(subkey string) error {
	var h uintptr
	sub, err := syscall.BytePtrFromString(subkey)
	if err != nil {
		return err
	}
	var disp uint32
	rc, _, _ := procRegCreateKeyExA.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(sub)), 0, 0, 0,
		keyAllAccess, 0, uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&disp)))
	if rc != 0 {
		return fmt.Errorf("RegCreateKeyExA(HKCU, %s) -> %d", subkey, rc)
	}
	procRegCloseKey.Call(h)
	procRegDeleteKeyA.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(sub)))
	return nil
}
