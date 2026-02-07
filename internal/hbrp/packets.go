package hbrp

import (
	"crypto/sha256"
	"fmt"

	"github.com/USA-RedDragon/ipsc2hbrp/internal/hbrp/proto"
)

func (h *HBRPClient) sendLogin() {
	hexid := make([]byte, 8)
	copy(hexid, []byte(fmt.Sprintf("%08x", h.hbrpCfg.ID)))
	var (
		data = make([]byte, len("RPTL")+8)
		n    = copy(data, "RPTL")
	)
	copy(data[n:], hexid)
	h.connTX <- data
}

func (h *HBRPClient) sendRPTCL() {
	hexid := make([]byte, 8)
	copy(hexid, []byte(fmt.Sprintf("%08x", h.hbrpCfg.ID)))
	var (
		data = make([]byte, len("RPTCL")+8)
		n    = copy(data, "RPTCL")
	)
	copy(data[n:], hexid)
	h.connTX <- data
}

func (h *HBRPClient) sendRPTC() {
	str := []byte("RPTC")
	str = append(str, []byte(fmt.Sprintf("%-8s", h.hbrpCfg.Callsign))...)
	str = append(str, []byte(fmt.Sprintf("%08x", h.hbrpCfg.ID))...)
	str = append(str, []byte(fmt.Sprintf("%09d", h.hbrpCfg.RXFreq))...)
	str = append(str, []byte(fmt.Sprintf("%09d", h.hbrpCfg.TXFreq))...)
	str = append(str, []byte(fmt.Sprintf("%02d", h.hbrpCfg.TXPower))...)
	str = append(str, []byte(fmt.Sprintf("%02d", h.hbrpCfg.ColorCode))...)
	str = append(str, []byte(fmt.Sprintf("%-08f", h.hbrpCfg.Latitude)[:8])...)
	str = append(str, []byte(fmt.Sprintf("%-09f", h.hbrpCfg.Longitude)[:9])...)
	str = append(str, []byte(fmt.Sprintf("%03d", h.hbrpCfg.Height))...)
	str = append(str, []byte(fmt.Sprintf("%-20s", h.hbrpCfg.Location))...)
	str = append(str, []byte(fmt.Sprintf("%-20s", h.hbrpCfg.Description))...)
	str = append(str, []byte(fmt.Sprintf("%-124s", h.hbrpCfg.URL))...)
	str = append(str, []byte(fmt.Sprintf("%-40s", ""))...)
	str = append(str, []byte(fmt.Sprintf("%-40s", ""))...)

	h.connTX <- str
}

func (h *HBRPClient) sendRPTK(random []byte) {
	// Generate a sha256 hash of the random data and the password
	s256 := sha256.New()
	s256.Write(random)
	s256.Write([]byte(h.hbrpCfg.Password))
	token := []byte(fmt.Sprintf("%x", s256.Sum(nil)))

	hexid := make([]byte, 8)
	copy(hexid, []byte(fmt.Sprintf("%08x", h.hbrpCfg.ID)))
	var (
		data = make([]byte, len("RPTK")+8+64)
		n    = copy(data, "RPTK")
	)
	copy(data[n:], hexid)
	copy(data[n+8:], token)
	h.connTX <- data
}

func (h *HBRPClient) sendPing() {
	hexid := make([]byte, 8)
	copy(hexid, []byte(fmt.Sprintf("%08x", h.hbrpCfg.ID)))
	var (
		data = make([]byte, len("MSTPING")+8)
		n    = copy(data, "MSTPING")
	)
	copy(data[n:], hexid)
	h.connTX <- data
}

func (h *HBRPClient) sendPacket(packet proto.Packet) {
	data := make([]byte, 53)
	copy(data, packet.Encode())
	h.connTX <- data
}
