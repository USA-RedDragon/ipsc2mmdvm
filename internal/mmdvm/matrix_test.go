package mmdvm

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/packet"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
	"github.com/USA-RedDragon/dmrgo/v2/vocoder"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/klauspost/compress/zstd"
)

const (
	matrixTMSSource = "dmrgo internal/appcapture TMS"
	matrixTMS       = "4500003a000d00004011c2140c30b43c0c2febf60fa70fa70026509e001ca00081040d000a004d004f0054004f00200054004500530054000000"
)

type loopMaster struct {
	conn *net.UDPConn
	peer atomic.Pointer[net.UDPAddr]
	mu   sync.Mutex
	got  []dmrd.Data
	raw  [][]byte
}

func newLoopMaster(t *testing.T) *loopMaster {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadBuffer(4 << 20)
	t.Cleanup(func() { _ = conn.Close() })
	m := &loopMaster{conn: conn}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			m.peer.Store(addr)
			data := buf[:n]
			var reply []byte
			switch {
			case bytes.HasPrefix(data, []byte("DMRA")), bytes.HasPrefix(data, []byte("DMRG")):
				m.mu.Lock()
				m.raw = append(m.raw, slices.Clone(data))
				m.mu.Unlock()
			case bytes.HasPrefix(data, []byte("DMRD")):
				m.mu.Lock()
				m.raw = append(m.raw, slices.Clone(data))
				m.mu.Unlock()
				if p, err := dmrd.DecodeData(data); err == nil {
					m.mu.Lock()
					m.got = append(m.got, p)
					m.mu.Unlock()
				}
			case bytes.HasPrefix(data, []byte("RPTPING")):
				reply = append([]byte("MSTPONG"), data[7:11]...)
			case bytes.HasPrefix(data, []byte("RPTL")):
				reply = []byte("RPTACK\x01\x02\x03\x04")
			case bytes.HasPrefix(data, []byte("RPTK")), bytes.HasPrefix(data, []byte("RPTC")):
				reply = append([]byte("RPTACK"), data[4:8]...)
			}
			if reply != nil {
				_, _ = conn.WriteToUDP(reply, addr)
			}
		}
	}()
	return m
}

func (m *loopMaster) packets() []dmrd.Data {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.got)
}

func (m *loopMaster) rawPackets() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.raw)
}

func (m *loopMaster) send(t *testing.T, pkts []dmrd.Data) {
	t.Helper()
	for _, p := range pkts {
		b, err := p.AppendBinary(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.conn.WriteToUDP(b, m.peer.Load()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
}

func startLoopClient(t *testing.T, m *loopMaster, raw bool) (*MMDVMClient, *ipscRecorder, *metrics.Metrics) {
	t.Helper()
	cfg := testMMDVMConfig()
	cfg.MasterServer = m.conn.LocalAddr().String()
	cfg.ColorCode = 7
	cfg.PassAllTG = []int{1, 2}
	cfg.PassAllPC = []int{1, 2}
	met := metrics.NewMetrics()
	c := NewMMDVMClient(cfg, met)
	c.retry = 25 * time.Millisecond
	c.timeout = 5 * time.Second
	c.inboundTSMgr = nil
	rec := &ipscRecorder{}
	c.SetIPSCHandler(rec.handler("loop"))
	c.SetIPSCRawBursts(raw)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	waitFor(t, 3*time.Second, "login", isReady(c))
	return c, rec, met
}

func settle(t *testing.T, n func() int, quiet time.Duration) {
	t.Helper()
	last, at := -1, time.Now()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if v := n(); v != last {
			last, at = v, time.Now()
		} else if time.Since(at) >= quiet {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("output did not settle")
}

func burstKey(slot enums.Slot, dt enums.DataType, raw layer2.RawBurst) string {
	var content []byte
	switch {
	case dt == enums.DataTypeVoiceLCHeader || dt == enums.DataTypeTerminatorWithLC:
		p, _ := raw.BPTCPayload()
		content = p[:9]
	case dt.IsBPTC():
		p, _ := raw.BPTCPayload()
		content = p[:]
	case dt == enums.DataTypeRate34:
		p, _ := raw.Rate34Payload()
		content = p[:]
	default:
		info := raw.Info()
		content = info[:]
	}
	return fmt.Sprintf("%v %s %x", slot, matrixKind(dt), content)
}

func voiceKey(slot enums.Slot, raw layer2.RawBurst, burstA bool) string {
	ambe, _, _ := raw.AMBEVoice()
	if burstA {
		return fmt.Sprintf("%v voice-A %x", slot, ambe)
	}
	return fmt.Sprintf("%v voice %x %x", slot, ambe, raw.EmbeddedData())
}

func matrixKind(dt enums.DataType) string {
	return strings.ReplaceAll(strings.ToLower(dt.String()), " ", "-")
}

func dmrdKey(d dmrd.Data) (string, bool) {
	switch d.Frame {
	case dmrd.FrameVoice, dmrd.FrameVoiceSync:
		return voiceKey(d.Slot, d.Burst, d.Frame == dmrd.FrameVoiceSync), true
	case dmrd.FrameDataSync:
		return burstKey(d.Slot, enums.DataType(d.Value), d.Burst), true
	case dmrd.FrameReserved:
	}
	return "", false
}

func ipscKey(p []byte) (string, bool) {
	u, err := ipscpkt.DecodeUser(p)
	if err != nil || u.Burst.Type() == ipscpkt.BurstSyncLost {
		return "", false
	}
	raw, err := u.RawBurst(7, layer2.SyncSourceBS)
	if dt := enums.DataType(u.Burst); err != nil && !u.IsVoice() && dt.IsBPTC() && u.Data.Bits == 96 {
		raw, err = layer2.BuildDataBurst([12]byte(u.Data.Octets[:12]), dt, 7, layer2.SyncSourceBS)
	}
	if err != nil {
		return "", false
	}
	if u.IsVoice() {
		return voiceKey(u.Info.Slot(), raw, u.Voice.Layout == ipscpkt.VoiceLayoutA), true
	}
	dt := enums.DataType(u.Burst)
	if u.Burst == ipscpkt.BurstRaw {
		st, ok := u.Data.SlotType()
		if !ok {
			return "", false
		}
		dt = st.DataType()
	}
	return burstKey(u.Info.Slot(), dt, raw), true
}

func carriedToIPSC(d dmrd.Data, raw bool) bool {
	if d.Frame == dmrd.FrameReserved {
		return false
	}
	if d.Frame != dmrd.FrameDataSync {
		return true
	}
	dt := enums.DataType(d.Value)
	switch dt { //nolint:exhaustive // the others are carried when they decode
	case enums.DataTypeIdle, enums.DataTypeUnifiedSingleBlock, enums.DataTypeRate1:
		return raw
	case enums.DataTypeRate34:
		_, res := d.Burst.Rate34Payload()
		return raw || !res.Uncorrectable
	case enums.DataTypeVoiceLCHeader, enums.DataTypeTerminatorWithLC, enums.DataTypeMBCContinuation:
		return true
	}
	if dt > enums.DataTypeUnifiedSingleBlock {
		return raw
	}
	p, _ := d.Burst.BPTCPayload()
	return raw || layer2.MMDVMHostAccepts(dt, p)
}

func multiset(keys []string) map[string]int {
	m := map[string]int{}
	for _, k := range keys {
		m[k]++
	}
	return m
}

func diff(want, got map[string]int) (missing, extra []string) {
	for k, n := range want {
		for range n - got[k] {
			missing = append(missing, k)
		}
	}
	for k, n := range got {
		for range n - want[k] {
			extra = append(extra, k)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra
}

func ipscKeys(recs []ipscRecord) []string {
	var keys []string
	seenHeader := map[uint32]bool{}
	for _, r := range recs {
		if r.burstType == 0x01 {
			if seenHeader[r.callControl] {
				continue
			}
			seenHeader[r.callControl] = true
		}
		if k, ok := ipscKey(r.raw); ok {
			keys = append(keys, k)
		}
	}
	return keys
}

func dmrdKeys(pkts []dmrd.Data, keep func(dmrd.Data) bool) []string {
	var keys []string
	for _, d := range pkts {
		if keep != nil && !keep(d) {
			continue
		}
		if k, ok := dmrdKey(d); ok {
			keys = append(keys, k)
		}
	}
	return keys
}

func kindsOf(keys []string) map[string]int {
	m := map[string]int{}
	for _, k := range keys {
		f := strings.Fields(k)
		m[f[0]+" "+f[1]]++
	}
	return m
}

func loadRFBursts(t *testing.T, name string) []layer2.RawBurst {
	t.Helper()
	f, err := os.Open("testdata/matrix/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []layer2.RawBurst
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		fs := strings.Fields(line)
		b, err := hex.DecodeString(fs[len(fs)-1])
		if err != nil || len(b) != 33 {
			t.Fatalf("%s: bad line %q", name, line)
		}
		out = append(out, layer2.RawBurst(b))
	}
	return out
}

func rfToDMRD(bursts []layer2.RawBurst, slot, idleSlot enums.Slot, stream uint32) []dmrd.Data {
	var out []dmrd.Data
	var src, dst uint32 = 3191868, 3140598
	group := false
	seq := uint8(0)
	idleStream := stream + 0x10000
	for _, raw := range bursts {
		st := raw.SlotType()
		if st.FEC.Uncorrectable {
			continue
		}
		dt := st.DataType
		p, _ := raw.BPTCPayload()
		switch dt { //nolint:exhaustive // blocks keep the stream and addresses of their header
		case enums.DataTypeIdle:
			out = append(out, dmrd.Data{Source: src, Destination: dst, Private: !group, Slot: idleSlot, Frame: dmrd.FrameDataSync, Value: uint8(dt), Stream: idleStream, Burst: raw})
			continue
		case enums.DataTypeCSBK:
			c := pdu.CSBKBytes(p)
			if tgt, s, ok := c.Addresses(); ok {
				dst, src = tgt, s
			}
			if g, ok := c.Group(); ok {
				group = g
			}
			stream++
			seq = 0
		case enums.DataTypeDataHeader:
			h := pdu.DataHeaderBytes(p)
			if tgt, s, ok := h.Addresses(); ok {
				dst, src = tgt, s
			}
			if g, ok := h.Group(); ok {
				group = g
			}
			stream++
			seq = 0
		case enums.DataTypeMBCHeader:
			stream++
			seq = 0
		}
		out = append(out, dmrd.Data{Seq: seq, Source: src, Destination: dst, Private: !group, Slot: slot, Frame: dmrd.FrameDataSync, Value: uint8(dt), Stream: stream, Burst: raw})
		seq++
	}
	return out
}

func packetDMRD(t *testing.T, h pdu.DataHeader, payload []byte, rate enums.DataType, slot enums.Slot, stream uint32) []dmrd.Data {
	t.Helper()
	p, err := packet.NewPacket(h, payload, rate)
	if err != nil {
		t.Fatal(err)
	}
	return packetBursts(t, &p, slot, stream)
}

func packetBursts(t *testing.T, p *packet.Packet, slot enums.Slot, stream uint32) []dmrd.Data {
	t.Helper()
	bursts, err := packet.Bursts(p, 7, layer2.SyncSourceBS)
	if err != nil {
		t.Fatal(err)
	}
	return rfToDMRD(bursts, slot, slot, stream-1)
}

func dataBurst(t *testing.T, dt enums.DataType, p [12]byte, seal bool) layer2.RawBurst {
	t.Helper()
	if seal {
		if err := pdu.Seal(&p, dt); err != nil {
			t.Fatal(err)
		}
	}
	b, err := layer2.BuildDataBurst(p, dt, 7, layer2.SyncSourceBS)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type xprGroup struct {
	name    string
	packets [][]byte
}

func loadXPRGroups(t *testing.T) []xprGroup {
	t.Helper()
	f, err := os.Open("../ipsc/testdata/xpr8400-ipsc.txt.zst")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var groups []xprGroup
	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "# "); ok {
			groups = append(groups, xprGroup{name: name})
			continue
		}
		_, h, _ := strings.Cut(line, " ")
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		switch b[0] {
		case 0x80, 0x81, 0x83, 0x84:
			g := &groups[len(groups)-1]
			g.packets = append(g.packets, b)
		}
	}
	return groups
}

type matrixRow struct {
	name   string
	source string
	pkts   []dmrd.Data
}

func mmdvmRows(t *testing.T) []matrixRow {
	t.Helper()
	var rows []matrixRow
	rf := func(name, file, source string, slot, idleSlot enums.Slot, stream uint32) {
		rows = append(rows, matrixRow{name, source, rfToDMRD(loadRFBursts(t, file), slot, idleSlot, stream)})
	}
	rf("anytone confirmed rate 3/4 SMS, SACK, ACKs", "anytone_sms_xpr.txt", "dmrgo data/testdata/anytone_sms_xpr.txt", enums.Slot2, enums.Slot1, 100)
	rf("anytone unconfirmed rate 1/2 text and idle", "anytone_dmr_text.txt", "dmrgo sms/testdata/anytone_dmr_text.txt", enums.Slot2, enums.Slot1, 200)
	rf("anytone H-SMS defined data", "anytone_hsms.txt", "dmrgo sms/testdata/anytone_hsms.txt", enums.Slot1, enums.Slot2, 300)
	rf("anytone GPS position reports", "anytone_aprs.txt", "dmrgo layer2/testdata/anytone_aprs.txt", enums.Slot2, enums.Slot1, 400)
	rf("anytone v1.05 text to the radio", "v105-b1-sent.txt", "rfcap runs/anytone-v105-20261022 b1.sent.hex", enums.Slot1, enums.Slot2, 500)
	rf("ETSI IP text to the radio", "v105-b4-sent.txt", "rfcap runs/anytone-v105-20261022 b4.sent.hex", enums.Slot2, enums.Slot1, 600)
	rf("anytone v1.05 SMS from the radio", "v105-phaseA.txt", "rfcap runs/anytone-v105-20261022 phaseA.rx3.txt", enums.Slot1, enums.Slot2, 700)

	headerless := rfToDMRD(loadRFBursts(t, "v105-b1-sent.txt"), enums.Slot2, enums.Slot1, 800)
	headerless = slices.DeleteFunc(headerless, func(d dmrd.Data) bool { return d.Value != uint8(enums.DataTypeRate12) })
	rows = append(rows, matrixRow{"rate 1/2 blocks whose header was lost", "rfcap runs/anytone-v105-20261022 b1.sent.hex without its header", headerless})

	var voice []dmrd.Data
	f, err := os.ReadFile("testdata/matrix/dmrgateway-master.txt")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(f)) {
		fs := strings.Fields(line)
		if len(fs) == 4 && fs[1] == "m>r" && strings.HasPrefix(fs[3], "444d5244") {
			b, _ := hex.DecodeString(fs[3])
			d, err := dmrd.DecodeData(b)
			if err != nil {
				t.Fatal(err)
			}
			voice = append(voice, d)
		}
	}
	rows = append(rows, matrixRow{"group voice TS2 from a DMRGateway master", "dmrgo transport/dmrd/testdata/dmrgateway-master.txt", voice})

	tms, _ := hex.DecodeString(matrixTMS)
	var conf pdu.DataHeader
	ch := pdu.ConfirmedDataHeader{ResponseRequested: true, SAP: enums.SAPIPBasedPacketData, LLIDDestination: 3140598, LLIDSource: 3191868, FullMessageFlag: true}
	if err := conf.SetConfirmed(ch); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, matrixRow{"Motorola TMS SMS, confirmed rate 1/2", matrixTMSSource, packetDMRD(t, conf, tms, enums.DataTypeRate12, enums.Slot1, 900)})
	rows = append(rows, matrixRow{"Motorola TMS SMS, confirmed rate 3/4", matrixTMSSource, packetDMRD(t, conf, tms, enums.DataTypeRate34, enums.Slot2, 1000)})
	var unc pdu.DataHeader
	if err := unc.SetUnconfirmed(pdu.UnconfirmedDataHeader{Group: true, SAP: enums.SAPIPBasedPacketData, LLIDDestination: 9, LLIDSource: 3191868, FullMessageFlag: true}); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, matrixRow{"group TMS SMS, unconfirmed rate 1/2", "dmrgo internal/appcapture TMS to TG 9", packetDMRD(t, unc, tms, enums.DataTypeRate12, enums.Slot1, 1100)})
	rows = append(rows, matrixRow{"TMS SMS at rate 1", matrixTMSSource, packetDMRD(t, unc, tms, enums.DataTypeRate1, enums.Slot2, 1200)})
	for i, r := range []packet.Response{packet.ResponseACK, packet.ResponseNACKOutOfSequence, packet.ResponseSACK} {
		data := &packet.ResponseData{}
		data.Retry[1] = true
		p, err := packet.NewResponse(&ch, r, data)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, matrixRow{"response " + r.String(), "packet.NewResponse to the TMS header", packetBursts(t, &p, enums.Slot1, uint32(1300+i*10))})
	}

	single := func(name string, frame dmrd.FrameType, dt enums.DataType, raw layer2.RawBurst, group bool, slot enums.Slot, stream uint32) {
		rows = append(rows, matrixRow{name, "built", []dmrd.Data{{Source: 3191868, Destination: 9, Private: !group, Slot: slot, Frame: frame, Value: uint8(dt), Stream: stream, Burst: raw}}})
	}
	single("unified single block", dmrd.FrameDataSync, enums.DataTypeUnifiedSingleBlock, dataBurst(t, enums.DataTypeUnifiedSingleBlock, [12]byte{0x01, 0x02}, false), false, enums.Slot1, 1400)
	reserved := dataBurst(t, enums.DataTypeIdle, [12]byte{0xAA}, false)
	if err := reserved.SetSlotType(pdu.SlotType{ColorCode: 7, DataType: enums.DataType(13)}); err != nil {
		t.Fatal(err)
	}
	single("reserved data type 13", dmrd.FrameDataSync, enums.DataType(13), reserved, true, enums.Slot2, 1410)
	bad := dataBurst(t, enums.DataTypeCSBK, [12]byte{0x3D, 0x00, 0x80, 0x0A, 0x30, 0xB4, 0x3C, 0x2F, 0xEB, 0xF6}, true)
	p, _ := bad.BPTCPayload()
	p[11] ^= 1
	bad.SetBPTCPayload(p)
	single("CSBK that fails its CRC", dmrd.FrameDataSync, enums.DataTypeCSBK, bad, false, enums.Slot1, 1420)
	single("DMRD frame type 3", dmrd.FrameReserved, 0, dataBurst(t, enums.DataTypeCSBK, [12]byte{0x3D}, true), true, enums.Slot2, 1430)
	rc := dataBurst(t, enums.DataTypeCSBK, [12]byte{0x24, 0x10, 0x00, 0x8C, 0x30, 0xB4, 0x3C, 0x2F, 0xEB, 0xF6}, true)
	single("Motorola radio check CSBK (FID 0x10)", dmrd.FrameDataSync, enums.DataTypeCSBK, rc, false, enums.Slot2, 1440)
	mbcHead := dataBurst(t, enums.DataTypeMBCHeader, [12]byte{0x28, 0x00, 0x80, 0x30, 0xB4, 0x3C, 0x2F, 0xEB, 0xF6, 0x01}, true)
	mbcCont := dataBurst(t, enums.DataTypeMBCContinuation, [12]byte{0x44, 0x4D, 0x52, 0x47, 0x4F, 0x20, 0x54, 0x45, 0x53, 0x54, 0x00, 0x01}, false)
	rows = append(rows, matrixRow{"MBC header and continuation", "built", []dmrd.Data{
		{Source: 3191868, Destination: 3140598, Private: true, Slot: enums.Slot1, Frame: dmrd.FrameDataSync, Value: uint8(enums.DataTypeMBCHeader), Stream: 1450, Burst: mbcHead},
		{Seq: 1, Source: 3191868, Destination: 3140598, Private: true, Slot: enums.Slot1, Frame: dmrd.FrameDataSync, Value: uint8(enums.DataTypeMBCContinuation), Stream: 1450, Burst: mbcCont},
	}})
	return rows
}

func TestLoopbackMatrix(t *testing.T) {
	t.Parallel()
	for _, raw := range []bool{false, true} {
		for _, row := range mmdvmRows(t) {
			t.Run(fmt.Sprintf("master to IPSC and back, raw %v, %s", raw, row.name), func(t *testing.T) {
				t.Parallel()
				matrixFromMaster(t, row, raw)
			})
		}
		for _, g := range loadXPRGroups(t) {
			if len(g.packets) == 0 {
				continue
			}
			t.Run(fmt.Sprintf("IPSC to master and back, raw %v, %s", raw, g.name), func(t *testing.T) {
				t.Parallel()
				matrixFromIPSC(t, g, raw)
			})
		}
	}
}

func matrixFromMaster(t *testing.T, row matrixRow, raw bool) {
	m1 := newLoopMaster(t)
	_, rec1, met := startLoopClient(t, m1, raw)
	m1.send(t, row.pkts)
	settle(t, func() int { return len(rec1.snapshot()) }, 800*time.Millisecond)
	want := dmrdKeys(row.pkts, func(d dmrd.Data) bool { return carriedToIPSC(d, raw) })
	got := ipscKeys(rec1.snapshot())
	if missing, extra := diff(multiset(want), multiset(got)); len(missing)+len(extra) > 0 {
		t.Fatalf("source %s: to IPSC, missing %d %v, extra %d %v", row.source, len(missing), missing, len(extra), extra)
	}
	undelivered := 0
	for _, d := range row.pkts {
		if !carriedToIPSC(d, raw) {
			undelivered++
		}
	}
	if n := countMetric(t, met, "translator_undeliverable_total"); n != undelivered {
		t.Fatalf("source %s: %d bursts counted as undeliverable, want %d", row.source, n, undelivered)
	}

	m2 := newLoopMaster(t)
	c2, _, _ := startLoopClient(t, m2, raw)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 50000}
	for _, r := range rec1.snapshot() {
		c2.HandleIPSCBurst(r.raw[0], r.raw, addr)
	}
	settle(t, func() int { return len(m2.packets()) }, 300*time.Millisecond)
	back := dmrdKeys(m2.packets(), nil)
	if missing, extra := diff(multiset(want), multiset(back)); len(missing)+len(extra) > 0 {
		t.Fatalf("source %s: back to the master, missing %d %v, extra %d %v", row.source, len(missing), missing, len(extra), extra)
	}
	t.Logf("source %s: %v", row.source, kindsOf(want))
}

func matrixFromIPSC(t *testing.T, g xprGroup, raw bool) {
	m1 := newLoopMaster(t)
	c1, _, _ := startLoopClient(t, m1, raw)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 50000}
	in := make([][]byte, 0, len(g.packets))
	for _, p := range g.packets {
		c1.HandleIPSCBurst(p[0], slices.Clone(p), addr)
		in = append(in, p)
	}
	settle(t, func() int { return len(m1.packets()) }, 300*time.Millisecond)
	toMaster := m1.packets()
	want, voiceWant := xprExpected(in)
	got := dmrdKeys(toMaster, nil)
	checkFromIPSC(t, g.name+" to the master", want, voiceWant, got)

	m2 := newLoopMaster(t)
	_, rec2, _ := startLoopClient(t, m2, raw)
	m2.send(t, toMaster)
	settle(t, func() int { return len(rec2.snapshot()) }, 800*time.Millisecond)
	gotBack := ipscKeys(rec2.snapshot())
	wantBack := dmrdKeys(toMaster, func(d dmrd.Data) bool { return carriedToIPSC(d, raw) })
	if missing, extra := diff(multiset(wantBack), multiset(gotBack)); len(missing)+len(extra) > 0 {
		t.Fatalf("%s: back to IPSC, missing %d %v, extra %d %v", g.name, len(missing), missing, len(extra), extra)
	}
	t.Logf("xpr8400-ipsc %s: %v", g.name, kindsOf(append(want, voiceWant...)))
}

func xprExpected(in [][]byte) (data, voice []string) {
	seenHeader := map[uint32]bool{}
	for _, p := range in {
		u, err := ipscpkt.DecodeUser(p)
		if err != nil || u.Burst.Type() == ipscpkt.BurstSyncLost {
			continue
		}
		if u.IsVoice() {
			if _, bad := vocoder.UnpackAMBEVoiceFlags(u.Voice.AMBE); u.Voice.Control&0x01 != 0 || slices.Contains(bad[:], true) {
				continue
			}
			if u.Voice.Layout == ipscpkt.VoiceLayoutA || u.Voice.Layout == ipscpkt.VoiceLayoutE {
				if k, ok := ipscKey(p); ok {
					voice = append(voice, k)
				}
			}
			continue
		}
		dt := enums.DataType(u.Burst)
		if dt == enums.DataTypeVoiceLCHeader || dt == enums.DataTypeTerminatorWithLC {
			if dt == enums.DataTypeVoiceLCHeader {
				if seenHeader[u.CallControl] {
					continue
				}
				seenHeader[u.CallControl] = true
			}
			if pl := [12]byte(u.Data.Octets[:12]); !layer2.MMDVMHostAccepts(dt, pl) {
				continue
			}
		}
		if k, ok := ipscKey(p); ok {
			data = append(data, k)
		}
	}
	return data, voice
}

func checkFromIPSC(t *testing.T, what string, want, voiceWant, got []string) {
	t.Helper()
	gotSet := multiset(got)
	var missing []string
	for k, n := range multiset(append(slices.Clone(want), voiceWant...)) {
		for range n - gotSet[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		t.Fatalf("%s: missing %d %v", what, len(missing), missing)
	}
	wantSet := multiset(want)
	var extra []string
	for k, n := range gotSet {
		if strings.Contains(k, " voice") || strings.Contains(k, "voice-lc-header") || strings.Contains(k, "terminator-with-lc") {
			continue
		}
		for range n - wantSet[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		t.Fatalf("%s: extra %d %v", what, len(extra), extra)
	}
}

func countMetric(t *testing.T, m *metrics.Metrics, name string) int {
	t.Helper()
	n := 0
	for line := range strings.Lines(scrapeMetrics(t, m)) {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		var v float64
		if _, err := fmt.Sscan(line[strings.LastIndexByte(line, ' ')+1:], &v); err == nil {
			n += int(v)
		}
	}
	return n
}
