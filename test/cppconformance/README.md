# cppconformance — C 植入体命令一致性验证套件

对 `sliver/implant-cpp` 编译出的真实 `implant.exe` 逐条跑完所有已实现 RPC，并
按 `protobuf/sliverpb` 的真实消息定义解码每个响应，对**语义**（不只是"有回包"）做断言，
输出逐命令 PASS/FAIL 矩阵。

套件位于主 Go 模块内（`test/cppconformance` 不属于任何 server/client 包），因此可以
直接 `import "github.com/bishopfox/sliver/protobuf/sliverpb"`，不会引入 CGO。

---

## 1. 构建

```powershell
cd D:\GOPATH\src\C2\sliver
$env:CGO_ENABLED='0'
go build -mod=vendor -o D:\GOPATH\src\C2\.audit\cppconf.exe ./test/cppconformance
```

不要加 `go_sqlite` tag，也不要 import `server/`（会拉 CGO）。

## 2. 运行

```powershell
# 全量（自动选端口、自动拉起并最终 kill 植入体）
D:\GOPATH\src\C2\.audit\cppconf.exe

# 指定植入体 / 端口 / 工作目录 / 超时
.\cppconf.exe -implant ..\sliver\implant-cpp\build\msvc-x64-debug\bin\implant.exe `
              -addr 127.0.0.1 -port 9446 `
              -tmp  D:\GOPATH\src\C2\.audit\confwork `
              -timeout 10s

# 只跑子集（逗号分隔，名字见下表第一列）
.\cppconf.exe -only pwd,ls,shell,portfwd

# 打开 wire 级日志（未匹配的 envelope、坏签名、短帧等）
.\cppconf.exe -v
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-implant` | `…\build\msvc-x64-debug\bin\implant.exe` | 被驱动的植入体 |
| `-addr` | `127.0.0.1` | 监听地址（同时写进植入体 `--mtls` 参数） |
| `-port` | `0`（自动选空闲端口） | 监听端口 |
| `-tmp` | `D:\GOPATH\src\C2\.audit\confwork` | 夹具/测试文件/植入体 stderr 的工作目录 |
| `-timeout` | `10s` | 单条命令超时（execute/download/screenshot/getsystem 等自动放宽） |
| `-only` | 空（全跑） | 逗号分隔命令子集 |
| `-log` | `D:\GOPATH\src\C2\.audit\conformance-run.log` | 原始输出落盘（stdout 同步复制） |
| `-age-priv` | 固定测试串 | 派生 envelope 签名密钥（与 `mtls-session-selftest.ps1` 一致） |
| `-name` | `cpp-conformance` | Register.Name |
| `-keep` | false | 结束后保留植入体进程（排障用） |
| `-v` | false | wire 级日志 |

退出码：**任何一条 FAIL → 1**，全 PASS → 0，无法建立会话/参数错误 → 2。

## 3. 服务端行为（为什么结果可信）

套件复刻真实 Sliver 服务端的会话层，而不是"自定义一个测试服务端"：

1. TLS 1.3 mTLS（P-256 CA、P-521 叶证书，`client.pem/client.key/ca.pem/server.pub` 由套件生成并写成文件），
   `MUX/1` preface，`hashicorp/yamux` 服务端。
2. 帧格式 `[74B minisign 签名][u32 LE 长度][protobuf Envelope]`；
   服务端→植入体的帧用 `seed=SHA256("env-signing-v1:"+agePriv)` 派生的 ed25519 私钥签名，
   植入体回帧用同一派生公钥验签（`SigOK` 逐帧记录）。
3. 请求：每条 RPC 在**新的 yamux 流**上写 `Envelope{ID: 非零, Type: 请求 id, Data: 请求}`。
4. 响应：植入体在自己的新流上回 `Envelope{ID: 同一 ID, Type: 由 C 侧 handler 表决定}`；
   套件**按 ID 匹配**（等同 `server/c2/mtls.go` → `DeliverResponse`、
   `server/core/connnection.go` 中 `ID != 0` 视为同步响应），Type 只记录不断言。
5. `ID == 0` 的帧按 Type 归入异步通道（`MsgTunnelData=22`、`MsgTunnelClose=23`、`MsgPing=3`、
   `MsgRegister=1`），与真实服务端 handler 表一致。
6. 所有等待都有 deadline；任何一步超时记为 FAIL 并继续下一条命令（单条挂死不拖垮整轮）。
7. 响应一律用 `google.golang.org/protobuf` + 真实 proto 类型解码；解码失败时用内置的
   宽松 protobuf 走查器（`lenient.go`）定位"哪个字段、哪几个字节"不是合法 UTF-8，
   并继续用宽松解析结果评估语义。

### 两个实测的协议细节（踩坑记录）

* **portfwd 没有 bind ack**：真实服务端只等客户端侧 `tunnel.ClientBound()`，植入体
  `sliver_portfwd_bind` 明确不回包。套件直接发 `PortfwdReq(80)`→`Portfwd(81)`，
  再用 `TunnelData{Sequence:0}` 往返验证数据面，绝不等 bind ack。
* **shell 的上行序号从 0 开始**（C 侧 `read_sequence` 初始 0），下行同理；套件按
  `TunnelData.TunnelID` 过滤 ID==0 的帧，并**按 Sequence 重排后再拼接**（真实服务端
  `server/core/tunnels.go` 也是用 `pendingFromImplant` 缓冲乱序帧）。实测下行帧会乱序到达
  （`dir C:\Windows\System32` 场景约 176/2428 帧，最大超前 16 帧，远小于服务端窗口
  `maxTunnelPendingFrames=128`），所以按到达顺序拼接会误报丢数据。

## 4. 覆盖的命令（45 条 + register）

| 命令 | 请求/响应 id | 关键断言 |
| --- | --- | --- |
| register | 植入体主动 → 1 | 签名可验、Name/Hostname/Os/Arch/Pid/ActiveC2 |
| ping | 3 | 不回包（heartbeat），随后 pwd 正常 |
| pwd | 12/13 | 绝对路径且目录真实存在 |
| ls | 5/6 | Exists=true；能看到刚创建的 `ls-probe.txt`（Size>0）与 `confdir`（IsDir） |
| cd | 11/13 | 回 `Pwd.Path` 等于目标，再 cd 回工作目录 |
| mkdir | 16/17 | 目录在磁盘上出现 |
| cp | 127/128 | BytesWritten==源大小，目标内容逐字节相同 |
| mv | 98/99 | 源消失、目标存在且内容一致 |
| chmod | 109/110 | Path 回显 + 无错误（Windows 为属性 no-op） |
| chtimes | 113/114 | mtime == 请求的 unix 秒（±2s） |
| download | 7/8 | Data 与源文件逐字节一致、**ReadFiles≥1**（为 0 时真实客户端直接 return） |
| upload | 9/10 | WrittenFiles=1、落盘内容一致、随后的 ls 能看到 |
| grep | 129 | 命中 2 行、行号 2/4、行内含 needle |
| ps | 18/19 | 非空且含植入体自身 pid |
| env | 66/67 | 含 PATH（严格解码失败时用宽松解析并定位非法字段） |
| execute | 44 | Stdout 含标记串、Status=0、Pid>0 |
| execute-async | 44 | 慢命令在跑时 pwd 仍 <5s 应答；随后 Execute 按同一 ID 到达 |
| shell | 20/21 | TunnelID 回显、Pid>0、`echo` 回显经 TunnelData 到达 |
| shell-default | 20/21 | 默认 shell（Path=""）+ `dir System32` + `echo after-big`，
  复现 mtls-testserver 卡死场景，并报告帧数/字节/序号连续性、乱序帧数与最大超前帧数 |
| shell-bulk | 20/21 | 默认 shell 打印 1200 行编号，逐行校验**一行不缺**（把"帧序号跳跃"和"真实丢字节"区分开） |
| shell-resize | 135 | 同 ID 上有应答（真实服务端 unary 等待），会话存活 |
| portfwd | 80/81 | Host/Port/TunnelID 回显 + `TunnelData` 与本地 echo server 双向回显 |
| execute-children | 136 | 非空（shell/后台 execute 子进程存活）且 pid>0 |
| tunnel-close | 23 | 不回包、shell 子进程被回收、随后 pwd 正常 |
| terminate | 45/46 | Pid 回显且目标进程确实退出 |
| ifconfig | 42/43 | 至少一个接口，含 IP |
| netstat | 49 | 至少一条连接且带本地地址 |
| mount | 134 | 至少一个卷且 MountPoint 非空 |
| screenshot | 47/48 | Data>0 且 PNG 魔数 `89 50 4E 47 0D 0A 1A 0A` |
| getprivs | 87 | PrivInfo 非空、ProcessIntegrity 非空 |
| current-token-owner | 100/101 | Output 非空且形如 `DOMAIN\user` |
| rev2self | 31/30 | 无错误、会话存活 |
| make-token | 64/65 | 错误凭据必须明确失败（LogonUser 1326）且会话存活 |
| getsystem | 32/33 | 明确结果（失败路径也算）+ 会话不崩 |
| services | 130 | 非空且每项有 Name |
| service-detail | 131 | Detail.Name==Spooler |
| start-service | 132 | 不存在的服务名必须返回明确错误 |
| reg-create / write / read / values / subkeys / delete | 73/72/71/89/88/97 | 建键→写→读往返→列值→列子键→删值+删键清理，键路径 `HKCU\Software\AppDataLow\SliverCppConf` |
| reg-read-hive | 133 | 导出成功或明确失败（需 SeBackupPrivilege） |
| spawn-dll | 40 | `g_handlers` 未注册 → 必须回 `UnknownMessageType` |
| unknown-type | 200 | 必须回 `Envelope{同 ID, UnknownMessageType=true}` |
| rm | 14/15 | 目标路径消失 |

> 注册表测试键放在 `HKCU\Software\AppDataLow`：审计沙箱里套件与植入体都跑在
> **Low 完整性级别**（运行日志头部会打印 `host token: Low(S-1-16-4096)` 并用同一 token
> 自测一次写权限），`HKCU\Software` 直写会被 ACCESS_DENIED 拒绝，那是环境限制而不是植入体缺陷。
> `AppDataLow` 是 Low IL 可写区，因此注册表 RPC 可以端到端验证。

## 5. 输出

1. 运行头部：植入体路径、监听地址、工作目录、超时、**宿主 token 完整性 + 套件自身写注册表自测**、
   TLS 版本/套件、register 字段。
2. 逐命令表：`命令 | 发送(ids) | 收包(id/type) | 解码 | 语义断言 | 结果 | 备注`。
3. 汇总：`register + N 条命令, 通过 P/M`。
4. 失败块：每条失败的原因、首个非法字段/字节窗口、`resp-raw` hexdump、以及（隧道类）
   植入体 stderr 的相关行。
5. 最后 60 行 `implant.log` 尾巴（崩溃/断言最直接的证据）。
6. 全量原始输出同时写入 `-log`（默认 `D:\GOPATH\src\C2\.audit\conformance-run.log`）。

工作目录 `-tmp` 里会留下：`fixtures/`（证书与 server.pub）、`implant.log`（植入体 stderr）、
以及各命令的测试文件（`cp-src.txt`、`grep.txt`、`upload-out.txt`…）。

## 6. 复跑与清理

```powershell
# 复跑一次完整审计
cd D:\GOPATH\src\C2\sliver
$env:CGO_ENABLED='0'
go build -mod=vendor -o D:\GOPATH\src\C2\.audit\cppconf.exe ./test/cppconformance
D:\GOPATH\src\C2\.audit\cppconf.exe | Tee-Object D:\GOPATH\src\C2\.audit\conformance-run.log
```

* 套件结束时用 `taskkill /F /T /PID <implant>` 回收植入体及其子 shell；如需保留现场用 `-keep`。
* 若提示端口占用，用 `-port 0` 或换端口。
* 注册表测试键每次运行都会重建/清理；`read-hive` 在无 SeBackupPrivilege 时按明确失败计 PASS。

## 7. 局限（阅读结果时必须知道）

* Type 只记录不断言：真实服务端按 ID 匹配同步响应，C 侧若干 handler 的 `resp_type`
  与 Go 常量名不完全对应（例如 `shell-resize` 回 type=0、`netstat/mount/grep/services`
  回请求 id）。这些不影响 mTLS 同步 RPC，且已在每条命令的"收包(id/type)"列里列出。
* `spawn-dll`(40) 与真实注入语义无关：C 侧只有 self-load 实现且未注册进 `g_handlers`，
  套件断言的是"未知类型必须明确回 UnknownMessageType"。
* `getsystem`/`reg-read-hive` 依赖提权/备份特权，套件只断言"有明确结果且不崩"。
* 文件/注册表用例都在 `-tmp` 与 `HKCU\Software\AppDataLow` 内，运行结束不留测试数据。
