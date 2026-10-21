package ipsc

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd"
	"github.com/klauspost/compress/zstd"
)

// captureRecord is one packet that the XPR 8400 sent to the test master,
// without its authentication trailer, and when the master got it.
type captureRecord struct {
	at   time.Time
	data []byte
}

// captureGroup is the packets of one capture log or rig run.
type captureGroup struct {
	name    string
	records []captureRecord
}

// loadXPR8400Capture reads testdata/xpr8400-ipsc.txt.zst: every packet that
// the XPR 8400 (CC 7, both slots on IPSC) sent to the test master of
// /home/reddragon/Projects/dmrgo-rfcap, 2026-10-15 to 2026-10-20, as the
// master logged it. Rate 1/2, 3/4 and 1 data, voice, late entry, PI headers,
// preambles and ACK/SACK responses are in it. A line "# name" starts a
// group; the others are "time hex".
func loadXPR8400Capture(t *testing.T) []captureGroup {
	t.Helper()
	f, err := os.Open("testdata/xpr8400-ipsc.txt.zst")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var groups []captureGroup
	base := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	var last time.Time
	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "# "); ok {
			groups = append(groups, captureGroup{name: name})
			last = time.Time{}
			continue
		}
		clock, hexData, ok := strings.Cut(line, " ")
		if !ok || len(groups) == 0 {
			t.Fatalf("bad capture line %q", line)
		}
		at, err := time.Parse("15:04:05.999", clock)
		if err != nil {
			t.Fatalf("bad capture time %q: %v", clock, err)
		}
		at = base.Add(at.Sub(time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)))
		for !last.IsZero() && at.Before(last) {
			at = at.Add(24 * time.Hour)
		}
		last = at
		data, err := hex.DecodeString(hexData)
		if err != nil {
			t.Fatalf("bad capture hex %q: %v", hexData, err)
		}
		g := &groups[len(groups)-1]
		g.records = append(g.records, captureRecord{at: at, data: data})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return groups
}

// goldenXPR8400 is the SHA-256 of what TestGoldenXPR8400 writes for each
// capture group, from the translator before it moved onto the dmrgo
// transport codecs (c0e6ef5 with dmrgo caac8d7).
func goldenXPR8400(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile("testdata/xpr8400-golden.txt")
	if err != nil {
		t.Fatal(err)
	}
	golden := map[string]string{}
	for line := range strings.Lines(string(data)) {
		name, sum, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			golden[name] = sum
		}
	}
	return golden
}

// TestGoldenXPR8400 replays every user packet that the XPR 8400 sent through
// TranslateToMMDVM, then each DMRD packet that comes out through the
// TranslateToIPSC of a second translator, with the capture times as the
// clock, and checks the octets of every packet out against the golden
// digests. Set IPSC_GOLDEN_DUMP to a directory to write the full output of
// each group there, and IPSC_GOLDEN_UPDATE=1 to rewrite the digests.
func TestGoldenXPR8400(t *testing.T) {
	t.Parallel()
	groups := loadXPR8400Capture(t)
	golden := goldenXPR8400(t)
	dump := os.Getenv("IPSC_GOLDEN_DUMP")
	var update bytes.Buffer
	for _, g := range groups {
		out := replayGroup(t, g)
		sum := sha256.Sum256(out)
		got := hex.EncodeToString(sum[:])
		fmt.Fprintf(&update, "%s %s\n", g.name, got)
		if dump != "" {
			if err := os.WriteFile(dump+"/"+g.name+".txt", out, 0o600); err != nil { //nolint:gosec // a test dump directory the developer names
				t.Fatal(err)
			}
		}
		if os.Getenv("IPSC_GOLDEN_UPDATE") == "" && golden[g.name] != got {
			t.Errorf("group %s: output digest %s, want %s", g.name, got, golden[g.name])
		}
	}
	if os.Getenv("IPSC_GOLDEN_UPDATE") != "" {
		if err := os.WriteFile("testdata/xpr8400-golden.txt", update.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// replayGroup returns the text of every packet that the two translators
// build for the packets of g: "m i hex" for a DMRD packet from user packet
// i, "i i hex" for an IPSC packet built from it, and "e hex" for the packets
// of EndStreams at the end.
func replayGroup(t *testing.T, g captureGroup) []byte {
	t.Helper()
	var now time.Time
	clock := func() time.Time { return now }
	toMMDVM, err := NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	toMMDVM.SetPeerID(3149999)
	toMMDVM.SetColorCode(7)
	toMMDVM.now = clock
	toIPSC, err := NewIPSCTranslator()
	if err != nil {
		t.Fatal(err)
	}
	toIPSC.SetPeerID(3149999)
	toIPSC.SetColorCode(7)
	toIPSC.now = clock
	var out bytes.Buffer
	for i, r := range g.records {
		now = r.at
		switch r.data[0] {
		case 0x80, 0x81, 0x83, 0x84:
		default:
			continue
		}
		for _, pkt := range toMMDVM.TranslateToMMDVM(r.data[0], r.data) {
			fmt.Fprintf(&out, "m %d %x\n", i, goldenDMRD(pkt))
			for _, p := range toIPSC.TranslateToIPSC(pkt) {
				fmt.Fprintf(&out, "i %d %x\n", i, p)
			}
		}
	}
	for _, p := range toIPSC.EndStreams() {
		fmt.Fprintf(&out, "e %x\n", p)
	}
	return out.Bytes()
}

// goldenDMRD returns the 53 octets of pkt.
func goldenDMRD(pkt dmrd.Data) []byte {
	pkt.Size = 0
	b, err := pkt.AppendBinary(nil)
	if err != nil {
		panic(err)
	}
	return b
}
