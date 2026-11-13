package netstat

import (
	"encoding/binary"
	"net/netip"
	"os"
	"os/exec"
	"testing"
)

func sockaddrBytes(b []byte, ap netip.AddrPort) {
	if ap.Addr().Is4() {
		b[0], b[1] = 16, 2
		binary.BigEndian.PutUint16(b[2:], ap.Port())
		a := ap.Addr().As4()
		copy(b[4:], a[:])
		return
	}
	b[0], b[1] = 28, 30
	binary.BigEndian.PutUint16(b[2:], ap.Port())
	a := ap.Addr().As16()
	copy(b[8:], a[:])
}

// updateMsg is an nstat_msg_src_update as the kernel lays it out.
func updateMsg(provider uint32, ref uint64, pid int, local, remote string, c Counts, flags uint16) []byte {
	l := layouts[provider]
	descLen := map[uint32]int{providerTCP: 336, providerUDP: 248}[provider]
	b := make([]byte, updateData+descLen)
	le := binary.LittleEndian
	le.PutUint32(b[8:], msgSrcUpdate)
	le.PutUint16(b[12:], uint16(len(b)))
	le.PutUint16(b[14:], flags)
	le.PutUint64(b[16:], ref)
	le.PutUint64(b[updateCounts:], c.RxPackets)
	le.PutUint64(b[updateCounts+8:], c.RxBytes)
	le.PutUint64(b[updateCounts+16:], c.TxPackets)
	le.PutUint64(b[updateCounts+24:], c.TxBytes)
	le.PutUint32(b[updateProv:], provider)
	d := b[updateData:]
	le.PutUint32(d[l.pid:], uint32(pid))
	if local != "" {
		sockaddrBytes(d[l.local:], netip.MustParseAddrPort(local))
	}
	if remote != "" {
		sockaddrBytes(d[l.remote:], netip.MustParseAddrPort(remote))
	}
	return b
}

func removedMsg(ref uint64) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[8:], msgSrcRemoved)
	binary.LittleEndian.PutUint16(b[12:], 24)
	binary.LittleEndian.PutUint64(b[16:], ref)
	return b
}

func feed(t *testing.T, a *Account, parts ...[]byte) {
	t.Helper()
	var buf []byte
	for _, p := range parts {
		buf = append(buf, p...)
	}
	msgs, err := parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(parts) {
		t.Fatalf("parsed %d messages from %d", len(msgs), len(parts))
	}
	for _, m := range msgs {
		switch m.typ {
		case msgSrcUpdate:
			a.update(*m.src, m.flags&flagClosing != 0)
		case msgSrcRemoved:
			a.remove(m.ref)
		}
	}
}

func TestParseReadsTheDescriptors(t *testing.T) {
	msgs, err := parse(append(
		updateMsg(providerTCP, 7, 42, "[::ffff:127.0.0.1]:5000", "127.0.0.1:6000", Counts{1, 2, 3, 4}, flagClosing),
		updateMsg(providerUDP, 8, 43, "127.0.0.1:53", "", Counts{5, 6, 7, 8}, 0)...))
	if err != nil {
		t.Fatal(err)
	}
	tcp, udp := msgs[0].src, msgs[1].src
	if tcp.ref != 7 || tcp.pid != 42 || tcp.local.String() != "127.0.0.1:5000" || tcp.remote.String() != "127.0.0.1:6000" || tcp.counts != (Counts{1, 2, 3, 4}) || msgs[0].flags&flagClosing == 0 {
		t.Errorf("tcp: %+v flags %#x", *tcp, msgs[0].flags)
	}
	if udp.ref != 8 || udp.pid != 43 || udp.local.String() != "127.0.0.1:53" || udp.remote.IsValid() || udp.counts != (Counts{5, 6, 7, 8}) {
		t.Errorf("udp: %+v", *udp)
	}
}

func TestParseRefusesAShortDescriptor(t *testing.T) {
	b := updateMsg(providerTCP, 1, 1, "127.0.0.1:1", "127.0.0.1:2", Counts{}, 0)[:updateData+100]
	binary.LittleEndian.PutUint16(b[12:], uint16(len(b)))
	if _, err := parse(b); err == nil {
		t.Fatal("a descriptor shorter than the layout parsed")
	}
}

func TestAccountCountsTheContainersSockets(t *testing.T) {
	a := NewAccount("")
	a.SetMembers([]int{10, 11})
	feed(t, a,
		// The container's client of a peer outside it.
		updateMsg(providerTCP, 1, 10, "127.0.0.1:50000", "127.0.0.1:7000", Counts{1, 100, 2, 200}, 0),
		updateMsg(providerTCP, 2, 99, "127.0.0.1:7000", "127.0.0.1:50000", Counts{2, 200, 1, 100}, 0),
		// A connection between two of its own processes.
		updateMsg(providerTCP, 3, 10, "127.0.0.1:50001", "127.0.0.1:8000", Counts{1, 1000, 1, 1000}, 0),
		updateMsg(providerTCP, 4, 11, "127.0.0.1:8000", "127.0.0.1:50001", Counts{1, 1000, 1, 1000}, 0),
		// Another process's traffic.
		updateMsg(providerUDP, 5, 99, "127.0.0.1:9000", "", Counts{9, 9000, 9, 9000}, 0),
	)
	if got := a.total(); got != (Counts{1, 100, 2, 200}) {
		t.Fatalf("total %+v, want the client's traffic alone", got)
	}
	// Closing keeps the counts: the client's final ones, and none for the
	// internal connection, whichever end closes first.
	feed(t, a,
		updateMsg(providerTCP, 1, 10, "127.0.0.1:50000", "127.0.0.1:7000", Counts{2, 150, 3, 300}, flagClosing),
		removedMsg(1),
		updateMsg(providerTCP, 4, 11, "127.0.0.1:8000", "127.0.0.1:50001", Counts{1, 1000, 1, 1000}, flagClosing),
		removedMsg(4),
		updateMsg(providerTCP, 3, 10, "127.0.0.1:50001", "127.0.0.1:8000", Counts{1, 1000, 1, 1000}, flagClosing),
		removedMsg(3),
	)
	if got := a.total(); got != (Counts{2, 150, 3, 300}) {
		t.Fatalf("total after closing %+v, want the client's final counts", got)
	}
	// A socket stays the container's after the process that used it left.
	feed(t, a, updateMsg(providerUDP, 6, 11, "127.0.0.1:9100", "", Counts{1, 10, 1, 10}, 0))
	a.SetMembers([]int{10})
	if got := a.total(); got != (Counts{3, 160, 4, 310}) {
		t.Fatalf("total %+v, want the former member's socket counted", got)
	}
}

func TestAccountCountsTheRoutersSockets(t *testing.T) {
	a := NewAccount("")
	a.SetMembers([]int{10})
	a.AddEndpoint(20, 4000)
	feed(t, a,
		// A peer reaches the router, whose accepted socket is the
		// container's.
		updateMsg(providerTCP, 1, 99, "127.0.0.1:50000", "127.0.0.1:4000", Counts{1, 10, 1, 20}, 0),
		updateMsg(providerTCP, 2, 20, "127.0.0.1:4000", "127.0.0.1:50000", Counts{1, 20, 1, 10}, 0),
		// The same process's other sockets are not.
		updateMsg(providerTCP, 3, 20, "127.0.0.1:4100", "127.0.0.1:50002", Counts{1, 500, 1, 500}, 0),
	)
	if got := a.total(); got != (Counts{1, 20, 1, 10}) {
		t.Fatalf("total %+v, want the router's accepted socket alone", got)
	}
}

// A process whose environment carries the container's tag is the
// container's, though no scan found it yet.
func TestAccountCountsTaggedProcesses(t *testing.T) {
	const tag = "SANDBOX_ROOT=/netstat-test"
	// The shell blocks reading its input, never between two programs.
	child := exec.Command("/bin/sh", "-c", "echo ready; read x")
	child.Env = append(os.Environ(), tag)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); child.Wait() }()
	// Once it prints, the child runs with its own environment.
	if _, err := out.Read(make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	a := NewAccount(tag)
	feed(t, a,
		updateMsg(providerTCP, 1, child.Process.Pid, "127.0.0.1:50000", "127.0.0.1:7000", Counts{1, 100, 1, 100}, 0),
		updateMsg(providerTCP, 2, os.Getpid(), "127.0.0.1:7000", "127.0.0.1:50000", Counts{1, 100, 1, 100}, 0),
	)
	if got := a.total(); got != (Counts{1, 100, 1, 100}) {
		t.Fatalf("total %+v, want the tagged process's socket alone", got)
	}
}

// The kernel announces a client's socket before it connects, so its first
// report has no addresses. An internal connection whose server end closes
// before a poll brings the client's addresses up to date still counts for
// neither end.
func TestAccountPairsAClientAnnouncedBeforeItConnected(t *testing.T) {
	a := NewAccount("")
	a.SetMembers([]int{10, 11})
	feed(t, a,
		updateMsg(providerTCP, 1, 11, "", "", Counts{}, 0),
		updateMsg(providerTCP, 2, 10, "127.0.0.1:8000", "127.0.0.1:50001", Counts{1, 100, 1, 300}, 0),
		updateMsg(providerTCP, 2, 10, "127.0.0.1:8000", "127.0.0.1:50001", Counts{1, 100, 1, 300}, flagClosing),
		removedMsg(2),
		// The poll before the total reports the client's addresses.
		updateMsg(providerTCP, 1, 11, "127.0.0.1:50001", "127.0.0.1:8000", Counts{1, 300, 1, 100}, 0),
	)
	if got := a.total(); got != (Counts{}) {
		t.Fatalf("total %+v, want nothing for an internal connection", got)
	}
	feed(t, a,
		updateMsg(providerTCP, 1, 11, "127.0.0.1:50001", "127.0.0.1:8000", Counts{1, 300, 1, 100}, flagClosing),
		removedMsg(1),
	)
	if got := a.total(); got != (Counts{}) {
		t.Fatalf("total %+v after the client closed, want nothing", got)
	}
}

// A process that ended before its socket's report was read cannot be told
// apart: its socket counts for nothing, and the container's socket it is
// the peer of counts as internal, as a health check's connection to the
// container's server is.
func TestAccountTakesAnEndedPeerForTheContainers(t *testing.T) {
	ended := exec.Command("/bin/sh", "-c", "exit 0")
	if err := ended.Run(); err != nil {
		t.Fatal(err)
	}
	a := NewAccount("SANDBOX_ROOT=/netstat-test")
	a.SetMembers([]int{os.Getpid()})
	feed(t, a,
		updateMsg(providerTCP, 1, ended.Process.Pid, "127.0.0.1:50001", "127.0.0.1:8000", Counts{1, 119, 1, 103}, 0),
		updateMsg(providerTCP, 2, os.Getpid(), "127.0.0.1:8000", "127.0.0.1:50001", Counts{1, 103, 1, 119}, 0),
		// An ended process's socket with no peer of the container's.
		updateMsg(providerUDP, 3, ended.Process.Pid, "127.0.0.1:9000", "", Counts{9, 900, 9, 900}, 0),
	)
	if got := a.total(); got != (Counts{}) {
		t.Fatalf("total %+v, want nothing", got)
	}
	feed(t, a,
		updateMsg(providerTCP, 1, ended.Process.Pid, "127.0.0.1:50001", "127.0.0.1:8000", Counts{1, 119, 1, 103}, flagClosing),
		removedMsg(1),
		updateMsg(providerTCP, 2, os.Getpid(), "127.0.0.1:8000", "127.0.0.1:50001", Counts{1, 103, 1, 119}, flagClosing),
		removedMsg(2),
		updateMsg(providerUDP, 3, ended.Process.Pid, "127.0.0.1:9000", "", Counts{9, 900, 9, 900}, flagClosing),
		removedMsg(3),
	)
	if got := a.total(); got != (Counts{}) {
		t.Fatalf("total after closing %+v, want nothing", got)
	}
}
