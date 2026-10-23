package ipsc

import (
	"log/slog"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/enums"
	ipscpkt "github.com/USA-RedDragon/dmrgo/v2/transport/ipsc"
)

// CallSplits joins the calls that an XPR 8400 splits back to the calls
// they came from. When the embedded LC of an RF voice call is a talker
// alias LC, the XPR reads it as a voice LC: it starts a new IPSC call whose
// destination and source are octets 3-5 and 6-8 of the alias, and sends
// voice LC header packets with the alias LC in place of voice bursts E and
// F. CallSplits gives the packets of such a call the call, call control and
// RTP clock of the call before it on the same slot of the same peer, so
// routing, logs and the MMDVM stream see one call. A CallSplits is not safe
// for concurrent use.
type CallSplits struct {
	calls map[splitKey]*splitCall
	last  map[splitSlot]*splitCall
	now   func() time.Time
}

type splitKey struct {
	peer, callControl uint32
}

type splitSlot struct {
	peer uint32
	slot enums.Slot
}

type splitCall struct {
	splitParent
	joined     bool
	dropped    bool
	before     splitParent
	firstSeen  time.Time
	firstSeq   uint16
	firstTS    uint32
	dSeq       uint16
	dTimestamp uint32
}

type splitParent struct {
	opcode      ipscpkt.Opcode
	src, dst    uint32
	callControl uint32
	ok          bool
	ended       bool
	lastSeq     uint16
	lastTS      uint32
	lastSeen    time.Time
}

const splitWindow = time.Second

const rtpVoiceStep = 480

// NewCallSplits returns an empty CallSplits.
func NewCallSplits() *CallSplits {
	return &CallSplits{
		calls: map[splitKey]*splitCall{},
		last:  map[splitSlot]*splitCall{},
		now:   time.Now,
	}
}

// Repair returns the IPSC user packet data of packetType as the bridge
// should take it. A packet of a split call comes back with the call, call
// control and RTP clock of the call it was split from. ok is false when
// the packet belongs to a split call with no call in progress before it;
// such a packet carries no address that can be routed.
func (s *CallSplits) Repair(packetType byte, data []byte) (byte, []byte, bool) {
	op := ipscpkt.Opcode(packetType)
	if op != ipscpkt.OpGroupVoice && op != ipscpkt.OpPrivateVoice {
		return packetType, data, true
	}
	u, err := ipscpkt.DecodeUser(data)
	if err != nil {
		return packetType, data, true
	}
	now := s.now()
	s.prune(now)
	key := splitSlot{peer: u.ID, slot: u.Info.Slot()}
	ck := splitKey{peer: u.ID, callControl: u.CallControl}
	c, ok := s.calls[ck]
	switch {
	case !ok || c.ended && (u.IsVoice() || byte(u.Burst) == ipscBurstVoiceHead):
		c = s.start(&u, key, now)
		s.calls[ck] = c
	case !c.joined && !c.dropped && !c.ended && splitCallLC(&u):
		s.join(c, c.before)
	}
	c.lastSeen = now
	term := byte(u.Burst) == ipscBurstVoiceTerm
	if c.dropped {
		c.ended = c.ended || term
		return packetType, nil, false
	}
	if !c.ended || term {
		s.last[key] = c
	}
	c.ended = c.ended || term
	if !c.joined {
		c.lastSeq, c.lastTS = u.RTP.Seq, u.RTP.Timestamp
		return packetType, data, true
	}
	u.Opcode = c.opcode
	u.Destination, u.Source = c.dst, c.src
	u.CallControl = c.callControl
	u.RTP.Seq += c.dSeq
	u.RTP.Timestamp += c.dTimestamp
	c.lastSeq, c.lastTS = u.RTP.Seq, u.RTP.Timestamp
	out, err := u.AppendBinary(make([]byte, 0, len(data)))
	if err != nil {
		slog.Debug("IPSC: cannot encode a packet of a split call", "error", err)
		return packetType, nil, false
	}
	return byte(c.opcode), out, true
}

func (s *CallSplits) start(u *ipscpkt.User, key splitSlot, now time.Time) *splitCall {
	c := &splitCall{
		splitParent: splitParent{opcode: u.Opcode, src: u.Source, dst: u.Destination, callControl: u.CallControl, ok: true},
		firstSeen:   now,
		firstSeq:    u.RTP.Seq,
		firstTS:     u.RTP.Timestamp,
	}
	if p, ok := s.last[key]; ok && !p.dropped {
		c.before = p.splitParent
	}
	if splitCallLC(u) {
		s.join(c, c.before)
	}
	return c
}

func (s *CallSplits) join(c *splitCall, p splitParent) {
	c.before = splitParent{}
	if !p.ok || p.ended || c.firstSeen.Sub(p.lastSeen) > splitWindow {
		slog.Debug("IPSC: dropping a call split from a talker alias with no call before it")
		c.dropped = true
		return
	}
	slog.Debug("IPSC: joining a call split from a talker alias", "src", p.src, "dst", p.dst)
	c.opcode, c.src, c.dst, c.callControl = p.opcode, p.src, p.dst, p.callControl
	c.joined = true
	c.dSeq = p.lastSeq + 1 - c.firstSeq
	c.dTimestamp = p.lastTS + rtpVoiceStep - c.firstTS
}

func (s *CallSplits) prune(now time.Time) {
	for k, c := range s.calls {
		if now.Sub(c.lastSeen) > streamIdleTimeout {
			delete(s.calls, k)
		}
	}
	for k, c := range s.last {
		if now.Sub(c.lastSeen) > splitWindow {
			delete(s.last, k)
		}
	}
}

func splitCallLC(u *ipscpkt.User) bool {
	var lc [9]byte
	switch {
	case u.IsVoice() && u.Voice.Layout == ipscpkt.VoiceLayoutE:
		lc = u.Voice.LC
	case byte(u.Burst) == ipscBurstVoiceHead:
		lc = [9]byte(u.Data.Octets[:9])
	default:
		return false
	}
	flco := enums.FLCO(lc[0] & 0x3F)
	if flco < enums.FLCOTalkerAliasHeader || flco > enums.FLCOTalkerAliasBlock3 {
		return false
	}
	if flco == enums.FLCOTalkerAliasHeader && !aliasFormatValid(lc[2]) {
		return false
	}
	target := uint32(lc[3])<<16 | uint32(lc[4])<<8 | uint32(lc[5])
	source := uint32(lc[6])<<16 | uint32(lc[7])<<8 | uint32(lc[8])
	return target == lcAddress(u.Destination) && source == lcAddress(u.Source)
}

func aliasFormatValid(b byte) bool {
	n := b >> 1 & 0x1F
	return n != 0 && (b>>6 != 3 || n <= 15)
}
