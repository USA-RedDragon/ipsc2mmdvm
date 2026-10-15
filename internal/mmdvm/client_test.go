package mmdvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/config"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

// Test protocol tag constants to avoid goconst warnings.
const (
	tagRPTL    = "RPTL"
	tagRPTCL   = "RPTCL"
	tagRPTC    = "RPTC"
	tagRPTK    = "RPTK"
	tagRPTPING = "RPTPING"
	tagDMRD    = "DMRD"

	testRuleName = "test"
	testMSTNAK   = "MSTNAK\x00\x04\xC2\x34"
)

func testMMDVMConfig() *config.MMDVM {
	return &config.MMDVM{
		Name:        "TestNet",
		ID:          311860,
		Callsign:    "N0CALL",
		RXFreq:      449000000,
		TXFreq:      444000000,
		TXPower:     50,
		ColorCode:   1,
		Latitude:    35.0,
		Longitude:   -97.0,
		Height:      30,
		Location:    "Oklahoma",
		Description: "Test Repeater",
		URL:         "https://example.com",
		Password:    "s3cret",
	}
}

// newTestClient creates an MMDVMClient with buffered channels
// so packet-building methods and handler can be tested without a real connection.
func newTestClient(t *testing.T) *MMDVMClient {
	t.Helper()
	cfg := testMMDVMConfig()
	translator, err := ipsc.NewIPSCTranslator()
	if err != nil {
		t.Fatalf("NewIPSCTranslator: %v", err)
	}
	client := &MMDVMClient{
		cfg:        cfg,
		connTX:     make(chan []byte, 16),
		connRX:     make(chan []byte, 16),
		tx_chan:    make(chan proto.Packet, 16),
		done:       make(chan struct{}),
		translator: translator,
		netLC:      ipsc.NewLCRewriter(),
		rfLC:       ipsc.NewLCRewriter(),
	}
	client.state.Store(uint32(STATE_IDLE))
	return client
}

func TestStateConstants(t *testing.T) {
	t.Parallel()
	if STATE_IDLE != 0 {
		t.Fatalf("expected STATE_IDLE=0, got %d", STATE_IDLE)
	}
	if STATE_SENT_LOGIN != 1 {
		t.Fatalf("expected STATE_SENT_LOGIN=1, got %d", STATE_SENT_LOGIN)
	}
	if STATE_SENT_AUTH != 2 {
		t.Fatalf("expected STATE_SENT_AUTH=2, got %d", STATE_SENT_AUTH)
	}
	if STATE_SENT_RPTC != 3 {
		t.Fatalf("expected STATE_SENT_RPTC=3, got %d", STATE_SENT_RPTC)
	}
	if STATE_READY != 4 {
		t.Fatalf("expected STATE_READY=4, got %d", STATE_READY)
	}
	if STATE_TIMEOUT != 5 {
		t.Fatalf("expected STATE_TIMEOUT=5, got %d", STATE_TIMEOUT)
	}
}

func TestNewMMDVMClient(t *testing.T) {
	t.Parallel()
	cfg := testMMDVMConfig()
	client := NewMMDVMClient(cfg, nil)
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.cfg != cfg {
		t.Fatal("expected config to be set")
	}
	if client.started.Load() {
		t.Fatal("expected started=false")
	}
	if client.state.Load() != uint32(STATE_IDLE) {
		t.Fatalf("expected STATE_IDLE, got %d", client.state.Load())
	}
	if client.keepAlive.Seconds() != 5 {
		t.Fatalf("expected 5s keepalive, got %v", client.keepAlive)
	}
	if client.timeout.Seconds() != 15 {
		t.Fatalf("expected 15s timeout, got %v", client.timeout)
	}
	if client.translator == nil {
		t.Fatal("expected non-nil translator")
	}
}

func TestSendLoginPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendLogin()

	data := <-client.connTX
	// Should start with "RPTL"
	if string(data[:4]) != tagRPTL {
		t.Fatalf("expected RPTL prefix, got %q", string(data[:4]))
	}
	// Should contain the binary ID (big-endian uint32)
	gotID := binary.BigEndian.Uint32(data[4:8])
	if gotID != client.cfg.ID {
		t.Fatalf("expected ID %d, got %d", client.cfg.ID, gotID)
	}
	// Total length = 4 (RPTL) + 4 (binary ID)
	if len(data) != 8 {
		t.Fatalf("expected 8 bytes, got %d", len(data))
	}
}

func TestSendRPTCLPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	data := client.rptclPacket()
	if string(data[:5]) != tagRPTCL {
		t.Fatalf("expected RPTCL prefix, got %q", string(data[:5]))
	}
	gotID := binary.BigEndian.Uint32(data[5:9])
	if gotID != client.cfg.ID {
		t.Fatalf("expected ID %d, got %d", client.cfg.ID, gotID)
	}
	if len(data) != 9 {
		t.Fatalf("expected 9 bytes, got %d", len(data))
	}
}

func TestSendRPTCPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Should start with "RPTC"
	if string(data[:4]) != tagRPTC {
		t.Fatalf("expected RPTC prefix, got %q", string(data[:4]))
	}

	// Check binary ID (big-endian uint32, 4 bytes at offset 4)
	gotID := binary.BigEndian.Uint32(data[4:8])
	if gotID != client.cfg.ID {
		t.Fatalf("expected ID %d, got %d", client.cfg.ID, gotID)
	}

	// Check callsign (8 bytes, left-justified, at offset 8)
	callsign := string(data[8:16])
	if !strings.HasPrefix(callsign, "N0CALL") {
		t.Fatalf("expected callsign starting with N0CALL, got %q", callsign)
	}

	// Check RX freq (9 bytes at offset 16)
	expectedRX := fmt.Sprintf("%09d", client.cfg.RXFreq)
	if string(data[16:25]) != expectedRX {
		t.Fatalf("expected RX freq %q, got %q", expectedRX, string(data[16:25]))
	}

	// Check TX freq (9 bytes at offset 25)
	expectedTX := fmt.Sprintf("%09d", client.cfg.TXFreq)
	if string(data[25:34]) != expectedTX {
		t.Fatalf("expected TX freq %q, got %q", expectedTX, string(data[25:34]))
	}

	// Check TX power (2 bytes at offset 34)
	expectedPower := fmt.Sprintf("%02d", client.cfg.TXPower)
	if string(data[34:36]) != expectedPower {
		t.Fatalf("expected TX power %q, got %q", expectedPower, string(data[34:36]))
	}

	// Check color code (2 bytes at offset 36)
	expectedCC := fmt.Sprintf("%02d", client.cfg.ColorCode)
	if string(data[36:38]) != expectedCC {
		t.Fatalf("expected color code %q, got %q", expectedCC, string(data[36:38]))
	}
}

func TestSendRPTKPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	random := []byte("12345678")
	client.sendRPTK(random)

	data := <-client.connTX
	// Should start with "RPTK"
	if string(data[:4]) != tagRPTK {
		t.Fatalf("expected RPTK prefix, got %q", string(data[:4]))
	}

	// Binary ID at offset 4
	gotID := binary.BigEndian.Uint32(data[4:8])
	if gotID != client.cfg.ID {
		t.Fatalf("expected ID %d, got %d", client.cfg.ID, gotID)
	}

	// Raw SHA256 token at offset 8 (32 bytes)
	token := data[8:40]

	// Verify the token is the correct sha256(random + password)
	s256 := sha256.New()
	s256.Write(random)
	s256.Write([]byte(client.cfg.Password))
	expectedToken := s256.Sum(nil)
	if !bytes.Equal(token, expectedToken) {
		t.Fatalf("expected token %x, got %x", expectedToken, token)
	}

	// Total length = 4 (RPTK) + 4 (binary ID) + 32 (SHA256)
	if len(data) != 40 {
		t.Fatalf("expected 40 bytes, got %d", len(data))
	}
}

func TestSendPingPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendPing()

	data := <-client.connTX
	if string(data[:7]) != tagRPTPING {
		t.Fatalf("expected RPTPING prefix, got %q", string(data[:7]))
	}
	gotID := binary.BigEndian.Uint32(data[7:11])
	if gotID != client.cfg.ID {
		t.Fatalf("expected ID %d, got %d", client.cfg.ID, gotID)
	}
	if len(data) != 11 {
		t.Fatalf("expected 11 bytes, got %d", len(data))
	}
}

func TestSendPacketEncodesAndSends(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.setState(STATE_READY)
	pkt := proto.Packet{
		Signature:   tagDMRD,
		Seq:         1,
		Src:         100,
		Dst:         200,
		Repeater:    3001,
		Slot:        true,
		GroupCall:   true,
		FrameType:   0,
		DTypeOrVSeq: 0,
		StreamID:    0x1234,
	}
	client.sendPacket(pkt)

	data := <-client.connTX
	if len(data) != 53 {
		t.Fatalf("expected 53 bytes, got %d", len(data))
	}
	if string(data[:4]) != tagDMRD {
		t.Fatalf("expected DMRD prefix, got %q", string(data[:4]))
	}
}

func TestSetIPSCHandler(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	if client.ipscHandler.Load() != nil {
		t.Fatal("expected nil handler initially")
	}
	called := false
	client.SetIPSCHandler(func(data []byte) {
		called = true
	})
	handler := client.ipscHandler.Load()
	if handler == nil {
		t.Fatal("expected non-nil handler after SetIPSCHandler")
	}
	(*handler)([]byte{})
	if !called {
		t.Fatal("expected handler to be called")
	}
}

func TestPacketTypeMstack(t *testing.T) {
	t.Parallel()
	if packetTypeMstack != "MSTACK" {
		t.Fatalf("expected packetTypeMstack=%q, got %q", "MSTACK", packetTypeMstack)
	}
}

func TestSendLoginBinaryIDFormat(t *testing.T) {
	t.Parallel()
	// Test with ID=1 to verify binary encoding
	cfg := testMMDVMConfig()
	cfg.ID = 1
	client := &MMDVMClient{
		cfg:    cfg,
		connTX: make(chan []byte, 16),
	}
	client.sendLogin()

	data := <-client.connTX
	gotID := binary.BigEndian.Uint32(data[4:8])
	if gotID != 1 {
		t.Fatalf("expected ID 1, got %d", gotID)
	}
}

func TestSendRPTKDifferentRandomProducesDifferentToken(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)

	client.sendRPTK([]byte("aaaaaaaa"))
	data1 := <-client.connTX
	token1 := data1[8:40]

	client.sendRPTK([]byte("bbbbbbbb"))
	data2 := <-client.connTX
	token2 := data2[8:40]

	if bytes.Equal(token1, token2) {
		t.Fatal("expected different tokens for different random data")
	}
}

// --- Helper: create a loopback UDP "server" for integration tests ---

// udpPair creates a connected pair: a "server" UDPConn that the client talks to,
// and returns the server conn plus the client (already connected).
func udpPair(t *testing.T) (*net.UDPConn, *MMDVMClient) {
	t.Helper()
	// Start a UDP listener (acts as fake MMDVM master)
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("server listen: %v", err)
	}
	srvAddr, ok := serverConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	cfg := testMMDVMConfig()
	cfg.MasterServer = fmt.Sprintf("127.0.0.1:%d", srvAddr.Port)

	client := NewMMDVMClient(cfg, nil)
	if _, err := client.connect(); err != nil {
		serverConn.Close()
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() {
		serverConn.Close()
	})

	return serverConn, client
}

// readFromServer reads one datagram from the server with a timeout.
func readFromServer(t *testing.T, serverConn *net.UDPConn, timeout time.Duration) ([]byte, *net.UDPAddr) {
	t.Helper()
	if err := serverConn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, addr, err := serverConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP: %v", err)
	}
	return buf[:n], addr
}

// --- connect() tests ---

func TestConnectSuccess(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	if client.conn == nil {
		t.Fatal("expected non-nil conn after connect")
	}
}

func TestConnectBadAddress(t *testing.T) {
	t.Parallel()
	cfg := testMMDVMConfig()
	cfg.MasterServer = "this-is-not-a-valid-address:::::999999"
	client := NewMMDVMClient(cfg, nil)

	_, err := client.connect()
	if err == nil {
		t.Fatal("expected error connecting to invalid address")
	}
}

// --- tx() tests ---

func TestTxWritesToConn(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.wg.Add(1)
	go client.tx()

	// Send data via connTX channel
	client.connTX <- []byte("HELLO_TX")

	got, _ := readFromServer(t, serverConn, time.Second)
	if string(got) != "HELLO_TX" {
		t.Fatalf("expected HELLO_TX, got %q", got)
	}

	close(client.done)
	client.wg.Wait()
}

func TestTxStopsOnDone(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.wg.Add(1)
	go client.tx()

	close(client.done)
	client.wg.Wait()
	// Should have exited cleanly
}

func TestTxHandlesClosedConn(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	// Close the connection before tx writes
	client.conn.Close()

	client.wg.Add(1)
	go client.tx()

	client.connTX <- []byte("should-fail")

	// Give tx() a moment to process the write error
	time.Sleep(50 * time.Millisecond)

	close(client.done)
	client.wg.Wait()
}

// --- rx() tests ---

func TestRxReceivesData(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.started.Store(true)
	client.wg.Add(1)
	go client.rx(client.conn, client.connStop)

	// Send a probe from the client so the server knows its address
	client.connMu.Lock()
	_, err := client.conn.Write([]byte("PROBE"))
	client.connMu.Unlock()
	if err != nil {
		t.Fatalf("probe write: %v", err)
	}
	_, clientAddr := readFromServer(t, serverConn, time.Second)

	// Now server sends data back
	_, err = serverConn.WriteToUDP([]byte("FROM_SERVER"), clientAddr)
	if err != nil {
		t.Fatalf("server write: %v", err)
	}

	select {
	case data := <-client.connRX:
		if string(data) != "FROM_SERVER" {
			t.Fatalf("expected FROM_SERVER, got %q", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rx data")
	}

	client.started.Store(false)
	client.conn.Close()
	client.wg.Wait()
}

func TestRxStopsOnConnClose(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.started.Store(false) // simulate not started → will exit on read error
	client.wg.Add(1)
	go client.rx(client.conn, client.connStop)

	client.conn.Close()
	client.wg.Wait()
}

// --- handler() state machine tests ---

func TestHandlerIdleState(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_IDLE))

	client.wg.Add(1)
	go client.handler()

	// Send data while in IDLE — should just log, not crash
	client.connRX <- []byte("MSTACK__random12")

	// Give handler time to process
	time.Sleep(50 * time.Millisecond)

	close(client.done)
	client.wg.Wait()
}

func TestHandlerSentLoginMSTACK(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_SENT_LOGIN))

	client.wg.Add(1)
	go client.handler()

	// Simulate RPTACK response with random suffix (last 4 bytes used)
	resp := append([]byte("RPTACK"), []byte("12345678")...)
	client.connRX <- resp

	// Should transition to SENT_AUTH and send RPTK
	select {
	case data := <-client.connTX:
		if string(data[:4]) != tagRPTK {
			t.Fatalf("expected RPTK, got %q", string(data[:4]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RPTK")
	}

	//nolint:gosec // G115: test-only, state values fit in uint8
	if state(client.state.Load()) != STATE_SENT_AUTH {
		t.Fatalf("expected STATE_SENT_AUTH, got %d", client.state.Load())
	}

	close(client.done)
	client.wg.Wait()
}

func TestHandlerSentLoginRejected(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.keepAlive = time.Minute
	client.timeout = time.Hour
	client.setState(STATE_SENT_LOGIN)

	client.handleState([]byte("RPTNAK__________"))
	if len(client.connTX) != 0 {
		t.Fatal("expected no immediate resend after rejection")
	}

	client.tick(time.Now().Add(2 * time.Minute))
	data := <-client.connTX
	if string(data[:4]) != tagRPTL {
		t.Fatalf("expected RPTL retry, got %q", string(data[:4]))
	}
}

func TestHandlerSentAuthMSTACK(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_SENT_AUTH))

	client.wg.Add(1)
	go client.handler()

	// RPTACK means auth accepted
	client.connRX <- []byte("RPTACK__________")

	// Should send RPTC
	select {
	case data := <-client.connTX:
		if string(data[:4]) != tagRPTC {
			t.Fatalf("expected RPTC, got %q", string(data[:4]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RPTC")
	}

	//nolint:gosec // G115: test-only, state values fit in uint8
	if state(client.state.Load()) != STATE_SENT_RPTC {
		t.Fatalf("expected STATE_SENT_RPTC, got %d", client.state.Load())
	}

	close(client.done)
	client.wg.Wait()
}

func TestHandlerSentAuthMSTNAK(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.metrics = metrics.NewMetrics()
	client.keepAlive = time.Minute
	client.timeout = time.Hour
	client.setState(STATE_SENT_AUTH)

	client.handleState([]byte(testMSTNAK))
	if st := client.currentState(); st != STATE_SENT_LOGIN {
		t.Fatalf("expected STATE_SENT_LOGIN, got %d", st)
	}
	rec := httptest.NewRecorder()
	client.metrics.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if want := `mmdvm_auth_failures_total{network="TestNet"} 1`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("expected %q in metrics output", want)
	}

	client.tick(time.Now().Add(2 * time.Minute))
	data := <-client.connTX
	if string(data[:4]) != tagRPTL {
		t.Fatalf("expected RPTL retry, got %q", string(data[:4]))
	}
}

func TestHandlerSentRPTCAccepted(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_SENT_RPTC))
	// Need a connection for ping to work (it sends via connTX)
	// Use a real loopback
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer serverConn.Close()

	cfg := testMMDVMConfig()
	srvAddr, ok := serverConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	cfg.MasterServer = fmt.Sprintf("127.0.0.1:%d", srvAddr.Port)
	client.cfg = cfg

	// Give it a real conn for ping
	if _, err := client.connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Override keepAlive/timeout for faster test
	client.keepAlive = 100 * time.Millisecond
	client.timeout = 5 * time.Second

	client.wg.Add(1)
	go client.handler()

	// RPTACK means config accepted
	client.connRX <- []byte("RPTACK__________")

	// Should transition to STATE_READY and start ping (which sends RPTPING)
	select {
	case data := <-client.connTX:
		if string(data[:7]) != tagRPTPING {
			t.Fatalf("expected RPTPING from ping(), got %q", string(data[:min(7, len(data))]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MSTPING")
	}

	//nolint:gosec // G115: test-only, state values fit in uint8
	if state(client.state.Load()) != STATE_READY {
		t.Fatalf("expected STATE_READY, got %d", client.state.Load())
	}

	close(client.done)
	client.wg.Wait()
}

func TestHandlerSentRPTCRejected(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.keepAlive = time.Minute
	client.timeout = time.Hour
	client.setState(STATE_SENT_RPTC)

	client.handleState([]byte(testMSTNAK))
	if st := client.currentState(); st != STATE_SENT_LOGIN {
		t.Fatalf("expected STATE_SENT_LOGIN after config rejection, got %d", st)
	}

	client.tick(time.Now().Add(2 * time.Minute))
	data := <-client.connTX
	if string(data[:4]) != tagRPTL {
		t.Fatalf("expected RPTL after config rejection, got %q", string(data[:4]))
	}
}

func TestHandlerReadyMSTNAKLogsInAgain(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.setState(STATE_READY)

	client.handleState([]byte(testMSTNAK))
	if st := client.currentState(); st != STATE_SENT_LOGIN {
		t.Fatalf("expected STATE_SENT_LOGIN, got %d", st)
	}
	select {
	case data := <-client.connTX:
		if string(data[:4]) != tagRPTL {
			t.Fatalf("expected RPTL, got %q", string(data[:4]))
		}
	default:
		t.Fatal("expected an immediate RPTL")
	}
}

func TestHandlerMSTCLReconnects(t *testing.T) {
	t.Parallel()
	for _, st := range []state{STATE_READY, STATE_SENT_AUTH} {
		serverConn, client := udpPair(t)
		client.setState(st)
		oldConn := client.conn

		client.handleState(append([]byte(packetTypeMstcl), 0, 4, 0xC2, 0x34))

		if got := client.currentState(); got != STATE_SENT_LOGIN {
			t.Fatalf("state %d: expected STATE_SENT_LOGIN, got %d", st, got)
		}
		if client.conn == oldConn {
			t.Fatalf("state %d: expected a new connection", st)
		}
		data := <-client.connTX
		if string(data[:4]) != tagRPTL {
			t.Fatalf("state %d: expected RPTL, got %q", st, string(data[:4]))
		}
		client.Stop()
		serverConn.Close()
	}
}

func TestRxHandlesPongWithoutHandler(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	client.metrics = metrics.NewMetrics()
	client.lastPingSent.Store(time.Now().UnixNano())
	client.wg.Add(1)
	go client.rx(client.conn, client.connStop)

	if _, err := client.conn.Write([]byte("PROBE")); err != nil {
		t.Fatalf("probe write: %v", err)
	}
	_, clientAddr := readFromServer(t, serverConn, time.Second)
	before := time.Now().UnixNano()
	if _, err := serverConn.WriteToUDP([]byte("MSTPONG\x00\x04\xC2\x34"), clientAddr); err != nil {
		t.Fatalf("server write: %v", err)
	}

	waitFor(t, time.Second, "lastHeard update", func() bool { return client.lastHeard.Load() >= before })
	if len(client.connRX) != 0 {
		t.Fatal("expected MSTPONG to be handled in rx, not queued for the handler")
	}
	client.Stop()
}

func TestHandlerReadyRPTSBKN(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_READY))

	client.wg.Add(1)
	go client.handler()

	// Should just log, not crash
	client.connRX <- []byte("RPTSBKN_________")

	time.Sleep(50 * time.Millisecond)

	close(client.done)
	client.wg.Wait()
}

func TestHandlerReadyDMRDPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_READY))

	// Add a passthrough net rewrite rule so inbound packets aren't dropped.
	client.netRewrites = []rewrite.Rule{
		&rewrite.TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 1, ToSlot: 1, ToTG: 1, Range: 999999},
	}

	var receivedPackets [][]byte
	var mu sync.Mutex
	client.SetIPSCHandler(func(data []byte) {
		mu.Lock()
		receivedPackets = append(receivedPackets, data)
		mu.Unlock()
	})

	client.wg.Add(1)
	go client.handler()

	// Build a valid 53-byte DMRD packet
	pkt := proto.Packet{
		Signature:   tagDMRD,
		Seq:         0,
		Src:         100,
		Dst:         200,
		Repeater:    3001,
		Slot:        false,
		GroupCall:   true,
		FrameType:   2, // data sync
		DTypeOrVSeq: 1, // voice LC header
		StreamID:    0x5555,
	}
	encoded := pkt.Encode()
	client.connRX <- encoded

	waitFor(t, 2*time.Second, "IPSC packets", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(receivedPackets) >= 3
	})

	mu.Lock()
	count := len(receivedPackets)
	mu.Unlock()

	// Voice LC Header produces 3 IPSC packets
	if count != 3 {
		t.Fatalf("expected 3 IPSC packets from voice header, got %d", count)
	}

	close(client.done)
	client.wg.Wait()
}

func TestHandlerReadyDMRDNoHandler(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_READY))
	// No IPSC handler set

	// Add a passthrough net rewrite rule so the packet passes the filter.
	client.netRewrites = []rewrite.Rule{
		&rewrite.TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 1, ToSlot: 1, ToTG: 1, Range: 999999},
	}

	client.wg.Add(1)
	go client.handler()

	pkt := proto.Packet{
		Signature:   tagDMRD,
		Seq:         0,
		Src:         100,
		Dst:         200,
		Repeater:    3001,
		GroupCall:   true,
		FrameType:   2,
		DTypeOrVSeq: 1,
		StreamID:    0x6666,
	}
	client.connRX <- pkt.Encode()

	// Should not panic
	time.Sleep(50 * time.Millisecond)

	close(client.done)
	client.wg.Wait()
}

func TestHandlerReadyDMRDInvalidPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_READY))

	client.wg.Add(1)
	go client.handler()

	// Send "DMRD" prefix but too short to decode
	client.connRX <- []byte("DMRD_short")

	// Should log error, not crash
	time.Sleep(50 * time.Millisecond)

	close(client.done)
	client.wg.Wait()
}

func TestHandlerReadyUnknownPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_READY))

	client.wg.Add(1)
	go client.handler()

	client.connRX <- []byte("UNKN_some_data__")

	// Should just log, not crash
	time.Sleep(50 * time.Millisecond)

	close(client.done)
	client.wg.Wait()
}

func TestHandlerTimeoutState(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_TIMEOUT))

	client.wg.Add(1)
	go client.handler()

	client.connRX <- []byte("MSTACK__________")

	// Should just log, not crash or transition
	time.Sleep(50 * time.Millisecond)

	//nolint:gosec // G115: test-only, state values fit in uint8
	if state(client.state.Load()) != STATE_TIMEOUT {
		t.Fatalf("expected state to remain TIMEOUT, got %d", client.state.Load())
	}

	close(client.done)
	client.wg.Wait()
}

// --- supervise() tests ---

func TestTickSendsPingWhenReady(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.timeout = 5 * time.Second
	client.setState(STATE_READY)
	client.lastHeard.Store(time.Now().UnixNano())

	client.tick(time.Now())
	data := <-client.connTX
	if string(data[:7]) != tagRPTPING {
		t.Fatalf("expected RPTPING, got %q", string(data[:min(7, len(data))]))
	}
}

func TestTickResendsHandshakeSteps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		st  state
		tag string
	}{
		{STATE_SENT_LOGIN, tagRPTL},
		{STATE_SENT_AUTH, tagRPTK},
		{STATE_SENT_RPTC, tagRPTC},
	} {
		client := newTestClient(t)
		client.timeout = 5 * time.Second
		client.salt = []byte{1, 2, 3, 4}
		client.setState(tc.st)
		client.tick(time.Now())
		data := <-client.connTX
		if string(data[:4]) != tc.tag {
			t.Fatalf("state %d: expected %s, got %q", tc.st, tc.tag, string(data[:4]))
		}
	}
}

func TestSuperviseStopsOnDone(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.keepAlive = 10 * time.Millisecond
	client.wg.Add(1)
	go client.supervise()
	close(client.done)
	client.wg.Wait()
}

func TestTickReconnectsOnPingTimeout(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()
	client.timeout = 10 * time.Millisecond
	client.setState(STATE_READY)
	client.lastHeard.Store(time.Now().Add(-time.Minute).UnixNano())
	oldConn := client.conn

	client.tick(time.Now())

	data := <-client.connTX
	if string(data[:4]) != tagRPTL {
		t.Fatalf("expected RPTL after timeout, got %q", string(data[:min(4, len(data))]))
	}
	if st := client.currentState(); st != STATE_SENT_LOGIN {
		t.Fatalf("expected STATE_SENT_LOGIN after timeout, got %d", st)
	}
	if client.conn == oldConn {
		t.Fatal("expected a new connection")
	}
	client.Stop()
}

func TestTickReconnectsOnHandshakeTimeout(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()
	client.timeout = 10 * time.Millisecond
	client.setState(STATE_SENT_AUTH)

	client.tick(time.Now().Add(time.Second))

	data := <-client.connTX
	if string(data[:4]) != tagRPTL {
		t.Fatalf("expected RPTL after handshake timeout, got %q", string(data[:min(4, len(data))]))
	}
	client.Stop()
}

// --- forwardTX() tests ---

func TestForwardTXSendsPackets(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.setState(STATE_READY)

	client.wg.Add(1)
	go client.forwardTX()

	pkt := proto.Packet{
		Signature: tagDMRD,
		Seq:       5,
		Src:       100,
		Dst:       200,
		Repeater:  3001,
		GroupCall: true,
		StreamID:  0x7777,
	}
	client.tx_chan <- pkt

	select {
	case data := <-client.connTX:
		if len(data) != 53 {
			t.Fatalf("expected 53 bytes, got %d", len(data))
		}
		if string(data[:4]) != tagDMRD {
			t.Fatalf("expected DMRD, got %q", string(data[:4]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forwarded packet")
	}

	close(client.done)
	client.wg.Wait()
}

func TestForwardTXStopsOnDone(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)

	client.wg.Add(1)
	go client.forwardTX()

	close(client.done)
	client.wg.Wait()
}

// --- HandleIPSCBurst tests ---

func TestHandleIPSCBurstWhenNotStarted(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.started.Store(false)

	// Should return false immediately without sending
	if client.HandleIPSCBurst(0x80, make([]byte, 54), &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}) {
		t.Fatal("expected false when not started")
	}

	// tx_chan should be empty
	select {
	case <-client.tx_chan:
		t.Fatal("expected no packet when not started")
	default:
	}
}

func TestHandleIPSCBurstTranslatesAndSends(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.setState(STATE_READY)
	client.started.Store(true)
	if client.translator != nil {
		client.translator.SetPeerID(client.cfg.ID)
	}

	// Add a passthrough RF rewrite rule so the packet isn't dropped.
	// The IPSC packet below is a group call to dst 200 on slot 1.
	client.rfRewrites = []rewrite.Rule{
		&rewrite.TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 1, ToSlot: 1, ToTG: 1, Range: 999999},
	}

	// Build an IPSC voice header packet
	data := make([]byte, 54)
	data[0] = 0x80 // group voice
	// Peer ID
	data[1] = 0x00
	data[2] = 0x00
	data[3] = 0x00
	data[4] = 0x01
	// Src
	data[6] = 0x00
	data[7] = 0x00
	data[8] = 0x64 // 100
	// Dst
	data[9] = 0x00
	data[10] = 0x00
	data[11] = 0xC8 // 200
	// Call type
	data[12] = 0x02 // group
	// Call control
	data[13] = 0x00
	data[14] = 0x00
	data[15] = 0xAA
	data[16] = 0xBB
	// RTP header
	data[18] = 0x80
	// Burst type = voice head
	data[30] = 0x01

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	client.HandleIPSCBurst(0x80, data, addr)

	// Should produce at least 1 MMDVM packet on tx_chan
	select {
	case pkt := <-client.tx_chan:
		if pkt.Signature != tagDMRD {
			t.Fatalf("expected DMRD, got %q", pkt.Signature)
		}
		if pkt.Src != 100 {
			t.Fatalf("expected src 100, got %d", pkt.Src)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for translated packet")
	}
}

func TestHandleIPSCBurstStopsOnDone(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.setState(STATE_READY)
	client.started.Store(true)
	if client.translator != nil {
		client.translator.SetPeerID(client.cfg.ID)
	}

	// Add a passthrough RF rewrite rule so the packet isn't dropped.
	client.rfRewrites = []rewrite.Rule{
		&rewrite.TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 1, ToSlot: 1, ToTG: 1, Range: 999999},
	}

	// Close done channel before handling burst
	close(client.done)

	// Build a valid voice header
	data := make([]byte, 54)
	data[0] = 0x80
	data[1] = 0x00
	data[2] = 0x00
	data[3] = 0x00
	data[4] = 0x01
	data[6] = 0x00
	data[7] = 0x00
	data[8] = 0x64
	data[9] = 0x00
	data[10] = 0x00
	data[11] = 0xC8
	data[12] = 0x02
	data[13] = 0x00
	data[14] = 0x00
	data[15] = 0xCC
	data[16] = 0xDD
	data[18] = 0x80
	data[30] = 0x01

	// Fill tx_chan to make it block
	for i := 0; i < cap(client.tx_chan); i++ {
		client.tx_chan <- proto.Packet{}
	}

	// Should not hang — done channel will prevent blocking send
	done := make(chan struct{})
	go func() {
		client.HandleIPSCBurst(0x80, data, &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleIPSCBurst should not hang when done is closed")
	}
}

func TestHandleIPSCBurstUnsupportedType(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.started.Store(true)

	// Unsupported type (0x99) — should return false
	if client.HandleIPSCBurst(0x99, make([]byte, 54), &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}) {
		t.Fatal("expected false for unsupported type")
	}

	// Should produce no packets
	select {
	case <-client.tx_chan:
		t.Fatal("expected no packet for unsupported type")
	default:
	}
}

// --- Stop() tests ---

func TestStopIdempotent(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.started.Store(true)

	// Multiple stops should not panic
	client.Stop()
	client.Stop()
	client.Stop()
}

func TestStopSendsRPTCL(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.started.Store(true)

	client.Stop()

	// Read what was sent to the server
	got, _ := readFromServer(t, serverConn, time.Second)
	if string(got[:5]) != tagRPTCL {
		t.Fatalf("expected RPTCL disconnect, got %q", string(got[:min(5, len(got))]))
	}
}

func TestStopWithNilConn(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.conn = nil

	// Should not panic
	client.Stop()
}

func TestStopSetsStartedFalse(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.started.Store(true)
	client.Stop()

	if client.started.Load() {
		t.Fatal("expected started=false after Stop")
	}
}

// --- sendRPTCLDirect test ---

func TestSendRPTCLDirect(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.connMu.Lock()
	client.sendRPTCLDirect()
	client.connMu.Unlock()

	got, _ := readFromServer(t, serverConn, time.Second)
	if string(got[:5]) != tagRPTCL {
		t.Fatalf("expected RPTCL, got %q", string(got[:min(5, len(got))]))
	}
	if len(got) != 9 {
		t.Fatalf("expected 9 bytes, got %d", len(got))
	}
	if id := binary.BigEndian.Uint32(got[5:9]); id != client.cfg.ID {
		t.Fatalf("expected binary peer ID %d, got %d", client.cfg.ID, id)
	}
}

// --- Full integration: Start → login handshake → ready ---

func TestStartAndFullHandshake(t *testing.T) {
	t.Parallel()
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("server listen: %v", err)
	}
	defer serverConn.Close()
	srvAddr, ok := serverConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	cfg := testMMDVMConfig()
	cfg.MasterServer = fmt.Sprintf("127.0.0.1:%d", srvAddr.Port)

	client := NewMMDVMClient(cfg, nil)
	client.keepAlive = 200 * time.Millisecond
	client.timeout = 5 * time.Second

	if _, err := client.connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	client.started.Store(true)

	client.wg.Add(4)
	go client.handler()
	go client.rx(client.conn, client.connStop)
	go client.tx()
	go client.forwardTX()

	// Step 1: Client sends RPTL
	client.sendLogin()
	client.state.Store(uint32(STATE_SENT_LOGIN))

	// Read RPTL from server
	loginData, clientAddr := readFromServer(t, serverConn, 2*time.Second)
	if string(loginData[:4]) != tagRPTL {
		t.Fatalf("expected RPTL, got %q", string(loginData[:4]))
	}

	// Step 2: Server sends RPTACK with random
	rptack := append([]byte("RPTACK"), []byte("ABCDEFGH")...)
	if _, err := serverConn.WriteToUDP(rptack, clientAddr); err != nil {
		t.Fatalf("server write RPTACK: %v", err)
	}

	// Step 3: Client sends RPTK
	rptkData, _ := readFromServer(t, serverConn, 2*time.Second)
	if string(rptkData[:4]) != tagRPTK {
		t.Fatalf("expected RPTK, got %q", string(rptkData[:4]))
	}

	// Step 4: Server sends RPTACK (auth accepted)
	if _, err := serverConn.WriteToUDP([]byte("RPTACK__________"), clientAddr); err != nil {
		t.Fatalf("server write RPTACK: %v", err)
	}

	// Step 5: Client sends RPTC
	rptcData, _ := readFromServer(t, serverConn, 2*time.Second)
	if string(rptcData[:4]) != tagRPTC {
		t.Fatalf("expected RPTC, got %q", string(rptcData[:4]))
	}

	// Step 6: Server sends RPTACK (config accepted)
	if _, err := serverConn.WriteToUDP([]byte("RPTACK__________"), clientAddr); err != nil {
		t.Fatalf("server write RPTACK: %v", err)
	}

	// Step 7: Client should now be in READY state and send RPTPING
	pingData, _ := readFromServer(t, serverConn, 2*time.Second)
	if string(pingData[:7]) != tagRPTPING {
		t.Fatalf("expected RPTPING, got %q", string(pingData[:min(7, len(pingData))]))
	}

	// Wait for state to reach READY
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		//nolint:gosec // G115: test-only, state values fit in uint8
		if state(client.state.Load()) == STATE_READY {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	//nolint:gosec // G115: test-only, state values fit in uint8
	if state(client.state.Load()) != STATE_READY {
		t.Fatalf("expected STATE_READY, got %d", client.state.Load())
	}

	// Step 8: Server sends MSTPONG
	if _, err := serverConn.WriteToUDP([]byte("MSTPONG_________"), clientAddr); err != nil {
		t.Fatalf("server write MSTPONG: %v", err)
	}

	// Give handler time to process
	time.Sleep(100 * time.Millisecond)

	if client.lastHeard.Load() == 0 {
		t.Fatal("expected lastHeard to be set after MSTPONG")
	}

	client.Stop()
}

// --- RPTC field encoding tests ---

func TestSendRPTCLatLong(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Latitude (8 bytes) starts at offset 38
	lat := string(data[38:46])
	if lat != "+35.0000" {
		t.Fatalf("expected latitude '+35.0000', got %q", lat)
	}

	// Longitude (9 bytes) starts at offset 46
	lon := string(data[46:55])
	if lon != "-097.0000" {
		t.Fatalf("expected longitude '-097.0000', got %q", lon)
	}
}

func TestSendRPTCHeight(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Height (3 bytes) at offset 55
	height := string(data[55:58])
	if height != "030" {
		t.Fatalf("expected height 030, got %q", height)
	}
}

func TestSendRPTCLocation(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Location (20 bytes) at offset 58
	location := strings.TrimRight(string(data[58:78]), " ")
	if location != "Oklahoma" {
		t.Fatalf("expected location 'Oklahoma', got %q", location)
	}
}

func TestSendRPTCDescription(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Description (19 bytes) at offset 78
	desc := strings.TrimRight(string(data[78:97]), " ")
	if desc != "Test Repeater" {
		t.Fatalf("expected description 'Test Repeater', got %q", desc)
	}
}

func TestSendRPTCURL(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// URL (124 bytes) at offset 98
	url := strings.TrimRight(string(data[98:222]), " ")
	if url != "https://example.com" {
		t.Fatalf("expected URL 'https://example.com', got %q", url)
	}
}

// --- Verify sendRPTK token correctness with specific random ---

func TestSendRPTKTokenVerification(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	random := []byte("ABCDEFGH")
	client.sendRPTK(random)

	data := <-client.connTX
	token := data[8:40]

	s256 := sha256.New()
	s256.Write(random)
	s256.Write([]byte(client.cfg.Password))
	expected := s256.Sum(nil)

	if !bytes.Equal(token, expected) {
		t.Fatalf("token mismatch:\n  got:  %x\n  want: %x", token, expected)
	}
}

// --- Verify sendPacket encodes correctly ---

func TestSendPacketFieldsEncoded(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.setState(STATE_READY)
	pkt := proto.Packet{
		Signature:   tagDMRD,
		Seq:         42,
		Src:         0x123456,
		Dst:         0xABCDEF,
		Repeater:    0xDEADBEEF,
		Slot:        true,
		GroupCall:   false,
		FrameType:   2,
		DTypeOrVSeq: 3,
		StreamID:    0x12345678,
	}
	client.sendPacket(pkt)

	data := <-client.connTX

	// Decode back and verify
	decoded, ok := proto.Decode(data)
	if !ok {
		t.Fatal("failed to decode sent packet")
	}
	if !pkt.Equal(decoded) {
		t.Fatalf("packet mismatch:\n  sent: %+v\n  got:  %+v", pkt, decoded)
	}
}

// --- Verify multiple HandleIPSCBurst calls ---

func TestHandleIPSCBurstMultiple(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.setState(STATE_READY)
	client.started.Store(true)
	if client.translator != nil {
		client.translator.SetPeerID(client.cfg.ID)
	}

	// Add a passthrough RF rewrite rule so packets aren't dropped.
	client.rfRewrites = []rewrite.Rule{
		&rewrite.TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 1, ToSlot: 1, ToTG: 1, Range: 999999},
	}

	// Send multiple voice headers with different call controls
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	for i := 0; i < 3; i++ {
		data := make([]byte, 54)
		data[0] = 0x80
		data[1] = 0x00
		data[2] = 0x00
		data[3] = 0x00
		data[4] = byte(i + 1)
		data[6] = 0x00
		data[7] = 0x00
		data[8] = 0x64
		data[9] = 0x00
		data[10] = 0x00
		data[11] = 0xC8
		data[12] = 0x02
		data[13] = byte(i + 1) // different call control per iteration
		data[14] = 0x00
		data[15] = 0x00
		data[16] = byte(i + 1)
		data[18] = 0x80
		data[30] = 0x01

		client.HandleIPSCBurst(0x80, data, addr)
	}

	// Drain all packets
	var count int
	timeout := time.After(time.Second)
loop:
	for {
		select {
		case <-client.tx_chan:
			count++
		case <-timeout:
			break loop
		default:
			if count > 0 {
				break loop
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if count < 3 {
		t.Fatalf("expected at least 3 translated packets, got %d", count)
	}
}

// --- State transitions in numeric order ---

func TestStateTransitionOrder(t *testing.T) {
	t.Parallel()
	// Verify the numeric order of states
	states := []state{STATE_IDLE, STATE_SENT_LOGIN, STATE_SENT_AUTH, STATE_SENT_RPTC, STATE_READY, STATE_TIMEOUT}
	for i := 0; i < len(states)-1; i++ {
		if states[i] >= states[i+1] {
			t.Fatalf("state %d should be less than state %d", states[i], states[i+1])
		}
	}
}

// --- Verify atomic started flag ---

func TestStartedFlag(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)

	if client.started.Load() {
		t.Fatal("expected started=false initially")
	}

	client.started.Store(true)
	if !client.started.Load() {
		t.Fatal("expected started=true after store")
	}

	client.started.Store(false)
	if client.started.Load() {
		t.Fatal("expected started=false after reset")
	}
}

func TestSendRPTCOversizedFieldsKeepFixedLayout(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.cfg.Callsign = "KD9ABC-RPT"
	client.cfg.RXFreq = 4400000000
	client.cfg.TXFreq = 1234567890
	client.cfg.TXPower = 200
	client.cfg.ColorCode = 255
	client.cfg.Latitude = 123.4
	client.cfg.Longitude = -500
	client.cfg.Height = 5000
	client.cfg.Location = strings.Repeat("L", 30)
	client.cfg.Description = strings.Repeat("D", 30)
	client.cfg.Slots = 0
	client.cfg.URL = strings.Repeat("u", 200)

	client.sendRPTC()
	data := <-client.connTX

	if len(data) != 302 {
		t.Fatalf("expected 302 bytes, got %d", len(data))
	}
	want := []struct {
		from, to int
		val      string
	}{
		{0, 4, tagRPTC},
		{8, 16, "KD9ABC-R"},
		{16, 25, "999999999"},
		{25, 34, "999999999"},
		{34, 36, "99"},
		{36, 38, "99"},
		{38, 46, "+90.0000"},
		{46, 55, "-180.0000"},
		{55, 58, "999"},
		{58, 78, strings.Repeat("L", 20)},
		{78, 97, strings.Repeat("D", 19)},
		{97, 98, "3"},
		{98, 222, strings.Repeat("u", 124)},
		{222, 262, fmt.Sprintf("%-40s", "20210921")},
		{262, 302, fmt.Sprintf("%-40s", "MMDVM_MMDVM_HS_Dual_Hat")},
	}
	for _, w := range want {
		if got := string(data[w.from:w.to]); got != w.val {
			t.Errorf("bytes %d:%d = %q, want %q", w.from, w.to, got, w.val)
		}
	}
	if id := binary.BigEndian.Uint32(data[4:8]); id != client.cfg.ID {
		t.Errorf("expected ID %d, got %d", client.cfg.ID, id)
	}
}

func TestSendRPTCDefaultLength(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()
	if data := <-client.connTX; len(data) != 302 {
		t.Fatalf("expected 302 bytes, got %d", len(data))
	}
}

// fakeMaster is a minimal homebrew master on loopback for lifecycle tests.
type fakeMaster struct {
	conn         *net.UDPConn
	answer       atomic.Bool
	dropLogins   atomic.Int32
	logins       atomic.Int32
	pings        atomic.Int32
	peer         atomic.Pointer[net.UDPAddr]
	pongsEnabled atomic.Bool
}

func newFakeMaster(t *testing.T) *fakeMaster {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := &fakeMaster{conn: conn}
	m.answer.Store(true)
	m.pongsEnabled.Store(true)
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			m.peer.Store(addr)
			data := buf[:n]
			var reply []byte
			switch {
			case bytes.HasPrefix(data, []byte(tagRPTPING)):
				m.pings.Add(1)
				if m.pongsEnabled.Load() {
					reply = []byte("MSTPONG\x00\x00\x00\x01")
				}
			case bytes.HasPrefix(data, []byte(tagRPTCL)):
			case bytes.HasPrefix(data, []byte(tagRPTL)):
				m.logins.Add(1)
				if m.dropLogins.Load() > 0 {
					m.dropLogins.Add(-1)
					continue
				}
				reply = []byte("RPTACK\x01\x02\x03\x04")
			case bytes.HasPrefix(data, []byte(tagRPTK)), bytes.HasPrefix(data, []byte(tagRPTC)):
				reply = []byte("RPTACK\x00\x04\xC2\x34")
			}
			if reply != nil && m.answer.Load() {
				_, _ = conn.WriteToUDP(reply, addr)
			}
		}
	}()
	return m
}

func (m *fakeMaster) addr() string {
	return m.conn.LocalAddr().String()
}

func (m *fakeMaster) send(t *testing.T, data []byte) {
	t.Helper()
	addr := m.peer.Load()
	if addr == nil {
		t.Fatal("fake master has no peer yet")
	}
	if _, err := m.conn.WriteToUDP(data, addr); err != nil {
		t.Fatalf("fake master write: %v", err)
	}
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func isReady(c *MMDVMClient) func() bool {
	return func() bool { return c.currentState() == STATE_READY }
}

func TestSetIPSCHandlerAfterStartIsRaceFree(t *testing.T) {
	t.Parallel()
	m := newFakeMaster(t)
	cfg := testMMDVMConfig()
	cfg.MasterServer = m.addr()
	cfg.PassAllTG = []int{1, 2}
	c := NewMMDVMClient(cfg, nil)
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Stop)
	waitFor(t, 2*time.Second, "READY", isReady(c))

	got := make(chan struct{}, 1)
	c.SetIPSCHandler(func([]byte) {
		select {
		case got <- struct{}{}:
		default:
		}
	})
	pkt := proto.Packet{Signature: tagDMRD, Dst: 91, GroupCall: true, FrameType: 2, DTypeOrVSeq: 1, StreamID: 9}
	m.send(t, pkt.Encode())
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("IPSC handler was not called")
	}
}

func TestStopReturnsWhileSendBlockedOnFullQueue(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	for len(client.connTX) < cap(client.connTX) {
		client.connTX <- []byte("filler")
	}
	client.wg.Add(1)
	go func() {
		defer client.wg.Done()
		client.sendLogin()
		client.sendPing()
		client.sendRPTC()
		client.sendRPTK([]byte{1, 2, 3, 4})
		client.sendPacket(proto.Packet{Signature: tagDMRD})
	}()

	stopped := make(chan struct{})
	go func() {
		client.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung while a sender was blocked on a full connTX")
	}
}

func TestTxDropsPacketsWhenConnectionClosed(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()
	client.conn.Close()
	for len(client.connTX) < cap(client.connTX) {
		client.connTX <- []byte("queued")
	}

	client.wg.Add(1)
	go client.tx()
	waitFor(t, time.Second, "tx to drain connTX", func() bool { return len(client.connTX) == 0 })

	close(client.done)
	client.wg.Wait()
}

func startFastClient(t *testing.T, m *fakeMaster) *MMDVMClient {
	t.Helper()
	cfg := testMMDVMConfig()
	cfg.MasterServer = m.addr()
	c := NewMMDVMClient(cfg, nil)
	c.keepAlive = 25 * time.Millisecond
	c.timeout = 300 * time.Millisecond
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Stop)
	return c
}

func TestRecoversAfterMasterStopsAnswering(t *testing.T) {
	t.Parallel()
	m := newFakeMaster(t)
	c := startFastClient(t, m)
	waitFor(t, 2*time.Second, "READY", isReady(c))

	m.answer.Store(false)
	waitFor(t, 2*time.Second, "ping timeout", func() bool { return c.currentState() != STATE_READY })
	logins := m.logins.Load()
	waitFor(t, 2*time.Second, "login retries while unanswered", func() bool { return m.logins.Load() >= logins+2 })

	m.answer.Store(true)
	waitFor(t, 2*time.Second, "READY again", isReady(c))
}

func TestRetriesLostLogin(t *testing.T) {
	t.Parallel()
	m := newFakeMaster(t)
	m.dropLogins.Store(3)
	c := startFastClient(t, m)
	waitFor(t, 2*time.Second, "READY", isReady(c))
}

func TestRedialsAfterDialFailure(t *testing.T) {
	t.Parallel()
	m := newFakeMaster(t)
	c := startFastClient(t, m)
	waitFor(t, 2*time.Second, "READY", isReady(c))

	var failures atomic.Int32
	failures.Store(3)
	var attempts atomic.Int32
	c.sessMu.Lock()
	realDial := c.dial
	c.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		attempts.Add(1)
		if failures.Add(-1) >= 0 {
			return nil, &net.DNSError{Err: "no such host", Name: address, IsNotFound: true}
		}
		return realDial(ctx, network, address)
	}
	c.sessMu.Unlock()

	m.pongsEnabled.Store(false)
	waitFor(t, 2*time.Second, "ping timeout", func() bool { return c.currentState() != STATE_READY })
	m.pongsEnabled.Store(true)
	waitFor(t, 3*time.Second, "READY after redial", isReady(c))
	if n := attempts.Load(); n < 4 {
		t.Fatalf("expected at least 4 dial attempts, got %d", n)
	}
}

func TestHandshakeRejectionDoesNotBlockHandler(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		st   state
		data string
	}{
		{STATE_SENT_LOGIN, testMSTNAK},
		{STATE_SENT_AUTH, "RPTNAK\x00\x04\xC2\x34"},
		{STATE_SENT_AUTH, testMSTNAK},
		{STATE_SENT_RPTC, testMSTNAK},
	} {
		client := newTestClient(t)
		client.state.Store(uint32(tc.st))
		start := time.Now()
		client.handleState([]byte(tc.data))
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Fatalf("state %d: handling %q blocked for %v", tc.st, tc.data[:6], d)
		}
	}
}

func TestStartSucceedsWhileMasterUnresolvable(t *testing.T) {
	t.Parallel()
	m := newFakeMaster(t)
	cfg := testMMDVMConfig()
	cfg.MasterServer = m.addr()
	c := NewMMDVMClient(cfg, nil)
	c.keepAlive = 25 * time.Millisecond
	c.timeout = 300 * time.Millisecond
	var failures, attempts atomic.Int32
	failures.Store(3)
	c.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		attempts.Add(1)
		if failures.Add(-1) >= 0 {
			return nil, &net.DNSError{Err: "no such host", Name: address, IsNotFound: true}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start failed while the master was unresolvable: %v", err)
	}
	t.Cleanup(c.Stop)
	waitFor(t, 2*time.Second, "READY once resolvable", isReady(c))
	if n := attempts.Load(); n < 4 {
		t.Fatalf("expected at least 4 dial attempts, got %d", n)
	}
}

func TestStartRejectsMalformedMasterAddress(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"missing-port", "127.0.0.1:99999", "127.0.0.1:abc", "127.0.0.1:", "127.0.0.1:0"} {
		cfg := testMMDVMConfig()
		cfg.MasterServer = addr
		c := NewMMDVMClient(cfg, nil)
		if err := c.Start(); err == nil {
			c.Stop()
			t.Errorf("expected an error for master address %q", addr)
		}
	}
}

func TestStopCancelsPendingDial(t *testing.T) {
	t.Parallel()
	cfg := testMMDVMConfig()
	cfg.MasterServer = "127.0.0.1:62031"
	c := NewMMDVMClient(cfg, nil)
	dialing := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	c.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		once.Do(func() { close(dialing) })
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}
	startErr := make(chan error, 1)
	go func() { startErr <- c.Start() }()
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked on the dial")
	}
	<-dialing

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung on a pending dial")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("dial context was not canceled")
	}
}

func TestDialTimesOut(t *testing.T) {
	t.Parallel()
	m := newFakeMaster(t)
	cfg := testMMDVMConfig()
	cfg.MasterServer = m.addr()
	c := NewMMDVMClient(cfg, nil)
	c.keepAlive = 25 * time.Millisecond
	c.timeout = 300 * time.Millisecond
	c.dialTimeout = 50 * time.Millisecond
	var hung atomic.Bool
	hung.Store(true)
	c.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if hung.CompareAndSwap(true, false) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Stop)
	waitFor(t, 2*time.Second, "READY after a hung dial", isReady(c))
}

func TestBusyHandlerDoesNotCauseReconnect(t *testing.T) {
	t.Parallel()
	for _, packets := range []int{1, 64} {
		t.Run(fmt.Sprintf("%d packets", packets), func(t *testing.T) {
			t.Parallel()
			m := newFakeMaster(t)
			cfg := testMMDVMConfig()
			cfg.MasterServer = m.addr()
			cfg.PassAllTG = []int{1, 2}
			c := NewMMDVMClient(cfg, nil)
			c.keepAlive = 20 * time.Millisecond
			c.timeout = 200 * time.Millisecond
			release := make(chan struct{})
			blocked := make(chan struct{})
			var once sync.Once
			c.SetIPSCHandler(func([]byte) {
				once.Do(func() { close(blocked) })
				<-release
			})
			if err := c.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(c.Stop)
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			waitFor(t, 2*time.Second, "READY", isReady(c))
			c.connMu.Lock()
			conn := c.conn
			c.connMu.Unlock()
			reconnected := func() bool {
				c.connMu.Lock()
				defer c.connMu.Unlock()
				return c.conn != conn || c.currentState() != STATE_READY
			}

			for i := range packets {
				pkt := proto.Packet{Signature: tagDMRD, Dst: 91, GroupCall: true, FrameType: 2, DTypeOrVSeq: 1, StreamID: 9, Seq: uint(i)}
				m.send(t, pkt.Encode())
			}
			<-blocked
			busyUntil := time.Now().Add(4 * c.timeout)
			for time.Now().Before(busyUntil) {
				if reconnected() {
					t.Fatalf("reconnected while the handler was busy (state %d)", c.currentState())
				}
				time.Sleep(5 * time.Millisecond)
			}
			close(release)
			waitFor(t, time.Second, "handler to drain", func() bool { return len(c.connRX) == 0 && !c.rxWaiting.Load() })
			time.Sleep(2 * c.keepAlive)
			if reconnected() {
				t.Fatalf("reconnected after the handler drained (state %d)", c.currentState())
			}
		})
	}
}

func TestReconnectDiscardsPacketsFromOldConnection(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	client.timeout = time.Hour
	client.wg.Add(1)
	go client.rx(client.conn, client.connStop)

	if _, err := client.conn.Write([]byte("PROBE")); err != nil {
		t.Fatalf("probe write: %v", err)
	}
	_, clientAddr := readFromServer(t, serverConn, time.Second)
	for range cap(client.connRX) + 1 {
		if _, err := serverConn.WriteToUDP([]byte("RPTACK\x01\x02\x03\x04"), clientAddr); err != nil {
			t.Fatalf("server write: %v", err)
		}
	}
	waitFor(t, time.Second, "rx to block on a full queue", client.rxWaiting.Load)

	client.sessMu.Lock()
	client.setState(STATE_SENT_LOGIN)
	client.reconnect()
	client.sessMu.Unlock()

	waitFor(t, time.Second, "old rx to give up", func() bool { return !client.rxWaiting.Load() })
	if n := len(client.connRX); n != 0 {
		t.Fatalf("expected no stale packets queued after reconnect, got %d", n)
	}
	client.Stop()
}

func TestSendPacketDroppedUntilReady(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.metrics = metrics.NewMetrics()
	for _, st := range []state{STATE_IDLE, STATE_SENT_LOGIN, STATE_SENT_AUTH, STATE_SENT_RPTC, STATE_TIMEOUT} {
		client.setState(st)
		client.sendPacket(proto.Packet{Signature: tagDMRD})
		if n := len(client.connTX); n != 0 {
			t.Fatalf("state %d: expected DMRD to be dropped, got %d queued", st, n)
		}
	}
	rec := httptest.NewRecorder()
	client.metrics.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if want := `mmdvm_packets_dropped_total{network="TestNet",reason="not_ready"} 5`; !strings.Contains(body, want) {
		t.Fatalf("expected %q in metrics output", want)
	}
	if strings.Contains(body, "mmdvm_packets_sent_total{") {
		t.Fatal("dropped packets must not be counted as sent")
	}
}

func TestHandshakeResendEverySecondInterval(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.keepAlive = time.Second
	client.timeout = time.Hour
	client.setState(STATE_SENT_LOGIN)
	start := time.Now()

	client.tick(start.Add(time.Second))
	if n := len(client.connTX); n != 0 {
		t.Fatalf("expected no resend after one interval, got %d queued", n)
	}
	client.tick(start.Add(2 * time.Second))
	if n := len(client.connTX); n != 1 {
		t.Fatalf("expected one resend after two intervals, got %d queued", n)
	}
	client.tick(start.Add(3 * time.Second))
	if n := len(client.connTX); n != 1 {
		t.Fatalf("expected no resend one interval after the last, got %d queued", n)
	}
	client.tick(start.Add(4 * time.Second))
	if n := len(client.connTX); n != 2 {
		t.Fatalf("expected a second resend, got %d queued", n)
	}
}

func TestStopStopsInboundTimeslotManager(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.setState(STATE_READY)
	mgr := timeslot.NewManager()
	client.inboundTSMgr = mgr
	pkt := func(stream uint) proto.Packet {
		return proto.Packet{Signature: tagDMRD, StreamID: stream, GroupCall: true}
	}

	if v := mgr.Submit(false, 1, "ipsc", pkt(1), false, client.forwardInbound); v != timeslot.Deliver {
		t.Fatalf("expected Deliver, got %v", v)
	}
	if v := mgr.Submit(false, 2, "ipsc", pkt(2), false, client.forwardInbound); v != timeslot.Queued {
		t.Fatalf("expected Queued, got %v", v)
	}
	for len(client.tx_chan) < cap(client.tx_chan) {
		client.tx_chan <- pkt(9)
	}
	mgr.Release(false, 1)

	stopped := make(chan struct{})
	go func() {
		client.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung with an inbound replay blocked on tx_chan")
	}
	if v := mgr.Submit(false, 3, "ipsc", pkt(3), false, client.forwardInbound); v != timeslot.Dropped {
		t.Fatalf("expected the inbound manager to be stopped, Submit returned %v", v)
	}
}
