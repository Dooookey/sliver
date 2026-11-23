# 全量审计与修复 — 最终状态（v0.10.0）

三路只读审计（执行崩溃/并发、架构分层、协议对齐）+ 逐项修复后的收尾状态。

## 1. 已修复（全部本地验证，按提交顺序）

| # | 问题 | 修复 | 提交 |
|---|---|---|---|
| R1 | execute/shell 崩溃根因：`sliver_thread_detach` 在 worker trampoline 读取 `t->fn/t->arg` 前 `free(t)` | trampoline 独占 `sliver_thread_t` 并在退出时 free；join/detach 只释放句柄 | d791132 |
| R2 | yamux 写路径把 `pump()==1`（TLS 100ms 超时=暂无帧）当硬错误，多帧响应被丢 | rc==1 重试+睡眠 | d791132 |
| R3 | session teardown 销毁 tx_lock，detached worker 仍可能进入 | tx_lock 进程级保活 | af61077 |
| R4 | fs.h/proc.h 声明落在 include guard 之外 | 移回 guard 内 | af61077 |
| R5 | 6 个平台头缺 extern "C" | 统一补 | af61077 |
| R6 | posix 缺 token_last_error / acp_from_utf8 | 补 stub | af61077 |
| R7 | win32/registry.c 用 malloc/free 未含 stdlib.h（x64 指针截断） | 补 stdlib.h | c8f11ec |
| R8 | yamux streams[] 无公共锁（send 走 tx_lock、pump 走 pump_lock） | streams_lock + 固定锁序 | c5a166e |
| R9 | g_tx 指向栈对象，teardown 后 worker 悬垂 | static tx + closed 标志 + 锁内失效 | c31c847 |
| R10 | mbedtls_ssl_write 在 WANT_READ 时直接"稍后重试"，不排空握手后记录 | 排空一次再重试 | 3debe85 |
| R11 | portfwd 乱序帧静默丢弃 | 缺帧发 Resend（对齐 shell） | fb31348 |
| R12 | privs C4456 告警 | 变量改名 | fb31348 |
| R13 | execute 缺 Stdout/Stderr/PPid；netstat 缺 Executable | 补字段 + output 重定向 + Toolhelp 解析进程名 | 93d6b03 |
| R14 | shell sequence/read_sequence 跨线程无锁 | per-tunnel seq_lock | 94aae5b |
| R15 | log 写无锁 + 锁只在 debug 初始化 | 写加 g_lock + 无条件 init | 18ac8df |
| R16 | yamux 帧头被 100ms 超时劈开 / 单槽 pending / 写窗口无界 | 帧头缓冲 + 30s body 超时 + 10s 写超时 + 32 槽 pending | 3e3a5a1 |
| R17 | execute 缺 PPid；shell 中文未在 harness 覆盖 | PPid 走 PROC_THREAD_ATTRIBUTE_PARENT_PROCESS；harness 发送 `echo 中文测试` | 344dd7d |

## 2. P2 架构（6 项）

| # | 项 | 状态 | 提交 |
|---|---|---|---|
| #1a | proto.c 拆出 crypto 门控 | ✅ | 56b6761 |
| #1b | crypto=OFF 可构建/链接/运行 | ✅（实测 exit 3） | 0699022 |
| #2 | shared DLL 跑会话循环 | ✅（106 KB DLL） | 4e60ad0 |
| #5 | dispatch 宏化 + spawn-dll 注册 | ✅ | 49b91be |
| #3 | 传输抽象 vtable | ✅ | b181f37 |
| #4 | proto encoder 下沉 | ⏳ 唯一剩余 | — |

## 3. 验证基线（v0.10.0）

- `--selftest-all PASS`（13 模块）
- `ctest 3/3`（config / crypto_kat / wire_vectors）
- 端到端 harness **全绿**：`ASYNC EXECUTE / EXECUTE / TUNNEL BIND / PORTFWD(+echo) / IMPLANT REPLY / SHELL`（488 帧 291 KB，big-output-ok=true，**chinese-ok=true**）
- crypto=OFF：configure+build 成功，exe exit 3
- shared：`msvc-x64-dll` 构建出 `implant.dll`（106 KB）
- 实机（operator 确认）：register、文件、execute、shell（中文）、portfwd、screenshot、getprivs、services、registry、make-token、rev2self

## 4. 仍待办

| 级别 | 项 | 落点 |
|---|---|---|
| P2 #4 | proto.c 反向依赖 platform 头：~14 个平台依赖编码器下沉到 `domain_enc.c`（fs/proc/net/privs/services/registry），proto.c 只留 buf/reader/envelope + 纯 proto 编码器 | `src/proto/proto.c` |

## 5. 里程碑

- `v0.6.0-p4`（P4 权限/服务/信息子集）
- `v0.7.0-p1`（阶段 A 收口）
- `v0.8.0`（全量审计 + P0/P1 修复）
- `v0.9.0`（P2 架构 5/6：构建选项、分层、传输抽象、dispatch 宏化；shell harness 转绿）
- **`v0.10.0`（execute PPid + shell 中文 chinese-ok 转绿；P2 剩余 #4 唯一）**
