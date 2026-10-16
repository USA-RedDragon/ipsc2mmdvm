package ipsc

import (
	"encoding/hex"
	"testing"
	"time"
)

// TestMBCContinuationAfterDataEndIsDropped sends an MBC continuation in
// the call of TestDataBlockAfterEndIsDropped after the call ended, by the
// end flag or by the 0x13 end packet alone. The headerless check covers
// only rate 1/2, 3/4 and 1 blocks, so the record of the ended call must
// drop it, and no stream is left open.
func TestMBCContinuationAfterDataEndIsDropped(t *testing.T) {
	t.Parallel()
	packets := []string{
		"840004ce2e2e30b43c30b43c0100006a7c0080ddedea3659efc9000000000640000a800a006043a230b43c30b43c8100d06d00764679",
		"840004ce2e2e30b43c30b43c0100006a7c40805eedeb3659f1a9000000000642000a800a0060b800000004ce2e30b43cd6a800764692",
		"840004ce2e2e30b43c30b43c0100006a7c40805eedec3659f3890000000013000000",
	}
	mbc, _ := hex.DecodeString("840004ce2e2e30b43c30b43c0100006a7c00805deded3659f569000000000840000d800a0090444d52474f20544553540000a3ecda7c002e0078469b")
	mbc[30] = ipscBurstMBCContinuation
	for _, skip := range []int{-1, 1} {
		tr := newTestTranslator(t)
		for i, h := range packets {
			if i == skip {
				continue
			}
			d, _ := hex.DecodeString(h)
			tr.TranslateToMMDVM(d[0], d)
		}
		if out := tr.TranslateToMMDVM(mbc[0], mbc); len(out) != 0 {
			t.Fatalf("without packet %d: MBC continuation after the end gave %d packets", skip, len(out))
		}
		if len(tr.reverseStreams) != 0 {
			t.Fatalf("without packet %d: %d streams open", skip, len(tr.reverseStreams))
		}
	}
}

// TestEndedDataCallsArePruned checks that the record of an ended IPSC
// data call goes once it has been idle for longer than streamIdleTimeout.
func TestEndedDataCallsArePruned(t *testing.T) {
	t.Parallel()
	tr, clock, _ := newClockedTranslator(t)
	hdr, _ := hex.DecodeString("840004ce2e2e30b43c30b43c0100006a7c0080ddedea3659efc9000000000640000a800a006043a230b43c30b43c8100d06d00764679")
	end, _ := hex.DecodeString("840004ce2e2e30b43c30b43c0100006a7c40805eedec3659f3890000000013000000")
	tr.TranslateToMMDVM(hdr[0], hdr)
	tr.TranslateToMMDVM(end[0], end)
	if len(tr.endedData) != 1 {
		t.Fatalf("%d ended data calls recorded, want 1", len(tr.endedData))
	}
	clock.t = clock.t.Add(streamIdleTimeout + time.Minute)
	tr.TranslateToMMDVM(hdr[0], append([]byte(nil), hdr...))
	if _, ok := tr.endedData[0x6a7c]; ok {
		t.Fatal("ended data call still recorded after streamIdleTimeout")
	}
}
