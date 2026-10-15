package ipsc

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	mmdvm "github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

// embeddedLCOf decodes the embedded LC of four fragments as MMDVMHost does
// and fails the test when it does not decode.
func embeddedLCOf(t *testing.T, fragments [4][4]byte) [9]byte {
	t.Helper()
	lc, result := layer2.DecodeEmbeddedLC(fragments, true)
	if result.Uncorrectable {
		t.Fatal("embedded LC does not decode")
	}
	return lc
}

func fragmentsOf(t *testing.T, bursts [][33]byte) [4][4]byte {
	t.Helper()
	if len(bursts) != 4 {
		t.Fatalf("expected 4 bursts, got %d", len(bursts))
	}
	var fragments [4][4]byte
	for f, data := range bursts {
		raw := layer2.RawBurst(data)
		fragments[f] = raw.EmbeddedData()
	}
	return fragments
}

func checkEmbeddedLC(t *testing.T, fragments [4][4]byte, wantSrc, wantDst uint) {
	t.Helper()
	lc := embeddedLCOf(t, fragments)
	dst := uint(lc[3])<<16 | uint(lc[4])<<8 | uint(lc[5])
	src := uint(lc[6])<<16 | uint(lc[7])<<8 | uint(lc[8])
	if dst != wantDst || src != wantSrc {
		t.Fatalf("embedded LC % X: dst %d src %d, want dst %d src %d", lc, dst, src, wantDst, wantSrc)
	}
	if got := layer2.EncodeEmbeddedLC(lc); got != fragments {
		t.Fatalf("EncodeEmbeddedLC(% X) does not reproduce the on-air bits", lc)
	}
}

// TestEncodeEmbeddedLCMatchesAirData checks the embedded LC encoder against
// bursts B-E of a real call received from an MMDVM master (from ok-dmrlib's
// test_vbptc_128_72.py).
func TestEncodeEmbeddedLCMatchesAirData(t *testing.T) {
	t.Parallel()
	frames := []string{
		"444d52446920baf80008650020baef81212a684978f8e0361b6519cdd55ad9c3301130a00030a91b7529dee349fbe3147e040bc9d10000",
		"444d52446a20baf80008650020baef82212a6849c762a2114c736c7a45f562c133617170a06057439c9df11e936ec26335ecf569bf0000",
		"444d52446b20baf80008650020baef83212a6849f30c872376d6102d4791df85442170c112200747b289e11dd5c2877046b1e36bcf0000",
		"444d52446c20baf80008650020baef84212a6849e1e48370246e951422bda7c73511505223f3a07309cda701bdb6e4733318ef91220000",
	}
	bursts := make([][33]byte, 0, len(frames))
	var pkt mmdvm.Packet
	for i, f := range frames {
		data, err := hex.DecodeString(f)
		if err != nil {
			t.Fatal(err)
		}
		var ok bool
		pkt, ok = mmdvm.Decode(data)
		if !ok || pkt.DTypeOrVSeq != uint(i+1) { //nolint:gosec // i is in [0,3]
			t.Fatalf("frame %d: decode ok=%t vseq=%d", i, ok, pkt.DTypeOrVSeq)
		}
		bursts = append(bursts, pkt.DMRData)
	}
	checkEmbeddedLC(t, fragmentsOf(t, bursts), pkt.Src, pkt.Dst)
}

func burstWithLC(t *testing.T, lc9 [9]byte, dataType enums.DataType, colorCode enums.ColorCode) [33]byte {
	t.Helper()
	return buildLCDataBurst(t, testFullLC(t, lc9, dataType), dataType, colorCode)
}

func lcPacket(dataType enums.DataType, src, dst uint, group bool, data [33]byte) mmdvm.Packet {
	return mmdvm.Packet{
		Signature: mmdvm.DMRDSignature, Src: src, Dst: dst, GroupCall: group,
		FrameType: mmdvmFrameTypeDataSync, DTypeOrVSeq: uint(dataType), StreamID: 7, DMRData: data,
	}
}

func voicePacket(vseq uint, lcss enums.LCSS) mmdvm.Packet {
	b := layer2.Burst{Kind: layer2.KindVoice}
	if vseq == 0 {
		b.SyncPattern = enums.SyncMSSourcedVoice
		b.VoiceBurst = enums.VoiceBurstA
	} else {
		b.SyncPattern = enums.SyncNone
		b.EmbeddedSignalling = pdu.EmbeddedSignalling{ColorCode: 3, PreemptionAndPowerControlIndicator: true, LCSS: lcss}
	}
	ft := mmdvmFrameTypeVoice
	if vseq == 0 {
		ft = mmdvmFrameTypeVoiceSync
	}
	data, err := b.Encode()
	if err != nil {
		panic(err)
	}
	if vseq != 0 {
		data.SetEmbeddedData([4]byte{0x55, 0x55, 0x55, 0x55})
	}
	return mmdvm.Packet{
		Signature: mmdvm.DMRDSignature, Src: 1234, Dst: 5555,
		FrameType: ft, DTypeOrVSeq: vseq, StreamID: 7, DMRData: data,
	}
}

func decodedLC(t *testing.T, pkt mmdvm.Packet, dataType enums.DataType) [12]byte {
	t.Helper()
	lc, ok := decodeBPTCPayload(pkt.DMRData)
	if !ok || !layer2.MMDVMHostAccepts(dataType, lc) {
		t.Fatalf("%s: invalid full LC % X", dataType.String(), lc)
	}
	return lc
}

// sameBits reports whether a and b agree on bits [from, to).
func sameBits(a, b [33]byte, from, to int) bool {
	for i := from; i < to; i++ {
		if a[i/8]>>(7-i%8)&1 != b[i/8]>>(7-i%8)&1 {
			return false
		}
	}
	return true
}

func TestLCRewriterPrivateToGroup(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	// Protect flag, Motorola FID, emergency + privacy service options.
	priv := [9]byte{0x83, 0x10, 0xC0, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	want := [9]byte{0x80, 0x00, 0xC0, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}

	for _, dt := range []enums.DataType{enums.DataTypeVoiceLCHeader, enums.DataTypeTerminatorWithLC} {
		orig := lcPacket(dt, 1234, 5555, false, burstWithLC(t, priv, dt, 5))
		pkt := orig
		pkt.Dst, pkt.GroupCall = 9, true
		r.Rewrite(orig, &pkt)
		if got := decodedLC(t, pkt, dt); got != testFullLC(t, want, dt) {
			t.Fatalf("%s: LC % X, want % X", dt.String(), got, testFullLC(t, want, dt))
		}
		if !sameBits(orig.DMRData, pkt.DMRData, 98, 166) {
			t.Fatalf("%s: slot type or sync changed", dt.String())
		}
	}
}

func TestLCRewriterKeepsFeatureSetForSameCallType(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	in := [9]byte{0x00, 0x10, 0x20, 0x00, 0x0C, 0x30, 0x2F, 0x9B, 0xE5}
	want := [9]byte{0x00, 0x10, 0x20, 0x00, 0x00, 0x5B, 0x2F, 0x9B, 0xE5}
	orig := lcPacket(enums.DataTypeVoiceLCHeader, 0x2F9BE5, 3120, true, burstWithLC(t, in, enums.DataTypeVoiceLCHeader, 1))
	pkt := orig
	pkt.Dst = 91
	r.Rewrite(orig, &pkt)
	if got := decodedLC(t, pkt, enums.DataTypeVoiceLCHeader); got != testFullLC(t, want, enums.DataTypeVoiceLCHeader) {
		t.Fatalf("LC % X, want % X", got, testFullLC(t, want, enums.DataTypeVoiceLCHeader))
	}
}

// TestLCRewriterClearsReservedBit checks that a call type change clears the
// reserved bit of octet 0 and keeps the protect flag, as DMRGateway does.
func TestLCRewriterClearsReservedBit(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	in := [9]byte{0xC3, 0x00, 0x20, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	want := [9]byte{0x80, 0x00, 0x20, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}
	orig := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, burstWithLC(t, in, enums.DataTypeVoiceLCHeader, 1))
	pkt := orig
	pkt.Dst, pkt.GroupCall = 9, true
	r.Rewrite(orig, &pkt)
	if got := decodedLC(t, pkt, enums.DataTypeVoiceLCHeader); got != testFullLC(t, want, enums.DataTypeVoiceLCHeader) {
		t.Fatalf("LC % X, want % X", got, testFullLC(t, want, enums.DataTypeVoiceLCHeader))
	}
}

func TestLCRewriterBuildsLCWhenBurstLCIsInvalid(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	orig := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, [33]byte{})
	pkt := orig
	pkt.Dst, pkt.GroupCall = 9, true
	r.Rewrite(orig, &pkt)
	if got, want := decodedLC(t, pkt, enums.DataTypeVoiceLCHeader), constructFullLC(1234, 9, true, enums.DataTypeVoiceLCHeader); got != want {
		t.Fatalf("LC % X, want % X", got, want)
	}
}

func TestLCRewriterLeavesUnchangedPacketsAlone(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	in := [9]byte{0x03, 0x00, 0x00, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	orig := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, burstWithLC(t, in, enums.DataTypeVoiceLCHeader, 1))
	pkt := orig
	pkt.Slot = true
	r.Rewrite(orig, &pkt)
	if pkt.DMRData != orig.DMRData {
		t.Fatal("a slot-only rewrite changed the burst")
	}
	v := voicePacket(1, enums.LCSSFirstFragment)
	vp := v
	r.Rewrite(v, &vp)
	if vp.DMRData != v.DMRData {
		t.Fatal("an unrewritten voice burst changed")
	}
	if len(r.streams) != 0 {
		t.Fatalf("unrewritten packets left %d streams", len(r.streams))
	}
}

func TestLCRewriterRewritesEmbeddedLC(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	priv := [9]byte{0x03, 0x00, 0x80, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	orig := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, burstWithLC(t, priv, enums.DataTypeVoiceLCHeader, 1))
	pkt := orig
	pkt.Dst, pkt.GroupCall = 9, true
	r.Rewrite(orig, &pkt)

	lcss := [6]enums.LCSS{enums.LCSSSingleFragment, enums.LCSSFirstFragment, enums.LCSSContinuationFragment,
		enums.LCSSContinuationFragment, enums.LCSSSingleFragment, enums.LCSSSingleFragment}
	var bursts [][33]byte
	for vseq := uint(0); vseq < 6; vseq++ {
		v := voicePacket(vseq, lcss[vseq])
		out := v
		out.Dst, out.GroupCall = 9, true
		r.Rewrite(v, &out)
		if vseq == 0 || vseq == 5 {
			if out.DMRData != v.DMRData {
				t.Fatalf("burst %c changed", 'A'+vseq)
			}
			continue
		}
		bursts = append(bursts, out.DMRData)
		raw := layer2.RawBurst(out.DMRData)
		emb := raw.EMB()
		if emb.FEC.Uncorrectable || emb.LCSS != defaultLCSS(int(vseq)) || emb.ColorCode != 3 || !emb.PreemptionAndPowerControlIndicator { //nolint:gosec // vseq is in [1,4]
			t.Fatalf("burst %c EMB %s", 'A'+vseq, emb.String())
		}
		if !sameBits(v.DMRData, out.DMRData, 0, 108) || !sameBits(v.DMRData, out.DMRData, 156, 264) {
			t.Fatalf("burst %c voice bits changed", 'A'+vseq)
		}
	}
	raw := fragmentsOf(t, bursts)
	checkEmbeddedLC(t, raw, 1234, 9)
	if lc := embeddedLCOf(t, raw); lc != [9]byte{0x00, 0x00, 0x80, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2} {
		t.Fatalf("embedded LC % X keeps the old call type or service options", lc)
	}

	term := lcPacket(enums.DataTypeTerminatorWithLC, 1234, 5555, false, burstWithLC(t, priv, enums.DataTypeTerminatorWithLC, 1))
	tp := term
	tp.Dst, tp.GroupCall = 9, true
	r.Rewrite(term, &tp)
	if len(r.streams) != 0 {
		t.Fatal("terminator did not end the stream")
	}
}

func TestLCRewriterLateEntryUsesPacketFields(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	var bursts [][33]byte
	for vseq := uint(1); vseq <= 4; vseq++ {
		v := voicePacket(vseq, defaultLCSS(int(vseq))) //nolint:gosec // vseq is in [1,4]
		out := v
		out.Dst, out.GroupCall = 9, true
		r.Rewrite(v, &out)
		bursts = append(bursts, out.DMRData)
	}
	checkEmbeddedLC(t, fragmentsOf(t, bursts), 1234, 9)
}

// TestLCRewriterOutputCarriesRewrite checks that the rewritten LCs are in
// the output bytes as a receiver decodes them: the voice header decodes to
// the new call with no FEC error, and the embedded LC of bursts B-E decodes
// to the new addresses. A dmrgo Burst decoded from a rewritten burst and
// encoded again gives the same bytes, so the rewrite survives a hop that
// decodes and forwards it.
func TestLCRewriterOutputCarriesRewrite(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	priv := [9]byte{0x03, 0x00, 0x80, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	hdr := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, burstWithLC(t, priv, enums.DataTypeVoiceLCHeader, 1))
	out := hdr
	out.Dst, out.GroupCall = 9, true
	r.Rewrite(hdr, &out)
	if out.DMRData == hdr.DMRData {
		t.Fatal("the voice header was not rewritten")
	}

	dt := enums.DataTypeVoiceLCHeader
	var b layer2.Burst
	if err := b.DecodeFromBytesWith(out.DMRData, &layer2.DecodeOptions{Kind: layer2.KindData, DataType: &dt}); err != nil {
		t.Fatal(err)
	}
	flc, ok := b.Data.(*pdu.FullLinkControl)
	if !ok || flc.FEC.Uncorrectable || flc.FEC.ErrorsCorrected != 0 {
		t.Fatalf("voice header decodes to %T with FEC %+v", b.Data, b.FEC)
	}
	if target, source, ok := flc.Addresses(); !ok || flc.FLCO != enums.FLCOGroupVoiceChannelUser || target != 9 || source != 1234 {
		t.Fatalf("voice header LC %s, want a group call from 1234 to 9", flc.String())
	}
	if again, err := b.Encode(); err != nil || again != out.DMRData {
		t.Fatalf("voice header does not encode back to the rewritten bytes: %v", err)
	}

	var fragments [4][4]byte
	for vseq := uint(1); vseq <= 4; vseq++ {
		v := voicePacket(vseq, defaultLCSS(int(vseq))) //nolint:gosec // vseq is in [1,4]
		o := v
		o.Dst, o.GroupCall = 9, true
		r.Rewrite(v, &o)
		if o.DMRData == v.DMRData {
			t.Fatalf("burst %c was not rewritten", 'A'+vseq)
		}
		var vb layer2.Burst
		opts := layer2.DecodeOptions{Kind: layer2.KindVoice, VoiceBurst: enums.VoiceBurstA + enums.VoiceBurstType(vseq)} //nolint:gosec // vseq is in [1,4]
		if err := vb.DecodeFromBytesWith(o.DMRData, &opts); err != nil {
			t.Fatal(err)
		}
		if again, err := vb.Encode(); err != nil || again != o.DMRData {
			t.Fatalf("burst %c does not encode back to the rewritten bytes: %v", 'A'+vseq, err)
		}
		raw := layer2.RawBurst(o.DMRData)
		fragments[vseq-1] = raw.EmbeddedData()
	}
	lc := layer2.DecodeEmbeddedFullLinkControl(fragments, true)
	if target, source, ok := lc.Addresses(); lc.FEC.Uncorrectable || !ok || target != 9 || source != 1234 {
		t.Fatalf("embedded LC %s, want a call from 1234 to 9", lc.String())
	}
}

func TestLCRewriterPrunesIdleStreams(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	clock := &fakeClock{t: time.Unix(1000, 0)}
	r.now = clock.now
	v := voicePacket(1, enums.LCSSFirstFragment)
	out := v
	out.Dst = 9
	r.Rewrite(v, &out)
	if len(r.streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(r.streams))
	}
	clock.t = clock.t.Add(streamIdleTimeout + pruneInterval + time.Second)
	v.StreamID = 8
	out = v
	out.Dst = 9
	r.Rewrite(v, &out)
	if _, ok := r.streams[7]; ok || len(r.streams) != 1 {
		t.Fatalf("idle stream not pruned: %d streams", len(r.streams))
	}
}
