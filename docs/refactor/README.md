# Sliver Implant C/C++ 重构规划（总览 · v2）

> 目标：把 Sliver 的 **implant 端** 用 C/C++ 重写，**沿用原版 server 的线协议**（server 一行协议代码都不改），
> 平台 Windows / Linux，编译器 MSVC / MinGW-w64 / Clang，构建统一 CMake，产物**静态链接**、体积 1–5 MB（争取 KB 级）。
> 全部工作在 `D:\GOPATH\src\C2\sliver`（fork `Dooookey/sliver`，分支 `feat/implant-cpp`）。

**v2 的三条新约束（本版按此重排）**

1. **协议零改动**：C implant 必须能与现有 `sliver-server` 直接互通，走 `Envelope + yamux + 74B 签名帧` / `HTTP + age + ChaCha20-Poly1305` 原样协议。
2. **`GenerateCpp` 复刻官方 `Generate`**：同样的校验、同样的 `generate.GenerateConfig`、同样的 HTTP C2 profile、同样的 `ImplantBuildSave`，
   只把「编译后端」从 `go build` 换成「按 arch 选工具链的 CMake」。官方 Go 生成链路**不碰一行**。
3. **静态 + 体积**：硬上限 5 MB，目标 ≤512 KB，挑战 ≤150 KB；构建期就做静态性校验与体积断言（不是事后人工看）。

---

## 0. 结论先行

**协议可以完全复用，不需要动 server。** Sliver 1.7.8 的 implant↔server **不是 gRPC**，应用层只有 `sliverpb.Envelope`：
mtls/wg 走 `TLS1.3(+ 客户端证书) → "MUX/1" → yamux → 每 Envelope 一条 stream → [74B Ed25519 签名][u32 LE 长度][protobuf]`；
http 走 `HTTP/1.1 长轮询 + age(X25519) 寄送 32B 随机会话密钥 + gzip + ChaCha20-Poly1305`。
C 侧要复刻的是这套「帧 + 密码学 + yamux」，工作量集中在协议栈和平台能力层。

**体积可以做到 KB 级**——前提是**不要用 OpenSSL**。本版的密码学底座改为 **mbedTLS 3.6 LTS（TLS 1.3）+ monocypher（Ed25519/BLAKE2b）+ miniz/zlib**，
OpenSSL 只保留为「开发期对拍/参考实现」，不进产品构建。实测骨架：Windows MSVC `/MT` 静态 **150,016 B**，Linux glibc 静态 863,200 B（换 musl 预计 40–80 KB）。

---

## 1. 范围

| 在范围内 | 不在范围内 |
|---|---|
| `implant-cpp/`：C/C++ implant（协议栈 + 平台层） | server / client 的 Go 逻辑重写 |
| `server/cppbuild/`：CMake 构建后端（新增包） | 修改官方 `server/generate`、`server/c2` 任何代码 |
| `server/rpc/rpc-generate-cpp.go`：`GenerateCpp`（镜像 `Generate`） | 新协议、新消息号 |
| `cmake/` + presets + 静态/体积门禁 + CI | GUI / 桌面端 |
| `docs/refactor/`：调研、架构、构建、里程碑、风险、GenerateCpp 方案 | macOS 产物（预留不支持） |

---

## 2. 关键决策（v2 修订版）

| # | 决策 | 选择 | 理由 |
|---|---|---|---|
| D1 | 协议 | 逐字节复用 Go server 协议 | Envelope 无版本协商字段，改协议等于改 server |
| D2 | 生成链路 | `GenerateCpp` **逐句镜像** `Generate`，只换编译后端 | 「原版逻辑不受入侵」，出问题可一键回退到 Go implant |
| D3 | TLS/密码学 | **mbedTLS 3.6 LTS**（TLS1.3 + X25519 + AES-GCM/ChaCha20-Poly1305 + SHA256/HKDF + ECDSA P-256/384/521）+ **monocypher**（Ed25519、BLAKE2b-256/512）+ **miniz**（gzip） | OpenSSL 静态 1.5–3 MB，直接顶穿体积预算；mbedTLS 最小配置 mTLS 约 70 KB 量级（Cortex-M33 实测），x86_64 数百 KB 内；**mbedTLS 3.6 已支持 TLS 1.3**（早前文档的判断作废） |
| D4 | 复用层 | 自研 yamux（C，约 1.2k 行） | 线缆协议，必须实现；C 侧无成熟等价库 |
| D5 | protobuf | 自研 codegen（.proto 子集 → C struct + varint） | 只覆盖 P0/P1 消息；可产出金标向量 |
| D6 | 配置注入 | `config.json` → CMake `configure_file` → `implant_build_config.h` + `implant_secrets.c`（单 TU） | 等价 Go 模板注入；密钥隔离在一个 TU，便于 XOR/剔除 |
| D7 | 运行模型 | 专有线程 + 有界队列 + 定时器轮，无异常无 RTTI | 替代 goroutine；Win32/pthread 双实现 |
| D8 | 产物 | `exe` / `dll` / `so`；shellcode 走外置转换（donut/malasada 等价物） | 与官方形态对齐，一期不做转换器 |
| D9 | 许可 | implant 用 GPLv3；依赖只用 GPLv3 兼容（mbedTLS Apache-2.0、monocypher CC0/BSD-2、miniz MIT） | Sliver 本身 GPLv3；wolfSSL(GPLv2-only) 排除 |
| D10 | 静态链接 | Windows `/MT`（MSVC）或 `-static-libgcc -static-libstdc++`（MinGW）；Linux `-static`（优先 musl） | 产物零运行库依赖；`cppbuild` 构建后自动校验 |
| D11 | 体积门禁 | 硬上限 5 MB（失败），目标 512 KB（警告），挑战 150 KB | 让体积回归在 CI 里可见，不靠人盯 |
| D12 | 生成参数来源 | 全部从 `clientpb.ImplantConfig`/`ImplantBuild` 推导（GOOS/GOARCH/Format/Debug/IsBeacon/C2/Limits/HTTP profile） | 与官方同源，避免两套语义 |

---

## 3. 里程碑（「先打通」重排，详见 04）

| 里程碑 | 内容 | 人日 | 验收信号 |
|---|---|---|---|
| **P0 打通** | `GenerateCpp` + `cppbuild` + 配置注入 + mbedTLS TLS1.3 + yamux + 签名帧 + 最小 protobuf（Envelope/Register/Ping） | 8 | `sliver > generate --cpp …` 出文件 → 运行 → `sessions` 出现该 implant → `ping` 往返成功；产物静态且 ≤512 KB |
| **P1 可用** | 文件（ls/cd/pwd/download/upload/rm/mkdir）、进程（ps/terminate）、执行（execute）、隧道底座 + shell(PTY) | 10 | 交互式会话能执行命令、传文件、开 shell |
| **P2 传输扩展** | beacon 模式、HTTP(S) 传输（age 会话密钥、nonce/encoder、cookie） | 10 | `beacons` 列表 + 任务下发/回收；`--http` 可回连 |
| **P3 横向** | DNS 传输、pivot、rportfwd、socks5 | 12 | 三条传输任选可回连；隧道类命令可用 |
| **P4 平台深度** | Windows 令牌/注册表/服务；注入/CLR/BOF 单独立项 | 15 | 平台命令全通过 |
| **P5 硬化** | 体积收敛、字符串集中、导入表最小化、CI 门禁 | 5 | 产物 ≤150 KB（挑战）/≤512 KB（默认），`strings` 无绝对路径 |

> P0 的 8 人日不含 mbedTLS/yamux 的深度优化，含「能注册 + 能 ping + 静态 + 体积达标」这条最短链路。

---

## 4. 目标仓库结构

```
sliver/
├── implant/                     # 官方 Go implant（不动，作为协议基准与对拍）
├── implant-cpp/                 # 新增：C/C++ implant
│   ├── CMakeLists.txt / CMakePresets.json
│   ├── cmake/                   # 选项、告警、配置生成、工具链（msvc/mingw/mingw-386/mingw-arm64/clang-cl）
│   ├── include/sliver/          # config.h（含密钥槽枚举）、log.h、platform.h
│   ├── src/{core,proto,crypto,wire,transport,modes,rpc,tunnel,platform,entry}
│   ├── tests/{unit,vectors,fuzz}
│   └── tools/codegen/
├── server/
│   ├── cppbuild/                # 新增：CMake 构建后端（target/artifact/config/size）
│   ├── rpc/rpc-generate-cpp.go  # 新增：GenerateCpp（镜像 Generate）
│   └── generate/、c2/           # 官方实现，不改
└── docs/refactor/               # 本规划 01–06
```

---

## 5. 现在就能跑的东西

```powershell
# Windows：静态 exe（146 KiB）+ 静态 DLL，导出 StartW
cmake --preset msvc-x64      ; cmake --build --preset msvc-x64
.\build\msvc-x64\bin\implant.exe --self-test

# WSL：静态 ELF（现阶段 863 KiB，glibc；换 musl 后 40–80 KiB）
cmake --preset wsl-gcc       ; cmake --build --preset wsl-gcc
./build/wsl-gcc/bin/implant --self-test
```

`server/cppbuild` 已实现：按 `GOOS/GOARCH/Format` 选工具链、写 `config.json`、跑 `cmake -S/-B`、收集产物、
**校验 PE/ELF + 静态性（PT_INTERP / VC 运行时导入表）+ 体积（5 MB 硬限、512 KB 警告）**，并 `go build` 通过。

---

## 6. 文档索引

| 文档 | 内容 |
|---|---|
| [01 源码调研与协议契约](01-源码调研与协议契约.md) | 运行模型、传输/加密/协议字典、逐字节契约、MVP 消息分级 |
| [02 架构设计](02-架构设计.md) | 分层、线程/内存模型、传输实现、**密码学与体积策略（v2）**、codegen、配置注入 |
| [03 构建与工具链](03-构建与工具链.md) | CMake 结构、三编译器差异、**静态链接矩阵与体积门禁（v2）**、CI |
| [04 里程碑与验收](04-里程碑与验收.md) | **P0 打通 → P5 硬化**、每阶段 DoD 与命令 |
| [05 风险与开放问题](05-风险与开放问题.md) | 兼容/许可/体积/维护风险、待决问题 |
| [06 GenerateCpp 改造方案](06-GenerateCpp改造方案.md) | 镜像 `Generate` 的代码结构、`config.json` 契约、CMake 参数映射、错误处理 |
