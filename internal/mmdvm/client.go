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
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
)

type MMDVMClient struct {
	cfg         *config.MMDVM
	started     atomic.Bool
	done        chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
	tx_chan     chan proto.Packet
	conn        net.Conn
	connMu      sync.Mutex // protects conn
	state       atomic.Uint32
	connRX      chan []byte
	connTX      chan []byte
	keepAlive   time.Duration
	timeout     time.Duration
	lastPing    atomic.Int64 // UnixNano
	ipscHandler func(data []byte)
	translator  *ipsc.IPSCTranslator

	// Rewrite rules built from config, applied to packets
	// flowing through this network.
	rfRewrites  []rewrite.Rule // RF→Net (outbound to this master)
	netRewrites []rewrite.Rule // Net→RF (inbound from this master)
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

func NewMMDVMClient(cfg *config.MMDVM) *MMDVMClient {
	tx_chan := make(chan proto.Packet, 256)
	translator, err := ipsc.NewIPSCTranslator()
	if err != nil {
		slog.Warn("failed to load IPSC translator", "error", err)
	}
	c := &MMDVMClient{
		cfg:        cfg,
		done:       make(chan struct{}),
		tx_chan:    tx_chan,
		connRX:     make(chan []byte, 16),
		connTX:     make(chan []byte, 16),
		keepAlive:  5 * time.Second,
		timeout:    15 * time.Second,
		translator: translator,
	}
	c.state.Store(uint32(STATE_IDLE))
	c.buildRewriteRules()
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
}

func (h *MMDVMClient) Start() error {
	if h.translator != nil {
		h.translator.SetPeerID(h.cfg.ID)
	}

	slog.Info("Connecting to MMDVM server", "network", h.cfg.Name)

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
			currentState := h.state.Load()
			switch currentState {
			case uint32(STATE_IDLE):
				slog.Info("Got data from MMDVM server while idle", "network", h.cfg.Name)
			case uint32(STATE_SENT_LOGIN):
				if len(data) >= 6 && string(data[:6]) == "RPTACK" {
					if len(data) < 10 {
						slog.Warn("RPTACK response too short", "network", h.cfg.Name, "length", len(data))
						continue
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
			case uint32(STATE_SENT_AUTH):
				if len(data) >= 6 && string(data[:6]) == "RPTACK" {
					slog.Info("Authenticated. Sending configuration", "network", h.cfg.Name)
					h.state.Store(uint32(STATE_SENT_RPTC))
					h.sendRPTC()
				} else if len(data) >= 6 && string(data[:6]) == "RPTNAK" {
					slog.Info("Password rejected", "network", h.cfg.Name)
					h.state.Store(uint32(STATE_SENT_LOGIN))
					time.Sleep(1 * time.Second)
					h.sendLogin()
				}
			case uint32(STATE_SENT_RPTC):
				// The data starts with either MSTACK or MSTNAK
				if len(data) >= 6 && string(data[:6]) == "RPTACK" {
					slog.Info("Config accepted, starting ping routine", "network", h.cfg.Name)
					h.wg.Add(1)
					go h.ping()
					h.state.Store(uint32(STATE_READY))
				} else if len(data) >= 6 && string(data[:6]) == "MSTNAK" {
					slog.Info("Configuration rejected", "network", h.cfg.Name)
					time.Sleep(1 * time.Second)
					h.sendRPTC()
				}
			case uint32(STATE_READY):
				switch string(data[:4]) {
				case "MSTP":
					if len(data) >= 7 && string(data[:7]) == "RPTPONG" {
						h.lastPing.Store(time.Now().UnixNano())
					}
				case "RPTS":
					if len(data) >= 7 && string(data[:7]) == "RPTSBKN" {
						slog.Info("Server requested a roaming beacon transmission", "network", h.cfg.Name)
					}
				case "DMRD":
					packet, ok := proto.Decode(data)
					if !ok {
						slog.Info("Error unpacking packet", "network", h.cfg.Name)
						continue
					}
					slog.Debug("MMDVM DMRD received", "network", h.cfg.Name, "packet", packet)

					// Apply net→RF rewrite rules (inbound from this master)
					if len(h.netRewrites) > 0 {
						if !rewrite.Apply(h.netRewrites, &packet, false) {
							slog.Debug("MMDVM DMRD dropped (no rewrite rule matched)", "network", h.cfg.Name)
							continue
						}
					}

					if h.ipscHandler != nil && h.translator != nil {
						ipscPackets := h.translator.TranslateToIPSC(packet)
						for _, ipscData := range ipscPackets {
							h.ipscHandler(ipscData)
						}
					}
				default:
					slog.Info("Got unknown packet from MMDVM server", "network", h.cfg.Name, "data", data)
				}
			case uint32(STATE_TIMEOUT):
				slog.Info("Got data from MMDVM server while in timeout state", "network", h.cfg.Name)
			}
		case <-h.done:
			return
		}
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
			st := state(h.state.Load())
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
			slog.Debug("sending packet", "data", fmt.Sprintf("%4X", data), "strdata", string(data), "network", h.cfg.Name)
			_, err := h.conn.Write(data)
			h.connMu.Unlock()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					// Connection was closed by reconnect();
					// re-queue the data so it is sent on the
					// new connection, then loop back.
					select {
					case h.connTX <- data:
					default:
						slog.Warn("connTX full, dropping packet during reconnect", "network", h.cfg.Name)
					}
					select {
					case <-time.After(100 * time.Millisecond):
						continue
					case <-h.done:
						return
					}
				}
				slog.Error("Error writing to MMDVM server", "network", h.cfg.Name, "error", err)
				continue
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
	hexid := make([]byte, 8)
	copy(hexid, []byte(fmt.Sprintf("%08x", h.cfg.ID)))
	data := make([]byte, len("RPTCL")+8)
	n := copy(data, "RPTCL")
	copy(data[n:], hexid)
	if _, err := h.conn.Write(data); err != nil {
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

func (h *MMDVMClient) SetIPSCHandler(handler func(data []byte)) {
	h.ipscHandler = handler
}

// HandleIPSCBurst handles an incoming IPSC burst from the IPSC server.
// This is called when a connected IPSC peer transmits voice/data.
// It translates the IPSC packet(s) to MMDVM DMRD format and forwards them.
// If RF rewrite rules are configured, the packet is only forwarded if a rule matches.
func (h *MMDVMClient) HandleIPSCBurst(packetType byte, data []byte, addr *net.UDPAddr) {
	if !h.started.Load() {
		return
	}
	slog.Debug("HandleIPSCBurst: received IPSC burst", "network", h.cfg.Name, "type", packetType, "from", addr, "length", len(data))

	packets := h.translator.TranslateToMMDVM(packetType, data)
	for _, pkt := range packets {
		// Apply RF→Net rewrite rules (outbound to this master)
		if len(h.rfRewrites) > 0 {
			if !rewrite.Apply(h.rfRewrites, &pkt, false) {
				slog.Debug("HandleIPSCBurst: dropped (no RF rewrite rule matched)", "network", h.cfg.Name)
				continue
			}
		}

		select {
		case h.tx_chan <- pkt:
		case <-h.done:
			return
		}
	}
}
