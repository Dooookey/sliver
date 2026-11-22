# 阶段 A 收口记录（v0.7.0-p1）

本轮一次性交付：getprivs 描述、grep 上下文、全量自测入口、能力矩阵文档。

## 1. getprivs：描述列改为内置映射

`LookupPrivilegeDisplayNameA/W` 在本机（Low 完整性受限令牌）直接崩溃
（0xC0000005 / 0x80000003），因此改为**内置特权描述表**（17 条常见特权），
`priv_description()` 未命中时回退为特权名：

```
privs selftest: 5 privileges, integrity=Low first=SeShutdownPrivilege (Shut down the system)
```

这是行为上的完整实现（客户端 Description 列不再与 Name 相同），且不依赖会崩的 API。

## 2. grep：上下文行

`grep_file_ctx()` 用 8 行环形缓冲保留 `LinesBefore`，命中后按 `LinesAfter` 继续收集：

- `LinesBefore` / `LinesAfter` 生效（上限 3，避免 payload 膨胀）；
- 匹配仍是**子串**语义（无正则引擎依赖），`Recursive` 仍只作用于单文件；
- 应答仍是 `Grep{Results=1 map<string, GrepResultsForFile>}`。

## 3. 全量自测入口

```
implant.exe --selftest-all      # 仅 debug 构建
```

依次运行：privs → screenshot → registry → services → token → netstat → mount →
ifconfig → grep → children → fs → spawn，输出一行汇总：

```
[info ] selftest-all: PASS (0 failure(s))
```

`registry` 的写入路径在受限令牌下无法执行（HKCU 写入被拒），自测把它降级为
**警告**而不是失败——读与枚举仍然验证通过。

## 4. 本机验证结果

| 自测 | 结果 |
|---|---|
| privs | 5 特权 + 描述 + Low 完整性 |
| screenshot | ~250 KB PNG（SOI 校验） |
| registry | ProductName 读取 + 子键枚举；写入 skipped（令牌） |
| services | 64 个服务，首个 AdobeARMservice；rev2self ok |
| token | 坏凭据干净失败；getsystem 跳过 |
| netstat | 39 条连接，状态正确 |
| mount | C/D/E/F 卷 + NTFS + 真实容量 |
| ifconfig | 8 个接口（含 WLAN / VMware / WSL） |
| grep | 命中数正确 |
| children | 0 子进程（独立运行） |
| fs | copy/move/chmod/chtimes ok |
| spawn | pid + stdout ok |

`ctest 3/3`。

## 5. 仍未验证（需要真机/管理员）

- `make-token` 成功路径（真实凭据）
- `getsystem` 成功路径（`SeImpersonatePrivilege` + SYSTEM 管道客户端）
- `registry` 写入成功路径、`read-hive`（未实现）
- `spawn-dll` / `execute-assembly` / `migrate` / `sideload`（未实现）

## 6. 编译期守门（建议，未启用）

`/W4 /WX` 未开启：当前代码仍有个别 `C4456`（变量遮蔽）告警。计划先清零再开启，
避免一次引入大量失败。
