# Full code scan: architecture stability and extensibility

Scope: 40 C sources / 10.3k lines / 18 headers, release artifact 852 KB, 41 dispatch
entries. Two read-only audits (architecture, runtime). No code changed.

## A. Findings that undermine the foundation (P0)

| # | Issue | Evidence | Impact |
|---|---|---|---|
| A1 | yamux `pump()` runs on several threads | `wire/yamux.c` write path calls pump; session thread calls pump in accept | two readers split a frame header -> corrupted stream, session death |
| A2 | stream table and `s->in` are unsynchronised; slot reclaim frees a live stream | `yamux.c` alloc_stream/release_stream | use-after-free, silent data loss |
| A3 | session teardown destroys `tx_lock`, frees yamux/tls while detached threads run; `g_tx` points at a stack frame | `modes/session.c` | UAF on reconnect; a stale thread can inject frames into the new session |
| A4 | portfwd socket double free | release closes+frees, relay thread closes the cached copy | heap corruption |
| A5 | bind-only tunnel entries never free their slot | `modes/portfwd.c`, `modes/shell.c` | after 8 reconnects shell/portfwd stop working |
| A6 | blocking socket/pipe writes on the session thread | `portfwd.c` send, `shell.c` child write | session stalls; peer declares it dead |
| A7 | `handle_spawn_dll` exists but is not in the table | `rpc/dispatch.c` | compiles, silently absent at runtime |

## B. Structural issues (extensibility)

| # | Issue | Impact |
|---|---|---|
| B1 | one `sliver_core` target for all layers | no visibility boundary; reverse deps compile fine; 10 `#if _WIN32` blocks in dispatch |
| B2 | hand-maintained dispatch table, numeric ids inline, proto.h duplicates ids (netstat/mount = req id) | 6-9 touch points per new message; drift goes unnoticed |
| B3 | no transport abstraction (`transport.h` exposes TLS only, `feature_http/dns/wg` unusable) | new transports require editing the session state machine |
| B4 | global `*_set_sender` + stack-owned `g_tx` | implicit single session; handlers untestable; detached threads dangle |
| B5 | handler return protocol is `int` (-2 = replied, other = unknown) | errors surface as UnknownMessageType; four identical async wrappers |
| B6 | header append blocks sit outside include guards / extern "C" | duplicate typedefs and C++ linkage mismatch |
| B7 | proto.c depends on platform headers; encoders declared in platform headers | platform struct layout leaks into wire format |
| B8 | no injection / CLR / tunnel abstractions; tunnels are 8-slot arrays with private locks | each new capability re-implements registry+lock+sender |
| B9 | lazy global locks (portfwd/shell/log/drbg) with no destroy; `log_init` only in debug builds | release build logs without a lock; teardown never clean |
| B10 | `sliver_plat_init` -> `log_init`(debug) -> session order is implicit | no idempotent runtime init; missing step = silent message loss |
| B11 | DLL entry cannot connect (session loop only in main.c) and never joins its thread | shared-library artifact does not work |
| B12 | `IMPLANT_CRYPTO=OFF` removes the sources main.c needs | that option cannot build |
| B13 | tests: only config/crypto/wire in ctest; Go peer never exits non-zero | new modules cannot be validated automatically |
| B14 | main.c mixes entry + 20 selftests; `run_from_build_config` failure returns 0 | test code in the product; failed startup looks successful |

## C. Recommended order

1. **P0 batch** (A1-A7): single-threaded pump + stream mutex, owned sender with refcount,
   portfwd single ownership, slot reclamation, no blocking writes on session thread,
   register the missing handler. Every item has a field symptom already seen.
2. **Test gate** (B13/B14): Go peer becomes table-driven with non-zero exit, wired into
   ctest; selftests move out of main.c behind `IMPLANT_SELFTEST`.
3. **Dispatch codegen** (B2 + B5): X-macro from sliver.proto ids, bsearch table, tri-state
   handler result, one async wrapper driven by a flag.
4. **Layering** (B1 + B6 + B7): split targets (platform/wire/transport/modes/rpc) so reverse
   dependencies fail to link; move appended header blocks inside guards; wire model header.
5. **Context object** (B4 + B10 + B11): `sliver_ctx_t` threaded through dispatch, runtime
   init/deinit, DLL entry reuses the same run loop.
6. **Abstractions** (B3 + B8): transport ops table; `inject.h` / `tunnel.h` registries.
7. **Options and polish** (B12, B9): make IMPLANT_CRYPTO and shared format build in CI.

## D. What stays as-is for now

`grep` substring semantics, English privilege descriptions, WIC screenshot encoder and
getsystem's named-pipe approach are conscious trade-offs documented in 13-status.md; the
audit did not find stability risks in them.
