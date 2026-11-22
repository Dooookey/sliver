# Stage B

Stage B covers the capabilities that cannot be validated inside the restricted
(Low integrity) sandbox. They are implemented; verification needs an elevated
session or real credentials.

## What is implemented

| Capability | Message | Implementation |
|---|---|---|
| make-token | MakeTokenReq(64) -> MakeToken(65) | LogonUserA + ImpersonateLoggedOnUser |
| getsystem | InvokeGetSystemReq(32) -> GetSystem(33) | named pipe + ImpersonateNamedPipeClient |
| rev2self | RevToSelfReq(31) | RevertToSelf |
| services | ServicesReq(130) | EnumServicesStatusEx + QueryServiceConfig(2) |
| service detail | ServiceDetailReq(131) | per-service config/status |
| start-service-by-name | StartServiceByNameReq(132) | OpenSCManager/OpenService/StartService |

## What the sandbox proved

- bad credentials fail cleanly: `token: LogonUser(sliver-no-such-user) failed (1326)`
- the process token is Low integrity, so HKCU writes are denied to the implant
  (`RegCreateKeyEx -> 5`) while PowerShell in the same session can write HKCU.
  That is a harness token restriction, not a code path.

## How to verify Stage B

```
pwsh -File tests\scripts\stage-b-elevated.ps1 -Implant build\msvc-x64-debug\bin\implant.exe `
     -Username <user> -Password <pass> -Domain .
```

The script writes `build\stage-b-report.txt`: selftest-all output, HKCU probe,
elevation state, and the exact client commands to run for make-token / getsystem /
service start. Paste that report back to close the remaining items.

## Not implemented (needs a dedicated stage)

- spawn-dll, execute-assembly (CLR host), migrate, sideload
- registry read-hive (133) - needs SeBackupPrivilege semantics
- /W4 /WX build gate (C4456 warnings still present)
