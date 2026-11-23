//go:build !windows

package main

// Non-Windows stub: the implant this suite drives is a Windows PE, so these
// helpers only exist to keep the package buildable elsewhere.

func selfIntegrity() string { return "n/a (non-windows host)" }

func regCreateProbe(string) error { return nil }
