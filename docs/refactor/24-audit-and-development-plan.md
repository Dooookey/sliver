# 全量审计快照与后续开发计划

状态：`feat/beacon` @ sliver `a74b762f` / implant-cpp `5e6283f`。
实测结论：**session×mtls、beacon×mtls、beacon×http(s) 三条线均可上线并执行任务**。

---

## 1. 架构现状（分层与职责）

```
entry/     main.c dllmain.c            入口、CLI/自检、GUI WinMain
modes/     implant.c(配置装配+模式分派) session.c(长连接调度) beacon.c(短连接调度)
rpc/       dispatch.c                 请求→响应表（无连接状态），部分异步(-2)
proto/     proto.c domain_enc.c        protobuf 编解码（不含连接概念）
wire/      frame.c yamux.c             签名帧 / 多路复用（仅 STREAM）
transport/ channel.h channel.c         信道工厂（按 scheme 分派）
           channel_mtls.c              STREAM：TLS+MUX/1+yamux+frame
           channel_http.c              POLL：HTTP(S)，裸帧或会话层
           http_session.c              真实 listener 会话（age 交换+cookie+cipher+encoder）
           tls_client.c transport.c    TLS 与字节流（channel_mtls 内部）
crypto/    gzip cipher age age_keyex encoders minisign ed25519 mbedtls
core/      config(注入配置访问器) log fileio
platform/  win32/ posix/
```

依赖严格单向：`entry → modes → rpc → proto/wire → transport → crypto`，
`core/platform` 横切；`modes/` 内**不再出现** `sliver_yamux_*` / `sliver_tls_*`；

### 两模式的共享面与差异面

| 面 | session | beacon |
|---|---|---|
| wire / proto / dispatch / 执行体 | 共用 | 共用 |
| channel（mtls 或 http） | 共用 | 共用 |
| 调度 | 长连接 + accept 驱动 + 心跳 | 短连接 + checkin + 结果队列 + jitter |
| 双工能力 | `SLIVER_CAP_TUNNEL`（shell/portfwd 可用） | 能力位 0（明确拒绝） |
| 产物体积 | 相同（两个调度器都链接，已文档化） | 相同 |

---

## 2. 稳定性风险清单（按严重度）

| 级别 | 项 | 现状 | 处置 |
|---|---|---|---|
| **P0** | HTTP 会话 cookie 轮换 | 每响应都更新（已修） | 已闭环 |
| **P0** | poll 响应体带 74B 签名头 | 兼容两种布局（已修） | 已闭环 |
| **P1** | HTTP 长轮询未接入 | recv 用固定 15s socket 超时，`poll_timeout` 未生效 | 计划 3.1 |
| **P1** | 编码器仅 NOP 可用 | `pick_nonce` 恒选 NoEncoder；base58/english/png 未实现 | 计划 3.2 |
| **P1** | session×http 未回归 | 仅 beacon×http 实测 | 计划 3.3 |
| **P2** | beacon 结果上传侧未加签名头 | 服务端当前接受；若 profile 开启校验会失败 | 计划 3.4 |
| **P2** | 心跳仅 session 有 | beacon 以 checkin 代心跳（官方一致） | 保持 |
| **P2** | 进程级共享状态 | `g_rpc_send` / `g_tx` / `g_pending` / sender 注册 | 已文档化，改动时需守规则（见 §5） |
| **P3** | 产物携带两个调度器 | 已知取舍（体积 +5~7KB） | 可选裁剪 |

---

## 3. 开发计划（按优先级）

### 3.1 HTTP 长轮询（P1，最小改动最高收益）

- **目标**：`poll_timeout` 生效；session over HTTP 不空转。
- **改动面**：`http_session.c`（recv 的 socket 超时取 `poll_timeout_ns`，超时→按 204 处理并返回 -1）、`config`（已有 `sliver_config_poll_timeout_ns`）。
- **验收**：beacon 在无任务时请求间隔 ≈ poll_timeout；有任务时立即返回。
- **风险**：低（仅超时语义）。

### 3.2 启用真实编码器（P1，OPSEC 与兼容性）

- **目标**：不再恒用 NoEncoder；按配置的 encoder id 选择 base64/hex/base32/gzip。
- **改动面**：`encoders.c`（已完成编码器）、`http_session.c`（`pick_nonce` 改为"按可用 kind 轮换"，并把选中的 kind 存进 session 用于收发）、`http_channel.c`（同步）。
- **验收**：对真实 listener，base64/hex/base32/gzip 四种各跑一次注册+checkin。
- **风险**：中（编码不一致会导致 404，需逐个实测）；base58/english/png 暂不选。

### 3.3 session × HTTP 全量回归（P1）

- **目标**：session 模式在 http(s) 上完成 13 类 RPC + shell（shell 需要隧道，POLL 下应被能力位正确拒绝或改为缓冲式）。
- **改动面**：无（验证为主）；若 shell 需要支持，见 3.5。
- **验收**：harness 增加 `-Session -Https` 组合；session 能上线、`ls/ps/execute` 正常，`shell` 给出明确不支持提示而非挂起。

### 3.4 上传侧签名头（P2）

- **目标**：请求体变为 `[74B minisign][nonce‖AEAD]`，与服务端 `CipherContext.Decrypt` 对齐。
- **改动面**：`http_session.c`（send 前用 `sliver_frame_signer_t` 签名；注意 HTTP 不带 u32 长度前缀）、`frame.c` 暴露 `sign_only` 辅助。
- **验收**：服务端日志无签名告警；把 profile 的校验打开后仍可用。

### 3.5 beacon 的 shell（P2，可选）

- 方案 A（推荐）：保持不支持（与官方一致），用 `execute`。
- 方案 B：缓冲式 shell —— beacon 常驻子进程，输出经 sender 入 pending 队列；**必须做背压**（队列满时暂停 reader）。延迟 = interval。

### 3.6 传输扩展（P2）

- `channel_dns.c`（POLL 分片）、`channel_wg.c`（STREAM）——`channel.h` 已预留 `poll()`。
- **前置**：config 契约补 DNS/WG 字段；`sliver_channel_dial` 加分派分支（调度器零改动）。

### 3.7 产物裁剪与体积（P3）

- 按 `SLIVER_IS_BEACON` 选择调度器源文件；`#if` 改造 `implant.c` 分派。
- 收益 −5~7KB；代价是构建配置翻倍、回归需两套。

### 3.8 测试与 CI（P1，持续）

- 现有：`ctest 7/7`（config/crypto/wire/gzip/cipher/age_keyex/encoders）、mtls harness（session+beacon）。
- 待补：HTTP 协议桩（模拟 200/204/403 + cookie + cipher），使 http 路径可离线回归；
  `-Session -Https` 组合；beacon 任务闭环断言（tasks → completed）。

---

## 4. 命令对齐矩阵（服务端 ↔ 植入体）

| 服务端命令 | 消息 | session | beacon | 备注 |
|---|---|---|---|---|
| `ls/pwd/cd/mkdir/rm/mv/cp/chmod/chtimes` | 5/6,11..17,98/99,109/110,113/114,127/128 | ✅ | ✅ | harness 已覆盖 |
| `upload/download` | 9/10, 7/8 | ✅ | ✅ | |
| `ps/env/netstat/ifconfig` | 18/19,66/67,49,42/43 | ✅ | ✅ | |
| `execute`（同步/异步） | 44, 136 | ✅ | ✅ | 异步走 -2 + sender |
| `grep/cat`（文件类） | 129 | ✅ | ✅ | |
| `registry` 系列 | 71/72/73/88/89/97/133 | ✅ | ✅ | Windows |
| `getprivs/maketoken/tokenowner/rev2self` | 87,64/65,100/101,30/31 | ✅ | ✅ | Windows |
| `services` 系列 | 130..132 | ✅ | ✅ | Windows |
| `screenshot` | 47/48 | ✅ | ✅ | |
| `shell`（交互） | 20/21 + 隧道 | ✅ | ⛔ 能力位拒绝 | 见 3.5 |
| `portfwd/socks` | 80/81 + 隧道 | ✅ | ⛔ 能力位拒绝 | |
| `invoke-getsystem/spawndll` | 32/33, 40/41 | ✅ | ✅（不依赖隧道时） | Windows |
| 未知类型 | — | 回 `UnknownMessageType` | 同 | 已验 |

> 对齐结论：**请求/响应类命令两模式均可用**；双工类（shell/portfwd）在 beacon 下被显式拒绝（不挂起），与官方语义一致。

---

## 5. 改动守则（避免破坏现有稳定性）

1. `modes/` 不得直接调用 `sliver_yamux_*` / `sliver_tls_*`（只能经 channel）。
2. 禁止文件级 static 的 yamux/tls/transport/channel 实例。
3. beacon 不得复用 `session_tx_t` / `g_tx`。
4. 两模式的 sender 各自注册，不共享 static 适配器。
5. 新增命令：`proto.h` 加 id → `proto.c`/`domain_enc.c` 编解码 → `dispatch.c` 加表项；
   需要隧道则在 handler 顶部检查 `SLIVER_CAP_TUNNEL`。
6. 改动源文件列表后**干净构建**再比较产物（VS 增量会链接旧 `sliver_core.lib`）。
7. 每次改动跑：`ctest` + mtls harness（session+beacon）；HTTP 相关再加真实 listener 实测。

---

## 6. 建议的下一步（收敛排序）

1. **3.1 长轮询** + **3.3 session×http 回归**（都是小改动、直接提升可用性）
2. **3.2 真实编码器**（提升兼容/OPSEC；需逐种实测）
3. **3.8 HTTP 协议桩**（让 http 路径进入离线回归，避免以后靠人工实测）
4. **3.4 上传签名** / **3.6 DNS、WG** / **3.5 beacon shell** / **3.7 体积裁剪**（按需）
