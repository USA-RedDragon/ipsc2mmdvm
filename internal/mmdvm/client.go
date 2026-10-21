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
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd/session"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/config"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

type MMDVMClient struct {
	cfg      *config.MMDVM
	metrics  *metrics.Metrics
	started  atomic.Bool
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	tx_chan  chan dmrd.Data
	conn     net.Conn
	connMu   sync.Mutex    // protects conn and connStop
	connStop chan struct{} // closed when conn is replaced
	connRX   chan rxPacket
	connGen  atomic.Uint64 // bumped whenever conn is closed or replaced
	connTX   chan []byte
	dataRX   chan []byte
	// peerMu guards peer, epoch and pingSent. The session goroutine holds
	// it for each call of the Peer and Stop holds it for Close.
	peerMu      sync.Mutex
	peer        *session.Peer
	epoch       time.Time
	pingSent    time.Time
	up          atomic.Bool
	retry       time.Duration
	timeout     time.Duration
	ipscHandler atomic.Pointer[func(ctx context.Context, data []byte)]
	// ipscOut queues the IPSC packets for forwardIPSC, which closes
	// ipscOutDone when it returns. ipscOnce starts it on first use.
	ipscOnce    sync.Once
	ipscOut     chan []byte
	ipscOutDone chan struct{}
	translator  *ipsc.IPSCTranslator
	netLC       *ipsc.LCRewriter // re-encodes LC after netRewrites
	rfLC        *ipsc.LCRewriter // re-encodes LC after rfRewrites
	dial        func(ctx context.Context, network, address string) (net.Conn, error)
	dialTimeout time.Duration

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
	outData   map[uint32]*outboundDataCall

	// preambles holds the outbound call with preambles on each timeslot,
	// to send its preambles and data as one call. slotStreams holds when
	// each outbound stream on each timeslot was last seen.
	preambleMu  sync.Mutex
	preambles   [2]*preambleCall
	slotStreams [2]map[uint32]time.Time

	// inboundSlots holds the timeslot of each inbound call that has not
	// ended, to end a data call whose last packet was dropped.
	inboundMu    sync.Mutex
	inboundSlots map[uint32]inboundCall
}

// inboundCall is the timeslot of an IPSC call that has not ended.
type inboundCall struct {
	slot     enums.Slot
	lastSeen time.Time
}

// maxInboundCalls bounds inboundSlots; the oldest calls are dropped first.
const maxInboundCalls = 64

// noteInboundSlot records the timeslot, after rewriting, of the inbound
// call streamID, or forgets it when end is set.
func (h *MMDVMClient) noteInboundSlot(streamID uint32, slot enums.Slot, end bool) {
	h.inboundMu.Lock()
	defer h.inboundMu.Unlock()
	if end {
		delete(h.inboundSlots, streamID)
		return
	}
	if h.inboundSlots == nil {
		h.inboundSlots = make(map[uint32]inboundCall)
	}
	if _, ok := h.inboundSlots[streamID]; !ok && len(h.inboundSlots) >= maxInboundCalls {
		var oldest uint32
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
func (h *MMDVMClient) endInboundCall(streamID uint32) {
	h.inboundMu.Lock()
	c, ok := h.inboundSlots[streamID]
	delete(h.inboundSlots, streamID)
	h.inboundMu.Unlock()
	if !ok || h.inboundTSMgr == nil {
		return
	}
	if h.inboundTSMgr.Submit(isTS2(c.slot), uint(streamID), "ipsc", dataCallEnd{}, true, h.forwardInbound) == timeslot.Deliver {
		h.inboundTSMgr.Release(isTS2(c.slot), uint(streamID))
	}
}

// outboundDataCall is the data packet so far of one MMDVM data call.
type outboundDataCall struct {
	data     ipsc.DataCall
	lastSeen time.Time
}

// outDataIdle is how long an unfinished outbound data call is kept.
const outDataIdle = 30 * time.Second

// ipscQueueSize is how many IPSC packets a client queues for the IPSC
// handler, about 4 s of voice on both timeslots.
const ipscQueueSize = 256

// endCallsTimeout bounds how long Stop spends sending IPSC the packets
// that end the calls still open toward it.
const endCallsTimeout = time.Second

// dataQueueSize is how many DMRD packets of the master wait for the
// handler. The session goroutine drops a packet that finds it full rather
// than wait, so the pongs behind it keep the session up.
const dataQueueSize = 256

// rxPacket is a datagram read from the connection of generation gen.
type rxPacket struct {
	gen  uint64
	data []byte
}

func NewMMDVMClient(cfg *config.MMDVM, m *metrics.Metrics) *MMDVMClient {
	tx_chan := make(chan dmrd.Data, 256)
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
		dataRX:       make(chan []byte, dataQueueSize),
		translator:   translator,
		netLC:        ipsc.NewLCRewriter(),
		rfLC:         ipsc.NewLCRewriter(),
		inboundTSMgr: timeslot.NewManager(),
		dial:         (&net.Dialer{}).DialContext,
		dialTimeout:  defaultDialTimeout,
	}
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
// until it answers. Start fails if the master address is malformed or the
// session config is out of range.
func (h *MMDVMClient) Start() error {
	_, port, err := net.SplitHostPort(h.cfg.MasterServer)
	if err != nil {
		return fmt.Errorf("invalid master server address %q: %w", h.cfg.MasterServer, err)
	}
	if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
		return fmt.Errorf("invalid master server port in %q", h.cfg.MasterServer)
	}
	if err := h.newPeer(); err != nil {
		return err
	}

	if h.translator != nil {
		h.translator.SetPeerID(h.cfg.ID)
		h.translator.SetColorCode(h.cfg.ColorCode)
	}

	slog.Info("Connecting to MMDVM server", "network", h.cfg.Name)
	h.setConnState(1)
	h.started.Store(true)

	h.wg.Add(4)
	go h.handler()
	go h.tx()
	go h.forwardTX()
	go h.session()

	return nil
}

// newPeer makes the session of the MMDVM network protocol, which
// DMRGateway speaks and BrandMeister, HBlink, FreeDMR and DMRHub masters
// accept.
func (h *MMDVMClient) newPeer() error {
	p, err := session.NewPeer(&session.PeerConfig{
		Dialect:  dmrd.DialectMMDVM,
		ID:       h.cfg.ID,
		Password: []byte(h.cfg.Password),
		Config:   h.rptcPacket(),
		Retry:    h.retry,
		Timeout:  h.timeout,
	})
	if err != nil {
		return fmt.Errorf("network %s: %w", h.cfg.Name, err)
	}
	h.peer = p
	h.epoch = time.Now()
	return nil
}

func (h *MMDVMClient) clock() time.Duration {
	return time.Since(h.epoch)
}

func (h *MMDVMClient) setConnState(v float64) {
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(v)
	}
}

// session runs the Peer: it feeds it the packets of the master and its
// timers until Stop. The packets that wait are taken before each Tick, so
// a pong that came in time never loses to the timeout.
func (h *MMDVMClient) session() {
	defer h.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	out := make([]session.Output, 0, session.MaxOutputs)
	for {
		h.peerMu.Lock()
		at, ok := h.peer.Deadline()
		h.peerMu.Unlock()
		if ok {
			timer.Reset(max(0, at-h.clock()))
		}
		select {
		case <-h.done:
			return
		case pkt := <-h.connRX:
			out = h.fromMaster(pkt, out)
		case <-timer.C:
			for drained := false; !drained; {
				select {
				case pkt := <-h.connRX:
					out = h.fromMaster(pkt, out)
				default:
					drained = true
				}
			}
			h.peerMu.Lock()
			out = h.peer.Tick(h.clock(), out[:0])
			h.outputs(out)
			h.peerMu.Unlock()
		}
	}
}

// fromMaster passes a packet of the master to the Peer. A packet from a
// connection that has since been closed is dropped, so it cannot act on
// the session that replaced it.
func (h *MMDVMClient) fromMaster(pkt rxPacket, out []session.Output) []session.Output {
	if pkt.gen != h.connGen.Load() {
		slog.Debug("Dropping packet from a closed connection", "network", h.cfg.Name)
		return out
	}
	h.peerMu.Lock()
	defer h.peerMu.Unlock()
	if dmrd.Classify(pkt.data, dmrd.DialectMMDVM) == dmrd.CommandMSTPONG && h.metrics != nil && !h.pingSent.IsZero() {
		h.metrics.MMDVMPingRTT.WithLabelValues(h.cfg.Name).Observe(time.Since(h.pingSent).Seconds())
		h.pingSent = time.Time{}
	}
	out = h.peer.Packet(h.clock(), pkt.data, out[:0])
	h.outputs(out)
	return out
}

// outputs acts on the outputs of the Peer. Must be called with peerMu
// held.
func (h *MMDVMClient) outputs(out []session.Output) {
	for _, o := range out {
		switch o.Kind {
		case session.OutputSend:
			h.sendControl(o.Packet)
		case session.OutputUp:
			slog.Info("Logged in", "network", h.cfg.Name)
			h.up.Store(true)
			h.setConnState(2)
		case session.OutputDown:
			h.down(o)
		case session.OutputData:
			select {
			case h.dataRX <- o.Packet:
			default:
				slog.Warn("DMRD queue full, dropping packet", "network", h.cfg.Name)
				if h.metrics != nil {
					h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "handler_busy").Inc()
				}
			}
		case session.OutputMessage:
			if o.Command == dmrd.CommandRPTSBKN {
				slog.Info("Server requested a roaming beacon transmission", "network", h.cfg.Name)
			}
		case session.OutputLogin, session.OutputConfig, session.OutputOptions, session.OutputPong:
		}
	}
}

// sendControl queues a copy of a login step or ping of the Peer. It dials
// the master first when there is no connection, so a failed dial is tried
// again with the next send of the Peer.
func (h *MMDVMClient) sendControl(p []byte) {
	h.connMu.Lock()
	connected := h.conn != nil
	h.connMu.Unlock()
	if !connected && !h.connect() {
		return
	}
	if dmrd.Classify(p, dmrd.DialectMMDVM) == dmrd.CommandRPTPING {
		h.pingSent = time.Now()
	}
	h.enqueue(append([]byte(nil), p...))
}

// down acts on the end of a session. After a timeout or an MSTCL the
// master may have moved or restarted, so the next login dials it again;
// after an MSTNAK the login goes again on the same connection.
func (h *MMDVMClient) down(o session.Output) {
	h.up.Store(false)
	slog.Warn("Session with the master ended", "network", h.cfg.Name, "reason", o.Reason.String(), "state", o.State.String())
	if o.Reason == session.ReasonRefused && o.State == session.StateAuth && h.metrics != nil {
		h.metrics.MMDVMAuthFailures.WithLabelValues(h.cfg.Name).Inc()
	}
	switch o.Reason { //nolint:exhaustive // the other reasons keep the connection
	case session.ReasonTimeout, session.ReasonClosed:
		h.reconnect()
	default:
		h.setConnState(1)
	}
}

// reconnect closes the current connection, so the next send of the Peer
// dials a new one. Must be called with peerMu held.
func (h *MMDVMClient) reconnect() {
	h.setConnState(0)
	if h.metrics != nil {
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

// connect dials the master, installs the new connection and starts rx for
// it.
func (h *MMDVMClient) connect() bool {
	conn, err := h.dialMaster()
	if err != nil {
		select {
		case <-h.done:
		default:
			slog.Error("Error connecting to MMDVM server", "network", h.cfg.Name, "error", err)
		}
		return false
	}
	h.connMu.Lock()
	stop := h.connStop
	gen := h.connGen.Load()
	h.connMu.Unlock()
	h.wg.Add(1)
	go h.rx(conn, stop, gen)
	h.setConnState(1)
	return true
}

// dialMaster dials the master and installs the new connection. The dial is
// bounded by dialTimeout and canceled by Stop.
func (h *MMDVMClient) dialMaster() (net.Conn, error) {
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

const defaultDialTimeout = 10 * time.Second

// handler passes the DMRD packets of the master to the bridge, off the
// session goroutine.
func (h *MMDVMClient) handler() {
	defer h.wg.Done()
	for {
		select {
		case data := <-h.dataRX:
			h.handleReady(data)
		case <-h.done:
			return
		}
	}
}

func (h *MMDVMClient) handleReady(data []byte) {
	packet, err := dmrd.DecodeData(data)
	if err != nil {
		slog.Info("Error unpacking packet", "network", h.cfg.Name, "error", err)
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

// rx reads from one connection until it is closed and hands each packet to
// the session goroutine.
func (h *MMDVMClient) rx(conn net.Conn, stop <-chan struct{}, gen uint64) {
	defer h.wg.Done()
	for {
		data := make([]byte, session.MaxPacket+1)
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
		select {
		case h.connRX <- rxPacket{gen: gen, data: data[:n]}:
		case <-stop:
			return
		case <-h.done:
			return
		}
	}
}

func (h *MMDVMClient) Stop() {
	h.stopOnce.Do(func() {
		slog.Info("Stopping MMDVM client", "network", h.cfg.Name)

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

		h.closeSession()
		h.started.Store(false)
	})

	h.wg.Wait()
	h.ipscQueue()
	<-h.ipscOutDone
	h.endOutboundCalls()
}

// closeSession ends the session with the RPTCL of Peer.Close, written
// straight on the connection, and closes the connection.
func (h *MMDVMClient) closeSession() {
	h.peerMu.Lock()
	defer h.peerMu.Unlock()
	var out []session.Output
	if h.peer != nil {
		out = h.peer.Close(make([]session.Output, 0, session.MaxOutputs))
	}
	h.up.Store(false)
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.conn == nil {
		return
	}
	for _, o := range out {
		if o.Kind != session.OutputSend {
			continue
		}
		if _, err := h.conn.Write(o.Packet); err != nil {
			slog.Error("Error sending RPTCL disconnect", "network", h.cfg.Name, "error", err)
		}
	}
	_ = h.conn.Close()
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

// translateAndForwardToIPSC converts a dmrd.Data to IPSC and sends it.
func (h *MMDVMClient) translateAndForwardToIPSC(packet dmrd.Data) {
	if h.ipscHandler.Load() != nil && h.translator != nil {
		h.sendIPSC(h.translator.TranslateToIPSC(packet))
	}
}

// sendIPSC queues IPSC packets for the IPSC handler, which forwardIPSC
// calls in order. It never waits for the handler, which paces its sends,
// so a slow send cannot hold up the goroutine that reads the master. A
// packet that finds the queue full is dropped.
func (h *MMDVMClient) sendIPSC(ipscPackets [][]byte) {
	if h.ipscHandler.Load() == nil {
		return
	}
	queue := h.ipscQueue()
	for _, ipscData := range ipscPackets {
		select {
		case <-h.done:
			return
		case queue <- ipscData:
		default:
			slog.Warn("IPSC send queue full, dropping packet", "network", h.cfg.Name)
			if h.metrics != nil {
				h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "ipsc_queue_full").Inc()
			}
		}
	}
}

// ipscQueue returns the queue of forwardIPSC, and starts it the first time.
func (h *MMDVMClient) ipscQueue() chan<- []byte {
	h.ipscOnce.Do(func() {
		h.ipscOut = make(chan []byte, ipscQueueSize)
		h.ipscOutDone = make(chan struct{})
		go h.forwardIPSC()
	})
	return h.ipscOut
}

// forwardIPSC passes the packets that sendIPSC queues to the IPSC handler,
// one at a time in order, until the client stops.
func (h *MMDVMClient) forwardIPSC() {
	defer close(h.ipscOutDone)
	ctx := stopContext{h.done}
	for {
		select {
		case <-h.done:
			return
		case data := <-h.ipscOut:
			if handler := h.ipscHandler.Load(); handler != nil {
				(*handler)(ctx, data)
			}
		}
	}
}

// OutboundPacketKind tells the outbound timeslot manager what packet is,
// for the holds of timeslot.Manager.SetCallHang: a voice burst, voice LC
// header, PI header or terminator with LC is voice, a data header or a
// rate 1/2, 3/4 or 1 block is data, and anything else, such as a CSBK or
// the end of a call of preambles, is neither.
func OutboundPacketKind(packet any) timeslot.PacketKind {
	p, ok := packet.(dmrd.Data)
	if !ok {
		return timeslot.KindOther
	}
	if p.Frame != dmrd.FrameDataSync {
		if p.Frame < dmrd.FrameDataSync {
			return timeslot.KindVoice
		}
		return timeslot.KindOther
	}
	switch enums.DataType(p.Value) { //nolint:exhaustive,gosec // the other data types are neither; the data type has 4 bits
	case enums.DataTypeVoiceLCHeader, enums.DataTypePIHeader, enums.DataTypeTerminatorWithLC:
		return timeslot.KindVoice
	case enums.DataTypeDataHeader, enums.DataTypeRate12, enums.DataTypeRate34, enums.DataTypeRate1:
		return timeslot.KindData
	}
	return timeslot.KindOther
}

// outboundCallEnds reports whether pkt ends its call on the outbound
// timeslot: a voice terminator, a CSBK that nothing of its call follows, or
// the burst that completes a data packet. Data calls have no terminator, so
// without this they hold the slot until it times out.
func (h *MMDVMClient) outboundCallEnds(pkt dmrd.Data) bool {
	if pkt.Frame != dmrd.FrameDataSync {
		return false
	}
	switch enums.DataType(pkt.Value) { //nolint:exhaustive,gosec // other data types never end a call; the data type has 4 bits
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
	call := h.outData[pkt.Stream]
	if call == nil {
		for id, c := range h.outData {
			if now.Sub(c.lastSeen) > outDataIdle {
				delete(h.outData, id)
			}
		}
		if h.outData == nil {
			h.outData = make(map[uint32]*outboundDataCall)
		}
		call = &outboundDataCall{}
		h.outData[pkt.Stream] = call
	}
	call.lastSeen = now
	if !ipsc.DataBurstEndsCall(&call.data, pkt.Burst) {
		return false
	}
	delete(h.outData, pkt.Stream)
	return true
}

// csbkEndsCall reports whether the CSBK pkt ends its call, which it does
// unless more of the call can follow it: a preamble CSBK announces data or
// CSBKs to follow, and a data packet in progress on the stream has blocks
// to follow. MMDVMHost sends each CSBK and data header with a new stream
// ID, so its CSBKs other than preambles end their call; ipsc2mmdvm sends a
// whole call, preambles and data, with one stream ID.
func (h *MMDVMClient) csbkEndsCall(pkt dmrd.Data) bool {
	if isPreambleCSBK(pkt.Burst) {
		return false
	}
	h.outDataMu.Lock()
	defer h.outDataMu.Unlock()
	call, ok := h.outData[pkt.Stream]
	return !ok || time.Since(call.lastSeen) > outDataIdle
}

// isPreambleCSBK reports whether data, a CSBK burst, is a preamble CSBK
// (ETSI TS 102 361-2 7.1.2.5).
func isPreambleCSBK(data layer2.RawBurst) bool {
	payload, _ := data.BPTCPayload()
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

// isTS2 reports whether slot is timeslot 2, as the timeslot managers take
// it.
func isTS2(slot enums.Slot) bool {
	return slot == enums.Slot2
}

func isTerminator(pkt dmrd.Data) bool {
	return pkt.Frame == dmrd.FrameDataSync && pkt.Value == uint8(enums.DataTypeTerminatorWithLC)
}

// submitOutbound passes a packet for the MMDVM→IPSC direction through
// the shared timeslot manager. Buffered packets are later sent by
// forwardOutbound on this client, so a call always uses one translator.
func (h *MMDVMClient) submitOutbound(packet dmrd.Data) {
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
	switch h.outboundTSMgr.Submit(isTS2(packet.Slot), uint(packet.Stream), h.cfg.Name, packet, end, h.forwardOutbound) {
	case timeslot.Deliver:
		h.translateAndForwardToIPSC(packet)
		if end {
			h.outboundTSMgr.Release(isTS2(packet.Slot), uint(packet.Stream))
		}
	case timeslot.Queued:
		slog.Debug("MMDVM DMRD buffered (timeslot busy)",
			"network", h.cfg.Name, "slot", packet.Slot, "streamID", packet.Stream)
	case timeslot.Dropped:
		h.countTimeslotDrop(packet)
	}
}

func (h *MMDVMClient) countTimeslotDrop(pkt dmrd.Data) {
	slog.Debug("packet dropped (timeslot buffer full)",
		"network", h.cfg.Name, "slot", pkt.Slot, "streamID", pkt.Stream)
	if h.metrics != nil {
		h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "timeslot_full").Inc()
	}
}

func (h *MMDVMClient) forwardOutbound(packet any) {
	switch p := packet.(type) {
	case dmrd.Data:
		h.translateAndForwardToIPSC(p)
	case outboundEnd:
		if h.translator != nil {
			h.sendIPSC(h.translator.EndDataCall(p.pkt))
		}
	}
}

func (h *MMDVMClient) forwardInbound(packet any) {
	if pkt, ok := packet.(dmrd.Data); ok {
		h.sendInbound(pkt)
	}
}

// sendInbound queues a packet for the MMDVM master. It returns false
// if the client is stopping.
func (h *MMDVMClient) sendInbound(pkt dmrd.Data) bool {
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
	u, err := ipscpkt.DecodeUser(data)
	if err != nil {
		return false
	}
	// Build a probe packet from IPSC header fields.
	probe := dmrd.Data{
		Source:      u.Source,
		Destination: u.Destination,
		Private:     u.Opcode != ipscpkt.OpGroupVoice && u.Opcode != ipscpkt.OpGroupData,
		Slot:        u.Info.Slot(),
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
			h.endInboundCall(streamID)
		}
	}()
	for i, pkt := range packets {
		slog.Debug("HandleIPSCBurst: pre-rewrite", "network", h.cfg.Name, "src", pkt.Source, "dst", pkt.Destination, "groupCall", !pkt.Private, "slot", pkt.Slot)
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
		slog.Debug("HandleIPSCBurst: post-rewrite", "network", h.cfg.Name, "src", pkt.Source, "dst", pkt.Destination, "groupCall", !pkt.Private, "slot", pkt.Slot)

		if h.inboundTSMgr == nil {
			matched = true
			if !h.sendInbound(pkt) {
				return matched
			}
			continue
		}

		end := isTerminator(pkt) || (dataEnd && i == len(packets)-1)
		h.noteInboundSlot(pkt.Stream, pkt.Slot, end)
		switch h.inboundTSMgr.Submit(isTS2(pkt.Slot), uint(pkt.Stream), "ipsc", pkt, end, h.forwardInbound) {
		case timeslot.Deliver:
			matched = true
			if !h.sendInbound(pkt) {
				return matched
			}
			if end {
				h.inboundTSMgr.Release(isTS2(pkt.Slot), uint(pkt.Stream))
			}
		case timeslot.Queued:
			slog.Debug("HandleIPSCBurst: buffered (timeslot busy)",
				"network", h.cfg.Name, "slot", pkt.Slot, "streamID", pkt.Stream)
		case timeslot.Dropped:
			h.countTimeslotDrop(pkt)
		}
	}
	return matched
}
