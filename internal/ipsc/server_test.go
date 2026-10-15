package ipsc

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/config"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

func testConfig(authEnabled bool, authKey string) *config.Config {
	return &config.Config{
		MMDVM: []config.MMDVM{{
			ID: 311860,
		}},
		IPSC: config.IPSC{
			Auth: config.IPSCAuth{
				Enabled: authEnabled,
				Key:     authKey,
			},
		},
	}
}

func TestParsePeerID(t *testing.T) {
	t.Parallel()
	data := make([]byte, 5)
	binary.BigEndian.PutUint32(data[1:5], 12345)
	id, err := parsePeerID(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 12345 {
		t.Fatalf("expected 12345, got %d", id)
	}
}

func TestParsePeerIDTooShort(t *testing.T) {
	t.Parallel()
	_, err := parsePeerID([]byte{0x90, 0x00})
	if err == nil {
		t.Fatal("expected error for short data")
	}
}

func TestParsePeerIDMaxValue(t *testing.T) {
	t.Parallel()
	data := []byte{0x90, 0xFF, 0xFF, 0xFF, 0xFF}
	id, err := parsePeerID(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 0xFFFFFFFF {
		t.Fatalf("expected 0xFFFFFFFF, got %d", id)
	}
}

func TestUint16ToBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		val    uint16
		expect [2]byte
	}{
		{0, [2]byte{0, 0}},
		{1, [2]byte{0, 1}},
		{0xFF00, [2]byte{0xFF, 0x00}},
		{0xBEEF, [2]byte{0xBE, 0xEF}},
	}
	for _, tt := range tests {
		b := uint16ToBytes(tt.val)
		if b[0] != tt.expect[0] || b[1] != tt.expect[1] {
			t.Errorf("uint16ToBytes(%d) = %v, want %v", tt.val, b, tt.expect)
		}
	}
}

func TestUint32ToBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		val    uint32
		expect [4]byte
	}{
		{0, [4]byte{0, 0, 0, 0}},
		{1, [4]byte{0, 0, 0, 1}},
		{0xDEADBEEF, [4]byte{0xDE, 0xAD, 0xBE, 0xEF}},
	}
	for _, tt := range tests {
		b := uint32ToBytes(tt.val)
		if b[0] != tt.expect[0] || b[1] != tt.expect[1] || b[2] != tt.expect[2] || b[3] != tt.expect[3] {
			t.Errorf("uint32ToBytes(%d) = %v, want %v", tt.val, b, tt.expect)
		}
	}
}

func TestCloneUDPAddrNil(t *testing.T) {
	t.Parallel()
	if cloneUDPAddr(nil) != nil {
		t.Fatal("expected nil for nil input")
	}
}

func TestCloneUDPAddr(t *testing.T) {
	t.Parallel()
	orig := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234, Zone: "eth0"}
	clone := cloneUDPAddr(orig)

	if !clone.IP.Equal(orig.IP) || clone.Port != orig.Port || clone.Zone != orig.Zone {
		t.Fatalf("clone mismatch: orig=%v clone=%v", orig, clone)
	}

	// Mutating the clone must not affect the original.
	clone.IP[0] = 99
	if orig.IP[0] == 99 {
		t.Fatal("clone shares underlying IP slice with original")
	}
}

func TestCloneUDPAddrNilIP(t *testing.T) {
	t.Parallel()
	orig := &net.UDPAddr{Port: 5678}
	clone := cloneUDPAddr(orig)
	if clone.IP != nil {
		t.Fatalf("expected nil IP, got %v", clone.IP)
	}
	if clone.Port != 5678 {
		t.Fatalf("expected port 5678, got %d", clone.Port)
	}
}

func TestAuth(t *testing.T) {
	t.Parallel()
	key := "0000000000000000000000000000000000001234"
	cfg := testConfig(true, "1234")
	s := NewIPSCServer(cfg, nil)

	payload := []byte("hello world")
	h := hmac.New(sha1.New, mustDecodeHex(t, key))
	h.Write(payload)
	hash := h.Sum(nil)[:10]
	data := make([]byte, 0, len(payload)+len(hash))
	data = append(data, payload...)
	data = append(data, hash...)

	if !s.auth(data) {
		t.Fatal("expected auth to pass")
	}
}

func TestAuthBadHash(t *testing.T) {
	t.Parallel()
	cfg := testConfig(true, "1234")
	s := NewIPSCServer(cfg, nil)

	payload := []byte("hello world")
	bad := make([]byte, 10)
	data := make([]byte, 0, len(payload)+len(bad))
	data = append(data, payload...)
	data = append(data, bad...)

	if s.auth(data) {
		t.Fatal("expected auth to fail with bad hash")
	}
}

func mustDecodeHex(t *testing.T, hexStr string) []byte {
	t.Helper()
	b := make([]byte, len(hexStr)/2)
	for i := 0; i < len(hexStr); i += 2 {
		var val byte
		for j := 0; j < 2; j++ {
			c := hexStr[i+j]
			switch {
			case c >= '0' && c <= '9':
				val = val*16 + (c - '0')
			case c >= 'a' && c <= 'f':
				val = val*16 + (c - 'a' + 10)
			case c >= 'A' && c <= 'F':
				val = val*16 + (c - 'A' + 10)
			}
		}
		b[i/2] = val
	}
	return b
}

func TestNewIPSCServerNoAuth(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if s.authKey != nil {
		t.Fatal("expected nil auth key when auth disabled")
	}
	if s.localID != cfg.MMDVM[0].ID {
		t.Fatalf("expected localID %d, got %d", cfg.MMDVM[0].ID, s.localID)
	}
}

func TestNewIPSCServerWithAuth(t *testing.T) {
	t.Parallel()
	cfg := testConfig(true, "ABCD")
	s := NewIPSCServer(cfg, nil)
	if s.authKey == nil {
		t.Fatal("expected non-nil auth key")
	}
	if len(s.authKey) != 20 {
		t.Fatalf("expected 20-byte auth key, got %d", len(s.authKey))
	}
}

func TestDefaultModeByte(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	mode := s.defaultModeByte()
	// Should have operational, digital, ts1, ts2 bits
	if mode&0b01000000 == 0 {
		t.Fatal("expected peerOperational bit set")
	}
	if mode&0b00100000 == 0 {
		t.Fatal("expected peerDigital bit set")
	}
	if mode&0b00001000 == 0 {
		t.Fatal("expected ts1On bit set")
	}
	if mode&0b00000010 == 0 {
		t.Fatal("expected ts2On bit set")
	}
}

func TestDefaultFlagsBytesNoAuth(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	flags := s.defaultFlagsBytes()
	if flags[3]&0x10 != 0 {
		t.Fatal("expected auth flag clear when auth disabled")
	}
	if flags[3]&0x0D != 0x0D {
		t.Fatalf("expected base flags 0x0D, got %02X", flags[3])
	}
}

func TestDefaultFlagsBytesWithAuth(t *testing.T) {
	t.Parallel()
	cfg := testConfig(true, "1234")
	s := NewIPSCServer(cfg, nil)
	flags := s.defaultFlagsBytes()
	if flags[3]&0x10 == 0 {
		t.Fatal("expected auth flag set when auth enabled")
	}
}

func TestBuildMasterRegisterReply(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	reply := s.buildMasterRegisterReply()

	if reply[0] != byte(PacketType_MasterRegisterReply) {
		t.Fatalf("expected packet type 0x%02X, got 0x%02X", PacketType_MasterRegisterReply, reply[0])
	}

	id := binary.BigEndian.Uint32(reply[1:5])
	if id != cfg.MMDVM[0].ID {
		t.Fatalf("expected ID %d, got %d", cfg.MMDVM[0].ID, id)
	}
}

func TestBuildMasterAliveReply(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	reply := s.buildMasterAliveReply()

	if reply[0] != byte(PacketType_MasterAliveReply) {
		t.Fatalf("expected packet type 0x%02X, got 0x%02X", PacketType_MasterAliveReply, reply[0])
	}
}

func TestBuildPeerListReplyEmpty(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	reply := s.buildPeerListReply()

	if reply[0] != byte(PacketType_PeerListReply) {
		t.Fatalf("expected packet type 0x%02X, got 0x%02X", PacketType_PeerListReply, reply[0])
	}

	// Peer count should be 0
	peerCount := binary.BigEndian.Uint16(reply[5:7])
	if peerCount != 0 {
		t.Fatalf("expected 0 peers, got %d", peerCount)
	}
}

func TestBuildPeerListReplyWithPeers(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 100), Port: 50000}
	if err := s.upsertPeer(42, addr, 0x6A, [4]byte{0, 0, 0, 0x0D}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}

	reply := s.buildPeerListReply()
	if reply[0] != byte(PacketType_PeerListReply) {
		t.Fatalf("expected packet type 0x%02X, got 0x%02X", PacketType_PeerListReply, reply[0])
	}

	// Should have at least the peer entry bytes after the header
	if len(reply) < 7+11 {
		t.Fatalf("reply too short for 1 peer: %d bytes", len(reply))
	}
}

func TestUpsertPeerAndCount(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	if s.peerCount() != 0 {
		t.Fatalf("expected 0 peers initially, got %d", s.peerCount())
	}

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	if err := s.upsertPeer(100, addr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}

	if s.peerCount() != 1 {
		t.Fatalf("expected 1 peer, got %d", s.peerCount())
	}

	// Upsert same peer should not increase count
	if err := s.upsertPeer(100, addr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
	if s.peerCount() != 1 {
		t.Fatalf("expected still 1 peer after upsert, got %d", s.peerCount())
	}

	// Add a different peer
	addr2 := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 5678}
	if err := s.upsertPeer(200, addr2, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
	if s.peerCount() != 2 {
		t.Fatalf("expected 2 peers, got %d", s.peerCount())
	}
}

func TestMarkPeerAlive(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	if s.markPeerAlive(100) {
		t.Fatal("markPeerAlive accepted an unregistered peer")
	}
	if s.peerCount() != 0 {
		t.Fatalf("expected no peers after markPeerAlive, got %d", s.peerCount())
	}

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	if err := s.upsertPeer(100, addr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
	for range 2 {
		if !s.markPeerAlive(100) {
			t.Fatal("markPeerAlive rejected a registered peer")
		}
	}
	s.mu.RLock()
	peer := s.peers[100]
	keepAlive := peer.KeepAliveReceived
	s.mu.RUnlock()

	if keepAlive != 2 {
		t.Fatalf("expected 2 keepalives, got %d", keepAlive)
	}
}

func TestHandlePacketTooShort(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	_, err := s.handlePacket([]byte{}, addr)
	if err == nil {
		t.Fatal("expected error on empty packet")
	}
}

func TestHandlePacketUnknownType(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	data := []byte{0xFF, 0, 0, 0, 1}
	_, err := s.handlePacket(data, addr)
	if err == nil {
		t.Fatal("expected error for unknown packet type 0xFF")
	}
}

func TestHandlePacketReplyTypesIgnored(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	replyTypes := []byte{
		byte(PacketType_MasterRegisterReply),
		byte(PacketType_PeerListReply),
		byte(PacketType_MasterAliveReply),
	}
	for _, pt := range replyTypes {
		data := make([]byte, 5)
		data[0] = pt
		_, err := s.handlePacket(data, addr)
		if !errors.Is(err, ErrPacketIgnored) {
			t.Fatalf("expected ErrPacketIgnored for type 0x%02X, got %v", pt, err)
		}
	}
}

func TestPacketTypeValues(t *testing.T) {
	t.Parallel()
	// Verify the packet type constants match the IPSC protocol
	expected := map[PacketType]byte{
		PacketType_GroupVoice:            0x80,
		PacketType_PrivateVoice:          0x81,
		PacketType_GroupData:             0x83,
		PacketType_PrivateData:           0x84,
		PacketType_RepeaterWakeUp:        0x85,
		PacketType_MasterRegisterRequest: 0x90,
		PacketType_MasterRegisterReply:   0x91,
		PacketType_PeerListRequest:       0x92,
		PacketType_PeerListReply:         0x93,
		PacketType_MasterAliveRequest:    0x96,
		PacketType_MasterAliveReply:      0x97,
	}
	for pt, val := range expected {
		if byte(pt) != val {
			t.Errorf("PacketType %v: expected 0x%02X, got 0x%02X", pt, val, byte(pt))
		}
	}
}

func TestUpsertPeerRegistrationStatus(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	if err := s.upsertPeer(100, addr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}

	s.mu.RLock()
	peer := s.peers[100]
	registered := peer.RegistrationStatus
	lastSeen := peer.LastSeen
	s.mu.RUnlock()

	if !registered {
		t.Fatal("expected peer to be registered")
	}
	if time.Since(lastSeen) > time.Second {
		t.Fatal("expected LastSeen to be recent")
	}
}

// --- Helper: create a server with a real loopback UDP socket ---

func newTestServerWithUDP(t *testing.T, authEnabled bool, authKey string) (*IPSCServer, *net.UDPAddr) {
	t.Helper()
	cfg := testConfig(authEnabled, authKey)
	s := NewIPSCServer(cfg, nil)

	// Bind to loopback on a random port
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s.udp = conn
	t.Cleanup(func() {
		s.stopped.Store(true)
		conn.Close()
	})

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	return s, addr
}

// registerPeer registers peerID as if it had sent a master register request.
func registerPeer(t *testing.T, s *IPSCServer, peerID uint32) {
	t.Helper()
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50000}
	if err := s.upsertPeer(peerID, addr, s.defaultModeByte(), s.defaultFlagsBytes()); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
}

// startBurstWorker runs the burst worker for tests that call handlePacket directly.
func startBurstWorker(t *testing.T, s *IPSCServer) {
	t.Helper()
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.burstWorker(quit)
	}()
	t.Cleanup(func() {
		close(quit)
		<-done
	})
}

// readUDP reads one datagram from the given conn with a timeout.
func readUDP(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP: %v", err)
	}
	return buf[:n]
}

// makeControlPacket builds a minimal control packet (type + 4-byte peer ID).
func makeControlPacket(packetType PacketType, peerID uint32) []byte {
	data := make([]byte, 5)
	data[0] = byte(packetType)
	binary.BigEndian.PutUint32(data[1:5], peerID)
	return data
}

// makeControlPacketWithModeFlags builds a control packet with mode + flags.
func makeControlPacketWithModeFlags(packetType PacketType, peerID uint32, mode byte, flags [4]byte) []byte {
	data := make([]byte, 10)
	data[0] = byte(packetType)
	binary.BigEndian.PutUint32(data[1:5], peerID)
	data[5] = mode
	copy(data[6:10], flags[:])
	return data
}

// signPacket appends an HMAC-SHA1 hash to the packet data.
func signPacket(t *testing.T, data []byte, hexKey string) []byte {
	t.Helper()
	key := mustDecodeHex(t, hexKey)
	h := hmac.New(sha1.New, key)
	h.Write(data)
	hash := h.Sum(nil)[:10]
	return append(data, hash...)
}

// --- sendPacket tests ---

func TestSendPacketNoAuth(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	// Create a client UDP socket to receive the packet
	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()
	clientAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	// Send a packet to the client
	_ = srvAddr // server address, not needed directly
	payload := []byte("hello")
	pkt := &Packet{data: payload}
	if err := s.sendPacket(pkt, clientAddr); err != nil {
		t.Fatalf("sendPacket error: %v", err)
	}

	got := readUDP(t, client)
	if string(got) != "hello" {
		t.Fatalf("expected 'hello', got %q", got)
	}
}

func TestSendPacketWithAuth(t *testing.T) {
	t.Parallel()
	hexKey := "0000000000000000000000000000000000001234"
	s, _ := newTestServerWithUDP(t, true, "1234")

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()
	clientAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	payload := []byte("secure")
	pkt := &Packet{data: payload}
	if err := s.sendPacket(pkt, clientAddr); err != nil {
		t.Fatalf("sendPacket error: %v", err)
	}

	got := readUDP(t, client)
	// Should be payload + 10-byte HMAC
	if len(got) != len(payload)+10 {
		t.Fatalf("expected %d bytes (payload+hash), got %d", len(payload)+10, len(got))
	}

	// Verify the HMAC
	authKey := mustDecodeHex(t, hexKey)
	h := hmac.New(sha1.New, authKey)
	h.Write(payload)
	expectedHash := h.Sum(nil)[:10]
	if !hmac.Equal(got[len(payload):], expectedHash) {
		t.Fatal("HMAC mismatch on sent packet")
	}
}

// --- Handler flows with real UDP ---

func TestHandleMasterRegisterRequestFlow(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Build a register request with mode and flags
	peerID := uint32(55555)
	reqData := makeControlPacketWithModeFlags(PacketType_MasterRegisterRequest, peerID, 0x6A, [4]byte{0, 0, 0, 0x0D})
	clientUDPAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	_, err = s.handlePacket(reqData, clientUDPAddr)
	if err != nil {
		t.Fatalf("handlePacket error: %v", err)
	}

	// Verify peer was registered
	s.mu.RLock()
	peer, ok := s.peers[peerID]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected peer to be registered")
	}
	if peer.Mode != 0x6A {
		t.Fatalf("expected mode 0x6A, got 0x%02X", peer.Mode)
	}
	if !peer.RegistrationStatus {
		t.Fatal("expected peer registered=true")
	}

	// Verify reply was sent
	reply := readUDP(t, client)
	if reply[0] != byte(PacketType_MasterRegisterReply) {
		t.Fatalf("expected register reply type 0x%02X, got 0x%02X", PacketType_MasterRegisterReply, reply[0])
	}
}

func TestHandleMasterRegisterRequestShortPacket(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Short register request (no mode/flags) — should still work using defaults
	reqData := makeControlPacket(PacketType_MasterRegisterRequest, 77777)
	clientUDPAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	_, err = s.handlePacket(reqData, clientUDPAddr)
	if err != nil {
		t.Fatalf("handlePacket error: %v", err)
	}

	s.mu.RLock()
	peer := s.peers[77777]
	s.mu.RUnlock()
	if peer == nil {
		t.Fatal("expected peer to exist")
	}
	// Mode should be the default
	if peer.Mode != s.defaultModeByte() {
		t.Fatalf("expected default mode, got 0x%02X", peer.Mode)
	}
}

func TestHandleMasterAliveRequestFlow(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	peerID := uint32(66666)
	reqData := makeControlPacket(PacketType_MasterAliveRequest, peerID)
	aliveAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	registerPeer(t, s, peerID)
	_, err = s.handlePacket(reqData, aliveAddr)
	if err != nil {
		t.Fatalf("handlePacket error: %v", err)
	}

	// Verify peer was marked alive
	s.mu.RLock()
	peer := s.peers[peerID]
	s.mu.RUnlock()
	if peer == nil {
		t.Fatal("expected peer to exist after alive request")
	}
	if peer.KeepAliveReceived != 1 {
		t.Fatalf("expected 1 keepalive, got %d", peer.KeepAliveReceived)
	}

	// Verify reply was sent
	reply := readUDP(t, client)
	if reply[0] != byte(PacketType_MasterAliveReply) {
		t.Fatalf("expected alive reply type 0x%02X, got 0x%02X", PacketType_MasterAliveReply, reply[0])
	}
}

func TestHandlePeerListRequestFlow(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	reqData := makeControlPacket(PacketType_PeerListRequest, 88888)
	peerListAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	registerPeer(t, s, 88888)
	_, err = s.handlePacket(reqData, peerListAddr)
	if err != nil {
		t.Fatalf("handlePacket error: %v", err)
	}

	reply := readUDP(t, client)
	if reply[0] != byte(PacketType_PeerListReply) {
		t.Fatalf("expected peer list reply type 0x%02X, got 0x%02X", PacketType_PeerListReply, reply[0])
	}
}

func TestHandlePeerListRequestTooShort(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Packet type + only 2 bytes (too short for peer ID)
	data := []byte{byte(PacketType_PeerListRequest), 0x00, 0x01}
	shortAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	_, err = s.handlePacket(data, shortAddr)
	if err == nil {
		t.Fatal("expected error for too-short peer list request")
	}
}

func TestHandleRepeaterWakeUp(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 3000}
	peerID := uint32(11111)
	data := makeControlPacket(PacketType_RepeaterWakeUp, peerID)

	registerPeer(t, s, peerID)
	_, err := s.handlePacket(data, addr)
	if err != nil {
		t.Fatalf("handlePacket error: %v", err)
	}

	s.mu.RLock()
	peer := s.peers[peerID]
	s.mu.RUnlock()
	if peer == nil {
		t.Fatal("expected peer to exist after wake-up")
	}
	if peer.KeepAliveReceived != 1 {
		t.Fatalf("expected 1 keepalive, got %d", peer.KeepAliveReceived)
	}
}

func TestHandleRepeaterWakeUpTooShort(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 3000}
	data := []byte{byte(PacketType_RepeaterWakeUp), 0x00}
	_, err := s.handlePacket(data, addr)
	if err == nil {
		t.Fatal("expected error for too-short wake-up packet")
	}
}

// --- User packet and burst handler tests ---

func TestHandleUserPacketCallsBurstHandler(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	startBurstWorker(t, s)

	var received atomic.Bool
	var gotType atomic.Uint32
	var wg sync.WaitGroup
	wg.Add(1)

	s.SetBurstHandler(func(packetType byte, data []byte, addr *net.UDPAddr) {
		defer wg.Done()
		received.Store(true)
		gotType.Store(uint32(packetType))
	})

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	data := make([]byte, 54)
	data[0] = byte(PacketType_GroupVoice)
	binary.BigEndian.PutUint32(data[1:5], 42)

	registerPeer(t, s, 42)
	_, err := s.handlePacket(data, addr)
	if err != nil {
		t.Fatalf("handlePacket error: %v", err)
	}

	wg.Wait()
	if !received.Load() {
		t.Fatal("burst handler was not called")
	}
	if gotType.Load() != uint32(PacketType_GroupVoice) {
		t.Fatalf("expected packet type 0x80, got 0x%02X", gotType.Load())
	}

	// Verify peer was marked alive
	s.mu.RLock()
	peer := s.peers[42]
	s.mu.RUnlock()
	if peer == nil {
		t.Fatal("expected peer to be created")
	}
}

func TestHandleUserPacketNoBurstHandler(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	// No burst handler set

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	data := make([]byte, 54)
	data[0] = byte(PacketType_PrivateVoice)
	binary.BigEndian.PutUint32(data[1:5], 43)

	registerPeer(t, s, 43)
	_, err := s.handlePacket(data, addr)
	if err != nil {
		t.Fatalf("handlePacket error (no handler): %v", err)
	}
}

func TestHandleUserPacketAllTypes(t *testing.T) {
	t.Parallel()

	types := []PacketType{
		PacketType_GroupVoice,
		PacketType_PrivateVoice,
		PacketType_GroupData,
		PacketType_PrivateData,
	}

	for _, pt := range types {
		cfg := testConfig(false, "")
		s := NewIPSCServer(cfg, nil)

		addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
		data := make([]byte, 54)
		data[0] = byte(pt)
		binary.BigEndian.PutUint32(data[1:5], 50)

		registerPeer(t, s, 50)
		_, err := s.handlePacket(data, addr)
		if err != nil {
			t.Fatalf("handlePacket for type 0x%02X error: %v", byte(pt), err)
		}
	}
}

func TestHandleUserPacketTooShortForPeerID(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	data := []byte{byte(PacketType_GroupVoice), 0x00, 0x01}

	_, err := s.handlePacket(data, addr)
	if err == nil {
		t.Fatal("expected error for user packet too short for peer ID")
	}
}

func TestHandleUserPacketBurstHandlerReceivesDataCopy(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	startBurstWorker(t, s)

	var receivedData []byte
	var wg sync.WaitGroup
	wg.Add(1)

	s.SetBurstHandler(func(packetType byte, data []byte, addr *net.UDPAddr) {
		defer wg.Done()
		receivedData = data
	})

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	data := make([]byte, 20)
	data[0] = byte(PacketType_GroupVoice)
	binary.BigEndian.PutUint32(data[1:5], 42)
	data[10] = 0xAA // sentinel value

	registerPeer(t, s, 42)
	_, err := s.handlePacket(data, addr)
	if err != nil {
		t.Fatalf("handlePacket error: %v", err)
	}

	wg.Wait()

	// Mutate original to verify handler got a copy
	data[10] = 0xFF
	if receivedData[10] != 0xAA {
		t.Fatal("burst handler should receive a copy of the data, not a reference")
	}
}

// --- Auth-related handlePacket tests ---

func TestHandlePacketAuthEnabledTooShort(t *testing.T) {
	t.Parallel()
	cfg := testConfig(true, "1234")
	s := NewIPSCServer(cfg, nil)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	// A packet with auth enabled but only 10 bytes (not enough for payload + hash)
	data := make([]byte, 10)
	data[0] = byte(PacketType_MasterRegisterRequest)
	_, err := s.handlePacket(data, addr)
	if err == nil {
		t.Fatal("expected error for auth-enabled packet too short")
	}
}

func TestHandlePacketAuthEnabledBadHash(t *testing.T) {
	t.Parallel()
	cfg := testConfig(true, "1234")
	s := NewIPSCServer(cfg, nil)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	// Valid-length packet but bad hash
	payload := makeControlPacket(PacketType_MasterRegisterRequest, 12345)
	payload = append(payload, make([]byte, 10)...) // 10 zero bytes as bad hash
	data := payload
	_, err := s.handlePacket(data, addr)
	if err == nil {
		t.Fatal("expected auth failure error")
	}
}

func TestHandlePacketAuthEnabledSuccess(t *testing.T) {
	t.Parallel()
	hexKey := "0000000000000000000000000000000000001234"
	s, srvAddr := newTestServerWithUDP(t, true, "1234")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	peerID := uint32(99999)
	payload := makeControlPacket(PacketType_MasterRegisterRequest, peerID)
	data := signPacket(t, payload, hexKey)

	authAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	_, err = s.handlePacket(data, authAddr)
	if err != nil {
		t.Fatalf("expected auth success, got error: %v", err)
	}

	// Verify peer was registered
	s.mu.RLock()
	_, ok = s.peers[peerID]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected peer to be registered after authenticated request")
	}
}

func TestAuthDisabledAlwaysPasses(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	// When auth is disabled, no HMAC is needed
	data := makeControlPacket(PacketType_RepeaterWakeUp, 12345)
	registerPeer(t, s, 12345)
	_, err := s.handlePacket(data, addr)
	if err != nil {
		t.Fatalf("expected no error with auth disabled, got %v", err)
	}
}

// --- SendUserPacket tests ---

func TestSendUserPacketToMultiplePeers(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")

	// Create two client sockets to receive
	client1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client1 listen: %v", err)
	}
	defer client1.Close()

	client2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client2 listen: %v", err)
	}
	defer client2.Close()

	// Register two peers
	client1Addr, ok := client1.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	client2Addr, ok := client2.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	if err := s.upsertPeer(1, client1Addr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
	if err := s.upsertPeer(2, client2Addr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}

	payload := []byte("broadcast")
	s.SendUserPacket(t.Context(), payload)

	got1 := readUDP(t, client1)
	got2 := readUDP(t, client2)

	if string(got1) != "broadcast" {
		t.Fatalf("client1 expected 'broadcast', got %q", got1)
	}
	if string(got2) != "broadcast" {
		t.Fatalf("client2 expected 'broadcast', got %q", got2)
	}
}

func TestSendUserPacketWhenStopped(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	stoppedAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	if err := s.upsertPeer(1, stoppedAddr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
	s.stopped.Store(true)

	s.SendUserPacket(t.Context(), []byte("should not arrive"))

	// Try reading with a short timeout — should get nothing
	if err := client.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1500)
	_, _, err = client.ReadFromUDP(buf)
	if err == nil {
		t.Fatal("expected no data when server is stopped")
	}
}

func TestSendUserPacketNoPeers(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")

	// Should not panic with no peers
	s.SendUserPacket(t.Context(), []byte("no peers"))
}

func TestSendUserPacketSkipsNilAddrPeers(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	// Add a peer with nil addr
	s.mu.Lock()
	s.peers[999] = &Peer{ID: 999, Addr: nil}
	s.mu.Unlock()

	// Add a peer with a real addr
	clientAddrSelective, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	if err := s.upsertPeer(1, clientAddrSelective, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}

	s.SendUserPacket(t.Context(), []byte("selective"))

	got := readUDP(t, client)
	if string(got) != "selective" {
		t.Fatalf("expected 'selective', got %q", got)
	}
}

func TestSendUserPacketWithAuth(t *testing.T) {
	t.Parallel()
	hexKey := "0000000000000000000000000000000000005678"
	s, _ := newTestServerWithUDP(t, true, "5678")

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	clientAddrAuth, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	if err := s.upsertPeer(1, clientAddrAuth, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}

	payload := []byte("authenticated-broadcast")
	s.SendUserPacket(t.Context(), payload)

	got := readUDP(t, client)
	if len(got) != len(payload)+10 {
		t.Fatalf("expected %d bytes, got %d", len(payload)+10, len(got))
	}

	// Verify HMAC on received data
	authKey := mustDecodeHex(t, hexKey)
	h := hmac.New(sha1.New, authKey)
	h.Write(got[:len(got)-10])
	if !hmac.Equal(got[len(got)-10:], h.Sum(nil)[:10]) {
		t.Fatal("HMAC mismatch on broadcast packet")
	}
}

func TestSendUserPacketDataIsCopied(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	copyAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	if err := s.upsertPeer(1, copyAddr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}

	payload := []byte("original")
	s.SendUserPacket(t.Context(), payload)

	// Mutate original after sending — should not affect what was sent
	payload[0] = 'X'

	got := readUDP(t, client)
	if got[0] != 'o' {
		t.Fatal("SendUserPacket should copy data, not use original slice")
	}
}

// --- pacePeer tests ---

func TestPacePeerFirstCallNoDelay(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	start := time.Now()
	s.pacePeer(t.Context(), 1)
	elapsed := time.Since(start)

	// First call should not sleep
	if elapsed > 10*time.Millisecond {
		t.Fatalf("first pacePeer took too long: %v", elapsed)
	}
}

func TestPacePeerEnforcesInterval(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	// First call establishes the timestamp
	s.pacePeer(t.Context(), 1)

	// Second call immediately after should sleep ~30ms
	start := time.Now()
	s.pacePeer(t.Context(), 1)
	elapsed := time.Since(start)

	if elapsed < 20*time.Millisecond {
		t.Fatalf("expected pacePeer to sleep ~30ms, only waited %v", elapsed)
	}
}

func TestPacePeerSeparatePeersIndependent(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	// Pace peer 1
	s.pacePeer(t.Context(), 1)

	// Pace peer 2 immediately — should not be delayed by peer 1
	start := time.Now()
	s.pacePeer(t.Context(), 2)
	elapsed := time.Since(start)

	if elapsed > 10*time.Millisecond {
		t.Fatalf("pacing different peer shouldn't delay, took %v", elapsed)
	}
}

func TestPacePeerAfterInterval(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	s.pacePeer(t.Context(), 1)
	// Wait longer than the burst interval (30ms)
	time.Sleep(40 * time.Millisecond)

	start := time.Now()
	s.pacePeer(t.Context(), 1)
	elapsed := time.Since(start)

	// Should not need to sleep since interval has passed
	if elapsed > 10*time.Millisecond {
		t.Fatalf("pacePeer should not sleep after interval passed, took %v", elapsed)
	}
}

// --- handler() loop and Stop() tests ---

func TestHandlerLoopProcessesPackets(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	// Bind to loopback
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.udp = conn
	srvAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	// Start the handler goroutine
	s.wg.Add(1)
	go s.handler()

	// Send a registration request to the server
	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	peerID := uint32(22222)
	data := makeControlPacket(PacketType_MasterRegisterRequest, peerID)
	if _, err := client.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Poll for peer to appear (the handler processes asynchronously)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		_, ok := s.peers[peerID]
		s.mu.RUnlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	s.mu.RLock()
	_, found := s.peers[peerID]
	s.mu.RUnlock()
	if !found {
		t.Fatal("expected peer to be created by handler loop")
	}

	// Stop cleanly
	s.stopped.Store(true)
	conn.Close()
	s.wg.Wait()
}

func TestStopIdempotent(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.udp = conn

	s.wg.Add(1)
	go s.handler()

	// Calling Stop multiple times should not panic
	s.Stop()
	s.Stop()
	s.Stop()
}

func TestStopWithNilConn(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	// udp is nil — Stop should not panic
	s.Stop()
}

func TestHandlerLoopWithBurstHandler(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.udp = conn
	srvAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	var received atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	s.SetBurstHandler(func(packetType byte, data []byte, addr *net.UDPAddr) {
		defer wg.Done()
		received.Store(true)
	})

	registerPeer(t, s, 33333)
	s.wg.Add(1)
	go s.handler()

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	data := make([]byte, 54)
	data[0] = byte(PacketType_GroupVoice)
	binary.BigEndian.PutUint32(data[1:5], 33333)

	if _, err := client.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	wg.Wait()
	if !received.Load() {
		t.Fatal("burst handler was not called via handler loop")
	}

	s.stopped.Store(true)
	conn.Close()
	s.wg.Wait()
}

// --- buildPeerList edge cases ---

func TestBuildPeerListSkipsNilAddr(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	s.mu.Lock()
	s.peers[1] = &Peer{ID: 1, Addr: nil, Mode: 0x6A}
	s.peers[2] = &Peer{ID: 2, Addr: &net.UDPAddr{IP: nil, Port: 1234}, Mode: 0x6A}
	s.mu.Unlock()

	peerList := s.buildPeerList()
	// Both peers should be skipped (nil Addr, nil IP)
	if len(peerList) != 0 {
		t.Fatalf("expected empty peer list, got %d bytes", len(peerList))
	}
}

func TestBuildPeerListMultiplePeers(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	if err := s.upsertPeer(1, &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5000}, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
	if err := s.upsertPeer(2, &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 6000}, 0x6B, [4]byte{}); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}

	peerList := s.buildPeerList()
	// Each peer entry = 4 (ID) + 4 (IP) + 2 (port) + 1 (mode) = 11 bytes
	if len(peerList) != 22 {
		t.Fatalf("expected 22 bytes for 2 peers, got %d", len(peerList))
	}
}

// --- SetBurstHandler ---

func TestSetBurstHandler(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	if s.burstHandler != nil {
		t.Fatal("expected nil burst handler initially")
	}

	handler := func(packetType byte, data []byte, addr *net.UDPAddr) {}
	s.SetBurstHandler(handler)

	if s.burstHandler == nil {
		t.Fatal("expected non-nil burst handler after set")
	}
}

// --- Full registration → alive → peer list integration test ---

func TestFullRegistrationFlow(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	clientAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	peerID := uint32(44444)

	// Step 1: Register
	regReq := makeControlPacketWithModeFlags(PacketType_MasterRegisterRequest, peerID, 0x6A, [4]byte{0, 0, 0, 0x0D})
	_, err = s.handlePacket(regReq, clientAddr)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	regReply := readUDP(t, client)
	if regReply[0] != byte(PacketType_MasterRegisterReply) {
		t.Fatal("expected register reply")
	}

	// Step 2: Keep-alive
	aliveReq := makeControlPacket(PacketType_MasterAliveRequest, peerID)
	_, err = s.handlePacket(aliveReq, clientAddr)
	if err != nil {
		t.Fatalf("alive: %v", err)
	}
	aliveReply := readUDP(t, client)
	if aliveReply[0] != byte(PacketType_MasterAliveReply) {
		t.Fatal("expected alive reply")
	}

	// Step 3: Peer list
	peerListReq := makeControlPacket(PacketType_PeerListRequest, peerID)
	_, err = s.handlePacket(peerListReq, clientAddr)
	if err != nil {
		t.Fatalf("peer list: %v", err)
	}
	peerListReply := readUDP(t, client)
	if peerListReply[0] != byte(PacketType_PeerListReply) {
		t.Fatal("expected peer list reply")
	}

	// Verify peer state
	s.mu.RLock()
	peer := s.peers[peerID]
	s.mu.RUnlock()

	if peer == nil {
		t.Fatal("expected peer to exist")
	}
	if !peer.RegistrationStatus {
		t.Fatal("expected registered")
	}
	if peer.KeepAliveReceived < 1 {
		t.Fatal("expected at least 1 keepalive")
	}
	if peer.Mode != 0x6A {
		t.Fatalf("expected mode 0x6A, got 0x%02X", peer.Mode)
	}
}

// --- netlink error test ---

func TestNetlinkFailsBadInterface(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	cfg.IPSC.Interface = "nonexistent_iface_xyz"
	s := NewIPSCServer(cfg, nil)

	err := s.netlink()
	if err == nil {
		t.Fatal("expected netlink error for nonexistent interface")
	}
}

// --- handlePacket with MasterAliveRequest too short ---

func TestHandleMasterAliveRequestTooShort(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	data := []byte{byte(PacketType_MasterAliveRequest), 0x00, 0x01}
	aliveShortAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	_, err = s.handlePacket(data, aliveShortAddr)
	if err == nil {
		t.Fatal("expected error for too-short alive request")
	}
}

// --- handlePacket with MasterRegisterRequest too short for peer ID ---

func TestHandleMasterRegisterRequestTooShort(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	data := []byte{byte(PacketType_MasterRegisterRequest), 0x00}
	regShortAddr, ok := client.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	_, err = s.handlePacket(data, regShortAddr)
	if err == nil {
		t.Fatal("expected error for too-short register request")
	}
}

// --- Verify handlePacket returns Packet on success ---

func TestHandlePacketReturnsPacket(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	data := makeControlPacket(PacketType_RepeaterWakeUp, 12345)
	registerPeer(t, s, 12345)
	pkt, err := s.handlePacket(data, addr)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if pkt == nil {
		t.Fatal("expected non-nil packet")
	}
	if len(pkt.data) != len(data) {
		t.Fatalf("expected packet data length %d, got %d", len(data), len(pkt.data))
	}
}

// --- Verify handlePacket strips auth hash from returned data ---

func TestHandlePacketStripsAuthHash(t *testing.T) {
	t.Parallel()
	hexKey := "0000000000000000000000000000000000001234"
	cfg := testConfig(true, "1234")
	s := NewIPSCServer(cfg, nil)

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	payload := makeControlPacket(PacketType_RepeaterWakeUp, 12345)
	data := signPacket(t, payload, hexKey)

	registerPeer(t, s, 12345)
	pkt, err := s.handlePacket(data, addr)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	// The returned packet data should have the hash stripped
	if len(pkt.data) != len(payload) {
		t.Fatalf("expected data length %d (hash stripped), got %d", len(payload), len(pkt.data))
	}
}

// --- ErrPacketIgnored is a sentinel error ---

func TestErrPacketIgnoredIsSentinel(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	s := NewIPSCServer(cfg, nil)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	data := makeControlPacket(PacketType_MasterRegisterReply, 12345)
	_, err := s.handlePacket(data, addr)
	if !errors.Is(err, ErrPacketIgnored) {
		t.Fatalf("expected ErrPacketIgnored, got %v", err)
	}
}

func TestHandlerDeliversBurstsInOrder(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	const total = 3000
	var mu sync.Mutex
	var got []uint32
	s.SetBurstHandler(func(_ byte, data []byte, _ *net.UDPAddr) {
		mu.Lock()
		got = append(got, binary.BigEndian.Uint32(data[20:24]))
		mu.Unlock()
	})

	registerPeer(t, s, 1)
	s.wg.Add(1)
	go s.handler()

	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	for i := range total {
		data := make([]byte, 54)
		data[0] = byte(PacketType_GroupVoice)
		binary.BigEndian.PutUint32(data[1:5], 1)
		binary.BigEndian.PutUint32(data[20:24], uint32(i))
		if _, err := client.Write(data); err != nil {
			t.Fatalf("write: %v", err)
		}
		if i%50 == 0 {
			time.Sleep(time.Millisecond)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(got) > 0 && got[len(got)-1] == total-1
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	s.stopped.Store(true)
	s.udp.Close()
	s.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) < total/2 {
		t.Fatalf("delivered only %d of %d bursts", len(got), total)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("burst %d delivered after burst %d", got[i], got[i-1])
		}
	}
}

func TestSlowBurstHandlerDoesNotBlockKeepalives(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	s.SetBurstHandler(func(byte, []byte, *net.UDPAddr) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})

	s.wg.Add(1)
	go s.handler()

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer client.Close()

	const peerID = 4242
	if _, err := client.WriteToUDP(makeControlPacket(PacketType_MasterRegisterRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	if reply := readUDP(t, client); reply[0] != byte(PacketType_MasterRegisterReply) {
		t.Fatalf("expected register reply, got 0x%02X", reply[0])
	}

	voice := make([]byte, 54)
	voice[0] = byte(PacketType_GroupVoice)
	binary.BigEndian.PutUint32(voice[1:5], peerID)
	if _, err := client.WriteToUDP(voice, srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("burst handler was not called")
	}

	if _, err := client.WriteToUDP(makeControlPacket(PacketType_MasterAliveRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	if reply := readUDP(t, client); reply[0] != byte(PacketType_MasterAliveReply) {
		t.Fatalf("expected alive reply, got 0x%02X", reply[0])
	}

	close(release)
	s.stopped.Store(true)
	s.udp.Close()
	s.wg.Wait()
}

func TestBurstQueueFullDropsInsteadOfBlocking(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.SetBurstHandler(func(byte, []byte, *net.UDPAddr) {})

	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	data := make([]byte, 54)
	data[0] = byte(PacketType_GroupVoice)
	binary.BigEndian.PutUint32(data[1:5], 42)

	registerPeer(t, s, 42)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range burstQueueSize + 10 {
			if _, err := s.handlePacket(data, addr); err != nil {
				t.Errorf("handlePacket error: %v", err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handlePacket blocked on a full burst queue")
	}
	if len(s.bursts) != burstQueueSize {
		t.Fatalf("expected %d queued bursts, got %d", burstQueueSize, len(s.bursts))
	}
}

func TestPeersRegisteredGaugeCountsOnlyRegisteredPeers(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	s := NewIPSCServer(testConfig(false, ""), m)
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}

	_, _ = s.handlePacket(makeControlPacket(PacketType_RepeaterWakeUp, 42), addr)
	registerPeer(t, s, 7)

	if got := gaugeValue(t, m.IPSCPeersRegistered); got != 1 {
		t.Fatalf("expected 1 registered peer, gauge is %v", got)
	}
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(g)
	families, err := reg.Gather()
	if err != nil || len(families) != 1 || len(families[0].GetMetric()) != 1 {
		t.Fatalf("reading gauge: %v", err)
	}
	return families[0].GetMetric()[0].GetGauge().GetValue()
}

func expectNoUDP(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1500)
	if n, _, err := conn.ReadFromUDP(buf); err == nil {
		t.Fatalf("expected no datagram, got % X", buf[:n])
	}
}

func makeVoicePacket(peerID, marker uint32) []byte {
	data := make([]byte, 54)
	data[0] = byte(PacketType_GroupVoice)
	binary.BigEndian.PutUint32(data[1:5], peerID)
	binary.BigEndian.PutUint32(data[20:24], marker)
	return data
}

func TestUnregisteredPeerCreatesNoPeerAndGetsNoTraffic(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")

	victim, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer victim.Close()
	victimAddr, ok := victim.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	_, _ = s.handlePacket(makeControlPacket(PacketType_RepeaterWakeUp, 42), victimAddr)
	_, _ = s.handlePacket(makeVoicePacket(43, 0), victimAddr)

	if n := s.peerCount(); n != 0 {
		t.Fatalf("expected no peers from unregistered packets, got %d", n)
	}

	s.SendUserPacket(t.Context(), []byte("voice"))
	expectNoUDP(t, victim)
}

func TestSendUserPacketSkipsUnregisteredPeers(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")

	victim, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer victim.Close()
	victimAddr, ok := victim.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	s.mu.Lock()
	s.peers[42] = &Peer{ID: 42, Addr: victimAddr}
	s.mu.Unlock()

	s.SendUserPacket(t.Context(), []byte("voice"))
	expectNoUDP(t, victim)
}

func TestUnregisteredPeerVoiceIsNotBridged(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	var mu sync.Mutex
	var got []uint32
	delivered := make(chan struct{}, 10)
	s.SetBurstHandler(func(_ byte, data []byte, _ *net.UDPAddr) {
		mu.Lock()
		got = append(got, binary.BigEndian.Uint32(data[20:24]))
		mu.Unlock()
		delivered <- struct{}{}
	})

	s.wg.Add(1)
	go s.handler()

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer client.Close()

	const peerID = 77
	if _, err := client.WriteToUDP(makeVoicePacket(peerID, 1), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := client.WriteToUDP(makeControlPacket(PacketType_MasterRegisterRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUDP(t, client)
	if _, err := client.WriteToUDP(makeVoicePacket(peerID, 2), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("voice from registered peer was not bridged")
	}
	time.Sleep(100 * time.Millisecond)

	s.stopped.Store(true)
	s.udp.Close()
	s.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("expected only the registered peer's burst 2, got %v", got)
	}
}

func TestUnregisteredPeerKeepaliveGetsNoReply(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")

	s.wg.Add(1)
	go s.handler()

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer client.Close()

	const peerID = 88
	if _, err := client.WriteToUDP(makeControlPacket(PacketType_MasterAliveRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectNoUDP(t, client)
	if _, err := client.WriteToUDP(makeControlPacket(PacketType_PeerListRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectNoUDP(t, client)
	if n := s.peerCount(); n != 0 {
		t.Fatalf("expected no peers, got %d", n)
	}

	if _, err := client.WriteToUDP(makeControlPacket(PacketType_MasterRegisterRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	if reply := readUDP(t, client); reply[0] != byte(PacketType_MasterRegisterReply) {
		t.Fatalf("expected register reply, got 0x%02X", reply[0])
	}
	if _, err := client.WriteToUDP(makeControlPacket(PacketType_MasterAliveRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	if reply := readUDP(t, client); reply[0] != byte(PacketType_MasterAliveReply) {
		t.Fatalf("expected alive reply, got 0x%02X", reply[0])
	}

	s.stopped.Store(true)
	s.udp.Close()
	s.wg.Wait()
}

func TestRegistrationIsCappedAtMaxPeers(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}

	for id := uint32(1); id <= maxPeers+10; id++ {
		_ = s.upsertPeer(id, addr, 0x6A, [4]byte{})
	}
	if n := s.peerCount(); n != maxPeers {
		t.Fatalf("expected peer map capped at %d, got %d", maxPeers, n)
	}
	if err := s.upsertPeer(1, addr, 0x6A, [4]byte{}); err != nil {
		t.Fatalf("re-registering an existing peer failed: %v", err)
	}
}

func TestSilentPeerStopsGettingTraffic(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")

	victim, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer victim.Close()
	victimAddr, ok := victim.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	if err := s.upsertPeer(42, victimAddr, s.defaultModeByte(), s.defaultFlagsBytes()); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
	s.mu.Lock()
	s.peers[42].LastSeen = time.Now().Add(-peerTimeout - time.Second)
	s.mu.Unlock()

	s.SendUserPacket(t.Context(), []byte("voice"))
	expectNoUDP(t, victim)
}

func TestExpiredPeerKeepaliveIsRejected(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	registerPeer(t, s, 7)

	s.mu.Lock()
	s.peers[7].LastSeen = time.Now().Add(-peerTimeout - time.Second)
	s.mu.Unlock()
	if s.markPeerAlive(7) {
		t.Fatal("expected keepalive from a timed-out peer to require registration")
	}
}

func TestExpirePeersRemovesSilentPeers(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	s, _ := newTestServerWithUDP(t, false, "")
	s.metrics = m

	live, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer live.Close()
	liveAddr, ok := live.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}

	if err := s.upsertPeer(1, liveAddr, s.defaultModeByte(), s.defaultFlagsBytes()); err != nil {
		t.Fatalf("upsertPeer: %v", err)
	}
	registerPeer(t, s, 2)
	s.pacePeer(t.Context(), 2)

	now := time.Now()
	s.mu.Lock()
	s.peers[2].LastSeen = now.Add(-peerTimeout - time.Second)
	s.mu.Unlock()

	s.expirePeers(now)

	s.mu.RLock()
	_, stale := s.peers[2]
	_, staleSend := s.lastSend[2]
	_, kept := s.peers[1]
	s.mu.RUnlock()
	if stale || staleSend {
		t.Fatal("expected silent peer to be removed")
	}
	if !kept {
		t.Fatal("expected live peer to be kept")
	}
	if got := gaugeValue(t, m.IPSCPeersRegistered); got != 1 {
		t.Fatalf("expected gauge 1 after expiry, got %v", got)
	}

	reply := readUDP(t, live)
	if reply[0] != byte(PacketType_PeerListReply) {
		t.Fatalf("expected peer list broadcast, got 0x%02X", reply[0])
	}
	if n := binary.BigEndian.Uint16(reply[5:7]); n != 11 {
		t.Fatalf("expected one peer in the list, got %d bytes", n)
	}
	if id := binary.BigEndian.Uint32(reply[7:11]); id != 1 {
		t.Fatalf("expected peer 1 in the list, got %d", id)
	}
}

func TestKeepalivesPreventExpiry(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	registerPeer(t, s, 7)

	s.mu.Lock()
	s.peers[7].LastSeen = time.Now().Add(-peerTimeout + time.Second)
	s.mu.Unlock()
	if !s.markPeerAlive(7) {
		t.Fatal("markPeerAlive rejected a live peer")
	}

	s.expirePeers(time.Now().Add(2 * time.Second))
	if s.peerCount() != 1 {
		t.Fatal("expected peer with recent keepalive to be kept")
	}
}

func TestStopEndsMaintenance(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.wg.Add(1)
	go s.maintenance()

	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not end the maintenance goroutine")
	}
}

// pacedServer returns a server with one active peer that may not be sent
// to for an hour.
func pacedServer(t *testing.T) *IPSCServer {
	t.Helper()
	s, _ := newTestServerWithUDP(t, false, "")
	s.pace = time.Hour
	registerPeer(t, s, 1)
	s.mu.Lock()
	s.lastSend[1] = time.Now()
	s.mu.Unlock()
	return s
}

func requireReturns(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

func TestSendUserPacketGivesUpWhenContextIsDone(t *testing.T) {
	t.Parallel()
	s := pacedServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	requireReturns(t, "SendUserPacket", func() { s.SendUserPacket(ctx, []byte("x")) })
	requireReturns(t, "SendUserPacket", func() { s.SendUserPacket(ctx, []byte("x")) })
}

func TestSendUserPacketGivesUpWhenServerStops(t *testing.T) {
	t.Parallel()
	s := pacedServer(t)
	time.AfterFunc(20*time.Millisecond, s.Stop)
	requireReturns(t, "SendUserPacket", func() { s.SendUserPacket(t.Context(), []byte("x")) })
}
