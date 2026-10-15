package mmdvm

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
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

func dmrd(streamID, frameType, dtype uint) []byte {
	return dmrdFrom(1, streamID, frameType, dtype)
}

func dmrdFrom(src, streamID, frameType, dtype uint) []byte {
	p := proto.Packet{Signature: "DMRD", Src: src, Dst: 9, GroupCall: true, FrameType: frameType, DTypeOrVSeq: dtype, StreamID: streamID}
	return p.Encode()
}

type ipscRecord struct {
	via         string
	src         uint32
	callControl uint32
	burstType   byte
}

type ipscRecorder struct {
	mu   sync.Mutex
	recs []ipscRecord
	hook func(ipscRecord)
}

func (r *ipscRecorder) handler(name string) func([]byte) {
	return func(d []byte) {
		rec := ipscRecord{
			via:         name,
			src:         uint32(d[6])<<16 | uint32(d[7])<<8 | uint32(d[8]),
			callControl: binary.BigEndian.Uint32(d[13:17]),
			burstType:   d[30],
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
	for i := range uint(5) {
		c2.translator.TranslateToIPSC(proto.Packet{StreamID: 900 + i, FrameType: 2, DTypeOrVSeq: 1, Dst: 1, GroupCall: true})
	}

	c1.handleReady(dmrdFrom(srcA, 0xA, 2, 1))
	c2.handleReady(dmrdFrom(srcB, 0xB, 2, 1))
	c1.handleReady(dmrdFrom(srcA, 0xA, 2, 2))
	c2.handleReady(dmrdFrom(srcB, 0xB, 2, 1))
	c2.handleReady(dmrdFrom(srcB, 0xB, 2, 2))

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

func TestReplayDoesNotBlockHandler(t *testing.T) {
	t.Parallel()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c1 := newSlotTestClient(t, "net1", mgr)
	c2 := newSlotTestClient(t, "net2", mgr)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	c1.SetIPSCHandler(func([]byte) {})
	c2.SetIPSCHandler(func([]byte) { <-gate })

	c1.handleReady(dmrdFrom(srcA, 0xA, 2, 1))
	c2.handleReady(dmrdFrom(srcB, 0xB, 2, 1))
	returned := make(chan struct{})
	go func() {
		c1.handleReady(dmrdFrom(srcA, 0xA, 2, 2))
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

	c1.handleReady(dmrdFrom(srcA, 0xA, 2, 1))
	c2.handleReady(dmrdFrom(srcB, 0xB, 2, 1))
	c2.handleReady(dmrdFrom(srcB+1, 0xB, 2, 1))
	go c1.handleReady(dmrdFrom(srcA, 0xA, 2, 2))
	<-blocked
	liveDone := make(chan struct{})
	go func() {
		c2.handleReady(dmrdFrom(srcB+2, 0xB, 2, 1))
		close(liveDone)
	}()
	select {
	case <-liveDone:
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	<-liveDone
	c2.handleReady(dmrdFrom(srcB+3, 0xB, 2, 2))

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
	c1.handleReady(dmrd(0xA, 2, 1))
	c2.handleReady(dmrd(0xB, 2, 1))
	if body := scrapeMetrics(t, m); strings.Contains(body, `reason="timeslot_busy"`) {
		t.Fatalf("buffered packet counted as dropped:\n%s", grepLines(body, "mmdvm_packets_dropped_total"))
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
