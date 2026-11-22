# C/C++ implant - status overview

Last updated at commit 962db82 (implant) / 36aa10e0 (sliver).

## 1. Transport and session

| Item | State |
|---|---|
| TLS 1.3 mTLS (mbedTLS, vendored + empty-issuer patch) | implemented |
| MUX/1 preface, hashicorp yamux client (odd stream ids, windows, write lock) | implemented |
| Frame: [minisign sig][u32 len][protobuf] | implemented |
| Register / heartbeat / reconnect loop | implemented |
| Build-time config injection (config.json -> generated header) | implemented |
| Debug vs release behaviour (console+file logs vs silent) | implemented |
| Debug-only selftests (--selftest-all and per-module) | implemented |
| HTTP / DNS / WireGuard / pivot transports | not implemented |
| Beacon (async task) mode | not implemented |

## 2. RPC surface

Implemented (id in parentheses):

- files: ls(5) pwd(12) cd(11) rm(14) mkdir(16) cp(127) mv(98) chmod(109)
  chtimes(113) download(7) upload(9) grep(129)
- process/exec: ps(18) terminate(45) env(66) execute(44, sync+async)
  execute-children(136) shell(20) shell-resize(135)
- tunnels: tunnel-data(22) tunnel-close(23) portfwd(80)
- info: ifconfig(42) netstat(49) mount(134) screenshot(47) getprivs(87)
  current-token-owner(100)
- privilege: rev2self(31) make-token(64) getsystem(32)
- services: services(130) service-detail(131) start-service-by-name(132)
- registry: read(71) write(72) create(73) subkeys(88) values(89) delete(97)
  read-hive(133)
- protocol: UnknownMessageType reply for unimplemented ids

Not implemented:

- spawn-dll remote injection (self-load only), migrate, sideload
- execute-assembly / in-proc assembly / BOF (CLR host)
- memfiles (Go registers them on Linux only; this fork has no MemfilesList message)
- pivot / rportfwd / socks5 / WireGuard
- extensions (WASM), shikata-ga-nai, msi/service install helpers

## 3. Known limitations

| Area | Limitation |
|---|---|
| grep | substring match, no regexp; LinesBefore/After capped at 3; Recursive parsed but single file |
| getprivs | descriptions + LUID names come from built-in tables (English), not localized |
| yamux | FIN/RST handling minimal; no resend/reorder buffer |
| portfwd | protocol aligned with Go; end-to-end relay confirmed on the test peer, real-server client confirmation still pending |
| getsystem | failure path only (needs SeImpersonatePrivilege + SYSTEM pipe client) |
| read-hive | needs SeBackupPrivilege |
| build | /W4 /WX not enabled (C4456-class warnings remain) |
| size | release ~880 KiB vs 512 KiB target |

## 4. Extension points

1. Transports: HTTP(S), DNS, WireGuard, pivot; beacon task queue on top of the
   existing dispatch (async workers already exist).
2. Injection: remote spawn-dll (OpenProcess/VirtualAllocEx/CreateRemoteThread),
   migrate, sideload, then CLR hosting for execute-assembly and BOF.
3. Linux parity: the RPC layer is already portable; add the Linux-only handlers
   (memfiles, /proc based ps refinements).
4. Stealth: AMSI/ETW patching, sleep obfuscation, indirect syscalls, PPID spoofing
   - all sit behind the existing `execute`/`spawn` abstractions.
5. Size: link-time optimisation, drop unused mbedTLS modules, replace WIC with a
   compact PNG writer, then revisit the 512 KiB target.
6. Test depth: protocol fuzzing on the frame/proto decoders, yamux conformance
   against the Go client, and a CI job that runs --selftest-all in a VM.

## 5. Verification snapshot

- ctest 3/3 (config, crypto KAT, wire vectors)
- --selftest-all PASS: privs, screenshot, registry, services, token, netstat,
  mount, ifconfig, grep, children, fs, spawn
- field-verified: register, ls/pwd/cd/mkdir/rm/ps/env, upload/download,
  execute (sync/async), execute-children, shell (UTF-8), portfwd request path,
  ifconfig/netstat/mount, screenshot (PNG), getprivs (incl. impersonation
  Low->Medium->Low), services, registry read/write/subkeys, make-token, rev2self
