package mmdvm

import (
	"fmt"
	"log/slog"
	"math"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/ipsc"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

// enqueue hands data to tx. It gives up when the client stops, so a full
// connTX can never block Stop.
func (h *MMDVMClient) enqueue(data []byte) {
	select {
	case h.connTX <- data:
	case <-h.done:
	}
}

// rptcPacket builds the RPTC configuration message. Every field has a fixed
// width, so strings are cut and numbers are clamped to the range their
// field can hold, as MMDVMHost does.
func (h *MMDVMClient) rptcPacket() []byte {
	slots := h.cfg.Slots
	if slots == 0 {
		slots = 3
	}
	var c dmrd.MMDVMConfig
	c.Reset()
	c.SetID(h.cfg.ID)
	errs := []error{
		c.SetCallsign(cut(h.cfg.Callsign, 8)),
		c.SetRXFrequency(uint32(min(h.cfg.RXFreq, 999999999))), //nolint:gosec // clamped to 9 digits
		c.SetTXFrequency(uint32(min(h.cfg.TXFreq, 999999999))), //nolint:gosec // clamped to 9 digits
		c.SetPower(min(h.cfg.TXPower, 99)),
		c.SetColorCode(enums.ColorCode(min(h.cfg.ColorCode, 99))),
		c.SetLatitude(cut(fmt.Sprintf("%+08.4f", clampCoord(h.cfg.Latitude, 90)), 8)),
		c.SetLongitude(cut(fmt.Sprintf("%+09.4f", clampCoord(h.cfg.Longitude, 180)), 9)),
		c.SetHeight(min(h.cfg.Height, 999)),
		c.SetLocation(cut(h.cfg.Location, 20)),
		c.SetDescription(cut(h.cfg.Description, 19)),
		c.SetSlots(min(slots, 9)),
		c.SetURL(cut(h.cfg.URL, 124)),
		c.SetSoftwareID([]byte("20210921")),
		c.SetPackageID([]byte("MMDVM_MMDVM_HS_Dual_Hat")),
	}
	for _, err := range errs {
		if err != nil {
			slog.Error("cannot build RPTC", "network", h.cfg.Name, "error", err)
		}
	}
	return c[:]
}

// cut returns the first n octets of s.
func cut(s string, n int) []byte {
	if len(s) > n {
		s = s[:n]
	}
	return []byte(s)
}

func clampCoord(v, limit float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(-limit, math.Min(limit, v))
}

// sendPacket queues a DMRD packet for the master. Like DMRGateway, it only
// sends traffic while logged in.
func (h *MMDVMClient) sendPacket(packet dmrd.Data) {
	if !h.up.Load() {
		slog.Debug("Dropping DMRD, not logged in", "network", h.cfg.Name)
		if h.metrics != nil {
			h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "not_ready").Inc()
		}
		return
	}
	packet.Size = 0
	data, err := packet.AppendBinary(make([]byte, 0, 55))
	if err != nil {
		slog.Warn("Dropping DMRD that does not encode", "network", h.cfg.Name, "error", err)
		return
	}
	if h.metrics != nil {
		h.metrics.MMDVMPacketsSent.WithLabelValues(h.cfg.Name).Inc()
	}
	h.enqueue(data)
}

const (
	txQueueSize   = 256
	masterDialect = dmrd.DialectMMDVM
)

type toMaster struct {
	dmrd.Data
	side sideband
}

type sideband []byte

func (h *MMDVMClient) sidebandOf(lc ipsc.EmbeddedLC) sideband {
	var b []byte
	var err error
	if lc.LC.FLCO == enums.FLCOGPSInfo {
		p := dmrd.Position{Repeater: h.cfg.ID, Source: lc.Source, Data: lc.LC.RawPayload, Size: 18}
		b, err = p.AppendBinary(make([]byte, 0, 18))
	} else {
		ta := dmrd.TalkerAlias{Repeater: h.cfg.ID, Source: lc.Source, Type: uint8(lc.LC.FLCO - enums.FLCOTalkerAliasHeader), Data: lc.LC.RawPayload, Size: 19}
		b, err = ta.AppendBinary(make([]byte, 0, 19))
	}
	if err != nil {
		slog.Debug("Dropping a sideband message that does not encode", "network", h.cfg.Name, "error", err)
		return nil
	}
	if dmrd.Classify(b, masterDialect) == dmrd.CommandUnknown {
		return nil
	}
	return b
}

func (h *MMDVMClient) offerSideband(p sideband) {
	if p == nil {
		return
	}
	if len(h.tx_chan) <= cap(h.tx_chan)/2 {
		select {
		case h.tx_chan <- toMaster{side: p}:
			return
		default:
		}
	}
	slog.Debug("Dropping a sideband message, the queue to the master is busy", "network", h.cfg.Name)
	if h.metrics != nil {
		h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "sideband_busy").Inc()
	}
}

func (h *MMDVMClient) sidebandAlone(lc ipsc.EmbeddedLC) bool {
	pkt := lc.Call
	orig := pkt
	if !rewrite.Apply(h.rfRewrites, &pkt) && !rewrite.Apply(h.passallRewrites, &pkt) {
		if h.metrics != nil {
			h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "no_rewrite").Inc()
		}
		return false
	}
	h.rfLC.Rewrite(orig, &pkt)
	side := h.sidebandOf(lc)
	if h.inboundTSMgr == nil {
		h.offerSideband(side)
		return true
	}
	h.submitSideband(pkt, side)
	return true
}

func (h *MMDVMClient) submitSideband(pkt dmrd.Data, side sideband) {
	if side == nil {
		return
	}
	switch h.inboundTSMgr.Submit(isTS2(pkt.Slot), uint(pkt.Stream), "ipsc", side, false, h.forwardInbound) {
	case timeslot.Deliver:
		h.offerSideband(side)
	case timeslot.Queued:
	case timeslot.Dropped:
		h.countTimeslotDrop(pkt)
	}
}

func (h *MMDVMClient) sendSideband(p sideband) {
	if !h.up.Load() {
		if h.metrics != nil {
			h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "not_ready").Inc()
		}
		return
	}
	if h.metrics != nil {
		h.metrics.MMDVMPacketsSent.WithLabelValues(h.cfg.Name).Inc()
	}
	h.enqueue(p)
}

func (h *MMDVMClient) sidebandFromMaster(cmd dmrd.Command, p []byte) {
	reason := "position"
	var info any
	var err error
	if cmd == dmrd.CommandDMRA {
		reason = "talker_alias"
		info, err = dmrd.DecodeTalkerAlias(p)
	} else {
		info, err = dmrd.DecodePosition(p)
	}
	slog.Debug("Ignoring a repeater sideband packet from the master", "network", h.cfg.Name, "command", cmd.String(), "length", len(p), "packet", info, "error", err)
	if h.metrics != nil {
		h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, reason).Inc()
	}
}
