# Stage C

## Delivered

| Item | State |
|---|---|
| spawn-dll step 1 (self-load) | done: LoadLibraryA + optional zero-arg export, `--dll-selftest` loads kernel32 |
| registry read-hive (133) | done: RegSaveKeyEx -> temp file -> bytes, clear error without SeBackupPrivilege |
| placeholder `bin/<name>.bin` | done: written next to the artifact so client-side paths that expect the asset keep working (the C implant answers the RPC itself) |
| stage B field results | docs/refactor/11-stage-b-results.md |

## Not delivered, and why

| Item | Reason |
|---|---|
| spawn-dll remote injection | needs a live target process; a wrong CreateRemoteThread entry point kills the target, so it is a separate verified step |
| migrate | built on top of remote injection |
| sideload / execute-assembly | CLR host / reflective loader, same verification constraint |
| /W4 /WX gate | enabling it now fails the build on remaining C4456-style warnings; the plan is to clear them file by file first |

## Next verification (needs the operator)

1. start notepad.exe, note the pid
2. run spawn-dll against it and report the client output
3. if it succeeds, migrate follows the same code path
