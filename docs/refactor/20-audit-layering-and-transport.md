# 结构审计：分层、session/beacon 的真实差异、多传输演进

基线：`feat/beacon` @ `f79ce2e`（= sliver `de13a0a9`，session 稳定）。
本文只做"关系梳理与结构判断"，不含任何代码改动。

---

## 1. 分层实测（由 `#include` 边推导，非猜测）

```
entry/        main.c(1088) dllmain.c(71)
              入口、CLI/自检、GUI WinMain；只做"装配 + 调 modes"

modes/        implant.c(110)  ← 入口装配 + 重连循环（模式选择应在此）
              session.c(378)  ← 通道 + 会话调度（当前混在一起，见 §3）
              shell.c(512)    ← 子进程 + 隧道    （只依赖 rpc/fs/proc）
              portfwd.c(425)  ← TCP 中继         （只依赖 rpc/proto）

rpc/          dispatch.c(1317)  ← 请求→响应分发；**无连接状态**，纯函数式
proto/        proto.c(1410) domain_enc.c(410)  ← 编解码；不依赖传输
wire/         frame.c(193)  ← 签名帧（Ed25519/minisign），只要求 read_fn/write_fn
              yamux.c(753)  ← 多路复用
transport/    tls_client.c(611) transport.c(63)  ← TLS 握手 + 字节流 vtable
crypto/       age/minisign/ed25519/mbedtls       ← 密钥、哈希
core/         config.c log.c fileio.c            ← 注入配置、日志、文件
platform/     win32/ posix/                      ← OS 层
```

依赖方向严格单向：`entry → modes → rpc → proto/wire → transport → crypto`，
`core/platform` 为横切。**没有反向依赖**（`proto.c` 已不再 include 平台头）。

## 2. 三个结构性发现（本次审计重点）

### 发现 1：`transport.h` 的抽象是"字节流"，不是"消息"

```c
struct sliver_transport { long (*read)(...); long (*write)(...); void (*close)(...);
                          const char *(*version)(...); const char *(*ciphersuite)(...); };
```

它是给 **mTLS 长连接**量身做的。而 HTTP(S)/DNS 的承载单位是"一次请求携带一帧"，
没有长连接字节流可读写。所以这个 seam 对 HTTP 是**错的**：
- 若 HTTP 实现在它上面 → 要在 HTTP 上再实现 yamux（协议不允许/极难）；
- 若在它下面 → 又要绕过 `read/write`，抽象失效。

### 发现 2：`frame.c` 已经是"消息级"且传输无关 —— 这是正确的支点

```c
int sliver_frame_write(signer, sliver_io_write_fn write_fn, void *ctx, const void *env, size_t len);
int sliver_frame_read(const char *server_pubkey_text, sliver_io_read_fn read_fn, void *ctx, sliver_buf_t *out);
```

它只要求"能把字节按顺序写出去/读回来"。**任何传输只要能提供这两个回调，就能承载 Sliver 协议**。

### 发现 3：`session.c` 把"通道能力"与"会话调度"焊在一个文件里

| 函数 | 实际职责 | 归属 |
|---|---|---|
| `stream_read/stream_write` | yamux 流上的读写回调 | **通道** |
| `transport_read/transport_write` | transport vtable 转发 | **通道** |
| `send_envelope` | 开流 + 写签名帧 + 关流（含 tx_lock/closed 保护） | **通道** |
| `rpc_send_adapter` / `shell_send_adapter` | 把业务数据包成 Envelope 并交给通道 | **通道（适配器）** |
| `heartbeat_thread` | 周期 Ping | **模式（session 专属）** |
| `sliver_session_run` | 连接建立→register→accept 循环→teardown | **模式** |

10 个函数里 6 个是通道级、与"session 还是 beacon"无关。这就是后续加 HTTP 必然回归的根因：
**任何新传输都要改 session.c。**

## 3. session 与 beacon 的真实差异（代码级，只有 3 处）

共同部分（**不应**分化）：wire（Envelope+签名帧）、编解码、`sliver_rpc_dispatch`、
shell/portfwd 的业务逻辑、crypto、platform、config。

| 维度 | session | beacon |
|---|---|---|
| 连接生命周期 | 一次建立、长期持有；`for(;;)` accept 服务端推流 | 每次 checkin 建连→用→关 |
| 谁先发 envelope | 服务端（Accept 后收 request，回 response 到新流） | 植入体（先 Send checkin；无结果再 Recv 任务） |
| 结果回传时机 | 立即（worker 直接 send） | 延迟到下一次 checkin（pending 队列） |
| 心跳 | `heartbeat_thread` 发 Ping | 无（checkin 即心跳） |
| 任务执行 | `sliver_rpc_dispatch` | **同一函数** |

→ 所以正确表述是：**两模式共用"通道 + 分发 + 业务"，只在"调度器"上不同。**
`modes/` 里应有两个很薄的调度器（各 100–200 行），而不是两套连接代码。

## 4. 目标结构：把 seam 上移到 channel（消息级）

```c
/* include/sliver/channel.h */
typedef struct sliver_channel sliver_channel_t;
struct sliver_channel {
  int  (*send)(sliver_channel_t *c, const void *env, size_t len);  /* 已签名帧 */
  int  (*recv)(sliver_channel_t *c, sliver_buf_t *env_out);        /* 一帧 */
  void (*close)(sliver_channel_t *c);
  const char *(*describe)(sliver_channel_t *c);                    /* "mtls"/"http"/... */
  void *state;
};

/* 按注入配置里 C2 URL 的 scheme 选择实现；同时完成握手/前导/认证 */
int sliver_channel_dial(const sliver_session_config_t *cfg, sliver_channel_t **out,
                        char *err, size_t err_len);
void sliver_channel_free(sliver_channel_t *c);
```

实现映射：

| 实现 | 组成 | 每帧成本 |
|---|---|---|
| `channel_mtls.c` | TLS → `MUX/1` → yamux → frame | 开流+写+封流 |
| `channel_http.c` | HTTPS POST（body=帧）/ 响应体=回帧 | 一次 HTTP RTT |
| `channel_dns.c` | DNS TXT/AAAA 分片承载帧 | 多次查询 |
| `channel_wg.c` | WireGuard → yamux → frame | 同 mtls |

**关键约束**：`SLIVER_YAMUX_PREFACE`、yamux、`transport_t` 全部成为 `channel_mtls.c` 的**内部细节**；
`session.c`/`beacon.c` 只 include `channel.h`。

## 5. 为什么这样加 HTTP(S) 不会伤 session

| 变化 | 对 session 的影响 |
|---|---|
| 新增 `channel_mtls.c`（把现有代码原样搬入） | 行为等价，harness 可逐字节比对 |
| `session.c` 改为调 `channel.send/recv` | 纯替换调用点；`tx_lock/closed` 保护可随之下沉到 channel |
| 新增 `channel_http.c` | 新文件，session 不感知 |
| `sliver_channel_dial` 按 scheme 分派 | session 的 `mtls://` 走原路径 |

反之，**不抽 channel** 的后果：每加一种传输都要动 `session.c`（378 行，含锁与生命周期），
每次都是高风险改动 —— 这正是要避免的。

## 6. 建议的演进顺序（每阶段 session 必须保持绿）

| 阶段 | 内容 | 触碰 | 验证 |
|---|---|---|---|
| **0** | 修 D1(`SLIVER_IS_BEACON` 的 ON/true)、D2(LTO 默认 OFF)、D3(crypto=OFF+exe 报错) | cmake/CMakeLists | harness 全绿；header 值正确 |
| **1** | **纯搬运**：把通道能力从 `session.c` 抽成 `channel_mtls.c` + `channel.h` | 新增 2 文件；`session.c` 改调用 | harness 全绿（行为应逐字节不变） |
| **2** | `beacon.c`：用同一 channel 实现 register/checkin 调度 | 新增 1 文件 + `CMakeLists` 一行 | session 仍绿；beacon 实机注册 |
| **3** | `implant.c` 加 `is_beacon` 分派（3 行） | `implant.c` | 两模式并行验证 |
| **4** | `channel_http.c` + `sliver_channel_dial` 按 scheme 分派 | 新增文件 + `implant.c` 少量 | session(mtls) 仍绿；http 可连 |
| **5** | 可选：抽出 `implant_common`（register 组装、sender 注册模板） | `modes/` | 同上 |

阶段 1 是**唯一**会大面积动 `session.c` 的一次，且是纯搬运、可逐字节验证；
其余阶段对 session 都是"新增文件"或"3 行分派"。

## 7. 禁止清单（保证两模式与多传输长期稳定）

1. 禁止在 `session.c`/`beacon.c` 里直接出现 `sliver_yamux_*` / `sliver_tls_*`（阶段 1 之后）。
2. 禁止任何文件级（static）的 `sliver_yamux_t` / `sliver_tls_t` / `sliver_transport_t`；实例必须栈上或由 channel 拥有。
3. 禁止 beacon 复用 `session.c` 的 `session_tx_t` / `g_tx`（会清掉 session 状态）。
4. 禁止两模式共享同一份 `static` sender 适配器；各自在启动时 `sliver_*_set_sender` 注册自己的。
5. 禁止把传输相关的名字（`MUX/1`、`preface`）泄漏到 `modes/` 层。
6. 禁止 `proto.c` 反向 include `platform/` 头（已修，勿回退）。

## 8. 回归不变量（每阶段结束必跑）

1. `--selftest-all` PASS
2. `tests\scripts\inject-config-selftest.ps1` 全绿（含 `SHELL` 的 `chinese-ok`/`big-output-ok`）
3. session 配置 → `SLIVER_IS_BEACON 0`；beacon 配置 → `1`
4. session register 报文逐字节不变（阶段 1 的硬指标）
5. 产物含 `MUX/1`，体积 850–900 KB（排除 LTO 静默失效）
