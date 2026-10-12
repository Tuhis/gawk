// Package fetchmodel is the `fetch-model` subcommand the chart's init
// container runs (docs/71 D5): download the model weights from a pinned URL,
// verify a pinned SHA-256, and only then put the file where the sampler loads
// it. The weights are never in the image, so gawk never redistributes them;
// the hash is what keeps a swapped file from loading.
package fetchmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Run parses args and fetches.
func Run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("gawk-sampler fetch-model", flag.ContinueOnError)
	u := fs.String("url", "", "model download URL (required)")
	sum := fs.String("sha256", "", "expected SHA-256 of the file, hex (required)")
	out := fs.String("out", "", "destination path (required)")
	timeout := fs.Duration("timeout", 10*time.Minute, "download deadline")
	maxBytes := fs.Int64("max-bytes", 1<<30, "refuse a download larger than this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *u == "" || *sum == "" || *out == "" {
		return errors.New("fetch-model: -url, -sha256 and -out are required")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	fetched, err := Fetch(ctx, http.DefaultClient, *u, *sum, *out, *maxBytes)
	if err != nil {
		return err
	}
	if fetched {
		fmt.Fprintf(stdout, "fetched %s, sha256 %s\n", *out, strings.ToLower(*sum))
	} else {
		fmt.Fprintf(stdout, "%s already present, sha256 %s\n", *out, strings.ToLower(*sum))
	}
	return nil
}

// Fetch makes out hold the file at url whose SHA-256 is sum. A file already
// at out with that hash is kept (fetched false). Otherwise the download goes
// to a temporary file beside out and is renamed into place only after the
// hash matches, so out never holds an unverified byte.
func Fetch(ctx context.Context, client *http.Client, url, sum, out string, maxBytes int64) (bool, error) {
	want, err := hex.DecodeString(strings.TrimSpace(sum))
	if err != nil || len(want) != sha256.Size {
		return false, fmt.Errorf("fetch-model: -sha256 %q is not a SHA-256 in hex", sum)
	}
	if got, err := fileSum(out); err == nil && hex.EncodeToString(got) == hex.EncodeToString(want) {
		return false, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("fetch-model: %w", err)
	}
	// The pinned URL is GitHub's API asset route: it serves the bytes only to
	// a client that asks for them, and the asset's JSON metadata otherwise.
	// It answers with a redirect to the storage host, which client follows
	// (the default policy); the hash, not the host, is what is trusted.
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("fetch-model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("fetch-model: %s answered %d", url, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".*")
	if err != nil {
		return false, fmt.Errorf("fetch-model: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBytes+1))
	if err == nil && n > maxBytes {
		err = fmt.Errorf("download larger than %d bytes", maxBytes)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, fmt.Errorf("fetch-model: %w", err)
	}
	if got := h.Sum(nil); hex.EncodeToString(got) != hex.EncodeToString(want) {
		return false, fmt.Errorf("fetch-model: SHA-256 mismatch: got %x, want %x — refusing to install %s", got, want, out)
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return false, fmt.Errorf("fetch-model: %w", err)
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return false, fmt.Errorf("fetch-model: %w", err)
	}
	return true, nil
}

func fileSum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
