package main

// wire.go - the Sliver *server* side of the mTLS session plane, copied from the
// shapes proven by implant-cpp/tests/mtls-testserver/main.go:
//
//	TLS 1.3 mTLS (P-521 EE certs under a P-256 CA)
//	MUX/1 preface
//	hashicorp/yamux server
//	[74B minisign signature][u32 LE length][protobuf Envelope]
//
// On top of the wire format this file implements the *dispatch* contract that
// server/c2/mtls.go + server/core/connnection.go use:
//
//   - Envelope.ID != 0  -> synchronous reply, matched to the waiting request by
//     ID (the Type is recorded but never used for matching)
//   - Envelope.ID == 0  -> asynchronous push (TunnelData, TunnelClose, Ping,
//     Register); queued and classified by Type
//
// Every wait has a deadline. Nothing here blocks on a frame the implant is not
// required to send.

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/crypto/blake2b"
	"google.golang.org/protobuf/proto"

	"github.com/bishopfox/sliver/protobuf/sliverpb"
)

// ---------------------------------------------------------------------------
// signed frame (74 byte minisign header + u32 length + envelope)
// ---------------------------------------------------------------------------

const (
	frameSigLen      = 74
	frameMaxEnvelope = 8 << 20
)

// signerFromAge derives the envelope signing key the same way the implant does:
// seed = SHA256("env-signing-v1:" + agePrivateKeyString).
func signerFromAge(agePriv string) (ed25519.PrivateKey, ed25519.PublicKey) {
	seed := sha256.Sum256([]byte("env-signing-v1:" + agePriv))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv, priv.Public().(ed25519.PublicKey)
}

// minisignText renders the two line minisign public key text the implant reads
// from server.pub.
func minisignText(pub ed25519.PublicKey) string {
	digest := blake2b.Sum256(pub)
	keyID := binary.LittleEndian.Uint64(digest[:8])
	raw := make([]byte, 42)
	raw[0], raw[1] = 0x45, 0x64
	binary.LittleEndian.PutUint64(raw[2:], keyID)
	copy(raw[10:], pub)
	return fmt.Sprintf("untrusted comment: minisign public key: %016X\n%s\n",
		keyID, base64.StdEncoding.EncodeToString(raw))
}

// rawSignature builds the 74 byte minisign-compatible signature blob.
func rawSignature(priv ed25519.PrivateKey, pub ed25519.PublicKey, msg []byte) []byte {
	digest := blake2b.Sum256(pub)
	sig := make([]byte, frameSigLen)
	binary.LittleEndian.PutUint16(sig, 0x6445)
	binary.LittleEndian.PutUint64(sig[2:], binary.LittleEndian.Uint64(digest[:8]))
	copy(sig[10:], ed25519.Sign(priv, msg))
	return sig
}

func writeFrame(w io.Writer, sig, env []byte) error {
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(env)))
	if _, err := w.Write(sig); err != nil {
		return err
	}
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(env)
	return err
}

func readFrame(r io.Reader) ([]byte, []byte, error) {
	sig := make([]byte, frameSigLen)
	if _, err := io.ReadFull(r, sig); err != nil {
		return nil, nil, err
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, nil, err
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if n == 0 || n > frameMaxEnvelope {
		return nil, nil, fmt.Errorf("bad frame length %d", n)
	}
	env := make([]byte, n)
	if _, err := io.ReadFull(r, env); err != nil {
		return nil, nil, err
	}
	return sig, env, nil
}

// ---------------------------------------------------------------------------
// fixtures (CA, server + client certs, server.pub)
// ---------------------------------------------------------------------------

type fixture struct {
	certPEM, keyPEM []byte
	cert            *x509.Certificate
	key             *ecdsa.PrivateKey
}

func newCA() (*x509.Certificate, *ecdsa.PrivateKey, *fixture, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sliver-cpp conformance CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(7 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	return cert, key, &fixture{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		cert:    cert,
		key:     key,
	}, nil
}

func issue(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, usage x509.ExtKeyUsage) (*fixture, error) {
	key, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &fixture{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		cert:    cert,
		key:     key,
	}, nil
}

// pki is the single CA/cert set shared by the listener and the implant.
type pki struct {
	ca, server, client *fixture
	caCert             *x509.Certificate
}

// newPKI builds one CA, a server cert and a client cert, and writes the PEM
// files the implant is launched with (ca.pem / client.pem / client.key /
// server.pub) into dir. Server-side material stays in memory so the listener
// and the implant always agree on the same CA.
func newPKI(dir string, serverSigningPub ed25519.PublicKey) (*pki, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	caCert, caKey, ca, err := newCA()
	if err != nil {
		return nil, err
	}
	server, err := issue(caCert, caKey, "sliver-cpp conformance server", x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, err
	}
	client, err := issue(caCert, caKey, "sliver-cpp conformance client", x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"ca.pem":     ca.certPEM,
		"server.pem": server.certPEM,
		"server.key": server.keyPEM,
		"client.pem": client.certPEM,
		"client.key": client.keyPEM,
		"server.pub": []byte(minisignText(serverSigningPub)),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	return &pki{ca: ca, server: server, client: client, caCert: caCert}, nil
}

// listen starts the TLS 1.3 mTLS listener for this PKI and returns it plus the
// concrete address it bound to (so -port 0 works).
func (k *pki) listen(addr string, port int) (net.Listener, string, error) {
	pool := x509.NewCertPool()
	pool.AddCert(k.caCert)
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{k.server.cert.Raw}, PrivateKey: k.server.key}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	ln, err := tls.Listen("tcp", net.JoinHostPort(addr, fmt.Sprint(port)), cfg)
	if err != nil {
		return nil, "", err
	}
	return ln, ln.Addr().String(), nil
}

// ---------------------------------------------------------------------------
// peer: signed envelope dispatcher
// ---------------------------------------------------------------------------

// frame is one envelope the implant sent us.
type frame struct {
	ID      int64
	Type    uint32
	Data    []byte
	Unknown bool
	SigOK   bool
	Raw     []byte
	Stream  uint32
	At      time.Time
}

type peer struct {
	sess       *yamux.Session
	signerPriv ed25519.PrivateKey
	signerPub  ed25519.PublicKey
	clientPub  ed25519.PublicKey
	logf       func(string, ...interface{})

	mu      sync.Mutex
	waiters map[int64]chan *frame
	nextID  int64
	odd     int // frames whose ID matched no waiter

	aMu  sync.Mutex
	aQ   []*frame
	aSig chan struct{}

	deadOnce sync.Once
	dead     chan struct{}
	deadErr  error
}

func newPeer(sess *yamux.Session, priv ed25519.PrivateKey, pub, clientPub ed25519.PublicKey,
	logf func(string, ...interface{})) *peer {
	p := &peer{
		sess:       sess,
		signerPriv: priv,
		signerPub:  pub,
		clientPub:  clientPub,
		logf:       logf,
		waiters:    map[int64]chan *frame{},
		nextID:     0x1000,
		aSig:       make(chan struct{}, 1),
		dead:       make(chan struct{}),
	}
	go p.readLoop()
	return p
}

func (p *peer) markDead(err error) {
	p.deadOnce.Do(func() {
		p.deadErr = err
		close(p.dead)
	})
}

func (p *peer) isDead() bool {
	select {
	case <-p.dead:
		return true
	default:
		return false
	}
}

func (p *peer) readLoop() {
	for {
		st, err := p.sess.AcceptStream()
		if err != nil {
			p.markDead(err)
			return
		}
		go p.handleStream(st)
	}
}

func (p *peer) handleStream(st *yamux.Stream) {
	defer st.Close()
	_ = st.SetReadDeadline(time.Now().Add(60 * time.Second))
	sig, raw, err := readFrame(st)
	if err != nil {
		p.logf("[wire] stream %d: frame read failed: %v", st.StreamID(), err)
		return
	}
	env := &sliverpb.Envelope{}
	if err := proto.Unmarshal(raw, env); err != nil {
		p.logf("[wire] stream %d: envelope decode failed: %v (raw=%s)", st.StreamID(), err, hexdump(raw, 64))
		return
	}
	f := &frame{
		ID:      env.ID,
		Type:    env.Type,
		Data:    env.Data,
		Unknown: env.UnknownMessageType,
		SigOK:   ed25519.Verify(p.clientPub, raw, sig[10:]),
		Raw:     raw,
		Stream:  st.StreamID(),
		At:      time.Now(),
	}
	if !f.SigOK {
		p.logf("[wire] stream %d: implant response signature INVALID (id=%d type=%d)", st.StreamID(), f.ID, f.Type)
	}
	if f.ID != 0 {
		p.mu.Lock()
		ch, ok := p.waiters[f.ID]
		p.mu.Unlock()
		if !ok {
			p.mu.Lock()
			p.odd++
			p.mu.Unlock()
			p.logf("[wire] unmatched reply id=%d type=%d (%d bytes) - no waiter", f.ID, f.Type, len(f.Data))
			return
		}
		select {
		case ch <- f:
		default:
			p.logf("[wire] waiter for id=%d already satisfied (duplicate reply)", f.ID)
		}
		return
	}
	p.pushAsync(f)
}

func (p *peer) pushAsync(f *frame) {
	p.aMu.Lock()
	p.aQ = append(p.aQ, f)
	p.aMu.Unlock()
	select {
	case p.aSig <- struct{}{}:
	default:
	}
}

func (p *peer) newID() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	if p.nextID <= 0 {
		p.nextID = 0x1000
	}
	return p.nextID
}

func (p *peer) register(id int64) chan *frame {
	ch := make(chan *frame, 1)
	p.mu.Lock()
	p.waiters[id] = ch
	p.mu.Unlock()
	return ch
}

func (p *peer) unregister(id int64) {
	p.mu.Lock()
	delete(p.waiters, id)
	p.mu.Unlock()
}

// send writes one signed envelope on a fresh yamux stream (exactly what the
// real server does for every RPC).
func (p *peer) send(id int64, typ uint32, payload []byte) error {
	if p.isDead() {
		return fmt.Errorf("session closed: %v", p.deadErr)
	}
	env := &sliverpb.Envelope{ID: id, Type: typ, Data: payload}
	raw, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	st, err := p.sess.OpenStream()
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	_ = st.SetWriteDeadline(time.Now().Add(20 * time.Second))
	if err := writeFrame(st, rawSignature(p.signerPriv, p.signerPub, raw), raw); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	// The implant reads exactly one frame per stream and then closes its write
	// side; closing here would race its FIN handling, so the stream is left for
	// the session teardown (matching how few streams a real session keeps).
	return nil
}

// call sends req and waits for the reply envelope carrying the same ID.
func (p *peer) call(typ uint32, msg proto.Message, timeout time.Duration) (*frame, int64, error) {
	var payload []byte
	if msg != nil {
		var err error
		payload, err = proto.Marshal(msg)
		if err != nil {
			return nil, 0, err
		}
	}
	return p.callBytes(typ, payload, timeout)
}

func (p *peer) callBytes(typ uint32, payload []byte, timeout time.Duration) (*frame, int64, error) {
	id, err := p.begin(typ, payload)
	if err != nil {
		return nil, id, err
	}
	f, err := p.awaitReply(id, timeout)
	return f, id, err
}

// begin sends a request but does not wait for its reply; pair it with
// awaitReply. This is how the harness proves a handler is non-blocking: the
// request is in flight while other RPCs are served.
func (p *peer) begin(typ uint32, payload []byte) (int64, error) {
	id := p.newID()
	p.register(id)
	if err := p.send(id, typ, payload); err != nil {
		p.unregister(id)
		return id, err
	}
	return id, nil
}

func (p *peer) beginMsg(typ uint32, msg proto.Message) (int64, error) {
	var payload []byte
	if msg != nil {
		var err error
		payload, err = proto.Marshal(msg)
		if err != nil {
			return 0, err
		}
	}
	return p.begin(typ, payload)
}

// awaitReply waits for the reply envelope whose ID matches id.
func (p *peer) awaitReply(id int64, timeout time.Duration) (*frame, error) {
	p.mu.Lock()
	ch, ok := p.waiters[id]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no waiter registered for id=%d", id)
	}
	defer p.unregister(id)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case f := <-ch:
		return f, nil
	case <-timer.C:
		return nil, fmt.Errorf("timeout after %s waiting for reply id=%d", timeout, id)
	case <-p.dead:
		return nil, fmt.Errorf("session died while waiting for id=%d: %v", id, p.deadErr)
	}
}

// fire sends an envelope that is not expected to be answered (TunnelData,
// TunnelClose, Ping).
func (p *peer) fire(typ uint32, msg proto.Message) (int64, error) {
	return p.fireID(0, typ, msg)
}

func (p *peer) fireID(id int64, typ uint32, msg proto.Message) (int64, error) {
	var payload []byte
	if msg != nil {
		var err error
		payload, err = proto.Marshal(msg)
		if err != nil {
			return 0, err
		}
	}
	if err := p.send(id, typ, payload); err != nil {
		return id, err
	}
	return id, nil
}

// waitAsync pops the first ID==0 frame matching pred, or fails after timeout.
// Frames that do not match stay queued.
func (p *peer) waitAsync(pred func(*frame) bool, timeout time.Duration) (*frame, error) {
	deadline := time.Now().Add(timeout)
	for {
		p.aMu.Lock()
		for i, f := range p.aQ {
			if pred(f) {
				p.aQ = append(p.aQ[:i], p.aQ[i+1:]...)
				p.aMu.Unlock()
				return f, nil
			}
		}
		p.aMu.Unlock()
		remain := time.Until(deadline)
		if remain <= 0 {
			return nil, fmt.Errorf("timeout after %s waiting for async frame", timeout)
		}
		timer := time.NewTimer(remain)
		select {
		case <-p.aSig:
			timer.Stop()
		case <-timer.C:
			return nil, fmt.Errorf("timeout after %s waiting for async frame", timeout)
		case <-p.dead:
			timer.Stop()
			return nil, fmt.Errorf("session died while waiting for async frame: %v", p.deadErr)
		}
	}
}

// drainAsync empties the async queue, returning the frames for the report.
func (p *peer) drainAsync() []*frame {
	p.aMu.Lock()
	defer p.aMu.Unlock()
	out := p.aQ
	p.aQ = nil
	return out
}

func (p *peer) pendingAsync() int {
	p.aMu.Lock()
	defer p.aMu.Unlock()
	return len(p.aQ)
}

func (p *peer) oddReplies() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.odd
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func hexdump(b []byte, max int) string {
	if len(b) > max {
		b = b[:max]
	}
	var sb []byte
	for i, c := range b {
		if i > 0 && i%16 == 0 {
			sb = append(sb, '\n')
		}
		sb = append(sb, []byte(fmt.Sprintf("%02x ", c))...)
	}
	return string(sb)
}

// msgName maps the IDs the harness uses to their sliverpb constant names, for
// readable "sent/received" columns.
func msgName(t uint32) string {
	switch t {
	case sliverpb.MsgRegister:
		return "MsgRegister"
	case sliverpb.MsgPing:
		return "MsgPing"
	case sliverpb.MsgLsReq:
		return "MsgLsReq"
	case sliverpb.MsgLs:
		return "MsgLs"
	case sliverpb.MsgDownloadReq:
		return "MsgDownloadReq"
	case sliverpb.MsgDownload:
		return "MsgDownload"
	case sliverpb.MsgUploadReq:
		return "MsgUploadReq"
	case sliverpb.MsgUpload:
		return "MsgUpload"
	case sliverpb.MsgCdReq:
		return "MsgCdReq"
	case sliverpb.MsgPwdReq:
		return "MsgPwdReq"
	case sliverpb.MsgPwd:
		return "MsgPwd"
	case sliverpb.MsgRmReq:
		return "MsgRmReq"
	case sliverpb.MsgRm:
		return "MsgRm"
	case sliverpb.MsgMkdirReq:
		return "MsgMkdirReq"
	case sliverpb.MsgMkdir:
		return "MsgMkdir"
	case sliverpb.MsgPsReq:
		return "MsgPsReq"
	case sliverpb.MsgPs:
		return "MsgPs"
	case sliverpb.MsgShellReq:
		return "MsgShellReq"
	case sliverpb.MsgShell:
		return "MsgShell"
	case sliverpb.MsgTunnelData:
		return "MsgTunnelData"
	case sliverpb.MsgTunnelClose:
		return "MsgTunnelClose"
	case sliverpb.MsgRevToSelfReq:
		return "MsgRevToSelfReq"
	case sliverpb.MsgInvokeGetSystemReq:
		return "MsgInvokeGetSystemReq"
	case sliverpb.MsgGetSystem:
		return "MsgGetSystem"
	case sliverpb.MsgSpawnDllReq:
		return "MsgSpawnDllReq"
	case sliverpb.MsgSpawnDll:
		return "MsgSpawnDll"
	case sliverpb.MsgIfconfigReq:
		return "MsgIfconfigReq"
	case sliverpb.MsgIfconfig:
		return "MsgIfconfig"
	case sliverpb.MsgExecuteReq:
		return "MsgExecuteReq"
	case sliverpb.MsgTerminateReq:
		return "MsgTerminateReq"
	case sliverpb.MsgTerminate:
		return "MsgTerminate"
	case sliverpb.MsgScreenshotReq:
		return "MsgScreenshotReq"
	case sliverpb.MsgScreenshot:
		return "MsgScreenshot"
	case sliverpb.MsgNetstatReq:
		return "MsgNetstatReq"
	case sliverpb.MsgMakeTokenReq:
		return "MsgMakeTokenReq"
	case sliverpb.MsgMakeToken:
		return "MsgMakeToken"
	case sliverpb.MsgEnvReq:
		return "MsgEnvReq"
	case sliverpb.MsgEnvInfo:
		return "MsgEnvInfo"
	case sliverpb.MsgRegistryReadReq:
		return "MsgRegistryReadReq"
	case sliverpb.MsgRegistryWriteReq:
		return "MsgRegistryWriteReq"
	case sliverpb.MsgRegistryCreateKeyReq:
		return "MsgRegistryCreateKeyReq"
	case sliverpb.MsgPortfwdReq:
		return "MsgPortfwdReq"
	case sliverpb.MsgPortfwd:
		return "MsgPortfwd"
	case sliverpb.MsgGetPrivsReq:
		return "MsgGetPrivsReq"
	case sliverpb.MsgRegistrySubKeysListReq:
		return "MsgRegistrySubKeysListReq"
	case sliverpb.MsgRegistryListValuesReq:
		return "MsgRegistryListValuesReq"
	case sliverpb.MsgRegistryDeleteKeyReq:
		return "MsgRegistryDeleteKeyReq"
	case sliverpb.MsgCurrentTokenOwnerReq:
		return "MsgCurrentTokenOwnerReq"
	case sliverpb.MsgCurrentTokenOwner:
		return "MsgCurrentTokenOwner"
	case sliverpb.MsgChmodReq:
		return "MsgChmodReq"
	case sliverpb.MsgChmod:
		return "MsgChmod"
	case sliverpb.MsgChtimesReq:
		return "MsgChtimesReq"
	case sliverpb.MsgChtimes:
		return "MsgChtimes"
	case sliverpb.MsgCpReq:
		return "MsgCpReq"
	case sliverpb.MsgCp:
		return "MsgCp"
	case sliverpb.MsgMvReq:
		return "MsgMvReq"
	case sliverpb.MsgMv:
		return "MsgMv"
	case sliverpb.MsgGrepReq:
		return "MsgGrepReq"
	case sliverpb.MsgServicesReq:
		return "MsgServicesReq"
	case sliverpb.MsgServiceDetailReq:
		return "MsgServiceDetailReq"
	case sliverpb.MsgStartServiceByNameReq:
		return "MsgStartServiceByNameReq"
	case sliverpb.MsgRegistryReadHiveReq:
		return "MsgRegistryReadHiveReq"
	case sliverpb.MsgMountReq:
		return "MsgMountReq"
	case sliverpb.MsgShellResizeReq:
		return "MsgShellResizeReq"
	case sliverpb.MsgExecuteChildrenReq:
		return "MsgExecuteChildrenReq"
	}
	return fmt.Sprintf("type-%d", t)
}
