package poplink

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// Pipe is the in-memory link the Wave 3 unit tests run records over: a
// stream one end opens is the other's next accept, and a close reaches
// both ends with its code.
func TestPipeCarriesRecordsAndTheCloseCode(t *testing.T) {
	pop, home := Pipe(netip.MustParseAddrPort("192.0.2.1:5000"), netip.MustParseAddrPort("198.51.100.1:4433"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		s, err := pop.OpenStream(ctx)
		if err != nil {
			return
		}
		_ = Write(s, Hello{Version: "0.44.0", Proto: ProtoMajor})
	}()
	s, err := home.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Read(s)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := rec.(*Hello); !ok || h.Version != "0.44.0" {
		t.Fatalf("record = %#v", rec)
	}
	if home.RemoteAddr().String() != "192.0.2.1:5000" || pop.RemoteAddr().String() != "198.51.100.1:4433" {
		t.Fatalf("addrs %v %v", home.RemoteAddr(), pop.RemoteAddr())
	}
	_ = home.CloseWithError(CloseCodeLinkEnd, "bye")
	select {
	case <-pop.Context().Done():
	case <-ctx.Done():
		t.Fatal("the close never reached the other end")
	}
	if code, ok := CloseCode(SessionEndErr(pop)); !ok || code != CloseCodeLinkEnd {
		t.Fatalf("pop close code = %d %v", code, ok)
	}
	if _, err := pop.AcceptStream(ctx); err == nil {
		t.Fatal("accept on a closed pipe succeeded")
	} else if code, ok := CloseCode(err); !ok || code != CloseCodeLinkEnd {
		t.Fatalf("accept error = %v", err)
	}
	if _, err := s.Write([]byte{1}); err == nil {
		t.Fatal("a stream outlived its session")
	}
}
