package mmdvm

import (
	"encoding/hex"
	"testing"

	"github.com/USA-RedDragon/dmrgo/v2/transport/dmrd/session"
)

// TestGoldenLoginMessages logs the session of the client into a master
// that sends the salt 0x1234abcd, and checks the octets of every control
// message it sends against what the client sent before it moved onto the
// dmrgo transport codecs.
func TestGoldenLoginMessages(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	if err := client.newPeer(); err != nil {
		t.Fatal(err)
	}
	p := client.peer
	var got []string
	keep := func(out []session.Output) {
		for _, o := range out {
			if o.Kind == session.OutputSend {
				got = append(got, hex.EncodeToString(o.Packet))
			}
		}
	}
	ack := []byte("RPTACK\x00\x04\xc2\x34")
	keep(p.Tick(0, nil))
	keep(p.Packet(0, []byte("RPTACK\x12\x34\xab\xcd"), nil))
	keep(p.Packet(0, ack, nil))
	keep(p.Packet(0, ack, nil))
	keep(p.Tick(session.RetryDefault, nil))
	keep(p.Close(session.RetryDefault, nil))
	want := []string{
		goldenRPTL, goldenRPTK, goldenRPTC, goldenRPTPING, goldenRPTCL,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: got %s, want %s", i, got[i], want[i])
		}
	}
}

const (
	goldenRPTL    = "5250544c0004c234"
	goldenRPTK    = "5250544b0004c234e6bf2de4cfa44cd4e641c00fa716ffdc0a259988d14e78292c763350dd59eb27"
	goldenRPTC    = "525054430004c2344e3043414c4c2020343439303030303030343434303030303030353030312b33352e303030302d3039372e303030303033304f6b6c61686f6d61202020202020202020202020546573742052657065617465722020202020203368747470733a2f2f6578616d706c652e636f6d202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020202020323032313039323120202020202020202020202020202020202020202020202020202020202020204d4d44564d5f4d4d44564d5f48535f4475616c5f4861742020202020202020202020202020202020"
	goldenRPTPING = "52505450494e470004c234"
	goldenRPTCL   = "525054434c0004c234"
)
