# 交接文档（v0.14.0）— 新会话从这里开始

本文是给"接替工作"的新会话准备的**唯一入口**。读完本文 + `23-http-c2-protocol.md` 即可动手，
不需要重新翻 Go 源码。

---

## 1. 仓库与分支

| 角色 | 路径 | 分支 | v0.14.0 指向 |
|---|---|---|---|
| 服务端（Go，含 generate-cpp） | `D:\GOPATH\src\C2\sliver` | `feat/beacon` | `bf33a98b` |
| C/C++ 植入体（子模块） | `D:\GOPATH\src\C2\sliver\implant-cpp` | `feat/beacon` | `1c216f3` |

**铁律**：任何 implant-cpp 的提交，都必须在 sliver 里 `git add implant-cpp` 再提交一次（更新子模块指针）。
历史上两次"改了没生效"都是漏了这一步。

---

## 2. 实测可用矩阵（v0.14.0）

| 组合 | 上线 | 请求/响应命令 | 交互 shell | 大响应 |
|---|---|---|---|---|
| session × mtls | ✅ | ✅ | ✅ | ✅ |
| beacon × mtls | ✅ | ✅ | ⛔（能力位拒绝） | ✅ |
| beacon × http(s) | ✅ | ✅ | ⛔ | ✅ |
| session × http | ✅ | ✅ | ✅（逐帧 POST，延迟较高） | ✅（66KB 实测） |

生成命令：
```powershell
generate-cpp --mtls 127.0.0.1:8888 --os windows --arch amd64 --format exe --debug
generate-cpp beacon --mtls 127.0.0.1:8888 --os windows --arch amd64 --format exe --debug --seconds 10 --jitter 3
generate-cpp --http http://127.0.0.1:80 --os windows --arch amd64 --format exe --debug
generate-cpp beacon --http http://127.0.0.1:80 --os windows --arch amd64 --format exe --debug --seconds 10 --jitter 3
```
> listener 是明文时必须写 `http://`；写成 `https://` 会得到 `tls: handshake failed rc=-0x7200`。

---

## 3. 代码地图（改哪找哪）

```
src/entry/main.c          CLI/自检/GUI 入口；--selftest-all
src/modes/implant.c       config 装配(sliver_session_config_build) + is_beacon 分派
src/modes/session.c       长连接调度（register→accept 循环→teardown）+ 能力位
src/modes/beacon.c        短连接调度（register→checkin→pending 队列→jitter）
src/modes/shell.c         隧道子进程（20/21/22/23/135）
src/modes/portfwd.c       TCP 中继（80/81）
src/rpc/dispatch.c        请求→响应表；-2 = 异步；能力位实现
src/proto/proto.c         核心编解码 + beacon 编解码
src/proto/domain_enc.c    文件/进程/网络/权限/服务/注册表 编码器
src/wire/frame.c          签名帧（74B minisign + u32 len + payload）
src/wire/yamux.c          多路复用（仅 STREAM）
include/sliver/channel.h  信道接口（STREAM/POLL、send/recv/poll/close、dial）
src/transport/channel.c   按 scheme 分派：mtls→channel_mtls，http(s)→channel_http
src/transport/channel_mtls.c  TLS+MUX/1+yamux+frame（原 session.c 的管道）
src/transport/channel_http.c  POLL 通道（vtable 已指向 http_session）
src/transport/http_session.c  真实 listener 会话：age 交换+cookie+cipher+encoder+长轮询
src/transport/tls_client.c     TLS（mTLS 与 plain/insecure 两用）
src/crypto/gzip.c         gzip（stored 压缩 + 完整 DEFLATE inflate）
src/crypto/cipher.c       CipherContext 帧：nonce12‖AEAD(gzip)
src/crypto/age.c          age v1（stanza 已按 epk‖rp 修正）
src/crypto/age_keyex.c    AgeKeyExToServer + peer 公钥派生
src/crypto/encoders.c     nop/hex/base64/base32/gzip（Sliver 自定义字母表）
src/crypto/minisign.c     签名/验签
src/core/config.c         注入配置访问器（含 http profile、encoder id）
cmake/SliverConfigGen.cmake  JSON→宏 + implant_secrets.c + implant_http_c2.c
```

**分层约束**：`entry → modes → rpc → proto/wire → transport → crypto`，`core/platform` 横切。
`modes/` 内禁止出现 `sliver_yamux_*` / `sliver_tls_*`。

---

## 4. 回归门禁（每次改动都要跑）

```powershell
cd D:\GOPATH\src\C2\sliver\implant-cpp
# 1) 单元/对拍测试（与 Go 逐字节对拍）
ctest --test-dir build/msvc-x64 -C Release            # 期望 7/7

# 2) HTTP 全链路（离线桩：握手→注册→任务→结果）
tests\scripts\http-stub-selftest.ps1 -Port 9796       # 期望 "http stub selftest passed"

# 3) mtls 全量（13 类 RPC + shell 400+ 帧 + 3 轮重连）
tests\scripts\inject-config-selftest.ps1 -Port 9794
tests\scripts\inject-config-selftest.ps1 -Port 9794 -Beacon
```
Go 工具离线构建（依赖已在 GOMODCACHE）：
```powershell
$env:GOPROXY='off'; $env:GOFLAGS='-mod=mod'
```

---

## 5. 已知坑（踩过的，别再踩）

| 坑 | 现象 | 处置 |
|---|---|---|
| 增量构建链接旧 `sliver_core.lib` | 产物 385KB、无 `MUX/1`、不上线 | 改源文件列表后删构建目录 |
| LTO 默认曾为 ON | 同上（现已默认 OFF） | 勿随意改回 |
| `crypto=OFF` + exe | 产物不连网 | 已改 configure 期 FATAL_ERROR |
| beacon 注册日志假成功 | `beacon: registered` 但没上线 | 已修（检查 send 返回值） |
| PowerShell `git commit -m "带引号"` | 参数被拆坏 | 用 `-F 消息文件` |
| HTTP 请求缺 nonce | 服务端 404 | `session_target()` 已自动加，勿删 |
| poll 响应带 74B 签名头 | 解密失败 | 已兼容两种布局 |
| session over http 静默退出 | 无日志 | 已修（mTLS 前置检查仅对 mtls://） |

---

## 6. 剩余工作（按优先级，含落点）

### A. 完成 3.2 编码器实测（最小改动）
1. `src/transport/http_session.c` 顶部 `#define SLIVER_HTTP_SESSION_ENCODER SLIVER_ENCODER_NOP`
   改为 `SLIVER_ENCODER_BASE64`；
2. 跑桩：`$env:STUB_ENC_MAP="101:base64"`（id 需与测试配置 `encoders.base64` 一致），
   执行 `http-stub-selftest.ps1`；
3. 依次换 `HEX`(104) / `BASE32`(103) / `GZIP`(106) 各跑一遍；
4. 全绿后把默认值改成运行时选择："优先 base64，缺失则 hex，最后 nop"。
> **风险**：编码不一致会 404；**收益**：摆脱 NoEncoder 恒等编码。

### B. 3.4 上传侧签名头
- 现状：请求体是裸 `nonce‖AEAD`，服务端当前接受；
- 目标：`[74B minisign][nonce‖AEAD]`（服务端 `CipherContext.Decrypt` 会剥 74 字节）；
- 做法：`frame.c` 暴露"只签名不写长度"的辅助；`http_session.c` send 前调用；
- 验证：先让桩开启校验（红）→ 改 implant → 绿。

### C. 隧道帧批量 flush（性能）
- 现状：session×http 下每帧隧道数据一次 POST，交互延迟明显；
- 目标：`http_session_send` 支持把多帧攒批（如 50ms 或积满 8 帧）一次提交；
- 落点：`http_session.c` + `channel_http.c` 的 send 回调；**不要动** `shell.c`。

### D. 桩补 beacon 模式
- 目标：桩实现 `BeaconRegister(93)/BeaconTasks(94)`，把 beacon×http 纳入离线回归；
- 落点：`tests/gzipfix/stub.go`；断言 `STUB BEACON REGISTER ok` / `STUB BEACON CHECKIN ok`。

### E. 3.6 DNS / WG 通道
- `channel.h` 已预留 `poll()`；`channel.c` 加分派分支即可，调度器零改动；
- 前置：`config.json` 补 DNS/WG 字段（`cppbuild/config.go` + cmake 宏 + C 访问器）。

### F. 可选
- beacon 缓冲式 shell（**必须做背压**：队列满时暂停 reader）；
- 按 `SLIVER_IS_BEACON` 裁剪产物（省 5~7KB，但构建配置翻倍）；
- 心跳间隔可配置。

---

## 7. 关键设计约束（改动守则）

1. `modes/` 只经 `channel.h` 访问传输；
2. 禁止文件级 static 的 yamux/tls/transport/channel 实例；
3. beacon 不得复用 `session_tx_t` / `g_tx`；
4. 两模式的 sender 各自注册，不共享 static 适配器；
5. 新增命令：`proto.h` 加 id → `proto.c`/`domain_enc.c` 编解码 → `dispatch.c` 加表项；
   需要隧道的在 handler 顶部检查 `SLIVER_CAP_TUNNEL`；
6. 每次改动：ctest + 对应 harness；HTTP 相关再加真实 listener 实测。

---

## 8. 版本历史（里程碑）

| tag | implant-cpp | sliver | 含义 |
|---|---|---|---|
| v0.11.0 | `f79ce2e` | `de13a0a9` | 纯 session 基线（阶段一收口） |
| v0.12.0 | `98ef2db` | `836dda68` | session + beacon（mtls） |
| v0.13.0 | `5e6283f` | `7beb0d16` | + beacon×http(s)（**session×http 尚未修**） |
| **v0.14.0** | **`1c216f3`** | **`bf33a98b`** | **四模式全绿 + 离线 HTTP 回归网** |
