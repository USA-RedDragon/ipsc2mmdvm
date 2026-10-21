package mmdvm

import (
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd/auth"
)

// dialect is the form of the control messages the client speaks: the MMDVM
// network protocol of DMRGateway, which BrandMeister, HBlink, FreeDMR and
// DMRHub masters accept.
const dialect = dmrd.DialectMMDVM

// enqueue hands data to tx. It gives up when the client stops, so a full
// connTX can never block Stop.
func (h *MMDVMClient) enqueue(data []byte) {
	select {
	case h.connTX <- data:
	case <-h.done:
	}
}

// control returns the control message cmd with the repeater ID.
func (h *MMDVMClient) control(cmd dmrd.Command) []byte {
	data, err := dmrd.Control{Command: cmd, ID: h.cfg.ID}.AppendBinary(nil, dialect)
	if err != nil {
		slog.Error("cannot build control message", "network", h.cfg.Name, "command", cmd, "error", err)
	}
	return data
}

func (h *MMDVMClient) sendLogin() {
	h.enqueue(h.control(dmrd.CommandRPTL))
}

// rptclPacket builds the RPTCL disconnect message.
func (h *MMDVMClient) rptclPacket() []byte {
	return h.control(dmrd.CommandRPTCL)
}

func (h *MMDVMClient) sendRPTC() {
	h.enqueue(h.rptcPacket())
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

// sendRPTK sends the login token for the salt of the master's RPTACK.
func (h *MMDVMClient) sendRPTK(salt uint32) {
	m := dmrd.Control{Command: dmrd.CommandRPTK, ID: h.cfg.ID}
	var err error
	if m.Token, err = auth.Token(dialect, salt, []byte(h.cfg.Password)); err != nil {
		slog.Error("cannot compute login token", "network", h.cfg.Name, "error", err)
	}
	data, err := m.AppendBinary(nil, dialect)
	if err != nil {
		slog.Error("cannot build RPTK", "network", h.cfg.Name, "error", err)
	}
	h.enqueue(data)
}

func (h *MMDVMClient) sendPing() {
	h.lastPingSent.Store(time.Now().UnixNano())
	h.enqueue(h.control(dmrd.CommandRPTPING))
}

// sendPacket queues a DMRD packet for the master. Like DMRGateway, it only
// sends traffic while logged in.
func (h *MMDVMClient) sendPacket(packet dmrd.Data) {
	if h.currentState() != STATE_READY {
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
