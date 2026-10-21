package ipsc

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
	"github.com/USA-RedDragon/dmrgo/v2/layer2/pdu"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
)

// positionLCSS returns the LCSS of the EMB of the voice burst at superframe
// position pos (1-5 for B-F) in a superframe with an embedded LC.
func positionLCSS(pos int) enums.LCSS {
	lcss, _ := voiceBurst(pos).LCSS()
	return lcss
}

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

func checkEmbeddedLC(t *testing.T, fragments [4][4]byte, wantSrc, wantDst uint32) {
	t.Helper()
	lc := embeddedLCOf(t, fragments)
	dst := uint32(lc[3])<<16 | uint32(lc[4])<<8 | uint32(lc[5])
	src := uint32(lc[6])<<16 | uint32(lc[7])<<8 | uint32(lc[8])
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
	var pkt dmrd.Data
	for i, f := range frames {
		data, err := hex.DecodeString(f)
		if err != nil {
			t.Fatal(err)
		}
		pkt, err = dmrd.DecodeData(data)
		if err != nil || pkt.Value != uint8(i+1) { //nolint:gosec // i is in [0,3]
			t.Fatalf("frame %d: decode %v vseq=%d", i, err, pkt.Value)
		}
		bursts = append(bursts, pkt.Burst)
	}
	checkEmbeddedLC(t, fragmentsOf(t, bursts), pkt.Source, pkt.Destination)
}

func burstWithLC(t *testing.T, lc9 [9]byte, dataType enums.DataType, colorCode enums.ColorCode) [33]byte {
	t.Helper()
	return buildLCDataBurst(t, testFullLC(t, lc9, dataType), dataType, colorCode)
}

func lcPacket(dataType enums.DataType, src, dst uint32, group bool, data [33]byte) dmrd.Data {
	return dmrd.Data{
		Source: src, Destination: dst, Private: !group,
		Frame: dmrd.FrameDataSync, Value: uint8(dataType), Stream: 7, Burst: data,
	}
}

func voicePacket(vseq uint, lcss enums.LCSS) dmrd.Data {
	b := layer2.Burst{Kind: layer2.KindVoice}
	if vseq == 0 {
		b.SyncPattern = enums.SyncMSSourcedVoice
		b.VoiceBurst = enums.VoiceBurstA
	} else {
		b.SyncPattern = enums.SyncNone
		b.EmbeddedSignalling = pdu.EmbeddedSignalling{ColorCode: 3, PreemptionAndPowerControlIndicator: true, LCSS: lcss}
	}
	ft := dmrd.FrameVoice
	if vseq == 0 {
		ft = dmrd.FrameVoiceSync
	}
	data, err := b.Encode()
	if err != nil {
		panic(err)
	}
	if vseq != 0 {
		data.SetEmbeddedData([4]byte{0x55, 0x55, 0x55, 0x55})
	}
	return dmrd.Data{
		Private: true,
		Source:  1234, Destination: 5555,
		Frame: ft, Value: uint8(vseq), Stream: 7, Burst: data, //nolint:gosec // vseq is 0 to 5
	}
}

func decodedLC(t *testing.T, pkt dmrd.Data, dataType enums.DataType) [12]byte {
	t.Helper()
	lc, ok := decodeBPTCPayload(pkt.Burst, dataType)
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
		pkt.Destination, pkt.Private = 9, false
		r.Rewrite(orig, &pkt)
		if got := decodedLC(t, pkt, dt); got != testFullLC(t, want, dt) {
			t.Fatalf("%s: LC % X, want % X", dt.String(), got, testFullLC(t, want, dt))
		}
		if !sameBits(orig.Burst, pkt.Burst, 98, 166) {
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
	pkt.Destination = 91
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
	pkt.Destination, pkt.Private = 9, false
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
	pkt.Destination, pkt.Private = 9, false
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
	pkt.Slot = enums.Slot2
	r.Rewrite(orig, &pkt)
	if pkt.Burst != orig.Burst {
		t.Fatal("a slot-only rewrite changed the burst")
	}
	v := voicePacket(1, enums.LCSSFirstFragment)
	vp := v
	r.Rewrite(v, &vp)
	if vp.Burst != v.Burst {
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
	pkt.Destination, pkt.Private = 9, false
	r.Rewrite(orig, &pkt)

	lcss := [6]enums.LCSS{enums.LCSSSingleFragment, enums.LCSSFirstFragment, enums.LCSSContinuationFragment,
		enums.LCSSContinuationFragment, enums.LCSSSingleFragment, enums.LCSSSingleFragment}
	var bursts [][33]byte
	for vseq := uint(0); vseq < 6; vseq++ {
		v := voicePacket(vseq, lcss[vseq])
		out := v
		out.Destination, out.Private = 9, false
		r.Rewrite(v, &out)
		if vseq == 0 || vseq == 5 {
			if out.Burst != v.Burst {
				t.Fatalf("burst %c changed", 'A'+vseq)
			}
			continue
		}
		bursts = append(bursts, out.Burst)
		raw := out.Burst
		emb := raw.EMB()
		if emb.FEC.Uncorrectable || emb.LCSS != positionLCSS(int(vseq)) || emb.ColorCode != 1 || emb.PreemptionAndPowerControlIndicator { //nolint:gosec // vseq is in [1,4]
			t.Fatalf("burst %c EMB %s", 'A'+vseq, emb.String())
		}
		if !sameBits(v.Burst, out.Burst, 0, 108) || !sameBits(v.Burst, out.Burst, 156, 264) {
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
	tp.Destination, tp.Private = 9, false
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
		v := voicePacket(vseq, positionLCSS(int(vseq))) //nolint:gosec // vseq is in [1,4]
		out := v
		out.Destination, out.Private = 9, false
		r.Rewrite(v, &out)
		bursts = append(bursts, out.Burst)
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
	out.Destination, out.Private = 9, false
	r.Rewrite(hdr, &out)
	if out.Burst == hdr.Burst {
		t.Fatal("the voice header was not rewritten")
	}

	dt := enums.DataTypeVoiceLCHeader
	var b layer2.Burst
	if err := b.DecodeFromBytesWith(out.Burst, &layer2.DecodeOptions{Kind: layer2.KindData, DataType: &dt}); err != nil {
		t.Fatal(err)
	}
	flc, ok := b.Data.(*pdu.FullLinkControl)
	if !ok || flc.FEC.Uncorrectable || flc.FEC.ErrorsCorrected != 0 {
		t.Fatalf("voice header decodes to %T with FEC %+v", b.Data, b.FEC)
	}
	if target, source, ok := flc.Addresses(); !ok || flc.FLCO != enums.FLCOGroupVoiceChannelUser || target != 9 || source != 1234 {
		t.Fatalf("voice header LC %s, want a group call from 1234 to 9", flc.String())
	}
	if again, err := b.Encode(); err != nil || again != out.Burst {
		t.Fatalf("voice header does not encode back to the rewritten bytes: %v", err)
	}

	var fragments [4][4]byte
	for vseq := uint(1); vseq <= 4; vseq++ {
		v := voicePacket(vseq, positionLCSS(int(vseq))) //nolint:gosec // vseq is in [1,4]
		o := v
		o.Destination, o.Private = 9, false
		r.Rewrite(v, &o)
		if o.Burst == v.Burst {
			t.Fatalf("burst %c was not rewritten", 'A'+vseq)
		}
		var vb layer2.Burst
		opts := layer2.DecodeOptions{Kind: layer2.KindVoice, VoiceBurst: voiceBurst(int(vseq))} //nolint:gosec // vseq is in [1,4]
		if err := vb.DecodeFromBytesWith(o.Burst, &opts); err != nil {
			t.Fatal(err)
		}
		if again, err := vb.Encode(); err != nil || again != o.Burst {
			t.Fatalf("burst %c does not encode back to the rewritten bytes: %v", 'A'+vseq, err)
		}
		fragments[vseq-1] = o.Burst.EmbeddedData()
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
	out.Destination = 9
	r.Rewrite(v, &out)
	if len(r.streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(r.streams))
	}
	clock.t = clock.t.Add(streamIdleTimeout + pruneInterval + time.Second)
	v.Stream = 8
	out = v
	out.Destination = 9
	r.Rewrite(v, &out)
	if _, ok := r.streams[7]; ok || len(r.streams) != 1 {
		t.Fatalf("idle stream not pruned: %d streams", len(r.streams))
	}
}

// A stream left without a terminator is pruned even when no later packet
// needs a rewrite.
func TestLCRewriterPrunesWithoutRewrites(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	clock := &fakeClock{t: time.Unix(1000, 0)}
	r.now = clock.now
	v := voicePacket(1, enums.LCSSFirstFragment)
	out := v
	out.Destination = 9
	r.Rewrite(v, &out)
	clock.t = clock.t.Add(streamIdleTimeout + pruneInterval + time.Second)
	v.Stream = 8
	out = v
	r.Rewrite(v, &out)
	r.mu.Lock()
	n := len(r.streams)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("idle stream not pruned: %d streams", n)
	}
}

// TestTranslateToIPSCFullLCFollowsPacketFields checks that the translator
// takes the call type, destination, and source of the IPSC full LC from the
// packet fields, which a rewrite rule may have changed, and keeps the
// feature set ID and service options of the decoded LC.
func TestTranslateToIPSCFullLCFollowsPacketFields(t *testing.T) {
	t.Parallel()
	priv := [9]byte{0x03, 0x10, 0xC0, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	group := [9]byte{0x00, 0x00, 0xC0, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}
	tr := newTestTranslator(t)

	hdr := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 9, true, burstWithLC(t, priv, enums.DataTypeVoiceLCHeader, 1))
	out := tr.TranslateToIPSC(hdr)
	want := testFullLC(t, group, enums.DataTypeVoiceLCHeader)
	if len(out) != 3 || string(out[0][38:50]) != string(want[:]) {
		t.Fatalf("header LC % X, want % X", out[0][38:50], want)
	}
	for vseq := uint(0); vseq <= 4; vseq++ {
		v := voicePacket(vseq, positionLCSS(int(vseq))) //nolint:gosec // vseq is in [0,4]
		v.Source, v.Destination, v.Private = 1234, 9, false
		out = tr.TranslateToIPSC(v)
	}
	if len(out) != 1 || len(out[0]) != 66 || string(out[0][56:59]) != string(group[0:3]) {
		t.Fatalf("burst E LC prefix % X, want % X", out[0][56:59], group[0:3])
	}
	term := lcPacket(enums.DataTypeTerminatorWithLC, 1234, 9, true, burstWithLC(t, priv, enums.DataTypeTerminatorWithLC, 1))
	out = tr.TranslateToIPSC(term)
	want = testFullLC(t, group, enums.DataTypeTerminatorWithLC)
	if len(out) != 1 || string(out[0][38:50]) != string(want[:]) {
		t.Fatalf("terminator LC % X, want % X", out[0][38:50], want)
	}
}

// withCRC returns p with the CRC that MMDVMHost checks in a burst of dt.
func withCRC(p [12]byte, dt enums.DataType) [12]byte {
	_ = layer2.MMDVMHostSeal(dt, &p)
	return p
}

func rewriteBlock(t *testing.T, dataType enums.DataType, in [12]byte, src, dst uint32) [12]byte {
	t.Helper()
	r := NewLCRewriter()
	orig := lcPacket(dataType, src, dst, false, buildLCDataBurst(t, in, dataType, 1))
	pkt := orig
	pkt.Destination, pkt.Private = 9, false
	r.Rewrite(orig, &pkt)
	got, _ := decodeBPTCPayload(pkt.Burst, dataType)
	if !sameBits(orig.Burst, pkt.Burst, 98, 166) {
		t.Fatalf("%s: slot type or sync changed", dataType.String())
	}
	return got
}

// TestLCRewriterRewritesCSBK checks a real preamble CSBK from a private call
// (CSBKO 0x3D, dst 0x233863, src 0x23383B) rewritten into a group call to 9,
// as DMRGateway's processCSBK and CDMRCSBK do, a NACK response, and a
// P_PROTECT and a C_AHOY whose G/I bit follows the new call type. A C_ALOHA,
// which carries no target and source pair, is left alone.
func TestLCRewriterRewritesCSBK(t *testing.T) {
	t.Parallel()
	in := [12]byte{0xbd, 0x00, 0x80, 0x1d, 0x23, 0x38, 0x63, 0x23, 0x38, 0x3b, 0x58, 0x89}
	want := withCRC([12]byte{0xbd, 0x00, 0xC0, 0x1d, 0x00, 0x00, 0x09, 0x23, 0x38, 0x3b}, enums.DataTypeCSBK)
	if got := rewriteBlock(t, enums.DataTypeCSBK, in, 0x23383B, 0x233863); got != want {
		t.Fatalf("CSBK % X, want % X", got, want)
	}

	nack := withCRC([12]byte{0x26, 0x00, 0x00, 0x00, 0x00, 0x04, 0xD2, 0x00, 0x15, 0xB3}, enums.DataTypeCSBK)
	want = withCRC([12]byte{0x26, 0x00, 0x00, 0x00, 0x00, 0x04, 0xD2, 0x00, 0x00, 0x09}, enums.DataTypeCSBK)
	if got := rewriteBlock(t, enums.DataTypeCSBK, nack, 1234, 5555); got != want {
		t.Fatalf("NACK CSBK % X, want % X", got, want)
	}

	protect := withCRC([12]byte{0x2F, 0x00, 0x00, 0x00, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}, enums.DataTypeCSBK)
	want = withCRC([12]byte{0x2F, 0x00, 0x00, 0x01, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}, enums.DataTypeCSBK)
	if got := rewriteBlock(t, enums.DataTypeCSBK, protect, 1234, 5555); got != want {
		t.Fatalf("P_PROTECT CSBK % X, want % X", got, want)
	}

	ahoy := withCRC([12]byte{0x1C, 0x00, 0x00, 0x00, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}, enums.DataTypeCSBK)
	want = withCRC([12]byte{0x1C, 0x00, 0x00, 0x40, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}, enums.DataTypeCSBK)
	if got := rewriteBlock(t, enums.DataTypeCSBK, ahoy, 1234, 5555); got != want {
		t.Fatalf("C_AHOY CSBK % X, want % X", got, want)
	}

	bad := in
	bad[11] ^= 0x01
	if got := rewriteBlock(t, enums.DataTypeCSBK, bad, 0x23383B, 0x233863); got != bad {
		t.Fatalf("CSBK with a bad CRC changed: % X", got)
	}

	aloha := withCRC([12]byte{0x19, 0x00, 0x00, 0x00, 0x00, 0x04, 0xD2, 0x00, 0x15, 0xB3}, enums.DataTypeCSBK)
	if got := rewriteBlock(t, enums.DataTypeCSBK, aloha, 1234, 5555); got != aloha {
		t.Fatalf("C_ALOHA CSBK changed: % X", got)
	}
}

// TestLCRewriterRewritesDataHeader checks an unconfirmed data header
// rewritten into a group call to 9, as DMRGateway's processDataHeader and
// CDMRDataHeader do. Proprietary and reserved format headers are left
// alone, and a response header gets the addresses but no group flag.
func TestLCRewriterRewritesDataHeader(t *testing.T) {
	t.Parallel()
	in := withCRC([12]byte{0x02, 0x00, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2, 0x81, 0x00}, enums.DataTypeDataHeader)
	want := withCRC([12]byte{0x82, 0x00, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2, 0x81, 0x00}, enums.DataTypeDataHeader)
	if got := rewriteBlock(t, enums.DataTypeDataHeader, in, 1234, 5555); got != want {
		t.Fatalf("data header % X, want % X", got, want)
	}

	prop := withCRC([12]byte{0x0F, 0x00, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2, 0x81, 0x00}, enums.DataTypeDataHeader)
	if got := rewriteBlock(t, enums.DataTypeDataHeader, prop, 1234, 5555); got != prop {
		t.Fatalf("proprietary data header changed: % X", got)
	}

	reserved := withCRC([12]byte{0x04, 0x00, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2, 0x81, 0x00}, enums.DataTypeDataHeader)
	if got := rewriteBlock(t, enums.DataTypeDataHeader, reserved, 1234, 5555); got != reserved {
		t.Fatalf("reserved format data header changed: % X", got)
	}

	rsp := withCRC([12]byte{0x01, 0x00, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2, 0x10, 0x00}, enums.DataTypeDataHeader)
	want = withCRC([12]byte{0x01, 0x00, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2, 0x10, 0x00}, enums.DataTypeDataHeader)
	if got := rewriteBlock(t, enums.DataTypeDataHeader, rsp, 1234, 5555); got != want {
		t.Fatalf("response header % X, want % X", got, want)
	}
}

// voiceWithEmbeddedLC returns voice burst vseq (1-5) of a superframe whose
// bursts B-E carry fragments as their embedded LC.
func voiceWithEmbeddedLC(vseq uint, fragments [4][4]byte) dmrd.Data {
	pos := int(vseq) //nolint:gosec // vseq is in [1,5]
	p := voicePacket(vseq, positionLCSS(pos))
	if pos > burstE {
		return p
	}
	raw := p.Burst
	raw.SetEmbeddedData(fragments[pos-burstB])
	p.Burst = raw
	return p
}

// TestLCRewriterPassesNonVoiceEmbeddedLC checks that, as in DMRGateway's
// CRewrite::processEmbeddedData, only voice embedded LCs are replaced: a
// talker alias passes through one superframe late, and the first superframe
// always carries the new LC.
func TestLCRewriterPassesNonVoiceEmbeddedLC(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	priv := [9]byte{0x03, 0x00, 0x80, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	newLC := [9]byte{0x00, 0x00, 0x80, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}
	alias1 := [9]byte{0x04, 0x00, 0x44, 'N', '0', 'C', 'A', 'L', 'L'}
	alias2 := [9]byte{0x05, 0x00, 'S', 'P', 'R', 'I', 'N', 'G', 'F'}

	hdr := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, burstWithLC(t, priv, enums.DataTypeVoiceLCHeader, 1))
	out := hdr
	out.Destination, out.Private = 9, false
	r.Rewrite(hdr, &out)

	in := [][9]byte{alias1, alias2, priv, priv}
	want := [][9]byte{newLC, alias1, alias2, newLC}
	for sf := range in {
		raw := layer2.EncodeEmbeddedLC(in[sf])
		var bursts [][33]byte
		for vseq := uint(0); vseq <= 5; vseq++ {
			v := voicePacket(0, enums.LCSSSingleFragment)
			if vseq > 0 {
				v = voiceWithEmbeddedLC(vseq, raw)
			}
			o := v
			o.Destination, o.Private = 9, false
			r.Rewrite(v, &o)
			if vseq == 0 || vseq == 5 {
				if o.Burst != v.Burst {
					t.Fatalf("superframe %d: burst %c changed", sf, 'A'+vseq)
				}
				continue
			}
			bursts = append(bursts, o.Burst)
		}
		if lc := embeddedLCOf(t, fragmentsOf(t, bursts)); lc != want[sf] {
			t.Fatalf("superframe %d: embedded LC % X, want % X", sf, lc, want[sf])
		}
	}
}

// TestLCRewriterDecodesEmbeddedLCLikeMMDVMHost checks that a received
// embedded LC is decoded as MMDVMHost decodes it: one bit error in a row is
// corrected, so the talker alias passes through, and two bit errors in a row
// make it undecodable, so it is replaced by the rewritten LC.
func TestLCRewriterDecodesEmbeddedLCLikeMMDVMHost(t *testing.T) {
	t.Parallel()
	priv := [9]byte{0x03, 0x00, 0x80, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	newLC := [9]byte{0x00, 0x00, 0x80, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}
	alias := [9]byte{0x04, 0x00, 0x44, 'N', '0', 'C', 'A', 'L', 'L'}
	one := layer2.EncodeEmbeddedLC(alias)
	one[0][0] ^= 0x04 // fragment bit 5: row 5, column 0
	two := one
	two[0][1] ^= 0x04 // fragment bit 13: row 5, column 1

	for _, tc := range []struct {
		name string
		in   [4][4]byte
		want [9]byte
	}{
		{"one error in a row", one, alias},
		{"two errors in a row", two, newLC},
	} {
		r := NewLCRewriter()
		hdr := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, burstWithLC(t, priv, enums.DataTypeVoiceLCHeader, 1))
		out := hdr
		out.Destination, out.Private = 9, false
		r.Rewrite(hdr, &out)

		var bursts [][33]byte
		for sf, in := range [][4][4]byte{tc.in, layer2.EncodeEmbeddedLC(priv)} {
			for vseq := uint(0); vseq <= 5; vseq++ {
				v := voicePacket(0, enums.LCSSSingleFragment)
				if vseq > 0 {
					v = voiceWithEmbeddedLC(vseq, in)
				}
				o := v
				o.Destination, o.Private = 9, false
				r.Rewrite(v, &o)
				if sf == 1 && vseq >= 1 && vseq <= 4 {
					bursts = append(bursts, o.Burst)
				}
			}
		}
		if lc := embeddedLCOf(t, fragmentsOf(t, bursts)); lc != tc.want {
			t.Fatalf("%s: embedded LC % X, want % X", tc.name, lc, tc.want)
		}
	}
}

// withUncorrectableEMB flips bits of the EMB of a voice burst until it can
// no longer be decoded.
func withUncorrectableEMB(t *testing.T, v dmrd.Data) dmrd.Data {
	t.Helper()
	for mask := 1; mask < 1<<16; mask++ {
		d := v.Burst
		for i := range 16 {
			if mask>>i&1 == 0 {
				continue
			}
			bit := 108 + i
			if i >= 8 {
				bit = 148 + i - 8
			}
			d[bit/8] ^= 1 << (7 - bit%8)
		}
		if res := d.EMB().FEC; res.Uncorrectable {
			v.Burst = d
			return v
		}
	}
	t.Fatal("no uncorrectable EMB found")
	return v
}

// A rewritten burst gets an EMB with the LCSS for its position, the color
// code from the slot type of the call's voice header, and PI clear, even
// when its own EMB is uncorrectable. A late entry with no voice header
// keeps the color code of a correctable EMB.
func TestLCRewriterRebuildsEMB(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		header  bool
		corrupt bool
		wantCC  enums.ColorCode
	}{
		{"header and uncorrectable EMB", true, true, 1},
		{"header and correctable EMB", true, false, 1},
		{"late entry", false, false, 3},
	} {
		r := NewLCRewriter()
		if tc.header {
			lc := [9]byte{0x00, 0x00, 0x00, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
			h := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, true, burstWithLC(t, lc, enums.DataTypeVoiceLCHeader, 1))
			out := h
			out.Destination = 9
			r.Rewrite(h, &out)
		}
		for vseq := uint(1); vseq <= 4; vseq++ {
			v := voicePacket(vseq, enums.LCSSSingleFragment)
			if tc.corrupt {
				v = withUncorrectableEMB(t, v)
			}
			out := v
			out.Destination = 9
			r.Rewrite(v, &out)
			emb := out.Burst.EMB()
			if emb.FEC.Uncorrectable || emb.LCSS != positionLCSS(int(vseq)) || emb.ColorCode != tc.wantCC || emb.PreemptionAndPowerControlIndicator { //nolint:gosec // vseq is in [1,4]
				t.Errorf("%s: burst %c EMB %s", tc.name, 'A'+vseq, emb.String())
			}
		}
	}
}

// TestLCRewriterNeverMixesSuperframes sends a call whose superframes all
// carry the same talker alias, with bursts lost: E of the first superframe,
// and B to D of the third. The alias fragments B-D of the first superframe
// and E of the third would decode to the alias, but they are from two
// superframes, so the third superframe has no embedded LC and the fourth
// carries the rewritten LC.
func TestLCRewriterNeverMixesSuperframes(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	priv := [9]byte{0x03, 0x00, 0x80, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	newLC := [9]byte{0x00, 0x00, 0x80, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}
	alias := layer2.EncodeEmbeddedLC([9]byte{0x04, 0x00, 0x44, 'N', '0', 'C', 'A', 'L', 'L'})

	hdr := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, burstWithLC(t, priv, enums.DataTypeVoiceLCHeader, 1))
	out := hdr
	out.Destination, out.Private = 9, false
	r.Rewrite(hdr, &out)

	var bursts [][33]byte
	for sf, vseqs := range [][]uint{{0, 1, 2, 3, 5}, {0, 1, 2, 3, 4, 5}, {0, 4, 5}, {0, 1, 2, 3, 4, 5}} {
		for _, vseq := range vseqs {
			v := voicePacket(0, enums.LCSSSingleFragment)
			if vseq > 0 {
				v = voiceWithEmbeddedLC(vseq, alias)
			}
			o := v
			o.Destination, o.Private = 9, false
			r.Rewrite(v, &o)
			if sf == 3 && vseq >= 1 && vseq <= 4 {
				bursts = append(bursts, o.Burst)
			}
		}
	}
	if lc := embeddedLCOf(t, fragmentsOf(t, bursts)); lc != newLC {
		t.Fatalf("embedded LC % X, want % X", lc, newLC)
	}
}

// TestEmbeddedFragmentsChecks adds embedded fragments of bursts B-E in
// sequences that each break one rule of embeddedFragments.add. Only the
// unbroken sequences complete an LC at their last burst.
func TestEmbeddedFragmentsChecks(t *testing.T) {
	t.Parallel()
	type burst struct {
		pos, lastPos int
		lcss         enums.LCSS
		embOK        bool
	}
	ok := func(pos, lastPos int) burst {
		return burst{pos, lastPos, positionLCSS(pos), true}
	}
	badEMB := ok(burstC, burstB)
	badEMB.embOK = false
	badLCSS := ok(burstC, burstB)
	badLCSS.lcss = enums.LCSSFirstFragment
	tests := []struct {
		name   string
		bursts []burst
		want   bool
	}{
		{"complete", []burst{ok(burstB, burstA), ok(burstC, burstB), ok(burstD, burstC), ok(burstE, burstD)}, true},
		{"EMB does not decode", []burst{ok(burstB, burstA), badEMB, ok(burstD, burstC), ok(burstE, burstD)}, false},
		{"wrong LCSS", []burst{ok(burstB, burstA), badLCSS, ok(burstD, burstC), ok(burstE, burstD)}, false},
		{"burst F between D and E", []burst{ok(burstB, burstA), ok(burstC, burstB), ok(burstD, burstC), ok(burstE, burstF)}, false},
		{"B starts again", []burst{ok(burstB, burstA), ok(burstC, burstB), ok(burstB, burstA), ok(burstC, burstB), ok(burstD, burstC), ok(burstE, burstD)}, true},
	}
	for _, tc := range tests {
		var f embeddedFragments
		var got bool
		for i, b := range tc.bursts {
			got = f.add([4]byte{byte(i)}, pdu.EmbeddedSignalling{LCSS: b.lcss}, b.embOK, b.pos, b.lastPos)
		}
		if got != tc.want {
			t.Errorf("%s: complete %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLCRewriterForgetsOldSuperframes sends a call whose superframes carry a
// talker alias, with burst E of the third lost. The third superframe's
// buffer still holds the alias of the first, two superframes earlier, so
// the fourth superframe must not take it for the third's: it carries the
// rewritten LC.
func TestLCRewriterForgetsOldSuperframes(t *testing.T) {
	t.Parallel()
	r := NewLCRewriter()
	priv := [9]byte{0x03, 0x00, 0x80, 0x00, 0x15, 0xB3, 0x00, 0x04, 0xD2}
	newLC := [9]byte{0x00, 0x00, 0x80, 0x00, 0x00, 0x09, 0x00, 0x04, 0xD2}
	alias := layer2.EncodeEmbeddedLC([9]byte{0x04, 0x00, 0x44, 'N', '0', 'C', 'A', 'L', 'L'})

	hdr := lcPacket(enums.DataTypeVoiceLCHeader, 1234, 5555, false, burstWithLC(t, priv, enums.DataTypeVoiceLCHeader, 1))
	out := hdr
	out.Destination, out.Private = 9, false
	r.Rewrite(hdr, &out)

	var bursts [][33]byte
	for sf, vseqs := range [][]uint{{0, 1, 2, 3, 4, 5}, {0, 1, 2, 3, 4, 5}, {0, 1, 2, 3, 5}, {0, 1, 2, 3, 4, 5}} {
		for _, vseq := range vseqs {
			v := voicePacket(0, enums.LCSSSingleFragment)
			if vseq > 0 {
				v = voiceWithEmbeddedLC(vseq, alias)
			}
			o := v
			o.Destination, o.Private = 9, false
			r.Rewrite(v, &o)
			if sf == 3 && vseq >= 1 && vseq <= 4 {
				bursts = append(bursts, o.Burst)
			}
		}
	}
	if lc := embeddedLCOf(t, fragmentsOf(t, bursts)); lc != newLC {
		t.Fatalf("embedded LC % X, want % X", lc, newLC)
	}
}
