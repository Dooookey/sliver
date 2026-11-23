package main

// lenient.go - a tiny, tolerant protobuf field walker.
//
// The C implant copies Windows ANSI/OEM bytes into proto3 `string` fields. When
// such a value contains a non-ASCII byte (GBK service descriptions, OEM console
// output, ...) the message is no longer valid UTF-8, and a real Go server
// (protobuf-go validates UTF-8 on unmarshal) fails the whole RPC. The strict
// decode in the harness reproduces exactly that.
//
// To still be able to say *which* value is broken - and whether the rest of the
// payload is semantically correct - the harness falls back to this walker, which
// extracts the same fields without UTF-8 validation and reports the offending
// field paths.

import (
	"encoding/binary"
	"fmt"
	"unicode/utf8"
)

type pbField struct {
	num   uint64
	wire  int
	bytes []byte
	uvar  uint64
}

func pbWalk(b []byte) ([]pbField, error) {
	var out []pbField
	i := 0
	for i < len(b) {
		key, n := binary.Uvarint(b[i:])
		if n <= 0 {
			return out, fmt.Errorf("bad field key at %d", i)
		}
		i += n
		f := pbField{num: key >> 3, wire: int(key & 7)}
		switch f.wire {
		case 0:
			var v uint64
			v, n = binary.Uvarint(b[i:])
			if n <= 0 {
				return out, fmt.Errorf("bad varint at %d", i)
			}
			i += n
			f.uvar = v
		case 1:
			if i+8 > len(b) {
				return out, fmt.Errorf("truncated fixed64 at %d", i)
			}
			i += 8
		case 2:
			l, n2 := binary.Uvarint(b[i:])
			if n2 <= 0 {
				return out, fmt.Errorf("bad length at %d", i)
			}
			i += n2
			if i+int(l) > len(b) {
				return out, fmt.Errorf("truncated bytes field %d", f.num)
			}
			f.bytes = b[i : i+int(l)]
			i += int(l)
		case 5:
			if i+4 > len(b) {
				return out, fmt.Errorf("truncated fixed32 at %d", i)
			}
			i += 4
		default:
			return out, fmt.Errorf("unsupported wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out, nil
}

// lossy converts raw bytes to a displayable string, replacing invalid UTF-8
// bytes with '.' so the report never contains mojibake soup.
func lossy(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out []rune
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out = append(out, '?')
		} else {
			out = append(out, r)
		}
		b = b[size:]
	}
	return string(out)
}

// invalidUTF8Paths walks the message recursively and returns field paths whose
// value is not valid UTF-8, together with a short sample.
func invalidUTF8Paths(b []byte, path string, depth int) []string {
	if depth > 6 || len(b) == 0 {
		return nil
	}
	fields, err := pbWalk(b)
	if err != nil || len(fields) == 0 {
		return nil
	}
	var out []string
	for _, f := range fields {
		if f.wire != 2 {
			continue
		}
		p := fmt.Sprintf("%s.%d", path, f.num)
		if utf8.Valid(f.bytes) {
			continue
		}
		// A length-delimited field can be a nested message; if it parses as one
		// and its children are the problem, descend instead of blaming the blob.
		if inner := invalidUTF8Paths(f.bytes, p, depth+1); len(inner) > 0 {
			out = append(out, inner...)
			continue
		}
		out = append(out, fmt.Sprintf("%s = %q (raw %s)", p, lossy(f.bytes), hexdump(f.bytes, 24)))
	}
	return out
}

func firstInvalidUTF8(b []byte) string {
	if paths := invalidUTF8Paths(b, "", 0); len(paths) > 0 {
		return paths[0]
	}
	return ""
}

// utf8Hit points at the first non-UTF-8 string value inside a message.
type utf8Hit struct {
	path   string
	length int
	offset int
	window []byte
	tail   []byte
}

func (h *utf8Hit) String() string {
	if h == nil {
		return ""
	}
	lo := h.offset - 12
	if lo < 0 {
		lo = 0
	}
	hi := h.offset + 12
	if hi > len(h.window) {
		hi = len(h.window)
	}
	return fmt.Sprintf("字段%s: 字符串 %d 字节, 首个非法字节@%d, 附近=%s, 尾部=%s",
		h.path, h.length, h.offset, hexdump(h.window[lo:hi], 25), hexdump(h.tail, 16))
}

func findInvalidUTF8(b []byte, path string, depth int) *utf8Hit {
	if depth > 8 || len(b) == 0 {
		return nil
	}
	fields, err := pbWalk(b)
	if err != nil || len(fields) == 0 {
		return nil
	}
	for _, f := range fields {
		if f.wire != 2 || utf8.Valid(f.bytes) {
			continue
		}
		p := fmt.Sprintf("%s.%d", path, f.num)
		// Nested message? A message body is never valid UTF-8 as a blob, so only
		// blame the blob when it does not parse and contains no valid child.
		if inner := findInvalidUTF8(f.bytes, p, depth+1); inner != nil {
			return inner
		}
		if _, perr := pbWalk(f.bytes); perr == nil {
			continue // parses as a nested message with only valid strings
		}
		off := -1
		for i := 0; i < len(f.bytes); {
			r, size := utf8.DecodeRune(f.bytes[i:])
			if r == utf8.RuneError && size == 1 {
				off = i
				break
			}
			i += size
		}
		if off < 0 {
			off = 0
		}
		tail := f.bytes
		if len(tail) > 16 {
			tail = tail[len(tail)-16:]
		}
		return &utf8Hit{path: p, length: len(f.bytes), offset: off, window: f.bytes, tail: tail}
	}
	return nil
}

// --- per-message lenient extractors ---------------------------------------

type envPair struct{ key, value string }

func lenientEnv(data []byte) []envPair {
	fields, err := pbWalk(data)
	if err != nil {
		return nil
	}
	var out []envPair
	for _, f := range fields {
		if f.num != 1 || f.wire != 2 {
			continue
		}
		sub, err := pbWalk(f.bytes)
		if err != nil {
			continue
		}
		var kv envPair
		for _, s := range sub {
			switch {
			case s.num == 1 && s.wire == 2:
				kv.key = lossy(s.bytes)
			case s.num == 2 && s.wire == 2:
				kv.value = lossy(s.bytes)
			}
		}
		out = append(out, kv)
	}
	return out
}

type svcInfo struct {
	name, display, desc, binPath, account string
	status, startup                       uint64
}

func lenientService(b []byte) svcInfo {
	var s svcInfo
	fields, err := pbWalk(b)
	if err != nil {
		return s
	}
	for _, f := range fields {
		switch {
		case f.num == 1 && f.wire == 2:
			s.name = lossy(f.bytes)
		case f.num == 2 && f.wire == 2:
			s.display = lossy(f.bytes)
		case f.num == 3 && f.wire == 2:
			s.desc = lossy(f.bytes)
		case f.num == 4 && f.wire == 0:
			s.status = f.uvar
		case f.num == 5 && f.wire == 0:
			s.startup = f.uvar
		case f.num == 6 && f.wire == 2:
			s.binPath = lossy(f.bytes)
		case f.num == 7 && f.wire == 2:
			s.account = lossy(f.bytes)
		}
	}
	return s
}

func lenientServices(data []byte) []svcInfo {
	fields, err := pbWalk(data)
	if err != nil {
		return nil
	}
	var out []svcInfo
	for _, f := range fields {
		if f.num == 1 && f.wire == 2 {
			out = append(out, lenientService(f.bytes))
		}
	}
	return out
}

func lenientServiceDetail(data []byte) (svcInfo, string) {
	var s svcInfo
	msg := ""
	fields, err := pbWalk(data)
	if err != nil {
		return s, ""
	}
	for _, f := range fields {
		switch {
		case f.num == 1 && f.wire == 2:
			s = lenientService(f.bytes)
		case f.num == 2 && f.wire == 2:
			msg = lossy(f.bytes)
		}
	}
	return s, msg
}

// lenientNames reads a repeated string field 1 (Subkeys / ValueNames).
func lenientNames(data []byte) []string {
	fields, err := pbWalk(data)
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range fields {
		if f.num == 1 && f.wire == 2 {
			out = append(out, lossy(f.bytes))
		}
	}
	return out
}

// lenientStringField reads a single string field by number.
func lenientStringField(data []byte, num uint64) string {
	fields, err := pbWalk(data)
	if err != nil {
		return ""
	}
	for _, f := range fields {
		if f.num == num && f.wire == 2 {
			return lossy(f.bytes)
		}
	}
	return ""
}
