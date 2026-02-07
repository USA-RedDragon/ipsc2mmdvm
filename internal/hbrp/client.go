package hbrp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/ipsc2hbrp/internal/config"
	"github.com/USA-RedDragon/ipsc2hbrp/internal/hbrp/proto"
	"github.com/USA-RedDragon/ipsc2hbrp/internal/hbrp/rewrite"
	"github.com/USA-RedDragon/ipsc2hbrp/internal/ipsc"
)

type HBRPClient struct {
	hbrpCfg     *config.HBRP
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

func NewHBRPClient(hbrpCfg *config.HBRP) *HBRPClient {
	tx_chan := make(chan proto.Packet, 256)
	translator, err := ipsc.NewIPSCTranslator()
	if err != nil {
		slog.Warn("failed to load IPSC translator", "error", err)
	}
	c := &HBRPClient{
		hbrpCfg:    hbrpCfg,
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
func (h *HBRPClient) Name() string {
	return h.hbrpCfg.Name
}

// buildRewriteRules constructs the rewrite rule chains from config.
// For each TGRewrite config entry, two rules are created:
//   - rfRewrite: fromSlot/fromTG → toSlot/toTG (for RF→Net direction)
//   - netRewrite: toSlot/toTG → fromSlot/fromTG (reverse, for Net→RF direction)
//
// PCRewrite only creates an RF rewrite (outbound).
// TypeRewrite only creates an RF rewrite (outbound).
// SrcRewrite only creates a Net rewrite (inbound).
func (h *HBRPClient) buildRewriteRules() {
	name := h.hbrpCfg.Name

	for _, cfg := range h.hbrpCfg.TGRewrites {
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

	for _, cfg := range h.hbrpCfg.PCRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.rfRewrites = append(h.rfRewrites, &rewrite.PCRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromID: cfg.FromID,
			ToSlot: cfg.ToSlot, ToID: cfg.ToID, Range: rng,
		})
	}

	for _, cfg := range h.hbrpCfg.TypeRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.rfRewrites = append(h.rfRewrites, &rewrite.TypeRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromTG: cfg.FromTG,
			ToSlot: cfg.ToSlot, ToID: cfg.ToID, Range: rng,
		})
	}

	for _, cfg := range h.hbrpCfg.SrcRewrites {
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

func (h *HBRPClient) Start() error {
	if h.translator != nil {
		h.translator.SetPeerID(h.hbrpCfg.ID)
	}

	slog.Info("Connecting to HBRP server", "network", h.hbrpCfg.Name)

	err := h.connect()
	if err != nil {
		return err
	}

	h.started.Store(true)

	h.wg.Add(4)
	go h.handler()
	go h.rx()
	go h.tx()
	go h.forwardTX()

	h.sendLogin()
	h.state.Store(uint32(STATE_SENT_LOGIN))

	return nil
}

func (h *HBRPClient) connect() error {
	var err error
	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "udp", h.hbrpCfg.MasterServer)
	if err != nil {
		return err
	}
	h.connMu.Lock()
	h.conn = conn
	h.connMu.Unlock()
	return nil
}

func (h *HBRPClient) handler() {
	defer h.wg.Done()
	for {
		select {
		case data := <-h.connRX:
			currentState := h.state.Load()
			switch currentState {
			case uint32(STATE_IDLE):
				slog.Info("Got data from HBRP server while idle", "network", h.hbrpCfg.Name)
			case uint32(STATE_SENT_LOGIN):
				if string(data[:6]) == packetTypeMstack {
					slog.Info("Connected. Authenticating", "network", h.hbrpCfg.Name)
					random := data[len(data)-8:]
					h.sendRPTK(random)
					h.state.Store(uint32(STATE_SENT_AUTH))
				} else {
					slog.Info("Server rejected login request", "network", h.hbrpCfg.Name)
					time.Sleep(1 * time.Second)
					h.sendLogin()
				}
			case uint32(STATE_SENT_AUTH):
				if string(data[:6]) == packetTypeMstack {
					slog.Info("Authenticated. Sending configuration", "network", h.hbrpCfg.Name)
					h.state.Store(uint32(STATE_SENT_RPTC))
					h.sendRPTC()
				} else if string(data[:6]) == "MSTNAK" {
					slog.Info("Password rejected", "network", h.hbrpCfg.Name)
					h.state.Store(uint32(STATE_SENT_LOGIN))
					time.Sleep(1 * time.Second)
					h.sendLogin()
				}
			case uint32(STATE_SENT_RPTC):
				// The data starts with either MSTACK or MSTNAK
				if string(data[:6]) == packetTypeMstack {
					slog.Info("Config accepted, starting ping routine", "network", h.hbrpCfg.Name)
					h.wg.Add(1)
					go h.ping()
					h.state.Store(uint32(STATE_READY))
				} else if string(data[:6]) == "MSTNAK" {
					slog.Info("Configuration rejected", "network", h.hbrpCfg.Name)
					time.Sleep(1 * time.Second)
					h.sendRPTC()
				}
			case uint32(STATE_READY):
				switch string(data[:4]) {
				case "RPTP":
					if string(data[:7]) == "RPTPONG" {
						h.lastPing.Store(time.Now().UnixNano())
					}
				case "RPTS":
					if string(data[:7]) == "RPTSBKN" {
						slog.Info("Server requested a roaming beacon transmission", "network", h.hbrpCfg.Name)
					}
				case "DMRD":
					packet, ok := proto.Decode(data)
					if !ok {
						slog.Info("Error unpacking packet", "network", h.hbrpCfg.Name)
						continue
					}
					slog.Debug("HBRP DMRD received", "network", h.hbrpCfg.Name, "packet", packet)

					// Apply net→RF rewrite rules (inbound from this master)
					if len(h.netRewrites) > 0 {
						if !rewrite.Apply(h.netRewrites, &packet, false) {
							slog.Debug("HBRP DMRD dropped (no rewrite rule matched)", "network", h.hbrpCfg.Name)
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
					slog.Info("Got unknown packet from HBRP server", "network", h.hbrpCfg.Name, "data", data)
				}
			case uint32(STATE_TIMEOUT):
				slog.Info("Got data from HBRP server while in timeout state", "network", h.hbrpCfg.Name)
			}
		case <-h.done:
			return
		}
	}
}

func (h *HBRPClient) ping() {
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
				slog.Info("Connection timed out", "network", h.hbrpCfg.Name)
				h.state.Store(uint32(STATE_TIMEOUT))
				h.connMu.Lock()
				if err := h.conn.Close(); err != nil {
					slog.Error("Error closing connection", "network", h.hbrpCfg.Name, "error", err)
				}
				h.connMu.Unlock()
				if err := h.connect(); err != nil {
					slog.Error("Error reconnecting to HBRP server", "network", h.hbrpCfg.Name, "error", err)
				}
				h.sendLogin()
				h.state.Store(uint32(STATE_SENT_LOGIN))
				return
			}
			h.sendPing()
		case <-h.done:
			return
		}
	}
}

func (h *HBRPClient) tx() {
	defer h.wg.Done()
	for {
		select {
		case <-h.done:
			return
		case data := <-h.connTX:
			h.connMu.Lock()
			_, err := h.conn.Write(data)
			h.connMu.Unlock()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				slog.Error("Error writing to HBRP server", "network", h.hbrpCfg.Name, "error", err)
				continue
			}
		}
	}
}

func (h *HBRPClient) rx() {
	defer h.wg.Done()
	for {
		data := make([]byte, 128)
		h.connMu.Lock()
		conn := h.conn
		h.connMu.Unlock()
		n, err := conn.Read(data)
		if err != nil {
			if !h.started.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("Error reading from HBRP server", "network", h.hbrpCfg.Name, "error", err)
			continue
		}
		select {
		case h.connRX <- data[:n]:
		case <-h.done:
			return
		}
	}
}

func (h *HBRPClient) Stop() {
	h.stopOnce.Do(func() {
		slog.Info("Stopping HBRP client", "network", h.hbrpCfg.Name)

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
func (h *HBRPClient) sendRPTCLDirect() {
	hexid := make([]byte, 8)
	copy(hexid, []byte(fmt.Sprintf("%08x", h.hbrpCfg.ID)))
	data := make([]byte, len("RPTCL")+8)
	n := copy(data, "RPTCL")
	copy(data[n:], hexid)
	if _, err := h.conn.Write(data); err != nil {
		slog.Error("Error sending RPTCL disconnect", "network", h.hbrpCfg.Name, "error", err)
	}
}

func (h *HBRPClient) forwardTX() {
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

func (h *HBRPClient) SetIPSCHandler(handler func(data []byte)) {
	h.ipscHandler = handler
}

// HandleIPSCBurst handles an incoming IPSC burst from the IPSC server.
// This is called when a connected IPSC peer transmits voice/data.
// It translates the IPSC packet(s) to HBRP DMRD format and forwards them.
// If RF rewrite rules are configured, the packet is only forwarded if a rule matches.
func (h *HBRPClient) HandleIPSCBurst(packetType byte, data []byte, addr *net.UDPAddr) {
	if !h.started.Load() {
		return
	}
	slog.Debug("HandleIPSCBurst: received IPSC burst", "network", h.hbrpCfg.Name, "type", packetType, "from", addr, "length", len(data))

	packets := h.translator.TranslateToHBRP(packetType, data)
	for _, pkt := range packets {
		// Apply RF→Net rewrite rules (outbound to this master)
		if len(h.rfRewrites) > 0 {
			if !rewrite.Apply(h.rfRewrites, &pkt, false) {
				slog.Debug("HandleIPSCBurst: dropped (no RF rewrite rule matched)", "network", h.hbrpCfg.Name)
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
