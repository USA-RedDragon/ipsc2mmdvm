package mmdvm

import (
	"encoding/hex"
	"net"
	"slices"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/rewrite"
	"github.com/USA-RedDragon/ipsc2mmdvm/internal/timeslot"
)

// xprDDHeadCall is a Defined Short Data message from 3191868 to 3140598 on
// TS2 as an XPR 8400 sent it over IPSC (dmrgo-rfcap ipsc_master.log,
// 2026-10-16 00:23:47, and runs/sms-hyt): five preamble CSBKs, a copy of
// the last one in place of the DD_HEAD data header, and the two rate 1/2
// blocks.
func xprDDHeadCall() []string {
	return []string{
		"840004ce2e6730b43c2febf601000039012080ddf42161b9f6000000000003c0000a800a0060bd0080072febf630b43c0094007339ea",
		"840004ce2e6730b43c2febf6010000390120805df42261b9f7e00000000003c0000a800a0060bd0080062febf630b43cb8f500733a0b",
		"840004ce2e6730b43c2febf6010000390120805df42361b9f9c00000000003c0000a800a0060bd0080052febf630b43c607700733a26",
		"840004ce2e6730b43c2febf6010000390120805df42461b9fba00000000003c0000a800a0060bd0080042febf630b43cd81600733a23",
		"840004ce2e6730b43c2febf6010000390120805df42561b9fd800000000003c0000a800a0060bd0080032febf630b43cc152007339e6",
		"840004ce2e6730b43c2febf6010000390120805df42661b9ff600000000003c0000a800a0060bd0080032febf630b43cc152007339e6",
		"840004ce2e6730b43c2febf6010000390120805df42761ba01400000000007c0000a800a0060450048005900540020005400007739f2",
		"840004ce2e6730b43c2febf6010000390160805ef42861ba03200000000007c0000a800a0060450053005400000070532851007739ee",
	}
}

// TestXPRDDHeadCall replays xprDDHeadCall. The six preamble CSBKs and the
// two rate 1/2 blocks, which have no data header before them, go to the
// master, and the end flag of the last block ends the call and releases
// TS2.
func TestXPRDDHeadCall(t *testing.T) {
	t.Parallel()
	c := newInboundTestClient(t)
	t.Cleanup(c.inboundTSMgr.Stop)
	c.rfRewrites = []rewrite.Rule{&rewrite.PassAllPC{Name: testRuleName, Slot: 2}}
	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 0, 231), Port: 50000}
	var last []byte
	for _, h := range xprDDHeadCall() {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		c.HandleIPSCBurst(b[0], b, addr)
		last = b
	}
	var types []uint8
	for len(c.tx_chan) > 0 {
		p := <-c.tx_chan
		if p.Slot == enums.Slot1 || p.Source != 3191868 || p.Destination != 3140598 || !p.Private {
			t.Errorf("packet with slot %v src %d dst %d group %t", p.Slot, p.Source, p.Destination, !p.Private)
		}
		types = append(types, p.Value)
	}
	want := append(slices.Repeat([]uint8{uint8(enums.DataTypeCSBK)}, 6), uint8(enums.DataTypeRate12), uint8(enums.DataTypeRate12))
	if !slices.Equal(types, want) {
		t.Fatalf("sent %v, want %v", types, want)
	}
	if _, ok := c.translator.ReverseStreamID(last); ok {
		t.Error("the call is still open in the translator")
	}
	c.inboundMu.Lock()
	open := len(c.inboundSlots)
	c.inboundMu.Unlock()
	if open != 0 {
		t.Errorf("%d inbound calls open", open)
	}
	if v := c.inboundTSMgr.Submit(true, 999, "ipsc", nil, true, nil); v != timeslot.Deliver {
		t.Errorf("TS2 still held: %v", v)
	}
}

func TestOrphanBlockReleasesSlot(t *testing.T) {
	t.Parallel()
	c := newInboundTestClient(t)
	t.Cleanup(c.inboundTSMgr.Stop)
	c.rfRewrites = []rewrite.Rule{&rewrite.PassAllPC{Name: testRuleName, Slot: 1}}
	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 0, 231), Port: 50000}
	for _, h := range []string{
		"840004ce2e2e30b43c30b43c0100006a7c0080ddedea3659efc9000000000640000a800a006043a230b43c30b43c8100d06d00764679",
		"840004ce2e2e30b43c30b43c0100006a7c40805eedec3659f3890000000013000000",
		"840004ce2e2e30b43c30b43c0100006a7c00805deded3659f569000000000840000d800a0090444d52474f20544553540000a3ecda7c002e0078469b",
	} {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		c.HandleIPSCBurst(b[0], b, addr)
	}
	var types []uint8
	for len(c.tx_chan) > 0 {
		types = append(types, (<-c.tx_chan).Value)
	}
	if want := []uint8{uint8(enums.DataTypeDataHeader), uint8(enums.DataTypeRate34)}; !slices.Equal(types, want) {
		t.Fatalf("sent %v, want %v", types, want)
	}
	c.inboundMu.Lock()
	open := len(c.inboundSlots)
	c.inboundMu.Unlock()
	if open != 0 {
		t.Errorf("%d inbound calls open", open)
	}
	if v := c.inboundTSMgr.Submit(false, 999, "ipsc", nil, true, nil); v != timeslot.Deliver {
		t.Errorf("TS1 still held: %v", v)
	}
}
