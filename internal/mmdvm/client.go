package mmdvm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/config"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

type MMDVMClient struct {
	cfg          *config.MMDVM
	metrics      *metrics.Metrics
	started      atomic.Bool
	done         chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
	tx_chan      chan proto.Packet
	conn         net.Conn
	connMu       sync.Mutex    // protects conn and connStop
	connStop     chan struct{} // closed when conn is replaced
	state        atomic.Uint32
	stateSince   atomic.Int64 // UnixNano of the last state change
	connRX       chan []byte
	connTX       chan []byte
	keepAlive    time.Duration
	timeout      time.Duration
	lastHeard    atomic.Int64 // UnixNano — last packet read from the master
	rxWaiting    atomic.Bool  // rx holds a packet the handler has not taken yet
	lastPingSent atomic.Int64 // UnixNano — last RPTPING sent
	ipscHandler  atomic.Pointer[func(data []byte)]
	translator   *ipsc.IPSCTranslator
	dial         func(ctx context.Context, network, address string) (net.Conn, error)
	dialTimeout  time.Duration
	sessMu       sync.Mutex // serializes handshake transitions with supervise
	salt         []byte     // last RPTACK salt, guarded by sessMu
	resentAt     time.Time  // last handshake resend, guarded by sessMu

	// Rewrite rules built from config, applied to packets
	// flowing through this network.
	rfRewrites      []rewrite.Rule // RF→Net (outbound to this master)
	netRewrites     []rewrite.Rule // Net→RF (inbound from this master)
	passallRewrites []rewrite.Rule // PassAll fallback for RF→Net

	// Timeslot managers prevent interleaved calls on the same slot.
	// outboundTSMgr is shared across all clients for the MMDVM→IPSC
	// direction. inboundTSMgr is per-client for the IPSC→MMDVM direction.
	outboundTSMgr *timeslot.Manager
	inboundTSMgr  *timeslot.Manager
}

type state uint8

const (
	STATE_IDLE state = iota
	STATE_SENT_LOGIN
	STATE_SENT_AUTH
	STATE_SENT_RPTC
	STATE_READY
	STATE_TIMEOUT
)

const (
	packetTypeMstack = "MSTACK"
	packetTypeMstnak = "MSTNAK"
	packetTypeMstcl  = "MSTCL"
)

// DMR frame type and data type constants for call termination detection.
const (
	frameTypeDataSync     uint = 2 // FrameType value for data sync (header/terminator)
	dtypeTerminatorWithLC uint = 2 // DataType value for Terminator with Link Control
)

func NewMMDVMClient(cfg *config.MMDVM, m *metrics.Metrics) *MMDVMClient {
	tx_chan := make(chan proto.Packet, 256)
	translator, err := ipsc.NewIPSCTranslator()
	if err != nil {
		slog.Warn("failed to load IPSC translator", "error", err)
	}
	c := &MMDVMClient{
		cfg:          cfg,
		metrics:      m,
		done:         make(chan struct{}),
		tx_chan:      tx_chan,
		connRX:       make(chan []byte, 16),
		connTX:       make(chan []byte, 16),
		keepAlive:    5 * time.Second,
		timeout:      15 * time.Second,
		translator:   translator,
		inboundTSMgr: timeslot.NewManager(),
		dial:         (&net.Dialer{}).DialContext,
		dialTimeout:  defaultDialTimeout,
	}
	c.state.Store(uint32(STATE_IDLE))
	c.buildRewriteRules()
	if m != nil {
		if translator != nil {
			translator.SetMetrics(m)
		}
		c.inboundTSMgr.SetMetrics(m, "inbound")
	}
	return c
}

// Name returns the configured network name for this client.
func (h *MMDVMClient) Name() string {
	return h.cfg.Name
}

// buildRewriteRules constructs the rewrite rule chains from config.
// For each TGRewrite config entry, two rules are created:
//   - rfRewrite: fromSlot/fromTG → toSlot/toTG (for RF→Net direction)
//   - netRewrite: toSlot/toTG → fromSlot/fromTG (reverse, for Net→RF direction)
//
// PCRewrite only creates an RF rewrite (outbound).
// TypeRewrite only creates an RF rewrite (outbound).
// SrcRewrite only creates a Net rewrite (inbound).
func (h *MMDVMClient) buildRewriteRules() {
	name := h.cfg.Name

	for _, cfg := range h.cfg.TGRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.rfRewrites = append(h.rfRewrites, &rewrite.TGRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromTG: cfg.FromTG,
			ToSlot: cfg.ToSlot, ToTG: cfg.ToTG, Range: rng,
		})
		// Reverse direction
		h.netRewrites = append(h.netRewrites, &rewrite.TGRewrite{
			Name: name, FromSlot: cfg.ToSlot, FromTG: cfg.ToTG,
			ToSlot: cfg.FromSlot, ToTG: cfg.FromTG, Range: rng,
		})
	}

	for _, cfg := range h.cfg.PCRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.rfRewrites = append(h.rfRewrites, &rewrite.PCRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromID: cfg.FromID,
			ToSlot: cfg.ToSlot, ToID: cfg.ToID, Range: rng,
		})
	}

	for _, cfg := range h.cfg.TypeRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.rfRewrites = append(h.rfRewrites, &rewrite.TypeRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromTG: cfg.FromTG,
			ToSlot: cfg.ToSlot, ToID: cfg.ToID, Range: rng,
		})
	}

	for _, cfg := range h.cfg.SrcRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.netRewrites = append(h.netRewrites, &rewrite.SrcRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromID: cfg.FromID,
			ToSlot: cfg.ToSlot, ToTG: cfg.ToTG, Range: rng,
		})
	}

	for _, slot := range h.cfg.PassAllTG {
		if slot < 0 {
			continue
		}
		s := uint(slot) //nolint:gosec
		r := &rewrite.PassAllTG{Name: name, Slot: s}
		h.passallRewrites = append(h.passallRewrites, r)
		h.netRewrites = append(h.netRewrites, &rewrite.PassAllTG{Name: name, Slot: s})
	}
	for _, slot := range h.cfg.PassAllPC {
		if slot < 0 {
			continue
		}
		s := uint(slot) //nolint:gosec
		r := &rewrite.PassAllPC{Name: name, Slot: s}
		h.passallRewrites = append(h.passallRewrites, r)
		h.netRewrites = append(h.netRewrites, &rewrite.PassAllPC{Name: name, Slot: s})
	}
}

// Start launches the client and returns without waiting for the network.
// The master is dialed and logged into in the background, and retried
// until it answers. Start fails only if the master address is malformed.
func (h *MMDVMClient) Start() error {
	_, port, err := net.SplitHostPort(h.cfg.MasterServer)
	if err != nil {
		return fmt.Errorf("invalid master server address %q: %w", h.cfg.MasterServer, err)
	}
	if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
		return fmt.Errorf("invalid master server port in %q", h.cfg.MasterServer)
	}

	if h.translator != nil {
		h.translator.SetPeerID(h.cfg.ID)
	}

	slog.Info("Connecting to MMDVM server", "network", h.cfg.Name)

	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(1)
	}

	h.started.Store(true)

	h.wg.Add(4)
	go h.handler()
	go h.tx()
	go h.forwardTX()
	go h.supervise()

	return nil
}

// connect dials the master and installs the new connection. The dial is
// bounded by dialTimeout and canceled by Stop. The caller starts rx for
// the connection.
func (h *MMDVMClient) connect() (net.Conn, error) {
	dial := h.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	timeout := h.dialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	go func() {
		select {
		case <-h.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	conn, err := dial(ctx, "udp", h.cfg.MasterServer)
	if err != nil {
		return nil, err
	}
	h.connMu.Lock()
	defer h.connMu.Unlock()
	select {
	case <-h.done:
		_ = conn.Close()
		return nil, net.ErrClosed
	default:
	}
	h.conn = conn
	h.connStop = make(chan struct{})
	return conn, nil
}

const rptAck = "RPTACK"

const defaultDialTimeout = 10 * time.Second

func (h *MMDVMClient) handler() {
	defer h.wg.Done()
	for {
		select {
		case data := <-h.connRX:
			slog.Debug("received packet", "data", fmt.Sprintf("% X", data), "strdata", string(data), "network", h.cfg.Name)
			if len(data) < 4 {
				slog.Warn("Ignoring short packet from MMDVM server", "network", h.cfg.Name, "length", len(data))
				continue
			}
			h.handleState(data)
		case <-h.done:
			return
		}
	}
}

func (h *MMDVMClient) currentState() state {
	return state(h.state.Load()) //nolint:gosec // only state values are stored
}

// setState records a state change and when it happened. Handshake states
// time out relative to that moment.
func (h *MMDVMClient) setState(s state) {
	h.stateSince.Store(time.Now().UnixNano())
	h.state.Store(uint32(s))
}

func (h *MMDVMClient) handleState(data []byte) {
	h.sessMu.Lock()
	if h.currentState() == STATE_READY {
		h.sessMu.Unlock()
		h.handleReady(data)
		return
	}
	defer h.sessMu.Unlock()
	if len(data) >= 5 && string(data[:5]) == packetTypeMstcl {
		slog.Warn("Master closed the connection, reconnecting", "network", h.cfg.Name)
		h.reconnect()
		return
	}
	switch h.currentState() {
	case STATE_IDLE:
		slog.Info("Got data from MMDVM server while idle", "network", h.cfg.Name)
	case STATE_SENT_LOGIN:
		h.handleSentLogin(data)
	case STATE_SENT_AUTH:
		h.handleSentAuth(data)
	case STATE_SENT_RPTC:
		h.handleSentRPTC(data)
	case STATE_READY:
	case STATE_TIMEOUT:
		slog.Info("Got data from MMDVM server while in timeout state", "network", h.cfg.Name)
	}
}

// The handleSent* functions run with sessMu held. A rejected step is not
// resent here; supervise retries it on its next tick.

func (h *MMDVMClient) handleSentLogin(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		if len(data) < 10 {
			slog.Warn("RPTACK response too short", "network", h.cfg.Name, "length", len(data))
			return
		}
		slog.Info("Connected. Authenticating", "network", h.cfg.Name)
		h.salt = append(h.salt[:0], data[len(data)-4:]...)
		h.sendRPTK(h.salt)
		h.setState(STATE_SENT_AUTH)
	} else {
		slog.Warn("Server rejected login request", "network", h.cfg.Name)
	}
}

func (h *MMDVMClient) handleSentAuth(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		slog.Info("Authenticated. Sending configuration", "network", h.cfg.Name)
		h.setState(STATE_SENT_RPTC)
		h.sendRPTC()
	} else if len(data) >= 6 && string(data[:6]) == packetTypeMstnak {
		slog.Warn("Password rejected", "network", h.cfg.Name)
		if h.metrics != nil {
			h.metrics.MMDVMAuthFailures.WithLabelValues(h.cfg.Name).Inc()
		}
		h.setState(STATE_SENT_LOGIN)
	}
}

func (h *MMDVMClient) handleSentRPTC(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		slog.Info("Config accepted, logged in", "network", h.cfg.Name)
		h.lastHeard.Store(time.Now().UnixNano())
		h.setState(STATE_READY)
		if h.metrics != nil {
			h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(2)
		}
		h.sendPing()
	} else if len(data) >= 6 && string(data[:6]) == packetTypeMstnak {
		slog.Warn("Configuration rejected, logging in again", "network", h.cfg.Name)
		h.setState(STATE_SENT_LOGIN)
	}
}

func (h *MMDVMClient) handleReady(data []byte) {
	switch string(data[:4]) {
	case "MSTN":
		if len(data) >= 6 && string(data[:6]) == packetTypeMstnak {
			h.relogin()
		}
	case "MSTC":
		if len(data) >= 5 && string(data[:5]) == packetTypeMstcl {
			slog.Warn("Master closed the connection, reconnecting", "network", h.cfg.Name)
			h.sessMu.Lock()
			h.reconnect()
			h.sessMu.Unlock()
		}
	case "RPTS":
		if len(data) >= 7 && string(data[:7]) == "RPTSBKN" {
			slog.Info("Server requested a roaming beacon transmission", "network", h.cfg.Name)
		}
	case "DMRD":
		packet, ok := proto.Decode(data)
		if !ok {
			slog.Info("Error unpacking packet", "network", h.cfg.Name)
			return
		}
		if h.metrics != nil {
			h.metrics.MMDVMPacketsReceived.WithLabelValues(h.cfg.Name).Inc()
		}
		slog.Debug("MMDVM DMRD received", "network", h.cfg.Name, "packet", packet)

		if !rewrite.Apply(h.netRewrites, &packet) {
			slog.Debug("MMDVM DMRD dropped (no rewrite rule matched)", "network", h.cfg.Name)
			if h.metrics != nil {
				h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "no_rewrite").Inc()
			}
			return
		}

		slog.Debug("MMDVM DMRD after rewrite", "network", h.cfg.Name, "packet", packet)

		h.submitOutbound(packet)
	default:
		slog.Info("Got unknown packet from MMDVM server", "network", h.cfg.Name, "data", data)
	}
}

// supervise owns the connection for the whole run. Every keepalive
// interval it pings when READY, resends a stalled handshake step every
// second interval (a faster resend can reset a session on hblink), and
// reconnects when the master has stopped answering, the
// handshake has not progressed within the timeout, or there is no
// connection at all.
func (h *MMDVMClient) supervise() {
	defer h.wg.Done()
	h.sessMu.Lock()
	h.connectAndLogin()
	h.sessMu.Unlock()
	ticker := time.NewTicker(h.keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.tick(time.Now())
		case <-h.done:
			return
		}
	}
}

func (h *MMDVMClient) tick(now time.Time) {
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	switch st := h.currentState(); st {
	case STATE_READY:
		if !h.rxWaiting.Load() && now.Sub(time.Unix(0, h.lastHeard.Load())) > h.timeout {
			slog.Info("Connection timed out", "network", h.cfg.Name)
			h.reconnect()
			return
		}
		h.sendPing()
	case STATE_SENT_LOGIN, STATE_SENT_AUTH, STATE_SENT_RPTC:
		if now.Sub(time.Unix(0, h.stateSince.Load())) > h.timeout {
			slog.Warn("Handshake timed out, reconnecting", "network", h.cfg.Name, "state", st)
			h.reconnect()
			return
		}
		last := time.Unix(0, h.stateSince.Load())
		if h.resentAt.After(last) {
			last = h.resentAt
		}
		if now.Sub(last) >= 2*h.keepAlive {
			h.resentAt = now
			h.resend(st)
		}
	case STATE_IDLE, STATE_TIMEOUT:
		h.connectAndLogin()
	}
}

// resend repeats the handshake step for st. Must be called with sessMu held.
func (h *MMDVMClient) resend(st state) {
	switch st {
	case STATE_SENT_LOGIN:
		h.sendLogin()
	case STATE_SENT_AUTH:
		h.sendRPTK(h.salt)
	case STATE_SENT_RPTC:
		h.sendRPTC()
	case STATE_IDLE, STATE_READY, STATE_TIMEOUT:
	}
}

// relogin starts a new login on the current connection after the master
// rejected an established session.
func (h *MMDVMClient) relogin() {
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	if h.currentState() != STATE_READY {
		return
	}
	slog.Warn("Master rejected the session, logging in again", "network", h.cfg.Name)
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(1)
	}
	h.startLogin()
}

// startLogin begins a new handshake on the current connection. Must be
// called with sessMu held.
func (h *MMDVMClient) startLogin() {
	h.setState(STATE_SENT_LOGIN)
	h.sendLogin()
}

// reconnect closes the current connection, then dials a new one and starts
// a fresh login. Must be called with sessMu held.
func (h *MMDVMClient) reconnect() {
	h.setState(STATE_TIMEOUT)
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(0)
		h.metrics.MMDVMReconnects.WithLabelValues(h.cfg.Name).Inc()
	}
	h.connMu.Lock()
	if h.conn != nil {
		if err := h.conn.Close(); err != nil {
			slog.Error("Error closing connection", "network", h.cfg.Name, "error", err)
		}
		h.conn = nil
		close(h.connStop)
	}
	h.connMu.Unlock()
	for len(h.connRX) > 0 {
		select {
		case <-h.connRX:
		default:
		}
	}
	h.connectAndLogin()
}

// connectAndLogin dials the master and starts a login. If the dial fails
// the client stays in STATE_TIMEOUT and the next tick tries again. Must be
// called with sessMu held.
func (h *MMDVMClient) connectAndLogin() {
	h.setState(STATE_TIMEOUT)
	conn, err := h.connect()
	if err != nil {
		select {
		case <-h.done:
		default:
			slog.Error("Error connecting to MMDVM server", "network", h.cfg.Name, "error", err)
		}
		return
	}
	h.connMu.Lock()
	stop := h.connStop
	h.connMu.Unlock()
	h.wg.Add(1)
	go h.rx(conn, stop)
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(1)
	}
	h.startLogin()
}

func (h *MMDVMClient) tx() {
	defer h.wg.Done()
	for {
		select {
		case <-h.done:
			return
		case data := <-h.connTX:
			h.connMu.Lock()
			conn := h.conn
			h.connMu.Unlock()
			if conn == nil {
				slog.Debug("Dropping packet, not connected", "network", h.cfg.Name)
				continue
			}
			slog.Debug("sending packet", "data", fmt.Sprintf("% X", data), "strdata", string(data), "network", h.cfg.Name)
			if _, err := conn.Write(data); err != nil {
				if errors.Is(err, net.ErrClosed) {
					slog.Debug("Dropping packet, connection closed", "network", h.cfg.Name)
					continue
				}
				slog.Error("Error writing to MMDVM server", "network", h.cfg.Name, "error", err)
			}
		}
	}
}

// rx reads from one connection until it is closed. It records when the
// master was last heard from and handles MSTPONG itself, so keepalive does
// not depend on the handler goroutine.
func (h *MMDVMClient) rx(conn net.Conn, stop <-chan struct{}) {
	defer h.wg.Done()
	for {
		data := make([]byte, 512)
		n, err := conn.Read(data)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-h.done:
				return
			default:
			}
			slog.Error("Error reading from MMDVM server", "network", h.cfg.Name, "error", err)
			continue
		}
		h.lastHeard.Store(time.Now().UnixNano())
		if n >= 7 && string(data[:7]) == "MSTPONG" {
			h.handlePong()
			continue
		}
		if !h.deliver(data[:n], stop) {
			return
		}
	}
}

// deliver hands a packet to the handler. While it waits, rxWaiting tells
// supervise that the master is still talking to us: the handler is busy
// (for example replaying a buffered call), and MSTPONGs queued behind this
// packet are not lost. Returns false when the client stops or the
// connection is replaced, so a stale packet never reaches a new session.
func (h *MMDVMClient) deliver(data []byte, stop <-chan struct{}) bool {
	select {
	case <-stop:
		return false
	default:
	}
	select {
	case h.connRX <- data:
		return true
	default:
	}
	h.rxWaiting.Store(true)
	defer func() {
		h.lastHeard.Store(time.Now().UnixNano())
		h.rxWaiting.Store(false)
	}()
	select {
	case h.connRX <- data:
		return true
	case <-stop:
		return false
	case <-h.done:
		return false
	}
}

func (h *MMDVMClient) handlePong() {
	if h.metrics == nil {
		return
	}
	if sent := h.lastPingSent.Load(); sent != 0 {
		h.metrics.MMDVMPingRTT.WithLabelValues(h.cfg.Name).Observe(time.Since(time.Unix(0, sent)).Seconds())
	}
}

func (h *MMDVMClient) Stop() {
	h.stopOnce.Do(func() {
		slog.Info("Stopping MMDVM client", "network", h.cfg.Name)

		// Signal all goroutines to stop.
		close(h.done)

		// Must follow close(done): Manager.Stop waits for replays, and
		// a replay blocked in sendInbound only returns once done closes.
		if h.inboundTSMgr != nil {
			h.inboundTSMgr.Stop()
		}

		// Send the disconnect message directly on the wire (best-effort).
		h.connMu.Lock()
		if h.conn != nil {
			h.sendRPTCLDirect()
			h.conn.Close()
		}
		h.connMu.Unlock()

		h.started.Store(false)
	})

	// Wait for all goroutines to finish.
	h.wg.Wait()
}

// sendRPTCLDirect writes the disconnect message directly on the connection.
// Must be called with connMu held.
func (h *MMDVMClient) sendRPTCLDirect() {
	if _, err := h.conn.Write(h.rptclPacket()); err != nil {
		slog.Error("Error sending RPTCL disconnect", "network", h.cfg.Name, "error", err)
	}
}

func (h *MMDVMClient) forwardTX() {
	defer h.wg.Done()
	for {
		select {
		case <-h.done:
			return
		case pkt := <-h.tx_chan:
			h.sendPacket(pkt)
		}
	}
}

// translateAndForwardToIPSC converts a proto.Packet to IPSC and sends it.
func (h *MMDVMClient) translateAndForwardToIPSC(packet proto.Packet) {
	handler := h.ipscHandler.Load()
	if handler != nil && h.translator != nil {
		ipscPackets := h.translator.TranslateToIPSC(packet)
		for _, ipscData := range ipscPackets {
			(*handler)(ipscData)
		}
	}
}

func isTerminator(pkt proto.Packet) bool {
	return pkt.FrameType == frameTypeDataSync && pkt.DTypeOrVSeq == dtypeTerminatorWithLC
}

// submitOutbound passes a packet for the MMDVM→IPSC direction through
// the shared timeslot manager. Buffered packets are later sent by
// forwardOutbound on this client, so a call always uses one translator.
func (h *MMDVMClient) submitOutbound(packet proto.Packet) {
	if h.outboundTSMgr == nil {
		h.translateAndForwardToIPSC(packet)
		return
	}
	end := isTerminator(packet)
	switch h.outboundTSMgr.Submit(packet.Slot, packet.StreamID, h.cfg.Name, packet, end, h.forwardOutbound) {
	case timeslot.Deliver:
		h.translateAndForwardToIPSC(packet)
		if end {
			h.outboundTSMgr.Release(packet.Slot, packet.StreamID)
		}
	case timeslot.Queued:
		slog.Debug("MMDVM DMRD buffered (timeslot busy)",
			"network", h.cfg.Name, "slot", packet.Slot, "streamID", packet.StreamID)
	case timeslot.Dropped:
		h.countTimeslotDrop(packet)
	}
}

func (h *MMDVMClient) countTimeslotDrop(pkt proto.Packet) {
	slog.Debug("packet dropped (timeslot buffer full)",
		"network", h.cfg.Name, "slot", pkt.Slot, "streamID", pkt.StreamID)
	if h.metrics != nil {
		h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "timeslot_full").Inc()
	}
}

func (h *MMDVMClient) forwardOutbound(packet any) {
	if pkt, ok := packet.(proto.Packet); ok {
		h.translateAndForwardToIPSC(pkt)
	}
}

func (h *MMDVMClient) forwardInbound(packet any) {
	if pkt, ok := packet.(proto.Packet); ok {
		h.sendInbound(pkt)
	}
}

// sendInbound queues a packet for the MMDVM master. It returns false
// if the client is stopping.
func (h *MMDVMClient) sendInbound(pkt proto.Packet) bool {
	select {
	case h.tx_chan <- pkt:
		return true
	case <-h.done:
		return false
	}
}

// SetIPSCHandler sets the function that receives translated IPSC packets.
// It is safe to call at any time, including after Start.
func (h *MMDVMClient) SetIPSCHandler(handler func(data []byte)) {
	if handler == nil {
		h.ipscHandler.Store(nil)
		return
	}
	h.ipscHandler.Store(&handler)
}

// SetOutboundTSManager sets the shared timeslot manager used for the
// MMDVM→IPSC direction. This manager is shared across all clients so
// that only one MMDVM master can feed a given timeslot at a time.
func (h *MMDVMClient) SetOutboundTSManager(mgr *timeslot.Manager) {
	h.outboundTSMgr = mgr
}

// MatchesRules checks whether the given IPSC data would match this client's
// rewrite rules without translating or modifying any state. It extracts
// routing-relevant fields (src, dst, groupCall, slot) directly from the
// IPSC packet header. When passallOnly is true, only passall rules are checked.
func (h *MMDVMClient) MatchesRules(packetType byte, data []byte, passallOnly bool) bool {
	if len(data) < 18 {
		return false
	}
	// Build a probe packet from IPSC header fields.
	probe := proto.Packet{
		Src:       uint(data[6])<<16 | uint(data[7])<<8 | uint(data[8]),
		Dst:       uint(data[9])<<16 | uint(data[10])<<8 | uint(data[11]),
		GroupCall: packetType == 0x80 || packetType == 0x83,
		Slot:      (data[17] & 0x20) != 0,
	}
	if passallOnly {
		return rewrite.Apply(h.passallRewrites, &probe)
	}
	return rewrite.Apply(h.rfRewrites, &probe)
}

// HandleIPSCBurst handles an incoming IPSC burst from the IPSC server.
// This is called when a connected IPSC peer transmits voice/data.
// It translates the IPSC packet(s) to MMDVM DMRD format and forwards them.
// Callers should use MatchesRules first to determine which network wins,
// then call HandleIPSCBurst only on the winning client.
func (h *MMDVMClient) HandleIPSCBurst(packetType byte, data []byte, addr *net.UDPAddr) bool {
	if !h.started.Load() {
		return false
	}
	slog.Debug("HandleIPSCBurst: received IPSC burst", "network", h.cfg.Name, "type", packetType, "from", addr, "length", len(data))

	packets := h.translator.TranslateToMMDVM(packetType, data)
	matched := false
	for _, pkt := range packets {
		slog.Debug("HandleIPSCBurst: pre-rewrite", "network", h.cfg.Name, "src", pkt.Src, "dst", pkt.Dst, "groupCall", pkt.GroupCall, "slot", pkt.Slot)
		// Apply RF→Net rewrite rules (outbound to this master).
		// Try specific rewrites first; if none match, try passall
		// rules as a fallback.
		if !rewrite.Apply(h.rfRewrites, &pkt) {
			if !rewrite.Apply(h.passallRewrites, &pkt) {
				slog.Debug("HandleIPSCBurst: dropped (no rewrite rule matched)", "network", h.cfg.Name)
				if h.metrics != nil {
					h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "no_rewrite").Inc()
				}
				continue
			}
		}
		slog.Debug("HandleIPSCBurst: post-rewrite", "network", h.cfg.Name, "src", pkt.Src, "dst", pkt.Dst, "groupCall", pkt.GroupCall, "slot", pkt.Slot)

		if h.inboundTSMgr == nil {
			matched = true
			if !h.sendInbound(pkt) {
				return matched
			}
			continue
		}

		end := isTerminator(pkt)
		switch h.inboundTSMgr.Submit(pkt.Slot, pkt.StreamID, "ipsc", pkt, end, h.forwardInbound) {
		case timeslot.Deliver:
			matched = true
			if !h.sendInbound(pkt) {
				return matched
			}
			if end {
				h.inboundTSMgr.Release(pkt.Slot, pkt.StreamID)
			}
		case timeslot.Queued:
			slog.Debug("HandleIPSCBurst: buffered (timeslot busy)",
				"network", h.cfg.Name, "slot", pkt.Slot, "streamID", pkt.StreamID)
		case timeslot.Dropped:
			h.countTimeslotDrop(pkt)
		}
	}
	return matched
}
