package fetchmodel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetch(t *testing.T) {
	body := []byte("pretend weights")
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/model.onnx":
			w.Write(body)
		case "/swapped.onnx":
			w.Write([]byte("different weights"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "nudenet.onnx")
	ctx := context.Background()

	fetched, err := Fetch(ctx, srv.Client(), srv.URL+"/model.onnx", good, out, 1<<20)
	if err != nil || !fetched {
		t.Fatalf("Fetch = %v, %v", fetched, err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
		t.Fatal("installed file differs")
	}
	// Already present with the right hash: no download.
	if fetched, err := Fetch(ctx, srv.Client(), srv.URL+"/model.onnx", good, out, 1<<20); err != nil || fetched || hits != 1 {
		t.Fatalf("re-run: fetched=%v err=%v hits=%d", fetched, err, hits)
	}

	// CF6: a SHA mismatch fails and installs nothing.
	out2 := filepath.Join(dir, "other.onnx")
	if _, err := Fetch(ctx, srv.Client(), srv.URL+"/swapped.onnx", good, out2, 1<<20); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("swapped file: %v", err)
	}
	if _, err := os.Stat(out2); !os.IsNotExist(err) {
		t.Fatal("an unverified file was installed")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	for _, c := range []struct{ url, sum string }{
		{srv.URL + "/missing.onnx", good},
		{srv.URL + "/model.onnx", "nothex"},
		{srv.URL + "/model.onnx", good[:10]},
	} {
		if _, err := Fetch(ctx, srv.Client(), c.url, c.sum, out2, 1<<20); err == nil {
			t.Errorf("Fetch(%s, %s) succeeded", c.url, c.sum)
		}
	}
	if _, err := Fetch(ctx, srv.Client(), srv.URL+"/model.onnx", good, out2, 4); err == nil {
		t.Error("a download over -max-bytes succeeded")
	}
}

// The chart pins the weights by GitHub's API asset route
// (api.github.com/repos/…/releases/assets/<id>): it answers JSON metadata
// unless asked for application/octet-stream, and then redirects to the
// storage host. fetch-model asks for the bytes and follows the redirect.
func TestFetchAsksForBytesAndFollowsTheAssetRedirect(t *testing.T) {
	body := []byte("pretend weights")
	sum := sha256.Sum256(body)
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/octet-stream" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"name":"320n.onnx","size":15}`))
			return
		}
		http.Redirect(w, r, storage.URL+"/blob", http.StatusFound)
	}))
	defer api.Close()
	out := filepath.Join(t.TempDir(), "nudenet.onnx")
	fetched, err := Fetch(context.Background(), http.DefaultClient, api.URL+"/repos/notAI-tech/NudeNet/releases/assets/1", hex.EncodeToString(sum[:]), out, 1<<20)
	if err != nil || !fetched {
		t.Fatalf("Fetch = %v, %v", fetched, err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
		t.Fatal("installed file is not the asset's bytes")
	}
}

func TestRunRequiresFlags(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"-url", "http://x.invalid/m"}, &out); err == nil {
		t.Fatal("Run without -sha256 and -out succeeded")
	}
}
