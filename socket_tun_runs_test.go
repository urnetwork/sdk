package sdk

import (
	"net/netip"
	"testing"
)

func socketTunRunsTestPacket(destination netip.Addr, marker byte) []byte {
	if destination.Is4() {
		packet := make([]byte, 21)
		packet[0] = 0x45
		a := destination.As4()
		copy(packet[16:20], a[:])
		packet[20] = marker
		return packet
	}
	packet := make([]byte, 41)
	packet[0] = 0x60
	a := destination.As16()
	copy(packet[24:40], a[:])
	packet[40] = marker
	return packet
}

// The socket tun takes exactly the packets addressed to it, in arrival order,
// one batch for each run of them: a burst that is all its own is one batch, a
// lone packet is a batch of one, and other destinations' packets never enter.
func TestSocketTunRunsKeepOrderAndSkipOtherDestinations(t *testing.T) {
	v4 := netip.MustParseAddr("169.254.2.9")
	v6 := netip.MustParseAddr("fd00::9")
	other4 := netip.MustParseAddr("169.254.2.10")
	other6 := netip.MustParseAddr("fd00::a")
	local := []netip.Addr{v4, v6}

	cases := []struct {
		name string
		// one letter per packet: m = mine over v4, M = mine over v6,
		// o = another v4 destination, O = another v6 destination, x = too short
		burst string
		runs  []string
	}{
		{"empty", "", nil},
		{"all mine", "mmmmmm", []string{"mmmmmm"}},
		{"both families", "mMmM", []string{"mMmM"}},
		{"single", "m", []string{"m"}},
		{"none mine", "oOox", nil},
		{"split", "mmoMmOm", []string{"mm", "Mm", "m"}},
		{"edges", "ommo", []string{"mm"}},
		{"short packet", "mxm", []string{"m", "m"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var packets [][]byte
			for i, kind := range c.burst {
				switch kind {
				case 'm':
					packets = append(packets, socketTunRunsTestPacket(v4, byte(i)))
				case 'M':
					packets = append(packets, socketTunRunsTestPacket(v6, byte(i)))
				case 'o':
					packets = append(packets, socketTunRunsTestPacket(other4, byte(i)))
				case 'O':
					packets = append(packets, socketTunRunsTestPacket(other6, byte(i)))
				default:
					packets = append(packets, []byte{0x45, byte(i)})
				}
			}
			var got []string
			var markers []byte
			socketTunRuns(packets, local, func(run [][]byte) {
				if len(run) == 0 {
					t.Fatal("an empty run was written")
				}
				kinds := make([]byte, len(run))
				for i, packet := range run {
					marker := packet[len(packet)-1]
					markers = append(markers, marker)
					kinds[i] = c.burst[marker]
				}
				got = append(got, string(kinds))
			})
			if len(got) != len(c.runs) {
				t.Fatalf("runs = %q, want %q", got, c.runs)
			}
			for i := range got {
				if got[i] != c.runs[i] {
					t.Fatalf("runs = %q, want %q", got, c.runs)
				}
			}
			for i := 1; i < len(markers); i += 1 {
				if markers[i] <= markers[i-1] {
					t.Fatalf("packets were written out of arrival order: %v", markers)
				}
			}
		})
	}
}
