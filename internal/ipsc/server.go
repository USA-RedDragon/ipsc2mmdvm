package ipsc

import (
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

	"github.com/USA-RedDragon/ipsc2hbrp/internal/config"
	"github.com/vishvananda/netlink"
)

type IPSCServer struct {
	cfg *config.Config
	udp *net.UDPConn
	mu  sync.RWMutex

	localID  uint32
	authKey  []byte // 20-byte HMAC key decoded from hex
	peers    map[uint32]*Peer
	lastSend map[uint32]time.Time

	burstHandler func(packetType byte, data []byte, addr *net.UDPAddr)

	wg       sync.WaitGroup
	stopped  atomic.Bool
	stopOnce sync.Once
}

type Packet struct {
	data []byte
}

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
)

var (
	//nolint:gochecknoglobals
	ipscVersion = []byte{0x04, 0x02, 0x04, 0x01}
)

var ErrPacketIgnored = errors.New("packet ignored")

func NewIPSCServer(cfg *config.Config) *IPSCServer {
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

	// Use the first HBRP network's ID as the local peer identity.
	var localID uint32
	if len(cfg.HBRP) > 0 {
		localID = cfg.HBRP[0].ID
	}

	return &IPSCServer{
		cfg:      cfg,
		localID:  localID,
		authKey:  authKey,
		peers:    map[uint32]*Peer{},
		lastSend: map[uint32]time.Time{},
	}
}

func (s *IPSCServer) Start() error {
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

	s.wg.Add(1)
	go s.handler()

	return nil
}

func (s *IPSCServer) Stop() {
	s.stopOnce.Do(func() {
		slog.Info("Stopping IPSC server")
		s.stopped.Store(true)
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
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("error reading from UDP", "error", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		s.wg.Add(1)
		go func(packetData []byte, packetAddr *net.UDPAddr) {
			defer s.wg.Done()
			packet, err := s.handlePacket(packetData, packetAddr)
			if err != nil {
				if errors.Is(err, ErrPacketIgnored) {
					return
				}
				slog.Warn("error parsing packet", "peer", packetAddr, "error", err, "length", len(packetData), "packet", packetData)
				return
			}

			slog.Debug("received packet", "peer", packetAddr, "length", len(packetData), "packet", packet)
		}(data, addr)
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
			return nil, fmt.Errorf("authentication failed")
		}
		data = data[:len(data)-10] // Remove the hash from the data
	}

	switch PacketType(packetType) {
	case PacketType_GroupVoice, PacketType_PrivateVoice, PacketType_GroupData, PacketType_PrivateData:
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_RepeaterWakeUp:
		if err := s.handleRepeaterWakeUp(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterRegisterRequest:
		if err := s.handleMasterRegisterRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterAliveRequest:
		if err := s.handleMasterAliveRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_PeerListRequest:
		if err := s.handlePeerListRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterRegisterReply, PacketType_PeerListReply, PacketType_MasterAliveReply:
		// These are reply packets, we shouldn't receive them as a server, keeping quiet.
		return nil, ErrPacketIgnored
	default:
		return nil, fmt.Errorf("unknown packet type: %d", packetType)
	}

	return &Packet{data: data}, nil
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

	s.upsertPeer(peerID, addr, mode, flags)

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

	s.markPeerAlive(peerID, addr)

	packet := &Packet{data: s.buildMasterAliveReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending master alive reply: %w", err)
	}

	return nil
}

func (s *IPSCServer) handlePeerListRequest(data []byte, addr *net.UDPAddr) error {
	if _, err := parsePeerID(data); err != nil {
		return err
	}

	packet := &Packet{data: s.buildPeerListReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending peer list reply: %w", err)
	}

	return nil
}

func (s *IPSCServer) handleRepeaterWakeUp(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	s.markPeerAlive(peerID, addr)
	slog.Debug("repeater wake-up packet received", "peer", addr, "peerID", peerID, "length", len(data))
	return nil
}

func (s *IPSCServer) handleUserPacket(packetType PacketType, data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	s.markPeerAlive(peerID, addr)
	slog.Debug("IPSC burst received", "peer", addr, "peerID", peerID, "packetType", byte(packetType), "length", len(data))
	if s.burstHandler != nil {
		packetCopy := make([]byte, len(data))
		copy(packetCopy, data)
		go s.burstHandler(byte(packetType), packetCopy, addr)
	}
	return nil
}

func (s *IPSCServer) SetBurstHandler(handler func(packetType byte, data []byte, addr *net.UDPAddr)) {
	s.burstHandler = handler
}

func (s *IPSCServer) upsertPeer(peerID uint32, addr *net.UDPAddr, mode byte, flags [4]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	peer, ok := s.peers[peerID]
	if !ok {
		peer = &Peer{ID: peerID}
		s.peers[peerID] = peer
	}
	peer.Addr = cloneUDPAddr(addr)
	peer.Mode = mode
	peer.Flags = flags
	peer.LastSeen = time.Now()
	peer.RegistrationStatus = true
}

func (s *IPSCServer) markPeerAlive(peerID uint32, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()

	peer, ok := s.peers[peerID]
	if !ok {
		peer = &Peer{ID: peerID}
		s.peers[peerID] = peer
	}
	peer.Addr = cloneUDPAddr(addr)
	peer.LastSeen = time.Now()
	peer.KeepAliveReceived++
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

	peerList := make([]byte, 0, len(s.peers)*11)
	for _, peer := range s.peers {
		if peer.Addr == nil || peer.Addr.IP == nil {
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
		return fmt.Errorf("error sending packet: %w", err)
	}
	if n != len(packet.data) {
		return fmt.Errorf("error sending packet: only sent %d of %d bytes", n, len(packet.data))
	}
	return nil
}

func (s *IPSCServer) SendUserPacket(data []byte) {
	if s.stopped.Load() {
		return
	}
	s.mu.RLock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, peer := range s.peers {
		if peer.Addr != nil {
			peers = append(peers, peer)
		}
	}
	s.mu.RUnlock()

	for _, peer := range peers {
		s.pacePeer(peer.ID)
		packetData := make([]byte, len(data))
		copy(packetData, data)
		packet := &Packet{data: packetData}
		slog.Debug("IPSC burst sending", "peer", peer.Addr, "length", len(packet.data))
		if err := s.sendPacket(packet, peer.Addr); err != nil {
			slog.Warn("failed sending IPSC user packet", "peer", peer.Addr, "error", err)
		}
	}
}

func (s *IPSCServer) pacePeer(peerID uint32) {
	const burstInterval = 30 * time.Millisecond

	s.mu.Lock()
	last := s.lastSend[peerID]
	now := time.Now()
	if !last.IsZero() {
		elapsed := now.Sub(last)
		if elapsed < burstInterval {
			s.mu.Unlock()
			time.Sleep(burstInterval - elapsed)
			s.mu.Lock()
		}
	}
	s.lastSend[peerID] = time.Now()
	s.mu.Unlock()
}
