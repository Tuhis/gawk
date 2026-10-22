package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// freeUDPPort returns a loopback UDP port that was free a moment ago.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// waitBound waits until something holds the UDP port, so a dial does not
// race the listener.
func waitBound(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			return
		}
		c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing bound UDP port %d", port)
}

// startRecv runs `recv` on a fresh port and returns its address and the
// channel its result arrives on (it returns once its one session ends).
func startRecv(t *testing.T, out string, extra ...string) (string, <-chan error) {
	t.Helper()
	port := freeUDPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	done := make(chan error, 1)
	go func() { done <- runRecv(append([]string{"-addr", addr, "-out", out}, extra...)) }()
	waitBound(t, port)
	return addr, done
}

func waitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

// readLog parses a recv-format event log into per-kind counts and lines.
func readLog(t *testing.T, path string) map[string][][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ",")
		if len(fields) != 6 {
			t.Fatalf("recv line %q: %d fields, want 6", sc.Text(), len(fields))
		}
		if _, err := strconv.ParseInt(fields[0], 10, 64); err != nil {
			t.Fatalf("recv line %q: bad timestamp", sc.Text())
		}
		out[fields[1]] = append(out[fields[1]], fields)
	}
	return out
}

// readSendLog parses legsend's F lines: frame -> (key, size, n, k).
type sentFrame struct{ key, size, n, k int }

func readSendLog(t *testing.T, path string) map[int]sentFrame {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]sentFrame{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 8 || f[0] != "F" || f[7] != "0" {
			t.Fatalf("send line %q: want F,frame,key,size,n,k,t_ns,0", line)
		}
		var v [5]int
		for i := range v {
			n, err := strconv.Atoi(f[i+1])
			if err != nil {
				t.Fatalf("send line %q: %v", line, err)
			}
			v[i] = n
		}
		out[v[0]] = sentFrame{key: v[1], size: v[2], n: v[3], k: v[4]}
	}
	return out
}

func labArgs(url, out string, extra ...string) []string {
	return append([]string{"-url", url, "-out", out, "-fps", "60", "-gop", "250ms", "-seconds", "0.6", "-mbps", "2"}, extra...)
}

// The PoP sender over its carrier: every delta rides the leg's reliable
// carrier stream (R/Q records, none as datagrams), keyframes ride their own
// uni streams, and the sender log accounts for every frame the receiver saw.
func TestLegSendCarrierReachesReceiver(t *testing.T) {
	dir := t.TempDir()
	recvOut, sendOut := filepath.Join(dir, "recv.csv"), filepath.Join(dir, "send.csv")
	hash := filepath.Join(dir, "hash.txt")
	addr, recvDone := startRecv(t, recvOut, "-cert-hash-file", hash, "-stream-window", "1048576", "-conn-window", "4194304")

	if err := runLegSend(labArgs("https://"+addr+"/lab", sendOut, "-mode", "carrier")); err != nil {
		t.Fatalf("legsend: %v", err)
	}
	if err := waitErr(t, recvDone, "recv"); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if h, err := os.ReadFile(hash); err != nil || len(h) != 64 {
		t.Fatalf("cert hash file %q (%v): want 64 hex chars", h, err)
	}

	sent := readSendLog(t, sendOut)
	if len(sent) < 10 {
		t.Fatalf("sender logged %d frames in 0.6s at 60fps", len(sent))
	}
	keys := 0
	for id, f := range sent {
		if f.key == 1 {
			keys++
			if f.n != 0 || f.k != 0 {
				t.Fatalf("keyframe %d logged chunks %d/%d", id, f.n, f.k)
			}
			if (id-1)%15 != 0 {
				t.Fatalf("frame %d is a keyframe; a 250ms GOP at 60fps keys every 15th", id)
			}
		} else if f.n < 1 || f.k != min(2, f.n) {
			t.Fatalf("delta %d: %d chunks, %d parity; want >=1 and min(2, chunks)", id, f.n, f.k)
		}
	}
	if keys < 2 {
		t.Fatalf("%d keyframes sent, want >= 2", keys)
	}

	got := readLog(t, recvOut)
	if len(got["D"]) != 0 || len(got["P"]) != 0 {
		t.Fatalf("carrier mode sent %d chunk and %d parity datagrams", len(got["D"]), len(got["P"]))
	}
	if len(got["O"]) == 0 {
		t.Fatal("receiver saw no carrier open")
	}
	if len(got["R"]) == 0 || len(got["Q"]) == 0 {
		t.Fatalf("receiver saw %d chunk records and %d parity records", len(got["R"]), len(got["Q"]))
	}
	for _, r := range got["R"] {
		id, _ := strconv.Atoi(r[2])
		f, ok := sent[id]
		if !ok || f.key == 1 {
			t.Fatalf("record for frame %d the sender never sent as a delta", id)
		}
		if r[4] != strconv.Itoa(f.n) {
			t.Fatalf("frame %d record count %s, sender chunked it into %d", id, r[4], f.n)
		}
	}
	if len(got["K"]) == 0 {
		t.Fatal("receiver completed no keyframe stream")
	}
	for _, k := range got["K"] {
		id, _ := strconv.Atoi(k[2])
		if f, ok := sent[id]; !ok || f.key != 1 {
			t.Fatalf("keyframe stream for frame %d, which the sender did not key", id)
		}
	}
}

// -mode datagram ships deltas as datagrams, never opening a carrier; a
// trace replaces the synthetic GOP and its K flags decide the keyframes.
func TestLegSendDatagramModeWithTrace(t *testing.T) {
	dir := t.TempDir()
	recvOut, sendOut := filepath.Join(dir, "recv.csv"), filepath.Join(dir, "send.csv")
	trace := filepath.Join(dir, "trace.txt")
	if err := os.WriteFile(trace, []byte("5000,K__\n800,___\n\n 0 ,___\n2300,___\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addr, recvDone := startRecv(t, recvOut)

	if err := runLegSend(labArgs("https://"+addr+"/lab", sendOut, "-mode", "datagram", "-trace", trace, "-parity", "1")); err != nil {
		t.Fatalf("legsend: %v", err)
	}
	if err := waitErr(t, recvDone, "recv"); err != nil {
		t.Fatalf("recv: %v", err)
	}
	sent := readSendLog(t, sendOut)
	for id, f := range sent {
		switch (id - 1) % 4 {
		case 0:
			if f.key != 1 || f.size <= 5000 {
				t.Fatalf("frame %d: %+v; trace line 1 is a 5000-byte keyframe (plus header)", id, f)
			}
		case 1:
			if f.key != 0 || f.size != 800 || f.n != 1 || f.k != 1 {
				t.Fatalf("frame %d: %+v; want an 800-byte delta, 1 chunk, 1 parity", id, f)
			}
		case 2:
			if f.key != 0 || f.size != 1 {
				t.Fatalf("frame %d: %+v; a 0-size trace line is clamped to 1 byte", id, f)
			}
		case 3:
			if f.key != 0 || f.size != 2300 || f.n != 3 {
				t.Fatalf("frame %d: %+v; want a 2300-byte delta in 3 chunks", id, f)
			}
		}
	}
	got := readLog(t, recvOut)
	if len(got["O"]) != 0 || len(got["R"]) != 0 {
		t.Fatalf("datagram mode opened %d carriers with %d records", len(got["O"]), len(got["R"]))
	}
	if len(got["D"]) == 0 || len(got["K"]) == 0 {
		t.Fatalf("receiver saw %d chunk datagrams and %d keyframes", len(got["D"]), len(got["K"]))
	}
}

func TestLegSendRejectsBadFlags(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	bad := filepath.Join(dir, "bad.txt")
	_ = os.WriteFile(empty, []byte("\n\n"), 0o644)
	_ = os.WriteFile(bad, []byte("12,K\nabc,_\n"), 0o644)
	out := filepath.Join(dir, "send.csv")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"bbr", []string{"-cc", "bbr"}, "Cubic only"},
		{"parity", []string{"-parity", "3"}, "want 0, 1 or 2"},
		{"negative parity", []string{"-parity", "-1"}, "want 0, 1 or 2"},
		{"mode", []string{"-mode", "stream"}, "want carrier or datagram"},
		{"missing trace", []string{"-trace", filepath.Join(dir, "nope")}, "no such file"},
		{"empty trace", []string{"-trace", empty}, "is empty"},
		{"bad trace", []string{"-trace", bad}, `"abc,_"`},
		{"unwritable log", []string{"-out", filepath.Join(dir, "no", "such", "dir.csv")}, "no such file"},
		{"bad url", []string{"-url", "ftp://%zz"}, "dial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runLegSend(append([]string{"-out", out}, tc.args...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// A frame past wire.MaxParityDataChunks goes without parity, as the
// broadcasters' do; a size of 0 still ships one 1-byte chunk.
func TestDeltaDatagramsEdges(t *testing.T) {
	dgrams, parity, err := deltaDatagrams(1, 0, 256*10, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(dgrams) != 256 || parity != nil {
		t.Fatalf("%d chunks, %d parity; want 256 and none", len(dgrams), len(parity))
	}
	dgrams, parity, err = deltaDatagrams(2, 0, 255*10, 10, 1)
	if err != nil || len(dgrams) != 255 || len(parity) != 1 {
		t.Fatalf("255 chunks: %d chunks, %d parity, %v; want parity at the limit", len(dgrams), len(parity), err)
	}
	dgrams, parity, err = deltaDatagrams(3, 0, 0, 1100, 2)
	// One chunk gets one symbol however many were asked for: a second
	// would duplicate P again (wire.ComputeParity's min(k, n)).
	if err != nil || len(dgrams) != 1 || len(parity) != 1 {
		t.Fatalf("size 0: %d chunks, %d parity, %v", len(dgrams), len(parity), err)
	}
	if _, p, _ := wire.ParseVideoChunk(dgrams[0]); len(p) != 1 {
		t.Fatalf("size 0 chunk payload %d bytes, want 1", len(p))
	}
	// A chunk past the datagram limit is the wire's error, not a panic.
	if _, _, err := deltaDatagrams(4, 0, 5000, 5000, 0); err == nil {
		t.Fatal("a 5000-byte chunk was accepted")
	}
}

func TestLoadTrace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.txt")
	_ = os.WriteFile(p, []byte("  100 ,K_\r\n-5\n7,__\n"), 0o644)
	got, err := loadTrace(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []traceFrame{{100, true}, {1, false}, {7, false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("trace %v, want %v", got, want)
	}
}

func TestRecvFailsOnUnwritableLog(t *testing.T) {
	if err := runRecv([]string{"-out", filepath.Join(t.TempDir(), "x", "y.csv")}); err == nil {
		t.Fatal("recv started with an unwritable log")
	}
}

// interrupt delivers SIGINT to this process until done fires. The test's
// own Notify keeps the default action (exit) from ever running.
func interrupt(t *testing.T, done <-chan error) error {
	t.Helper()
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, syscall.SIGINT)
	defer signal.Stop(sig)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case err := <-done:
			return err
		case <-tick.C:
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		case <-deadline:
			t.Fatal("did not stop on SIGINT")
		}
	}
}

// The PoP sender through the impairment proxy, end to end: the proxy
// forwards both ways, counts what it forwarded and writes the counters on
// SIGINT.
func TestProxyForwardsAndReports(t *testing.T) {
	dir := t.TempDir()
	recvOut, sendOut, stats := filepath.Join(dir, "recv.csv"), filepath.Join(dir, "send.csv"), filepath.Join(dir, "stats.txt")
	addr, recvDone := startRecv(t, recvOut)
	pport := freeUDPPort(t)
	paddr := fmt.Sprintf("127.0.0.1:%d", pport)
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- runProxy([]string{"-listen", paddr, "-upstream", addr, "-rtt", "2ms", "-jitter", "1ms",
			"-rate", "100000000", "-arm-after", "0s", "-stats", stats})
	}()
	waitBound(t, pport)

	if err := runLegSend(labArgs("https://"+paddr+"/lab", sendOut)); err != nil {
		t.Fatalf("legsend via proxy: %v", err)
	}
	if err := waitErr(t, recvDone, "recv"); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if err := interrupt(t, proxyDone); err != nil {
		t.Fatalf("proxy: %v", err)
	}
	raw, err := os.ReadFile(stats)
	if err != nil {
		t.Fatal(err)
	}
	var upF, upL, upO, upH, upQ, dnF, dnL, dnO, dnH, dnQ int
	if _, err := fmt.Sscanf(string(raw),
		"up forwarded=%d random_loss=%d outage_drop=%d outage_held=%d queue_drop=%d\n"+
			"down forwarded=%d random_loss=%d outage_drop=%d outage_held=%d queue_drop=%d\n",
		&upF, &upL, &upO, &upH, &upQ, &dnF, &dnL, &dnO, &dnH, &dnQ); err != nil {
		t.Fatalf("stats %q: %v", raw, err)
	}
	if upF == 0 || dnF == 0 {
		t.Fatalf("proxy forwarded %d up, %d down", upF, dnF)
	}
	if upL+upO+upH+dnL+dnO+dnH != 0 {
		t.Fatalf("an unimpaired path lost or held packets: %q", raw)
	}
	if len(readLog(t, recvOut)["K"]) == 0 {
		t.Fatal("no keyframe crossed the proxy")
	}
}

func TestProxyRejectsBadAddresses(t *testing.T) {
	if err := runProxy([]string{"-listen", "nope:x"}); err == nil {
		t.Fatal("bad -listen accepted")
	}
	if err := runProxy([]string{"-listen", "127.0.0.1:0", "-upstream", "nope:x"}); err == nil {
		t.Fatal("bad -upstream accepted")
	}
}

// subRelay is a stand-in relay for `sub`: /subscribe/{id} sends one of each
// message recv logs, then ends the session.
func subRelay(t *testing.T, wantID string) string {
	t.Helper()
	cert, err := selfSigned()
	if err != nil {
		t.Fatal(err)
	}
	port := freeUDPPort(t)
	mux := http.NewServeMux()
	srv := &webtransport.Server{
		H3: &http3.Server{
			Addr:            fmt.Sprintf("127.0.0.1:%d", port),
			TLSConfig:       http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
			Handler:         mux,
			EnableDatagrams: true,
			QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
		},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	webtransport.ConfigureHTTP3Server(srv.H3)
	mux.HandleFunc("/subscribe/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != wantID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sess, err := srv.Upgrade(w, r)
		if err != nil {
			return
		}
		chunk, _ := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{FrameID: 9, ChunkIndex: 0, ChunkCount: 1}, []byte{1, 2, 3})
		par, _ := wire.AppendParityChunk(nil, wire.ParityChunkHeader{FrameID: 9, ChunkCount: 1, FrameBytes: 3}, []byte{1, 2, 3})
		audio, _ := wire.AppendAudioFrame(nil, wire.AudioFrameHeader{Seq: 1}, []byte{0xfc})
		for range 5 {
			for _, d := range [][]byte{chunk, par, audio, {0xff}, {wire.Version, wire.TypeVideoChunk}} {
				_ = sess.SendDatagram(d)
			}
		}
		// A keyframe that completes, one whose body is cut short and one
		// with trailing bytes past its declared length.
		hdr, _ := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{Keyframe: true, FrameID: 8, PayloadLen: 4})
		for _, body := range [][]byte{append(hdr, 1, 2, 3, 4), append(hdr, 1), append(hdr, 1, 2, 3, 4, 5)} {
			st, err := sess.OpenUniStream()
			if err != nil {
				return
			}
			_, _ = st.Write(body)
			_ = st.Close()
		}
		// A carrier with a record, then one cut mid-record.
		rec, _ := wire.AppendCarrierRecord(wire.AppendCarrierPrologue(nil), chunk)
		for _, body := range [][]byte{rec, rec[:len(rec)-2]} {
			st, err := sess.OpenUniStream()
			if err != nil {
				return
			}
			_, _ = st.Write(body)
			_ = st.Close()
		}
		time.Sleep(400 * time.Millisecond)
		_ = sess.CloseWithError(4001, "bye")
	})
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Log("sub relay:", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })
	waitBound(t, port)
	return fmt.Sprintf("https://127.0.0.1:%d", port)
}

func TestSubLogsWhatTheRelaySends(t *testing.T) {
	dir := t.TempDir()
	idFile, out := filepath.Join(dir, "id.txt"), filepath.Join(dir, "sub.csv")
	if err := os.WriteFile(idFile, []byte(" abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := subRelay(t, "abc123")
	err := runSub([]string{"-url", base, "-id-file", idFile, "-out", out})
	if err == nil || !strings.Contains(err.Error(), "session ended") {
		t.Fatalf("sub: %v; want the relay's close as a session-ended error", err)
	}
	got := readLog(t, out)
	for _, kind := range []string{"D", "P", "A"} {
		if len(got[kind]) == 0 {
			t.Fatalf("sub logged no %s datagram: %v", kind, got)
		}
	}
	if len(got["K"]) != 1 || got["K"][0][2] != "8" || got["K"][0][5] != "4" {
		t.Fatalf("keyframes %v; want exactly frame 8 with 4 body bytes", got["K"])
	}
	if len(got["X"]) != 3 {
		t.Fatalf("%d failed streams logged, want 3 (short keyframe, trailing bytes, cut carrier)", len(got["X"]))
	}
	if len(got["O"]) != 2 || len(got["F"]) != 1 || len(got["R"]) != 1 {
		t.Fatalf("carriers: %d opened, %d finished, %d records; want 2, 1, 1", len(got["O"]), len(got["F"]), len(got["R"]))
	}
}

// A subscribe the relay refuses is a dial error; SIGINT ends a live
// subscription cleanly.
func TestSubDialErrorAndInterrupt(t *testing.T) {
	dir := t.TempDir()
	idFile := filepath.Join(dir, "id.txt")
	_ = os.WriteFile(idFile, []byte("other"), 0o644)
	base := subRelay(t, "abc123")
	err := runSub([]string{"-url", base, "-id-file", idFile, "-out", filepath.Join(dir, "a.csv")})
	if err == nil || !strings.Contains(err.Error(), "subscribe") {
		t.Fatalf("sub to an unknown ID: %v", err)
	}
	if err := runSub([]string{"-url", base, "-id-file", idFile, "-out", filepath.Join(dir, "x", "y.csv")}); err == nil {
		t.Fatal("sub started with an unwritable log")
	}

	// A relay that never closes: SIGINT is the only way out.
	_ = os.WriteFile(idFile, []byte("abc123"), 0o644)
	quiet := subRelayQuiet(t)
	done := make(chan error, 1)
	go func() {
		done <- runSub([]string{"-url", quiet, "-id-file", idFile, "-out", filepath.Join(dir, "b.csv")})
	}()
	time.Sleep(300 * time.Millisecond)
	if err := interrupt(t, done); err != nil {
		t.Fatalf("sub on SIGINT: %v, want nil", err)
	}
}

func subRelayQuiet(t *testing.T) string {
	t.Helper()
	cert, err := selfSigned()
	if err != nil {
		t.Fatal(err)
	}
	port := freeUDPPort(t)
	mux := http.NewServeMux()
	srv := &webtransport.Server{
		H3: &http3.Server{
			Addr:            fmt.Sprintf("127.0.0.1:%d", port),
			TLSConfig:       http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
			Handler:         mux,
			EnableDatagrams: true,
			QUICConfig:      &quic.Config{EnableDatagrams: true},
		},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	webtransport.ConfigureHTTP3Server(srv.H3)
	mux.HandleFunc("/subscribe/{id}", func(w http.ResponseWriter, r *http.Request) {
		sess, err := srv.Upgrade(w, r)
		if err != nil {
			return
		}
		<-sess.Context().Done()
	})
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	waitBound(t, port)
	return fmt.Sprintf("https://127.0.0.1:%d", port)
}
