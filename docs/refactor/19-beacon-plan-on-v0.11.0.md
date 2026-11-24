# Beacon 模式增补：审计与开发规划（基线 v0.11.0 / `de13a0a9`）

目标：在 **session 模式保持稳定** 的前提下，按官方 `generate` 的语义为 `generate-cpp` 与
C/C++ 植入体增补 beacon 模式。本文只回答两件事：**哪里会碰坏 session**、**按什么顺序加
才可回滚可验证**。

---

## 1. 基线事实（代码级，已核实）

| 位置 | 事实 |
|---|---|
| `src/entry/main.c:434-435` | `run_from_build_config()` 是**一行包装**：`return sliver_implant_run(argv0);` |
| `src/modes/implant.c:14-110` | `sliver_implant_run` 内联做三件事：解析 C2 → 组装 `cfg`+`reg` → 无限重连循环调 `sliver_session_run` |
| `src/modes/implant.c:20,48` | `cfg.host` 指向**函数内栈缓冲** `host[256]`，生命周期与循环绑定 |
| `src/modes/session.c:191-192` | `static session_tx_t tx;` + `static sliver_mutex_t *tx_lock`（进程级、跨会话复用） |
| `src/modes/session.c:238-241` | 每次 `sliver_session_run` 入口都会重设 `g_tx` 与**三个 sender**（shell/portfwd/rpc） |
| `src/modes/session.c:343-349` | teardown 在锁内 `closed=1; y=NULL; signer=NULL; g_tx=NULL` |
| `src/core/config.c:28,29,32` | `sliver_config_beacon_interval_ns/jitter_ns/is_beacon` **已存在，当前无人调用** |
| `cmake/SliverConfigGen.cmake:60,154-158` | `beacon` / `beacon_interval_ns` / `beacon_jitter_ns` → `SLIVER_IS_BEACON` 等宏 |
| `server/cppbuild/config.go:20-23,103-106` | 服务端**已正确写入** `beacon`、`beacon_interval_ns`、`beacon_jitter_ns` |
| `client/command/cppgen/commands.go` | 当前用 `--beacon` **flag**（官方 `generate` 是 `beacon` **子命令**） |

**结论一：服务端 cppbuild 的 config.json 契约已经是 beacon-ready，不需要改。**
**结论二：beacon 的缺口只在三处——(a) CMake 布尔解析、(b) 入口分派、(c) wire 编解码与循环。**

## 2. 基线里仍然存在的三个缺陷（必须先修，与 beacon 是否开发无关）

| 代号 | 缺陷 | 后果 | 证据 |
|---|---|---|---|
| **D1** | `SliverConfigGen.cmake:64` 用 `STREQUAL "true"` 比较，而 CMake `string(JSON GET)` 对 JSON 布尔返回 **`ON`** | beacon 构建得到 `SLIVER_IS_BEACON 0` → 走 session，永不出现 beacon | 实测 `beacon=[ON]` |
| **D2** | `SliverOptions.cmake:12` `IMPLANT_LTO` 默认 **ON**（`/GL`+`/LTCG`） | 用户实测产出 385 KB、**缺 `MUX/1`**、不上线的"能链接但无功能"产物 | 二进制指纹对比 |
| **D3** | `IMPLANT_CRYPTO=OFF` + `FORMAT=exe` 仍能配置并产出不连网的 exe | 静默失败，无法从产物诊断 | 源码在 `if(IMPLANT_CRYPTO)` 内门控 |

## 3. 接缝分析：session 与 beacon 的共享面 / 可隔离面

| 面 | session 归属 | beacon 必须怎么做 | 风险等级 |
|---|---|---|---|
| C2 地址解析 + register 组装 | 内联在 `sliver_implant_run` | **抽取为纯函数**，调用方持有 host 缓冲 | 中（易改坏） |
| mTLS + yamux + 签名帧 | `session.c` 栈上实例 | 各自持有栈上实例，**禁止文件级 yamux/tls 静态量** | 低（已隔离） |
| `session_tx_t` / `g_tx` | `session.c` static | **绝对不碰**，beacon 用自己的 `beacon_conn_t` | 高（若复用会清掉 session 状态） |
| `sliver_*_set_sender`（3 个） | `session.c:239-241` 每次重设 | beacon 启动时设自己的；session 下次运行会自动覆盖 → 自愈 | 中 |
| `sliver_rpc_dispatch` | 共用 | 共用即可（纯请求→响应，无全局态） | 低 |
| 全局 config 访问器 | 共用 | 共用（只读宏） | 无 |
| `main.c` 入口 | 1 行包装 | 在 `sliver_implant_run` 顶部加 3 行分派 | 低（`is_beacon==0` 时路径不变） |

## 4. Session 回归不变量（每一步之后必跑，任一失败即回滚该步）

1. `--selftest-all` PASS
2. `tests\scripts\inject-config-selftest.ps1` 全绿：`handshake ok` / `REGISTER ok` /
   `LS,PWD,CD,MKDIR,PS,ENV,RM,UPLOAD,DOWNLOAD,EXECUTE,ASYNC EXECUTE,TUNNEL BIND,PORTFWD,IMPLANT REPLY` /
   `SHELL`（`chinese-ok=true`, `big-output-ok=true`）
3. session 配置生成的 header 必须是 `SLIVER_IS_BEACON 0`
4. beacon 配置生成的 header 必须是 `SLIVER_IS_BEACON 1`
5. session 的 register 报文与基线**逐字节一致**（可用 `sliver_register_encode` 单测固化）

## 5. 分步计划（每步独立提交、可回滚）

| 步 | 内容 | 触碰文件 | 为什么不会伤 session | 验证 |
|---|---|---|---|---|
| **B0** | 修 D2/D3：`IMPLANT_LTO` 默认 OFF；`crypto=OFF`+exe 直接 `FATAL_ERROR` | `cmake/SliverOptions.cmake`、`CMakeLists.txt` | 只改构建默认与配置期校验，不产码 | 配置成功且 `IMPLANT_LTO:BOOL=OFF`；`crypto=OFF`+exe 报错；harness 全绿 |
| **B1** | 修 D1：`if(SLIVER_BEACON)` 取代字符串比较 | `cmake/SliverConfigGen.cmake` | 只影响 beacon 配置；session 配置 `beacon:false` → 仍为 0 | 用 beacon config 生成 header 得 1；用 session config 得 0 |
| **B2** | beacon wire 编解码：`BeaconRegister`/`BeaconTasks` encode + decode | `include/sliver/proto.h`、`src/proto/proto.c` | 纯新增函数，无调用方 | 编译 + 单测（编码→解码回环） |
| **B3** | 抽取 `sliver_session_config_build()`（**纯重构**） | `include/sliver/implant.h`、`src/modes/implant.c` | 语义等价；`cfg.host` 缓冲由调用方持有 | 不变量 5（register 字节一致）+ 全量 harness |
| **B4** | `src/modes/beacon.c`：连接/发送/接收 + 注册一次 + checkin 循环（**不自带 task 执行**） | 新增 `beacon.c`、`CMakeLists.txt`、`implant.h` | 新文件；`CMakeLists` 仅在 crypto 列表追加一行 | 编译；`--selftest-all`；session harness 不受影响 |
| **B5** | 入口分派：`if (sliver_config_is_beacon()) return sliver_beacon_run(argv0);` | `src/modes/implant.c`（+3 行） | `is_beacon==0` 时直接落回原路径 | session harness + beacon 实机注册 |
| **B6** | beacon 的 sender 接入 + pending 结果队列（异步 execute 结果回传） | `src/modes/beacon.c` | 队列为 beacon 进程级静态 + 互斥；session 用自己的 sender 注册，互不覆盖 | beacon 下 `execute` 有结果；session harness 仍绿 |
| **B7** | jitter 调度 + 收尾（1s 宽限、连接释放顺序） | `src/modes/beacon.c` | 同上 | 连续 3 个 checkin 无异常 |

> B0/B1 是"修既有缺陷"，与 beacon 功能解耦，建议先落地。B3 是唯一会碰 session 文件的重构，
> 单独一步、单独验证，便于出问题时精确回滚。

## 6. 服务端 `generate-cpp` 与官方 `generate` 的对齐

官方形态是 **`generate beacon` 子命令**（`coreBeaconFlags` 只挂在子命令上），当前 `generate-cpp`
用的是 `--beacon` flag。建议：

1. 新增 `generate-cpp beacon` 子命令：继承全部植入体 flags + 追加 `--interval/--jitter`，
   `IsBeacon` 由 Run 闭包传 `true`（不依赖 flag）。
2. 父命令加 `Args: cobra.NoArgs`：**避免 `generate-cpp beacon ...` 里 `beacon` 被当位置参数静默忽略**
   → 这正是之前"命令写对了却生成 session"的直接原因。
3. `--beacon` flag 可保留为兼容别名，但帮助文本标注 deprecated。
4. 服务端 `cppbuild` 无需改动（config.json 已含 beacon 三字段）；确认 `compile()` 传参不含
   `IMPLANT_CRYPTO/LTO`，保持由 CMake 默认决定。

## 7. 验收标准

- session：harness 全绿，连续 3 次重连循环无异常，`SLIVER_IS_BEACON 0`
- beacon：`beacons` 列表出现；每 interval(+jitter) checkin；`ls/execute` 通过 beacon 返回结果；
  无 `yamux: Failed to read stream data / Failed to write header`
- 构建：`IMPLANT_LTO:BOOL=OFF`，产物约 850–900 KB（非 385 KB），含 `MUX/1`
- 两条线互不影响：`git checkout feat/implant-cpp` 可随时回到纯 session 基线
