# 全量审计与修复 — 最终状态（v0.8.0）

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

## 2. 验证基线

- `--selftest-all PASS`（13 个模块）
- `ctest 3/3`（config / crypto_kat / wire_vectors）
- 端到端对端：`ASYNC EXECUTE ok / EXECUTE ok / TUNNEL BIND ok / PORTFWD ok + DATA echo / IMPLANT REPLY ok`
- 实机（operator 确认）：register、文件、execute、shell（中文）、portfwd 请求路径、screenshot、getprivs、services、registry、make-token、rev2self

## 3. 仍待办（均已定位到文件/行号）

| 级别 | 项 | 落点 |
|---|---|---|
| 测试侧 | harness 的 shell 阶段 `want type 21 got 0`：Shell 响应 TLS 写返回 -2 重试 200ms 后失败，实测是对端（mtls-testserver）停读导致的流控，非植入体 bug；实机 shell 正常 | `tests/mtls-testserver/main.go` 的 shell 用例读流方式 |
| P2 | execute 的 PPid 真正设置子进程父进程（需 `CreateProcess` 父作业/属性列表） | `win32/exec.c` |
| P2 | IMPLANT_CRYPTO=OFF 断链、IMPLANT_FORMAT=shared 空壳、无传输抽象、proto 反向依赖 platform、log 写路径加锁 | 见 docs/refactor/14-architecture-audit.md |

## 4. 里程碑

- `v0.6.0-p4`（P4 权限/服务/信息子集）
- `v0.7.0-p1`（阶段 A 收口）
- **`v0.8.0`（本次：全量审计 + P0/P1 修复）**
