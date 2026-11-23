# Sliver Implant-CPP 后续开发规划（v0.10.0 起）

现状：session 模式全链路可用（register/ls/cd/mkdir/rm/ps/env/upload/download/execute/shell/portfwd/ifconfig/netstat/mount/screenshot/getprivs/services/registry/make-token/rev2self/getsystem），P0/P1 缺陷清零，P2 架构 5/6。harness 全绿（含 shell chinese-ok）。

## 阶段一：架构收尾（P2 #4，1 个专注会话）

- proto.c 反向依赖 platform 头 → 把 ~14 个平台依赖编码器下沉到 `src/proto/domain_enc.c`（fs/proc/net/privs/services/registry），proto.c 只留 buf/reader/envelope + 纯 proto 编码器。
- 逐步 build 验证（crypto=ON/off、shared、harness）。

## 阶段二：协议补全（对齐 Go implant 未实现的 RPC）

| 项 | 说明 |
|---|---|
| execute Env(7) | 解析 ExecuteReq.Env 并传入子进程环境 |
| netstat UDP | 当前只枚举 TCP；补 GetExtendedUdpTable |
| whoami/token 完整字段 | CurrentTokenOwner 补 Token 类型/域/权限 |
| registry 枚举/二进制值 | reg_subkeys/values/read 的二进制 value 返回 |
| 文件校验和 | Chmod/Chtimes/Cp 的错误消息与 Go 对齐 |
| Mtls vs Wg | 现有 transport vtable 上挂 HTTP/DNS/WireGuard |

## 阶段三：beacon 模式

- 当前只有 session 循环；补 beacon（定时取任务、Sleep/Jitter、C2 profile）。
- 复用 `sliver_implant_run` 入口，加 `sliver_beacon_run` 分支。

## 阶段四：Windows 深度（P4 延续）

- 进程注入（CreateRemoteThread / APC / 进程镂空）、spawn-dll 远程注入（当前 self-load）。
- 令牌模拟完整（当前 make-token/getsystem/rev2self 基础）。
- 持久化（服务/计划任务/注册表 Run 键）。
- 反调试/反虚拟机（当前无反检测，需新增）。
- AMSI/ETW 规避（PowerShell 载荷落地）。

## 阶段五：工程化

| 项 | 说明 |
|---|---|
| CI | GitHub Actions 矩阵：msvc-x64 / mingw / wsl-gcc / crypto=OFF / shared |
| 一致性测试 | `test/cppconformance`（sliver 仓库）接入 CI，跑完整 RPC 对齐 |
| 符号/体积 | 验证 1–5 MB 静态（目标 ~512 KB），LTO/strip 配置 |
| 文档 | 每个 RPC 的 Go↔C 字段映射表补全 |

## 优先级建议

1. P2 #4（收尾，纯重构，先做避免技术债滚雪球）
2. 阶段二协议补全（用户可见功能，直接提升对 Go 的兼容度）
3. 阶段三 beacon（C2 模式完整性）
4. 阶段四 Windows 深度（opsec/持久化/注入）
5. 阶段五 CI/测试（支撑前四项稳定迭代）
