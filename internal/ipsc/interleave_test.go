package ipsc

import (
	"encoding/hex"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
)

// interleavedMMDVMCalls returns the MMDVM packets of four calls, each on its
// own stream: the PI voice call of rigPICallBursts, the late-entry voice
// call of rigLateEntryBursts, the unconfirmed rate 3/4 packet of
// rigRate34Calls and the confirmed rate 1/2 packet of
// rigConfirmedRate12Packet.
func interleavedMMDVMCalls(t *testing.T) [][]dmrd.Data {
	t.Helper()
	pkt := func(stream, dst uint32, ft dmrd.FrameType, dt uint8, data [33]byte) dmrd.Data {
		return dmrd.Data{
			Source: 3191868, Destination: dst, Private: false, Slot: enums.Slot2,
			Frame: ft, Value: dt, Stream: stream, Burst: data,
		}
	}
	hexBurst := func(h string) [33]byte {
		w, err := hex.DecodeString(h)
		if err != nil || len(w) != 33 {
			t.Fatalf("bad burst %q", h)
		}
		return [33]byte(w)
	}
	calls := make([][]dmrd.Data, 0, 4)
	pi := make([]dmrd.Data, 0, len(rigPICallBursts()))
	for i, h := range rigPICallBursts() {
		k := rigPICallKinds()[i]
		pi = append(pi, pkt(78, 9, dmrd.FrameType(k[0]), k[1], hexBurst(h)))
	}
	calls = append(calls, pi)
	late := make([]dmrd.Data, 0, len(rigLateEntryBursts()))
	for i, h := range rigLateEntryBursts() {
		ft := dmrd.FrameVoice
		if i == burstA {
			ft = dmrd.FrameVoiceSync
		}
		late = append(late, pkt(79, 91, ft, uint8(i), hexBurst(h)))
	}
	calls = append(calls, late)
	c := rigRate34Calls()[0]
	r34 := make([]dmrd.Data, 0, len(c.bursts))
	for i, h := range c.bursts {
		r34 = append(r34, pkt(77, c.dst, dmrd.FrameDataSync, uint8(c.types[i]), hexBurst(h)))
	}
	calls = append(calls, r34)
	bursts, types := rigConfirmedRate12Packet(false)
	r12 := make([]dmrd.Data, 0, len(bursts))
	for i, b := range bursts {
		p := pkt(85, 3140598, dmrd.FrameDataSync, uint8(types[i]), b)
		p.Private, p.Slot = true, enums.Slot1
		r12 = append(r12, p)
	}
	return append(calls, r12)
}

// callFieldsCleared returns p without its call sequence number (byte 5)
// and call control (bytes 13-16), which a translator counts across calls.
func callFieldsCleared(p []byte) string {
	q := append([]byte(nil), p...)
	q[5] = 0
	clear(q[13:17])
	return hex.EncodeToString(q)
}

// TestInterleavedStreamsTranslateAsAlone sends the calls of
// interleavedMMDVMCalls to IPSC through one translator with their packets
// interleaved, and through a translator for each call alone. The translator
// decodes every voice burst into one reused layer2.Burst, whose decoded
// fields are valid only until its next decode, so a stream that kept any of
// them would see the burst of another stream. Each call must come out the
// same as it does alone, apart from the fields of callFieldsCleared.
func TestInterleavedStreamsTranslateAsAlone(t *testing.T) {
	t.Parallel()
	calls := interleavedMMDVMCalls(t)
	alone := make([][]string, len(calls))
	for i, call := range calls {
		tr := newTestTranslator(t)
		tr.SetColorCode(7)
		for _, p := range call {
			for _, out := range tr.TranslateToIPSC(p) {
				alone[i] = append(alone[i], callFieldsCleared(out))
			}
		}
	}
	tr := newTestTranslator(t)
	tr.SetColorCode(7)
	mixed := make([][]string, len(calls))
	for n := 0; ; n++ {
		sent := false
		for i, call := range calls {
			if n >= len(call) {
				continue
			}
			sent = true
			for _, out := range tr.TranslateToIPSC(call[n]) {
				mixed[i] = append(mixed[i], callFieldsCleared(out))
			}
		}
		if !sent {
			break
		}
	}
	for i := range calls {
		if len(alone[i]) == 0 {
			t.Fatalf("call %d: no packets", i)
		}
		if len(mixed[i]) != len(alone[i]) {
			t.Fatalf("call %d: %d packets interleaved, %d alone", i, len(mixed[i]), len(alone[i]))
		}
		for j := range alone[i] {
			if mixed[i][j] != alone[i][j] {
				t.Errorf("call %d packet %d:\ninterleaved %s\n      alone %s", i, j, mixed[i][j], alone[i][j])
			}
		}
	}
}

// TestInterleavedIPSCCallsTranslateAsAlone does the same for two IPSC
// calls to MMDVM, the PI voice call of rigPICall and the unconfirmed rate
// 3/4 packet of rigRate34Calls, apart from the MMDVM stream ID, which a
// translator counts across calls.
func TestInterleavedIPSCCallsTranslateAsAlone(t *testing.T) {
	t.Parallel()
	calls := [][]string{rigPICall(), rigRate34Calls()[0].packets}
	run := func(tr *IPSCTranslator, h string) []string {
		data, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		out := tr.TranslateToMMDVM(data[0], data)
		got := make([]string, 0, len(out))
		for _, p := range out {
			p.Stream = 0
			got = append(got, hex.EncodeToString(goldenDMRD(p)))
		}
		return got
	}
	alone := make([][]string, len(calls))
	for i, call := range calls {
		tr := newTestTranslator(t)
		tr.SetColorCode(7)
		for _, h := range call {
			alone[i] = append(alone[i], run(tr, h)...)
		}
	}
	tr := newTestTranslator(t)
	tr.SetColorCode(7)
	mixed := make([][]string, len(calls))
	for n := 0; n < max(len(calls[0]), len(calls[1])); n++ {
		for i, call := range calls {
			if n < len(call) {
				mixed[i] = append(mixed[i], run(tr, call[n])...)
			}
		}
	}
	for i := range calls {
		if len(alone[i]) == 0 || len(mixed[i]) != len(alone[i]) {
			t.Fatalf("call %d: %d packets interleaved, %d alone", i, len(mixed[i]), len(alone[i]))
		}
		for j := range alone[i] {
			if mixed[i][j] != alone[i][j] {
				t.Errorf("call %d packet %d:\ninterleaved %s\n      alone %s", i, j, mixed[i][j], alone[i][j])
			}
		}
	}
}
