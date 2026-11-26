# Sliver HTTP(S) C2 wire protocol（H3 参考）

本文把 `implant/sliver/transports/httpclient/httpclient.go` 与 `server/c2/http.go`
里与会话建立/收发相关的**已核实事实**固化下来，作为 C 端实现的依据。所有行号为本仓库实测。

---

## 1. 会话建立（匿名请求）

参考 `httpclient.go:372-431` `establishSessionID` 与 `server/c2/http.go` `anonymousHandler`。

1. 生成 32 字节随机 `sKey`，`SessionCtx = NewCipherContext(sKey)`。
2. `sessionInit = proto(HTTPSessionInit{ Key = sKey })`（单字段：`bytes Key = 1`）。
3. `sessionInit = AgeKeyExToServer(sessionInit)`
   —— 见 H1c：`SHA256(peer.public) || age_encrypt(server.public, HMAC||plaintext)`。
4. `nonce, encoder = RandomEncoder(0)`；`payload = encoder.Encode(sessionInit)`。
5. `uri = randomPath(profile) + nonce 查询参数`；`POST`（无 session 头）。
6. 期望 **200**；响应体 `encoder.Decode` → `SessionCtx.Decrypt` → **session id 字符串**。

C 端可强制 **NoEncoder**：只要 `nonce % 65537 == 0`（`EncoderFromNonce` 返回 id 0 →
`NoEncoder`），此时第 4、6 步的 `encoder.*` 都是恒等变换。

## 2. 信封收发（已认证）

- **发送**（`httpclient.go` 的 `writeEnvelope` 路径）：
  `body = encoder.Encode( SessionCtx.Encrypt( envelope ) )` → `POST`，
  期望 **202 Accepted**（无响应体，需 drain）。
- **接收 / 轮询**（`ReadEnvelope`，`httpclient.go:436-476`）：
  `GET`（无请求体）→
  - **200**：响应体 `encoder.Decode` → `SessionCtx.Decrypt` → `proto.Unmarshal` 成 Envelope；
  - **204**：无任务（正常空轮询）；
  - **403**：session 失效 → 必须重建会话。
- **关闭**：`CloseSession()`（`httpclient.go:567+`）先撤销在途 poll，再发关闭请求。

## 3. 服务端校验顺序（404/403 的来源）

`server/c2/http.go`：

- 路由是通配的：`router.HandleFunc("/{rpath:.*}", s.mainHandler)`（`:377`），
  所以**路径不匹配不会 404**；profile 的路径只用于伪装。
- `mainHandler` → `getHTTPSession` → 无会话则 `anonymousHandler`。
- 请求体处理（`:779-791`）：
  1. `encoder = EncoderFromNonce(nonce)`（`:438`，`encoderID = nonce % 65537`）
  2. `data = decodeReqBodyWithMaxLen(encoder, body)`（`:779`）
  3. `plaintext = CipherCtx.Decrypt(data)`（`:785`）
  任一失败 → `defaultHandler` → **404**。
- 因此：**裸信封一定 404**（我们之前的现象），路径正确与否无关。

## 4. C 端已具备的构件（H1/H2 成果）

| 构件 | 位置 | 状态 |
|---|---|---|
| gzip（compress + 完整 inflate） | `src/crypto/gzip.c` | ✅ Go 双向对拍 |
| CipherContext（`nonce12‖AEAD(gzip)`） | `src/crypto/cipher.c` | ✅ Go 双向对拍 |
| `AgeKeyExToServer` | `src/crypto/age_keyex.c` | ✅ Go 验签+解密+HMAC 通过 |
| 编码器（nop/hex/base64/base32/gzip） | `src/crypto/encoders.c` | ✅ 与 `util/encoders` 逐字节一致 |
| encoder id 注入 | `cppbuild/config.go` → cmake → `sliver_config_encoder_id` | ✅ |
| 签名帧 | `src/wire/frame.c` | ✅（mtls 已在用） |

## 5. session id 的携带方式（已锁定）

**结论：Cookie，且 cookie 名字每次响应随机。**

服务端 `server/c2/http.go`：

```go
// :866-881 会话查找就是遍历 Cookie
func (s *SliverHTTPC2) getHTTPSession(req *http.Request) *HTTPSession {
    for _, cookie := range req.Cookies() {
        httpSession := s.HTTPSessions.Get(cookie.Value)   // cookie 的“值”就是 session ID
        ...
    }
    return nil // 没有有效 cookie -> 视为匿名请求
}

// :891-895 session ID 形态：16 随机字节的 hex（32 字符）
func newHTTPSessionID() string { ... return hex.EncodeToString(buf) }

// :665-670 会话建立响应里由服务端下发 cookie
http.SetCookie(resp, &http.Cookie{
    Domain: s.ServerConf.Domain,
    Name:   s.getCookieName(implantConfig.HTTPC2ConfigName),
    Value:  httpSession.ID,
})

// getCookieName：从 profile 的 ServerConfig.Cookies 里随机挑一个名字！
cookies := httpC2Config.ServerConfig.Cookies
index := util.Intn(len(cookies))
return cookies[index].Name
```

因此 C 端必须实现一个**最小 cookie jar**：

1. 初始化请求不带 Cookie；
2. 解析响应里的 `Set-Cookie: <name>=<value>`，记住这一对（**名字不要硬编码**，每次响应都可能变）；
3. 之后每个请求都带 `Cookie: <name>=<value>`；
4. 收到 403 → 清空 jar 并重新建立会话。

Go 客户端之所以"看不见"这段逻辑，是因为它用的是 `http.Client{Jar: ...}`
（`gohttp.go` 的 `cookieJar()`），jar 自动做了存取。

### 其余两点

- **poll 超时**：`DoPoll` 用 `PollTimeout` 取消在途 GET（`:324-352`）；C 端把
  `SLIVER_POLL_TIMEOUT_NS` 接进 recv 的 socket 超时，超时视为 204（无任务）。
- **关闭**：`CloseSession` 先撤销在途 poll，然后发关闭请求（`:567+`）。

## 6. 计划中的实现（`src/transport/http_session.c`）

```c
typedef struct {
  char host[256]; uint16_t port; int use_tls;
  char base_path[512];
  /* cookie jar: 名字每次响应都可能变，所以按"最近一次 Set-Cookie"保存 */
  char cookie_name[128];
  char cookie_value[128];
  int have_cookie;
  unsigned char key[32];          /* CipherContext key */
  sliver_encoder_kind_t enc;      /* 固定 NoEncoder（nonce % 65537 == 0） */
  long long poll_timeout_ns;
  sliver_frame_signer_t signer;   /* envelope 签名 */
  const char *server_pubkey;
} http_session_t;

/* §1：age keyex -> POST -> 200 -> 存 Set-Cookie -> 记 session 上下文 */
int  sliver_http_session_establish(http_session_t *s, ...);
/* §2 发送：body = encode(cipher(envelope))，POST，期望 202 */
int  sliver_http_session_send(http_session_t *s, const void *env, size_t len);
/* §2 轮询：GET，200 解出信封 / 204 无任务 / 403 需重建 */
int  sliver_http_session_recv(http_session_t *s, sliver_buf_t *out);
void sliver_http_session_close(http_session_t *s);
```

接入方式与阶段 5 的 channel 设计一致：`sliver_channel_dial()` 在 `http(s)://` 分支
返回一个"会话化"的 channel，`send/recv` 内部做 encode+cipher+cookie；
`session.c` / `beacon.c` 仍然不需要改动。

## 7. 验收标准

1. 对**真实** `sliver-server` 的 HTTP(S) listener 能建立会话（200 + 可解出 session id）。
2. 注册后出现在 `sessions` / `beacons` 列表。
3. beacon 每 interval checkin 一次，`ps`/`ls` 能返回结果。
4. mtls 的 session/beacon 行为不变（ctest 7/7 + harness 全绿）。
