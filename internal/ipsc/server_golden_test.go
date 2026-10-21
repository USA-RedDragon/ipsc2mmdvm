package ipsc

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// goldenControlKey is the authentication key of TestGoldenXPR8400Control. It
// is not the key of the rig.
const goldenControlKey = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

// goldenControlReplies is what the server sent, before it moved onto the
// dmrgo transport codecs, for each control packet of the XPR 8400 capture
// in the order they first appear, with the port of the peer in a peer list
// and the trailer of a peer list written as zeros.
// The last entry is the de-registration of Stop.
func goldenControlReplies() []string {
	return []string{
		"900004ce2e6a0000805c04080400 -> 910004c2346a0000001d000104020401b02ee611702c5a361c92",
		"920004ce2e -> 930004c234000b0004ce2e7f00000100006a00000000000000000000",
		"f00004ce2e00000000 -> ",
		"960004ce2e6a000080dc04020401 -> 970004c2346a0000001d0402040119e426f53fde9881bd77",
		"850004ce2e000000010002 -> ",
		"f00004ce2e56631100 -> ",
		"960004ce2e6a000280dc04020401 -> 970004c2346a0000001d0402040119e426f53fde9881bd77",
		"850004ce2e000000010101 -> ",
		"9a0004ce2e -> ",
		"stop -> 9a0004c234bd3c58144f37430887a9",
	}
}

// TestGoldenXPR8400Control sends the server, with authentication on, each
// kind of control packet that the XPR 8400 sent, signed, and checks every
// reply against the golden octets. Set IPSC_GOLDEN_DUMP to print the
// replies.
func TestGoldenXPR8400Control(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerWithUDP(t, true, goldenControlKey)
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	addr, ok := peer.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("expected *net.UDPAddr")
	}
	var bodies []string
	for _, b := range []string{
		"900004ce2e6a0000805c04080400", "920004ce2e", "f00004ce2e00000000",
		"960004ce2e6a000080dc04020401", "850004ce2e000000010002", "f00004ce2e56631100",
		"960004ce2e6a000280dc04020401", "850004ce2e000000010101", "9a0004ce2e",
	} {
		if !slices.Contains(bodies, b) {
			bodies = append(bodies, b)
		}
	}
	got := make([]string, 0, len(bodies)+1)
	for _, b := range bodies {
		data := signPacket(t, mustDecodeHex(t, b), goldenControlKey)
		if _, err := s.handlePacket(data, addr); err != nil && !errors.Is(err, ErrPacketIgnored) {
			t.Fatalf("%s: %v", b, err)
		}
		got = append(got, b+" -> "+readReplies(t, peer, addr))
	}
	if err := s.upsertPeer(0x0004ce2e, addr, 0x6a, [4]byte{0, 0, 0x80, 0xdc}); err != nil {
		t.Fatal(err)
	}
	s.deRegisterSelf()
	got = append(got, "stop -> "+readReplies(t, peer, addr))
	want := goldenControlReplies()
	if os.Getenv("IPSC_GOLDEN_DUMP") != "" {
		for _, g := range got {
			t.Log(g)
		}
	}
	if !slices.Equal(got, want) {
		for i := range max(len(got), len(want)) {
			var g, w string
			if i < len(got) {
				g = got[i]
			}
			if i < len(want) {
				w = want[i]
			}
			if g != w {
				t.Errorf("reply %d: got %q, want %q", i, g, w)
			}
		}
	}
}

// readReplies returns the hex of the datagrams that conn gets within 50 ms,
// joined by spaces, with the port of addr in a peer list entry and the
// trailer of a peer list as zeros.
func readReplies(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr) string {
	t.Helper()
	var out []string
	buf := make([]byte, 1500)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		p := slices.Clone(buf[:n])
		if p[0] == 0x93 {
			for i := 7; i+11 <= len(p)-10; i += 11 {
				if binary.BigEndian.Uint16(p[i+8:i+10]) == uint16(addr.Port) { //nolint:gosec // a UDP port
					p[i+8], p[i+9] = 0, 0
				}
			}
			clear(p[len(p)-10:])
		}
		out = append(out, hex.EncodeToString(p))
	}
	return strings.Join(out, " ")
}
