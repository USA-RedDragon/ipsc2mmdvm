package ipsc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
	"github.com/USA-RedDragon/dmrgo/v2/transport/ipsc/auth"
	"github.com/USA-RedDragon/dmrgo/v2/transport/ipsc/session"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/config"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/vishvananda/netlink"
)

// IPSCServer is the IPSC master that the repeaters register with. A
// session.Master runs the registration, keepalives, peer list, expiry and
// authentication; the server does the IO, the metrics and the pacing.
type IPSCServer struct {
	cfg     *config.Config
	metrics *metrics.Metrics
	udp     *net.UDPConn

	mu       sync.Mutex
	master   *session.Master
	authKey  *auth.Key
	out      []session.Output
	epoch    time.Time
	lastSend map[uint32]time.Time
	wake     chan struct{}
	pace     time.Duration
	newTimer func(time.Duration) *time.Timer
	armTimer func(time.Duration) *time.Timer
	now      func() time.Time

	burstHandler func(packetType byte, data []byte, addr *net.UDPAddr)
	bursts       chan burst

	lifeMu   sync.Mutex
	wg       sync.WaitGroup
	stopped  atomic.Bool
	stopOnce sync.Once
	done     chan struct{}
}

type burst struct {
	packetType byte
	data       []byte
	addr       *net.UDPAddr
}

// burstQueueSize bounds the user packets waiting for the burst handler, so a
// stalled MMDVM side drops voice instead of blocking keepalive replies.
const burstQueueSize = 256

// ErrServerStopped is the error of Start after Stop.
var ErrServerStopped = errors.New("server stopped")

const (
	maxPeers = 64
	// defaultPace is the minimum gap between user packets sent to one peer.
	// Two busy timeslots need one packet every 30 ms plus repeated voice
	// headers, so the pace must stay well under 30 ms or a queue builds.
	defaultPace = 15 * time.Millisecond
)

// NewIPSCServer returns a server for cfg that reports to m, which may be nil.
func NewIPSCServer(cfg *config.Config, m *metrics.Metrics) *IPSCServer {
	mc := session.MasterConfig{MaxPeers: maxPeers}
	if len(cfg.MMDVM) > 0 {
		mc.ID = cfg.MMDVM[0].ID
	}
	var authKey *auth.Key
	if cfg.IPSC.Auth.Enabled {
		k, err := auth.ParseKey(cfg.IPSC.Auth.Key)
		if err != nil {
			slog.Error("failed to parse the IPSC auth key", "error", err)
		}
		mc.Auth, mc.Key = true, k
		authKey = auth.NewKey(k)
	}
	master, err := session.NewMaster(&mc)
	if err != nil {
		slog.Error("cannot make the IPSC master", "error", err)
	}
	s := &IPSCServer{
		cfg:      cfg,
		metrics:  m,
		master:   master,
		authKey:  authKey,
		epoch:    time.Now(),
		lastSend: map[uint32]time.Time{},
		wake:     make(chan struct{}, 1),
		pace:     defaultPace,
		newTimer: time.NewTimer,
		armTimer: time.NewTimer,
		now:      time.Now,
		bursts:   make(chan burst, burstQueueSize),
		done:     make(chan struct{}),
	}
	if master != nil {
		s.out = make([]session.Output, 0, master.MaxOutputs())
	}
	return s
}

// Start configures the interface and starts serving. It returns
// ErrServerStopped once Stop has been called.
func (s *IPSCServer) Start() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.stopped.Load() {
		return ErrServerStopped
	}
	if s.master == nil {
		return errors.New("IPSC master is not configured")
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

// Stop de-registers from every peer, closes the socket and waits for the
// goroutines of the server.
func (s *IPSCServer) Stop() {
	s.stopOnce.Do(func() {
		slog.Info("Stopping IPSC server")
		s.lifeMu.Lock()
		defer s.lifeMu.Unlock()
		s.stopped.Store(true)
		close(s.done)
		if s.udp != nil {
			s.deRegisterSelf()
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

	buf := make([]byte, session.MaxPacket+1)
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
		s.handlePacket(buf[:n], addr)
	}
}

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

func (s *IPSCServer) clock() time.Duration {
	return s.now().Sub(s.epoch)
}

func (s *IPSCServer) handlePacket(data []byte, addr *net.UDPAddr) {
	from, ok := sessionAddr(addr)
	if !ok || len(data) == 0 {
		slog.Debug("dropped IPSC packet", "peer", addr, "length", len(data))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.master == nil {
		return
	}
	s.out = s.master.Packet(s.clock(), from, data, s.out[:0])
	if len(s.out) == 0 && s.authKey != nil {
		if _, ok := s.authKey.Verify(data); !ok {
			if s.metrics != nil {
				s.metrics.IPSCAuthFailures.Inc()
			}
			slog.Debug("dropped IPSC packet that failed authentication", "peer", addr)
			return
		}
	}
	s.countReceived(ipscpkt.Opcode(data[0]))
	s.actLocked(ipscpkt.Opcode(data[0]))
}

func (s *IPSCServer) actLocked(op ipscpkt.Opcode) {
	changed := false
	for i := range s.out {
		o := &s.out[i]
		switch o.Kind {
		case session.OutputSend:
			s.writeLocked(o.Packet, udpAddr(o.Addr))
		case session.OutputUp:
			changed = true
			slog.Info("IPSC peer registered", "peerID", o.ID, "peer", udpAddr(o.Addr), "opcode", op)
			select {
			case s.wake <- struct{}{}:
			default:
			}
		case session.OutputDown:
			changed = true
			delete(s.lastSend, o.ID)
			slog.Warn("IPSC peer de-registered", "peerID", o.ID, "peer", udpAddr(o.Addr), "reason", o.Reason)
		case session.OutputData:
			s.dataLocked(o)
		case session.OutputPeers:
		}
	}
	if changed && s.metrics != nil {
		s.metrics.IPSCPeersRegistered.Set(float64(s.master.Peers()))
	}
}

func (s *IPSCServer) dataLocked(o *session.Output) {
	op := ipscpkt.Opcode(o.Packet[0])
	switch op { //nolint:exhaustive // the other packets are not bridged
	case ipscpkt.OpGroupVoice, ipscpkt.OpPrivateVoice, ipscpkt.OpGroupData, ipscpkt.OpPrivateData:
	default:
		slog.Debug("IPSC packet not bridged", "peerID", o.ID, "opcode", op, "length", len(o.Packet))
		return
	}
	if s.burstHandler == nil {
		return
	}
	addr := udpAddr(o.Addr)
	select {
	case s.bursts <- burst{packetType: byte(op), data: append([]byte(nil), o.Packet...), addr: addr}:
	default:
		slog.Warn("IPSC burst queue full, dropping packet", "peer", addr, "peerID", o.ID)
	}
}

func (s *IPSCServer) countReceived(op ipscpkt.Opcode) {
	if s.metrics == nil {
		return
	}
	name := "other"
	switch op { //nolint:exhaustive // other opcodes are counted as other
	case ipscpkt.OpGroupVoice:
		name = "group_voice"
	case ipscpkt.OpPrivateVoice:
		name = "private_voice"
	case ipscpkt.OpGroupData:
		name = "group_data"
	case ipscpkt.OpPrivateData:
		name = "private_data"
	case ipscpkt.OpWakeup:
		name = "wake_up"
	case ipscpkt.OpRegisterRequest:
		name = "register"
	case ipscpkt.OpAliveRequest:
		name = "alive"
	case ipscpkt.OpPeerListRequest:
		name = "peer_list"
	case ipscpkt.OpDeregisterRequest:
		name = "de_register"
	case ipscpkt.OpStatus:
		name = "repeater_status"
	case ipscpkt.OpRegisterReply, ipscpkt.OpPeerListReply, ipscpkt.OpAliveReply, ipscpkt.OpDeregisterReply:
		return
	}
	s.metrics.IPSCPacketsReceived.WithLabelValues(name).Inc()
}

// SetBurstHandler sets the function that gets each user packet of a
// registered peer, without its authentication trailer. Call it before Start.
func (s *IPSCServer) SetBurstHandler(handler func(packetType byte, data []byte, addr *net.UDPAddr)) {
	s.burstHandler = handler
}

func (s *IPSCServer) maintenance() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		at, ok := s.master.Deadline()
		wait := time.Hour
		if ok {
			wait = max(at-s.clock(), 0)
		}
		s.mu.Unlock()
		timer := s.armTimer(wait)
		select {
		case <-s.done:
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
			s.tick()
		}
	}
}

func (s *IPSCServer) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = s.master.Tick(s.clock(), s.out[:0])
	s.actLocked(0)
}

func (s *IPSCServer) deRegisterSelf() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.master == nil {
		return
	}
	s.out = s.master.Close(s.clock(), s.out[:0])
	s.actLocked(0)
}

func (s *IPSCServer) writeLocked(p []byte, addr *net.UDPAddr) bool {
	if s.udp == nil {
		return false
	}
	if _, err := s.udp.WriteToUDP(p, addr); err != nil {
		if s.metrics != nil {
			s.metrics.IPSCUDPErrors.WithLabelValues("write").Inc()
		}
		slog.Warn("error sending IPSC packet", "peer", addr, "error", err)
		return false
	}
	return true
}

func sessionAddr(addr *net.UDPAddr) (session.Addr, bool) {
	if addr == nil {
		return session.Addr{}, false
	}
	ip := addr.IP.To4()
	if ip == nil || addr.Port < 0 || addr.Port > 0xFFFF {
		return session.Addr{}, false
	}
	return session.Addr{IP: [4]byte(ip), Port: uint16(addr.Port)}, true //nolint:gosec // bounds checked
}

func udpAddr(a session.Addr) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(a.IP[0], a.IP[1], a.IP[2], a.IP[3]), Port: int(a.Port)}
}

type peerTarget struct {
	id   uint32
	addr *net.UDPAddr
}

func (s *IPSCServer) activePeers() []peerTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.master == nil {
		return nil
	}
	targets := make([]peerTarget, 0, s.master.Peers())
	for i := range s.master.Peers() {
		p := s.master.PeerAt(i)
		targets = append(targets, peerTarget{id: p.ID, addr: udpAddr(p.Addr)})
	}
	return targets
}

func (s *IPSCServer) registeredLocked(id uint32) bool {
	for i := range s.master.Peers() {
		if s.master.PeerAt(i).ID == id {
			return true
		}
	}
	return false
}

// SendUserPacket sends data to every registered peer, pacing each peer. It
// gives up, possibly after some peers got the packet, once ctx is done or
// the server stops.
func (s *IPSCServer) SendUserPacket(ctx context.Context, data []byte) {
	if s.stopped.Load() || ctx.Err() != nil {
		return
	}
	for _, peer := range s.activePeers() {
		if !s.pacePeer(ctx, peer.id) {
			return
		}
		s.mu.Lock()
		if !s.registeredLocked(peer.id) {
			delete(s.lastSend, peer.id)
			s.mu.Unlock()
			continue
		}
		p := s.master.Sign(append(make([]byte, 0, len(data)+auth.TrailerLen), data...))
		slog.Debug("IPSC burst sending", "peer", peer.addr, "length", len(p))
		if s.writeLocked(p, peer.addr) && s.metrics != nil {
			s.metrics.IPSCPacketsSent.Inc()
		}
		s.mu.Unlock()
	}
}

// pacePeer waits until a packet may be sent to peerID. It claims the send
// time of the packet when it is called, so concurrent callers send in the
// order they called, each s.pace after the one before: a sender that keeps
// sending cannot hold off another. It returns false if ctx is done or the
// server stops first.
func (s *IPSCServer) pacePeer(ctx context.Context, peerID uint32) bool {
	s.mu.Lock()
	now := s.now()
	wait := s.claimSendLocked(peerID, now).Sub(now)
	s.mu.Unlock()
	if wait <= 0 {
		return true
	}
	timer := s.newTimer(wait)
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-s.done:
		timer.Stop()
		return false
	}
}

func (s *IPSCServer) claimSendLocked(peerID uint32, now time.Time) time.Time {
	at := now
	if next := s.lastSend[peerID].Add(s.pace); !s.lastSend[peerID].IsZero() && next.After(at) {
		at = next
	}
	s.lastSend[peerID] = at
	return at
}
