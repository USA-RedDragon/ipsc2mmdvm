package mmdvm

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

// newPreambleTestClient returns a client that passes private calls on TS1
// to IPSC through a timeslot manager, and the recorder of its IPSC packets.
func newPreambleTestClient(t *testing.T) (*MMDVMClient, *ipscRecorder) {
	t.Helper()
	mgr := timeslot.NewManager()
	t.Cleanup(mgr.Stop)
	c := newSlotTestClient(t, "net1", mgr)
	c.netRewrites = []rewrite.Rule{
		&rewrite.TGRewrite{Name: c.cfg.Name, FromSlot: 1, FromTG: 9, ToSlot: 1, ToTG: 9, Range: 1},
		&rewrite.PassAllPC{Name: c.cfg.Name, Slot: 1},
	}
	rec := &ipscRecorder{}
	c.SetIPSCHandler(rec.handler("net1"))
	t.Cleanup(func() {
		c.preambleMu.Lock()
		for slot := range c.preambles {
			c.forgetPreambleCallLocked(slot)
		}
		c.preambleMu.Unlock()
	})
	return c, rec
}

// sendRig sends packets of rigPreambleCall to c with the stream IDs ids,
// as an MMDVM master sends them.
func sendRig(t *testing.T, c *MMDVMClient, packets []string, ids []uint32) {
	t.Helper()
	for i, h := range packets {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		p, err := dmrd.DecodeData(b)
		if err != nil {
			t.Fatalf("packet %d does not decode: %v", i, err)
		}
		p.Stream = ids[i]
		c.handleReady(encodeDMRD(p))
	}
}

// mmdvmHostIDs returns stream IDs in the form MMDVMHost gives the data
// call of rigPreambleCall: a new one for each CSBK and for the data header,
// whose blocks keep it.
func mmdvmHostIDs() []uint32 {
	return []uint32{101, 102, 103, 104, 105, 105, 105}
}

// TestMMDVMHostPreamblesAreOneCall sends the data call of rigPreambleCall
// with stream IDs as MMDVMHost gives them. An XPR 8400 sends the preamble
// CSBKs and the data that follows them as one call (cap/preamble.txt), so
// they go out at once in one IPSC call: one call control and byte 5, the
// RTP marker on the first packet only and the end flag on the last.
func TestMMDVMHostPreamblesAreOneCall(t *testing.T) {
	t.Parallel()
	c, rec := newPreambleTestClient(t)
	start := time.Now()
	sendRig(t, c, rigPreambleCall()[8:], mmdvmHostIDs())
	recs := rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 7 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("data call went out after %v", d)
	}
	for i, r := range recs {
		p, first := r.raw, recs[0].raw
		if r.callControl != recs[0].callControl || p[5] != first[5] {
			t.Errorf("packet %d: call control %08x byte 5 %02x, want %08x %02x", i, r.callControl, p[5], recs[0].callControl, first[5])
		}
		if marker := p[19]&0x80 != 0; marker != (i == 0) {
			t.Errorf("packet %d: RTP marker %v", i, marker)
		}
		if end := p[17]&0x40 != 0; end != (i == 6) {
			t.Errorf("packet %d: end flag %v", i, end)
		}
	}
	c.preambleMu.Lock()
	pc := c.preambles[0]
	c.preambleMu.Unlock()
	if pc != nil {
		t.Error("call still followed after its last block")
	}
	time.Sleep(2 * preambleGap)
	if n := len(rec.snapshot()); n != 7 {
		t.Fatalf("got %d packets after the call ended, want 7", n)
	}
}

// xprPreambleEnd is the packet an XPR 8400 sent on TS2 to end call control
// 0x934 of cap/preamble.txt, whose preamble CSBKs no data followed.
const xprPreambleEnd = "840004ce2e5730b43c30b43c010000093460805e4c74706966a60000000013800000"

// TestPreamblesWithoutDataEnd sends the preamble CSBKs of rigPreambleCall
// and no data, with stream IDs as MMDVMHost gives them and with one stream
// ID as the bridge gives them. When no more comes, the call ends on IPSC as
// an XPR 8400 ends it, with a 0x13 packet with the end flag, and leaves the
// timeslot to the next call.
func TestPreamblesWithoutDataEnd(t *testing.T) {
	t.Parallel()
	for _, ids := range [][]uint32{mmdvmHostIDs()[:4], {7, 7, 7, 7}} {
		c, rec := newPreambleTestClient(t)
		sendRig(t, c, rigPreambleCall()[8:11], ids[:3])
		last := time.Now()
		sendRig(t, c, rigPreambleCall()[11:12], ids[3:])
		recs := rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 5 })
		end, want := recs[4].raw, mustHex(t, xprPreambleEnd)
		if len(end) != len(want) || end[0] != want[0] || end[12] != want[12] || end[17] != want[17]&^0x20 ||
			hex.EncodeToString(end[18:20]) != hex.EncodeToString(want[18:20]) ||
			end[30] != want[30] || end[31] != want[31]&^0x80 || hex.EncodeToString(end[32:]) != hex.EncodeToString(want[32:]) {
			t.Errorf("ids %v: end packet\n got %x\nwant %x on TS1", ids, end, want)
		}
		if recs[4].callControl != recs[0].callControl || end[5] != recs[0].raw[5] {
			t.Errorf("ids %v: end packet is not in the call: %x", ids, end)
		}
		if d := recs[4].at.Sub(last); d < preambleGap || d > preambleGap+time.Second {
			t.Errorf("ids %v: end packet %v after the last preamble", ids, d)
		}

		start := time.Now()
		sendRig(t, c, rigPreambleCall()[:1], []uint32{5})
		rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 8 })
		if d := time.Since(start); d > time.Second {
			t.Fatalf("ids %v: next call went out after %v", ids, d)
		}
	}
}

// TestOtherCallEndsPreambleCall sends a preamble CSBK of rigPreambleCall,
// then at once the voice call of rigPreambleCall on the same timeslot. The
// voice call shows the preamble call to be over, so the preamble call ends
// before the voice call starts, without waiting for preambleGap.
func TestOtherCallEndsPreambleCall(t *testing.T) {
	t.Parallel()
	c, rec := newPreambleTestClient(t)
	call := rigPreambleCall()
	start := time.Now()
	sendRig(t, c, []string{call[8], call[0]}, []uint32{101, 5})
	recs := rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 5 })
	if d := time.Since(start); d >= preambleGap {
		t.Fatalf("voice call went out after %v", d)
	}
	if recs[1].burstType != 0x13 || recs[1].raw[17]&0x40 == 0 || recs[2].burstType != 0x01 {
		t.Fatalf("got burst types %02x %02x %02x, want the CSBK, its end, and the voice header", recs[0].burstType, recs[1].burstType, recs[2].burstType)
	}
}

// rigQueuedPreambleCall returns the TS1 packets that an MMDVM master sent
// on the rig (runs/queued-rtl-dead/inject.txt, 500 ms to 1640 ms): a group
// voice call from 3191868 to 9 with stream ID 0xfe1f88cd, and while it
// holds the timeslot, a data call from 3191868 to 3140598 in the form
// MMDVMHost sends it, four preamble CSBKs and a confirmed rate 1/2 packet,
// each with a new stream ID, then a group rate 3/4 data call to 9.
func rigQueuedPreambleCall() []string {
	return []string{
		"444d52440030b43c000009003010af21fe1f88cd00d40ada05cc0e703f004d80dc4dff57d75df5d36194140039c03be02f01b20054",
		"444d52440130b43c000009003010af10fe1f88cd6f7d2c51611e1813bb0927dee94755fd7df75f71bbfd6724638188a7a161c49575",
		"444d52440230b43c000009003010af01fe1f88cd8a3e8c022d191619eeaa053b03c73000009003849ea5bcfdf812bca894359e323a",
		"444d52440330b43c000009003010af02fe1f88cd4922df4c3e3bd60c0f3432b218977060a0606dd16e595bc6c5fc8e707c1726043d",
		"444d52440430b43c000009003010af03fe1f88cd62b67b404c70f4053a0dca5bf7077090c240cddf8428b241d17b05dd18bf1b0b70",
		"444d52440530b43c000009003010af04fe1f88cd9d3b07147149793a31b2bcd2f0c750c2b2b24ae3805c3f7964072cffc3606f58eb",
		"444d52440630b43c000009003010af05fe1f88cde7588ce8e83658da7b9e26672e171000000004b0e3074705b103f7dff1760aabbe",
		"444d52440030b43c2febf6003010af630a319df74a540cd5a14c3b5472e8ce721cedff57d75df5d798be5488f5a8f961b8df2c96d9",
		"444d52440730b43c000009003010af10fe1f88cd298d0a33fd2bbc43cc28124ac5a755fd7df75f7138d2514ab1ba41155039c90579",
		"444d52440130b43c2febf6003010af634f2adbb54a300c55a0543b6c7689c6d01cedff57d75df5d798767428f568f9a1b05f3616dd",
		"444d52440830b43c000009003010af01fe1f88cdcb375d2a6f00ccf547faacd653f730000090038be3fa9874d4cd7a0a1be7a21f8c",
		"444d52440230b43c2febf6003010af6329769f914a700cd7a148397474b84a901cedff57d75df5d798065418b588fa01b59f3f16eb",
		"444d52440930b43c000009003010af02fe1f88cd626757029ce3de90092c67627b577060a0606dddeb9fd47377687f8c79688c6888",
		"444d52440330b43c2febf6003010af638b8cf2074a710c1da05c394474894e121cedff57d75df5d7985235883528fa81b9db3f16c8",
		"444d52440a30b43c000009003010af03fe1f88cd6b57cba90b6aefecc02847dea2b77090c240cddd28430f7625469dd2c4af5b44b2",
		"444d52440430b43c2febf6003010af66a0fc919b20f7370570bee26393b7cda7ddadff57d75df5de6accf9067160278e89420a3c95",
		"444d52440b30b43c000009003010af04fe1f88cdf1a3e06b49ade23799e66aa4a4b750c2b2b24ae8a07221c799fa5f3614369fac04",
		"444d52440530b43c2febf6003010af67a0fc919b06dd55694b3555ba4425c5649dcdff57d75df5ddc51065d0864171a60d174dab0b",
		"444d52440c30b43c000009003010af05fe1f88cd125d0cc5c54a901ae901dbc4c1171000000004b437fd4bbba5262cb8c2dba64259",
		"444d52440630b43c2febf6003010af67a0fc919b403201b0010f0182003002c01dcdff57d75df5ddc42001b002541b4c08007630d5",
		"444d52440d30b43c000009003010af10fe1f88cd7cbefe0097daea42f535a160269755fd7df75f7f58102289cb60f34405b36331d7",
		"444d52440e30b43c000009003010af01fe1f88cdeba8cf4f35ecbc900fd0d641a417300000900382a2bc16dce18d93230d37e85dd8",
		"444d52440f30b43c000009003010af02fe1f88cd1a039178a63494bc4c4375b4d2a77060a0606ddd6cd1c3f57d91714de2043bbcec",
		"444d52440030b43c000009003010af2609d26c65081f86c810e222381764d761ddadff57d75df5de683018fa32b8216984480490f3",
		"444d52441030b43c000009003010af03fe1f88cd23aa419590ac57e5713aef5dd4677090c240cddf7c5cbfeb3e92b4b376e9c83ea5",
		"444d52440130b43c000009003010af2809d26c65e669e3f222cdf5d5e00022edde1dff57d75df5d7d4ea2a3a322ce4762f79222166",
		"444d52441130b43c000009003010af04fe1f88cd4751b180dcb3f4ac49d1a58168f750c2b2b24aecdf4b1c36dc838b77349a9582e1",
		"444d52441230b43c000009003010af05fe1f88cd4b46394c5de1c78b5c1acb7b1de71000000004b58dd927533840339364a3b308eb",
		"444d52441330b43c000009003010af22fe1f88cd00bb0a0e057c0e083f704d20dc8dff57d75df5d4348017383ea037a02701ab0067",
	}
}

// TestPreambleCallQueuedBehindVoiceIsOneCall sends rigQueuedPreambleCall.
// The voice call holds the timeslot while the data call with preambles
// waits for it, and the voice packets between the preambles must not end
// the data call: it goes out after the voice call as one IPSC call, its
// preambles, header and blocks with one call control and the end flag on
// its last block, and no 0x13 packet ends any call.
func TestPreambleCallQueuedBehindVoiceIsOneCall(t *testing.T) {
	t.Parallel()
	c, rec := newPreambleTestClient(t)
	for i, h := range rigQueuedPreambleCall() {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		c.handleReady(b)
	}
	recs := rec.waitFor(t, func(recs []ipscRecord) bool {
		n := 0
		for _, r := range recs {
			if r.raw[0] == 0x84 {
				n++
			}
		}
		return n >= 7
	})
	var calls []uint32
	types := map[uint32][]string{}
	for _, r := range recs {
		if r.burstType == 0x13 {
			t.Errorf("call %#x ended with a 0x13 packet", r.callControl)
		}
		if r.raw[0] != 0x84 {
			continue
		}
		if _, ok := types[r.callControl]; !ok {
			calls = append(calls, r.callControl)
		}
		tag := hex.EncodeToString([]byte{r.burstType})
		if r.raw[17]&0x40 != 0 {
			tag += "E"
		}
		types[r.callControl] = append(types[r.callControl], tag)
	}
	if len(calls) != 1 {
		t.Fatalf("private data went out as %d calls, want 1: %v", len(calls), types)
	}
	if got, want := strings.Join(types[calls[0]], " "), "03 03 03 03 06 07 07E"; got != want {
		t.Errorf("private data call bursts %s, want %s", got, want)
	}
}

// TestPreambleJoinsOnlyItsCall sends a preamble CSBK of rigPreambleCall,
// then its data packet with the destination changed, or after preambleGap,
// or after preambleGap without the timer that ends the preamble call having
// run yet. In none of them is the data packet part of the preamble call.
func TestPreambleJoinsOnlyItsCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{"other destination", "late", "late before the timer"} {
		c, rec := newPreambleTestClient(t)
		call := rigPreambleCall()
		sendRig(t, c, call[8:9], []uint32{101})
		data := append([]string(nil), call[12:]...)
		switch tc {
		case "other destination":
			for i, h := range data {
				b := mustHex(t, h)
				b[10]++
				data[i] = hex.EncodeToString(b)
			}
		case "late":
			time.Sleep(preambleGap + 100*time.Millisecond)
		case "late before the timer":
			c.preambleMu.Lock()
			c.preambles[0].seen = c.preambles[0].seen.Add(-preambleGap)
			c.preambleMu.Unlock()
		}
		sendRig(t, c, data, []uint32{105, 105, 105})
		recs := rec.waitFor(t, func(recs []ipscRecord) bool { return len(recs) == 5 })
		if recs[2].callControl == recs[0].callControl {
			t.Errorf("%s: data header joined the preamble call", tc)
		}
	}
}
