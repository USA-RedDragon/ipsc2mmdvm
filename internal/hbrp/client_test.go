package hbrp

import (
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/ipsc2hbrp/internal/config"
	"github.com/USA-RedDragon/ipsc2hbrp/internal/hbrp/proto"
	"github.com/USA-RedDragon/ipsc2hbrp/internal/ipsc"
)

// Test protocol tag constants to avoid goconst warnings.
const (
	tagRPTL    = "RPTL"
	tagRPTCL   = "RPTCL"
	tagRPTC    = "RPTC"
	tagRPTK    = "RPTK"
	tagMSTPING = "MSTPING"
	tagDMRD    = "DMRD"
)

func testHBRPConfig() *config.Config {
	return &config.Config{
		HBRP: config.HBRP{
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
		},
	}
}

// newTestClient creates an HBRPClient with buffered channels
// so packet-building methods and handler can be tested without a real connection.
func newTestClient(t *testing.T) *HBRPClient {
	t.Helper()
	cfg := testHBRPConfig()
	translator, err := ipsc.NewIPSCTranslator()
	if err != nil {
		t.Fatalf("NewIPSCTranslator: %v", err)
	}
	client := &HBRPClient{
		config:     cfg,
		connTX:     make(chan []byte, 16),
		connRX:     make(chan []byte, 16),
		tx_chan:    make(chan proto.Packet, 16),
		done:       make(chan struct{}),
		translator: translator,
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

func TestNewHBRPClient(t *testing.T) {
	t.Parallel()
	cfg := testHBRPConfig()
	client := NewHBRPClient(cfg)
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.config != cfg {
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
	// Should contain the hex ID
	hexID := fmt.Sprintf("%08x", client.config.HBRP.ID)
	if string(data[4:12]) != hexID {
		t.Fatalf("expected hex ID %q, got %q", hexID, string(data[4:12]))
	}
	// Total length = 4 (RPTL) + 8 (hex ID)
	if len(data) != 12 {
		t.Fatalf("expected 12 bytes, got %d", len(data))
	}
}

func TestSendRPTCLPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTCL()

	data := <-client.connTX
	if string(data[:5]) != tagRPTCL {
		t.Fatalf("expected RPTCL prefix, got %q", string(data[:5]))
	}
	hexID := fmt.Sprintf("%08x", client.config.HBRP.ID)
	if string(data[5:13]) != hexID {
		t.Fatalf("expected hex ID %q, got %q", hexID, string(data[5:13]))
	}
	if len(data) != 13 {
		t.Fatalf("expected 13 bytes, got %d", len(data))
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

	// Check callsign (8 bytes, left-justified)
	callsign := string(data[4:12])
	if !strings.HasPrefix(callsign, "N0CALL") {
		t.Fatalf("expected callsign starting with N0CALL, got %q", callsign)
	}

	// Check hex radio ID (8 bytes)
	hexID := fmt.Sprintf("%08x", client.config.HBRP.ID)
	if string(data[12:20]) != hexID {
		t.Fatalf("expected hex ID %q at offset 12, got %q", hexID, string(data[12:20]))
	}

	// Check RX freq (9 bytes)
	expectedRX := fmt.Sprintf("%09d", client.config.HBRP.RXFreq)
	if string(data[20:29]) != expectedRX {
		t.Fatalf("expected RX freq %q, got %q", expectedRX, string(data[20:29]))
	}

	// Check TX freq (9 bytes)
	expectedTX := fmt.Sprintf("%09d", client.config.HBRP.TXFreq)
	if string(data[29:38]) != expectedTX {
		t.Fatalf("expected TX freq %q, got %q", expectedTX, string(data[29:38]))
	}

	// Check TX power (2 bytes)
	expectedPower := fmt.Sprintf("%02d", client.config.HBRP.TXPower)
	if string(data[38:40]) != expectedPower {
		t.Fatalf("expected TX power %q, got %q", expectedPower, string(data[38:40]))
	}

	// Check color code (2 bytes)
	expectedCC := fmt.Sprintf("%02d", client.config.HBRP.ColorCode)
	if string(data[40:42]) != expectedCC {
		t.Fatalf("expected color code %q, got %q", expectedCC, string(data[40:42]))
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

	// Hex ID at offset 4
	hexID := fmt.Sprintf("%08x", client.config.HBRP.ID)
	if string(data[4:12]) != hexID {
		t.Fatalf("expected hex ID %q, got %q", hexID, string(data[4:12]))
	}

	// Token at offset 12 should be 64 hex characters (sha256)
	token := string(data[12:76])
	if len(token) != 64 {
		t.Fatalf("expected 64-char token, got %d", len(token))
	}

	// Verify the token is the correct sha256(random + password)
	s256 := sha256.New()
	s256.Write(random)
	s256.Write([]byte(client.config.HBRP.Password))
	expectedToken := fmt.Sprintf("%x", s256.Sum(nil))
	if token != expectedToken {
		t.Fatalf("expected token %q, got %q", expectedToken, token)
	}

	// Total length = 4 (RPTK) + 8 (hex ID) + 64 (token)
	if len(data) != 76 {
		t.Fatalf("expected 76 bytes, got %d", len(data))
	}
}

func TestSendPingPacket(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendPing()

	data := <-client.connTX
	if string(data[:7]) != tagMSTPING {
		t.Fatalf("expected MSTPING prefix, got %q", string(data[:7]))
	}
	hexID := fmt.Sprintf("%08x", client.config.HBRP.ID)
	if string(data[7:15]) != hexID {
		t.Fatalf("expected hex ID %q, got %q", hexID, string(data[7:15]))
	}
	if len(data) != 15 {
		t.Fatalf("expected 15 bytes, got %d", len(data))
	}
}

func TestSendPacketEncodesAndSends(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
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
	if client.ipscHandler != nil {
		t.Fatal("expected nil handler initially")
	}
	called := false
	client.SetIPSCHandler(func(data []byte) {
		called = true
	})
	if client.ipscHandler == nil {
		t.Fatal("expected non-nil handler after SetIPSCHandler")
	}
	client.ipscHandler([]byte{})
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

func TestSendLoginHexIDFormat(t *testing.T) {
	t.Parallel()
	// Test with ID=1 to verify zero-padding
	cfg := testHBRPConfig()
	cfg.HBRP.ID = 1
	client := &HBRPClient{
		config: cfg,
		connTX: make(chan []byte, 16),
	}
	client.sendLogin()

	data := <-client.connTX
	hexID := string(data[4:12])
	if hexID != "00000001" {
		t.Fatalf("expected zero-padded hex ID %q, got %q", "00000001", hexID)
	}
}

func TestSendRPTKDifferentRandomProducesDifferentToken(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)

	client.sendRPTK([]byte("aaaaaaaa"))
	data1 := <-client.connTX
	token1 := string(data1[12:76])

	client.sendRPTK([]byte("bbbbbbbb"))
	data2 := <-client.connTX
	token2 := string(data2[12:76])

	if token1 == token2 {
		t.Fatal("expected different tokens for different random data")
	}
}

// --- Helper: create a loopback UDP "server" for integration tests ---

// udpPair creates a connected pair: a "server" UDPConn that the client talks to,
// and returns the server conn plus the client (already connected).
func udpPair(t *testing.T) (*net.UDPConn, *HBRPClient) {
	t.Helper()
	// Start a UDP listener (acts as fake HBRP master)
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("server listen: %v", err)
	}
	srvAddr, ok := serverConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	cfg := testHBRPConfig()
	cfg.HBRP.MasterServer = fmt.Sprintf("127.0.0.1:%d", srvAddr.Port)

	client := NewHBRPClient(cfg)
	if err := client.connect(); err != nil {
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
	cfg := testHBRPConfig()
	cfg.HBRP.MasterServer = "this-is-not-a-valid-address:::::999999"
	client := NewHBRPClient(cfg)

	err := client.connect()
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
	go client.rx()

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
	go client.rx()

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

	// Simulate MSTACK response with 8-byte random suffix
	resp := append([]byte("MSTACK"), []byte("12345678")...)
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
	client.state.Store(uint32(STATE_SENT_LOGIN))

	client.wg.Add(1)
	go client.handler()

	// Simulate rejection (not MSTACK)
	client.connRX <- []byte("MSTNAK__________")

	// Should retry login
	select {
	case data := <-client.connTX:
		if string(data[:4]) != tagRPTL {
			t.Fatalf("expected RPTL retry, got %q", string(data[:4]))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for RPTL retry")
	}

	close(client.done)
	client.wg.Wait()
}

func TestHandlerSentAuthMSTACK(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_SENT_AUTH))

	client.wg.Add(1)
	go client.handler()

	// MSTACK means auth accepted
	client.connRX <- []byte("MSTACK__________")

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
	client.state.Store(uint32(STATE_SENT_AUTH))

	client.wg.Add(1)
	go client.handler()

	// MSTNAK means password rejected
	client.connRX <- []byte("MSTNAK__________")

	// Should fall back to STATE_SENT_LOGIN and retry login
	select {
	case data := <-client.connTX:
		if string(data[:4]) != tagRPTL {
			t.Fatalf("expected RPTL retry after MSTNAK, got %q", string(data[:4]))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for RPTL retry")
	}

	//nolint:gosec // G115: test-only, state values fit in uint8
	if state(client.state.Load()) != STATE_SENT_LOGIN {
		t.Fatalf("expected STATE_SENT_LOGIN, got %d", client.state.Load())
	}

	close(client.done)
	client.wg.Wait()
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

	cfg := testHBRPConfig()
	srvAddr, ok := serverConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	cfg.HBRP.MasterServer = fmt.Sprintf("127.0.0.1:%d", srvAddr.Port)
	client.config = cfg

	// Give it a real conn for ping
	if err := client.connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Override keepAlive/timeout for faster test
	client.keepAlive = 100 * time.Millisecond
	client.timeout = 5 * time.Second

	client.wg.Add(1)
	go client.handler()

	// MSTACK means config accepted
	client.connRX <- []byte("MSTACK__________")

	// Should transition to STATE_READY and start ping (which sends MSTPING)
	select {
	case data := <-client.connTX:
		if string(data[:7]) != tagMSTPING {
			t.Fatalf("expected MSTPING from ping(), got %q", string(data[:min(7, len(data))]))
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
	client.state.Store(uint32(STATE_SENT_RPTC))

	client.wg.Add(1)
	go client.handler()

	// MSTNAK means config rejected
	client.connRX <- []byte("MSTNAK__________")

	// Should retry with RPTC
	select {
	case data := <-client.connTX:
		if string(data[:4]) != tagRPTC {
			t.Fatalf("expected RPTC retry, got %q", string(data[:4]))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for RPTC retry")
	}

	close(client.done)
	client.wg.Wait()
}

func TestHandlerReadyRPTPONG(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.state.Store(uint32(STATE_READY))

	client.wg.Add(1)
	go client.handler()

	before := time.Now().UnixNano()
	client.connRX <- []byte("RPTPONG_________")

	// Give handler time
	time.Sleep(50 * time.Millisecond)

	lastPing := client.lastPing.Load()
	if lastPing < before {
		t.Fatal("expected lastPing to be updated after RPTPONG")
	}

	close(client.done)
	client.wg.Wait()
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

	// Wait for handler to process
	time.Sleep(100 * time.Millisecond)

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

// --- ping() tests ---

func TestPingSendsInitialPing(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.keepAlive = 100 * time.Millisecond
	client.timeout = 5 * time.Second
	client.state.Store(uint32(STATE_READY))

	client.wg.Add(1)
	go client.ping()

	// Should send an immediate MSTPING
	select {
	case data := <-client.connTX:
		if string(data[:7]) != tagMSTPING {
			t.Fatalf("expected MSTPING, got %q", string(data[:min(7, len(data))]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial MSTPING")
	}

	close(client.done)
	client.wg.Wait()
}

func TestPingSendsPeriodicPings(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.keepAlive = 50 * time.Millisecond
	client.timeout = 5 * time.Second
	client.state.Store(uint32(STATE_READY))

	client.wg.Add(1)
	go client.ping()

	// Drain the initial ping
	<-client.connTX

	// Simulate pong to keep alive
	client.lastPing.Store(time.Now().UnixNano())

	// Wait for a periodic ping
	select {
	case data := <-client.connTX:
		if string(data[:7]) != tagMSTPING {
			t.Fatalf("expected periodic MSTPING, got %q", string(data[:min(7, len(data))]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for periodic MSTPING")
	}

	close(client.done)
	client.wg.Wait()
}

func TestPingStopsOnDone(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.keepAlive = 50 * time.Millisecond
	client.timeout = 5 * time.Second

	client.wg.Add(1)
	go client.ping()

	// Drain initial ping
	<-client.connTX

	close(client.done)
	client.wg.Wait()
}

func TestPingTimeoutReconnects(t *testing.T) {
	t.Parallel()
	serverConn, client := udpPair(t)
	defer serverConn.Close()

	client.keepAlive = 50 * time.Millisecond
	client.timeout = 10 * time.Millisecond // very short timeout
	client.state.Store(uint32(STATE_READY))

	// Set lastPing far in the past to trigger timeout
	client.lastPing.Store(time.Now().Add(-1 * time.Minute).UnixNano())

	client.wg.Add(1)
	go client.ping()

	// Drain initial ping
	<-client.connTX

	// Should send RPTL (login) after timeout
	select {
	case data := <-client.connTX:
		if string(data[:4]) != tagRPTL {
			t.Fatalf("expected RPTL after timeout, got %q", string(data[:min(4, len(data))]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RPTL after ping timeout")
	}

	//nolint:gosec // G115: test-only, state values fit in uint8
	if state(client.state.Load()) != STATE_SENT_LOGIN {
		t.Fatalf("expected STATE_SENT_LOGIN after timeout, got %d", client.state.Load())
	}

	close(client.done)
	client.wg.Wait()
}

// --- forwardTX() tests ---

func TestForwardTXSendsPackets(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)

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

	// Should return immediately without sending
	client.HandleIPSCBurst(0x80, make([]byte, 54), &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234})

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
	client.started.Store(true)
	if client.translator != nil {
		client.translator.SetPeerID(client.config.HBRP.ID)
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

	// Should produce at least 1 HBRP packet on tx_chan
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
	client.started.Store(true)
	if client.translator != nil {
		client.translator.SetPeerID(client.config.HBRP.ID)
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

	// Unsupported type (0x99)
	client.HandleIPSCBurst(0x99, make([]byte, 54), &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234})

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
	hexID := fmt.Sprintf("%08x", client.config.HBRP.ID)
	if string(got[5:13]) != hexID {
		t.Fatalf("expected hex ID %q, got %q", hexID, string(got[5:13]))
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

	cfg := testHBRPConfig()
	cfg.HBRP.MasterServer = fmt.Sprintf("127.0.0.1:%d", srvAddr.Port)

	client := NewHBRPClient(cfg)
	client.keepAlive = 200 * time.Millisecond
	client.timeout = 5 * time.Second

	if err := client.connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	client.started.Store(true)

	client.wg.Add(4)
	go client.handler()
	go client.rx()
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

	// Step 2: Server sends MSTACK with random
	mstack := append([]byte("MSTACK"), []byte("ABCDEFGH")...)
	if _, err := serverConn.WriteToUDP(mstack, clientAddr); err != nil {
		t.Fatalf("server write MSTACK: %v", err)
	}

	// Step 3: Client sends RPTK
	rptkData, _ := readFromServer(t, serverConn, 2*time.Second)
	if string(rptkData[:4]) != tagRPTK {
		t.Fatalf("expected RPTK, got %q", string(rptkData[:4]))
	}

	// Step 4: Server sends MSTACK (auth accepted)
	if _, err := serverConn.WriteToUDP([]byte("MSTACK__________"), clientAddr); err != nil {
		t.Fatalf("server write MSTACK: %v", err)
	}

	// Step 5: Client sends RPTC
	rptcData, _ := readFromServer(t, serverConn, 2*time.Second)
	if string(rptcData[:4]) != tagRPTC {
		t.Fatalf("expected RPTC, got %q", string(rptcData[:4]))
	}

	// Step 6: Server sends MSTACK (config accepted)
	if _, err := serverConn.WriteToUDP([]byte("MSTACK__________"), clientAddr); err != nil {
		t.Fatalf("server write MSTACK: %v", err)
	}

	// Step 7: Client should now be in READY state and send MSTPING
	pingData, _ := readFromServer(t, serverConn, 2*time.Second)
	if string(pingData[:7]) != tagMSTPING {
		t.Fatalf("expected MSTPING, got %q", string(pingData[:min(7, len(pingData))]))
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

	// Step 8: Server sends RPTPONG
	if _, err := serverConn.WriteToUDP([]byte("RPTPONG_________"), clientAddr); err != nil {
		t.Fatalf("server write RPTPONG: %v", err)
	}

	// Give handler time to process
	time.Sleep(100 * time.Millisecond)

	lastPing := client.lastPing.Load()
	if lastPing == 0 {
		t.Fatal("expected lastPing to be set after RPTPONG")
	}

	client.Stop()
}

// --- RPTC field encoding tests ---

func TestSendRPTCLatLong(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Latitude (8 bytes) starts at offset 42
	lat := string(data[42:50])
	if !strings.HasPrefix(lat, "35.0000") {
		t.Fatalf("expected latitude starting with 35.0000, got %q", lat)
	}

	// Longitude (9 bytes) starts at offset 50
	lon := string(data[50:59])
	if !strings.HasPrefix(lon, "-97.0000") {
		t.Fatalf("expected longitude starting with -97.0000, got %q", lon)
	}
}

func TestSendRPTCHeight(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Height (3 bytes) at offset 59
	height := string(data[59:62])
	if height != "030" {
		t.Fatalf("expected height 030, got %q", height)
	}
}

func TestSendRPTCLocation(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Location (20 bytes) at offset 62
	location := strings.TrimRight(string(data[62:82]), " ")
	if location != "Oklahoma" {
		t.Fatalf("expected location 'Oklahoma', got %q", location)
	}
}

func TestSendRPTCDescription(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// Description (20 bytes) at offset 82
	desc := strings.TrimRight(string(data[82:102]), " ")
	if desc != "Test Repeater" {
		t.Fatalf("expected description 'Test Repeater', got %q", desc)
	}
}

func TestSendRPTCURL(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.sendRPTC()

	data := <-client.connTX
	// URL (124 bytes) at offset 102
	url := strings.TrimRight(string(data[102:226]), " ")
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
	token := string(data[12:76])

	s256 := sha256.New()
	s256.Write(random)
	s256.Write([]byte(client.config.HBRP.Password))
	expected := fmt.Sprintf("%x", s256.Sum(nil))

	if token != expected {
		t.Fatalf("token mismatch:\n  got:  %s\n  want: %s", token, expected)
	}
}

// --- Verify sendPacket encodes correctly ---

func TestSendPacketFieldsEncoded(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
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
	client.started.Store(true)
	if client.translator != nil {
		client.translator.SetPeerID(client.config.HBRP.ID)
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
