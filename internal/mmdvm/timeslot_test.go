package mmdvm

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

func scrapeMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	return string(body)
}

func grepLines(s, substr string) string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func newSlotTestClient(t *testing.T, name string, mgr *timeslot.Manager) *MMDVMClient {
	t.Helper()
	c := newTestClient(t)
	c.cfg.Name = name
	c.state.Store(uint32(STATE_READY))
	c.netRewrites = []rewrite.Rule{&rewrite.TGRewrite{Name: name, FromSlot: 1, FromTG: 1, ToSlot: 1, ToTG: 1, Range: 999999}}
	c.SetOutboundTSManager(mgr)
	return c
}

func dmrdPacket(streamID uint32) []byte {
	return dmrdFrom(1, streamID, 1)
}

func dmrdFrom(src, streamID uint32, dtype uint8) []byte {
	p := dmrd.Data{Source: src, Destination: 9, Private: false, Frame: 2, Value: dtype, Stream: streamID}
	return encodeDMRD(p)
}

// encodeDMRD returns the 53 octets of p, on TS1 when p has no slot.
func encodeDMRD(p dmrd.Data) []byte {
	if p.Slot == 0 {
		p.Slot = enums.Slot1
	}
	b, err := p.AppendBinary(nil)
	if err != nil {
		panic(err)
	}
	return b
}

type ipscRecord struct {
	via         string
	src         uint32
	callControl uint32
	burstType   byte
	raw         []byte
	at          time.Time
}

type ipscRecorder struct {
	mu   sync.Mutex
	recs []ipscRecord
	hook func(ipscRecord)
}

func (r *ipscRecorder) handler(name string) func(context.Context, []byte) {
	return func(_ context.Context, d []byte) {
		rec := ipscRecord{
			via:         name,
			src:         uint32(d[6])<<16 | uint32(d[7])<<8 | uint32(d[8]),
			callControl: binary.BigEndian.Uint32(d[13:17]),
			burstType:   d[30],
			raw:         append([]byte(nil), d...),
			at:          time.Now(),
		}
		r.mu.Lock()
		hook := r.hook
		r.recs = append(r.recs, rec)
		r.mu.Unlock()
		if hook != nil {
			hook(rec)
		}
	}
}

func (r *ipscRecorder) snapshot() []ipscRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ipscRecord(nil), r.recs...)
}

func (r *ipscRecorder) waitFor(t *testing.T, cond func([]ipscRecord) bool) []ipscRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := r.snapshot()
		if cond(recs) {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for IPSC output, got %+v", recs)
		}
		time.Sleep(time.Millisecond)
	}
}

const (
	srcA = 1
	srcB = 10
)

func hasTerminatorFrom(src uint32) func([]ipscRecord) bool {
	return func(recs []ipscRecord) bool {
		for _, r := range recs {
			if r.src == src && r.burstType == 0x02 {
				return true
			}
		}
		return false
	}
}

func TestQueuedCallIsSentByItsOwnClient(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	c1 := newSlotTestClient(t, "net1", mgr)
	c2 := newSlotTestClient(t, "net2", mgr)
	rec := &ipscRecorder{}
	c1.SetIPSCHandler(rec.handler("net1"))
	c2.SetIPSCHandler(rec.handler("net2"))
	for i := range uint32(5) {
		c2.translator.TranslateToIPSC(dmrd.Data{Stream: 900 + i, Frame: 2, Value: 1, Destination: 1, Private: false})
	}

	c1.handleReady(dmrdFrom(srcA, 0xA, 1))
	c2.handleReady(dmrdFrom(srcB, 0xB, 1))
	c1.handleReady(dmrdFrom(srcA, 0xA, 2))
	c2.handleReady(dmrdFrom(srcB, 0xB, 1))
	c2.handleReady(dmrdFrom(srcB, 0xB, 2))

	recs := rec.waitFor(t, hasTerminatorFrom(srcB))
	var cc uint32
	for _, r := range recs {
		if r.src != srcB {
			continue
		}
		if r.via != "net2" {
			t.Fatalf("stream B packet sent via %s, want net2: %+v", r.via, recs)
		}
		if cc == 0 {
			cc = r.callControl
		}
		if r.callControl != cc {
			t.Fatalf("stream B call control changed mid-call: %+v", recs)
		}
	}
}

// TestOutboundDataCallEndReleasesSlot sends two confirmed rate 1/2 data
// packets from 3191868 to 3140598 on TS2 as separate streams, as an MMDVM
// master sends a packet and its retry. The first packet's last block ends
// the call, so the second one goes out at once instead of waiting for the
// timeslot timeout.
func TestOutboundDataCallEndReleasesSlot(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c := newSlotTestClient(t, "net1", mgr)
	c.netRewrites = []rewrite.Rule{&rewrite.PassAllPC{Name: c.cfg.Name, Slot: 2}}
	rec := &ipscRecorder{}
	c.SetIPSCHandler(rec.handler("net1"))

	bursts := []string{
		"20f7370570bee26393b7cda7ddadff57d75df5de6accf9067160278e89420a3c95",
		"06dd55694b3555ba4425c5649dcdff57d75df5ddc51065d0864171a60d174dab0b",
		"403201b0010f0182003002c01dcdff57d75df5ddc42001b002541b4c08007630d5",
	}
	dtypes := []uint8{6, 7, 7}
	start := time.Now()
	for _, stream := range []uint32{0xA, 0xB} {
		for i, h := range bursts {
			b, err := hex.DecodeString(h)
			if err != nil {
				t.Fatal(err)
			}
			p := dmrd.Data{Private: true, Source: 3191868, Destination: 3140598, Slot: enums.Slot2, Frame: 2,
				Value: dtypes[i], Stream: stream, Burst: [33]byte(b)}
			c.handleReady(encodeDMRD(p))
		}
	}
	rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 6 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("second data call went out after %v", d)
	}
}

// outboundDataBursts are a confirmed rate 1/2 data packet from 3191868 to
// 3140598, as dmrgo encodes it, with their data types.
func outboundDataBursts() ([]string, []uint8) {
	return []string{
		"20f7370570bee26393b7cda7ddadff57d75df5de6accf9067160278e89420a3c95",
		"06dd55694b3555ba4425c5649dcdff57d75df5ddc51065d0864171a60d174dab0b",
		"403201b0010f0182003002c01dcdff57d75df5ddc42001b002541b4c08007630d5",
	}, []uint8{6, 7, 7}
}

func outboundPacket(t *testing.T, stream uint32, h string, dtype uint8) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	p := dmrd.Data{Private: true, Source: 3191868, Destination: 3140598, Slot: enums.Slot2, Frame: 2,
		Value: dtype, Stream: stream, Burst: [33]byte(b)}
	return encodeDMRD(p)
}

// TestQueuedOutboundDataCallsEnd queues two data calls behind a voice call
// on TS2. When the voice call ends, both data calls must go out at once:
// the first must not hold the slot until the timeout.
func TestQueuedOutboundDataCallsEnd(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c := newSlotTestClient(t, "net1", mgr)
	c.netRewrites = []rewrite.Rule{&rewrite.PassAllPC{Name: c.cfg.Name, Slot: 2}}
	rec := &ipscRecorder{}
	c.SetIPSCHandler(rec.handler("net1"))

	voice := dmrd.Data{Private: true, Source: 3191868, Destination: 3140598, Slot: enums.Slot2, Frame: 2, Value: 1, Stream: 0xA}
	c.handleReady(encodeDMRD(voice))
	bursts, dtypes := outboundDataBursts()
	for _, stream := range []uint32{0xB, 0xC} {
		for i, h := range bursts {
			c.handleReady(outboundPacket(t, stream, h, dtypes[i]))
		}
	}
	start := time.Now()
	voice.Frame, voice.Value = 2, 2
	c.handleReady(encodeDMRD(voice))
	rec.waitFor(t, func(recs []ipscRecord) bool {
		n := 0
		for _, r := range recs {
			if r.burstType == 0x06 || r.burstType == 0x07 {
				n++
			}
		}
		return n == 6
	})
	if d := time.Since(start); d > time.Second {
		t.Fatalf("queued data calls went out after %v", d)
	}
}

// nonPreambleCSBK returns the first preamble CSBK of rigPreambleCall with
// its opcode changed to UU_V_Req (ETSI TS 102 361-2 7.1.2.2), a CSBK that
// nothing of its call follows.
func nonPreambleCSBK(t *testing.T) string {
	t.Helper()
	b, err := hex.DecodeString(rigPreambleCall()[8][40:106])
	if err != nil {
		t.Fatal(err)
	}
	p, _ := layer2.RawBurst(b).BPTCPayload()
	csbk := pdu.CSBKBytes(p)
	csbk[0] = 0x80 | byte(pdu.CSBKUnitToUnitVoiceServiceRequest)
	csbk.Reseal()
	burst, err := layer2.BuildDataBurst(csbk, enums.DataTypeCSBK, 1, layer2.SyncSourceBS)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(burst[:])
}

// TestOutboundCSBKEndsCall sends two CSBKs that are not preambles and a
// data packet on TS2, each with its own stream ID, as MMDVMHost does.
// Nothing follows such a CSBK in its call, so each CSBK ends its call and
// the data packet goes out at once.
func TestOutboundCSBKEndsCall(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c := newSlotTestClient(t, "net1", mgr)
	c.netRewrites = []rewrite.Rule{&rewrite.PassAllPC{Name: c.cfg.Name, Slot: 2}}
	rec := &ipscRecorder{}
	c.SetIPSCHandler(rec.handler("net1"))

	csbk := nonPreambleCSBK(t)
	start := time.Now()
	c.handleReady(outboundPacket(t, 1, csbk, 3))
	c.handleReady(outboundPacket(t, 2, csbk, 3))
	bursts, dtypes := outboundDataBursts()
	for i, h := range bursts {
		c.handleReady(outboundPacket(t, 3, h, dtypes[i]))
	}
	rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 5 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("data packet went out after %v", d)
	}
}

func TestReplayDoesNotBlockHandler(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c1 := newSlotTestClient(t, "net1", mgr)
	c2 := newSlotTestClient(t, "net2", mgr)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	c1.SetIPSCHandler(func(context.Context, []byte) {})
	c2.SetIPSCHandler(func(context.Context, []byte) { <-gate })

	c1.handleReady(dmrdFrom(srcA, 0xA, 1))
	c2.handleReady(dmrdFrom(srcB, 0xB, 1))
	returned := make(chan struct{})
	go func() {
		c1.handleReady(dmrdFrom(srcA, 0xA, 2))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("handler blocked while the queued call was replayed")
	}
}

func TestLivePacketsWaitBehindReplay(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	c1 := newSlotTestClient(t, "net1", mgr)
	c2 := newSlotTestClient(t, "net2", mgr)
	rec := &ipscRecorder{}
	blocked := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	rec.hook = func(r ipscRecord) {
		if r.src == srcB+1 {
			once.Do(func() {
				close(blocked)
				<-gate
			})
		}
	}
	c1.SetIPSCHandler(rec.handler("net1"))
	c2.SetIPSCHandler(rec.handler("net2"))

	c1.handleReady(dmrdFrom(srcA, 0xA, 1))
	c2.handleReady(dmrdFrom(srcB, 0xB, 1))
	c2.handleReady(dmrdFrom(srcB+1, 0xB, 1))
	go c1.handleReady(dmrdFrom(srcA, 0xA, 2))
	<-blocked
	liveDone := make(chan struct{})
	go func() {
		c2.handleReady(dmrdFrom(srcB+2, 0xB, 1))
		close(liveDone)
	}()
	select {
	case <-liveDone:
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	<-liveDone
	c2.handleReady(dmrdFrom(srcB+3, 0xB, 2))

	recs := rec.waitFor(t, hasTerminatorFrom(srcB+3))
	var last uint32
	for _, r := range recs {
		if r.src < srcB {
			continue
		}
		if r.src < last {
			t.Fatalf("stream B packets out of order: %+v", recs)
		}
		last = r.src
	}
}

func TestBufferedPacketsAreNotCountedAsDropped(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	mgr := timeslot.NewManager()
	c1 := newSlotTestClient(t, "net1", mgr)
	c2 := newSlotTestClient(t, "net2", mgr)
	c1.metrics = m
	c2.metrics = m
	c1.handleReady(dmrdPacket(0xA))
	c2.handleReady(dmrdPacket(0xB))
	if body := scrapeMetrics(t, m); strings.Contains(body, `reason="timeslot_busy"`) {
		t.Fatalf("buffered packet counted as dropped:\n%s", grepLines(body, "mmdvm_packets_dropped_total"))
	}
}

func TestTimeslotOverflowIsCountedAsDropped(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c1 := newSlotTestClient(t, "net1", mgr)
	c2 := newSlotTestClient(t, "net2", mgr)
	c2.metrics = m
	c1.handleReady(dmrdPacket(0xA))
	for range timeslot.MaxQueuedPackets + 3 {
		c2.handleReady(dmrdPacket(0xB))
	}
	want := `mmdvm_packets_dropped_total{network="net2",reason="timeslot_full"} 3`
	if body := scrapeMetrics(t, m); !strings.Contains(body, want) {
		t.Fatalf("want %s, got:\n%s", want, grepLines(body, "dropped_total"))
	}
}

func ipscVoiceHeader(callControl byte) []byte {
	data := make([]byte, 54)
	data[0] = 0x80
	data[4] = 0x01
	data[8] = 0x64
	data[11] = 0xC8
	data[12] = 0x02
	data[16] = callControl
	data[18] = 0x80
	data[30] = 0x01
	return data
}

func newInboundTestClient(t *testing.T) *MMDVMClient {
	t.Helper()
	c := newTestClient(t)
	c.started.Store(true)
	c.translator.SetPeerID(c.cfg.ID)
	c.rfRewrites = []rewrite.Rule{&rewrite.TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 1, ToSlot: 1, ToTG: 1, Range: 999999}}
	c.inboundTSMgr = timeslot.NewManager()
	return c
}

func TestBufferedInboundPacketsAreNotCountedAsDropped(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	c := newInboundTestClient(t)
	c.metrics = m
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	c.HandleIPSCBurst(0x80, ipscVoiceHeader(1), addr)
	c.HandleIPSCBurst(0x80, ipscVoiceHeader(2), addr)
	if body := scrapeMetrics(t, m); strings.Contains(body, `reason="timeslot_busy"`) {
		t.Fatalf("buffered packet counted as dropped:\n%s", grepLines(body, "mmdvm_packets_dropped_total"))
	}
}

func TestInboundTimeslotMetricsArePerNetwork(t *testing.T) {
	t.Parallel()
	m := metrics.NewMetrics()
	clients := make([]*MMDVMClient, 0, 2)
	for _, name := range []string{"A", "B"} {
		cfg := testMMDVMConfig()
		cfg.Name = name
		c := NewMMDVMClient(cfg, m)
		t.Cleanup(c.inboundTSMgr.Stop)
		clients = append(clients, c)
	}
	clients[0].inboundTSMgr.Submit(false, 1, "ipsc", nil, false, nil)
	clients[1].inboundTSMgr.Submit(false, 2, "ipsc", nil, false, nil)
	clients[1].inboundTSMgr.Release(false, 2)

	body := scrapeMetrics(t, m)
	for _, want := range []string{
		`timeslot_active_calls{direction="inbound",network="A",slot="1"} 1`,
		`timeslot_active_calls{direction="inbound",network="B",slot="1"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("want %s, got:\n%s", want, grepLines(body, "timeslot_active_calls"))
		}
	}
}

// rigPreambleCall is a TS1 voice call from 3191868 to group 9 with stream
// ID 5, then a data call from 3191868 to 3140598 with stream ID 7: four
// preamble CSBKs, a confirmed data header and two rate 1/2 blocks, all
// with one stream ID, as the bridge sent them to an MMDVM master on the rig
// (runs/rf-cycle-2/dmrd.txt).
func rigPreambleCall() []string {
	return []string{
		"444d52440030b43c000009003010af210000000500d40ada05cc0e703f004d80dc4dff57d75df5d36194140039c03be02f01b20054",
		"444d52440130b43c000009003010af1000000005298d0a33fd2bbc43cc28124ac5a755fd7df75f7138d2514ab1ba41155039c90579",
		"444d52440230b43c000009003010af0100000005cb375d2a6f00ccf547faacd653f730000090038be3fa9874d4cd7a0a1be7a21f8c",
		"444d52440330b43c000009003010af0200000005626757029ce3de90092c67627b577060a0606dddeb9fd47377687f8c79688c6888",
		"444d52440430b43c000009003010af03000000056b57cba90b6aefecc02847dea2b77090c240cddd28430f7625469dd2c4af5b44b2",
		"444d52440530b43c000009003010af0400000005f1a3e06b49ade23799e66aa4a4b750c2b2b24ae8a07221c799fa5f3614369fac04",
		"444d52440630b43c000009003010af0500000005125d0cc5c54a901ae901dbc4c1171000000004b437fd4bbba5262cb8c2dba64259",
		"444d52440730b43c000009003010af220000000500bb0a0e057c0e083f704d20dc8dff57d75df5d4348017383ea037a02701ab0067",
		"444d52440030b43c2febf6003010af63000000074a540cd5a14c3b5472e8ce721cedff57d75df5d798be5488f5a8f961b8df2c96d9",
		"444d52440130b43c2febf6003010af63000000074a300c55a0543b6c7689c6d01cedff57d75df5d798767428f568f9a1b05f3616dd",
		"444d52440230b43c2febf6003010af63000000074a700cd7a148397474b84a901cedff57d75df5d798065418b588fa01b59f3f16eb",
		"444d52440330b43c2febf6003010af63000000074a710c1da05c394474894e121cedff57d75df5d7985235883528fa81b9db3f16c8",
		"444d52440430b43c2febf6003010af660000000720f7370570bee26393b7cda7ddadff57d75df5de6accf9067160278e89420a3c95",
		"444d52440530b43c2febf6003010af670000000706dd55694b3555ba4425c5649dcdff57d75df5ddc51065d0864171a60d174dab0b",
		"444d52440630b43c2febf6003010af6700000007403201b0010f0182003002c01dcdff57d75df5ddc42001b002541b4c08007630d5",
	}
}

// TestQueuedPreambleDataCall sends the data call of rigPreambleCall while
// its voice call holds TS1. The preamble CSBKs do not end the data call, so
// all of it goes out when the voice call ends.
func TestQueuedPreambleDataCall(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c := newSlotTestClient(t, "net1", mgr)
	c.netRewrites = []rewrite.Rule{
		&rewrite.TGRewrite{Name: c.cfg.Name, FromSlot: 1, FromTG: 9, ToSlot: 1, ToTG: 9, Range: 1},
		&rewrite.PassAllPC{Name: c.cfg.Name, Slot: 1},
	}
	rec := &ipscRecorder{}
	c.SetIPSCHandler(rec.handler("net1"))

	call := rigPreambleCall()
	order := append(append([]string{call[0]}, call[8:]...), call[1:8]...)
	for _, h := range order {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		c.handleReady(b)
	}
	start := time.Now()
	rec.waitFor(t, func(recs []ipscRecord) bool {
		n := 0
		for _, r := range recs {
			if r.burstType == 0x03 || r.burstType == 0x06 || r.burstType == 0x07 {
				n++
			}
		}
		return n == 7
	})
	if d := time.Since(start); d > time.Second {
		t.Fatalf("queued data call went out after %v", d)
	}
}

// TestCSBKInsideDataPacketKeepsCall queues a data packet on TS2 behind a
// voice call, with a CSBK that is not a preamble between its header and
// its blocks, all with one stream ID. The data packet has blocks to follow,
// so the CSBK does not end the call, and the blocks go out after it.
func TestCSBKInsideDataPacketKeepsCall(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c := newSlotTestClient(t, "net1", mgr)
	c.netRewrites = []rewrite.Rule{&rewrite.PassAllPC{Name: c.cfg.Name, Slot: 2}}
	rec := &ipscRecorder{}
	c.SetIPSCHandler(rec.handler("net1"))

	voice := dmrd.Data{Private: true, Source: 3191868, Destination: 3140598, Slot: enums.Slot2, Frame: 2, Value: 1, Stream: 0xA}
	c.handleReady(encodeDMRD(voice))
	bursts, dtypes := outboundDataBursts()
	c.handleReady(outboundPacket(t, 0xB, bursts[0], dtypes[0]))
	c.handleReady(outboundPacket(t, 0xB, nonPreambleCSBK(t), 3))
	for i := 1; i < len(bursts); i++ {
		c.handleReady(outboundPacket(t, 0xB, bursts[i], dtypes[i]))
	}
	voice.Frame, voice.Value = 2, 2
	c.handleReady(encodeDMRD(voice))
	rec.waitFor(t, func(recs []ipscRecord) bool {
		var types []byte
		for _, r := range recs {
			if r.callControl != recs[0].callControl {
				types = append(types, r.burstType)
			}
		}
		return string(types) == "\x06\x03\x07\x07"
	})
}

// TestIdleOutboundDataCallsArePruned leaves two outbound data packets
// unfinished, one idle for longer than outDataIdle. A new data call drops
// the idle one only.
func TestIdleOutboundDataCallsArePruned(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	now := time.Now()
	c.outData = map[uint32]*outboundDataCall{
		1: {lastSeen: now.Add(-outDataIdle - time.Second)},
		2: {lastSeen: now.Add(-outDataIdle + time.Second)},
	}
	bursts, dtypes := outboundDataBursts()
	b, _ := hex.DecodeString(bursts[0])
	c.outboundCallEnds(dmrd.Data{Private: true, Frame: 2, Value: dtypes[0], Stream: 3, Burst: [33]byte(b)})
	if _, ok := c.outData[1]; ok {
		t.Error("idle data call was kept")
	}
	if _, ok := c.outData[2]; !ok {
		t.Error("data call that is not idle was dropped")
	}
}
