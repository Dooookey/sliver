# Beacon 模式与多传输：全量扫描结论 + 详细规划 + 代码整理

基线：`feat/beacon` @ implant-cpp `d3ab6bf`（阶段 0 已落地）/ sliver `407ff441`。
本文为**扫描与规划**，不含代码改动。所有事实标注了 `文件:行号`，均为本仓库实测。

---

## 0. 结论摘要（先看这段）

1. **beacon 参数通路已经打通到 C 端宏**：server 侧 `config.json` 已写 `beacon` / `beacon_interval_ns` /
   `beacon_jitter_ns`，CMake 已产出 `SLIVER_IS_BEACON` 等宏，`config.c` 已提供访问器。
   **唯一缺的是消费者**——`sliver_config_is_beacon()` 在当前 C 代码里调用次数为 **0**。
2. **模式是编译期常量**：`SLIVER_IS_BEACON` 是 `#define`。一个产物只承载一个模式，不存在运行时切换。
   因此"两模式并存"= 两个产物，这决定了分流方式。
3. **两模式的差别只有"调度"三点**（连接活多久 / 谁先发 / 结果何时回），
   **协议、编解码、RPC 分发、执行体 100% 共用**。
4. **当前的抽象层次错了**：`transport.h` 是"字节流"抽象，而官方 HTTP 传输是 **long-poll**（`DoPoll`），
   不是双工流。因此多传输的接缝必须上移到"**信道（channel）**"，并区分 `STREAM` / `POLL` 两类。
5. **`dispatch.c` 目前 0 处模式感知**，其中 5 个 handler 依赖双工（`1197-1201`）。
   beacon 下必须显式拒绝而不是挂起——这是"beacon 不卡死"的关键。

---

## 1. 端到端参数通路（实测，逐跳）

| 跳 | 位置 | 内容 |
|---|---|---|
| 1 | `client/command/cppgen/commands.go:102-104` | `--beacon` / `--interval`(默认60) / `--jitter`(默认0) |
| 2 | `client/command/cppgen/generate.go:134-136,162-164` | 读 flag → `ImplantConfig{IsBeacon, BeaconInterval(ns), BeaconJitter(ns)}` |
| 3 | `server/rpc/rpc-generate-cpp.go:78,108` | `config = req.Config`（非 profile 时）→ `cppbuild.CompileExecutable(name, build, config, …)` |
| 4 | `server/cppbuild/config.go:20,22,23` | 写 `config.json`：`beacon` / `beacon_interval_ns` / `beacon_jitter_ns` |
| 5 | `server/cppbuild/cppbuild.go:133-138` | cmake 只收 `CMAKE_BUILD_TYPE` / `SLIVER_CONFIG_JSON` / `IMPLANT_FORMAT` / `IMPLANT_DEBUG_LOG` / `IMPLANT_GUI` / `BUILD_TESTING` |
| 6 | `cmake/SliverConfigGen.cmake:60,66,156-160` | JSON → `SLIVER_IS_BEACON` / `SLIVER_BEACON_INTERVAL_NS` / `SLIVER_BEACON_JITTER_NS` |
| 7 | `src/core/config.c:28,29,32` | `sliver_config_is_beacon()` / `_beacon_interval_ns()` / `_beacon_jitter_ns()` |

### 两个必须记住的推论

- **beacon 只走 `config.json`，不经 `-D`**（第 5 跳证据）。所以新增 beacon 相关构建参数时，
  应优先扩展 JSON 契约，**不要在 `cppbuild.go` 里加 `-D`**——否则会分裂成两个真源。
- **第 7 跳零消费者**。所以"加 beacon"本质上是：在 `modes/` 里新增一个消费者 + 一处分流，
  而**不需要**改 server、cppbuild、cmake（除已修的三处缺陷）。

---

## 2. 模式分流设计（唯一判别点）

### 2.1 判别点

唯一真源：`SLIVER_IS_BEACON`（编译期宏）→ `sliver_config_is_beacon()`。

### 2.2 分流位置：只允许一处

建议放在 `src/modes/implant.c` 的 `sliver_implant_run()` 顶部（现 110 行，见 `implant.c:14`）：

```c
/* 示意（本阶段不落地） */
if (sliver_config_is_beacon()) {          /* 编译期常量：session 产物中该分支被消除 */
  return sliver_beacon_run(argv0);
}
... 现有 session 装配 + 重连循环保持不变 ...
```

**为什么这是最安全的**：
- `SLIVER_IS_BEACON` 是 `#define 0/1`，编译器会**常量折叠**；session 产物里既没有该分支，
  也不会链接 `sliver_beacon_run`（随后被 `/OPT:REF` 回收）→ session 的指令流与当前**完全一致**。
- 分流只发生在函数入口，`session` 的装配、重连、teardown 一行不动。
- 不需要在 `CMakeLists.txt` 里按模式增删源文件（避免构建矩阵翻倍）。

### 2.3 编译期 vs 运行期：为什么不选运行期

运行期分流会让**每个** session 产物都携带 beacon 代码与状态机，任何 beacon 缺陷都可能
在 session 路径上暴露（例如全局 sender 被改），直接违反"session 稳定优先"。编译期常量分流没有这个风险。

---

## 3. 目标分层（三层）

```
L3 调度器 (modes/)
    session_loop.c   ← 从 session.c 抽出：长期连接 + accept 驱动
    beacon_loop.c    ← 新增：短连接 + checkin 驱动 + pending 结果队列

L2 信道 (transport/)
    channel.h                     ← 统一接口（STREAM | POLL）
    channel_mtls.c                ← TLS + "MUX/1" + yamux + frame（现有代码原样搬入）
    channel_http.c （后续）        ← HTTP(S) POST 承载帧 + long-poll
    channel_dns.c / channel_wg.c（后续）

L1 协议与业务（不改）
    wire/frame.c  签名帧（传输无关）
    wire/yamux.c  仅 STREAM 类信道使用
    proto/*, rpc/dispatch.c, modes/shell.c, modes/portfwd.c
    crypto/*, core/*, platform/*
```

### 每层职责与现状

| 层 | 现有文件 | 现状问题 | 目标 |
|---|---|---|---|
| L3 | `modes/session.c`(378) | **通道与调度焊在一起**：10 个函数里 6 个是通道级（`stream_read/write`、`transport_read/write`、`send_envelope`、`rpc/shell_send_adapter`） | 拆成 `channel_mtls.c` + `session_loop.c` |
| L2 | `transport/transport.c`(63)、`tls_client.c`(611) | `transport_t` 只有字节流 read/write，**无法承载 HTTP** | 新增 `channel.h`；`transport_t` 降级为 `channel_mtls` 内部实现 |
| L1 | `frame.c`(193)、`yamux.c`(753)、`dispatch.c`(1317) | `dispatch.c` 无双工能力门控 | 增加能力位，5 个 handler 早退 |

---

## 4. 两种模式到底差在哪（代码级）

| 维度 | session | beacon | 共用? |
|---|---|---|---|
| 承载协议 | Envelope{ID,Type,Data} + Ed25519 签名帧 | 同 | ✅ 完全共用 |
| 编解码 | `proto.c` / `domain_enc.c` | 同 | ✅ |
| 请求→响应 | `sliver_rpc_dispatch` | 同 | ✅ |
| 执行体 | execute/fs/registry/token/… | 同 | ✅ |
| 子进程/隧道逻辑 | `shell.c` / `portfwd.c` | 不可用（需双工） | ⚠️ 需能力门控 |
| **连接生命周期** | 一次建立、长期持有 | 每次 checkin 建连→用→关 | ❌ 调度差异 |
| **发起方** | 服务端推流（Accept） | 植入体主动 Send/Recv | ❌ 调度差异 |
| **结果回传时机** | 立即 | 下一次 checkin（pending 队列） | ❌ 调度差异 |
| 心跳 | `heartbeat_thread` | 无（checkin 即心跳） | ❌ 调度差异 |

> 一句话：**差异全在 L3 调度器，L1/L2 一行都不该分叉。**

---

## 5. Channel 接口设计（对齐官方语义）

### 5.1 为什么必须区分 STREAM / POLL

- `mtls` / `wg`：TLS/WG 之上跑 yamux，**服务端可主动开流**（双工）→ `STREAM`。
- `http(s)`：官方实现是 `DoPoll` + `PollTimeout`（`httpclient.go:324-351`），
  一请求一响应，**没有服务端主动推送**→ `POLL`。
- `dns`：请求/响应式分片 → `POLL`。

如果沿用现有 `transport_t`（只有 read/write），HTTP 无处安放；如果调度器假设双工，
session over HTTP 会永远等不到"服务端推流"。

### 5.2 接口（示意）

```c
typedef enum { SLIVER_CH_STREAM, SLIVER_CH_POLL } sliver_ch_kind_t;

struct sliver_channel {
  sliver_ch_kind_t kind;
  const char *(*describe)(sliver_channel_t *);

  /* STREAM: 发送一帧（内部开流/写/封流）；POLL: 入待发队列，由 poll 一并提交 */
  int  (*send)(sliver_channel_t *, const void *env, size_t len);

  /* STREAM: 阻塞等一帧（服务端主动）；POLL: 返回一次 poll 的响应批 */
  int  (*recv)(sliver_channel_t *, sliver_buf_t *env_out);

  /* 仅 POLL：一次往返，携带 n 个待发帧与 up to m 个回收帧 */
  int  (*poll)(sliver_channel_t *, const sliver_buf_t *out, size_t n,
               sliver_buf_t *in, size_t max_in, size_t *in_count);

  void (*close)(sliver_channel_t *);
  void *state;
};

/* 按 cfg 的 C2 scheme 选择实现并完成握手/前导/认证 */
int sliver_channel_dial(const sliver_session_config_t *cfg, sliver_channel_t **out,
                        char *err, size_t err_len);
```

### 5.3 各传输的实现要点与限制

| 实现 | kind | 帧封装 | 关键限制 |
|---|---|---|---|
| `channel_mtls` | STREAM | TLS→`MUX/1`→yamux→frame | 现有能力，原样搬入 |
| `channel_http` | POLL | HTTPS POST body=帧；响应体=回帧 | 受 C2 profile 的 URL/headers 约束；`poll-timeout` 决定长轮询时长 |
| `channel_dns` | POLL | TXT/AAAA 分片 | 帧大小上限、需分片与重排 |
| `channel_wg` | STREAM | WG 隧道→yamux→frame | 需先建立 WG 隧道（`wg_*` secrets） |

---

## 6. 模式 × 传输 兼容矩阵（规划基准）

|  | mtls | wg | http(s) | dns |
|---|---|---|---|---|
| **session** | ✅ 现状 | 规划 | 需 POLL 驱动（官方即 poll） | 理论可行 |
| **beacon** | ✅ 目标 | 规划 | ✅ 天然契合 | ✅ 天然契合 |

推论：调度器**不能假设双工**。正确写法是两种驱动原语：

- `STREAM` 驱动：`recv()` 阻塞等（现有 session 行为）
- `POLL` 驱动：`poll(待发批, 回收批)` + 超时
- beacon = `POLL/STREAM 单次往返` + `sleep(interval+jitter)`
- session(STREAM) = 持续 `recv`；session(POLL) = 持续 `poll(PollTimeout)`

这样 session 与 beacon 共用同一套信道，差异只体现在"循环怎么转"。

---

## 7. RPC 能力分级（beacon 不卡死的关键）

`dispatch.c` 当前 0 处模式感知。表中 5 个条目依赖双工（`dispatch.c:1197-1201`）：

| Type | Handler | 双工依赖 |
|---|---|---|
| 20/21 | `sliver_rpc_handle_shell` | 需要长期双向隧道 |
| 22 | `sliver_rpc_handle_tunnel_data` | 隧道数据 |
| 23 | `sliver_rpc_handle_tunnel_close` | 隧道关闭 |
| 135 | `sliver_rpc_handle_shell_resize` | shell 相关 |
| 80/81 | `sliver_rpc_handle_portfwd` | 端口转发需双工 |

规划：引入能力位（如 `sliver_rpc_set_capabilities(SLIVER_CAP_TUNNEL)`），
上述 handler 在能力缺失时**立即返回明确错误**（例如 `"shell is not available in beacon mode"`），
而不是等待/挂起。beacon 调度器启动时清空该能力位。

---

## 8. 稳定性保障

### 8.1 不变量（每次改动后必跑）

1. `--selftest-all` PASS
2. `tests\scripts\inject-config-selftest.ps1` 全绿（含 `SHELL` 的 `chinese-ok` / `big-output-ok`）
3. session 配置 → `SLIVER_IS_BEACON 0`；beacon 配置 → `1`
4. session 的 register 报文**逐字节不变**（通道抽取阶段的硬指标）
5. 产物含 `MUX/1`、体积 850–900 KB（排除 LTO 静默失效；阶段 0 已把 LTO 默认关掉）
6. `dispatch.c` 的能力门控不改变 session 行为（session 恒有 `CAP_TUNNEL`）

### 8.2 回归矩阵

| 组合 | 验证内容 |
|---|---|
| session × mtls | 现有 harness 全量（13 类 RPC + shell + 3 次重连） |
| beacon × mtls | 注册进 `beacons`；每 interval(+jitter) checkin；`ls`/`execute` 有结果；无 yamux 报错 |
| session × http | （阶段 5 后）poll 驱动能收发，RPC 正常 |
| beacon × http | （阶段 5 后）checkin 走 poll |

---

## 9. 分阶段实施计划

| 阶段 | 内容 | 改动面 | 风险 | 验收 | 回滚 |
|---|---|---|---|---|---|
| **0 ✅** | 修 D1/D2/D3（已落地 `d3ab6bf`） | cmake/CMakeLists | 极低 | header 值正确、LTO=OFF、crypto=OFF 报错、harness 绿 | revert 该提交 |
| **1** | **通道抽取（纯搬运）**：`channel.h` + `channel_mtls.c`，`session.c` 改为调用 | 新增 2 文件；`session.c` 改调用点 | **中**（唯一大改） | 不变量 1–5，尤其 register 逐字节一致 | revert 该提交 |
| **2** | `beacon_loop.c`：dial→register→sleep→checkin（先不含任务执行） | 新增 1 文件 + CMake 一行 | 低 | beacon 出现在 `beacons`；无 yamux 报错 | 删文件 |
| **3** | `implant.c` 加 3 行分流 | 1 文件 | 极低 | 两模式各自正常；session 产物无 beacon 调用 | revert 3 行 |
| **4** | 能力门控：`CAP_TUNNEL` + 5 handler 早退 | `rpc.h`/`dispatch.c`/两个 sender 模块 | 低 | session shell/portfwd 仍正常；beacon 下明确报错 | revert |
| **5** | `channel_http.c` + `dial` 按 scheme 分派 | 新增 1–2 文件 + `implant.c` 少量 | 中 | session×mtls 仍绿；http 能连 | 删文件 |
| **6** | `channel_dns.c` / `channel_wg.c` | 新增 2 文件 | 中 | 同上 | 删文件 |

> 阶段 1 之后，`session.c`/`beacon_loop.c` 中不应再出现 `sliver_yamux_*` / `sliver_tls_*`。

---

## 10. 代码整理清单

### 10.1 新增

| 文件 | 职责 |
|---|---|
| `include/sliver/channel.h` | 信道接口（STREAM/POLL、send/recv/poll/close、dial） |
| `src/transport/channel_mtls.c` | 现 `session.c` 的通道能力迁入（TLS+preface+yamux+frame） |
| `src/transport/channel_http.c`（阶段5） | HTTPS POST + long-poll |
| `src/modes/beacon_loop.c` | beacon 调度：注册、checkin、pending 队列、jitter |
| `src/modes/session_loop.c`（可选，阶段1 可暂不拆） | session 调度 |

### 10.2 移动/降级

| 现有 | 去向 |
|---|---|
| `session.c:32-103`（`stream_read/write`、`transport_*`、`send_envelope`） | → `channel_mtls.c` |
| `session.c:115-148`（`rpc_send_adapter`/`shell_send_adapter`） | → 留作"适配器"，由各调度器注入（不要共享 static） |
| `transport/transport.c`、`transport.h` | 保留，但仅 `channel_mtls.c` 可见（内部实现） |
| `SLIVER_YAMUX_PREFACE` | 仅 `channel_mtls.c` 使用 |

### 10.3 保持不动（**不要碰**）

`proto/*`、`wire/frame.c`、`rpc/dispatch.c`（除阶段 4 的能力位）、
`modes/shell.c`、`modes/portfwd.c`、`crypto/*`、`core/*`、`platform/*`。

### 10.4 禁止清单

1. `session_loop` / `beacon_loop` 中出现 `sliver_yamux_*` / `sliver_tls_*`（阶段 1 后）。
2. 任何**文件级 static** 的 yamux / tls / transport / channel 实例。
3. beacon 复用 `session.c` 的 `session_tx_t` / `g_tx`（会清掉 session 状态）。
4. 两模式共享同一份 static sender 适配器（各自启动时 `sliver_*_set_sender` 注册自己的）。
5. 在 `cppbuild.go` 里为 beacon 增加 `-D`（会分裂真源；应扩展 `config.json`）。
6. `proto.c` 反向 include `platform/` 头（已修，勿回退）。

---

## 11. 已知风险与规避

| 风险 | 规避 |
|---|---|
| LTO 静默产出不上线产物 | 阶段 0 已默认关闭；CI 断言 `IMPLANT_LTO:BOOL=OFF` |
| `crypto=OFF` + exe 静默无功能 | 阶段 0 已改 FATAL_ERROR |
| `SLIVER_IS_BEACON` 恒 0（`ON` vs `true`） | 阶段 0 已修；回归断言两种配置的值 |
| 全局 sender 被 beacon 覆盖后 session 异常 | session 每次 `sliver_session_run` 都会重设（`session.c:238-241`）；beacon 只在自身生命周期内设置 |
| beacon `recv` 无超时导致卡死 | 阶段 2 用 poll/超时语义；STREAM 也要带超时上限 |
| pending 队列无限增长 | 固定上限 + 满则丢弃并记日志 |
| beacon 下 shell/portfwd 挂起 | 阶段 4 能力门控，早退报错 |

---

## 12. 验收定义（Definition of Done）

- **session**：harness 全绿、连续 3 次重连无异常、`SLIVER_IS_BEACON 0`
- **beacon**：`beacons` 出现、每 interval(+jitter) checkin、`ls`/`execute` 经 beacon 返回结果、
  无 `yamux: Failed to read/write` 报错
- **多传输**：新增传输只需新增 `channel_*.c` + `dial` 一个分支，**session/beacon 调度器零改动**
- **隔离**：`git checkout feat/implant-cpp` 可随时回到纯 session 基线且行为不变
