# Stage B results (field tests)

## make-token

- success: `Successfully impersonated \dell`, integrity Low -> Medium, 5 privileges
  visible while impersonating, back to Low after rev2self.
- fixes found by the field runs:
  1. LogonType from the client was passed straight to LogonUser -> Win32 1367
     (invalid logon type). Now validated: unknown types fall back to INTERACTIVE.
  2. Impersonation + execute crashed the implant: GetStdHandle returned NULL and
     CreateProcess got it with STARTF_USESTDHANDLES. exec.c now substitutes NUL.
  3. getprivs under impersonation: process token is not openable, the thread token
     is identification-level and refuses TokenPrivileges, and LookupPrivilegeNameA
     fails for its LUIs. Resolution: thread token first, process-token fallback,
     keep every row, and resolve well-known LUIs locally (34 entries).

## getsystem

- The client reads a per-implant `.bin` before sending InvokeGetSystemReq; with the
  C implant that asset does not exist, so the command failed locally
  (`NotFound ... DIFFICULT_TROUT.bin`).
- The implant side had a worse bug: ConnectNamedPipe(NULL) blocked the session and
  the client saw "implant timeout". Fixed with FILE_FLAG_OVERLAPPED + 3 s wait, so
  the command now fails cleanly when no SYSTEM client connects.

## Verified matrix

| Capability | Evidence |
|---|---|
| make-token | impersonation success, integrity change |
| rev2self | integrity returns to Low |
| execute under impersonation | returns desktop-v3av2pg\dell |
| getprivs | table renders in both states |
| registry write/create/delete | elevated selftest: create/write/delete ok |
| services / service detail / start | 64 services listed, rev2self ok |
| screenshot | PNG 229-260 KB, SOI validated |
| ifconfig / netstat / mount | 8 interfaces / 40-64 sockets / C-F volumes |

## Still open

- getsystem success path (needs SeImpersonatePrivilege + SYSTEM pipe client)
- spawn-dll, migrate, sideload, execute-assembly (CLR host)
- read-hive (implemented now, needs SeBackupPrivilege to verify)
- /W4 /WX gate (C4456 warnings remain)
