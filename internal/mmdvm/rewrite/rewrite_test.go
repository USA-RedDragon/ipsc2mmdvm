package rewrite

import (
	"testing"

	"github.com/USA-RedDragon/ipsc2mmdvm/internal/mmdvm/proto"
)

const testRuleName = "test"

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
	r := &TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 9, ToSlot: 2, ToTG: 100, Range: 1}
	pkt := groupPkt(1, 9)

	res := r.Process(pkt)
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
	r := &TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 100, ToSlot: 1, ToTG: 200, Range: 10}

	pkt := groupPkt(1, 105)
	res := r.Process(pkt)
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
	r := &TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 9, ToSlot: 2, ToTG: 100, Range: 1}
	pkt := groupPkt(2, 9) // wrong slot

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestTGRewrite_NoMatch_PrivateCall(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 9, ToSlot: 2, ToTG: 100, Range: 1}
	pkt := privatePkt(1, 9, 1234) // private call

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestTGRewrite_NoMatch_OutOfRange(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 100, ToSlot: 1, ToTG: 200, Range: 5}
	pkt := groupPkt(1, 106) // just outside range (100-104)

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestTGRewrite_SameSlot_SameTG(t *testing.T) {
	t.Parallel()
	r := &TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 9, ToSlot: 1, ToTG: 9, Range: 1}
	pkt := groupPkt(1, 9)

	res := r.Process(pkt)
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
	res := r.Process(pkt)
	if res != Matched {
		t.Fatal("expected Matched")
	}
}

// ── PCRewrite ────────────────────────────────────────────────────────────────

func TestPCRewrite_Match_Single(t *testing.T) {
	t.Parallel()
	r := &PCRewrite{Name: testRuleName, FromSlot: 1, FromID: 100, ToSlot: 2, ToID: 200, Range: 1}
	pkt := privatePkt(1, 100, 1234)

	res := r.Process(pkt)
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
	r := &PCRewrite{Name: testRuleName, FromSlot: 1, FromID: 1000, ToSlot: 1, ToID: 2000, Range: 100}
	pkt := privatePkt(1, 1050, 5678)

	res := r.Process(pkt)
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
	r := &PCRewrite{Name: testRuleName, FromSlot: 1, FromID: 100, ToSlot: 2, ToID: 200, Range: 1}
	pkt := groupPkt(1, 100) // group call won't match PC

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

func TestPCRewrite_NoMatch_WrongSlot(t *testing.T) {
	t.Parallel()
	r := &PCRewrite{Name: testRuleName, FromSlot: 1, FromID: 100, ToSlot: 2, ToID: 200, Range: 1}
	pkt := privatePkt(2, 100, 1234) // wrong slot

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

// ── TypeRewrite ──────────────────────────────────────────────────────────────

func TestTypeRewrite_Match(t *testing.T) {
	t.Parallel()
	r := &TypeRewrite{Name: testRuleName, FromSlot: 1, FromTG: 9, ToSlot: 2, ToID: 3100, Range: 1}
	pkt := groupPkt(1, 9)

	res := r.Process(pkt)
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
	r := &TypeRewrite{Name: testRuleName, FromSlot: 1, FromTG: 100, ToSlot: 1, ToID: 5000, Range: 10}
	pkt := groupPkt(1, 107)

	res := r.Process(pkt)
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
	r := &TypeRewrite{Name: testRuleName, FromSlot: 1, FromTG: 9, ToSlot: 2, ToID: 3100, Range: 1}
	pkt := privatePkt(1, 9, 1234) // private call

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched")
	}
}

// ── SrcRewrite ───────────────────────────────────────────────────────────────

func TestSrcRewrite_Match(t *testing.T) {
	t.Parallel()
	r := &SrcRewrite{Name: testRuleName, FromSlot: 1, FromID: 1234, ToSlot: 2, ToID: 9, Range: 1}
	pkt := privatePkt(1, 999, 1234) // private call from source 1234

	res := r.Process(pkt)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	if pkt.Src != 9 {
		t.Fatalf("expected Src=9, got %d", pkt.Src)
	}
	if pkt.Dst != 999 {
		t.Fatalf("expected Dst unchanged at 999, got %d", pkt.Dst)
	}
	if pkt.GroupCall {
		t.Fatal("expected GroupCall to remain false (private call)")
	}
	if pktSlot(pkt) != 2 {
		t.Fatalf("expected slot 2, got %d", pktSlot(pkt))
	}
}

func TestSrcRewrite_Match_Range(t *testing.T) {
	t.Parallel()
	r := &SrcRewrite{Name: testRuleName, FromSlot: 1, FromID: 1000, ToSlot: 1, ToID: 5000, Range: 100}
	pkt := privatePkt(1, 999, 1050) // source in range

	res := r.Process(pkt)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	if pkt.GroupCall {
		t.Fatal("expected GroupCall to remain false (private call)")
	}
	if pkt.Src != 5050 {
		t.Fatalf("expected Src=5050, got %d", pkt.Src)
	}
	if pkt.Dst != 999 {
		t.Fatalf("expected Dst unchanged at 999, got %d", pkt.Dst)
	}
}

func TestSrcRewrite_Match_GroupCall(t *testing.T) {
	t.Parallel()
	r := &SrcRewrite{Name: testRuleName, FromSlot: 1, FromID: 100, ToSlot: 2, ToID: 9, Range: 1}
	pkt := groupPkt(1, 9)
	pkt.Src = 100 // source matches

	res := r.Process(pkt)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	if !pkt.GroupCall {
		t.Fatal("expected GroupCall to remain true")
	}
	if pkt.Src != 9 {
		t.Fatalf("expected Src=9, got %d", pkt.Src)
	}
}

func TestSrcRewrite_NoMatch_WrongSource(t *testing.T) {
	t.Parallel()
	r := &SrcRewrite{Name: testRuleName, FromSlot: 1, FromID: 1234, ToSlot: 2, ToID: 9, Range: 1}
	pkt := privatePkt(1, 999, 5678) // wrong source

	res := r.Process(pkt)
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

	matched := Apply(rules, pkt)
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

	matched := Apply(rules, pkt)
	if matched {
		t.Fatal("expected no match")
	}
}

func TestApply_Empty(t *testing.T) {
	t.Parallel()
	pkt := groupPkt(1, 9)
	matched := Apply(nil, pkt)
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
	matched := Apply(rules, pkt)
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

// ── PassAllTG ────────────────────────────────────────────────────────────────

func TestPassAllTG_Match(t *testing.T) {
	t.Parallel()
	r := &PassAllTG{Name: testRuleName, Slot: 1}
	pkt := groupPkt(1, 12345)

	res := r.Process(pkt)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	// Should not modify any fields
	if pkt.Dst != 12345 {
		t.Fatalf("expected Dst=12345 unchanged, got %d", pkt.Dst)
	}
}

func TestPassAllTG_NoMatch_WrongSlot(t *testing.T) {
	t.Parallel()
	r := &PassAllTG{Name: testRuleName, Slot: 1}
	pkt := groupPkt(2, 12345)

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched for wrong slot")
	}
}

func TestPassAllTG_NoMatch_PrivateCall(t *testing.T) {
	t.Parallel()
	r := &PassAllTG{Name: testRuleName, Slot: 1}
	pkt := privatePkt(1, 9990, 1234)

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched for private call")
	}
}

// ── PassAllPC ────────────────────────────────────────────────────────────────

func TestPassAllPC_Match(t *testing.T) {
	t.Parallel()
	r := &PassAllPC{Name: testRuleName, Slot: 2}
	pkt := privatePkt(2, 9990, 1234)

	res := r.Process(pkt)
	if res != Matched {
		t.Fatal("expected Matched")
	}
	// Should not modify any fields
	if pkt.Dst != 9990 {
		t.Fatalf("expected Dst=9990 unchanged, got %d", pkt.Dst)
	}
	if pkt.Src != 1234 {
		t.Fatalf("expected Src=1234 unchanged, got %d", pkt.Src)
	}
}

func TestPassAllPC_NoMatch_WrongSlot(t *testing.T) {
	t.Parallel()
	r := &PassAllPC{Name: testRuleName, Slot: 2}
	pkt := privatePkt(1, 9990, 1234)

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched for wrong slot")
	}
}

func TestPassAllPC_NoMatch_GroupCall(t *testing.T) {
	t.Parallel()
	r := &PassAllPC{Name: testRuleName, Slot: 1}
	pkt := groupPkt(1, 9)

	res := r.Process(pkt)
	if res != Unmatched {
		t.Fatal("expected Unmatched for group call")
	}
}

// ── PassAll as fallback in Apply ─────────────────────────────────────────────

func TestApply_PassAllFallback(t *testing.T) {
	t.Parallel()
	// Specific rewrite only matches TG 100-109
	specificRules := []Rule{
		&TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 100, ToSlot: 1, ToTG: 200, Range: 10},
	}
	passallRules := []Rule{
		&PassAllTG{Name: testRuleName, Slot: 1},
		&PassAllPC{Name: testRuleName, Slot: 1},
	}

	// TG 9 doesn't match specific rules
	pkt := groupPkt(1, 9)
	if Apply(specificRules, pkt) {
		t.Fatal("specific rules should not match TG 9")
	}
	// But passall should match
	if !Apply(passallRules, pkt) {
		t.Fatal("passall should match TG 9 group call on slot 1")
	}
	if pkt.Dst != 9 {
		t.Fatalf("passall should not modify Dst, got %d", pkt.Dst)
	}

	// Private call to 9990 doesn't match specific TG rules
	pkt2 := privatePkt(1, 9990, 1234)
	if Apply(specificRules, pkt2) {
		t.Fatal("specific TG rules should not match private call")
	}
	if !Apply(passallRules, pkt2) {
		t.Fatal("passall PC should match private call on slot 1")
	}
	if pkt2.Dst != 9990 {
		t.Fatalf("passall should not modify Dst, got %d", pkt2.Dst)
	}
}

func TestApply_SpecificTakesPriorityOverPassAll(t *testing.T) {
	t.Parallel()
	// When specific rules match, passall should not be needed
	rules := []Rule{
		&TGRewrite{Name: testRuleName, FromSlot: 1, FromTG: 100, ToSlot: 1, ToTG: 200, Range: 10},
		&PassAllTG{Name: testRuleName, Slot: 1}, // appended after specific
	}

	pkt := groupPkt(1, 105)
	if !Apply(rules, pkt) {
		t.Fatal("expected match")
	}
	// Specific rule should have rewritten it
	if pkt.Dst != 205 {
		t.Fatalf("expected Dst=205 from specific rewrite, got %d", pkt.Dst)
	}
}
