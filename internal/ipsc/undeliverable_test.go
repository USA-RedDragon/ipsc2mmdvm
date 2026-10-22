package ipsc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
)

type undeliverableCase struct {
	name   string
	frame  dmrd.FrameType
	dt     enums.DataType
	burst  layer2.RawBurst
	label  string
	reason string
}

func undeliverableCases(t *testing.T) []undeliverableCase {
	t.Helper()
	bptc := func(dt enums.DataType, p [12]byte) layer2.RawBurst {
		b, err := layer2.BuildDataBurst(p, dt, 7, layer2.SyncSourceBS)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	rate1, err := layer2.BuildRate1DataBurst([24]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24}, 7, layer2.SyncSourceBS)
	if err != nil {
		t.Fatal(err)
	}
	reserved := bptc(enums.DataTypeIdle, [12]byte{0xAA})
	if err := reserved.SetSlotType(pdu.SlotType{ColorCode: 7, DataType: enums.DataType(13)}); err != nil {
		t.Fatal(err)
	}
	r34, err := layer2.BuildRate34DataBurst([18]byte{0x11, 0x22}, 7, layer2.SyncSourceBS)
	if err != nil {
		t.Fatal(err)
	}
	info := r34.Info()
	for i := range info {
		info[i] ^= 0x5A + 31*byte(i) //nolint:gosec // i is under 25
	}
	info[24] &= 0xF0
	if err := r34.SetInfo(info); err != nil {
		t.Fatal(err)
	}
	if _, res := r34.Rate34Payload(); !res.Uncorrectable {
		t.Fatal("rate 3/4 test burst is correctable")
	}
	csbk := withCRC([12]byte{0x3D, 0x00, 0x80, 0x0A, 0x30, 0xB4, 0x3C, 0x2F, 0xEB, 0xF6}, enums.DataTypeCSBK)
	csbk[11] ^= 0x01
	return []undeliverableCase{
		{"idle", dmrd.FrameDataSync, enums.DataTypeIdle, bptc(enums.DataTypeIdle, [12]byte{}), "idle", reasonNoIPSCForm},
		{"usbd", dmrd.FrameDataSync, enums.DataTypeUnifiedSingleBlock, bptc(enums.DataTypeUnifiedSingleBlock, [12]byte{0x01}), "unified_single_block", reasonNoIPSCForm},
		{"rate 1", dmrd.FrameDataSync, enums.DataTypeRate1, rate1, "rate_1", reasonNoIPSCForm},
		{"reserved", dmrd.FrameDataSync, enums.DataType(13), reserved, "reserved", reasonNoIPSCForm},
		{"bad CRC CSBK", dmrd.FrameDataSync, enums.DataTypeCSBK, bptc(enums.DataTypeCSBK, csbk), "csbk", reasonUncorrectable},
		{"bad rate 3/4", dmrd.FrameDataSync, enums.DataTypeRate34, r34, "rate_3_4", reasonUncorrectable},
		{"frame type 3", dmrd.FrameReserved, 0, bptc(enums.DataTypeCSBK, csbk), "none", reasonFrameType3},
	}
}

func scrapeTranslatorMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestUndeliverableBurstsAreCounted(t *testing.T) {
	t.Parallel()
	for _, c := range undeliverableCases(t) {
		tr, _, m := newClockedTranslator(t)
		tr.SetColorCode(7)
		pkt := makeTestMMDVMPacket(false, false, c.frame, uint8(c.dt))
		pkt.Burst = c.burst
		if out := tr.TranslateToIPSC(pkt); len(out) != 0 {
			t.Fatalf("%s: got %d IPSC packets, want none", c.name, len(out))
		}
		want := `translator_undeliverable_total{data_type="` + c.label + `",reason="` + c.reason + `"} 1`
		if body := scrapeTranslatorMetrics(t, m); !strings.Contains(body, want) {
			t.Fatalf("%s: metrics have no %s", c.name, want)
		}
	}
}
