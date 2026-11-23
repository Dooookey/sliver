package main

// commands.go - the command matrix. Every entry sends a real proto request,
// decodes the reply with the real proto type and asserts the semantics of the
// decoded fields (plus, where it matters, the effect on the filesystem).

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/bishopfox/sliver/protobuf/commonpb"
	"github.com/bishopfox/sliver/protobuf/sliverpb"
)

type commandSpec struct {
	name string
	run  func(*runCtx) *result
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func errOf(resp *commonpb.Response) string {
	if resp == nil {
		return ""
	}
	return resp.Err
}

// rpc sends a request and decodes the reply into resp.
func (r *runCtx) rpc(res *result, typ uint32, req proto.Message, resp proto.Message) (*frame, error) {
	return r.rpcTimeout(res, typ, req, resp, r.timeout)
}

func (r *runCtx) rpcTimeout(res *result, typ uint32, req proto.Message, resp proto.Message,
	timeout time.Duration) (*frame, error) {
	payload, err := marshalOrNil(req)
	if err != nil {
		return nil, err
	}
	return r.rpcPayload(res, typ, payload, resp, timeout)
}

func (r *runCtx) rpcPayload(res *result, typ uint32, payload []byte, resp proto.Message,
	timeout time.Duration) (*frame, error) {
	f, id, err := r.p.callBytes(typ, payload, timeout)
	res.sent = fmt.Sprintf("%d=%s id=%d", typ, msgName(typ), id)
	if err != nil {
		res.recv = "无回复"
		return nil, err
	}
	res.recv = fmt.Sprintf("id=%d type=%d(%s) %dB", f.ID, f.Type, msgName(f.Type), len(f.Data))
	res.raw = f.Data
	if f.Unknown {
		res.recv += " unknown-type"
	}
	if !f.SigOK {
		res.recv += " sig-invalid"
	}
	if resp == nil {
		res.decode = "—"
		return f, nil
	}
	if len(f.Data) == 0 {
		res.decode = "空(0B)"
		// An all-defaults message legitimately marshals to zero bytes; a
		// non-zero-length reply that fails to decode is a real failure.
		return f, nil
	}
	if derr := proto.Unmarshal(f.Data, resp); derr != nil {
		res.decode = "严格解码失败"
		if strings.Contains(derr.Error(), "invalid UTF-8") {
			res.decode = "严格解码失败(非 UTF-8 字符串)"
			return f, fmt.Errorf("protobuf 解码失败: %v; %s", derr, findInvalidUTF8(f.Data, "", 0))
		}
		return f, fmt.Errorf("protobuf 解码失败: %v; hex=%s", derr, hexdump(f.Data, 128))
	}
	res.decode = "ok"
	return f, nil
}

func marshalOrNil(m proto.Message) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return proto.Marshal(m)
}

func (r *runCtx) path(parts ...string) string {
	return filepath.Join(append([]string{r.dir}, parts...)...)
}

func cmdExe() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "cmd.exe")
}

// ---------------------------------------------------------------------------
// echo server used to prove port forwarding relays bytes both ways
// ---------------------------------------------------------------------------

type echoServer struct {
	ln   net.Listener
	addr string
	port int
}

func startEchoServer() (*echoServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	e := &echoServer{ln: ln, addr: ln.Addr().String()}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		e.port = tcp.Port
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
					n, err := c.Read(buf)
					if n > 0 {
						// One Write: "ECHO:" + payload lands in a single TCP
						// segment so the relay emits one TunnelData frame.
						reply := append([]byte("ECHO:"), buf[:n]...)
						_, _ = c.Write(reply)
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return e, nil
}

func (e *echoServer) Close() {
	if e != nil && e.ln != nil {
		_ = e.ln.Close()
	}
}

// ---------------------------------------------------------------------------
// the matrix
// ---------------------------------------------------------------------------

func commandSpecs() []commandSpec {
	return []commandSpec{
		{"ping", cmdPing},
		{"pwd", cmdPwd},
		{"ls", cmdLs},
		{"cd", cmdCd},
		{"mkdir", cmdMkdir},
		{"cp", cmdCp},
		{"mv", cmdMv},
		{"chmod", cmdChmod},
		{"chtimes", cmdChtimes},
		{"download", cmdDownload},
		{"upload", cmdUpload},
		{"grep", cmdGrep},
		{"ps", cmdPs},
		{"env", cmdEnv},
		{"execute", cmdExecute},
		{"execute-async", cmdExecuteAsync},
		{"shell", cmdShell},
		{"shell-default", cmdShellDefault},
		{"shell-bulk", cmdShellBulk},
		{"shell-resize", cmdShellResize},
		{"portfwd", cmdPortfwd},
		{"execute-children", cmdExecuteChildren},
		{"tunnel-close", cmdTunnelClose},
		{"terminate", cmdTerminate},
		{"ifconfig", cmdIfconfig},
		{"netstat", cmdNetstat},
		{"mount", cmdMount},
		{"screenshot", cmdScreenshot},
		{"getprivs", cmdGetPrivs},
		{"current-token-owner", cmdCurrentTokenOwner},
		{"rev2self", cmdRevToSelf},
		{"make-token", cmdMakeToken},
		{"getsystem", cmdGetSystem},
		{"services", cmdServices},
		{"service-detail", cmdServiceDetail},
		{"start-service", cmdStartService},
		{"reg-create", cmdRegCreate},
		{"reg-write", cmdRegWrite},
		{"reg-read", cmdRegRead},
		{"reg-values", cmdRegValues},
		{"reg-subkeys", cmdRegSubkeys},
		{"reg-delete", cmdRegDelete},
		{"reg-read-hive", cmdRegReadHive},
		{"spawn-dll", cmdSpawnDll},
		{"unknown-type", cmdUnknownType},
		{"rm", cmdRm},
	}
}

// --- transport level --------------------------------------------------------

// cmdPing: the C session treats MsgPing as a no-reply heartbeat, so the
// assertion is "no reply, and the session still serves the next RPC".
func cmdPing(r *runCtx) *result {
	res := &result{name: "ping"}
	id, err := r.p.beginMsg(sliverpb.MsgPing, &sliverpb.Ping{Nonce: 4242, Request: r.request()})
	res.sent = fmt.Sprintf("%d=%s id=%d", sliverpb.MsgPing, msgName(sliverpb.MsgPing), id)
	if err != nil {
		return res.fail("发送失败", "%v", err)
	}
	if f, err := r.p.awaitReply(id, 1500*time.Millisecond); err == nil {
		res.recv = fmt.Sprintf("id=%d type=%d", f.ID, f.Type)
		return res.fail("Ping 不应产生响应", "收到 id=%d type=%d data=%s", f.ID, f.Type, hexdump(f.Data, 48))
	}
	r.p.unregister(id)
	res.recv = "无回复(预期)"
	res.decode = "—"

	// session must still be alive
	out := &sliverpb.Pwd{}
	f, err2 := r.rpcTimeout(res, sliverpb.MsgPwdReq, &sliverpb.PwdReq{Request: r.request()}, out,
		5*time.Second)
	_ = f
	if err2 != nil {
		return res.fail("Ping 之后 pwd 无响应", "%v", err2)
	}
	if out.Path == "" {
		return res.fail("", "Ping 之后 pwd 返回空路径")
	}
	res.assert = "Ping 不回包；随后 pwd 正常"
	return res.pass("heartbeat 语义")
}

// --- file system -----------------------------------------------------------

func cmdPwd(r *runCtx) *result {
	res := &result{name: "pwd"}
	out := &sliverpb.Pwd{}
	if _, err := r.rpc(res, sliverpb.MsgPwdReq, &sliverpb.PwdReq{Request: r.request()}, out); err != nil {
		return res.fail("pwd 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q", out.Path)
	res.assert = "Path 非空、绝对路径、目录存在"
	if out.Path == "" {
		return res.fail("Path 为空", "")
	}
	if !filepath.IsAbs(out.Path) {
		return res.fail("Path 不是绝对路径", "Path=%q", out.Path)
	}
	fi, err := os.Stat(out.Path)
	if err != nil {
		return res.fail("Path 指向的目录不存在", "Path=%q stat: %v", out.Path, err)
	}
	if !fi.IsDir() {
		return res.fail("Path 不是目录", "Path=%q", out.Path)
	}
	return res.pass(fmt.Sprintf("cwd=%s", out.Path))
}

func cmdLs(r *runCtx) *result {
	res := &result{name: "ls"}
	out := &sliverpb.Ls{}
	if _, err := r.rpc(res, sliverpb.MsgLsReq, &sliverpb.LsReq{Path: r.dir, Request: r.request()}, out); err != nil {
		return res.fail("ls 无响应", "%v", err)
	}
	names := make([]string, 0, len(out.Files))
	var probe *sliverpb.FileInfo
	var dirEntry *sliverpb.FileInfo
	for _, f := range out.Files {
		names = append(names, f.Name)
		switch f.Name {
		case "ls-probe.txt":
			probe = f
		case "confdir":
			dirEntry = f
		}
	}
	sort.Strings(names)
	if len(names) > 6 {
		names = names[:6]
	}
	res.decode = fmt.Sprintf("Path=%q Exists=%v files=%d [%s]", out.Path, out.Exists,
		len(out.Files), strings.Join(names, ","))
	res.assert = "Exists=true；含刚创建的 ls-probe.txt(Size>0) 与 confdir(IsDir)"
	if !out.Exists {
		return res.fail("Exists=false", "Path=%q err=%q", out.Path, errOf(out.Response))
	}
	if probe == nil {
		return res.fail("看不到 ls-probe.txt", "目录内容: %v", names)
	}
	if probe.IsDir {
		return res.fail("", "ls-probe.txt 被报告为目录")
	}
	if probe.Size <= 0 {
		return res.fail("", "ls-probe.txt 的 Size=%d（应为 %d）", probe.Size, len("ls-probe\n"))
	}
	if dirEntry == nil || !dirEntry.IsDir {
		return res.fail("看不到目录项 confdir 或 IsDir=false", "confdir=%v", dirEntry)
	}
	return res.pass(fmt.Sprintf("%d 项, ls-probe.txt=%dB", len(out.Files), probe.Size))
}

func cmdCd(r *runCtx) *result {
	res := &result{name: "cd"}
	target := r.path("confdir")
	out := &sliverpb.Pwd{}
	if _, err := r.rpc(res, sliverpb.MsgCdReq, &sliverpb.CdReq{Path: target, Request: r.request()}, out); err != nil {
		return res.fail("cd 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q err=%q", out.Path, errOf(out.Response))
	res.assert = "cd 后 Path 等于目标目录，再 cd 回工作目录"
	if e := errOf(out.Response); e != "" {
		return res.fail("cd 报错", "err=%q target=%q", e, target)
	}
	if !samePath(out.Path, target) {
		return res.fail("", "cd 返回 Path=%q，期望 %q", out.Path, target)
	}
	// back to the work dir, verify with pwd
	back := &sliverpb.Pwd{}
	if _, err := r.rpc(res, sliverpb.MsgCdReq, &sliverpb.CdReq{Path: r.dir, Request: r.request()}, back); err != nil {
		return res.fail("cd 回工作目录无响应", "%v", err)
	}
	if !samePath(back.Path, r.dir) {
		return res.fail("", "cd 返回 Path=%q，期望 %q", back.Path, r.dir)
	}
	return res.pass("往返 ok")
}

func samePath(a, b string) bool {
	na := strings.ToLower(strings.ReplaceAll(filepath.Clean(a), "/", `\`))
	nb := strings.ToLower(strings.ReplaceAll(filepath.Clean(b), "/", `\`))
	return strings.TrimRight(na, `\`) == strings.TrimRight(nb, `\`)
}

func cmdMkdir(r *runCtx) *result {
	res := &result{name: "mkdir"}
	target := r.path("mkdir-probe")
	_ = os.RemoveAll(target)
	out := &sliverpb.Mkdir{}
	if _, err := r.rpc(res, sliverpb.MsgMkdirReq, &sliverpb.MkdirReq{Path: target, Request: r.request()}, out); err != nil {
		return res.fail("mkdir 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q err=%q", out.Path, errOf(out.Response))
	res.assert = "mkdir 后目录在磁盘上存在"
	if e := errOf(out.Response); e != "" {
		return res.fail("mkdir 报错", "err=%q", e)
	}
	fi, err := os.Stat(target)
	if err != nil {
		return res.fail("目录未创建", "stat %s: %v", target, err)
	}
	if !fi.IsDir() {
		return res.fail("", "%s 不是目录", target)
	}
	return res.pass("目录已创建")
}

func cmdRm(r *runCtx) *result {
	res := &result{name: "rm"}
	target := r.path("mkdir-probe")
	if _, err := os.Stat(target); err != nil {
		return res.fail("前置条件缺失：mkdir-probe 不存在", "%v", err)
	}
	out := &sliverpb.Rm{}
	if _, err := r.rpc(res, sliverpb.MsgRmReq,
		&sliverpb.RmReq{Path: target, Recursive: true, Force: true, Request: r.request()}, out); err != nil {
		return res.fail("rm 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q err=%q", out.Path, errOf(out.Response))
	res.assert = "rm 后路径在磁盘上不存在"
	if e := errOf(out.Response); e != "" {
		return res.fail("rm 报错", "err=%q", e)
	}
	if _, err := os.Stat(target); err == nil {
		return res.fail("目录仍存在", "stat %s 仍然成功", target)
	}
	return res.pass("已删除")
}

func cmdCp(r *runCtx) *result {
	res := &result{name: "cp"}
	src := r.path("cp-src.txt")
	dst := r.path("cp-dst.txt")
	_ = os.Remove(dst)
	want, err := os.ReadFile(src)
	if err != nil {
		return res.fail("前置文件缺失", "%v", err)
	}
	out := &sliverpb.Cp{}
	if _, err := r.rpc(res, sliverpb.MsgCpReq,
		&sliverpb.CpReq{Src: src, Dst: dst, Request: r.request()}, out); err != nil {
		return res.fail("cp 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Src=%q Dst=%q BytesWritten=%d err=%q", out.Src, out.Dst,
		out.BytesWritten, errOf(out.Response))
	res.assert = "BytesWritten==源大小 且目标内容逐字节相同"
	if e := errOf(out.Response); e != "" {
		return res.fail("cp 报错", "err=%q", e)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		return res.fail("目标文件不存在", "%v", rerr)
	}
	if string(got) != string(want) {
		return res.fail("内容不一致", "目标 %q != 源 %q", string(got), string(want))
	}
	if out.BytesWritten != int64(len(want)) {
		return res.fail("", "BytesWritten=%d，期望 %d", out.BytesWritten, len(want))
	}
	return res.pass(fmt.Sprintf("%dB", len(got)))
}

func cmdMv(r *runCtx) *result {
	res := &result{name: "mv"}
	src := r.path("cp-dst.txt")
	dst := r.path("mv-dst.txt")
	_ = os.Remove(dst)
	want, err := os.ReadFile(src)
	if err != nil {
		return res.fail("前置文件缺失（cp 未成功）", "%v", err)
	}
	out := &sliverpb.Mv{}
	if _, err := r.rpc(res, sliverpb.MsgMvReq,
		&sliverpb.MvReq{Src: src, Dst: dst, Request: r.request()}, out); err != nil {
		return res.fail("mv 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Src=%q Dst=%q err=%q", out.Src, out.Dst, errOf(out.Response))
	res.assert = "源消失、目标存在且内容一致"
	if e := errOf(out.Response); e != "" {
		return res.fail("mv 报错", "err=%q", e)
	}
	if _, err := os.Stat(src); err == nil {
		return res.fail("源文件仍存在", "%s", src)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		return res.fail("目标文件不存在", "%v", rerr)
	}
	if string(got) != string(want) {
		return res.fail("内容不一致", "%q != %q", string(got), string(want))
	}
	return res.pass("已移动")
}

func cmdChmod(r *runCtx) *result {
	res := &result{name: "chmod"}
	target := r.path("chmod.txt")
	out := &sliverpb.Chmod{}
	if _, err := r.rpc(res, sliverpb.MsgChmodReq,
		&sliverpb.ChmodReq{Path: target, FileMode: "0755", Request: r.request()}, out); err != nil {
		return res.fail("chmod 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q err=%q", out.Path, errOf(out.Response))
	res.assert = "Path 回显目标、无错误、文件仍可访问"
	if e := errOf(out.Response); e != "" {
		return res.fail("chmod 报错", "err=%q", e)
	}
	if !samePath(out.Path, target) {
		return res.fail("", "Path=%q，期望 %q", out.Path, target)
	}
	if _, err := os.Stat(target); err != nil {
		return res.fail("文件不可访问", "%v", err)
	}
	return res.pass("Windows 下为属性 no-op，成功即通过")
}

func cmdChtimes(r *runCtx) *result {
	res := &result{name: "chtimes"}
	target := r.path("chtimes.txt")
	want := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	atime := time.Now().Add(-24 * time.Hour).Unix()
	mtime := want.Unix()
	out := &sliverpb.Chtimes{}
	if _, err := r.rpc(res, sliverpb.MsgChtimesReq,
		&sliverpb.ChtimesReq{Path: target, ATime: atime, MTime: mtime, Request: r.request()}, out); err != nil {
		return res.fail("chtimes 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q err=%q", out.Path, errOf(out.Response))
	res.assert = "mtime 变为请求的 unix 秒值（±2s）"
	if e := errOf(out.Response); e != "" {
		return res.fail("chtimes 报错", "err=%q", e)
	}
	fi, err := os.Stat(target)
	if err != nil {
		return res.fail("文件不可访问", "%v", err)
	}
	got := fi.ModTime().Unix()
	if diff := got - mtime; diff > 2 || diff < -2 {
		return res.fail("mtime 未按请求设置", "期望 %d，实际 %d (diff %ds)", mtime, got, diff)
	}
	return res.pass(fmt.Sprintf("mtime=%d", got))
}

func cmdDownload(r *runCtx) *result {
	res := &result{name: "download"}
	src := r.path("download.bin")
	want, err := os.ReadFile(src)
	if err != nil {
		return res.fail("前置文件缺失", "%v", err)
	}
	out := &sliverpb.Download{}
	if _, err := r.rpc(res, sliverpb.MsgDownloadReq,
		&sliverpb.DownloadReq{Path: src, Request: r.request()}, out); err != nil {
		return res.fail("download 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q Exists=%v Start=%d Stop=%d Data=%dB ReadFiles=%d err=%q", out.Path,
		out.Exists, out.Start, out.Stop, len(out.Data), out.ReadFiles, errOf(out.Response))
	res.assert = "Exists=true、Data 与源文件逐字节一致、ReadFiles>=1（客户端 ReadFiles==0 会直接放弃）"
	if e := errOf(out.Response); e != "" {
		return res.fail("download 报错", "err=%q", e)
	}
	if !out.Exists {
		return res.fail("Exists=false", "")
	}
	if string(out.Data) != string(want) {
		return res.fail("字节不一致", "收到 %dB (%s)，期望 %dB (%s)", len(out.Data),
			hexdump(out.Data, 32), len(want), hexdump(want, 32))
	}
	if out.ReadFiles < 1 {
		return res.fail("ReadFiles=0 → 真实客户端 client/command/filesystem/download.go 会打印 "+
			"\"No files downloaded\" 并直接 return，操作者拿不到文件",
			"ReadFiles=%d Exists=%v Data=%dB", out.ReadFiles, out.Exists, len(out.Data))
	}
	return res.pass(fmt.Sprintf("%dB 原样返回, ReadFiles=%d", len(out.Data), out.ReadFiles))
}

func cmdUpload(r *runCtx) *result {
	res := &result{name: "upload"}
	target := r.path("upload-out.txt")
	_ = os.Remove(target)
	payload := []byte("upload-conformance-payload\nsecond line\n")
	out := &sliverpb.Upload{}
	if _, err := r.rpc(res, sliverpb.MsgUploadReq,
		&sliverpb.UploadReq{Path: target, Data: payload, Overwrite: true, Request: r.request()}, out); err != nil {
		return res.fail("upload 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q WrittenFiles=%d Unwriteable=%d err=%q", out.Path,
		out.WrittenFiles, out.UnwriteableFiles, errOf(out.Response))
	res.assert = "WrittenFiles=1 且磁盘内容一致；随后的 ls 能看到该文件"
	if e := errOf(out.Response); e != "" {
		return res.fail("upload 报错", "err=%q", e)
	}
	if out.WrittenFiles != 1 || out.UnwriteableFiles != 0 {
		return res.fail("", "WrittenFiles=%d UnwriteableFiles=%d", out.WrittenFiles, out.UnwriteableFiles)
	}
	got, rerr := os.ReadFile(target)
	if rerr != nil {
		return res.fail("文件未落盘", "%v", rerr)
	}
	if string(got) != string(payload) {
		return res.fail("内容不一致", "磁盘 %q != 上传 %q", string(got), string(payload))
	}
	// cross check through ls
	lsOut := &sliverpb.Ls{}
	if _, err := r.rpc(res, sliverpb.MsgLsReq, &sliverpb.LsReq{Path: r.dir, Request: r.request()}, lsOut); err != nil {
		return res.fail("上传后的 ls 无响应", "%v", err)
	}
	found := false
	for _, f := range lsOut.Files {
		if f.Name == "upload-out.txt" {
			found = true
		}
	}
	if !found {
		return res.fail("ls 看不到上传的文件", "")
	}
	return res.pass(fmt.Sprintf("%dB 落盘且 ls 可见", len(got)))
}

func cmdGrep(r *runCtx) *result {
	res := &result{name: "grep"}
	target := r.path("grep.txt")
	out := &sliverpb.Grep{}
	if _, err := r.rpc(res, sliverpb.MsgGrepReq,
		&sliverpb.GrepReq{SearchPattern: "needle", Path: target, LinesBefore: 1, LinesAfter: 1,
			Request: r.request()}, out); err != nil {
		return res.fail("grep 无响应", "%v", err)
	}
	var hits int
	var lines []string
	for key, fr := range out.Results {
		for _, hr := range fr.FileResults {
			hits++
			lines = append(lines, fmt.Sprintf("%s:%d:%q", filepath.Base(key), hr.LineNumber, hr.Line))
		}
	}
	sort.Strings(lines)
	res.decode = fmt.Sprintf("Results=%d hits=%d abs=%q err=%q", len(out.Results), hits,
		out.SearchPathAbsolute, errOf(out.Response))
	res.assert = "命中 2 行（needle one / needle two），行号 2 与 4"
	if e := errOf(out.Response); e != "" {
		return res.fail("grep 报错", "err=%q", e)
	}
	if hits != 2 {
		return res.fail(fmt.Sprintf("命中 %d 行", hits), "hits=%v results=%v", lines, out.Results)
	}
	var numbers []int64
	for _, fr := range out.Results {
		for _, hr := range fr.FileResults {
			numbers = append(numbers, hr.LineNumber)
			if !strings.Contains(hr.Line, "needle") {
				return res.fail("命中行不含 needle", "line=%q", hr.Line)
			}
		}
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	if len(numbers) == 2 && (numbers[0] != 2 || numbers[1] != 4) {
		return res.fail("行号不符", "got %v want [2 4]", numbers)
	}
	return res.pass(strings.Join(lines, " "))
}

// --- process / env ---------------------------------------------------------

func cmdPs(r *runCtx) *result {
	res := &result{name: "ps"}
	out := &sliverpb.Ps{}
	if _, err := r.rpc(res, sliverpb.MsgPsReq, &sliverpb.PsReq{FullInfo: true, Request: r.request()}, out); err != nil {
		return res.fail("ps 无响应", "%v", err)
	}
	self := false
	var sample string
	for _, p := range out.Processes {
		if int(p.Pid) == r.implantPID {
			self = true
			sample = fmt.Sprintf("self pid=%d exe=%q", p.Pid, p.Executable)
		}
	}
	if sample == "" && len(out.Processes) > 0 {
		sample = fmt.Sprintf("first pid=%d exe=%q", out.Processes[0].Pid, out.Processes[0].Executable)
	}
	res.decode = fmt.Sprintf("Processes=%d %s err=%q", len(out.Processes), sample, errOf(out.Response))
	res.assert = "非空且包含植入体自身 pid"
	if e := errOf(out.Response); e != "" {
		return res.fail("ps 报错", "err=%q", e)
	}
	if len(out.Processes) == 0 {
		return res.fail("进程列表为空", "")
	}
	if !self {
		return res.fail(fmt.Sprintf("没有 pid %d", r.implantPID), "列表 %d 项，未找到植入体自身", len(out.Processes))
	}
	return res.pass(fmt.Sprintf("%d 个进程", len(out.Processes)))
}

func cmdEnv(r *runCtx) *result {
	res := &result{name: "env"}
	out := &sliverpb.EnvInfo{}
	f, err := r.rpc(res, sliverpb.MsgEnvReq, &sliverpb.EnvReq{Request: r.request()}, out)
	res.assert = "含 PATH 且值非空"
	if err != nil {
		if f == nil {
			return res.fail("env 无响应", "%v", err)
		}
		// Strict decode rejected the payload (invalid UTF-8). Report both the
		// protocol-level failure and the semantic content.
		vars := lenientEnv(f.Data)
		path := ""
		for _, v := range vars {
			if strings.EqualFold(v.key, "PATH") {
				path = v.value
			}
		}
		res.decode = fmt.Sprintf("严格解码失败；宽松解析 %d 个变量", len(vars))
		maxLen := 0
		for _, v := range vars {
			if len(v.value) > maxLen {
				maxLen = len(v.value)
			}
		}
		note := fmt.Sprintf("%d 个变量可提取", len(vars))
		if path != "" {
			note += "，PATH 存在"
		} else {
			note += "，未找到 PATH"
		}
		if maxLen >= 500 {
			note += fmt.Sprintf("；最长值 %d 字节（sliver_env_t.value[512]，超出即被截断，PATH 会丢尾部）", maxLen)
		}
		return res.fail(note+"；响应含非 UTF-8(ANSI/GBK) 字符串（Go 服务端会在此 RPC 上反序列化失败）",
			"%v", err)
	}
	var path string
	for _, v := range out.Variables {
		if strings.EqualFold(v.Key, "PATH") {
			path = v.Value
		}
	}
	res.decode = fmt.Sprintf("Variables=%d err=%q", len(out.Variables), errOf(out.Response))
	if e := errOf(out.Response); e != "" {
		return res.fail("env 报错", "err=%q", e)
	}
	if path == "" {
		return res.fail("没有 PATH", "%d 个变量", len(out.Variables))
	}
	return res.pass(fmt.Sprintf("%d 个变量, PATH=%dB", len(out.Variables), len(path)))
}

func cmdExecute(r *runCtx) *result {
	res := &result{name: "execute"}
	out := &sliverpb.Execute{}
	req := &sliverpb.ExecuteReq{
		Path:    cmdExe(),
		Args:    []string{"/c", "echo conf-exec-ok"},
		Output:  true,
		Request: r.request(),
	}
	if _, err := r.rpcTimeout(res, sliverpb.MsgExecuteReq, req, out, maxDuration(r.timeout, 20*time.Second)); err != nil {
		return res.fail("execute 无响应（异步响应按 ID 匹配失败）", "%v", err)
	}
	res.decode = fmt.Sprintf("Status=%d Pid=%d Stdout=%q Stderr=%q err=%q", out.Status, out.Pid,
		string(out.Stdout), string(out.Stderr), errOf(out.Response))
	res.assert = "Stdout 含 conf-exec-ok、Status=0、Pid>0"
	if e := errOf(out.Response); e != "" {
		return res.fail("execute 报错", "err=%q", e)
	}
	if !strings.Contains(string(out.Stdout), "conf-exec-ok") {
		return res.fail("stdout 不含预期字符串", "stdout=%q stderr=%q status=%d", string(out.Stdout),
			string(out.Stderr), out.Status)
	}
	if out.Status != 0 {
		return res.fail("", "Status=%d", out.Status)
	}
	if out.Pid == 0 {
		return res.fail("", "Pid=0")
	}
	return res.pass(fmt.Sprintf("pid=%d", out.Pid))
}

// cmdExecuteAsync proves the execute handler is non-blocking: a slow child runs
// while pwd is answered, and the Execute reply still arrives on the request ID.
func cmdExecuteAsync(r *runCtx) *result {
	res := &result{name: "execute-async"}
	req := &sliverpb.ExecuteReq{
		Path:    cmdExe(),
		Args:    []string{"/c", "ping -n 3 127.0.0.1 >nul & echo conf-async-ok"},
		Output:  true,
		Request: r.request(),
	}
	id, err := r.p.beginMsg(sliverpb.MsgExecuteReq, req)
	res.sent = fmt.Sprintf("%d=%s id=%d（不等回复）", sliverpb.MsgExecuteReq, msgName(sliverpb.MsgExecuteReq), id)
	if err != nil {
		return res.fail("发送失败", "%v", err)
	}
	// while the child sleeps, pwd must still be served
	t0 := time.Now()
	pwd := &sliverpb.Pwd{}
	if _, err := r.rpcTimeout(res, sliverpb.MsgPwdReq, &sliverpb.PwdReq{Request: r.request()}, pwd,
		5*time.Second); err != nil {
		r.p.unregister(id)
		return res.fail("执行期间 pwd 无响应（会话被阻塞）", "%v", err)
	}
	elapsed := time.Since(t0)

	f, err := r.p.awaitReply(id, maxDuration(r.timeout, 20*time.Second))
	if err != nil {
		return res.fail("异步 Execute 回复未到达", "%v", err)
	}
	res.recv = fmt.Sprintf("id=%d type=%d(%s) %dB", f.ID, f.Type, msgName(f.Type), len(f.Data))
	out := &sliverpb.Execute{}
	if derr := proto.Unmarshal(f.Data, out); derr != nil {
		res.decode = "解码失败"
		return res.fail("", "protobuf 解码失败: %v; hex=%s", derr, hexdump(f.Data, 128))
	}
	res.decode = fmt.Sprintf("Status=%d Pid=%d Stdout=%q", out.Status, out.Pid, string(out.Stdout))
	res.assert = "并发期间 pwd 仍应答(<5s)，随后 Execute 回复按同一 ID 到达且 stdout 含 conf-async-ok"
	if !strings.Contains(string(out.Stdout), "conf-async-ok") {
		return res.fail("stdout 不含预期字符串", "stdout=%q stderr=%q", string(out.Stdout), string(out.Stderr))
	}
	return res.pass(fmt.Sprintf("pwd 并发应答 %s 后拿到 Execute", elapsed.Round(time.Millisecond)))
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func cmdExecuteChildren(r *runCtx) *result {
	res := &result{name: "execute-children"}
	out := &sliverpb.ExecuteChildren{}
	if _, err := r.rpc(res, sliverpb.MsgExecuteChildrenReq,
		&sliverpb.ExecuteChildrenReq{Request: r.request()}, out); err != nil {
		return res.fail("execute-children 无响应", "%v", err)
	}
	var desc []string
	for _, c := range out.Children {
		desc = append(desc, fmt.Sprintf("%d:%s", c.Pid, filepath.Base(c.Path)))
	}
	if len(desc) > 6 {
		desc = desc[:6]
	}
	res.decode = fmt.Sprintf("Children=%d [%s] err=%q", len(out.Children), strings.Join(desc, ","),
		errOf(out.Response))
	res.assert = "非空（此时 shell / 后台 execute 子进程仍存活）且 pid>0"
	if e := errOf(out.Response); e != "" {
		return res.fail("execute-children 报错", "err=%q", e)
	}
	if len(out.Children) == 0 {
		return res.fail("子进程列表为空", "shell 与后台 execute 的子进程应当仍存活")
	}
	for _, c := range out.Children {
		if c.Pid <= 0 {
			return res.fail("", "Pid=%d path=%q", c.Pid, c.Path)
		}
	}
	return res.pass(fmt.Sprintf("%d 个子进程", len(out.Children)))
}

func cmdTerminate(r *runCtx) *result {
	res := &result{name: "terminate"}
	victim := exec.Command(cmdExe(), "/c", "ping -n 120 127.0.0.1")
	if err := victim.Start(); err != nil {
		return res.fail("无法在服务端启动受害者进程", "%v", err)
	}
	pid := victim.Process.Pid
	done := make(chan error, 1)
	go func() { done <- victim.Wait() }()
	time.Sleep(200 * time.Millisecond)

	out := &sliverpb.Terminate{}
	if _, err := r.rpc(res, sliverpb.MsgTerminateReq,
		&sliverpb.TerminateReq{Pid: int32(pid), Force: true, Request: r.request()}, out); err != nil {
		return res.fail("terminate 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Pid=%d err=%q", out.Pid, errOf(out.Response))
	res.assert = "Pid 回显且目标进程确实退出"
	if e := errOf(out.Response); e != "" {
		return res.fail("terminate 报错", "err=%q", e)
	}
	if int(out.Pid) != pid {
		return res.fail("", "Pid=%d，期望 %d", out.Pid, pid)
	}
	select {
	case <-done:
		return res.pass(fmt.Sprintf("pid %d 已退出", pid))
	case <-time.After(5 * time.Second):
		_ = victim.Process.Kill()
		return res.fail("目标进程未退出", "pid %d 5s 后仍在运行", pid)
	}
}

// --- system information ----------------------------------------------------

func cmdIfconfig(r *runCtx) *result {
	res := &result{name: "ifconfig"}
	out := &sliverpb.Ifconfig{}
	if _, err := r.rpc(res, sliverpb.MsgIfconfigReq, &sliverpb.IfconfigReq{Request: r.request()}, out); err != nil {
		return res.fail("ifconfig 无响应", "%v", err)
	}
	withIP := 0
	var sample []string
	for _, it := range out.NetInterfaces {
		if it.Name == "" {
			return res.fail("接口名为空", "%+v", it)
		}
		if len(it.IPAddresses) > 0 {
			withIP++
		}
		if len(sample) < 3 {
			sample = append(sample, fmt.Sprintf("%s(%s)", it.Name, strings.Join(it.IPAddresses, "/")))
		}
	}
	res.decode = fmt.Sprintf("NetInterfaces=%d withIP=%d %v err=%q", len(out.NetInterfaces), withIP,
		sample, errOf(out.Response))
	res.assert = "至少一个接口且带 IP 地址"
	if e := errOf(out.Response); e != "" {
		return res.fail("ifconfig 报错", "err=%q", e)
	}
	if len(out.NetInterfaces) == 0 {
		return res.fail("接口列表为空", "")
	}
	if withIP == 0 {
		return res.fail("所有接口都没有 IP", "%d 个接口", len(out.NetInterfaces))
	}
	return res.pass(fmt.Sprintf("%d 个接口", len(out.NetInterfaces)))
}

func cmdNetstat(r *runCtx) *result {
	res := &result{name: "netstat"}
	out := &sliverpb.Netstat{}
	if _, err := r.rpc(res, sliverpb.MsgNetstatReq,
		&sliverpb.NetstatReq{TCP: true, UDP: true, IP4: true, Listening: true, Request: r.request()}, out); err != nil {
		return res.fail("netstat 无响应", "%v", err)
	}
	withAddr, withProc := 0, 0
	var sample []string
	for _, e := range out.Entries {
		if e.LocalAddr != nil && e.LocalAddr.Ip != "" {
			withAddr++
			if len(sample) < 3 {
				sample = append(sample, fmt.Sprintf("%s:%d->%d/%s", e.LocalAddr.Ip, e.LocalAddr.Port,
					remotePort(e), e.SkState))
			}
		}
		if e.Process != nil && e.Process.Pid > 0 {
			withProc++
		}
	}
	res.decode = fmt.Sprintf("Entries=%d withLocalAddr=%d withProc=%d %v err=%q", len(out.Entries),
		withAddr, withProc, sample, errOf(out.Response))
	res.assert = "至少一条连接且带本地地址"
	if e := errOf(out.Response); e != "" {
		return res.fail("netstat 报错", "err=%q", e)
	}
	if len(out.Entries) == 0 {
		return res.fail("连接表为空", "")
	}
	if withAddr == 0 {
		return res.fail("没有带上本地地址的条目", "%d 条", len(out.Entries))
	}
	return res.pass(fmt.Sprintf("%d 条连接", len(out.Entries)))
}

func remotePort(e *sliverpb.SockTabEntry) uint32 {
	if e.RemoteAddr == nil {
		return 0
	}
	return e.RemoteAddr.Port
}

func cmdMount(r *runCtx) *result {
	res := &result{name: "mount"}
	out := &sliverpb.Mount{}
	if _, err := r.rpc(res, sliverpb.MsgMountReq, &sliverpb.MountReq{Request: r.request()}, out); err != nil {
		return res.fail("mount 无响应", "%v", err)
	}
	var sample []string
	withPoint := 0
	for _, m := range out.Info {
		if m.MountPoint != "" {
			withPoint++
			if len(sample) < 3 {
				sample = append(sample, fmt.Sprintf("%s(%s,%d/%d)", m.MountPoint, m.FileSystem,
					m.FreeSpace, m.TotalSpace))
			}
		}
	}
	res.decode = fmt.Sprintf("Info=%d withMountPoint=%d %v err=%q", len(out.Info), withPoint, sample,
		errOf(out.Response))
	res.assert = "至少一个卷且 MountPoint 非空"
	if e := errOf(out.Response); e != "" {
		return res.fail("mount 报错", "err=%q", e)
	}
	if len(out.Info) == 0 {
		return res.fail("卷列表为空", "")
	}
	if withPoint == 0 {
		return res.fail("没有 MountPoint", "%d 个卷", len(out.Info))
	}
	return res.pass(fmt.Sprintf("%d 个卷", len(out.Info)))
}

func cmdScreenshot(r *runCtx) *result {
	res := &result{name: "screenshot"}
	out := &sliverpb.Screenshot{}
	if _, err := r.rpcTimeout(res, sliverpb.MsgScreenshotReq,
		&sliverpb.ScreenshotReq{Request: r.request()}, out, maxDuration(r.timeout, 20*time.Second)); err != nil {
		return res.fail("screenshot 无响应", "%v", err)
	}
	pngOK := len(out.Data) >= 8 && string(out.Data[:8]) == "\x89PNG\r\n\x1a\n"
	res.decode = fmt.Sprintf("Data=%dB png=%v head=%s err=%q", len(out.Data), pngOK,
		hexdump(out.Data, 16), errOf(out.Response))
	res.assert = "Data>0 且以 PNG 魔数 89504E470D0A1A0A 开头"
	if e := errOf(out.Response); e != "" {
		return res.fail("screenshot 报错", "err=%q (data=%dB)", e, len(out.Data))
	}
	if len(out.Data) == 0 {
		return res.fail("Data 为空", "")
	}
	if !pngOK {
		return res.fail("不是 PNG", "头 %s", hexdump(out.Data, 16))
	}
	return res.pass(fmt.Sprintf("%dB PNG", len(out.Data)))
}

func cmdGetPrivs(r *runCtx) *result {
	res := &result{name: "getprivs"}
	out := &sliverpb.GetPrivs{}
	if _, err := r.rpc(res, sliverpb.MsgGetPrivsReq, &sliverpb.GetPrivsReq{Request: r.request()}, out); err != nil {
		return res.fail("getprivs 无响应", "%v", err)
	}
	var names []string
	for _, p := range out.PrivInfo {
		if p.Name == "" {
			return res.fail("权限名为空", "%+v", p)
		}
		if len(names) < 3 {
			names = append(names, p.Name)
		}
	}
	res.decode = fmt.Sprintf("PrivInfo=%d integrity=%q proc=%q %v err=%q", len(out.PrivInfo),
		out.ProcessIntegrity, out.ProcessName, names, errOf(out.Response))
	res.assert = "权限列表非空且 ProcessIntegrity 非空"
	if e := errOf(out.Response); e != "" {
		return res.fail("getprivs 报错", "err=%q", e)
	}
	if len(out.PrivInfo) == 0 {
		return res.fail("权限列表为空", "")
	}
	if out.ProcessIntegrity == "" {
		return res.fail("ProcessIntegrity 为空", "")
	}
	return res.pass(fmt.Sprintf("%d 项, integrity=%s", len(out.PrivInfo), out.ProcessIntegrity))
}

func cmdCurrentTokenOwner(r *runCtx) *result {
	res := &result{name: "current-token-owner"}
	out := &sliverpb.CurrentTokenOwner{}
	if _, err := r.rpc(res, sliverpb.MsgCurrentTokenOwnerReq,
		&sliverpb.CurrentTokenOwnerReq{Request: r.request()}, out); err != nil {
		return res.fail("current-token-owner 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Output=%q", out.Output)
	res.assert = "Output 非空且形如 DOMAIN\\user（或未取到时明确报错）"
	if out.Output == "" {
		return res.fail("Output 为空", "err=%q", errOf(out.Response))
	}
	if !strings.Contains(out.Output, `\`) && !strings.Contains(out.Output, "@") {
		return res.fail("Output 不像 token owner", "Output=%q", out.Output)
	}
	return res.pass(out.Output)
}

func cmdRevToSelf(r *runCtx) *result {
	res := &result{name: "rev2self"}
	out := &sliverpb.RevToSelf{}
	if _, err := r.rpc(res, sliverpb.MsgRevToSelfReq, &sliverpb.RevToSelfReq{Request: r.request()}, out); err != nil {
		return res.fail("rev2self 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("err=%q", errOf(out.Response))
	res.assert = "无错误且会话存活（随后 pwd 正常）"
	if e := errOf(out.Response); e != "" {
		return res.fail("rev2self 报错", "err=%q", e)
	}
	pwd := &sliverpb.Pwd{}
	if _, err := r.rpc(res, sliverpb.MsgPwdReq, &sliverpb.PwdReq{Request: r.request()}, pwd); err != nil {
		return res.fail("rev2self 后 pwd 无响应", "%v", err)
	}
	return res.pass("revert ok")
}

func cmdMakeToken(r *runCtx) *result {
	res := &result{name: "make-token"}
	out := &sliverpb.MakeToken{}
	req := &sliverpb.MakeTokenReq{
		Username:  "sliver-no-such-user",
		Password:  "bad-password",
		Domain:    ".",
		LogonType: 2,
		Request:   r.request(),
	}
	if _, err := r.rpc(res, sliverpb.MsgMakeTokenReq, req, out); err != nil {
		return res.fail("make-token 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("err=%q", errOf(out.Response))
	res.assert = "错误凭据必须返回明确失败（LogonUser failed）且会话存活"
	e := errOf(out.Response)
	if e == "" {
		return res.fail("错误凭据没有报错", "这会被当成成功的 impersonation")
	}
	if !strings.Contains(e, "LogonUser") {
		return res.fail("错误信息不含 LogonUser", "err=%q", e)
	}
	pwd := &sliverpb.Pwd{}
	if _, err := r.rpc(res, sliverpb.MsgPwdReq, &sliverpb.PwdReq{Request: r.request()}, pwd); err != nil {
		return res.fail("make-token 后 pwd 无响应", "%v", err)
	}
	return res.pass(e)
}

func cmdGetSystem(r *runCtx) *result {
	res := &result{name: "getsystem"}
	out := &sliverpb.GetSystem{}
	if _, err := r.rpcTimeout(res, sliverpb.MsgInvokeGetSystemReq,
		&sliverpb.InvokeGetSystemReq{Request: r.request()}, out,
		maxDuration(r.timeout, 25*time.Second)); err != nil {
		return res.fail("getsystem 无响应", "%v", err)
	}
	e := errOf(out.Response)
	res.decode = fmt.Sprintf("err=%q", e)
	res.assert = "有明确结果（失败路径也可），且会话不崩溃"
	pwd := &sliverpb.Pwd{}
	if _, err := r.rpc(res, sliverpb.MsgPwdReq, &sliverpb.PwdReq{Request: r.request()}, pwd); err != nil {
		return res.fail("getsystem 后会话不可用（崩溃？）", "%v", err)
	}
	if r.p.isDead() {
		return res.fail("getsystem 后会话已死", "")
	}
	if e == "" {
		return res.pass("提升成功（SYSTEM）")
	}
	return res.pass("明确的失败路径: " + e)
}

// --- services --------------------------------------------------------------

func cmdServices(r *runCtx) *result {
	res := &result{name: "services"}
	out := &sliverpb.Services{}
	f, err := r.rpc(res, sliverpb.MsgServicesReq, &sliverpb.ServicesReq{Request: r.request()}, out)
	res.assert = "服务列表非空且每项有 Name"
	if err != nil {
		if f == nil {
			return res.fail("services 无响应", "%v", err)
		}
		svcs := lenientServices(f.Data)
		names := make([]string, 0, 4)
		empty := 0
		for _, s := range svcs {
			if s.name == "" {
				empty++
			}
			if len(names) < 4 {
				names = append(names, fmt.Sprintf("%s(%d/%d)", s.name, s.status, s.startup))
			}
		}
		res.decode = fmt.Sprintf("严格解码失败；宽松解析 %d 个服务", len(svcs))
		if len(svcs) == 0 || empty > 0 {
			return res.fail("宽松解析也不可用", "服务数=%d 空名=%d; %v", len(svcs), empty, err)
		}
		sort.Strings(names)
		return res.fail(fmt.Sprintf("%d 个服务可提取（%s）；描述字段含非 UTF-8(GBK) 字节，Go 服务端会反序列化失败",
			len(svcs), strings.Join(names, ",")), "%v", err)
	}
	var names []string
	for _, d := range out.Details {
		if d.Name == "" {
			return res.fail("服务名为空", "%+v", d)
		}
		if len(names) < 4 {
			names = append(names, fmt.Sprintf("%s(%d/%d)", d.Name, d.Status, d.StartupType))
		}
	}
	res.decode = fmt.Sprintf("Details=%d %v Error=%q err=%q", len(out.Details), names,
		out.Error, errOf(out.Response))
	if e := errOf(out.Response); e != "" {
		return res.fail("services 报错", "err=%q", e)
	}
	if len(out.Details) == 0 {
		return res.fail("服务列表为空", "Error=%q", out.Error)
	}
	return res.pass(fmt.Sprintf("%d 个服务", len(out.Details)))
}

func cmdServiceDetail(r *runCtx) *result {
	res := &result{name: "service-detail"}
	out := &sliverpb.ServiceDetail{}
	req := &sliverpb.ServiceDetailReq{
		ServiceInfo: &sliverpb.ServiceInfoReq{ServiceName: "Spooler"},
		Request:     r.request(),
	}
	res.assert = "Detail.Name==Spooler（取不到时必须有明确 Message）"
	f, err := r.rpc(res, sliverpb.MsgServiceDetailReq, req, out)
	if err != nil {
		if f == nil {
			return res.fail("service-detail 无响应", "%v", err)
		}
		svc, msg := lenientServiceDetail(f.Data)
		res.decode = fmt.Sprintf("严格解码失败；宽松解析 Detail{Name=%q} Message=%q", svc.name, msg)
		if !strings.EqualFold(svc.name, "Spooler") {
			return res.fail("宽松解析也没有 Detail.Name=Spooler", "name=%q msg=%q; %v", svc.name, msg, err)
		}
		return res.fail("Detail.Name=Spooler 正确，但 Description 含非 UTF-8(GBK) 字节，Go 服务端会反序列化失败",
			"%v", err)
	}
	name, display, bin := "", "", ""
	if out.Detail != nil {
		name, display, bin = out.Detail.Name, out.Detail.DisplayName, out.Detail.BinPath
	}
	res.decode = fmt.Sprintf("Detail{Name=%q Display=%q BinPath=%q} Message=%q err=%q", name, display,
		bin, out.Message, errOf(out.Response))
	if e := errOf(out.Response); e != "" {
		return res.fail("service-detail 报错", "err=%q", e)
	}
	if out.Detail == nil {
		return res.fail("Detail 为空", "Message=%q", out.Message)
	}
	if !strings.EqualFold(out.Detail.Name, "Spooler") {
		return res.fail("服务名不符", "Name=%q Message=%q", out.Detail.Name, out.Message)
	}
	return res.pass(fmt.Sprintf("Spooler status=%d start=%d", out.Detail.Status, out.Detail.StartupType))
}

func cmdStartService(r *runCtx) *result {
	res := &result{name: "start-service"}
	// The reply payload is the ServiceInfo wrapper (Response at field 9), which
	// is what a Go server unmarshals for MsgStartServiceByNameReq.
	out := &sliverpb.ServiceInfo{}
	req := &sliverpb.StartServiceByNameReq{
		ServiceInfo: &sliverpb.ServiceInfoReq{ServiceName: "sliver-no-such-service"},
		Request:     r.request(),
	}
	if _, err := r.rpc(res, sliverpb.MsgStartServiceByNameReq, req, out); err != nil {
		return res.fail("start-service 无响应", "%v", err)
	}
	e := errOf(out.Response)
	res.decode = fmt.Sprintf("Response.Err=%q", e)
	res.assert = "不存在服务名必须返回明确错误"
	if e == "" {
		return res.fail("不存在的服务没有报错", "响应 0 错误 —— 会被上层当成启动成功")
	}
	if !strings.Contains(strings.ToLower(e), "start-service") {
		return res.fail("错误信息不含 start-service", "err=%q", e)
	}
	return res.pass(e)
}

// --- registry --------------------------------------------------------------

const (
	regHive = "HKCU"
	// HKCU\Software\AppDataLow is the Low-integrity writable area. The harness
	// runs (and therefore the implant runs) at Low IL under the audit sandbox, so
	// HKCU\Software itself rejects writes with ACCESS_DENIED; AppDataLow lets the
	// registry RPCs be verified end to end instead of measuring the sandbox.
	regParent = `Software\AppDataLow`
	regKey    = `SliverCppConf`
	regPath   = `Software\AppDataLow\SliverCppConf`
	regValue  = `TestValue`
	regString = `conf-reg-ok`
)

func cmdRegCreate(r *runCtx) *result {
	res := &result{name: "reg-create"}
	out := &sliverpb.RegistryCreateKey{}
	req := &sliverpb.RegistryCreateKeyReq{Hive: regHive, Path: regParent, Key: regKey, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgRegistryCreateKeyReq, req, out); err != nil {
		return res.fail("reg-create 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("err=%q", errOf(out.Response))
	res.assert = "无错误，且随后 subkeys(Software) 能看到该键"
	if e := errOf(out.Response); e != "" {
		return res.fail("reg-create 报错", "err=%q", e)
	}
	sub := &sliverpb.RegistrySubKeyList{}
	req2 := &sliverpb.RegistrySubKeyListReq{Hive: regHive, Path: regParent, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgRegistrySubKeysListReq, req2, sub); err != nil {
		return res.fail("reg-create 后无法列举 subkeys", "%v", err)
	}
	for _, k := range sub.Subkeys {
		if strings.EqualFold(k, regKey) {
			return res.pass("键已创建")
		}
	}
	return res.fail("subkeys 中找不到新建的键", "subkeys=%v", sub.Subkeys)
}

func cmdRegWrite(r *runCtx) *result {
	res := &result{name: "reg-write"}
	out := &sliverpb.RegistryWrite{}
	req := &sliverpb.RegistryWriteReq{
		Hive:        regHive,
		Path:        regPath,
		Key:         regValue,
		StringValue: regString,
		Type:        uint32(sliverpb.RegistryType_String),
		Request:     r.request(),
	}
	if _, err := r.rpc(res, sliverpb.MsgRegistryWriteReq, req, out); err != nil {
		return res.fail("reg-write 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Response.Err=%q", errOf(out.Response))
	res.assert = "写入无错误，且立刻读回的值与写入一致"
	if e := errOf(out.Response); e != "" {
		return res.fail("reg-write 报错", "err=%q", e)
	}
	// round trip
	rd := &sliverpb.RegistryRead{}
	rdReq := &sliverpb.RegistryReadReq{Hive: regHive, Path: regPath, Key: regValue, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgRegistryReadReq, rdReq, rd); err != nil {
		return res.fail("写入后读回无响应", "%v", err)
	}
	if e := errOf(rd.Response); e != "" {
		return res.fail("写入后读回报错", "err=%q", e)
	}
	if rd.Value != regString {
		return res.fail("往返不一致", "写入 %q，读回 %q", regString, rd.Value)
	}
	return res.pass("write->read 往返一致")
}

func cmdRegRead(r *runCtx) *result {
	res := &result{name: "reg-read"}
	out := &sliverpb.RegistryRead{}
	req := &sliverpb.RegistryReadReq{Hive: regHive, Path: regPath, Key: regValue, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgRegistryReadReq, req, out); err != nil {
		return res.fail("reg-read 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Value=%q Type=%d Binary=%dB err=%q", out.Value, out.Type, len(out.Binary),
		errOf(out.Response))
	res.assert = fmt.Sprintf("Value==%q 且 Type==String(2)", regString)
	if e := errOf(out.Response); e != "" {
		return res.fail("reg-read 报错", "err=%q", e)
	}
	if out.Value != regString {
		return res.fail("值不一致", "got %q want %q（hex=%s）", out.Value, regString, hexdump([]byte(out.Value), 64))
	}
	if out.Type != sliverpb.RegistryType_String {
		return res.fail("类型不符", "Type=%d，期望 2(String)", out.Type)
	}
	return res.pass("string 类型值正确")
}

func cmdRegValues(r *runCtx) *result {
	res := &result{name: "reg-values"}
	out := &sliverpb.RegistryValuesList{}
	req := &sliverpb.RegistryListValuesReq{Hive: regHive, Path: regPath, Request: r.request()}
	res.assert = fmt.Sprintf("包含 %q", regValue)
	f, err := r.rpc(res, sliverpb.MsgRegistryListValuesReq, req, out)
	if err != nil {
		if f == nil {
			return res.fail("reg-values 无响应", "%v", err)
		}
		names := lenientNames(f.Data)
		res.decode = fmt.Sprintf("严格解码失败；宽松解析 %d 个值", len(names))
		for _, n := range names {
			if strings.EqualFold(n, regValue) {
				return res.fail("值存在，但列表含非 UTF-8 值名，Go 服务端会反序列化失败", "%v", err)
			}
		}
		return res.fail("宽松解析也没有 TestValue", "names=%v; %v", names, err)
	}
	res.decode = fmt.Sprintf("ValueNames=%v err=%q", out.ValueNames, errOf(out.Response))
	if e := errOf(out.Response); e != "" {
		return res.fail("reg-values 报错", "err=%q", e)
	}
	for _, n := range out.ValueNames {
		if strings.EqualFold(n, regValue) {
			return res.pass(fmt.Sprintf("%d 个值", len(out.ValueNames)))
		}
	}
	return res.fail("值列表里没有 TestValue", "ValueNames=%v", out.ValueNames)
}

func cmdRegSubkeys(r *runCtx) *result {
	res := &result{name: "reg-subkeys"}
	out := &sliverpb.RegistrySubKeyList{}
	req := &sliverpb.RegistrySubKeyListReq{Hive: regHive, Path: regParent, Request: r.request()}
	res.assert = fmt.Sprintf("%s 下包含 %q", regParent, regKey)
	f, err := r.rpc(res, sliverpb.MsgRegistrySubKeysListReq, req, out)
	if err != nil {
		if f == nil {
			return res.fail("reg-subkeys 无响应", "%v", err)
		}
		keys := lenientNames(f.Data)
		res.decode = fmt.Sprintf("严格解码失败；宽松解析 %d 个子键", len(keys))
		for _, k := range keys {
			if strings.EqualFold(k, regKey) {
				return res.fail("子键存在，但列表含非 UTF-8 键名，Go 服务端会反序列化失败", "%v", err)
			}
		}
		return res.fail("宽松解析也没有测试键", "keys=%v; %v", headStrings(keys, 12), err)
	}
	res.decode = fmt.Sprintf("Subkeys=%d %v err=%q", len(out.Subkeys), headStrings(out.Subkeys, 5),
		errOf(out.Response))
	if e := errOf(out.Response); e != "" {
		return res.fail("reg-subkeys 报错", "err=%q", e)
	}
	for _, k := range out.Subkeys {
		if strings.EqualFold(k, regKey) {
			return res.pass(fmt.Sprintf("%d 个子键", len(out.Subkeys)))
		}
	}
	return res.fail("子键列表里没有测试键", "Subkeys=%v", headStrings(out.Subkeys, 20))
}

func cmdRegDelete(r *runCtx) *result {
	res := &result{name: "reg-delete"}
	out := &sliverpb.RegistryDeleteKey{}
	req := &sliverpb.RegistryDeleteKeyReq{Hive: regHive, Path: regPath, Key: regValue, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgRegistryDeleteKeyReq, req, out); err != nil {
		return res.fail("reg-delete 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("delete value Response.Err=%q", errOf(out.Response))
	res.assert = "删除值后再读必须失败；再删除测试键并从 subkeys 消失（清理）"
	if e := errOf(out.Response); e != "" {
		return res.fail("reg-delete 报错", "err=%q", e)
	}
	rd := &sliverpb.RegistryRead{}
	rdReq := &sliverpb.RegistryReadReq{Hive: regHive, Path: regPath, Key: regValue, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgRegistryReadReq, rdReq, rd); err != nil {
		return res.fail("删除后读回无响应", "%v", err)
	}
	if errOf(rd.Response) == "" {
		return res.fail("值删除后仍可读出", "Value=%q", rd.Value)
	}
	// cleanup the key itself
	keyDel := &sliverpb.RegistryDeleteKey{}
	keyReq := &sliverpb.RegistryDeleteKeyReq{Hive: regHive, Path: regParent, Key: regKey, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgRegistryDeleteKeyReq, keyReq, keyDel); err != nil {
		return res.fail("清理测试键无响应", "%v", err)
	}
	if e := errOf(keyDel.Response); e != "" {
		return res.fail("清理测试键失败", "err=%q", e)
	}
	sub := &sliverpb.RegistrySubKeyList{}
	subReq := &sliverpb.RegistrySubKeyListReq{Hive: regHive, Path: regParent, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgRegistrySubKeysListReq, subReq, sub); err != nil {
		return res.fail("清理后列举 subkeys 无响应", "%v", err)
	}
	for _, k := range sub.Subkeys {
		if strings.EqualFold(k, regKey) {
			return res.fail("测试键仍存在", "subkeys 仍包含 %q", k)
		}
	}
	return res.pass("值已删除、测试键已清理")
}

func cmdRegReadHive(r *runCtx) *result {
	res := &result{name: "reg-read-hive"}
	out := &sliverpb.RegistryReadHive{}
	req := &sliverpb.RegistryReadHiveReq{
		RootHive:      regHive,
		RequestedHive: `HKCU\Software\SliverCppConf`,
		Request:       r.request(),
	}
	if _, err := r.rpcTimeout(res, sliverpb.MsgRegistryReadHiveReq, req, out,
		maxDuration(r.timeout, 25*time.Second)); err != nil {
		return res.fail("reg-read-hive 无响应", "%v", err)
	}
	e := errOf(out.Response)
	res.decode = fmt.Sprintf("Data=%dB Encoder=%q err=%q", len(out.Data), out.Encoder, e)
	res.assert = "要么导出成功(Data>0 且 Encoder 非空)，要么给出明确失败（需 SeBackupPrivilege）"
	if len(out.Data) > 0 {
		if out.Encoder == "" {
			return res.fail("有数据但 Encoder 为空", "Data=%dB", len(out.Data))
		}
		return res.pass(fmt.Sprintf("%dB hive, encoder=%s", len(out.Data), out.Encoder))
	}
	if e == "" {
		return res.fail("既没有数据也没有错误", "Data=0 err=\"\"")
	}
	return res.pass("明确失败路径: " + e)
}

func cmdSpawnDll(r *runCtx) *result {
	res := &result{name: "spawn-dll"}
	req := &sliverpb.SpawnDllReq{ProcessName: "sliver_payload.dll", Request: r.request()}
	payload, err := proto.Marshal(req)
	if err != nil {
		return res.fail("编码失败", "%v", err)
	}
	f, id, err := r.p.callBytes(sliverpb.MsgSpawnDllReq, payload, r.timeout)
	res.sent = fmt.Sprintf("%d=%s id=%d", sliverpb.MsgSpawnDllReq, msgName(sliverpb.MsgSpawnDllReq), id)
	if err != nil {
		return res.fail("spawn-dll 无响应", "%v", err)
	}
	res.recv = fmt.Sprintf("id=%d type=%d(%s) %dB", f.ID, f.Type, msgName(f.Type), len(f.Data))
	if f.Unknown {
		res.recv += " unknown-type"
		res.decode = "—"
		res.assert = "未注册到 g_handlers 的消息必须以 UnknownMessageType 回包"
		return res.pass("明确失败路径：UnknownMessageType=true")
	}
	out := &sliverpb.SpawnDll{}
	if derr := proto.Unmarshal(f.Data, out); derr != nil {
		res.decode = "解码失败"
		return res.fail("", "protobuf 解码失败: %v; hex=%s", derr, hexdump(f.Data, 128))
	}
	res.decode = fmt.Sprintf("Result=%q err=%q", out.Result, errOf(out.Response))
	res.assert = "有明确结果（Result 或 err 非空）"
	if out.Result == "" && errOf(out.Response) == "" {
		return res.fail("既无 Result 也无错误", "")
	}
	return res.pass("已处理: " + firstNonEmpty(out.Result, errOf(out.Response)))
}

func cmdUnknownType(r *runCtx) *result {
	res := &result{name: "unknown-type"}
	const typ = uint32(200)
	payload := []byte{0x08, 0x01} // field 1 = varint 1, arbitrary body
	f, id, err := r.p.callBytes(typ, payload, r.timeout)
	res.sent = fmt.Sprintf("%d=type-200 id=%d", typ, id)
	if err != nil {
		return res.fail("未知类型无响应", "%v", err)
	}
	res.recv = fmt.Sprintf("id=%d type=%d unknown=%v %dB", f.ID, f.Type, f.Unknown, len(f.Data))
	res.decode = "—"
	res.assert = "必须回 Envelope{ID 相同, UnknownMessageType=true}"
	if f.ID != id {
		return res.fail("ID 不匹配", "got %d want %d", f.ID, id)
	}
	if !f.Unknown {
		return res.fail("没有置 UnknownMessageType", "type=%d data=%s", f.Type, hexdump(f.Data, 64))
	}
	return res.pass("UnknownMessageType 正确")
}

// --- tunnels ---------------------------------------------------------------

func decodeTunnel(f *frame) (*sliverpb.TunnelData, bool) {
	td := &sliverpb.TunnelData{}
	if err := proto.Unmarshal(f.Data, td); err != nil {
		return nil, false
	}
	return td, true
}

// tunnelRx accumulates what arrived on one tunnel.
//
// Frames are reassembled by Sequence, exactly like the real server does
// (server/core/tunnels.go buffers out-of-order frames in pendingFromImplant and
// flushes them in order), so the byte stream is reconstructed even when the
// implant's frames arrive out of order.
type tunnelRx struct {
	text     string
	frames   int
	bytes    int
	seqs     []uint64
	acks     []uint64
	sizes    []int
	resends  int
	reorders int
	maxAhead uint64
	raw      [][]byte

	next    uint64
	pending map[uint64][]byte
	sb      strings.Builder
}

func (t *tunnelRx) add(seq uint64, data []byte) {
	if t.pending == nil {
		t.pending = map[uint64][]byte{}
	}
	if seq != t.next {
		t.reorders++
		if seq > t.next && seq-t.next > t.maxAhead {
			t.maxAhead = seq - t.next
		}
	}
	if _, dup := t.pending[seq]; !dup && seq >= t.next {
		t.pending[seq] = append([]byte(nil), data...)
	}
	for {
		d, ok := t.pending[t.next]
		if !ok {
			break
		}
		t.sb.Write(d)
		delete(t.pending, t.next)
		t.next++
	}
	t.text = t.sb.String()
}

func (t *tunnelRx) meta() string {
	if t.frames == 0 {
		return "0 帧"
	}
	n := len(t.seqs)
	if n > 8 {
		n = 8
	}
	return fmt.Sprintf("%d 帧/%dB seq=%v ack=%v len=%v 乱序=%d 最大超前=%d resend=%d", t.frames,
		t.bytes, t.seqs[:n], t.acks[:n], t.sizes[:n], t.reorders, t.maxAhead, t.resends)
}

// contiguity reports the first hole in the sequence space (0,1,2,…).
func (t *tunnelRx) contiguity() string {
	sorted := append([]uint64(nil), t.seqs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for i, s := range sorted {
		if s != uint64(i) {
			return fmt.Sprintf("序号空洞 帧#%d seq=%d 期望%d", i, s, i)
		}
	}
	return "序号 0..N 无空洞"
}

// collectTunnel accumulates TunnelData for one tunnel until marker shows up.
func (r *runCtx) collectTunnel(tunnel uint64, marker string, timeout time.Duration) (*tunnelRx, error) {
	deadline := time.Now().Add(timeout)
	rx := &tunnelRx{}
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return rx, fmt.Errorf("timeout: no TunnelData containing %q (got %dB)", marker, rx.sb.Len())
		}
		f, err := r.p.waitAsync(func(f *frame) bool {
			if f.Type != sliverpb.MsgTunnelData {
				return false
			}
			td, ok := decodeTunnel(f)
			return ok && td.TunnelID == tunnel
		}, remain)
		if err != nil {
			return rx, err
		}
		td, ok := decodeTunnel(f)
		if !ok {
			return rx, fmt.Errorf("TunnelData 解码失败: %s", hexdump(f.Data, 64))
		}
		rx.frames++
		rx.bytes += len(td.Data)
		rx.seqs = append(rx.seqs, td.Sequence)
		rx.acks = append(rx.acks, td.Ack)
		rx.sizes = append(rx.sizes, len(td.Data))
		if td.Resend {
			rx.resends++
		}
		rx.raw = append(rx.raw, f.Raw)
		rx.add(td.Sequence, td.Data)
		if marker != "" && strings.Contains(rx.text, marker) {
			return rx, nil
		}
	}
}

func cmdShell(r *runCtx) *result {
	res := &result{name: "shell"}
	tunnel := uint64(0xAABBCC01)
	r.shellTunnel = tunnel
	out := &sliverpb.Shell{}
	req := &sliverpb.ShellReq{
		Path:     "cmd.exe",
		Rows:     120,
		Cols:     40,
		TunnelID: tunnel,
		Request:  r.request(),
	}
	if _, err := r.rpc(res, sliverpb.MsgShellReq, req, out); err != nil {
		return res.fail("shell 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q Pid=%d TunnelID=%#x err=%q", out.Path, out.Pid, out.TunnelID,
		errOf(out.Response))
	res.assert = "TunnelID 回显客户端指定值、Pid>0；随后 TunnelData 双向可达（回显命令输出）"
	if e := errOf(out.Response); e != "" {
		return res.fail("shell 报错", "err=%q", e)
	}
	if out.TunnelID != tunnel {
		return res.fail("TunnelID 不符", "got %#x want %#x", out.TunnelID, tunnel)
	}
	if out.Pid == 0 {
		return res.fail("Pid=0", "")
	}
	r.shellPid = int(out.Pid)

	// stdin -> child. Sequence starts at 0: the implant's read_sequence starts 0.
	marker := "conf-shell-ok"
	if _, err := r.p.fire(sliverpb.MsgTunnelData, &sliverpb.TunnelData{
		Data:     []byte("echo " + marker + "\r\n"),
		Sequence: 0,
		TunnelID: tunnel,
	}); err != nil {
		return res.fail("发送 TunnelData 失败", "%v", err)
	}
	rx, err := r.collectTunnel(tunnel, marker, maxDuration(r.timeout, 20*time.Second))
	if err != nil {
		return res.fail("没有收到命令回显: "+rx.meta(), "%v; 已收 %q", err, rx.text)
	}
	return res.pass(fmt.Sprintf("pid=%d %s 含 %s", out.Pid, rx.meta(), marker))
}

// cmdShellDefault replicates the exact sequence tests/mtls-testserver hangs on:
// ShellReq without a Path (implant picks powershell.exe -NoExit / cmd.exe /K),
// then "dir C:\Windows\System32" + "echo after-big" over the tunnel.
func cmdShellDefault(r *runCtx) *result {
	res := &result{name: "shell-default"}
	tunnel := uint64(0xAABBCC03)
	r.defaultTunnel = tunnel
	out := &sliverpb.Shell{}
	req := &sliverpb.ShellReq{
		Rows:     120,
		Cols:     80,
		TunnelID: tunnel,
		Request:  r.request(),
	}
	if _, err := r.rpc(res, sliverpb.MsgShellReq, req, out); err != nil {
		return res.fail("shell(默认 path) 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Path=%q Pid=%d TunnelID=%#x err=%q", out.Path, out.Pid, out.TunnelID,
		errOf(out.Response))
	res.assert = "默认 shell 收到 dir+echo 命令后必须有 TunnelData 回显 after-big（mtls-testserver 卡死点）"
	if e := errOf(out.Response); e != "" {
		return res.fail("shell 报错", "err=%q", e)
	}
	if out.TunnelID != tunnel {
		return res.fail("TunnelID 不符", "got %#x want %#x", out.TunnelID, tunnel)
	}
	r.defaultPid = int(out.Pid)
	// Same bytes the Go peer writes.
	payload := []byte("dir C:\\Windows\\System32\r\necho after-big\r\n")
	if _, err := r.p.fire(sliverpb.MsgTunnelData, &sliverpb.TunnelData{
		Data:     payload,
		Sequence: 0,
		TunnelID: tunnel,
	}); err != nil {
		return res.fail("发送 TunnelData 失败", "%v", err)
	}
	rx, err := r.collectTunnel(tunnel, "after-big", maxDuration(r.timeout, 20*time.Second))
	note := fmt.Sprintf("shell=%q pid=%d %s %s", out.Path, out.Pid, rx.meta(), rx.contiguity())
	if err != nil {
		if rx.frames == 0 {
			note = fmt.Sprintf("shell=%q pid=%d 隧道 0 帧：stdin 未转发 或 子 shell 无输出", out.Path, out.Pid)
		} else {
			note = fmt.Sprintf("shell=%q pid=%d 有回显但无 after-big：%s", out.Path, out.Pid, rx.meta())
		}
		res.detail = append(res.detail, fmt.Sprintf("received-text=%q", rx.text))
		for i, raw := range rx.raw {
			if i >= 3 {
				break
			}
			res.detail = append(res.detail, fmt.Sprintf("envelope[%d]=%s", i, hexdump(raw, 96)))
		}
		res.detail = append(res.detail, r.implantLogEvidence("shell", "tunnel")...)
		return res.fail(note, "%v", err)
	}
	if !strings.Contains(rx.text, "after-big") {
		return res.fail("回显里没有 after-big", "text=%q", rx.text)
	}
	// The real server buffers out-of-order frames in a window of
	// maxTunnelPendingFrames (server/core/tunnels.go:128) and errors with
	// ErrTunnelSequenceWindow past it.
	if rx.maxAhead > 128 {
		return res.fail(fmt.Sprintf("乱序超前 %d 帧 > 真实服务端窗口 128（server/core/tunnels.go: "+
			"maxTunnelPendingFrames=128 → ErrTunnelSequenceWindow）", rx.maxAhead),
			"%s", rx.meta())
	}
	return res.pass(note)
}

// cmdShellBulk quantifies data loss: the child prints N numbered lines and the
// suite checks that every single one survived the tunnel. Frame numbering alone
// can look "gappy" without losing bytes, this row makes loss measurable.
func cmdShellBulk(r *runCtx) *result {
	res := &result{name: "shell-bulk"}
	tunnel := uint64(0xAABBCC04)
	r.bulkTunnel = tunnel
	const lines = 1200
	out := &sliverpb.Shell{}
	req := &sliverpb.ShellReq{Rows: 200, Cols: 200, TunnelID: tunnel, Request: r.request()}
	if _, err := r.rpc(res, sliverpb.MsgShellReq, req, out); err != nil {
		return res.fail("shell(默认 path) 无响应", "%v", err)
	}
	if out.TunnelID != tunnel || out.Pid == 0 {
		return res.fail("Shell 响应字段不符", "TunnelID=%#x Pid=%d", out.TunnelID, out.Pid)
	}
	r.bulkPid = int(out.Pid)
	cmdline := fmt.Sprintf("for ($i=1; $i -le %d; $i++) { \"LN$i\" }\r\necho bulk-done\r\n", lines)
	if _, err := r.p.fire(sliverpb.MsgTunnelData, &sliverpb.TunnelData{
		Data:     []byte(cmdline),
		Sequence: 0,
		TunnelID: tunnel,
	}); err != nil {
		return res.fail("发送 TunnelData 失败", "%v", err)
	}
	rx, err := r.collectTunnel(tunnel, "bulk-done", maxDuration(r.timeout, 30*time.Second))
	if err != nil {
		return res.fail("没有收到 bulk-done: "+rx.meta(), "%v", err)
	}
	seen := map[int]bool{}
	for _, line := range strings.Split(rx.text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "LN") {
			continue
		}
		if v, cerr := strconv.Atoi(line[2:]); cerr == nil {
			seen[v] = true
		}
	}
	var missing []string
	for i := 1; i <= lines; i++ {
		if !seen[i] {
			if len(missing) < 8 {
				missing = append(missing, fmt.Sprintf("LN%d", i))
			}
		}
	}
	res.decode = fmt.Sprintf("shell=%q Pid=%d 收到 %d 行标记", out.Path, out.Pid, len(seen))
	res.assert = fmt.Sprintf("%d 行编号输出必须一行不缺（帧编号跳跃是否造成了真实丢字节）", lines)
	if len(missing) > 0 {
		res.detail = append(res.detail, fmt.Sprintf("缺失 %d/%d 行, 前几个: %v", lines-len(seen), lines, missing))
		res.detail = append(res.detail, r.implantLogEvidence("tunnel", "shell")...)
		return res.fail(fmt.Sprintf("丢数据：缺 %d/%d 行；%s；%s", lines-len(seen), lines, rx.meta(),
			rx.contiguity()), "")
	}
	return res.pass(fmt.Sprintf("%d/%d 行齐全, %s, %s", len(seen), lines, rx.meta(), rx.contiguity()))
}

// implantLogEvidence pulls implant-side log lines mentioning any keyword, so a
// tunnel failure can be attributed (stdin never forwarded vs child silent).
func (r *runCtx) implantLogEvidence(keywords ...string) []string {
	data, err := os.ReadFile(r.implantLog)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	var out []string
	start := len(lines) - 400
	if start < 0 {
		start = 0
	}
	for _, l := range lines[start:] {
		low := strings.ToLower(l)
		for _, k := range keywords {
			if strings.Contains(low, k) {
				out = append(out, "implant.log: "+strings.TrimRight(l, "\r"))
				break
			}
		}
	}
	if len(out) > 8 {
		out = out[len(out)-8:]
	}
	return out
}

func cmdShellResize(r *runCtx) *result {
	res := &result{name: "shell-resize"}
	if r.shellTunnel == 0 {
		return res.fail("前置条件缺失：shell 未建立", "")
	}
	req := &sliverpb.ShellResizeReq{Rows: 50, Cols: 132, TunnelID: r.shellTunnel, Request: r.request()}
	f, id, err := r.p.callBytes(sliverpb.MsgShellResizeReq, mustMarshal(req), r.timeout)
	res.sent = fmt.Sprintf("%d=%s id=%d tunnel=%#x", sliverpb.MsgShellResizeReq,
		msgName(sliverpb.MsgShellResizeReq), id, r.shellTunnel)
	if err != nil {
		return res.fail("shell-resize 无响应", "%v", err)
	}
	res.recv = fmt.Sprintf("id=%d type=%d(%s) %dB", f.ID, f.Type, msgName(f.Type), len(f.Data))
	res.raw = f.Data
	res.decode = "空响应（无 payload 消息）"
	res.assert = "同一 ID 上收到应答（真实服务端 unary 等待），会话存活"
	if f.ID != id {
		return res.fail("ID 不匹配", "got %d want %d", f.ID, id)
	}
	pwd := &sliverpb.Pwd{}
	if _, err := r.rpc(res, sliverpb.MsgPwdReq, &sliverpb.PwdReq{Request: r.request()}, pwd); err != nil {
		return res.fail("resize 后会话不可用", "%v", err)
	}
	note := "resp type=0（C 表 resp_type=0），真实服务端按 ID 匹配仍可完成 unary"
	if f.Type == sliverpb.MsgShellResizeReq {
		note = "resp type 与请求相同"
	}
	return res.pass(note)
}

func mustMarshal(m proto.Message) []byte {
	b, _ := proto.Marshal(m)
	return b
}

func cmdPortfwd(r *runCtx) *result {
	res := &result{name: "portfwd"}
	echo, err := startEchoServer()
	if err != nil {
		return res.fail("服务端 echo server 启动失败", "%v", err)
	}
	defer echo.Close()
	r.echo = echo
	tunnel := uint64(0xAABBCC02)
	r.portfwdTunnel = tunnel

	out := &sliverpb.Portfwd{}
	req := &sliverpb.PortfwdReq{
		Host:     "127.0.0.1",
		Port:     uint32(echo.port),
		Protocol: int32(sliverpb.PortFwdProtoTCP),
		TunnelID: tunnel,
		Request:  r.request(),
	}
	if _, err := r.rpc(res, sliverpb.MsgPortfwdReq, req, out); err != nil {
		return res.fail("portfwd 无响应", "%v", err)
	}
	res.decode = fmt.Sprintf("Host=%q Port=%d TunnelID=%#x err=%q", out.Host, out.Port, out.TunnelID,
		errOf(out.Response))
	res.assert = "回显 host/port/tunnel；TunnelData 发到 %d 后能收到 echo server 的 ECHO: 前缀回包"
	if e := errOf(out.Response); e != "" {
		return res.fail("portfwd 报错", "err=%q", e)
	}
	if out.TunnelID != tunnel || int(out.Port) != echo.port || out.Host != "127.0.0.1" {
		return res.fail("响应字段不符", "got host=%q port=%d tunnel=%#x", out.Host, out.Port, out.TunnelID)
	}

	probe := "echo-me-please"
	if _, err := r.p.fire(sliverpb.MsgTunnelData, &sliverpb.TunnelData{
		Data:     []byte(probe),
		Sequence: 0,
		TunnelID: tunnel,
	}); err != nil {
		return res.fail("发送 TunnelData 失败", "%v", err)
	}
	rx, err := r.collectTunnel(tunnel, probe, maxDuration(r.timeout, 20*time.Second))
	if err != nil {
		return res.fail("没有收到回显数据: "+rx.meta(), "%v; 已收 %q", err, rx.text)
	}
	if !strings.Contains(rx.text, probe) {
		return res.fail("回显内容不含探测串", "收到 %q", rx.text)
	}
	if !strings.Contains(rx.text, "ECHO:") {
		return res.fail("回显没有经过 echo server", "收到 %q", rx.text)
	}
	return res.pass(fmt.Sprintf("%s 回显 %q", rx.meta(), strings.TrimSpace(rx.text)))
}

func cmdTunnelClose(r *runCtx) *result {
	res := &result{name: "tunnel-close"}
	if r.shellTunnel == 0 && r.portfwdTunnel == 0 {
		return res.fail("前置条件缺失：没有打开的隧道", "")
	}
	var closed []string
	if r.portfwdTunnel != 0 {
		if _, err := r.p.fire(sliverpb.MsgTunnelClose, &sliverpb.TunnelData{
			Closed:   true,
			Sequence: 1,
			TunnelID: r.portfwdTunnel,
		}); err != nil {
			return res.fail("portfwd TunnelClose 发送失败", "%v", err)
		}
		closed = append(closed, fmt.Sprintf("portfwd=%#x", r.portfwdTunnel))
	}
	if r.shellTunnel != 0 {
		if _, err := r.p.fire(sliverpb.MsgTunnelClose, &sliverpb.TunnelData{
			Closed:   true,
			Sequence: 1,
			TunnelID: r.shellTunnel,
		}); err != nil {
			return res.fail("shell TunnelClose 发送失败", "%v", err)
		}
		closed = append(closed, fmt.Sprintf("shell=%#x", r.shellTunnel))
	}
	if r.defaultTunnel != 0 {
		if _, err := r.p.fire(sliverpb.MsgTunnelClose, &sliverpb.TunnelData{
			Closed:   true,
			Sequence: 1,
			TunnelID: r.defaultTunnel,
		}); err != nil {
			return res.fail("shell-default TunnelClose 发送失败", "%v", err)
		}
		closed = append(closed, fmt.Sprintf("shell-default=%#x", r.defaultTunnel))
	}
	if r.bulkTunnel != 0 {
		if _, err := r.p.fire(sliverpb.MsgTunnelClose, &sliverpb.TunnelData{
			Closed:   true,
			Sequence: 1,
			TunnelID: r.bulkTunnel,
		}); err != nil {
			return res.fail("shell-bulk TunnelClose 发送失败", "%v", err)
		}
		closed = append(closed, fmt.Sprintf("shell-bulk=%#x", r.bulkTunnel))
	}
	res.sent = "23=" + msgName(sliverpb.MsgTunnelClose) + " id=0 tunnel=" + strings.Join(closed, ",")
	res.recv = "无回复(预期: TunnelClose 是单向控制帧)"
	res.decode = "—"
	res.assert = "不回包、不崩；shell 子进程被回收；随后 pwd 正常"

	// the shell children must be terminated by the close
	if r.shellPid != 0 {
		if alive := waitProcessGone(r.shellPid, 6*time.Second); alive {
			return res.fail("shell 子进程未被回收", "pid %d 6s 后仍存活", r.shellPid)
		}
	}
	if r.defaultPid != 0 {
		if alive := waitProcessGone(r.defaultPid, 6*time.Second); alive {
			return res.fail("shell-default 子进程未被回收", "pid %d 6s 后仍存活", r.defaultPid)
		}
	}
	if r.bulkPid != 0 {
		if alive := waitProcessGone(r.bulkPid, 6*time.Second); alive {
			return res.fail("shell-bulk 子进程未被回收", "pid %d 6s 后仍存活", r.bulkPid)
		}
	}
	pwd := &sliverpb.Pwd{}
	if _, err := r.rpc(res, sliverpb.MsgPwdReq, &sliverpb.PwdReq{Request: r.request()}, pwd); err != nil {
		return res.fail("关闭隧道后 pwd 无响应（会话可能崩了）", "%v", err)
	}
	if r.shellPid != 0 {
		return res.pass(fmt.Sprintf("shell pid %d 已回收", r.shellPid))
	}
	return res.pass("控制帧已发送，会话存活")
}

// waitProcessGone polls tasklist until pid disappears; returns true if still
// alive after the timeout.
func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return processAlive(pid)
}

func processAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf(" %d ", pid))
}

func headStrings(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
