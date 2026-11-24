# 审计：session 发布构建失效 + beacon 不上线（v0.10.0 后）

## 现象

1. `generate-cpp`（session）产出的 exe 只有 ~385 KB，运行后完全不上线。
2. `generate-cpp beacon` 产出的 exe 能打印 `beacon: registered`，但 server 端 `beacons` 为空，并伴随：
   ```
   yamux: Failed to read stream data: ... An established connection was aborted by the software in your host machine.
   yamux: Failed to write header: use of closed network connection
   ```

## 方法（证据优先，不靠猜测）

1. 本地按 cppbuild 的真实参数（`-G "Visual Studio 17 2022" -A x64`、`--config Release`、`IMPLANT_GUI` / `IMPLANT_DEBUG_LOG` 按 config 推导）复现构建。
2. 用二进制字符串指纹判断"某功能是否真的被链接进去"（`MUX/1`、`session: handshake ok`、`starting from injected config`、`crypto disabled build`）。
3. 对比用户构建目录的 `CMakeCache.txt`（`CMAKE_HOME_DIRECTORY`、`IMPLANT_*` 开关）。
4. 用 Go mTLS 对端（`tests/mtls-testserver`）+ 完整 harness 做端到端判定（含 shell 291 KB 输出、chinese-ok）。

## 结论与根因

### R1（session 不上线，主因）`IMPLANT_LTO=ON` + `IMPLANT_STRIP=ON`

用户的 session 构建缓存：

```
IMPLANT_CRYPTO=ON      IMPLANT_GUI=OFF
IMPLANT_DEBUG_LOG=ON   IMPLANT_LTO=ON      IMPLANT_STRIP=ON
```

同一源码树的 beacon 构建缓存里**没有** `IMPLANT_LTO`/`IMPLANT_STRIP` 条目。二进制指纹对比：

| 二进制 | 体积 | MUX/1 | session: handshake ok | starting from injected config |
|---|---|---|---|---|
| session（ON/ON） | 385 536 | 缺 | 缺 | 缺 |
| beacon（无条目） | 901 120 | 有 | 有 | 有 |

即：带 `/GL`+`/LTCG`+strip 的那份产物里**传输/会话代码整段不在**，程序启动后不连接就退出。这是"能链接但无功能"的构建，无法从产物本身诊断。

修复：`IMPLANT_LTO` 默认改为 **OFF**（`cmake/SliverOptions.cmake`）。体积换确定性。

### R2（静默无功能二进制）`IMPLANT_CRYPTO=OFF` + `FORMAT=exe`

`tls/frame/yamux/session/beacon/implant.c` 都在 `if(IMPLANT_CRYPTO)` 源列表里；OFF 时 `main.c` 走 `#else` 直接 `return 3`，产物能链接但不连网。属于同类静默失败。

修复：`CMakeLists.txt` 在 exe 分支加 `FATAL_ERROR`，crypto=OFF 的 exe 在 configure 阶段就失败并给出原因。

### R3（beacon 注册丢失 + yamux 报错）关闭连接过早

Go beacon 的顺序是 `Send(BeaconRegister)` → `Sleep(1s)` → `Close()`。C 侧发送后立即 `beacon_disconnect()`，server 还没消费完帧连接就断了 → 注册丢失 + `yamux ... connection aborted`。

修复：register 与每次 check-in 发送后各加 `sliver_sleep_ms(1000)` 再断开。

### 已在前序提交修好、本次复核确认的部分

| 项 | 结论 |
|---|---|
| `SLIVER_IS_BEACON` 解析（`ON` vs `true`） | 已修（`if(SLIVER_BEACON)`），header 实测为 1 |
| BeaconRegister/BeaconTasks 缺 Envelope 外壳 | 已修（type 93/94 包裹） |
| beacon 用 build_id（非 UUID）当注册 ID | 已修（运行时生成 UUIDv4） |
| session/beacon 入口分派（`sliver_config_is_beacon`） | 正常 |
| Release（`--config Release` + DEBUG_LOG）TLS 握手 | 正常（harness 全绿） |

## 验证基线（本轮）

- `--selftest-all` PASS（13 模块）
- 完整 mTLS harness 全绿：handshake ok / REGISTER / LS, PWD, CD, MKDIR, PS, ENV, RM, UPLOAD, DOWNLOAD, EXECUTE, ASYNC EXECUTE, TUNNEL BIND, PORTFWD, IMPLANT REPLY / SHELL（507 帧、291 KB、chinese-ok=true、big-output-ok=true）/ 3 次重连循环
- `IMPLANT_CRYPTO=OFF` + exe：configure 阶段 FATAL_ERROR（预期的显式失败）

## 仍需注意

1. **已有的旧构建目录**缓存了 `IMPLANT_LTO=ON`，改默认值不影响它们；`generate-cpp` 每次用随机名字建新目录，因此新构建自动生效。若在复用目录，删掉 `~/.sliver/slivers/<os>/<arch>/<name>/build` 再生成。
2. beacon 的 `recv` 仍用阻塞 `sliver_yamux_accept`，server 不响应时会停在 recv（后续可加 poll 超时）。
3. beacon 的 shell/portfwd（交互式隧道）未接 sender，属 session 导向功能；execute/ls/ps/env 等已可用。
4. LTO 关闭后体积会回到 ~850 KB 量级；若确实需要压缩体积，应单独用 `-DIMPLANT_LTO=ON` 并重跑完整 harness 验证，而不是作为默认。
