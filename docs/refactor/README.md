# Sliver Implant C/C++ 重构规划（总览）

> 目标：把 Sliver 的 **implant（植入体）端** 用 C/C++ 重写，与现成 Go server 保持**线协议兼容**；
> 平台覆盖 Windows / Linux，编译器覆盖 MSVC / MinGW-w64 / Clang(含 clang-cl)，构建统一用 CMake；
> 全部工作在 `D:\GOPATH\src\C2\sliver`（fork: `Dooookey/sliver`）内以 git 分支管理。

---

## 0. 一句话结论

**可行，且不需要动 server 的协议实现。** 关键事实：Sliver 1.7.8 的 implant↔server **不是 gRPC**，应用层只有一种消息 `sliverpb.Envelope`（protobuf），
传输层是「TLS/WG + yamux + 每 Envelope 一条 stream + 74 字节裸签名」或「HTTP/1.1 + age 会话密钥 + ChaCha20-Poly1305 信封」。
因此 C/C++ 侧重写的工作量集中在 **协议栈复刻 + 平台能力层**，而不是「重写一个 C2 框架」。

已存在的起点：本地 `sliver-1.7.8` 工作副本里已有 `server/rpc/rpc-generate-cpp.go` 与 `services.proto` 的 `GenerateCpp` 原型
（生成 `sliver_config.h` + 调 CMake；fork 的 master 分支尚未包含）。本规划把它作为 **server 侧对接点**原样迁到 fork 后继续演进，而不是推翻。

---

## 1. 范围

| 在范围内 | 不在范围内（本阶段） |
|---|---|
| `implant-cpp/`：全新 C/C++ implant 工程 | server / client 的 Go 逻辑重写 |
| `server/rpc/rpc-generate-cpp.go`：生成链路改造 | 新增协议（保持与 Go implant 二进制级兼容） |
| `cmake/` + `CMakePresets.json` + 构建脚本 + CI | GUI / 控制台客户端 |
| `docs/refactor/`：调研、架构、构建、里程碑文档 | 移动端、macOS 产物（预留但不排期） |
| `tests/`：金标向量、协议对拍、fuzz 入口 | 反检测/免杀成品化（仅保留接口与阶段计划） |

---

## 2. 关键设计决策（ADR 摘要）

| # | 决策 | 选择 | 理由（证据见 01 文档） |
|---|---|---|---|
| D1 | 协议兼容策略 | **线协议逐字节兼容**，复用 Go server | Envelope 无版本号、无协商字段，改协议必须同时改 server，成本远高于复刻 |
| D2 | 传输范围 | 一期 `mtls` + `http(s)`；二期 `dns`；三期 `pivot/rportfwd/socks`；`wg` 单独立项 | `mtls` 协议面最窄（TLS 承担机密性），`wg` 需用户态 TCP/IP 栈，投入最大（见 02 §6） |
| D3 | TLS / 密码学底座 | **OpenSSL 3.x**（TLS1.3 + X25519/Ed25519/ChaCha20-Poly1305/SHA256/HMAC/HKDF）+ **libsodium**（BLAKE2b 可变输出） | server 强制 TLS1.3（`server/c2/mtls.go:459`），mbedTLS 3.x 不满足；BLAKE2b-256 keyID 必须参数化输出，OpenSSL 只有 512 |
| D4 | 复用层 | **自研 yamux（C 版，约 1.2k 行）**，移植 `vendor/github.com/hashicorp/yamux` 协议 | yamux 是线缆协议（12B 头、BE、window 256KiB），无法省略；C 侧无成熟等价库 |
| D5 | protobuf | **自研 codegen**：`.proto` 子集 → C 结构体 + varint 编解码（不走 nanopb/protobuf-c） | 只需要 wire format 的一个子集；`map<>` 仅 Grep 用到可暂缓；codegen 同时产出金标向量，便于逐字节对拍 |
| D6 | 配置注入 | CMake `configure_file` → `implant_build_config.h`（开关/常量）+ `implant_secrets.c`（密钥/URL，单 TU，可 XOR+随机种子） | 等价于 Go 侧 `text/template` 注入（`server/generate/binaries.go:823-1054`），且能保证「同参数两次构建产物不同」 |
| D7 | 运行模型 | 专有线程 + 事件队列（读线程/写线程/定时器轮/任务池），**无异常无 RTTI** | 替代 goroutine；Windows 用 Win32 原语、Linux 用 pthread，避免 winpthreads 依赖 |
| D8 | 产物形态 | 一期 `exe`（Win/Linux）+ `so/dll`；shellcode 走「先出产物再转换」的外置步骤 | 与 Sliver 现状一致（`binaries.go:346-587`），避免把 malasada/donut 逻辑搬进 C 工程 |
| D9 | 许可证 | C implant 采用 **GPLv3**（与 Sliver 一致），依赖只用 GPLv3 兼容许可 | Sliver 是 GPLv3；wolfSSL（GPLv2-only）排除，OpenSSL(Apache-2.0)/libsodium(ISC)/zlib 兼容 |
| D10 | 与 server 的对接 | 保留并扩展 `GenerateCpp`：config 全量注入 + 多工具链 + 构建缓存 + 产物落 `ImplantBuild` | 现有原型只注入了 6 个密钥字段与 GOOS/GOARCH，缺 C2 URL/节奏/限制/HTTP profile |

---

## 3. 里程碑一览（详见 04）

| 里程碑 | 内容 | 周期（1 人） | 验收信号 |
|---|---|---|---|
| **M0** | 工程骨架、CMake 三工具链、配置生成、金标向量测试 | 1 周 | MSVC + WSL-gcc 各出 exe；向量测试全绿 |
| **M1** | protobuf codec + age/AEAD + envelope 签名 + yamux + TLS1.3 → **mtls 上线** | 2 周 | `sliver` 控制台 `sessions` 出现 C implant |
| **M2** | 核心 RPC：ping/ps/ls/cd/pwd/download/upload/execute/shell(PTY) + 隧道底座 | 3 周 | 交互式会话可执行命令、传文件、开 shell |
| **M3** | beacon 模式 + HTTP(S) 传输（age 会话密钥、nonce/encoder、cookie） | 2 周 | `beacons` 列表 + 任务下发/结果回收 |
| **M4** | DNS 传输 + pivot/rportfwd/socks5 | 3 周 | 三条传输任选可回连；隧道类命令可用 |
| **M5** | WireGuard 传输（可选立项） | 3 周+ | `wg://` 可回连（取决于用户态栈方案） |
| **M6** | Windows 深度能力（令牌/注册表/服务/注入）+ 产物硬化 + CI 门禁 | 3 周 | 平台命令全通过；字符串/路径卫生断言通过 |

---

## 4. 目标仓库结构

```
sliver/
├── implant/                    # 现有 Go implant（保留，作为协议参考与对拍基准）
├── implant-cpp/                # 新增：C/C++ implant
│   ├── CMakeLists.txt
│   ├── CMakePresets.json
│   ├── cmake/                  # 工具链、选项、配置生成、告警
│   ├── include/sliver/         # 公共头（config/envelope/transport/...）
│   ├── src/
│   │   ├── core/               # 生命周期、配置、日志、错误、事件
│   │   ├── proto/              # codegen 产物 + varint 运行时
│   │   ├── crypto/             # age、AEAD、ed25519、minisign、blake2b
│   │   ├── wire/               # yamux、envelope 帧、dns 编解码
│   │   ├── transport/          # mtls / http / dns / pivot / wg
│   │   ├── rpc/                # handler 分发表与各功能实现
│   │   ├── tunnel/             # TunnelData 序号/窗口/shell/portfwd/socks
│   │   └── platform/           # win32 / linux 抽象实现
│   ├── third_party/            # openssl/libsodium/zlib（submodule 或 fetch）
│   ├── tests/                  # 金标向量、单测、fuzz
│   └── tools/codegen/          # .proto → C 代码生成器
├── docs/refactor/              # 本规划（01~05）
└── server/rpc/rpc-generate-cpp.go   # 生成链路（改造）
```

---

## 5. 下一步（M0 的第一天就能做）

1. `git switch -c feat/implant-cpp`（已在本规划提交中创建）。
2. 装依赖：WSL `apt install -y clang mingw-w64 libssl-dev zlib1g-dev ninja-build`；Windows 侧用已装 VS2022（MSVC 19.44）。
3. `cmake --preset msvc-x64 && cmake --build --preset msvc-x64` 出第一个 exe。
4. 跑 `tests/vectors` 里由 Go 侧生成的 golden vector（见 03 §5），把「协议对齐」变成可持续的回归测试。

---

## 6. 文档索引

| 文档 | 内容 |
|---|---|
| [01-源码调研与协议契约](01-源码调研与协议契约.md) | implant 代码地图、运行模型、传输/加密/协议字典、逐字节契约清单、MVP 消息分级 |
| [02-架构设计](02-架构设计.md) | 分层、模块目录、线程与内存模型、传输实现策略、codegen、配置注入、平台能力分期 |
| [03-构建与工具链](03-构建与工具链.md) | CMake 结构、三编译器差异、presets、脚本、WSL 环境、CI 矩阵与产物卫生 |
| [04-里程碑与验收](04-里程碑与验收.md) | M0–M6 任务拆分、验收命令、人日估算 |
| [05-风险与开放问题](05-风险与开放问题.md) | 兼容/许可/体积/维护风险，待你拍板的开放问题 |
