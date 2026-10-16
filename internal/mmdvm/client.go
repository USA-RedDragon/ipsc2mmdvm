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

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
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
	connRX       chan rxPacket
	connGen      atomic.Uint64 // bumped whenever conn is closed or replaced
	connTX       chan []byte
	keepAlive    time.Duration
	timeout      time.Duration
	lastHeard    atomic.Int64 // UnixNano — last packet read from the master
	rxWaiting    atomic.Bool  // rx holds a packet the handler has not taken yet
	lastPingSent atomic.Int64 // UnixNano — last RPTPING sent
	ipscHandler  atomic.Pointer[func(ctx context.Context, data []byte)]
	translator   *ipsc.IPSCTranslator
	netLC        *ipsc.LCRewriter // re-encodes LC after netRewrites
	rfLC         *ipsc.LCRewriter // re-encodes LC after rfRewrites
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

	// outData holds the data packet so far of each outbound data call,
	// to find the block that ends it.
	outDataMu sync.Mutex
	outData   map[uint]*outboundDataCall

	// preambles holds the outbound call with preambles on each timeslot,
	// to send its preambles and data as one call. slotStreams holds when
	// each outbound stream on each timeslot was last seen.
	preambleMu  sync.Mutex
	preambles   [2]*preambleCall
	slotStreams [2]map[uint]time.Time

	// inboundSlots holds the timeslot of each inbound call that has not
	// ended, to end a data call whose last packet was dropped.
	inboundMu    sync.Mutex
	inboundSlots map[uint]inboundCall
}

// inboundCall is the timeslot of an IPSC call that has not ended.
type inboundCall struct {
	slot     bool
	lastSeen time.Time
}

// maxInboundCalls bounds inboundSlots; the oldest calls are dropped first.
const maxInboundCalls = 64

// noteInboundSlot records the timeslot, after rewriting, of the inbound
// call streamID, or forgets it when end is set.
func (h *MMDVMClient) noteInboundSlot(streamID uint, slot, end bool) {
	h.inboundMu.Lock()
	defer h.inboundMu.Unlock()
	if end {
		delete(h.inboundSlots, streamID)
		return
	}
	if h.inboundSlots == nil {
		h.inboundSlots = make(map[uint]inboundCall)
	}
	if _, ok := h.inboundSlots[streamID]; !ok && len(h.inboundSlots) >= maxInboundCalls {
		var oldest uint
		var t time.Time
		for id, c := range h.inboundSlots {
			if t.IsZero() || c.lastSeen.Before(t) {
				oldest, t = id, c.lastSeen
			}
		}
		delete(h.inboundSlots, oldest)
	}
	h.inboundSlots[streamID] = inboundCall{slot: slot, lastSeen: time.Now()}
}

// dataCallEnd marks the end of an inbound data call in the timeslot
// manager when the packet that ended it produced nothing for the master.
// forwardInbound ignores it.
type dataCallEnd struct{}

// endInboundCall ends the inbound call streamID, whose last packet was
// dropped, such as a block that failed its CRC or a 0x13 end packet, so
// its timeslot goes to the next call. A call that is waiting behind another
// one ends when its buffered packets have been sent. A call whose end
// packet reached the timeslot manager is no longer tracked, so it is left
// alone.
func (h *MMDVMClient) endInboundCall(streamID uint) {
	h.inboundMu.Lock()
	c, ok := h.inboundSlots[streamID]
	delete(h.inboundSlots, streamID)
	h.inboundMu.Unlock()
	if !ok || h.inboundTSMgr == nil {
		return
	}
	if h.inboundTSMgr.Submit(c.slot, streamID, "ipsc", dataCallEnd{}, true, h.forwardInbound) == timeslot.Deliver {
		h.inboundTSMgr.Release(c.slot, streamID)
	}
}

// outboundDataCall is the data packet so far of one MMDVM data call.
type outboundDataCall struct {
	data     ipsc.DataCall
	lastSeen time.Time
}

// outDataIdle is how long an unfinished outbound data call is kept.
const outDataIdle = 30 * time.Second

// endCallsTimeout bounds how long Stop spends sending IPSC the packets
// that end the calls still open toward it.
const endCallsTimeout = time.Second

// rxPacket is a datagram read from the connection of generation gen.
type rxPacket struct {
	gen  uint64
	data []byte
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
		connRX:       make(chan rxPacket, 16),
		connTX:       make(chan []byte, 16),
		keepAlive:    5 * time.Second,
		timeout:      15 * time.Second,
		translator:   translator,
		netLC:        ipsc.NewLCRewriter(),
		rfLC:         ipsc.NewLCRewriter(),
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
		c.inboundTSMgr.SetMetrics(m, "inbound", cfg.Name)
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
		h.translator.SetColorCode(h.cfg.ColorCode)
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
	h.connGen.Add(1)
	return conn, nil
}

const rptAck = "RPTACK"

const defaultDialTimeout = 10 * time.Second

func (h *MMDVMClient) handler() {
	defer h.wg.Done()
	for {
		select {
		case pkt := <-h.connRX:
			data := pkt.data
			slog.Debug("received packet", "data", fmt.Sprintf("% X", data), "strdata", string(data), "network", h.cfg.Name)
			if len(data) < 4 {
				slog.Warn("Ignoring short packet from MMDVM server", "network", h.cfg.Name, "length", len(data))
				continue
			}
			h.handleState(pkt.gen, data)
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

// handleState handles a packet read from the connection of generation gen.
// A packet from a connection that has since been closed is dropped, so it
// cannot act on the session that replaced it.
func (h *MMDVMClient) handleState(gen uint64, data []byte) {
	h.sessMu.Lock()
	if gen != h.connGen.Load() {
		h.sessMu.Unlock()
		slog.Debug("Dropping packet from a closed connection", "network", h.cfg.Name)
		return
	}
	if len(data) >= 5 && string(data[:5]) == packetTypeMstcl {
		defer h.sessMu.Unlock()
		slog.Warn("Master closed the connection, reconnecting after the retry interval", "network", h.cfg.Name)
		h.reconnect()
		return
	}
	if h.currentState() == STATE_READY {
		if len(data) >= 6 && string(data[:6]) == packetTypeMstnak {
			defer h.sessMu.Unlock()
			h.relogin()
			return
		}
		h.sessMu.Unlock()
		h.handleReady(data)
		return
	}
	defer h.sessMu.Unlock()
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
		h.setState(STATE_SENT_AUTH)
		h.sendRPTK(h.salt)
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

		orig := packet
		if !rewrite.Apply(h.netRewrites, &packet) {
			slog.Debug("MMDVM DMRD dropped (no rewrite rule matched)", "network", h.cfg.Name)
			if h.metrics != nil {
				h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "no_rewrite").Inc()
			}
			return
		}

		h.netLC.Rewrite(orig, &packet)
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
// connection at all. A dropped connection is dialed again only after a
// full keepalive interval, like the DMRGateway retry timer, so a master
// that keeps closing or rejecting the session is not flooded with logins.
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
		if h.intervalsPassed(now, last, 2) {
			h.resentAt = now
			h.resend(st)
		}
	case STATE_TIMEOUT:
		if h.intervalsPassed(now, time.Unix(0, h.stateSince.Load()), 1) {
			h.connectAndLogin()
		}
	case STATE_IDLE:
		h.connectAndLogin()
	}
}

// intervalsPassed reports whether n keepalive intervals have passed from
// since to the tick at now. A state change made just after a tick, such as
// a relogin on the MSTNAK that answers its ping, would otherwise miss the
// tick n intervals later by a few microseconds and wait a whole extra
// interval, so it allows a tenth of an interval of slack.
func (h *MMDVMClient) intervalsPassed(now, since time.Time, n int) bool {
	return now.Sub(since) >= time.Duration(n)*h.keepAlive-h.keepAlive/10
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

// relogin restarts the handshake on the current connection after the
// master rejected an established session. The RPTL goes out with the
// next handshake resend, not at once, so a master that rejects every
// session is not flooded with logins. Must be called with sessMu held.
func (h *MMDVMClient) relogin() {
	slog.Warn("Master rejected the session, logging in again", "network", h.cfg.Name)
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(1)
	}
	h.setState(STATE_SENT_LOGIN)
}

// startLogin begins a new handshake on the current connection. Must be
// called with sessMu held.
func (h *MMDVMClient) startLogin() {
	h.setState(STATE_SENT_LOGIN)
	h.sendLogin()
}

// reconnect closes the current connection. supervise dials a new one and
// starts a fresh login once the retry interval has passed. Must be called
// with sessMu held.
func (h *MMDVMClient) reconnect() {
	h.setState(STATE_TIMEOUT)
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(0)
		h.metrics.MMDVMReconnects.WithLabelValues(h.cfg.Name).Inc()
	}
	h.connMu.Lock()
	if h.conn != nil {
		if err := h.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Error("Error closing connection", "network", h.cfg.Name, "error", err)
		}
		h.conn = nil
		close(h.connStop)
		h.connGen.Add(1)
	}
	h.connMu.Unlock()
	for len(h.connRX) > 0 {
		select {
		case <-h.connRX:
		default:
		}
	}
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
	gen := h.connGen.Load()
	h.connMu.Unlock()
	h.wg.Add(1)
	go h.rx(conn, stop, gen)
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
func (h *MMDVMClient) rx(conn net.Conn, stop <-chan struct{}, gen uint64) {
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
		if !h.deliver(rxPacket{gen: gen, data: data[:n]}, stop) {
			return
		}
	}
}

// deliver hands a packet to the handler. While it waits, rxWaiting tells
// supervise that the master is still talking to us: the handler is busy
// (for example replaying a buffered call), and MSTPONGs queued behind this
// packet are not lost. Returns false when the client stops or the
// connection is replaced. A packet can still slip into the queue while
// the connection is replaced; handleState drops it by its generation.
func (h *MMDVMClient) deliver(pkt rxPacket, stop <-chan struct{}) bool {
	select {
	case <-stop:
		return false
	default:
	}
	select {
	case h.connRX <- pkt:
		return true
	default:
	}
	h.rxWaiting.Store(true)
	defer func() {
		h.lastHeard.Store(time.Now().UnixNano())
		h.rxWaiting.Store(false)
	}()
	select {
	case h.connRX <- pkt:
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

		h.preambleMu.Lock()
		for slot := range h.preambles {
			h.forgetPreambleCallLocked(slot)
		}
		h.preambleMu.Unlock()

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
	h.endOutboundCalls()
}

// endOutboundCalls sends IPSC the packets that end the calls this client
// still has open toward it, so that a repeater ends them at once rather
// than holding the slot. Stop calls it after the goroutines that translate
// have returned, while the IPSC server still runs; it gives up after
// endCallsTimeout.
func (h *MMDVMClient) endOutboundCalls() {
	handler := h.ipscHandler.Load()
	if handler == nil || h.translator == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), endCallsTimeout)
	defer cancel()
	for _, data := range h.translator.EndStreams() {
		if ctx.Err() != nil {
			return
		}
		(*handler)(ctx, data)
	}
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
	if h.ipscHandler.Load() != nil && h.translator != nil {
		h.sendIPSC(h.translator.TranslateToIPSC(packet))
	}
}

// sendIPSC sends IPSC packets through the IPSC handler.
func (h *MMDVMClient) sendIPSC(ipscPackets [][]byte) {
	handler := h.ipscHandler.Load()
	if handler == nil {
		return
	}
	ctx := stopContext{h.done}
	for _, ipscData := range ipscPackets {
		if ctx.Err() != nil {
			return
		}
		(*handler)(ctx, ipscData)
	}
}

// outboundCallEnds reports whether pkt ends its call on the outbound
// timeslot: a voice terminator, a CSBK that nothing of its call follows, or
// the burst that completes a data packet. Data calls have no terminator, so
// without this they hold the slot until it times out.
func (h *MMDVMClient) outboundCallEnds(pkt proto.Packet) bool {
	if pkt.FrameType != frameTypeDataSync {
		return false
	}
	switch enums.DataType(pkt.DTypeOrVSeq) { //nolint:exhaustive,gosec // other data types never end a call; the data type has 4 bits
	case enums.DataTypeTerminatorWithLC:
		return true
	case enums.DataTypeCSBK:
		return h.csbkEndsCall(pkt)
	case enums.DataTypeDataHeader, enums.DataTypeRate12, enums.DataTypeRate34, enums.DataTypeRate1:
	default:
		return false
	}
	h.outDataMu.Lock()
	defer h.outDataMu.Unlock()
	now := time.Now()
	call := h.outData[pkt.StreamID]
	if call == nil {
		for id, c := range h.outData {
			if now.Sub(c.lastSeen) > outDataIdle {
				delete(h.outData, id)
			}
		}
		if h.outData == nil {
			h.outData = make(map[uint]*outboundDataCall)
		}
		call = &outboundDataCall{}
		h.outData[pkt.StreamID] = call
	}
	call.lastSeen = now
	if !ipsc.DataBurstEndsCall(&call.data, pkt.DMRData) {
		return false
	}
	delete(h.outData, pkt.StreamID)
	return true
}

// csbkEndsCall reports whether the CSBK pkt ends its call, which it does
// unless more of the call can follow it: a preamble CSBK announces data or
// CSBKs to follow, and a data packet in progress on the stream has blocks
// to follow. MMDVMHost sends each CSBK and data header with a new stream
// ID, so its CSBKs other than preambles end their call; ipsc2mmdvm sends a
// whole call, preambles and data, with one stream ID.
func (h *MMDVMClient) csbkEndsCall(pkt proto.Packet) bool {
	if isPreambleCSBK(pkt.DMRData) {
		return false
	}
	h.outDataMu.Lock()
	defer h.outDataMu.Unlock()
	call, ok := h.outData[pkt.StreamID]
	return !ok || time.Since(call.lastSeen) > outDataIdle
}

// isPreambleCSBK reports whether data, a CSBK burst, is a preamble CSBK
// (ETSI TS 102 361-2 7.1.2.5).
func isPreambleCSBK(data [33]byte) bool {
	payload, _ := layer2.RawBurst(data).BPTCPayload()
	csbk := pdu.CSBKBytes(payload)
	return csbk.FID() == enums.FIDStandardized && csbk.Opcode() == pdu.CSBKPreamble
}

// stopContext is a context.Context that is canceled when the client stops.
type stopContext struct{ done <-chan struct{} }

func (stopContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c stopContext) Done() <-chan struct{}     { return c.done }
func (stopContext) Value(any) any               { return nil }

func (c stopContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func isTerminator(pkt proto.Packet) bool {
	return pkt.FrameType == frameTypeDataSync && pkt.DTypeOrVSeq == dtypeTerminatorWithLC
}

// submitOutbound passes a packet for the MMDVM→IPSC direction through
// the shared timeslot manager. Buffered packets are later sent by
// forwardOutbound on this client, so a call always uses one translator.
func (h *MMDVMClient) submitOutbound(packet proto.Packet) {
	packet, ended := h.joinPreambleCall(packet)
	if ended != nil {
		h.endOutboundCall(ended)
	}
	end := h.outboundCallEnds(packet)
	if end {
		h.preambleCallEnded(packet)
	}
	if h.outboundTSMgr == nil {
		h.translateAndForwardToIPSC(packet)
		return
	}
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
	switch p := packet.(type) {
	case proto.Packet:
		h.translateAndForwardToIPSC(p)
	case outboundEnd:
		if h.translator != nil {
			h.sendIPSC(h.translator.EndDataCall(p.pkt))
		}
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
// The handler must return soon after its context is done, which happens
// when the client stops. It is safe to call at any time, including after
// Start.
func (h *MMDVMClient) SetIPSCHandler(handler func(ctx context.Context, data []byte)) {
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

	dataEnd := ipsc.EndsDataCall(packetType, data)
	streamID, hadStream := h.translator.ReverseStreamID(data)
	packets := h.translator.TranslateToMMDVM(packetType, data)
	matched := false
	defer func() {
		if dataEnd && hadStream {
			h.endInboundCall(uint(streamID))
		}
	}()
	for i, pkt := range packets {
		slog.Debug("HandleIPSCBurst: pre-rewrite", "network", h.cfg.Name, "src", pkt.Src, "dst", pkt.Dst, "groupCall", pkt.GroupCall, "slot", pkt.Slot)
		// Apply RF→Net rewrite rules (outbound to this master).
		// Try specific rewrites first; if none match, try passall
		// rules as a fallback.
		orig := pkt
		if !rewrite.Apply(h.rfRewrites, &pkt) {
			if !rewrite.Apply(h.passallRewrites, &pkt) {
				slog.Debug("HandleIPSCBurst: dropped (no rewrite rule matched)", "network", h.cfg.Name)
				if h.metrics != nil {
					h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "no_rewrite").Inc()
				}
				continue
			}
		}
		h.rfLC.Rewrite(orig, &pkt)
		slog.Debug("HandleIPSCBurst: post-rewrite", "network", h.cfg.Name, "src", pkt.Src, "dst", pkt.Dst, "groupCall", pkt.GroupCall, "slot", pkt.Slot)

		if h.inboundTSMgr == nil {
			matched = true
			if !h.sendInbound(pkt) {
				return matched
			}
			continue
		}

		end := isTerminator(pkt) || (dataEnd && i == len(packets)-1)
		h.noteInboundSlot(pkt.StreamID, pkt.Slot, end)
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
