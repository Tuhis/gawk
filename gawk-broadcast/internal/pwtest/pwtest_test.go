package pwtest

import "testing"

// pw-dump prints a second array when registry events land before it exits.
// Decoding that with a single json.Unmarshal fails on the trailing bytes, and
// the failure surfaces as whichever test happened to call Dump.
func TestDecodeDumpReadsEveryBatch(t *testing.T) {
	const twoBatches = `[{"id":33,"type":"PipeWire:Interface:Node"}]
[{"id":41,"type":"PipeWire:Interface:Link"}]`

	objs, err := decodeDump([]byte(twoBatches))
	if err != nil {
		t.Fatalf("decoding two batches: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want the object from each batch: %+v", len(objs), objs)
	}
	if objs[0].ID != 33 || objs[1].ID != 41 {
		t.Errorf("got ids %d and %d, want 33 and 41", objs[0].ID, objs[1].ID)
	}
}

// An id in a later batch is the same object's newer state. Appending both would
// leave FindNode answering with the stale one and LinksInto counting one link
// twice — a graph assertion that is wrong rather than merely noisy.
func TestDecodeDumpKeepsTheNewestStateOfAnID(t *testing.T) {
	const restated = `[{"id":33,"type":"PipeWire:Interface:Node","info":{"props":{"node.name":"before"}}}]
[{"id":33,"type":"PipeWire:Interface:Node","info":{"props":{"node.name":"after"}}}]`

	objs, err := decodeDump([]byte(restated))
	if err != nil {
		t.Fatalf("decoding a restated id: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want the id folded into one: %+v", len(objs), objs)
	}
	if got := propStr(objs[0].Info.Props, "node.name"); got != "after" {
		t.Errorf("node.name is %q, want the newer %q", got, "after")
	}
}

func TestDecodeDumpReportsGarbage(t *testing.T) {
	if _, err := decodeDump([]byte(`[{"id":33}] not json`)); err == nil {
		t.Fatal("decoding trailing garbage succeeded, want an error the harness can print")
	}
}
