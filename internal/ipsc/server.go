package ipsc

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/config"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/vishvananda/netlink"
)

type IPSCServer struct {
	cfg     *config.Config
	metrics *metrics.Metrics
	udp     *net.UDPConn
	mu      sync.RWMutex

	localID  uint32
	authKey  []byte // 20-byte HMAC key decoded from hex
	peers    map[uint32]*Peer
	lastSend map[uint32]time.Time
	pace     time.Duration // minimum gap between packets to one peer

	burstHandler func(packetType byte, data []byte, addr *net.UDPAddr)
	bursts       chan burst

	lifeMu   sync.Mutex
	wg       sync.WaitGroup
	stopped  atomic.Bool
	stopOnce sync.Once
	done     chan struct{}
}

type Packet struct {
	data []byte
}

type burst struct {
	packetType byte
	data       []byte
	addr       *net.UDPAddr
}

// burstQueueSize bounds the user packets waiting for the burst handler, so a
// stalled MMDVM side drops voice instead of blocking keepalive replies.
const burstQueueSize = 256

type Peer struct {
	ID                 uint32
	Addr               *net.UDPAddr
	Mode               byte
	Flags              [4]byte
	LastSeen           time.Time
	KeepAliveReceived  uint64
	RegistrationStatus bool
}

type PacketType byte

const (
	PacketType_GroupVoice            PacketType = 0x80
	PacketType_PrivateVoice          PacketType = 0x81
	PacketType_GroupData             PacketType = 0x83
	PacketType_PrivateData           PacketType = 0x84
	PacketType_RepeaterWakeUp        PacketType = 0x85
	PacketType_MasterRegisterRequest PacketType = 0x90
	PacketType_MasterRegisterReply   PacketType = 0x91
	PacketType_PeerListRequest       PacketType = 0x92
	PacketType_PeerListReply         PacketType = 0x93
	PacketType_MasterAliveRequest    PacketType = 0x96
	PacketType_MasterAliveReply      PacketType = 0x97
	PacketType_DeRegisterRequest     PacketType = 0x9A
	PacketType_DeRegisterReply       PacketType = 0x9B
	// PacketType_RepeaterStatus is sent by an XPR 8400 right after each
	// master register or keepalive reply: the peer ID and four more bytes,
	// zero or a value like 56 63 11 00. Its meaning is unknown and it needs
	// no reply.
	PacketType_RepeaterStatus PacketType = 0xF0
)

var (
	//nolint:gochecknoglobals
	ipscVersion = []byte{0x04, 0x02, 0x04, 0x01}
)

var (
	ErrPacketIgnored    = errors.New("packet ignored")
	ErrUnregisteredPeer = errors.New("peer is not registered")
	ErrTooManyPeers     = errors.New("too many registered peers")
	ErrServerStopped    = errors.New("server stopped")
)

// maxPeers bounds the peer map so registrations with random peer IDs cannot
// grow it without limit. Motorola IPSC networks allow at most 15 peers.
const maxPeers = 64

const (
	// peerKeepAliveInterval is how often IPSC peers send keepalives, and how
	// often the server checks for silent peers.
	peerKeepAliveInterval = 5 * time.Second
	// peerTimeout is how long a registered peer may stay silent before it is
	// removed. It matches the 120 second timeout DMRlink uses as a master.
	peerTimeout = 24 * peerKeepAliveInterval
	// defaultPace is the minimum gap between user packets sent to one peer.
	// Two busy timeslots need one packet every 30 ms plus repeated voice
	// headers, so the pace must stay well under 30 ms or a queue builds.
	defaultPace = 15 * time.Millisecond
)

func NewIPSCServer(cfg *config.Config, m *metrics.Metrics) *IPSCServer {
	// Decode the auth key from hex string to raw bytes.
	// DMRlink left-pads the hex key to 40 characters (20 bytes) with zeros.
	var authKey []byte
	if cfg.IPSC.Auth.Enabled && cfg.IPSC.Auth.Key != "" {
		hexKey := cfg.IPSC.Auth.Key
		// Left-pad with zeros to 40 hex characters (20 bytes)
		for len(hexKey) < 40 {
			hexKey = "0" + hexKey
		}
		var err error
		authKey, err = hex.DecodeString(hexKey)
		if err != nil {
			slog.Error("failed to decode IPSC auth key as hex, using raw string", "error", err)
			authKey = []byte(cfg.IPSC.Auth.Key)
		}
	}

	// Use the first MMDVM network's ID as the local peer identity.
	var localID uint32
	if len(cfg.MMDVM) > 0 {
		localID = cfg.MMDVM[0].ID
	}

	return &IPSCServer{
		cfg:      cfg,
		metrics:  m,
		localID:  localID,
		authKey:  authKey,
		peers:    map[uint32]*Peer{},
		lastSend: map[uint32]time.Time{},
		pace:     defaultPace,
		bursts:   make(chan burst, burstQueueSize),
		done:     make(chan struct{}),
	}
}

// Start configures the interface and starts serving. It returns
// ErrServerStopped once Stop has been called.
func (s *IPSCServer) Start() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.stopped.Load() {
		return ErrServerStopped
	}

	if err := s.netlink(); err != nil {
		return fmt.Errorf("error configuring network: %w", err)
	}

	var err error
	s.udp, err = net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP(s.cfg.IPSC.IP),
		Port: int(s.cfg.IPSC.Port),
	})

	if err != nil {
		return fmt.Errorf("error starting UDP listener: %w", err)
	}

	s.wg.Add(2)
	go s.handler()
	go s.maintenance()

	return nil
}

func (s *IPSCServer) Stop() {
	s.stopOnce.Do(func() {
		slog.Info("Stopping IPSC server")
		s.lifeMu.Lock()
		defer s.lifeMu.Unlock()
		s.stopped.Store(true)
		close(s.done)
		if s.udp != nil {
			if err := s.udp.Close(); err != nil {
				slog.Error("error closing UDP listener", "error", err)
			}
		}
	})
	s.wg.Wait()
}

func (s *IPSCServer) netlink() error {
	link, err := netlink.LinkByName(s.cfg.IPSC.Interface)
	if err != nil {
		return fmt.Errorf("cannot find interface %s: %w", s.cfg.IPSC.Interface, err)
	}

	// Remove any existing addresses from the interface
	existingAddrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("cannot list addresses on interface %s: %w", s.cfg.IPSC.Interface, err)
	}
	for i := range existingAddrs {
		if err := netlink.AddrDel(link, &existingAddrs[i]); err != nil {
			return fmt.Errorf("cannot remove address %s from interface %s: %w", existingAddrs[i].IPNet, s.cfg.IPSC.Interface, err)
		}
	}

	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(s.cfg.IPSC.IP), Mask: net.CIDRMask(s.cfg.IPSC.SubnetMask, 32)}}); err != nil {
		return fmt.Errorf("cannot add IP address to interface %s: %w", s.cfg.IPSC.Interface, err)
	}

	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("cannot set interface up %s: %w", s.cfg.IPSC.Interface, err)
		}
	}

	return nil
}

func (s *IPSCServer) handler() {
	defer s.wg.Done()

	quit := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s.burstWorker(quit)
	}()
	defer func() {
		close(quit)
		<-workerDone
	}()

	buf := make([]byte, 1500)
	for {
		n, addr, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if s.metrics != nil {
				s.metrics.IPSCUDPErrors.WithLabelValues("read").Inc()
			}
			slog.Warn("error reading from UDP", "error", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		packet, err := s.handlePacket(data, addr)
		if err != nil {
			switch {
			case errors.Is(err, ErrPacketIgnored):
			case errors.Is(err, ErrUnregisteredPeer):
				slog.Debug("dropped packet from unregistered peer", "peer", addr, "error", err)
			default:
				slog.Warn("error parsing packet", "peer", addr, "error", err, "length", len(data), "packet", data)
			}
			continue
		}
		slog.Debug("received packet", "peer", addr, "length", len(data), "packet", packet)
	}
}

// burstWorker delivers queued user packets to the burst handler one at a time,
// in the order they were received, until quit is closed.
func (s *IPSCServer) burstWorker(quit <-chan struct{}) {
	for {
		select {
		case <-quit:
			return
		case b := <-s.bursts:
			if s.burstHandler != nil {
				s.burstHandler(b.packetType, b.data, b.addr)
			}
		}
	}
}

func (s *IPSCServer) handlePacket(data []byte, addr *net.UDPAddr) (*Packet, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("packet too short")
	}

	packetType := data[0]

	if s.cfg.IPSC.Auth.Enabled {
		if len(data) <= 10 {
			return nil, fmt.Errorf("packet too short for authentication")
		}
		if !s.auth(data) {
			if s.metrics != nil {
				s.metrics.IPSCAuthFailures.Inc()
			}
			return nil, fmt.Errorf("authentication failed")
		}
		data = data[:len(data)-10] // Remove the hash from the data
	}

	switch PacketType(packetType) {
	case PacketType_GroupVoice:
		s.countReceived("group_voice")
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_PrivateVoice:
		s.countReceived("private_voice")
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_GroupData:
		s.countReceived("group_data")
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_PrivateData:
		s.countReceived("private_data")
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_RepeaterWakeUp:
		s.countReceived("wake_up")
		if err := s.handleRepeaterWakeUp(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterRegisterRequest:
		s.countReceived("register")
		if err := s.handleMasterRegisterRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterAliveRequest:
		s.countReceived("alive")
		if err := s.handleMasterAliveRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_PeerListRequest:
		s.countReceived("peer_list")
		if err := s.handlePeerListRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_DeRegisterRequest:
		s.countReceived("de_register")
		if err := s.handleDeRegisterRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_RepeaterStatus:
		s.countReceived("repeater_status")
		return nil, ErrPacketIgnored
	case PacketType_MasterRegisterReply, PacketType_PeerListReply, PacketType_MasterAliveReply,
		PacketType_DeRegisterReply:
		// These are reply packets, we shouldn't receive them as a server, keeping quiet.
		return nil, ErrPacketIgnored
	default:
		s.countReceived("other")
		return nil, fmt.Errorf("unknown packet type: %d", packetType)
	}

	return &Packet{data: data}, nil
}

// countReceived increments the received packet counter for packetType.
func (s *IPSCServer) countReceived(packetType string) {
	if s.metrics != nil {
		s.metrics.IPSCPacketsReceived.WithLabelValues(packetType).Inc()
	}
}

func (s *IPSCServer) handleMasterRegisterRequest(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	mode := s.defaultModeByte()
	flags := s.defaultFlagsBytes()
	if len(data) >= 10 {
		mode = data[5]
		copy(flags[:], data[6:10])
	}

	if err := s.upsertPeer(peerID, addr, mode, flags); err != nil {
		return err
	}

	packet := &Packet{data: s.buildMasterRegisterReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending master register reply: %w", err)
	}

	return nil
}

func (s *IPSCServer) handleMasterAliveRequest(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	if !s.markPeerAlive(peerID) {
		return fmt.Errorf("keepalive from peer %d: %w", peerID, ErrUnregisteredPeer)
	}

	packet := &Packet{data: s.buildMasterAliveReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending master alive reply: %w", err)
	}

	return nil
}

func (s *IPSCServer) handlePeerListRequest(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}
	if !s.isRegistered(peerID) {
		return fmt.Errorf("peer list request from peer %d: %w", peerID, ErrUnregisteredPeer)
	}

	packet := &Packet{data: s.buildPeerListReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending peer list reply: %w", err)
	}

	return nil
}

// handleDeRegisterRequest removes a peer that leaves the network, as
// DMRlink's de_register_peer does, and sends the new peer list to the peers
// left, as ipsc2hbpc's remove_peer does. DMRlink sends no new list here, but
// it sends one to every peer whenever a peer asks for it, where this server
// answers only the peer that asks, so without it the peers would keep the
// departed one. Like DMRlink, it sends no reply. The request must come from
// the address the peer registered from.
func (s *IPSCServer) handleDeRegisterRequest(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	peer, ok := s.peers[peerID]
	if !ok || peer.Addr == nil || addr == nil || !peer.Addr.IP.Equal(addr.IP) || peer.Addr.Port != addr.Port {
		s.mu.Unlock()
		return fmt.Errorf("de-registration from peer %d: %w", peerID, ErrUnregisteredPeer)
	}
	delete(s.peers, peerID)
	delete(s.lastSend, peerID)
	s.updatePeersGaugeLocked()
	s.mu.Unlock()
	slog.Info("IPSC peer de-registered", "peerID", peerID, "peer", addr)
	s.sendPeerList(time.Now())
	return nil
}

func (s *IPSCServer) handleRepeaterWakeUp(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	if !s.markPeerAlive(peerID) {
		return fmt.Errorf("wake-up from peer %d: %w", peerID, ErrUnregisteredPeer)
	}
	slog.Debug("repeater wake-up packet received", "peer", addr, "peerID", peerID, "length", len(data))
	return nil
}

func (s *IPSCServer) handleUserPacket(packetType PacketType, data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	if !s.markPeerAlive(peerID) {
		return fmt.Errorf("user packet from peer %d: %w", peerID, ErrUnregisteredPeer)
	}
	slog.Debug("IPSC burst received", "peer", addr, "peerID", peerID, "packetType", byte(packetType), "length", len(data))
	if s.burstHandler != nil {
		packetCopy := make([]byte, len(data))
		copy(packetCopy, data)
		select {
		case s.bursts <- burst{packetType: byte(packetType), data: packetCopy, addr: addr}:
		default:
			slog.Warn("IPSC burst queue full, dropping packet", "peer", addr, "peerID", peerID)
		}
	}
	return nil
}

func (s *IPSCServer) SetBurstHandler(handler func(packetType byte, data []byte, addr *net.UDPAddr)) {
	s.burstHandler = handler
}

func (s *IPSCServer) upsertPeer(peerID uint32, addr *net.UDPAddr, mode byte, flags [4]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	peer, ok := s.peers[peerID]
	if !ok {
		if len(s.peers) >= maxPeers {
			return fmt.Errorf("registration from peer %d: %w", peerID, ErrTooManyPeers)
		}
		peer = &Peer{ID: peerID}
		s.peers[peerID] = peer
	}
	peer.Addr = cloneUDPAddr(addr)
	peer.Mode = mode
	peer.Flags = flags
	peer.LastSeen = time.Now()
	peer.RegistrationStatus = true
	s.updatePeersGaugeLocked()
	return nil
}

// updatePeersGaugeLocked sets the registered-peers gauge. s.mu must be held.
func (s *IPSCServer) updatePeersGaugeLocked() {
	if s.metrics == nil {
		return
	}
	registered := 0
	for _, peer := range s.peers {
		if peer.RegistrationStatus {
			registered++
		}
	}
	s.metrics.IPSCPeersRegistered.Set(float64(registered))
}

// maintenance removes expired peers every keepalive interval until Stop.
func (s *IPSCServer) maintenance() {
	defer s.wg.Done()
	ticker := time.NewTicker(peerKeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			s.expirePeers(now)
		}
	}
}

// expirePeers removes peers that have been silent for longer than
// peerTimeout and, like DMRlink, sends the new peer list to the peers left.
func (s *IPSCServer) expirePeers(now time.Time) {
	s.mu.Lock()
	var expired []uint32
	for id, peer := range s.peers {
		if !peerActive(peer, now) {
			expired = append(expired, id)
			delete(s.peers, id)
			delete(s.lastSend, id)
		}
	}
	if len(expired) > 0 {
		s.updatePeersGaugeLocked()
	}
	s.mu.Unlock()

	if len(expired) == 0 {
		return
	}
	slog.Warn("IPSC peers timed out, de-registering", "peerIDs", expired, "timeout", peerTimeout)
	s.sendPeerList(now)
}

// sendPeerList sends the peer list to every active peer.
func (s *IPSCServer) sendPeerList(now time.Time) {
	reply := s.buildPeerListReply()
	for _, target := range s.activePeers(now) {
		packet := &Packet{data: append([]byte(nil), reply...)}
		if err := s.sendPacket(packet, target.addr); err != nil {
			slog.Warn("failed sending IPSC peer list", "peer", target.addr, "error", err)
		}
	}
}

// peerActive reports whether peer is registered and has not timed out.
func peerActive(peer *Peer, now time.Time) bool {
	return peer.RegistrationStatus && now.Sub(peer.LastSeen) <= peerTimeout
}

type peerTarget struct {
	id   uint32
	addr *net.UDPAddr
}

// activePeers returns the ID and a copy of the address of every active peer.
func (s *IPSCServer) activePeers(now time.Time) []peerTarget {
	s.mu.RLock()
	defer s.mu.RUnlock()
	targets := make([]peerTarget, 0, len(s.peers))
	for _, peer := range s.peers {
		if peerActive(peer, now) && peer.Addr != nil {
			targets = append(targets, peerTarget{id: peer.ID, addr: cloneUDPAddr(peer.Addr)})
		}
	}
	return targets
}

// markPeerAlive records activity from a registered peer. It returns false,
// and changes nothing, when the peer has not registered.
func (s *IPSCServer) markPeerAlive(peerID uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	peer, ok := s.peers[peerID]
	if !ok || !peerActive(peer, now) {
		return false
	}
	peer.LastSeen = now
	peer.KeepAliveReceived++
	return true
}

func (s *IPSCServer) isRegistered(peerID uint32) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peer, ok := s.peers[peerID]
	return ok && peerActive(peer, time.Now())
}

func (s *IPSCServer) buildMasterRegisterReply() []byte {
	packet := make([]byte, 0, 1+4+5+2+4)
	packet = append(packet, byte(PacketType_MasterRegisterReply))
	packet = append(packet, s.localIDBytes()...)
	packet = append(packet, s.defaultModeByte())
	flags := s.defaultFlagsBytes()
	packet = append(packet, flags[:]...)

	numPeers := s.peerCount()
	if numPeers > math.MaxUint16 {
		numPeers = math.MaxUint16
	}
	packet = append(packet, uint16ToBytes(uint16(numPeers))...) //nolint:gosec // Bounds checked
	packet = append(packet, ipscVersion...)
	return packet
}

func (s *IPSCServer) buildMasterAliveReply() []byte {
	packet := make([]byte, 0, 1+4+5+4)
	packet = append(packet, byte(PacketType_MasterAliveReply))
	packet = append(packet, s.localIDBytes()...)
	packet = append(packet, s.defaultModeByte())
	flags := s.defaultFlagsBytes()
	packet = append(packet, flags[:]...)
	packet = append(packet, ipscVersion...)
	return packet
}

func (s *IPSCServer) buildPeerListReply() []byte {
	peerList := s.buildPeerList()
	packet := make([]byte, 0, 1+4+2+len(peerList))
	packet = append(packet, byte(PacketType_PeerListReply))
	packet = append(packet, s.localIDBytes()...)
	if len(peerList) > math.MaxUint16 {
		packet = append(packet, uint16ToBytes(math.MaxUint16)...)
	} else {
		packet = append(packet, uint16ToBytes(uint16(len(peerList)))...) //nolint:gosec // Bounds checked
	}
	packet = append(packet, peerList...)
	return packet
}

func (s *IPSCServer) buildPeerList() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.peers) == 0 {
		return nil
	}

	now := time.Now()
	peerList := make([]byte, 0, len(s.peers)*11)
	for _, peer := range s.peers {
		if !peerActive(peer, now) || peer.Addr == nil || peer.Addr.IP == nil {
			continue
		} //nolint:gosec
		peerList = append(peerList, uint32ToBytes(peer.ID)...)
		peerList = append(peerList, peer.Addr.IP.To4()...)
		peerPort := peer.Addr.Port
		if peerPort < 0 || peerPort > 65535 {
			peerPort = 0
		}
		peerList = append(peerList, uint16ToBytes(uint16(peerPort))...) //nolint:gosec // Bounds checked
		peerList = append(peerList, peer.Mode)
	}

	return peerList
}

func (s *IPSCServer) peerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

func (s *IPSCServer) localIDBytes() []byte {
	return uint32ToBytes(s.localID)
}

func (s *IPSCServer) defaultModeByte() byte {
	const (
		peerOperational = 0b01000000
		peerDigital     = 0b00100000
		ts1On           = 0b00001000
		ts2On           = 0b00000010
	)
	return peerOperational | peerDigital | ts1On | ts2On
}

func (s *IPSCServer) defaultFlagsBytes() [4]byte {
	flags := [4]byte{}
	flags[2] = 0x00
	flags[3] = 0x0D
	if s.cfg.IPSC.Auth.Enabled {
		flags[3] |= 0x10
	}
	return flags
}

func parsePeerID(data []byte) (uint32, error) {
	if len(data) < 5 {
		return 0, fmt.Errorf("packet too short for peer ID")
	}
	return binary.BigEndian.Uint32(data[1:5]), nil
}

func uint16ToBytes(value uint16) []byte {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, value)
	return buf
}

func uint32ToBytes(value uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, value)
	return buf
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	cloned := &net.UDPAddr{Port: addr.Port, Zone: addr.Zone}
	if addr.IP != nil {
		cloned.IP = append([]byte(nil), addr.IP...)
	}
	return cloned
}

func (s *IPSCServer) auth(data []byte) bool {
	// Last 10 bytes are the sha hash
	payload := data[:len(data)-10]
	hash := data[len(data)-10:]
	expectedHash := hmac.New(sha1.New, s.authKey)
	expectedHash.Write(payload)
	expectedHashSum := expectedHash.Sum(nil)[:10]

	return hmac.Equal(hash, expectedHashSum)
}

func (s *IPSCServer) sendPacket(packet *Packet, addr *net.UDPAddr) error {
	if s.cfg.IPSC.Auth.Enabled {
		hash := hmac.New(sha1.New, s.authKey)
		hash.Write(packet.data)
		hashSum := hash.Sum(nil)[:10]
		packet.data = append(packet.data, hashSum...)
	}

	n, err := s.udp.WriteToUDP(packet.data, addr)
	if err != nil {
		if s.metrics != nil {
			s.metrics.IPSCUDPErrors.WithLabelValues("write").Inc()
		}
		return fmt.Errorf("error sending packet: %w", err)
	}
	if n != len(packet.data) {
		return fmt.Errorf("error sending packet: only sent %d of %d bytes", n, len(packet.data))
	}
	return nil
}

// SendUserPacket sends data to every active peer, pacing each peer. It
// gives up, possibly after some peers got the packet, once ctx is done or
// the server stops.
func (s *IPSCServer) SendUserPacket(ctx context.Context, data []byte) {
	if s.stopped.Load() || ctx.Err() != nil {
		return
	}
	for _, peer := range s.activePeers(time.Now()) {
		if !s.pacePeer(ctx, peer.id) {
			return
		}
		packetData := make([]byte, len(data))
		copy(packetData, data)
		packet := &Packet{data: packetData}
		slog.Debug("IPSC burst sending", "peer", peer.addr, "length", len(packet.data))
		if err := s.sendPacket(packet, peer.addr); err != nil {
			slog.Warn("failed sending IPSC user packet", "peer", peer.addr, "error", err)
		} else if s.metrics != nil {
			s.metrics.IPSCPacketsSent.Inc()
		}
	}
}

// pacePeer waits until a packet may be sent to peerID and claims that send
// time. It checks again after each wait, so concurrent callers are spaced
// out too. It returns false if ctx is done or the server stops first.
func (s *IPSCServer) pacePeer(ctx context.Context, peerID uint32) bool {
	s.mu.Lock()
	for {
		wait := s.claimSendLocked(peerID, time.Now())
		if wait <= 0 {
			break
		}
		s.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-s.done:
			timer.Stop()
			return false
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	return true
}

// claimSendLocked claims now as the send time for peerID and returns 0, or
// returns how long to wait if the last send was less than s.pace ago. s.mu
// must be held.
func (s *IPSCServer) claimSendLocked(peerID uint32, now time.Time) time.Duration {
	if last := s.lastSend[peerID]; !last.IsZero() {
		if wait := s.pace - now.Sub(last); wait > 0 {
			return wait
		}
	}
	s.lastSend[peerID] = now
	return 0
}
