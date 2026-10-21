package mmdvm

import (
	"fmt"
	"log/slog"
	"math"

	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
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
	var c dmrd.Config
	c.Reset()
	c.SetID(h.cfg.ID)
	errs := []error{
		c.SetCallsign(cut(h.cfg.Callsign, 8)),
		c.SetRXFrequency(uint32(min(h.cfg.RXFreq, 999999999))), //nolint:gosec // clamped to 9 digits
		c.SetTXFrequency(uint32(min(h.cfg.TXFreq, 999999999))), //nolint:gosec // clamped to 9 digits
		c.SetPower(min(h.cfg.TXPower, 99)),
		c.SetColorCode(min(h.cfg.ColorCode, 99)),
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
	data, err := packet.AppendBinary(make([]byte, 0, 53))
	if err != nil {
		slog.Warn("Dropping DMRD that does not encode", "network", h.cfg.Name, "error", err)
		return
	}
	if h.metrics != nil {
		h.metrics.MMDVMPacketsSent.WithLabelValues(h.cfg.Name).Inc()
	}
	h.enqueue(data)
}
