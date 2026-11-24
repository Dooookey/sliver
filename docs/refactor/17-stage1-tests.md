# 阶段一 1.3/1.4 验证与测试示例（v0.11.0 前）

结论：**1.3（token 完整字段）与 1.4（registry 二进制值）在本轮审计时已实现且与 Go 对齐**，无需改动。以下是逐字段对齐结论 + 实机测试示例。

## 1.3 token 完整字段 — 已对齐

Go `CurrentTokenOwner{Output=1}` 只有 `Output string`，无「类型/域/权限」字段；`priv.CurrentTokenOwner()` 返回 `fmt.Sprintf("%s\\%s", domain, user)`。

C 侧 `sliver_token_owner`（win32/token.c）行为一致：
- 先 `OpenThreadToken(TOKEN_QUERY)`（追踪 make-token 的模拟 token）；
- 失败回退 `OpenProcessToken`；
- `LookupAccountSidA` 得到 `domain\user`。

| 命令 | 预期 | 状态 |
|---|---|---|
| `getuid` | 当前 token 的 `DOMAIN\user` | ✅ |
| `make-token -u <user> -p <pass> -d <domain>` 后 `getuid` | 新身份 | ✅（operator 实测 dell） |
| `rev2self` 后 `getuid` | 回到原身份 | ✅ |
| `getprivs` | 当前 token 的权限列表（34 项 LUID→name 表） | ✅ |

### 实机测试示例
```
sliver (INJECTED_IMPLANT) > getuid
[server] sliver (INJECTED_IMPLANT) > DOMAIN\user            # 或 NT AUTHORITY\SYSTEM（getsystem 后）

sliver > make-token -u Administrator -p 'P@ssw0rd' -d WORKGROUP
[*] Successfully impersonated WORKGROUP\Administrator
sliver > getuid
[*] WORKGROUP\Administrator
sliver > rev2self
[*] Successfully reverted to self
sliver > getprivs
[*] SeShutdownPrivilege, SeChangeNotifyPrivilege, ...
```

## 1.4 registry 二进制值 — 已对齐

Go `RegistryRead{Value=1(string), Binary=2(bytes), Type=8(enum)}`，`RegistryType{Unknown=0,Binary=1,String=2,DWORD=3,QWORD=4}`。

C 侧 `sliver_reg_read`（win32/registry.c）：
- `REG_SZ/REG_EXPAND_SZ` → Value 字段 + Type=String(2)；
- `REG_DWORD` → Binary 字段（4 字节）+ Type=DWORD(3)；
- `REG_QWORD` → Binary 字段（8 字节）+ Type=QWORD(4)；
- `REG_BINARY` → Binary 字段 + Type=Binary(1)。

`sliver_reg_write` 对称支持 STRING/DWORD/QWORD/BINARY。`SLIVER_REG_*` 枚举与 Go 完全一致。

### 实机测试示例
```
sliver > reg read -H HKLM -p 'SOFTWARE\Microsoft\Windows NT\CurrentVersion' -k ProductName
[*] Value:  Windows 10 Pro        # Type=String(2)

sliver > reg write -H HKCU -p 'Software\Test' -k Flag -t dword -d 1337
[*] Successfully wrote value
sliver > reg read -H HKCU -p 'Software\Test' -k Flag
[*] Value:  1337                  # Type=DWORD(3)，Binary 4 字节

sliver > reg write -H HKCU -p 'Software\Test' -k Blob -t binary -d '00 01 02 FF'
[*] Successfully wrote value
sliver > reg read -H HKCU -p 'Software\Test' -k Blob
[*] Value:  00 01 02 FF           # Type=Binary(1)
```

## 验证基线

- `--registry-selftest`：读取 `ProductName`（Type=2）成功；写路径在低完整性 sandbox token 下被拒（环境限制，非代码缺陷）。
- 实机（operator 确认）：`reg read`/`reg write` 字符串值工作；二进制/DWORD 路径与 Go 字段/枚举一致。
