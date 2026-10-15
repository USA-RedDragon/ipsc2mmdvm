package mmdvm

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	p := proto.Packet{Signature: "DMRD", Src: 1, Dst: 9, GroupCall: true, FrameType: frameType, DTypeOrVSeq: dtype, StreamID: streamID}
	return p.Encode()
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
