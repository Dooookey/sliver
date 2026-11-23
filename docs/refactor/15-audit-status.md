# 全量审计结论与修复状态

三路只读审计（执行崩溃/并发、架构分层、协议对齐）完成。本文记录根因、已修复项、以及带修复落点的剩余项。

## 1. 已修复（本轮，均已本地验证）

| # | 问题 | 修复 | 提交 |
|---|---|---|---|
| R1 | **execute/shell 会话崩溃根因**：`sliver_thread_detach` 在 worker 线程 trampoline 读取 `t->fn/t->arg` 之前就 `free(t)` → 野指针 → 访问违例 → 进程死亡 | trampoline 成为 `sliver_thread_t` 唯一所有者并在退出时 free；join/detach 只释放 OS 句柄不再 free | d791132 |
| R2 | yamux 写路径把 `pump()==1`（TLS 100ms 超时=暂无帧）当硬错误，多帧大响应被丢 | 写路径对 rc==1 重试+睡眠 | d791132 |
| R3 | session teardown 销毁 tx_lock，detached worker 仍可能进入 send_envelope | tx_lock 进程级保活（不销毁） | af61077 |
| R4 | fs.h/proc.h 声明落在 include guard 之外 | 移回 guard 内 | af61077 |
| R5 | 6 个平台头缺 extern "C" | 统一补 | af61077 |
| R6 | posix 缺 sliver_token_last_error / sliver_acp_from_utf8（符号不对称） | 补 stub | af61077 |
| R7 | win32/registry.c 用 malloc/free 未 include stdlib.h（x64 指针截断，同 getprivs/services 一类） | 补 stdlib.h | c8f11ec |

验证：`--selftest-all PASS`、`ctest 3/3`、端到端对端 `ASYNC EXECUTE ok / EXECUTE ok / TUNNEL BIND ok / PORTFWD ok / IMPLANT REPLY ok`。

## 2. 剩余（已定位，未修，附落点）

按优先级：

### P0 — 仍可能造成偶发断连
1. **yamux streams[] 数据竞争**：`open/write/close` 走 tx_lock，`pump` 走 pump_lock，对同一 `streams[]`/`send_window`/流标志无公共锁。
   落点：src/wire/yamux.c alloc_stream/find_stream/release_stream/maybe_release/sliver_yamux_open/write/close_write/pump。
   修法：新增 `streams_lock`，所有表与流标志读写统一持有；注意与 write_lock/pump_lock 的锁序，禁止持 streams_lock 时再调 send_frame。
   现象：harness 的 shell 阶段 `response send failed for type 20 → connection closed`（真机 shell 正常，但这是同一竞态的窗口）。

2. **g_tx 指向栈对象**：session.c:216 `g_tx=&tx`（tx 是 sliver_session_run 栈局部），worker 在 teardown 后读 g_tx 即悬垂。
   落点：src/modes/session.c + src/rpc/dispatch.c 的 rpc_send_adapter。
   修法：tx/tx_lock 堆分配 + 引用计数，或 teardown 前 join 所有 worker；g_tx 读写用仍存活的锁保护。

### P1 — 协议/功能
3. **portfwd 入站乱序帧静默丢弃**（shell 有 resend，portfwd 没有）。
   落点：src/modes/portfwd.c sliver_portfwd_tunnel_data（`sequence != read_sequence` 分支）。
   修法：建 pending 缓存 + 缺帧发 Resend（复用 sliver_tunnel_resend_encode）。

4. **execute 请求字段缺失**：Stdout/Stderr/Env/PPid 未解析。
   落点：src/proto/proto.c sliver_execute_req_decode + dispatch execute_worker。
   修法：补字段，`output && !background` 时写文件而非 tunnel。

5. **netstat Process 缺 Executable、execute-children 缺 Args/ExitCode/Exited**。
   落点：proto.c sliver_netstat_encode / sliver_execute_children_encode。

6. **shell sequence/read_sequence 跨线程无锁**（portfwd 有 f->lock）。
   落点：src/modes/shell.c，为 shell_tunnel_t 加锁。

### P2 — 架构扩展地基
7. IMPLANT_CRYPTO=OFF 断链、IMPLANT_FORMAT=shared 是空壳 DLL。
8. proto 层反向依赖 platform 头；dispatch 表混用魔法数字；netstat/mount req==resp 宏名误导。
9. 无传输抽象（transport.h 只有 TLS）；modes 层泄漏 windows.h。
10. 全局 sender 单例阻碍多会话；log 写路径不加锁。

详见 docs/refactor/14-architecture-audit.md 与三个审计子报告。

## 3. 建议顺序
P0 的 #1（streams_lock）→ #2（g_tx 堆化）→ P1 #3/#4/#5/#6 → P2。每步先本地 harness + --selftest-all 验证再提交。
