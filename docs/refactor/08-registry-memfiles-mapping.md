# Registry / Memfiles 报文对照（对齐 Go implant）

基准：`implant/sliver/handlers/handlers_windows.go`（Windows）与
`implant/sliver/handlers/handlers_linux.go`（Linux），协议定义 `protobuf/sliverpb/sliver.proto`。

## 1. 关键结论：应答类型 = 请求类型

`protobuf/sliverpb/constants.go` 的 `msgTypeFromMessage` 只有请求侧的映射，例如：

```go
case *RegistryReadReq: return MsgRegistryReadReq   // 71
```

Go handler 的 `resp(data, err)` 用**同一个 envelope 类型**回包，客户端按请求等待同一个
envelope id 并直接 `proto.Unmarshal` 到对应应答结构。因此 C 侧实现时：

> **应答 envelope 的 Type 必须等于请求的 Type。**

## 2. Registry（仅 Windows，Go 侧 `handlers_windows.go` 注册）

| 命令 | 请求 | req id | 请求字段 | 应答 | 应答字段 |
|---|---|---|---|---|---|
| `registry read` | `RegistryReadReq` | 71 | Hive=1, Path=2, Key=3, Hostname=4 | `RegistryRead` | Value=1, Binary=2, Type=8, Response=9 |
| `registry write` | `RegistryWriteReq` | 72 | Hive=1, Path=2, Key=3, Hostname=4, StringValue=5, ByteValue=6, DWordValue=7, QWordValue=8, Type=10 | `RegistryWrite` | Response=9 |
| `registry create` | `RegistryCreateKeyReq` | 73 | Hive=1, Path=2, Key=3, Hostname=4 | `RegistryCreateKey` | Response=9 |
| `registry subkeys` | `RegistrySubKeyListReq` | 88 | Hive=1, Path=2, Hostname=4 | `RegistrySubKeyList` | Subkeys=1 (repeated string), Response=9 |
| `registry values` | `RegistryListValuesReq` | 89 | Hive=1, Path=2, Hostname=4 | `RegistryValuesList` | ValueNames=1 (repeated string), Response=9 |
| `registry rm` | `RegistryDeleteKeyReq` | 97 | Hive=1, Path=2, Key=3, Hostname=4 | `RegistryDeleteKey` | Response=9 |
| `registry read-hive` | `RegistryReadHiveReq` | 133 | RootHive=1, RequestedHive=2 | `RegistryReadHive` | Data=1, Encoder=2, Response=9 |

**RegistryType 枚举**（`sliver.proto`）：

```
Unknown = 0, Binary = 1, String = 2, DWORD = 3, QWORD = 4
```

**Hive 名称**（`implant/sliver/registry/registry_windows.go`）：

```
HKCR, HKCU, HKLM, HKPD, HKU, HKCC
```

**语义要点（与 Go 一致）**：

- `read`：`REG_SZ`/`REG_EXPAND_SZ` → `Value` + `Type=String`；`REG_DWORD`/`REG_QWORD`/其他 →
  `Binary`（原始字节）+ 对应 Type；`REG_MULTI_SZ` → 以 `\n` 连接后放进 `Value`，Type=String。
- `write`：按 `Type` 选 `StringValue`/`DWordValue`/`QWordValue`/`ByteValue`。
- `rm`：先按 `DeleteValue(Key)` 删除值，失败再 `DeleteKey(Hive\Path\Key)` 删子键。
- `create`：在 `Hive\Path` 下创建子键 `Key`。
- `read-hive`：导出 hive 二进制（Go 用 `registry.ReadHive`，需要加载配置单元，C 侧尚未实现）。

## 3. Memfiles（仅 Linux，Go 侧 `handlers_linux.go` 注册）

| 命令 | 请求 id | 应答 | 说明 |
|---|---|---|---|
| `memfiles add` | 116 | `MemfilesAdd`(117) `Fd=1, Response=9` | 创建 memfd |
| `memfiles rm` | 118 | `MemfilesRm`(119) `Fd=1, Response=9` | 关闭 memfd |
| `memfiles list` | 115 | **协议中不存在 `MemfilesList` 应答消息**，`constants.go` 也无对应应答常量 | 该 fork 的 list 链路不完整 |

**结论**：Windows 的 Go implant **没有**注册 memfiles handler（`handlers_windows.go`
无 `MsgMemfiles*`），因此 C/C++ Windows implant **不实现 memfiles** 才是一致行为；
Linux 侧若要实现，需要先确认 list 的应答消息（当前 proto 缺失）。

## 4. C 侧实现状态

| 项 | 状态 |
|---|---|
| registry read / write / create / delete / subkeys / values | ✅ `src/platform/win32/registry.c` + `dispatch.c`（id 71/72/73/97/88/89） |
| registry read-hive (133) | ⏳ 未实现（需要 RegLoadAppKey/SaveKey 语义） |
| memfiles | ➖ 不适用（Windows Go implant 未注册） |
| `--registry-selftest` | ✅ 仅 debug 构建；read/list 已验证 |
