package transport

import (
	"context"
	"io"
	"sync"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// acceptStreams is the edge pump's uni-stream loop: it accepts every stream
// the origin opens and re-ingests it byte-identical (the hub caches and
// re-fans the exact message bytes), at most maxConcurrentKeyframeStreams at
// once. It returns the accept error that ended the loop, after every
// in-flight ingest has finished.
func (es *edgeSession) acceptStreams(ctx context.Context, up edgeUpstream, pub *hub.Publisher) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, maxConcurrentKeyframeStreams)
	for {
		stream, err := up.AcceptUniStream(ctx)
		if err != nil {
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			es.m.log.Warn("edge keyframe stream rejected: too many concurrent", "broadcast_id", es.id)
			continue
		}
		wg.Add(1)
		go func(st io.Reader) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := pub.IngestKeyframeStream(st); err != nil {
				es.m.log.Debug("edge keyframe ingest failed", "broadcast_id", es.id, "err", err)
			}
		}(stream)
	}
}
