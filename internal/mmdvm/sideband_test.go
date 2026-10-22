package mmdvm

import (
	"strings"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd/dmrdsession"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/metrics"
)

func sidebandPackets(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, size := range []uint8{15, 19} {
		ta := dmrd.TalkerAlias{Repeater: 3149999, Source: 3191868, Type: 0, Data: [7]byte{0x4b, 0x49, 0x35, 0x56, 0x4d, 0x46, 0x20}, Size: size}
		b, err := ta.AppendBinary(nil)
		if err != nil {
			t.Fatal(err)
		}
		out["talker_alias "+string(rune('0'+size%10))] = b
	}
	for _, size := range []uint8{14, 18} {
		p := dmrd.Position{Repeater: 3149999, Source: 3191868, Data: [7]byte{1, 2, 3, 4, 5, 6, 7}, Size: size}
		b, err := p.AppendBinary(nil)
		if err != nil {
			t.Fatal(err)
		}
		out["position "+string(rune('0'+size%10))] = b
	}
	return out
}

func TestSidebandFromMasterIsCountedNotQueued(t *testing.T) {
	t.Parallel()
	for name, p := range sidebandPackets(t) {
		c := newTestClient(t)
		m := metrics.NewMetrics()
		c.metrics = m
		cmd := dmrd.CommandDMRA
		if strings.HasPrefix(name, "position") {
			cmd = dmrd.CommandDMRG
		}
		c.outputs([]dmrdsession.Output{{Kind: dmrdsession.OutputData, Command: cmd, Packet: p}})
		if len(c.dataRX) != 0 {
			t.Fatalf("%s: queued for the DMRD handler", name)
		}
		reason := strings.Fields(name)[0]
		want := `mmdvm_packets_dropped_total{network="` + c.cfg.Name + `",reason="` + reason + `"} 1`
		if body := scrapeMetrics(t, m); !strings.Contains(body, want) {
			t.Fatalf("%s: metrics have no %s:\n%s", name, want, grepLines(body, "mmdvm_packets_dropped_total"))
		}
	}
}

func TestTruncatedPacketsFromMasterDoNotPanic(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	c.metrics = metrics.NewMetrics()
	for _, tag := range []string{"DMRA", "DMRG", "DMRD"} {
		for n := range 80 {
			p := make([]byte, n)
			copy(p, tag)
			for i := 4; i < n; i++ {
				p[i] = byte(i) * 37 //nolint:gosec // i is under 80
			}
			for _, cmd := range []dmrd.Command{dmrd.CommandDMRA, dmrd.CommandDMRG, dmrd.CommandDMRD} {
				c.outputs([]dmrdsession.Output{{Kind: dmrdsession.OutputData, Command: cmd, Packet: p}})
			}
			for len(c.dataRX) > 0 {
				c.handleReady(<-c.dataRX)
			}
			c.handleReady(p)
		}
	}
}
