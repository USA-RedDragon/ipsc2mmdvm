package mmdvm

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

func (h *MMDVMClient) sendLogin() {
	var (
		data = make([]byte, len("RPTL")+4)
		n    = copy(data, "RPTL")
	)
	binary.BigEndian.PutUint32(data[n:], h.cfg.ID)

	h.connTX <- data
}

// rptclPacket builds the RPTCL disconnect packet: "RPTCL" followed by the
// peer ID as 4 big-endian bytes.
func (h *MMDVMClient) rptclPacket() []byte {
	data := make([]byte, len("RPTCL")+4)
	n := copy(data, "RPTCL")
	binary.BigEndian.PutUint32(data[n:], h.cfg.ID)
	return data
}

// rptcLength is the fixed size of an RPTC configuration packet.
const rptcLength = 302

func (h *MMDVMClient) sendRPTC() {
	h.connTX <- h.rptcPacket()
}

// rptcPacket builds the RPTC configuration packet. Every field has a fixed
// width and offset, so strings are truncated or padded and numbers are
// clamped to the range their field can hold, as MMDVMHost does.
func (h *MMDVMClient) rptcPacket() []byte {
	slots := h.cfg.Slots
	if slots == 0 {
		slots = 3
	}

	buf := make([]byte, 8, rptcLength)
	copy(buf, "RPTC")
	binary.BigEndian.PutUint32(buf[4:], h.cfg.ID)

	field := func(s string, width int) {
		if len(s) > width {
			s = s[:width]
		}
		buf = append(buf, s...)
		for i := len(s); i < width; i++ {
			buf = append(buf, ' ')
		}
	}

	field(h.cfg.Callsign, 8)
	field(fmt.Sprintf("%09d", min(h.cfg.RXFreq, 999999999)), 9)
	field(fmt.Sprintf("%09d", min(h.cfg.TXFreq, 999999999)), 9)
	field(fmt.Sprintf("%02d", min(h.cfg.TXPower, 99)), 2)
	field(fmt.Sprintf("%02d", min(h.cfg.ColorCode, 99)), 2)
	field(fmt.Sprintf("%+08.4f", clampCoord(h.cfg.Latitude, 90)), 8)
	field(fmt.Sprintf("%+09.4f", clampCoord(h.cfg.Longitude, 180)), 9)
	field(fmt.Sprintf("%03d", min(h.cfg.Height, 999)), 3)
	field(h.cfg.Location, 20)
	field(h.cfg.Description, 19)
	field(fmt.Sprintf("%d", min(slots, 9)), 1)
	field(h.cfg.URL, 124)
	field("20210921", 40)
	field("MMDVM_MMDVM_HS_Dual_Hat", 40)

	return buf
}

func clampCoord(v, limit float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(-limit, math.Min(limit, v))
}

func (h *MMDVMClient) sendRPTK(random []byte) {
	// Generate a sha256 hash of the random data and the password
	s256 := sha256.New()
	s256.Write(random)
	s256.Write([]byte(h.cfg.Password))
	token := s256.Sum(nil)

	buf := make([]byte, 40)
	copy(buf[0:4], "RPTK")
	binary.BigEndian.PutUint32(buf[4:8], h.cfg.ID)
	copy(buf[8:], token)
	h.connTX <- buf
}

func (h *MMDVMClient) sendPing() {
	var (
		data = make([]byte, len("RPTPING")+4)
		n    = copy(data, "RPTPING")
	)
	binary.BigEndian.PutUint32(data[n:], h.cfg.ID)
	h.lastPingSent.Store(time.Now().UnixNano())
	h.connTX <- data
}

func (h *MMDVMClient) sendPacket(packet proto.Packet) {
	data := make([]byte, 53)
	copy(data, packet.Encode())
	if h.metrics != nil {
		h.metrics.MMDVMPacketsSent.WithLabelValues(h.cfg.Name).Inc()
	}
	h.connTX <- data
}
