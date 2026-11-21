# 06 GenerateCpp 改造方案（镜像官方 Generate）

> 原则：**官方生成链路一行不改**。`GenerateCpp` 是 `Generate` 的逐句镜像，唯一差异在「编译后端」：
> `generate.SliverExecutable/SharedLibrary/...`（go build）→ `cppbuild.CompileExecutable/SharedLibrary/...`（CMake）。
> 这样 C/C++ 后端出任何问题，都能立刻切回 Go implant，不会污染原版逻辑。

---

## 1. 调用链对照

| 步骤 | 官方 `Generate`（`server/rpc/rpc-generate.go:60-155`） | `GenerateCpp`（`server/rpc/rpc-generate-cpp.go`） |
|---|---|---|
| 1 名称 | `codenames.GetCodename()` / `util.AllowedName` | 同（逐句复制） |
| 2 配置解析 | `db.ImplantConfigByID` 或 `models.IsC2Enabled` 推导 Include* | 同 |
| 3 导出名默认 | `Exports = ["StartW"]` | 同 |
| 4 密钥/证书/WG | `generate.GenerateConfig(name, config)` | **同（复用官方函数）** |
| 5 HTTP C2 profile | `db.LoadHTTPC2ConfigByName(...)` | 同 |
| 6 **编译** | `generate.SliverExecutable(...)` 等（go build / garble / zig） | **`cppbuild.CompileExecutable(...)` 等（CMake）** |
| 7 读取产物 | `os.ReadFile(fPath)` | 同 |
| 8 落库 | `generate.ImplantBuildSave(build, config, fPath)` | 同（顺序也一致：先落库再返回） |
| 9 事件 | `core.EventBroker.Publish(BuildCompletedEvent)` | 同 |
| 10 返回 | `clientpb.Generate{File, ImplantName, ImplantBuildID}` | 同 |

第 4 步是关键：`ImplantBuild` 里的 **PeerPublicKey/PeerPrivateKey/MinisignServerPublicKey/Mtls*/WG*** 全部由官方代码生成并落库，
所以 C implant 回连时 server 能照常按 `SHA256(公钥串)` 查 build、按 keyID 查签名密钥——**这是「协议零改动」的前提**。

---

## 2. `server/cppbuild` 包结构（已实现）

```
server/cppbuild/
├── cppbuild.go   # Compile()：目录布局、写 config.json、跑 cmake、收集产物
├── target.go     # (GOOS, GOARCH, Format, host) → CMake 生成器/工具链/产物名
├── config.go     # *clientpb.ImplantConfig + ImplantBuild → config.json
└── artifact.go   # PE/ELF 校验 + 静态性校验 + 体积门禁
```

### 2.1 目录布局（与官方一致）

```
~/.sliver/slivers/<GOOS>/<GOARCH>/<name>/
├── config.json        # 本次构建的全量注入项（含密钥，权限 0600 概念上）
├── build/             # CMake 构建树（每 implant 一个）
└── bin/<name><ext>    # 最终产物（ext 取 config.Extension 或平台默认）
```

`ImplantBuildSave(build, config, fPath)` 存的路径就是 `bin/<name><ext>`，与 `Regenerate`（`ImplantFileFromBuild`）兼容——
因此后续 `regenerate` 命令对 C implant 同样可用。

### 2.2 目标解析（`target.go`）

| 宿主 | GOOS/GOARCH | 生成器 | 工具链参数 | 产物 |
|---|---|---|---|---|
| Windows | windows/amd64 | `Visual Studio 17 2022` | `-A x64` | `implant.exe` / `.dll` |
| Windows | windows/386 | 同上 | `-A Win32` | 同上 |
| Windows | windows/arm64 | 同上 | `-A ARM64` | 同上 |
| Linux(WSL) | windows/amd64 | `Ninja` | `-DCMAKE_TOOLCHAIN_FILE=cmake/toolchains/windows-mingw.cmake` | 同上 |
| Linux(WSL) | windows/386 / arm64 | `Ninja` | `windows-mingw-386.cmake` / `-arm64.cmake` | 同上 |
| Linux(WSL) | linux/amd64 · arm64 | `Ninja` | 宿主本地编译器 | `implant` / `implant.so` |
| 任意 | darwin/* | — | 明确报错「暂不支持」 | — |

这一层是「按 arch 选工具链」的唯一开关点：新增目标只改这一个文件。

### 2.3 `config.json` 契约

服务端产出、CMake 消费、C 侧读取。字段名就是契约（等价于原来的 Go 模板占位符）：

```json
{
  "name": "…", "build_id": "<ImplantBuild.ID>", "strategy": "s", "beacon": false,
  "reconnect_interval_ns": 60000000000, "beacon_interval_ns": 60000000000,
  "beacon_jitter_ns": 0, "max_connection_errors": 1000, "poll_timeout": 0,
  "features": { "mtls": true, "http": false, "dns": false, "wg": false, "pivot": false },
  "c2": [ { "url": "mtls://1.2.3.4:8888", "options": "" } ],
  "limits":  { "domain_joined": false, "datetime": "", "hostname": "", "username": "", "file_exists": "", "locale": "" },
  "secrets": { "peer_public_key": "age1…", "peer_private_key": "AGE-SECRET-KEY-1…", "…": "…" },
  "http":    { "user_agent": "…", "headers": [], "path_segments": [], "…": "…" }
}
```

生成侧（`cmake/SliverConfigGen.cmake`）把它拆成两个文件：

| 文件 | 内容 | 谁能 include |
|---|---|---|
| `implant_build_config.h` | 开关、数值常量、limits、`SLIVER_IS_BEACON`、`SLIVER_C2_COUNT` | 全工程 |
| `implant_secrets.c` | `sliver_c2_urls[]`、`sliver_secret_values[]`（按枚举顺序）、name/build_id | 只被 `src/core/config.c` 链接 |

`sliver_secret_id_t`（`include/sliver/config.h`）与生成顺序绑定，`sliver_config_self_check()` 会断言两侧数量一致，
防止「生成器加字段、C 侧忘改」这类静默错位。

### 2.4 CMake 参数映射（构建期从 ImplantConfig 推导）

| ImplantConfig 字段 | CMake 变量 | 作用 |
|---|---|---|
| `GOOS/GOARCH` | `-G/-A/CMAKE_TOOLCHAIN_FILE` | 目标平台 |
| `Format` | `-DIMPLANT_FORMAT=exe\|shared` | 产物形态 |
| `Debug` | `-DIMPLANT_DEBUG_LOG=ON` + `-DCMAKE_BUILD_TYPE=Debug` | 日志与断言 |
| `GOOS==windows && !Debug` | `-DIMPLANT_GUI=ON` | 等价 `-H=windowsgui` |
| `IsBeacon` | `config.json.beacon` → `SLIVER_IS_BEACON` | 主循环形态 |
| `C2[].URL` | `config.json.c2[]` | 回连地址 |
| `ReconnectInterval / BeaconInterval / BeaconJitter / MaxConnectionErrors` | 同名 JSON 字段 | 节奏 |
| `Limit*` | `config.json.limits` | 执行限制 |
| `HTTPC2ConfigName` → `HTTPC2ImplantConfig` | `config.json.http` | HTTP 流量画像 |
| （固定） | `-DBUILD_TESTING=OFF`、`IMPLANT_LTO=ON`、`IMPLANT_STRIP=ON`、`IMPLANT_STATIC_CRT=ON` | 体积 |

### 2.5 产物校验（`artifact.go`，构建后立即执行）

1. **镜像格式**：`MZ`（PE）或 `\x7fELF`；DLL 必须含 `StartW` 导出串。
2. **静态性**：
   - ELF：遍历 program header，出现 `PT_INTERP` → 直接失败（"dynamically linked"）；
   - PE：出现 `VCRUNTIME140` / `MSVCP140` / `ucrtbase.dll` / `api-ms-win-crt` / `libgcc_s_` / `libwinpthread` → 直接失败。
3. **体积**：`> 5 MiB` 失败（`MaxImplantSizeBytes`）；`> 512 KiB` 记 warning（`TargetImplantSizeBytes`）；其余打印实际大小。

这三条把「静态 + 1–5 MB」从口头要求变成**构建失败条件**。

---

## 3. 错误处理与可观测性

| 场景 | 行为 |
|---|---|
| `implant-cpp` 源码目录找不到 | 返回 `cppbuild: implant-cpp source dir not found: … (set SLIVER_CPP_SRC)`，`GenerateCpp` 转成 gRPC 错误 |
| cmake 不存在 | `cppbuild: cmake not found in PATH` |
| 宿主/目标不匹配（Windows 宿主生成 linux 目标） | 明确提示「linux target requires a linux host (run the server inside WSL)」 |
| cmake 失败 | 服务端日志打印命令行 + **最后 4 KB 输出**（cmake 输出太长，截尾），返回 `cppbuild: cmake <arg0>: <err>` |
| 体积超限 / 动态链接 | 构建失败，错误里带实测字节数与判定依据 |

环境变量（便于本地调试与 CI）：
`SLIVER_CPP_SRC`（源码目录）、`SLIVER_CMAKE`（cmake 路径）、`SLIVER_SLIVERS_DIR`（覆盖产物根目录，默认 `~/.sliver/slivers`）。

---

## 4. 仍需补的两件事（P0 收尾）

1. **proto 注册**：`protobuf/rpcpb/services.proto` 加

   ```proto
   // 新增：C++ Implant 生成（请求/响应复用 GenerateReq / Generate）
   rpc GenerateCpp(clientpb.GenerateReq) returns (clientpb.Generate);
   ```

   然后 `make pb` 重新生成（本机当前无 protoc，需在具备 protobuf 工具链的环境执行）。
   在此之前 Go 侧代码可编译（方法已存在），但 gRPC 客户端还调不到——这是 P0 的第一件事。

2. **客户端命令**：`client/command/generate` 增加 `--cpp` 开关（走 `GenerateCpp` 而非 `Generate`），
   或在现有命令里按 flag 分流；CLI 文案与 `--format` 语义保持不变。

---

## 5. P0 验收（打通定义）

```bash
# 1. 起 server（WSL 或 Windows 均可，取决于目标平台）
sliver-server

# 2. 生成 C implant（P0 阶段只有 mtls + exe）
sliver > generate --mtls 127.0.0.1:8888 --os windows --arch amd64 --format exe --cpp

# 3. 在目标机执行，观察注册
sliver > sessions           # 必须出现该 session，Transport=mtls
sliver > use <id>; ping     # 必须往返成功

# 4. 产物硬指标
sliver > implants           # 记录文件大小
# 本地核对：
#   Windows: dumpbin /dependents implant.exe  → 只有系统 DLL（无 VCRUNTIME/ucrtbase）
#   Linux:   readelf -l implant | grep INTERP → 无输出（静态）
#   体积： <= 512 KB（目标），<= 5 MB（硬限）
```

P0 通过 = 上面四步全绿。任何一步红，都视为未打通，不进入 P1。
