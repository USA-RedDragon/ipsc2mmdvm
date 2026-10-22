package ipsc

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
	"github.com/USA-RedDragon/dmrgo/v2/transport/ipsc/ipscsession"
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

func mustDecodeHex(t *testing.T, hexStr string) []byte {
	t.Helper()
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestServerWithUDP(t *testing.T, authEnabled bool, authKey string) (*IPSCServer, *net.UDPAddr) {
	t.Helper()
	s := NewIPSCServer(testConfig(authEnabled, authKey), nil)
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

func listenPeer(t *testing.T) (*net.UDPConn, *net.UDPAddr) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr from LocalAddr")
	}
	return conn, addr
}

func control(t *testing.T, op ipscpkt.Opcode, id uint32) []byte {
	t.Helper()
	var flags ipscpkt.Flags
	flags.Set(ipscpkt.FlagVoice, true)
	flags.Set(ipscpkt.FlagData, true)
	version := ipscsession.RegisterVersionDefault()
	if op == ipscpkt.OpAliveRequest || op == ipscpkt.OpAliveReply {
		version = ipscsession.AliveVersionDefault()
	}
	b, err := (&ipscpkt.Control{Opcode: op, ID: id, Mode: 0x6A, Flags: flags, Version: version}).AppendBinary(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func signed(t *testing.T, s *IPSCServer, p []byte) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.master.Sign(slices.Clone(p))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func registerPeer(t *testing.T, s *IPSCServer, peerID uint32) *net.UDPConn {
	t.Helper()
	conn, addr := listenPeer(t)
	s.handlePacket(signed(t, s, control(t, ipscpkt.OpRegisterRequest, peerID)), addr)
	if reply := readUDP(t, conn); reply[0] != byte(ipscpkt.OpRegisterReply) {
		t.Fatalf("register reply 0x%02X", reply[0])
	}
	return conn
}

func peerIDs(s *IPSCServer) []uint32 {
	peers := s.activePeers()
	ids := make([]uint32, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, p.id)
	}
	slices.Sort(ids)
	return ids
}

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

func signPacket(t *testing.T, data []byte, hexKey string) []byte {
	t.Helper()
	h := hmac.New(sha1.New, mustDecodeHex(t, hexKey))
	h.Write(data)
	return append(data, h.Sum(nil)[:10]...)
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
	data[0] = byte(ipscpkt.OpGroupVoice)
	binary.BigEndian.PutUint32(data[1:5], peerID)
	binary.BigEndian.PutUint32(data[20:24], marker)
	return data
}

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

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil || len(families) != 1 || len(families[0].GetMetric()) != 1 {
		t.Fatalf("reading counter: %v", err)
	}
	return families[0].GetMetric()[0].GetCounter().GetValue()
}

func dropped(t *testing.T, m *metrics.Metrics, reason string) float64 {
	t.Helper()
	return counterValue(t, m.IPSCPacketsDropped.WithLabelValues(reason))
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

// TestUserPacketsReachBurstHandler sends each user packet type from a
// registered peer and checks that the burst handler gets a copy of it, and
// that a wake-up packet is not bridged.
func TestUserPacketsReachBurstHandler(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	startBurstWorker(t, s)
	got := make(chan []byte, 8)
	s.SetBurstHandler(func(packetType byte, data []byte, _ *net.UDPAddr) {
		if packetType != data[0] {
			t.Errorf("type 0x%02X for packet 0x%02X", packetType, data[0])
		}
		got <- data
	})
	conn := registerPeer(t, s, 42)
	addr, _ := conn.LocalAddr().(*net.UDPAddr)
	s.handlePacket(mustDecodeHex(t, "850000002a000000010002"), addr)
	for _, op := range []ipscpkt.Opcode{ipscpkt.OpGroupVoice, ipscpkt.OpPrivateVoice, ipscpkt.OpGroupData, ipscpkt.OpPrivateData} {
		data := makeVoicePacket(42, uint32(op))
		data[0] = byte(op)
		s.handlePacket(data, addr)
		data[20] = 0xFF
		select {
		case b := <-got:
			if b[0] != byte(op) || binary.BigEndian.Uint32(b[20:24]) != uint32(op) {
				t.Fatalf("handler got % X for opcode 0x%02X", b[:24], op)
			}
		case <-time.After(time.Second):
			t.Fatalf("opcode 0x%02X not bridged", op)
		}
	}
	select {
	case b := <-got:
		t.Fatalf("handler got % X", b)
	default:
	}
}

// TestAuthFailuresAreDroppedAndCounted sends a register request with a bad
// trailer and one that is too short for a trailer.
func TestAuthFailuresAreDroppedAndCounted(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	s, _ := newTestServerWithUDP(t, true, "1234")
	s.metrics = m
	conn, addr := listenPeer(t)
	bad := append(control(t, ipscpkt.OpRegisterRequest, 12345), make([]byte, 10)...)
	s.handlePacket(bad, addr)
	s.handlePacket([]byte{0x90, 0, 0}, addr)
	expectNoUDP(t, conn)
	if len(peerIDs(s)) != 0 {
		t.Fatalf("peers %v", peerIDs(s))
	}
	if got := counterValue(t, m.IPSCAuthFailures); got != 2 {
		t.Fatalf("auth failures %v, want 2", got)
	}
	if got := dropped(t, m, "auth"); got != 2 {
		t.Fatalf("dropped for auth %v, want 2", got)
	}
	s.handlePacket(signPacket(t, control(t, ipscpkt.OpRegisterRequest, 12345), "0000000000000000000000000000000000001234"), addr)
	if reply := readUDP(t, conn); reply[0] != byte(ipscpkt.OpRegisterReply) {
		t.Fatalf("reply 0x%02X", reply[0])
	}
}

// TestKeepaliveRegistersPeerWithAuth sends a master alive request, signed
// or not, from a peer that never registered. With authentication on, a
// keepalive with a valid HMAC registers the peer and gets the alive reply;
// one with a bad HMAC does nothing. With authentication off, a keepalive
// from an unregistered peer does nothing.
func TestKeepaliveRegistersPeerWithAuth(t *testing.T) {
	t.Parallel()
	const key = "00000000000000000000000000001234567890ab"
	const peerID = uint32(314926)
	alive := mustDecodeHex(t, "960004ce2e6a0000805c04020401")
	for _, tc := range []struct {
		name     string
		auth     bool
		sign     func([]byte) []byte
		register bool
	}{
		{"auth on, valid HMAC", true, func(b []byte) []byte { return signPacket(t, b, key) }, true},
		{"auth on, bad HMAC", true, func(b []byte) []byte {
			p := signPacket(t, b, key)
			p[len(p)-1] ^= 0x01
			return p
		}, false},
		{"auth off", false, func(b []byte) []byte { return b }, false},
	} {
		s, _ := newTestServerWithUDP(t, tc.auth, key)
		peer, peerAddr := listenPeer(t)
		s.handlePacket(tc.sign(slices.Clone(alive)), peerAddr)
		if got := slices.Equal(peerIDs(s), []uint32{peerID}); got != tc.register {
			t.Errorf("%s: peers %v", tc.name, peerIDs(s))
		}
		if !tc.register {
			expectNoUDP(t, peer)
			continue
		}
		s.mu.Lock()
		p := s.master.PeerAt(0)
		s.mu.Unlock()
		if udpAddr(p.Addr).String() != peerAddr.String() || p.Mode != 0x6A || p.Flags != [4]byte{0x00, 0x00, 0x80, 0x5C} {
			t.Errorf("%s: peer %+v", tc.name, p)
		}
		if reply := readUDP(t, peer); reply[0] != byte(ipscpkt.OpAliveReply) {
			t.Errorf("%s: reply type 0x%02X, want 0x%02X", tc.name, reply[0], ipscpkt.OpAliveReply)
		}
	}
}

func TestSendUserPacketToMultiplePeers(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	c1 := registerPeer(t, s, 1)
	c2 := registerPeer(t, s, 2)
	payload := []byte("broadcast")
	s.SendUserPacket(t.Context(), payload)
	payload[0] = 'X'
	for _, c := range []*net.UDPConn{c1, c2} {
		if got := readUDP(t, c); string(got) != "broadcast" {
			t.Fatalf("got %q", got)
		}
	}
}

func TestSendUserPacketWhenStopped(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	c := registerPeer(t, s, 1)
	s.stopped.Store(true)
	s.SendUserPacket(t.Context(), []byte("should not arrive"))
	expectNoUDP(t, c)
}

func TestSendUserPacketNoPeers(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	s.SendUserPacket(t.Context(), []byte("no peers"))
}

func TestSendUserPacketWithAuth(t *testing.T) {
	t.Parallel()
	const hexKey = "0000000000000000000000000000000000005678"
	s, _ := newTestServerWithUDP(t, true, "5678")
	c := registerPeer(t, s, 1)
	payload := []byte("authenticated-broadcast")
	s.SendUserPacket(t.Context(), payload)
	got := readUDP(t, c)
	if want := signPacket(t, slices.Clone(payload), hexKey); !slices.Equal(got, want) {
		t.Fatalf("got %x, want %x", got, want)
	}
}

func TestPacePeerFirstCallNoDelay(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.newTimer = func(d time.Duration) *time.Timer {
		t.Errorf("first pacePeer waited %v", d)
		return time.NewTimer(0)
	}
	if !s.pacePeer(t.Context(), 1) {
		t.Fatal("first pacePeer did not claim a send")
	}
}

func TestPacePeerEnforcesInterval(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.pacePeer(t.Context(), 1)
	first := s.lastSend[1]
	s.pacePeer(t.Context(), 1)
	if gap := s.lastSend[1].Sub(first); gap < s.pace {
		t.Fatalf("second send claimed %v after the first, want at least %v", gap, s.pace)
	}
	if elapsed := time.Since(first); elapsed < s.pace {
		t.Fatalf("pacePeer returned %v after the first send, want at least %v", elapsed, s.pace)
	}
}

func TestPacePeerSeparatePeersIndependent(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.pacePeer(t.Context(), 1)
	start := time.Now()
	s.pacePeer(t.Context(), 2)
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("pacing different peer shouldn't delay, took %v", elapsed)
	}
}

func TestPacePeerAfterInterval(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.pacePeer(t.Context(), 1)
	time.Sleep(40 * time.Millisecond)
	start := time.Now()
	s.pacePeer(t.Context(), 1)
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("pacePeer should not sleep after interval passed, took %v", elapsed)
	}
}

func TestHandlerLoopRegistersAndBridges(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")
	got := make(chan struct{}, 1)
	s.SetBurstHandler(func(byte, []byte, *net.UDPAddr) { got <- struct{}{} })
	s.wg.Add(1)
	go s.handler()
	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Write(control(t, ipscpkt.OpRegisterRequest, 22222)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1500)
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := client.Read(buf); err != nil || buf[0] != byte(ipscpkt.OpRegisterReply) {
		t.Fatalf("register reply % X, %v", buf[:n], err)
	}
	if _, err := client.Write(makeVoicePacket(22222, 0)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("burst handler was not called via handler loop")
	}
	s.stopped.Store(true)
	s.udp.Close()
	s.wg.Wait()
}

func TestStopIdempotent(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	s.wg.Add(1)
	go s.handler()
	s.Stop()
	s.Stop()
	s.Stop()
}

func TestStopWithNilConn(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.Stop()
}

func TestStartAfterStopFails(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.Stop()
	if err := s.Start(); !errors.Is(err, ErrServerStopped) {
		t.Fatalf("Start after Stop returned %v, want %v", err, ErrServerStopped)
	}
	requireReturns(t, "Stop", s.Stop)
}

func TestNetlinkFailsBadInterface(t *testing.T) {
	t.Parallel()
	cfg := testConfig(false, "")
	cfg.IPSC.Interface = "nonexistent_iface_xyz"
	s := NewIPSCServer(cfg, nil)
	if err := s.netlink(); err == nil {
		t.Fatal("expected netlink error for nonexistent interface")
	}
}

func TestHandlerDeliversBurstsInOrder(t *testing.T) {
	t.Parallel()
	s, srvAddr := newTestServerWithUDP(t, false, "")
	const (
		total  = 3000
		window = 32
	)
	delivered := make(chan uint32, total)
	s.SetBurstHandler(func(_ byte, data []byte, _ *net.UDPAddr) {
		delivered <- binary.BigEndian.Uint32(data[20:24])
	})
	s.wg.Add(1)
	go s.handler()
	defer func() {
		s.stopped.Store(true)
		s.udp.Close()
		s.wg.Wait()
	}()
	client, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Write(control(t, ipscpkt.OpRegisterRequest, 1)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(buf); err != nil {
		t.Fatal(err)
	}
	want := uint32(0)
	for sent := 0; sent < total; {
		batch := min(window, total-sent)
		for range batch {
			if _, err := client.Write(makeVoicePacket(1, uint32(sent))); err != nil { //nolint:gosec // below total
				t.Fatalf("write: %v", err)
			}
			sent++
		}
		for range batch {
			select {
			case got := <-delivered:
				if got != want {
					t.Fatalf("got burst %d, want burst %d", got, want)
				}
				want++
			case <-time.After(5 * time.Second):
				t.Fatalf("burst %d was not delivered", want)
			}
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
	client, _ := listenPeer(t)
	const peerID = 4242
	if _, err := client.WriteToUDP(control(t, ipscpkt.OpRegisterRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	if reply := readUDP(t, client); reply[0] != byte(ipscpkt.OpRegisterReply) {
		t.Fatalf("expected register reply, got 0x%02X", reply[0])
	}
	if _, err := client.WriteToUDP(makeVoicePacket(peerID, 0), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("burst handler was not called")
	}
	if _, err := client.WriteToUDP(control(t, ipscpkt.OpAliveRequest, peerID), srvAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	if reply := readUDP(t, client); reply[0] != byte(ipscpkt.OpAliveReply) {
		t.Fatalf("expected alive reply, got 0x%02X", reply[0])
	}
	close(release)
	s.stopped.Store(true)
	s.udp.Close()
	s.wg.Wait()
}

func TestBurstQueueFullDropsInsteadOfBlocking(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	s.SetBurstHandler(func(byte, []byte, *net.UDPAddr) {})
	conn := registerPeer(t, s, 42)
	addr, _ := conn.LocalAddr().(*net.UDPAddr)
	requireReturns(t, "handlePacket", func() {
		for range burstQueueSize + 10 {
			s.handlePacket(makeVoicePacket(42, 0), addr)
		}
	})
	if len(s.bursts) != burstQueueSize {
		t.Fatalf("expected %d queued bursts, got %d", burstQueueSize, len(s.bursts))
	}
}

func TestPeersRegisteredGauge(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	s, _ := newTestServerWithUDP(t, false, "")
	s.metrics = m
	_, addr := listenPeer(t)
	s.handlePacket(control(t, ipscpkt.OpAliveRequest, 42), addr)
	registerPeer(t, s, 7)
	registerPeer(t, s, 7)
	if got := gaugeValue(t, m.IPSCPeersRegistered); got != 1 {
		t.Fatalf("expected 1 registered peer, gauge is %v", got)
	}
}

func TestUnregisteredPeerCreatesNoPeerAndGetsNoTraffic(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	s, _ := newTestServerWithUDP(t, false, "")
	s.metrics = m
	victim, victimAddr := listenPeer(t)
	s.handlePacket(mustDecodeHex(t, "850000002a000000010002"), victimAddr)
	s.handlePacket(makeVoicePacket(43, 0), victimAddr)
	s.handlePacket(control(t, ipscpkt.OpAliveRequest, 44), victimAddr)
	s.handlePacket(control(t, ipscpkt.OpPeerListRequest, 45), victimAddr)
	s.handlePacket([]byte{0x90, 0, 0}, victimAddr)
	if ids := peerIDs(s); len(ids) != 0 {
		t.Fatalf("expected no peers from unregistered packets, got %v", ids)
	}
	if got := dropped(t, m, "unregistered"); got != 4 {
		t.Fatalf("dropped as unregistered %v, want 4", got)
	}
	if got := dropped(t, m, "malformed"); got != 1 {
		t.Fatalf("dropped as malformed %v, want 1", got)
	}
	s.SendUserPacket(t.Context(), []byte("voice"))
	expectNoUDP(t, victim)
}

func TestUnregisteredPeerVoiceIsNotBridged(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	startBurstWorker(t, s)
	got := make(chan uint32, 10)
	s.SetBurstHandler(func(_ byte, data []byte, _ *net.UDPAddr) {
		got <- binary.BigEndian.Uint32(data[20:24])
	})
	const peerID = 77
	_, addr := listenPeer(t)
	s.handlePacket(makeVoicePacket(peerID, 1), addr)
	conn := registerPeer(t, s, peerID)
	addr, _ = conn.LocalAddr().(*net.UDPAddr)
	s.handlePacket(makeVoicePacket(peerID, 2), addr)
	select {
	case m := <-got:
		if m != 2 {
			t.Fatalf("bridged burst %d, want only 2", m)
		}
	case <-time.After(time.Second):
		t.Fatal("voice from registered peer was not bridged")
	}
}

func TestRegistrationIsCappedAtMaxPeers(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	s, _ := newTestServerWithUDP(t, false, "")
	s.metrics = m
	_, addr := listenPeer(t)
	for id := uint32(1); id <= maxPeers+10; id++ {
		s.handlePacket(control(t, ipscpkt.OpRegisterRequest, id), addr)
	}
	if n := len(peerIDs(s)); n != maxPeers {
		t.Fatalf("expected peers capped at %d, got %d", maxPeers, n)
	}
	if got := dropped(t, m, "full"); got != 10 {
		t.Fatalf("dropped as full %v, want 10", got)
	}
}

// TestExpirePeersRemovesSilentPeers registers two peers on a fake clock,
// keeps one alive, and runs the timers past the timeout of the other. The
// silent peer goes, with its pacing state, and the peer left gets the new
// peer list.
func TestExpirePeersRemovesSilentPeers(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	s, _ := newTestServerWithUDP(t, false, "")
	s.metrics = m
	now := time.Now()
	s.now = func() time.Time { return now }
	live := registerPeer(t, s, 1)
	liveAddr, _ := live.LocalAddr().(*net.UDPAddr)
	registerPeer(t, s, 2)
	s.pacePeer(t.Context(), 2)

	now = now.Add(ipscsession.TimeoutDefault / 2)
	s.handlePacket(control(t, ipscpkt.OpAliveRequest, 1), liveAddr)
	readUDP(t, live)
	now = now.Add(ipscsession.TimeoutDefault/2 + time.Second)
	s.tick()

	if ids := peerIDs(s); !slices.Equal(ids, []uint32{1}) {
		t.Fatalf("peers %v, want [1]", ids)
	}
	s.mu.Lock()
	_, staleSend := s.lastSend[2]
	s.mu.Unlock()
	if staleSend {
		t.Fatal("pacing state of the silent peer kept")
	}
	if got := gaugeValue(t, m.IPSCPeersRegistered); got != 1 {
		t.Fatalf("expected gauge 1 after expiry, got %v", got)
	}
	reply := readUDP(t, live)
	if reply[0] != byte(ipscpkt.OpPeerListReply) {
		t.Fatalf("expected peer list broadcast, got 0x%02X", reply[0])
	}
	if n := binary.BigEndian.Uint16(reply[5:7]); n != 11 {
		t.Fatalf("expected one peer in the list, got %d bytes", n)
	}
	if id := binary.BigEndian.Uint32(reply[7:11]); id != 1 {
		t.Fatalf("expected peer 1 in the list, got %d", id)
	}
}

// TestMaintenanceRunsTheMasterTimers starts maintenance with no peers and
// registers one. The registration alone must re-arm the timer for the
// timeout of the new peer, and the timer must drop the silent peer.
func TestMaintenanceRunsTheMasterTimers(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	var mu sync.Mutex
	now := time.Now()
	s.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	waits := make(chan time.Duration, 4)
	fire := make(chan time.Time, 1)
	s.armTimer = func(d time.Duration) *time.Timer {
		waits <- d
		tm := time.NewTimer(time.Hour)
		if d == ipscsession.TimeoutDefault {
			tm.C = fire
		}
		return tm
	}
	s.wg.Add(1)
	go s.maintenance()
	defer func() {
		s.stopOnce.Do(func() { close(s.done) })
		s.wg.Wait()
	}()
	if d := <-waits; d != time.Hour {
		t.Fatalf("first wait %v, want an hour", d)
	}
	registerPeer(t, s, 9)
	select {
	case d := <-waits:
		if d != ipscsession.TimeoutDefault {
			t.Fatalf("wait after registration %v, want %v", d, ipscsession.TimeoutDefault)
		}
	case <-time.After(time.Second):
		t.Fatal("registration did not re-arm the timer")
	}
	mu.Lock()
	now = now.Add(ipscsession.TimeoutDefault)
	mu.Unlock()
	fire <- time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for len(peerIDs(s)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("silent peer was not dropped")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPeerListGoesToEveryPeer checks that a peer list request from one
// peer sends the list to every registered peer, as DMRlink does.
func TestPeerListGoesToEveryPeer(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, false, "")
	c1 := registerPeer(t, s, 1)
	c2 := registerPeer(t, s, 2)
	addr, _ := c1.LocalAddr().(*net.UDPAddr)
	s.handlePacket(control(t, ipscpkt.OpPeerListRequest, 1), addr)
	for _, c := range []*net.UDPConn{c1, c2} {
		got := readUDP(t, c)
		if got[0] != byte(ipscpkt.OpPeerListReply) || binary.BigEndian.Uint16(got[5:7]) != 22 {
			t.Fatalf("got %x, want the list of two peers", got)
		}
	}
}

// TestSignedUserPacketIsBridgedWithoutTrailer sends a signed voice packet
// with authentication on and checks that the burst handler gets it without
// the trailer.
func TestSignedUserPacketIsBridgedWithoutTrailer(t *testing.T) {
	t.Parallel()
	const key = "0000000000000000000000000000000000001234"
	s, _ := newTestServerWithUDP(t, true, key)
	startBurstWorker(t, s)
	got := make(chan []byte, 1)
	s.SetBurstHandler(func(_ byte, data []byte, _ *net.UDPAddr) { got <- data })
	conn := registerPeer(t, s, 42)
	addr, _ := conn.LocalAddr().(*net.UDPAddr)
	voice := makeVoicePacket(42, 5)
	s.handlePacket(signPacket(t, slices.Clone(voice), key), addr)
	select {
	case b := <-got:
		if !slices.Equal(b, voice) {
			t.Fatalf("bridged %x, want %x", b, voice)
		}
	case <-time.After(time.Second):
		t.Fatal("signed voice was not bridged")
	}
}

func TestStopEndsMaintenance(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.wg.Add(1)
	go s.maintenance()
	requireReturns(t, "Stop", s.Stop)
}

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

// TestPacePeerSpacesConcurrentSenders starts two senders to one peer at
// once, on a fake clock, right after a send to the peer. They get send
// times one and two paces later, whichever comes first.
func TestPacePeerSpacesConcurrentSenders(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	s.pace = 50 * time.Millisecond
	now := time.Unix(1, 0)
	s.now = func() time.Time { return now }
	s.lastSend[1] = now
	waits := make(chan time.Duration, 2)
	s.newTimer = func(d time.Duration) *time.Timer {
		waits <- d
		return time.NewTimer(0)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if !s.pacePeer(t.Context(), 1) {
				t.Error("pacePeer gave up")
			}
		})
	}
	wg.Wait()
	got := []time.Duration{<-waits, <-waits}
	slices.Sort(got)
	if got[0] != s.pace || got[1] != 2*s.pace {
		t.Fatalf("senders wait %v, want %v and %v", got, s.pace, 2*s.pace)
	}
}

// TestPacePeerIsFIFO starts three senders to one peer, one after another,
// on a fake clock while a send to the peer was just claimed. Each must wait
// one pace longer than the one before it, in the order they came, so a
// sender that keeps sending cannot hold off one that came after it.
func TestPacePeerIsFIFO(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	now := time.Unix(1, 0)
	s.now = func() time.Time { return now }
	s.lastSend[1] = now
	waits := make(chan time.Duration)
	s.newTimer = func(d time.Duration) *time.Timer {
		waits <- d
		return time.NewTimer(0)
	}
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Go(func() {
			if !s.pacePeer(t.Context(), 1) {
				t.Error("pacePeer gave up")
			}
		})
		if d := <-waits; d != time.Duration(i)*s.pace {
			t.Errorf("sender %d waits %v, want %v", i, d, time.Duration(i)*s.pace)
		}
	}
	wg.Wait()
}

func TestPaceDrainsTwoBusySlots(t *testing.T) {
	t.Parallel()
	s := NewIPSCServer(testConfig(false, ""), nil)
	const (
		frame     = 60 * time.Millisecond
		overshoot = time.Millisecond
	)
	start := time.Now()
	arrivals := make([]time.Time, 0, 606)
	for range 3 {
		arrivals = append(arrivals, start, start)
	}
	for i := range 300 {
		at := start.Add(time.Duration(i) * frame)
		arrivals = append(arrivals, at, at)
	}
	var lag time.Duration
	for _, arrived := range arrivals {
		lag = s.claimSendLocked(1, arrived).Add(overshoot).Sub(arrived)
	}
	if lag > 2*s.pace {
		t.Fatalf("last packet sent %v after it arrived, want at most %v", lag, 2*s.pace)
	}
}

// TestDeRegisterRequestRemovesPeer sends the 5-byte de-registration request
// (0x9A and the peer ID) that DMRlink and ipsc2hbpc send. Only a request
// from the address the peer registered from removes it. No reply is sent,
// as in DMRlink, and the peer left gets the new peer list.
func TestDeRegisterRequestRemovesPeer(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	s, _ := newTestServerWithUDP(t, false, "")
	s.metrics = m
	client := registerPeer(t, s, 314926)
	peerAddr, _ := client.LocalAddr().(*net.UDPAddr)
	left := registerPeer(t, s, 7)

	req := mustDecodeHex(t, "9a0004ce2e")
	s.handlePacket(req, &net.UDPAddr{IP: peerAddr.IP, Port: peerAddr.Port + 1})
	if ids := peerIDs(s); !slices.Equal(ids, []uint32{7, 314926}) {
		t.Fatalf("peers %v after a request from another address", ids)
	}
	if got := dropped(t, m, "address"); got != 1 {
		t.Fatalf("dropped for address %v, want 1", got)
	}
	s.handlePacket(req, peerAddr)
	if ids := peerIDs(s); !slices.Equal(ids, []uint32{7}) {
		t.Fatalf("peers %v, want [7]", ids)
	}
	if got := gaugeValue(t, m.IPSCPeersRegistered); got != 1 {
		t.Fatalf("registered peers gauge is %v, want 1", got)
	}
	got := readUDP(t, left)
	if got[0] != byte(ipscpkt.OpPeerListReply) || binary.BigEndian.Uint16(got[5:7]) != 11 || binary.BigEndian.Uint32(got[7:11]) != 7 {
		t.Fatalf("peer left got %x, want the peer list of peer 7", got)
	}
	expectNoUDP(t, client)
}

// TestStopDeRegistersFromPeers checks that Stop sends each registered peer
// the de-registration request DMRlink sends on shutdown: 0x9A and the local
// ID, with one HMAC when authentication is on.
func TestStopDeRegistersFromPeers(t *testing.T) {
	t.Parallel()
	const key = "00000000000000000000000000001234567890ab"
	for _, authEnabled := range []bool{false, true} {
		s := NewIPSCServer(testConfig(authEnabled, key), nil)
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		s.udp = conn
		peer := registerPeer(t, s, 314926)
		s.Stop()
		want := mustDecodeHex(t, "9a0004c234")
		if authEnabled {
			want = signPacket(t, want, key)
		}
		if got := readUDP(t, peer); hex.EncodeToString(got) != hex.EncodeToString(want) {
			t.Errorf("auth %v: got %x, want %x", authEnabled, got, want)
		}
	}
}
