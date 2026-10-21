// Package rewrite provides DMRGateway-compatible rewrite rules for routing
// DMR packets between multiple MMDVM masters. The rewrite types mirror those
// found in g4klx/DMRGateway: TGRewrite, PCRewrite, TypeRewrite, and SrcRewrite.
//
// Each rule inspects the FLCO (Full Link Control Opcode), slot, and
// source/destination IDs of a DMR packet and, if matched, rewrites the
// relevant fields in-place. Rules support contiguous ID ranges via the
// Range parameter.
package rewrite

import (
	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
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
	Process(pkt *dmrd.Data) Result
}

// Apply iterates over rules and returns true if any rule matched.
// The first matching rule wins and the iteration stops.
func Apply(rules []Rule, pkt *dmrd.Data) bool {
	for _, r := range rules {
		if r.Process(pkt) == Matched {
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

func (r *TGRewrite) Process(pkt *dmrd.Data) Result {
	slot := pktSlot(pkt)
	if pkt.Private || slot != r.FromSlot || uint(pkt.Destination) < r.FromTG || uint(pkt.Destination) > r.fromTGEnd() {
		return Unmatched
	}

	if r.FromSlot != r.ToSlot {
		setPktSlot(pkt, r.ToSlot)
	}

	if r.FromTG != r.ToTG {
		pkt.Destination = id24(uint(pkt.Destination) + r.ToTG - r.FromTG)
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

func (r *PCRewrite) Process(pkt *dmrd.Data) Result {
	slot := pktSlot(pkt)
	if !pkt.Private || slot != r.FromSlot || uint(pkt.Destination) < r.FromID || uint(pkt.Destination) > r.fromIDEnd() {
		return Unmatched
	}

	if r.FromSlot != r.ToSlot {
		setPktSlot(pkt, r.ToSlot)
	}

	if r.FromID != r.ToID {
		pkt.Destination = id24(uint(pkt.Destination) + r.ToID - r.FromID)
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

func (r *TypeRewrite) Process(pkt *dmrd.Data) Result {
	slot := pktSlot(pkt)
	if pkt.Private || slot != r.FromSlot || uint(pkt.Destination) < r.FromTG || uint(pkt.Destination) > r.fromTGEnd() {
		return Unmatched
	}

	if r.FromSlot != r.ToSlot {
		setPktSlot(pkt, r.ToSlot)
	}

	if r.FromTG != r.ToID {
		pkt.Destination = id24(uint(pkt.Destination) + r.ToID - r.FromTG)
	}

	// Convert from Group to Private call
	pkt.Private = true

	return Matched
}

// --- SrcRewrite --------------------------------------------------------------
// Converts private calls to group calls by source: matches Private FLCO,
// fromSlot, and source ID in range [fromId, fromId+range-1]. Rewrites to Group
// FLCO addressed to toTG on toSlot. The source ID is not changed. This mirrors
// DMRGateway's CRewriteSrc::process (RewriteSrc.cpp).

// SrcRewrite converts matched private calls into group calls to a fixed TG.
type SrcRewrite struct {
	Name     string
	FromSlot uint
	FromID   uint // start of source ID range
	ToSlot   uint
	ToTG     uint // destination talkgroup
	Range    uint
}

func (r *SrcRewrite) fromIDEnd() uint { return r.FromID + r.Range - 1 }

func (r *SrcRewrite) Process(pkt *dmrd.Data) Result {
	slot := pktSlot(pkt)
	if !pkt.Private || slot != r.FromSlot || uint(pkt.Source) < r.FromID || uint(pkt.Source) > r.fromIDEnd() {
		return Unmatched
	}

	if r.FromSlot != r.ToSlot {
		setPktSlot(pkt, r.ToSlot)
	}

	pkt.Destination = id24(r.ToTG)
	pkt.Private = false

	return Matched
}

// --- PassAllTG ---------------------------------------------------------------
// Matches any group call on a given slot without rewriting anything.
// Used as a fallback rule after specific rewrites.

// PassAllTG allows all group calls on a specific slot to pass through.
type PassAllTG struct {
	Name string
	Slot uint // 1 or 2
}

func (r *PassAllTG) Process(pkt *dmrd.Data) Result {
	slot := pktSlot(pkt)
	matched := !pkt.Private && slot == r.Slot

	if matched {
		return Matched
	}
	return Unmatched
}

// --- PassAllPC ---------------------------------------------------------------
// Matches any private call on a given slot without rewriting anything.
// Used as a fallback rule after specific rewrites.

// PassAllPC allows all private calls on a specific slot to pass through.
type PassAllPC struct {
	Name string
	Slot uint // 1 or 2
}

func (r *PassAllPC) Process(pkt *dmrd.Data) Result {
	slot := pktSlot(pkt)
	matched := pkt.Private && slot == r.Slot

	if matched {
		return Matched
	}
	return Unmatched
}

// --- helpers -----------------------------------------------------------------

// pktSlot returns the slot number (1 or 2) of pkt.
func pktSlot(pkt *dmrd.Data) uint {
	if pkt.Slot == enums.Slot2 {
		return 2
	}
	return 1
}

// setPktSlot sets the slot of pkt. slot must be 1 or 2.
func setPktSlot(pkt *dmrd.Data, slot uint) {
	pkt.Slot = enums.Slot1
	if slot == 2 {
		pkt.Slot = enums.Slot2
	}
}

// id24 returns the low 24 bits of id, the part of it that a DMRD packet
// carries.
func id24(id uint) uint32 {
	return uint32(id & 0xFFFFFF)
}
