package mmdvm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	connMu       sync.Mutex // protects conn
	state        atomic.Uint32
	connRX       chan []byte
	connTX       chan []byte
	keepAlive    time.Duration
	timeout      time.Duration
	lastPing     atomic.Int64 // UnixNano — last MSTPONG received
	lastPingSent atomic.Int64 // UnixNano — last RPTPING sent
	ipscHandler  atomic.Pointer[func(data []byte)]
	translator   *ipsc.IPSCTranslator

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
			ToSlot: cfg.ToSlot, ToID: cfg.ToID, Range: rng,
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

func (h *MMDVMClient) Start() error {
	if h.translator != nil {
		h.translator.SetPeerID(h.cfg.ID)
	}

	slog.Info("Connecting to MMDVM server", "network", h.cfg.Name)

	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(1)
	}

	err := h.connect()
	if err != nil {
		return err
	}

	h.started.Store(true)

	h.wg.Add(5)
	go h.handler()
	go h.rx()
	go h.tx()
	go h.forwardTX()
	go h.handshakeWatchdog()

	h.state.Store(uint32(STATE_SENT_LOGIN))
	h.sendLogin()

	return nil
}

func (h *MMDVMClient) connect() error {
	var err error
	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "udp", h.cfg.MasterServer)
	if err != nil {
		return err
	}
	h.connMu.Lock()
	h.conn = conn
	h.connMu.Unlock()
	return nil
}

const rptAck = "RPTACK"

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

func (h *MMDVMClient) handleState(data []byte) {
	currentState := h.state.Load()
	switch currentState {
	case uint32(STATE_IDLE):
		slog.Info("Got data from MMDVM server while idle", "network", h.cfg.Name)
	case uint32(STATE_SENT_LOGIN):
		h.handleSentLogin(data)
	case uint32(STATE_SENT_AUTH):
		h.handleSentAuth(data)
	case uint32(STATE_SENT_RPTC):
		h.handleSentRPTC(data)
	case uint32(STATE_READY):
		h.handleReady(data)
	case uint32(STATE_TIMEOUT):
		slog.Info("Got data from MMDVM server while in timeout state", "network", h.cfg.Name)
	}
}

func (h *MMDVMClient) handleSentLogin(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		if len(data) < 10 {
			slog.Warn("RPTACK response too short", "network", h.cfg.Name, "length", len(data))
			return
		}
		slog.Info("Connected. Authenticating", "network", h.cfg.Name)
		random := data[len(data)-4:]
		h.sendRPTK(random)
		h.state.Store(uint32(STATE_SENT_AUTH))
	} else {
		slog.Info("Server rejected login request", "network", h.cfg.Name)
		time.Sleep(1 * time.Second)
		h.sendLogin()
	}
}

func (h *MMDVMClient) handleSentAuth(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		slog.Info("Authenticated. Sending configuration", "network", h.cfg.Name)
		h.state.Store(uint32(STATE_SENT_RPTC))
		h.sendRPTC()
	} else if len(data) >= 6 && string(data[:6]) == "RPTNAK" {
		slog.Info("Password rejected", "network", h.cfg.Name)
		if h.metrics != nil {
			h.metrics.MMDVMAuthFailures.WithLabelValues(h.cfg.Name).Inc()
		}
		h.state.Store(uint32(STATE_SENT_LOGIN))
		time.Sleep(1 * time.Second)
		h.sendLogin()
	}
}

func (h *MMDVMClient) handleSentRPTC(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		slog.Info("Config accepted, starting ping routine", "network", h.cfg.Name)
		h.state.Store(uint32(STATE_READY))
		if h.metrics != nil {
			h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(2)
		}
		h.wg.Add(1)
		go h.ping()
	} else if len(data) >= 6 && string(data[:6]) == "MSTNAK" {
		slog.Info("Configuration rejected", "network", h.cfg.Name)
		time.Sleep(1 * time.Second)
		h.sendRPTC()
	}
}

func (h *MMDVMClient) handleReady(data []byte) {
	switch string(data[:4]) {
	case "MSTP":
		if len(data) >= 7 && string(data[:7]) == "MSTPONG" {
			now := time.Now()
			if h.metrics != nil {
				sent := time.Unix(0, h.lastPingSent.Load())
				if !sent.IsZero() {
					h.metrics.MMDVMPingRTT.WithLabelValues(h.cfg.Name).Observe(now.Sub(sent).Seconds())
				}
			}
			h.lastPing.Store(now.UnixNano())
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

func (h *MMDVMClient) ping() {
	defer h.wg.Done()
	ticker := time.NewTicker(h.keepAlive)
	defer ticker.Stop()
	h.sendPing()
	h.lastPing.Store(time.Now().UnixNano())
	for {
		select {
		case <-ticker.C:
			lastPingTime := time.Unix(0, h.lastPing.Load())
			if time.Now().After(lastPingTime.Add(h.timeout)) {
				slog.Info("Connection timed out", "network", h.cfg.Name)
				h.reconnect()
				return
			}
			h.sendPing()
		case <-h.done:
			return
		}
	}
}

// handshakeWatchdog monitors the login/auth/config handshake and
// triggers a reconnect if the client doesn't reach STATE_READY
// within the timeout period. Once STATE_READY is reached the ping()
// goroutine takes over liveness monitoring.
func (h *MMDVMClient) handshakeWatchdog() {
	defer h.wg.Done()
	ticker := time.NewTicker(h.timeout)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			st := state(h.state.Load() & 0xFF) //nolint:gosec
			if st == STATE_READY {
				// Handshake completed, ping() is now responsible.
				return
			}
			slog.Warn("Handshake timed out, reconnecting", "network", h.cfg.Name, "state", st)
			h.reconnect()
			// Stay in the loop to watch the next handshake attempt.
		case <-h.done:
			return
		}
	}
}

// reconnect closes the current connection, dials a new one, and
// sends a fresh login. It is safe to call from any goroutine.
func (h *MMDVMClient) reconnect() {
	h.state.Store(uint32(STATE_TIMEOUT))
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(0)
		h.metrics.MMDVMReconnects.WithLabelValues(h.cfg.Name).Inc()
	}
	h.connMu.Lock()
	if h.conn != nil {
		if err := h.conn.Close(); err != nil {
			slog.Error("Error closing connection", "network", h.cfg.Name, "error", err)
		}
	}
	h.connMu.Unlock()
	if err := h.connect(); err != nil {
		slog.Error("Error reconnecting to MMDVM server", "network", h.cfg.Name, "error", err)
	}
	h.state.Store(uint32(STATE_SENT_LOGIN))
	h.sendLogin()
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

func (h *MMDVMClient) rx() {
	defer h.wg.Done()
	for {
		h.connMu.Lock()
		conn := h.conn
		h.connMu.Unlock()
		data := make([]byte, 512)
		n, err := conn.Read(data)
		if err != nil {
			if !h.started.Load() {
				return
			}
			// If the connection was closed (e.g. by ping() during
			// reconnect), loop back and pick up the new h.conn
			// instead of exiting the goroutine.
			if errors.Is(err, net.ErrClosed) {
				// Small sleep to avoid a tight loop while the
				// reconnect in ping() replaces h.conn.
				select {
				case <-time.After(100 * time.Millisecond):
					continue
				case <-h.done:
					return
				}
			}
			slog.Error("Error reading from MMDVM server", "network", h.cfg.Name, "error", err)
			continue
		}
		select {
		case h.connRX <- data[:n]:
		case <-h.done:
			return
		}
	}
}

func (h *MMDVMClient) Stop() {
	h.stopOnce.Do(func() {
		slog.Info("Stopping MMDVM client", "network", h.cfg.Name)

		// Signal all goroutines to stop.
		close(h.done)

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
