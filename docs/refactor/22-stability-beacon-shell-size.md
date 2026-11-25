# 稳定性扫描、beacon shell 设计、体积分析、后续规划（v0.12.0）

基线：`v0.12.0`（sliver `836dda68` / implant-cpp `98ef2db`，两仓库均已打 tag 并推送）。
本文所有结论均来自本仓库实测命令，标注了文件:行号。

---

## 1. 扫描结论（本次全量核对）

| 检查项 | 结果 |
|---|---|
| `modes/` 层是否还直接调用 `sliver_yamux_*` / `sliver_tls_*` / `sliver_transport_*` | **0 处**（全部经 `channel.h`） |
| 是否存在文件级 static 的 yamux/tls/transport/channel **实例** | 无（只有 static **函数**，实例都在栈上或由 channel 持有） |
| 能力位调用点 | `session.c:172` 设 `SLIVER_CAP_TUNNEL`；`beacon.c:261` 设 `0`；`shell.c:384`、`portfwd.c:253` 检查；`dispatch.c:339-341` 实现，默认全能力 |
| session/beacon 共享面 | channel、envelope 编解码、`sliver_rpc_dispatch`、全部业务 handler、crypto、platform |
| 差异面 | 只有 `modes/session.c`（长连接 + accept 驱动 + 心跳）与 `modes/beacon.c`（短连接 + checkin + 结果队列 + jitter） |
| 回归网 | session harness（13 类 RPC + PORTFWD + SHELL 400+ 帧 + 3 次重连）、beacon harness（注册 + checkin + dispatch + 结果回传 + 能力门控） |

### 进程级共享状态清单（唯一需要小心的部分）

| 状态 | 位置 | 何时设置 | 风险与控制 |
|---|---|---|---|
| `g_rpc_send` | `dispatch.c:331` | session 设 `rpc_send_adapter`；beacon 设 `beacon_rpc_send` | 一个进程只跑一种模式；两者都在各自入口设置 |
| `g_tx` | `session.c` | session 入口设、teardown 清 | beacon 完全不碰 |
| `g_pending`（64）/ `g_pending_lock` | `beacon.c` | beacon 入口初始化 | 互斥保护；满则丢弃并记日志 |
| 3 个 sender（shell/portfwd/rpc） | `rpc.h` | session 三个都设；beacon 只设 rpc | beacon 不支持 shell/portfwd → 能力位拒绝，不会调到 NULL sender |
| `tx_lock` / `send_lock` | session / channel | 进程生命周期内不销毁 | 故意泄漏一个互斥量，避免 detached worker UAF |

---

## 2. 体积问题：为什么 session 和 beacon 一样大

### 实测（干净构建，同一份 `config/dev.json` 仅改 beacon 字段）

| 构建 | 体积 | 含 `beacon: registered` | 含 `session: registered as` |
|---|---|---|---|
| `cmp-session`（`beacon=false`） | **797184** | 是 | 是 |
| `cmp-beacon`（`beacon=true`, interval 5s） | **797184** | 是 | 是 |

用户真实构建（带真实证书/密钥，字符串更大）：`798720` / `799232` —— 同样一致。

### 原因（设计使然）

1. `src/modes/beacon.c` 在 `CMakeLists.txt` 的源列表里**无条件编译**（不按模式增删源文件，避免构建矩阵翻倍）。
2. `implant.c` 的分派是 `if (sliver_config_is_beacon()) { return sliver_beacon_run(argv0); }` —— 这是**运行时**判断一个常量，链接器仍会保留 `sliver_beacon_run`。
3. 两种产物唯一的差别是 `SLIVER_IS_BEACON` 的**值**（0/1）与几个配置整数，代码与数据布局完全相同 → 体积必然一致。

**结论：一致是合理的、可预期的。** 代价是每个产物都携带另一个调度器（约 5–7 KB），收益是只有一个构建配置、不需要按模式维护两套产物预期。

### 若想让每个模式只带自己的调度器（可选）

把 `SLIVER_IS_BEACON` 从 `config.json` 提升为 CMake 变量（`SliverConfigGen.cmake` 已经在解析该 JSON，可直接 `set(SLIVER_BEACON_MODE 1)` 到父作用域），然后：

```cmake
if(SLIVER_BEACON_MODE)
  list(APPEND SLIVER_CORE_SOURCES src/modes/beacon.c)
else()
  list(APPEND SLIVER_CORE_SOURCES src/modes/session.c)
endif()
```

收益：体积 −5~7 KB，且另一模式的代码物理上不存在（更小的攻击面）。
风险：`implant.c` 的分派必须改成宏条件编译（`#if`），否则会引用不存在的符号；等于把"一个构建配置"变成"两个"，回归网要各跑一遍。

**建议**：暂不做。等阶段 5/6 传输也进来后再一次性评估（那时两个调度器 + 多传输的裁剪收益才明显）。

### ⚠️ 一个必须知道的坑（本次实测发现）

同一个 `build/` 目录做增量 VS 构建时，出现过**链接了旧 `sliver_core.lib`** 的产物：源文件列表已含 `beacon.c`，但产物里没有 beacon 字符串。这解释了历史那次"385 KB、无 `MUX/1`"的诡异产物。

**规则**：改动源文件列表（新增 `.c`）后，删除构建目录重新配置；比较体积/字符串前必须干净构建。
`generate-cpp` 每次都新建 `~/.sliver/slivers/.../<NAME>/build`，所以正常流程不受影响。

---

## 3. 后续新增命令会影响两种模式吗

### 三类命令，影响面完全不同

| 类型 | 例子 | 需要改的地方 | 对 session | 对 beacon |
|---|---|---|---|---|
| **A. 纯请求/响应** | `ls`、`ps`、`env`、`registry read`、`cat` | `proto.h` 加消息 id → `proto.c` 加编解码 → `domain_enc.c`/新文件加 encoder → `dispatch.c` 表里加一行 | 自动可用 | **自动可用** |
| **B. 需要双工（隧道）** | `shell`、`portfwd`、`socks5` | A 的全部 + 隧道数据/关闭消息 + 一个 sender 适配器 | 自动可用 | **必须能力门控**，否则客户端挂起 |
| **C. 需要新的传输能力** | `http`/`dns` C2、`pivot` | `channel_*.c` + `sliver_channel_dial` 分支 + `config.json` 契约 | 调度器零改动 | 调度器零改动 |

### 为什么 A 类天然双模式可用

`sliver_rpc_dispatch()` 是无状态表驱动（`dispatch.c:24-25` 的 `req_type/resp_type/handler`），session 与 beacon 调的是同一张表、同一批 handler。beacon 只是"什么时候发/什么时候收"不同。

### 新增命令的检查清单（每次照做即可）

1. 消息 id 加到 `proto.h`，不与他人冲突（当前最大 136）。
2. 编解码放到 `proto.c`（通用）或 `domain_enc.c`（域对象）；后者只依赖 `fs/proc/...` 抽象。
3. `dispatch.c` 表里加 `{REQ, RESP, handler}`；只回响应就 `return 0`，**异步**要 `return -2` 并自己 `sliver_rpc_send`。
4. 需要隧道 → 在 handler 顶部加 `SLIVER_CAP_TUNNEL` 检查，并在 `session.c`/`beacon.c` 声明的能力里对齐。
5. 跑两侧 harness（session 13 类 + beacon 注册/checkin）。
6. 不新增文件级静态连接状态。

> 也就是说：**A 类命令不会影响两种模式的编译/生成/运行**，只要不引入新的全局状态；B 类只要照第 4 条做门控就不会挂死 beacon。

---

## 4. beacon 上的 shell 怎么设计

### 现状：客户端在发 RPC 之前就拒绝了

`client/command/shell/commands.go:20`：

```go
Annotations: flags.RestrictTargets(consts.SessionCmdsFilter),
```

所以报错是 `Command shell is only available for: - session` —— **不是 C 端拒绝，是客户端过滤**。这与官方 Sliver 一致：beacon 是异步模型，交互式 shell 不在官方支持范围内。

### 为什么 beacon 不能直接照搬 session 的 shell

session 的 shell = 常驻子进程 + **双向隧道**：客户端写 `TunnelData`，植入体读入 stdin；子进程 stdout 立刻经 `TunnelData` 推回。beacon 只在 checkin 时能发帧，**没有实时回推通道**，所以"实时交互"在语义上不成立。

### 三个可选方案

| 方案 | 做法 | 改动量 | 延迟 | 风险 |
|---|---|---|---|---|
| **A. 维持官方语义（推荐）** | beacon 下用 `execute` 执行单条命令 | 无 | 一次 checkin | 无 |
| **B. 复用 execute 做"命令式 shell"** | 客户端在 beacon 上把 `shell` 映射成逐条 `execute`（不建隧道） | 仅客户端命令映射 | 一次 checkin/条 | 无 C 端改动；不是真交互 |
| **C. 缓冲式 shell（半双工）** | beacon 持一个常驻 `cmd.exe`；stdout 经 sender 入 pending 队列，下次 checkin 回传；stdin 由客户端 `TunnelData` 入队 | C 端 ~20 行 + 客户端放开注解 | **interval(+jitter)** | 输出积压/队列溢出、进程长期驻留 |

### 方案 C 的具体设计（若要做）

C 端其实**已经很接近可用**，因为 `shell.c` 的输出走 `sliver_shell_set_sender(...)`：

1. `beacon.c` 增加 `beacon_shell_send(type, data, len)`：与 `beacon_rpc_send` 一样只入 pending 队列（不碰链路）。
2. `beacon.c` 调 `sliver_shell_set_sender(beacon_shell_send)`，并把能力位改为 `SLIVER_CAP_TUNNEL`（或新增更细的 `SLIVER_CAP_SHELL_BUFFERED`）。
3. 背压：`pending` 满时不要丢输出，而是让 shell reader 暂停（例如给 shell 模块一个"队列水位"回调），否则大输出会静默丢失。
4. 客户端：放开 `shell` 注解，并在 UI 上明示"beacon shell 每 N 秒一批输出"。

**代价**：用户每敲一行要等 1 个 interval 才看到输出；`interval=60s` 时体验不可接受。只有在 `--seconds 5` 这类短间隔下才有意义。
**建议**：先做 A/B，C 作为可选项，等真有需求再开（并同步做背压）。

---

## 5. 稳定性不变量（每次改动后必跑）

1. `implant.exe --selftest-all` PASS
2. `ctest -C Release` 3/3
3. session harness：REGISTER + 13 类 RPC + PORTFWD(+DATA) + `SHELL`（chinese-ok / big-output-ok）+ 3 次重连
4. beacon harness（`-Beacon`）：`BEACON REGISTER ok` + `BEACON CHECKIN ok: tasks=2`（ls 成功 + shell 被拒）
5. session 配置 → `SLIVER_IS_BEACON 0`；beacon 配置 → `1`
6. 产物含 `MUX/1`；`IMPLANT_LTO:BOOL=OFF`
7. 改动源文件列表后**干净构建**再比较产物

---

## 6. 后续规划

| 阶段 | 内容 | 前置 | 主要风险 |
|---|---|---|---|
| **5a ✅** | HTTP(S) 配置契约：`config.json` 的 `http` profile → CMake 宏 + `implant_http_c2.c` 数组 → `sliver_config_http_*` 访问器（无运行时消费者） | 无 | 已完成，两侧 harness 全绿 |
| **5b ✅** | HTTP 通道：`channel_http.c`（POLL，`http://`）+ `channel.c` 按 scheme 分派 | 5a | 已完成，beacon×http 实测通过 |
| **5c** | HTTPS：`channel_http.c` 的 TLS 封装（需要不要求客户端证书的 TLS 客户端） | 5b | 证书校验/SNI；`https://` 目前明确报错 |
| **5d** | session × http 全量 RPC 回归（当前只验到 beacon×http 与 session×mtls） | 5b | 轮询语义下的长任务 |
| **6** | DNS / WireGuard 通道 | 阶段 5 的 POLL 抽象 | DNS 分片/重排；WG 需要 `wg_*` secrets |
| 7 | beacon 缓冲式 shell（可选） | 阶段 4 | 背压与体验 |
| 8 | 按模式裁剪产物（可选） | 阶段 5/6 | 构建配置翻倍，回归网需各跑 |

### 阶段 5 的关键判断

`channel.h` 已预留 `POLL` 与 `poll()`，`channel_http.c` 只需实现 `send/poll/close` 并在 `sliver_channel_dial` 里加一个 scheme 分支 —— **session.c / beacon.c 一行不改**。
真正的工作量在**配置侧**：官方 HTTP 传输需要 `c2profile`（URL、headers、`poll-timeout`），目前 `config.json` 里只有 `poll_timeout` 一个字段。这部分建议先单独做一次"契约扩展"提交（纯数据，零运行时风险）。

---

## 7. 版本标记

| 仓库 | tag | commit | 含义 |
|---|---|---|---|
| `sliver` | `v0.11.0` | `de13a0a9` | 阶段一收口（纯 session 基线） |
| `sliver` | `v0.12.0` | `836dda68` | session + beacon（阶段 0–4） |
| `sliver-implant-cpp` | `v0.11.0` | `f79ce2e` | 同上（子模块） |
| `sliver-implant-cpp` | `v0.12.0` | `98ef2db` | 同上 |

两个仓库的 tag 现已一致，`git checkout v0.12.0` 在任一仓库都可用（父仓库记得 `git submodule update --init --recursive`）。
