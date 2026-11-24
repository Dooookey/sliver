# Sliver Implant-CPP 后续开发规划（v0.11.0+，阶段一已收口）

现状：session 模式全链路可用（register/ls/cd/mkdir/rm/ps/env/upload/download/execute/shell/portfwd/ifconfig/netstat/tcp+udp/mount/screenshot/getprivs/services/registry/make-token/rev2self/getsystem）。P0/P1/P2 全量收口，阶段一（协议补全）5/5 完成。harness 全绿（含 shell chinese-ok、big-output-ok）。exe/DLL 双产物 + crypto=ON/OFF 双配置均构建通过。

## 阶段一：协议补全（✅ 已完成，v0.11.0）

| # | 项 | 状态 |
|---|---|---|
| 1.1 | execute Env(7) | ✅ |
| 1.2 | netstat UDP | ✅ |
| 1.3 | token 完整字段 | ✅（核查已实现） |
| 1.4 | registry 二进制值 | ✅（核查已实现） |
| 1.5 | 错误消息对齐 | ✅ |

## 阶段二：beacon 模式（C2 模式完整性）

目标：补 beacon（定时取任务 + Sleep/Jitter + C2 profile），session/beacon 两种模式齐全。

| # | 项 | 落点 |
|---|---|---|
| 2.1 | beacon 循环 | `sliver_implant_run` 加 `sliver_beacon_run` 分支 |
| 2.2 | Sleep/Jitter | 复用 `sliver_config_*` 的 beacon 参数 |
| 2.3 | 任务轮询 | BeaconTasks(94) 拉取 + 批量回传 |

里程碑 tag：`v0.12.0`（beacon）。

## 阶段三：Windows 深度（P4 延续，opsec/持久化/注入）

目标：补齐对抗与持久化能力（当前 make-token/getsystem/rev2self 是基础版）。

| # | 项 | 落点 |
|---|---|---|
| 3.1 | 进程注入 | `dllload.c` 补 CreateRemoteThread / APC / 进程镂空 |
| 3.2 | spawn-dll 远程注入 | 当前 self-load → 注入指定 PID |
| 3.3 | 持久化 | 服务 / 计划任务 / 注册表 Run 键 |
| 3.4 | 反调试/反虚拟 | 新增检测模块（当前无） |
| 3.5 | AMSI/ETW 规避 | PowerShell 载荷落地前 patch |

里程碑 tag：`v0.13.0`（Windows 深度）。

## 阶段四：多传输（利用已有 vtable）

目标：在 `sliver_transport_t` vtable 上挂新传输，摆脱单一 mtls。

| # | 项 | 说明 |
|---|---|---|
| 4.1 | HTTP(S) | `http_transport.c` 实现 read/write/close/version/ciphersuite |
| 4.2 | DNS | 数据经 DNS 隧道（优先级低） |
| 4.3 | WireGuard | 复用 Go 侧的 WG 握手 |

## 阶段五：工程化（支撑持续迭代）

| # | 项 | 说明 |
|---|---|---|
| 5.1 | CI 矩阵 | msvc-x64 / mingw / wsl-gcc / crypto=OFF / shared |
| 5.2 | 一致性测试 | `test/cppconformance`（sliver 仓库）接入 CI，跑完整 RPC 对齐 |
| 5.3 | 体积/符号 | 验证静态 1–5 MB（目标 ~512 KB），LTO/strip 配置 |
| 5.4 | 文档 | 每个 RPC 的 Go↔C 字段映射表补全 |

## 优先级与依赖

1. **阶段一**（协议补全）— 无依赖，纯增量，最高性价比。
2. **阶段二**（beacon）— 依赖阶段一的 RPC 稳定。
3. **阶段三**（Windows 深度）— 独立，可与阶段二并行。
4. **阶段四**（多传输）— 依赖阶段二的 C2 循环抽象。
5. **阶段五**（CI/测试）— 应尽早启动（先于阶段三/四更稳）。

## 建议的下一步（本轮可直接开工）

阶段一 1.1（execute Env）+ 1.2（netstat UDP）：两个都是「解析字段 + 平台调用」的增量改动，风险低、可 harness/实机验证，适合作为 v0.11.0 的第一批。
