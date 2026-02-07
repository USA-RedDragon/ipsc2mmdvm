// Package rewrite provides DMRGateway-compatible rewrite rules for routing
// DMR packets between multiple HBRP masters. The rewrite types mirror those
// found in g4klx/DMRGateway: TGRewrite, PCRewrite, TypeRewrite, and SrcRewrite.
//
// Each rule inspects the FLCO (Full Link Control Opcode), slot, and
// source/destination IDs of a DMR packet and, if matched, rewrites the
// relevant fields in-place. Rules support contiguous ID ranges via the
// Range parameter.
package rewrite

import (
	"fmt"
	"log/slog"

	"github.com/USA-RedDragon/ipsc2hbrp/internal/hbrp/proto"
)

// Result indicates the outcome of applying a rewrite rule.
type Result int

const (
	// Unmatched means the rule did not apply to the packet.
	Unmatched Result = iota
	// Matched means the rule matched and the packet was rewritten.
	Matched
)

// Rule is the interface all rewrite rules implement.
type Rule interface {
	// Process inspects and potentially rewrites pkt. Returns Matched if the
	// rule applied, Unmatched otherwise. When trace is true, debug logging
	// is emitted.
	Process(pkt *proto.Packet, trace bool) Result
}

// Apply iterates over rules and returns true if any rule matched.
// The first matching rule wins and the iteration stops.
func Apply(rules []Rule, pkt *proto.Packet, trace bool) bool {
	for _, r := range rules {
		if r.Process(pkt, trace) == Matched {
			return true
		}
	}
	return false
}

// --- TGRewrite ---------------------------------------------------------------
// Rewrites group (TG) calls: matches Group FLCO, fromSlot, and destination TG
// in range [fromTG, fromTG+range-1]. Rewrites slot and destination TG.

// TGRewrite rewrites talkgroup-addressed group calls.
type TGRewrite struct {
	Name     string
	FromSlot uint // 1 or 2
	FromTG   uint // start of source TG range
	ToSlot   uint // 1 or 2
	ToTG     uint // start of destination TG range
	Range    uint // number of contiguous TGs
}

func (r *TGRewrite) fromTGEnd() uint { return r.FromTG + r.Range - 1 }
func (r *TGRewrite) toTGEnd() uint   { return r.ToTG + r.Range - 1 }

func (r *TGRewrite) Process(pkt *proto.Packet, trace bool) Result {
	slot := pktSlot(pkt)
	if !pkt.GroupCall || slot != r.FromSlot || pkt.Dst < r.FromTG || pkt.Dst > r.fromTGEnd() {
		if trace {
			if r.FromTG == r.fromTGEnd() {
				slog.Debug(fmt.Sprintf("Rule Trace, TGRewrite from %s Slot=%d Dst=TG%d: not matched", r.Name, r.FromSlot, r.FromTG))
			} else {
				slog.Debug(fmt.Sprintf("Rule Trace, TGRewrite from %s Slot=%d Dst=TG%d-TG%d: not matched", r.Name, r.FromSlot, r.FromTG, r.fromTGEnd()))
			}
		}
		return Unmatched
	}

	if r.FromSlot != r.ToSlot {
		setPktSlot(pkt, r.ToSlot)
	}

	if r.FromTG != r.ToTG {
		pkt.Dst = pkt.Dst + r.ToTG - r.FromTG
	}

	if trace {
		if r.FromTG == r.fromTGEnd() {
			slog.Debug(fmt.Sprintf("Rule Trace, TGRewrite from %s Slot=%d Dst=TG%d: matched", r.Name, r.FromSlot, r.FromTG))
		} else {
			slog.Debug(fmt.Sprintf("Rule Trace, TGRewrite from %s Slot=%d Dst=TG%d-TG%d: matched", r.Name, r.FromSlot, r.FromTG, r.fromTGEnd()))
		}
		if r.ToTG == r.toTGEnd() {
			slog.Debug(fmt.Sprintf("Rule Trace, TGRewrite to %s Slot=%d Dst=TG%d", r.Name, r.ToSlot, r.ToTG))
		} else {
			slog.Debug(fmt.Sprintf("Rule Trace, TGRewrite to %s Slot=%d Dst=TG%d-TG%d", r.Name, r.ToSlot, r.ToTG, r.toTGEnd()))
		}
	}

	return Matched
}

// --- PCRewrite ---------------------------------------------------------------
// Rewrites private calls: matches Private FLCO, fromSlot, and destination ID
// in range [fromId, fromId+range-1]. Rewrites slot and destination ID.

// PCRewrite rewrites private-call addressed packets.
type PCRewrite struct {
	Name     string
	FromSlot uint
	FromID   uint // start of source ID range
	ToSlot   uint
	ToID     uint // start of destination ID range
	Range    uint
}

func (r *PCRewrite) fromIDEnd() uint { return r.FromID + r.Range - 1 }
func (r *PCRewrite) toIDEnd() uint   { return r.ToID + r.Range - 1 }

func (r *PCRewrite) Process(pkt *proto.Packet, trace bool) Result {
	slot := pktSlot(pkt)
	if pkt.GroupCall || slot != r.FromSlot || pkt.Dst < r.FromID || pkt.Dst > r.fromIDEnd() {
		if trace {
			slog.Debug(fmt.Sprintf("Rule Trace, PCRewrite from %s Slot=%d Dst=%d-%d: not matched", r.Name, r.FromSlot, r.FromID, r.fromIDEnd()))
		}
		return Unmatched
	}

	if r.FromSlot != r.ToSlot {
		setPktSlot(pkt, r.ToSlot)
	}

	if r.FromID != r.ToID {
		pkt.Dst = pkt.Dst + r.ToID - r.FromID
	}

	if trace {
		slog.Debug(fmt.Sprintf("Rule Trace, PCRewrite from %s Slot=%d Dst=%d-%d: matched", r.Name, r.FromSlot, r.FromID, r.fromIDEnd()))
		slog.Debug(fmt.Sprintf("Rule Trace, PCRewrite to %s Slot=%d Dst=%d-%d", r.Name, r.ToSlot, r.ToID, r.toIDEnd()))
	}

	return Matched
}

// --- TypeRewrite -------------------------------------------------------------
// Converts Group TG calls to Private calls: matches Group FLCO, fromSlot,
// and destination TG in range. Rewrites to Private FLCO with the mapped ID.

// TypeRewrite converts group TG calls to private calls.
type TypeRewrite struct {
	Name     string
	FromSlot uint
	FromTG   uint // start of source TG range
	ToSlot   uint
	ToID     uint // start of destination private ID range
	Range    uint
}

func (r *TypeRewrite) fromTGEnd() uint { return r.FromTG + r.Range - 1 }
func (r *TypeRewrite) toIDEnd() uint   { return r.ToID + r.Range - 1 }

func (r *TypeRewrite) Process(pkt *proto.Packet, trace bool) Result {
	slot := pktSlot(pkt)
	if !pkt.GroupCall || slot != r.FromSlot || pkt.Dst < r.FromTG || pkt.Dst > r.fromTGEnd() {
		if trace {
			if r.FromTG == r.fromTGEnd() {
				slog.Debug(fmt.Sprintf("Rule Trace, TypeRewrite from %s Slot=%d Dst=TG%d: not matched", r.Name, r.FromSlot, r.FromTG))
			} else {
				slog.Debug(fmt.Sprintf("Rule Trace, TypeRewrite from %s Slot=%d Dst=TG%d-TG%d: not matched", r.Name, r.FromSlot, r.FromTG, r.fromTGEnd()))
			}
		}
		return Unmatched
	}

	if r.FromSlot != r.ToSlot {
		setPktSlot(pkt, r.ToSlot)
	}

	if r.FromTG != r.ToID {
		pkt.Dst = pkt.Dst + r.ToID - r.FromTG
	}

	// Convert from Group to Private call
	pkt.GroupCall = false

	if trace {
		if r.FromTG == r.fromTGEnd() {
			slog.Debug(fmt.Sprintf("Rule Trace, TypeRewrite from %s Slot=%d Dst=TG%d: matched", r.Name, r.FromSlot, r.FromTG))
		} else {
			slog.Debug(fmt.Sprintf("Rule Trace, TypeRewrite from %s Slot=%d Dst=TG%d-TG%d: matched", r.Name, r.FromSlot, r.FromTG, r.fromTGEnd()))
		}
		if r.ToID == r.toIDEnd() {
			slog.Debug(fmt.Sprintf("Rule Trace, TypeRewrite to %s Slot=%d Dst=%d", r.Name, r.ToSlot, r.ToID))
		} else {
			slog.Debug(fmt.Sprintf("Rule Trace, TypeRewrite to %s Slot=%d Dst=%d-%d", r.Name, r.ToSlot, r.ToID, r.toIDEnd()))
		}
	}

	return Matched
}

// --- SrcRewrite --------------------------------------------------------------
// Matches Private calls by source ID and rewrites them as Group TG calls.
// Used for the network→RF direction in DMRGateway.

// SrcRewrite rewrites private calls (matched by source ID) into group calls.
type SrcRewrite struct {
	Name     string
	FromSlot uint
	FromID   uint // start of source ID range
	ToSlot   uint
	ToTG     uint // destination talkgroup
	Range    uint
}

func (r *SrcRewrite) fromIDEnd() uint { return r.FromID + r.Range - 1 }

func (r *SrcRewrite) Process(pkt *proto.Packet, trace bool) Result {
	slot := pktSlot(pkt)
	if pkt.GroupCall || slot != r.FromSlot || pkt.Src < r.FromID || pkt.Src > r.fromIDEnd() {
		if trace {
			slog.Debug(fmt.Sprintf("Rule Trace, SrcRewrite from %s Slot=%d Src=%d-%d: not matched", r.Name, r.FromSlot, r.FromID, r.fromIDEnd()))
		}
		return Unmatched
	}

	if r.FromSlot != r.ToSlot {
		setPktSlot(pkt, r.ToSlot)
	}

	pkt.Dst = r.ToTG
	pkt.GroupCall = true

	if trace {
		slog.Debug(fmt.Sprintf("Rule Trace, SrcRewrite from %s Slot=%d Src=%d-%d: matched", r.Name, r.FromSlot, r.FromID, r.fromIDEnd()))
		slog.Debug(fmt.Sprintf("Rule Trace, SrcRewrite to %s Slot=%d Dst=TG%d", r.Name, r.ToSlot, r.ToTG))
	}

	return Matched
}

// --- helpers -----------------------------------------------------------------

// pktSlot returns the slot number (1 or 2) from a proto.Packet.
// Slot=false → slot 1, Slot=true → slot 2.
func pktSlot(pkt *proto.Packet) uint {
	if pkt.Slot {
		return 2
	}
	return 1
}

// setPktSlot sets the slot on a proto.Packet. slot must be 1 or 2.
func setPktSlot(pkt *proto.Packet, slot uint) {
	pkt.Slot = (slot == 2)
}
