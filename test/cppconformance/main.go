package main

// cppconformance - a conformance harness for the C implant (implant-cpp).
//
// It speaks the real Sliver mTLS session protocol (TLS 1.3 mTLS + MUX/1 +
// yamux + signed Envelopes), launches the compiled implant.exe against itself,
// then exercises every RPC the implant implements and asserts the *semantics*
// of each reply (not just "something came back"), decoding every response with
// the real protobuf types from protobuf/sliverpb.
//
// Server model (this is what makes the results trustworthy):
//   - request: signed Envelope{ID: fresh non-zero, Type: request id, Data:
//     marshalled request} on a new yamux stream
//   - reply:   the implant opens its own stream and sends
//     Envelope{ID: same id, Type: whatever its handler table says}
//   - matching is by ID only (server/c2/mtls.go DeliverResponse,
//     server/core/connnection.go ID!=0 -> synchronous reply)
//   - ID==0 frames are asynchronous pushes (TunnelData/TunnelClose/Ping/
//     Register) and are classified by Type
//
// Exit code is non-zero when any command fails.

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"google.golang.org/protobuf/proto"

	"github.com/bishopfox/sliver/protobuf/commonpb"
	"github.com/bishopfox/sliver/protobuf/sliverpb"
)

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

type logger struct {
	mu sync.Mutex
	w  io.Writer
}

var glog = &logger{w: os.Stdout}

func (l *logger) Printf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, format+"\n", args...)
}

func (l *logger) Raw(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, format, args...)
}

// ---------------------------------------------------------------------------
// result rows
// ---------------------------------------------------------------------------

const (
	statusPass = "PASS"
	statusFail = "FAIL"
	statusSkip = "SKIP"
)

type result struct {
	name   string
	sent   string
	recv   string
	decode string
	assert string
	status string
	note   string
	detail []string
	raw    []byte // last response payload, kept for failure evidence
}

func (r *result) fail(note string, format string, args ...interface{}) *result {
	r.status = statusFail
	if note != "" {
		r.note = note
	}
	if format != "" {
		r.detail = append(r.detail, fmt.Sprintf(format, args...))
	}
	return r
}

func (r *result) pass(note string) *result {
	r.status = statusPass
	r.note = note
	return r
}

// ---------------------------------------------------------------------------
// run context
// ---------------------------------------------------------------------------

type runCtx struct {
	p          *peer
	dir        string
	fixtures   string
	timeout    time.Duration
	implantPID int
	echo       *echoServer
	implantLog string

	// tunnels opened by earlier rows, closed by later ones
	shellTunnel   uint64
	shellPid      int
	defaultTunnel uint64
	defaultPid    int
	bulkTunnel    uint64
	bulkPid       int
	portfwdTunnel uint64
}

// request builds the commonpb.Request every real server RPC carries
// (Request is field 9 of every request message; the implant must skip it).
func (r *runCtx) request() *commonpb.Request {
	return &commonpb.Request{
		SessionID: "cppconformance-session",
		Timeout:   int64(r.timeout / time.Second),
	}
}

// ---------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------

func main() {
	os.Exit(cli())
}

func cli() int {
	implant := flag.String("implant", `D:\GOPATH\src\C2\sliver\implant-cpp\build\msvc-x64-debug\bin\implant.exe`, "implant.exe to launch and drive")
	addr := flag.String("addr", "127.0.0.1", "listener address")
	port := flag.Int("port", 0, "listener port (0 picks a free port)")
	tmp := flag.String("tmp", `D:\GOPATH\src\C2\.audit\confwork`, "work directory for fixtures and test files")
	timeout := flag.Duration("timeout", 10*time.Second, "per-command timeout")
	logPath := flag.String("log", `D:\GOPATH\src\C2\.audit\conformance-run.log`, "raw run log")
	only := flag.String("only", "", "comma separated subset of command names")
	name := flag.String("name", "cpp-conformance", "implant name reported in Register")
	agePriv := flag.String("age-priv", "AGE-SECRET-KEY-1TESTTESTTESTTESTTESTTESTTESTTESTTESTTESTTESTTESTTEST", "age private key handed to the implant")
	verbose := flag.Bool("v", false, "echo wire level logging")
	keepImplant := flag.Bool("keep", false, "do not kill the implant at the end")
	flag.Parse()

	// --- log sink ---------------------------------------------------------
	if dir := filepath.Dir(*logPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	logFile, err := os.Create(*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot create log %s: %v\n", *logPath, err)
		return 2
	}
	defer logFile.Close()
	stdout := io.MultiWriter(os.Stdout, logFile)
	glog.w = stdout

	// --- work directory ---------------------------------------------------
	work := *tmp
	if work == "" {
		work = `D:\GOPATH\src\C2\.audit\confwork`
	}
	work, err = filepath.Abs(work)
	if err != nil {
		glog.Printf("bad -tmp: %v", err)
		return 2
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		glog.Printf("cannot create work dir %s: %v", work, err)
		return 2
	}
	fixtures := filepath.Join(work, "fixtures")
	_ = os.RemoveAll(fixtures)

	// --- fixtures + listener ---------------------------------------------
	// The implant's envelope signing key is derived from the age private key:
	// the same derivation gives us the key to verify its replies with.
	signerPriv, signerPub := signerFromAge(*agePriv)
	clientPub := signerPub

	k, err := newPKI(fixtures, signerPub)
	if err != nil {
		glog.Printf("cannot build PKI: %v", err)
		return 2
	}
	ln, bound, err := k.listen(*addr, *port)
	if err != nil {
		glog.Printf("cannot listen on %s:%d: %v", *addr, *port, err)
		return 2
	}
	defer ln.Close()
	_, boundPort, _ := splitHostPort(bound)

	glog.Printf("== cppconformance: C implant command conformance suite ==")
	glog.Printf("implant     : %s", *implant)
	glog.Printf("listener    : %s (TLS 1.3 mTLS, MUX/1, yamux, signed envelopes)", bound)
	glog.Printf("work dir    : %s", work)
	glog.Printf("timeout/cmd : %s", *timeout)
	glog.Printf("log         : %s", *logPath)
	// Attribution: if the harness itself (same token the implant inherits) cannot
	// write the registry test key, a registry failure is an environment limit, not
	// an implant defect. Both values are recorded in the run log.
	probeErr := regCreateProbe(`Software\AppDataLow\SliverCppProbe`)
	glog.Printf("host token  : %s ; harness can write HKCU\\Software\\AppDataLow: %v",
		selfIntegrity(), probeErr == nil)
	if probeErr != nil {
		glog.Printf("              (%v)", probeErr)
	}
	glog.Printf("")

	// --- fixtures the commands operate on ---------------------------------
	if err := seedWorkDir(work); err != nil {
		glog.Printf("cannot seed work dir: %v", err)
		return 2
	}

	// --- start the implant ------------------------------------------------
	implantLogPath := filepath.Join(work, "implant.log")
	implantLog, err := os.Create(implantLogPath)
	if err != nil {
		glog.Printf("cannot create %s: %v", implantLogPath, err)
		return 2
	}
	defer implantLog.Close()

	args := []string{
		"--mtls", *addr, boundPort,
		filepath.Join(fixtures, "ca.pem"),
		filepath.Join(fixtures, "client.pem"),
		filepath.Join(fixtures, "client.key"),
		*agePriv,
		filepath.Join(fixtures, "server.pub"),
		*name,
	}
	cmd := exec.Command(*implant, args...)
	cmd.Dir = work
	cmd.Stdout = implantLog
	cmd.Stderr = implantLog
	if err := cmd.Start(); err != nil {
		glog.Printf("cannot start implant: %v", err)
		return 2
	}
	implantPID := cmd.Process.Pid
	glog.Printf("implant pid : %d (stderr -> %s)", implantPID, implantLogPath)
	killed := false
	killImplant := func() {
		if killed || *keepImplant {
			return
		}
		killed = true
		// taskkill /T also reaps the shells/tunnels the implant spawned.
		_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(implantPID)).Run()
		_ = cmd.Process.Kill()
	}
	defer killImplant()

	// --- accept the session ----------------------------------------------
	type acceptRes struct {
		conn *tls.Conn
		err  error
	}
	accCh := make(chan acceptRes, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accCh <- acceptRes{nil, err}
			return
		}
		accCh <- acceptRes{conn.(*tls.Conn), nil}
	}()

	var conn *tls.Conn
	select {
	case ar := <-accCh:
		if ar.err != nil {
			glog.Printf("accept failed: %v", ar.err)
			dumpImplantLog(implantLogPath)
			return 2
		}
		conn = ar.conn
	case <-time.After(25 * time.Second):
		glog.Printf("implant did not connect within 25s")
		dumpImplantLog(implantLogPath)
		return 2
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := conn.Handshake(); err != nil {
		glog.Printf("TLS handshake failed: %v", err)
		dumpImplantLog(implantLogPath)
		return 2
	}
	cs := conn.ConnectionState()
	glog.Printf("tls         : %s / %s / client-cert=%s", tls.VersionName(cs.Version),
		tls.CipherSuiteName(cs.CipherSuite), cs.PeerCertificates[0].Subject.CommonName)
	_ = conn.SetDeadline(time.Time{})

	// MUX/1 preface
	preface := make([]byte, 5)
	if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != "MUX/1" {
		glog.Printf("bad MUX/1 preface %q: %v", preface, err)
		dumpImplantLog(implantLogPath)
		return 2
	}
	ycfg := yamux.DefaultConfig()
	ycfg.LogOutput = io.Discard
	ycfg.KeepAliveInterval = 120 * time.Second // the C yamux answers ping ACKs; nothing needs them
	ycfg.ConnectionWriteTimeout = 30 * time.Second
	sess, err := yamux.Server(conn, ycfg)
	if err != nil {
		glog.Printf("yamux server: %v", err)
		return 2
	}
	defer sess.Close()

	peerLogf := func(string, ...interface{}) {}
	if *verbose {
		peerLogf = func(f string, a ...interface{}) { glog.Printf("[wire] "+f, a...) }
	}
	p := newPeer(sess, signerPriv, signerPub, clientPub, peerLogf)

	// --- Register ---------------------------------------------------------
	res := &result{name: "register"}
	regFrame, err := p.waitAsync(func(f *frame) bool { return f.Type == sliverpb.MsgRegister }, 20*time.Second)
	var reg sliverpb.Register
	if err != nil {
		res.recv = "无"
		res.fail("register 未到达", "envelope 未在 20s 内收到 type=%d: %v", sliverpb.MsgRegister, err)
		glog.Printf("register failed: %v", err)
		dumpImplantLog(implantLogPath)
		printTable([]*result{res})
		glog.Printf("\n0/1 通过（传输层未建立，后续命令未执行）")
		return 1
	}
	res.sent = "—"
	res.recv = fmt.Sprintf("id=0/type=%d=%s", regFrame.Type, msgName(regFrame.Type))
	res.decode = fmt.Sprintf("sig-ok=%v %dB", regFrame.SigOK, len(regFrame.Data))
	if !regFrame.SigOK {
		res.fail("", "Register 帧的 ed25519 签名无法用派生公钥验证")
	} else if derr := proto.Unmarshal(regFrame.Data, &reg); derr != nil {
		res.fail("Register 解码失败", "%v; hex=%s", derr, hexdump(regFrame.Data, 96))
	} else {
		res.assert = fmt.Sprintf("name=%q os=%q arch=%q pid=%d c2=%q",
			reg.Name, reg.Os, reg.Arch, reg.Pid, reg.ActiveC2)
		switch {
		case reg.Name != *name:
			res.fail("", "期望 Name=%q，实际 %q", *name, reg.Name)
		case reg.Os != "windows" || reg.Arch != "amd64":
			res.fail("", "期望 os=windows arch=amd64，实际 %q/%q", reg.Os, reg.Arch)
		case int(reg.Pid) != implantPID:
			res.fail(fmt.Sprintf("pid %d", reg.Pid), "Register.Pid=%d 与实际进程 %d 不一致", reg.Pid, implantPID)
		case reg.ActiveC2 == "":
			res.fail("", "ActiveC2 为空")
		default:
			res.pass("签名校验通过")
		}
	}
	glog.Printf("register    : %s", res.assert)

	ctx := &runCtx{
		p:          p,
		dir:        work,
		fixtures:   fixtures,
		timeout:    *timeout,
		implantPID: implantPID,
		implantLog: implantLogPath,
	}

	// --- command matrix ---------------------------------------------------
	specs := commandSpecs()
	filter := parseOnly(*only)
	var results []*result
	for _, sp := range specs {
		if len(filter) > 0 && !filter[sp.name] {
			continue
		}
		if p.isDead() {
			results = append(results, &result{
				name: sp.name, sent: "—", recv: "—", decode: "—",
				status: statusFail,
				note:   "会话已断，命令未执行",
				detail: []string{fmt.Sprintf("session died earlier: %v", p.deadErr)},
			})
			continue
		}
		start := time.Now()
		r := sp.run(ctx)
		if r.name == "" {
			r.name = sp.name
		}
		if r.status == "" {
			r.fail("", "命令未给出结论（内部错误）")
		}
		if r.status == statusFail && len(r.detail) == 0 {
			r.detail = append(r.detail, "未提供细节")
		}
		if r.status == statusFail && len(r.raw) > 0 {
			r.detail = append(r.detail, fmt.Sprintf("resp-raw(%dB)=%s", len(r.raw), hexdump(r.raw, 128)))
		}
		r.detail = append(r.detail, fmt.Sprintf("elapsed=%s", time.Since(start).Round(time.Millisecond)))
		results = append(results, r)
	}

	// leftovers of async traffic are evidence, not failures
	if left := p.drainAsync(); len(left) > 0 {
		var parts []string
		for _, f := range left {
			parts = append(parts, fmt.Sprintf("type=%d(%d B)", f.Type, len(f.Data)))
		}
		glog.Printf("未消费的异步帧 (%d): %s", len(left), strings.Join(parts, ", "))
	}
	if n := p.oddReplies(); n > 0 {
		glog.Printf("警告: %d 个响应 envelope 的 ID 无法匹配任何等待者", n)
	}

	// --- report -----------------------------------------------------------
	glog.Printf("")
	printTable(results)
	// Full (untruncated) notes: the table clips cells, the log must not.
	glog.Printf("")
	glog.Printf("================ 逐条备注（完整） ================")
	glog.Printf("%-18s %-6s %s", "命令", "结果", "备注")
	for _, r := range append([]*result{res}, results...) {
		glog.Printf("%-18s %-6s %s", r.name, r.status, r.note)
	}
	pass := 0
	var failures []*result
	for _, r := range results {
		if r.status == statusPass {
			pass++
		} else {
			failures = append(failures, r)
		}
	}
	total := len(results) + 1 // + register
	if res.status == statusPass {
		pass++
	} else {
		failures = append([]*result{res}, failures...)
	}
	glog.Printf("")
	glog.Printf("================ 汇总 ================")
	glog.Printf("register + %d 条命令, 通过 %d/%d", len(results), pass, total)
	if len(failures) > 0 {
		glog.Printf("失败 %d 条:", len(failures))
		for _, f := range failures {
			glog.Printf("  - %s: %s", f.name, f.note)
			for _, d := range f.detail {
				glog.Printf("      %s", d)
			}
		}
	}
	glog.Printf("implant stderr -> %s", implantLogPath)
	glog.Printf("raw log        -> %s", *logPath)

	// flush the implant log tail into the run log: crashes show up here
	dumpImplantLogTail(implantLogPath, 60)

	if len(failures) > 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func splitHostPort(addr string) (string, string, error) {
	host, port, err := netSplit(addr)
	return host, port, err
}

func netSplit(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "", nil
	}
	return addr[:i], addr[i+1:], nil
}

func parseOnly(s string) map[string]bool {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part != "" {
			out[part] = true
		}
	}
	return out
}

func dumpImplantLog(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	glog.Printf("---- implant.log ----")
	glog.Raw("%s\n", data)
	glog.Printf("---------------------")
}

func dumpImplantLogTail(path string, lines int) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return
	}
	all := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	glog.Printf("---- implant.log (tail) ----")
	for _, l := range all {
		glog.Printf("%s", strings.TrimRight(l, "\r"))
	}
	glog.Printf("----------------------------")
}

// seedWorkDir creates the fixtures the file commands operate on.
func seedWorkDir(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "confdir"), 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"cp-src.txt":   []byte("cp-source-line-1\ncp-source-line-2\n"),
		"chmod.txt":    []byte("chmod-target\n"),
		"chtimes.txt":  []byte("chtimes-target\n"),
		"grep.txt":     []byte("alpha\nbravo needle one\ncharlie\ndelta needle two\necho\n"),
		"download.bin": []byte("0123456789abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUVWXYZ-download-payload"),
		"ls-probe.txt": []byte("ls-probe\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// table rendering (CJK aware)
// ---------------------------------------------------------------------------

func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, // hangul jamo
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6:
		return 2
	}
	return 1
}

func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func pad(s string, width int) string {
	if w := dispWidth(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

func truncate(s string, max int) string {
	if dispWidth(s) <= max {
		return s
	}
	out := ""
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > max-1 {
			break
		}
		out += string(r)
		w += rw
	}
	return out + "…"
}

func printTable(rows []*result) {
	headers := []string{"命令", "发送(ids)", "收包(id/type)", "解码", "语义断言", "结果", "备注"}
	limits := []int{18, 30, 26, 34, 64, 6, 40}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		cells = append(cells, []string{
			r.name, r.sent, r.recv, r.decode, r.assert, r.status, r.note,
		})
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = dispWidth(h)
	}
	for _, row := range cells {
		for i, c := range row {
			c = truncate(c, limits[i])
			if w := dispWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	line := func(row []string) {
		var sb strings.Builder
		for i, c := range row {
			c = truncate(c, limits[i])
			sb.WriteString(pad(c, widths[i]))
			if i < len(row)-1 {
				sb.WriteString(" | ")
			}
		}
		glog.Printf("%s", strings.TrimRight(sb.String(), " "))
	}
	var sep strings.Builder
	for i := range headers {
		sep.WriteString(strings.Repeat("-", widths[i]))
		if i < len(headers)-1 {
			sep.WriteString("-+-")
		}
	}
	line(headers)
	glog.Printf("%s", sep.String())
	for _, row := range cells {
		line(row)
	}
}
