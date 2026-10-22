package ipsc

import (
	"encoding/hex"
	"testing"
)

// TestMBCContinuationAfterDataEndPassesOn sends an MBC continuation in
// the call of TestDataBlockAfterEndPassesOn after the call ended, by the
// end flag or by the 0x13 end packet alone. It goes to MMDVM as a stream
// of its own, and no stream is left open.
func TestMBCContinuationAfterDataEndPassesOn(t *testing.T) {
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
		if out := tr.TranslateToMMDVM(mbc[0], mbc); len(out) != 1 || out[0].Value != ipscBurstMBCContinuation {
			t.Fatalf("without packet %d: MBC continuation after the end gave %+v", skip, out)
		}
		if len(tr.reverseStreams) != 0 {
			t.Fatalf("without packet %d: %d streams open", skip, len(tr.reverseStreams))
		}
	}
}
