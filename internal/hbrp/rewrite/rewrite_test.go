package rewrite

import (
	"testing"

	"github.com/USA-RedDragon/ipsc2hbrp/internal/hbrp/proto"
)

// helper to create a basic group-call packet
func groupPkt(slot uint, dst uint) *proto.Packet {
	return &proto.Packet{
		Signature: "DMRD",
		Slot:      slot == 2,
		GroupCall: true,
		Dst:       dst,
		Src:       1234,
	}
}

// helper to create a basic private-call packet
func privatePkt(slot uint, dst uint, src uint) *proto.Packet {
	return &proto.Packet{
		Signature: "DMRD",
		Slot:      slot == 2,
		GroupCall: false,
		Dst:       dst,
		Src:       src,
	}
}

// ── TGRewrite ────────────────────────────────────────────────────────────────

func TestTGRewrite_Match_Single(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: "test", FromSlot: 1, FromTG: 9, ToSlot: 2, ToTG: 100, Range: 1}
	pkt := groupPkt(1, 9)

	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	if pkt.Dst != 100 {
		t.Fatalf("expected Dst=100, got %d", pkt.Dst)
	}
	if pktSlot(pkt) != 2 {
		t.Fatalf("expected slot 2, got %d", pktSlot(pkt))
	}
}

func TestTGRewrite_Match_Range(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: "test", FromSlot: 1, FromTG: 100, ToSlot: 1, ToTG: 200, Range: 10}

	pkt := groupPkt(1, 105)
	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	// 105 + 200 - 100 = 205
	if pkt.Dst != 205 {
		t.Fatalf("expected Dst=205, got %d", pkt.Dst)
	}
}

func TestTGRewrite_NoMatch_WrongSlot(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: "test", FromSlot: 1, FromTG: 9, ToSlot: 2, ToTG: 100, Range: 1}
	pkt := groupPkt(2, 9) // wrong slot

	res := r.Process(pkt, false)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestTGRewrite_NoMatch_PrivateCall(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: "test", FromSlot: 1, FromTG: 9, ToSlot: 2, ToTG: 100, Range: 1}
	pkt := privatePkt(1, 9, 1234) // private call

	res := r.Process(pkt, false)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestTGRewrite_NoMatch_OutOfRange(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: "test", FromSlot: 1, FromTG: 100, ToSlot: 1, ToTG: 200, Range: 5}
	pkt := groupPkt(1, 106) // just outside range (100-104)

	res := r.Process(pkt, false)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestTGRewrite_SameSlot_SameTG(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: "test", FromSlot: 1, FromTG: 9, ToSlot: 1, ToTG: 9, Range: 1}
	pkt := groupPkt(1, 9)

	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched (passthrough)")
	}
	if pkt.Dst != 9 {
		t.Fatalf("expected Dst=9 (unchanged), got %d", pkt.Dst)
	}
}

func TestTGRewrite_Trace(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: "traced", FromSlot: 1, FromTG: 9, ToSlot: 2, ToTG: 100, Range: 1}
	pkt := groupPkt(1, 9)
	// Should not panic with trace enabled
	res := r.Process(pkt, true)
	if res != Matched {
		t.Fatal("expected Matched")
	}
}

// ── PCRewrite ────────────────────────────────────────────────────────────────

func TestPCRewrite_Match_Single(t *testing.T) {
	t.Parallel()
	r := &PCRewrite{Name: "test", FromSlot: 1, FromID: 100, ToSlot: 2, ToID: 200, Range: 1}
	pkt := privatePkt(1, 100, 1234)

	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	if pkt.Dst != 200 {
		t.Fatalf("expected Dst=200, got %d", pkt.Dst)
	}
	if pktSlot(pkt) != 2 {
		t.Fatalf("expected slot 2, got %d", pktSlot(pkt))
	}
}

func TestPCRewrite_Match_Range(t *testing.T) {
	t.Parallel()
	r := &PCRewrite{Name: "test", FromSlot: 1, FromID: 1000, ToSlot: 1, ToID: 2000, Range: 100}
	pkt := privatePkt(1, 1050, 5678)

	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	// 1050 + 2000 - 1000 = 2050
	if pkt.Dst != 2050 {
		t.Fatalf("expected Dst=2050, got %d", pkt.Dst)
	}
}

func TestPCRewrite_NoMatch_GroupCall(t *testing.T) {
	t.Parallel()
	r := &PCRewrite{Name: "test", FromSlot: 1, FromID: 100, ToSlot: 2, ToID: 200, Range: 1}
	pkt := groupPkt(1, 100) // group call won't match PC

	res := r.Process(pkt, false)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestPCRewrite_NoMatch_WrongSlot(t *testing.T) {
	t.Parallel()
	r := &PCRewrite{Name: "test", FromSlot: 1, FromID: 100, ToSlot: 2, ToID: 200, Range: 1}
	pkt := privatePkt(2, 100, 1234) // wrong slot

	res := r.Process(pkt, false)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

// ── TypeRewrite ──────────────────────────────────────────────────────────────

func TestTypeRewrite_Match(t *testing.T) {
	t.Parallel()
	r := &TypeRewrite{Name: "test", FromSlot: 1, FromTG: 9, ToSlot: 2, ToID: 3100, Range: 1}
	pkt := groupPkt(1, 9)

	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	if pkt.Dst != 3100 {
		t.Fatalf("expected Dst=3100, got %d", pkt.Dst)
	}
	if pkt.GroupCall {
		t.Fatal("expected GroupCall=false after TypeRewrite")
	}
	if pktSlot(pkt) != 2 {
		t.Fatalf("expected slot 2, got %d", pktSlot(pkt))
	}
}

func TestTypeRewrite_Match_Range(t *testing.T) {
	t.Parallel()
	r := &TypeRewrite{Name: "test", FromSlot: 1, FromTG: 100, ToSlot: 1, ToID: 5000, Range: 10}
	pkt := groupPkt(1, 107)

	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	// 107 + 5000 - 100 = 5007
	if pkt.Dst != 5007 {
		t.Fatalf("expected Dst=5007, got %d", pkt.Dst)
	}
	if pkt.GroupCall {
		t.Fatal("expected GroupCall=false")
	}
}

func TestTypeRewrite_NoMatch_PrivateCall(t *testing.T) {
	t.Parallel()
	r := &TypeRewrite{Name: "test", FromSlot: 1, FromTG: 9, ToSlot: 2, ToID: 3100, Range: 1}
	pkt := privatePkt(1, 9, 1234) // private call

	res := r.Process(pkt, false)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

// ── SrcRewrite ───────────────────────────────────────────────────────────────

func TestSrcRewrite_Match(t *testing.T) {
	t.Parallel()
	r := &SrcRewrite{Name: "test", FromSlot: 1, FromID: 1234, ToSlot: 2, ToTG: 9, Range: 1}
	pkt := privatePkt(1, 999, 1234) // private call from source 1234

	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	if pkt.Dst != 9 {
		t.Fatalf("expected Dst=9, got %d", pkt.Dst)
	}
	if !pkt.GroupCall {
		t.Fatal("expected GroupCall=true after SrcRewrite")
	}
	if pktSlot(pkt) != 2 {
		t.Fatalf("expected slot 2, got %d", pktSlot(pkt))
	}
}

func TestSrcRewrite_Match_Range(t *testing.T) {
	t.Parallel()
	r := &SrcRewrite{Name: "test", FromSlot: 1, FromID: 1000, ToSlot: 1, ToTG: 9, Range: 100}
	pkt := privatePkt(1, 999, 1050) // source in range

	res := r.Process(pkt, false)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	if !pkt.GroupCall {
		t.Fatal("expected GroupCall=true")
	}
	if pkt.Dst != 9 {
		t.Fatalf("expected Dst=9, got %d", pkt.Dst)
	}
}

func TestSrcRewrite_NoMatch_GroupCall(t *testing.T) {
	t.Parallel()
	r := &SrcRewrite{Name: "test", FromSlot: 1, FromID: 1234, ToSlot: 2, ToTG: 9, Range: 1}
	pkt := groupPkt(1, 9) // group call won't match

	res := r.Process(pkt, false)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestSrcRewrite_NoMatch_WrongSource(t *testing.T) {
	t.Parallel()
	r := &SrcRewrite{Name: "test", FromSlot: 1, FromID: 1234, ToSlot: 2, ToTG: 9, Range: 1}
	pkt := privatePkt(1, 999, 5678) // wrong source

	res := r.Process(pkt, false)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

// ── Apply ────────────────────────────────────────────────────────────────────

func TestApply_FirstMatchWins(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		&TGRewrite{Name: "rule1", FromSlot: 1, FromTG: 9, ToSlot: 1, ToTG: 100, Range: 1},
		&TGRewrite{Name: "rule2", FromSlot: 1, FromTG: 9, ToSlot: 1, ToTG: 200, Range: 1},
	}
	pkt := groupPkt(1, 9)

	matched := Apply(rules, pkt, false)
	if !matched {
		t.Fatal("expected a match")
	}
	// First rule should have set Dst=100
	if pkt.Dst != 100 {
		t.Fatalf("expected Dst=100 (first rule), got %d", pkt.Dst)
	}
}

func TestApply_NoMatch(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		&TGRewrite{Name: "rule1", FromSlot: 2, FromTG: 9, ToSlot: 1, ToTG: 100, Range: 1},
	}
	pkt := groupPkt(1, 9) // slot 1, rule expects slot 2

	matched := Apply(rules, pkt, false)
	if matched {
		t.Fatal("expected no match")
	}
}

func TestApply_Empty(t *testing.T) {
	t.Parallel()
	pkt := groupPkt(1, 9)
	matched := Apply(nil, pkt, false)
	if matched {
		t.Fatal("expected no match on empty rules")
	}
}

func TestApply_MixedRuleTypes(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		&TGRewrite{Name: "tg", FromSlot: 1, FromTG: 100, ToSlot: 1, ToTG: 200, Range: 1},
		&PCRewrite{Name: "pc", FromSlot: 1, FromID: 50, ToSlot: 1, ToID: 60, Range: 1},
	}

	// This should match the PC rule (private call to ID 50)
	pkt := privatePkt(1, 50, 1234)
	matched := Apply(rules, pkt, false)
	if !matched {
		t.Fatal("expected match on PCRewrite")
	}
	if pkt.Dst != 60 {
		t.Fatalf("expected Dst=60, got %d", pkt.Dst)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func TestPktSlot(t *testing.T) {
	t.Parallel()
	pkt := &proto.Packet{Slot: false}
	if pktSlot(pkt) != 1 {
		t.Fatalf("expected slot 1, got %d", pktSlot(pkt))
	}
	pkt.Slot = true
	if pktSlot(pkt) != 2 {
		t.Fatalf("expected slot 2, got %d", pktSlot(pkt))
	}
}

func TestSetPktSlot(t *testing.T) {
	t.Parallel()
	pkt := &proto.Packet{}
	setPktSlot(pkt, 2)
	if !pkt.Slot {
		t.Fatal("expected Slot=true for slot 2")
	}
	setPktSlot(pkt, 1)
	if pkt.Slot {
		t.Fatal("expected Slot=false for slot 1")
	}
}
