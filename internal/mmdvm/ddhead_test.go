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

// TestXPRDDHeadCall replays xprDDHeadCall. The six preamble CSBKs go to
// the master. The rate 1/2 blocks have no data header before them, so they
// are dropped, as MMDVMHost drops them, and the end flag of the last one
// still ends the call and releases TS2.
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
	want := slices.Repeat([]uint8{uint8(enums.DataTypeCSBK)}, 6)
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
