package mmdvm

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

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

func (h *MMDVMClient) sendRPTCL() {
	var (
		data = make([]byte, len("RPTCL")+4)
		n    = copy(data, "RPTCL")
	)
	binary.BigEndian.PutUint32(data[n:], h.cfg.ID)
	h.connTX <- data
}

func (h *MMDVMClient) sendRPTC() {
	str := []byte("RPTC")
	str = append(str, make([]byte, 4)...)
	binary.BigEndian.PutUint32(str[4:], h.cfg.ID)

	// Apply defaults for fields the config library may not handle.
	slots := h.cfg.Slots
	if slots == 0 {
		slots = 3
	}

	str = append(str, []byte(fmt.Sprintf("%-8s", h.cfg.Callsign))...)
	str = append(str, []byte(fmt.Sprintf("%09d", h.cfg.RXFreq))...)
	str = append(str, []byte(fmt.Sprintf("%09d", h.cfg.TXFreq))...)
	str = append(str, []byte(fmt.Sprintf("%02d", h.cfg.TXPower))...)
	str = append(str, []byte(fmt.Sprintf("%02d", h.cfg.ColorCode))...)
	str = append(str, []byte(fmt.Sprintf("%+08.4f", h.cfg.Latitude))...)
	str = append(str, []byte(fmt.Sprintf("%+09.4f", h.cfg.Longitude))...)
	str = append(str, []byte(fmt.Sprintf("%03d", h.cfg.Height))...)
	str = append(str, []byte(fmt.Sprintf("%-20s", h.cfg.Location))...)
	str = append(str, []byte(fmt.Sprintf("%-19s", h.cfg.Description))...)
	str = append(str, []byte(fmt.Sprintf("%d", slots))...)
	str = append(str, []byte(fmt.Sprintf("%-124s", h.cfg.URL))...)
	str = append(str, []byte(fmt.Sprintf("%-40s", "20210921"))...)
	str = append(str, []byte(fmt.Sprintf("%-40s", "MMDVM_MMDVM_HS_Dual_Hat"))...)

	h.connTX <- str
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
	h.connTX <- data
}

func (h *MMDVMClient) sendPacket(packet proto.Packet) {
	data := make([]byte, 53)
	copy(data, packet.Encode())
	h.connTX <- data
}
