package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Tuhis/gawk/gawk-server/screening"
)

// R40's calibration data (docs/71 D20–D22): the samples a sampler stores above
// its floor, the per-session count that numbers them, and the shard polls
// that make a missing sampler visible.

// IngestSamples stores one sampler batch (D20) and returns how many samples
// it wrote.
//
// gawk-admin numbers the samples, never the sampler: under the session row's
// lock, a sent sample's sequence number is the session's count before the
// batch plus its position, and the batch's Taken is then added to the count.
// So a restarted or re-homed sampler continues the same numbering, and two
// batches for one session (two replicas, a retry racing a new batch) get
// disjoint numbers. Sessions are locked in epoch order, so two batches naming
// the same sessions cannot deadlock each other.
func (s *Store) IngestSamples(ctx context.Context, b screening.SampleBatch) (int, error) {
	sessions := append([]screening.SessionSamples(nil), b.Sessions...)
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].PublisherEpoch < sessions[j].PublisherEpoch })

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: ingest samples: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored := 0
	for _, sess := range sessions {
		if _, err := tx.Exec(ctx, `INSERT INTO screening_sessions
			(publisher_epoch, broadcast_id, broadcast_key, first_sample_at, last_sample_at, floor, labels, mode, sampler_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (publisher_epoch) DO NOTHING`,
			sess.PublisherEpoch, sess.BroadcastID, sess.BroadcastKey, sess.FirstAt.UTC(), sess.LastAt.UTC(),
			b.Floor, b.Labels, b.Mode, b.SamplerID); err != nil {
			return 0, fmt.Errorf("store: ingest samples: session: %w", err)
		}
		var base int64
		if err := tx.QueryRow(ctx, `SELECT samples_taken FROM screening_sessions WHERE publisher_epoch = $1 FOR UPDATE`,
			sess.PublisherEpoch).Scan(&base); err != nil {
			return 0, fmt.Errorf("store: ingest samples: lock session: %w", err)
		}
		for _, sm := range sess.Samples {
			scores, err := json.Marshal(sm.Scores)
			if err != nil {
				return 0, fmt.Errorf("store: ingest samples: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO screening_samples
				(publisher_epoch, seq, sampled_at, broadcast_id, broadcast_key, scores) VALUES ($1,$2,$3,$4,$5,$6)`,
				sess.PublisherEpoch, base+int64(sm.Position), sm.At.UTC(), sess.BroadcastID, sess.BroadcastKey, scores); err != nil {
				return 0, fmt.Errorf("store: ingest samples: sample: %w", err)
			}
			stored++
		}
		if _, err := tx.Exec(ctx, `UPDATE screening_sessions SET
			samples_taken = samples_taken + $2, samples_stored = samples_stored + $3,
			first_sample_at = LEAST(first_sample_at, $4), last_sample_at = GREATEST(last_sample_at, $5),
			floor = GREATEST(floor, $6), sampler_id = $7, mode = $8
			WHERE publisher_epoch = $1`,
			sess.PublisherEpoch, sess.Taken, len(sess.Samples), sess.FirstAt.UTC(), sess.LastAt.UTC(),
			b.Floor, b.SamplerID, b.Mode); err != nil {
			return 0, fmt.Errorf("store: ingest samples: count: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: ingest samples: %w", err)
	}
	return stored, nil
}

// Replay errors: both are the caller's to fix, so the handler answers 400.
var (
	// ErrReplayBelowFloor: the threshold is under a replayed session's
	// floor, where a missing sample no longer means "below the threshold".
	ErrReplayBelowFloor = errors.New("store: replay threshold is below the sample floor")
	// ErrReplayUnknownLabel: a requested label is in no replayed session.
	ErrReplayUnknownLabel = errors.New("store: replay names a label no stored session has")
)

type replaySession struct {
	epoch, broadcastID, key string
	taken                   int64
	stored                  int64
	first, last             time.Time
	floor                   float64
	labels                  []string
}

// Replay runs a candidate window rule over the stored samples of every
// session that overlaps [From, To) (D21) and counts the trips that fall in
// it. Whole sessions are replayed — a window at the range's start depends on
// the samples before it — and broadcast-hours count only the overlap.
func (s *Store) Replay(ctx context.Context, req screening.ReplayRequest) (screening.ReplayResponse, error) {
	from, to := req.From.UTC(), req.To.UTC()
	out := screening.ReplayResponse{Schema: screening.SchemaReplay, Request: req, Trips: []screening.ReplayTrip{}}

	rows, err := s.pool.Query(ctx, `SELECT publisher_epoch, broadcast_id, broadcast_key, samples_taken, samples_stored,
			first_sample_at, last_sample_at, floor, labels
		FROM screening_sessions WHERE first_sample_at < $2 AND last_sample_at >= $1
		ORDER BY publisher_epoch`, from, to)
	if err != nil {
		return out, fmt.Errorf("store: replay: %w", err)
	}
	var sessions []replaySession
	known := map[string]bool{}
	for rows.Next() {
		var r replaySession
		if err := rows.Scan(&r.epoch, &r.broadcastID, &r.key, &r.taken, &r.stored, &r.first, &r.last, &r.floor, &r.labels); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: replay: %w", err)
		}
		sessions = append(sessions, r)
		for _, l := range r.labels {
			known[l] = true
		}
		if r.floor > out.Floor {
			out.Floor = r.floor
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: replay: %w", err)
	}
	if len(sessions) > 0 && req.Threshold < out.Floor {
		return out, fmt.Errorf("%w: threshold %v, floor %v", ErrReplayBelowFloor, req.Threshold, out.Floor)
	}
	for _, l := range req.Labels {
		if len(sessions) > 0 && !known[l] {
			return out, fmt.Errorf("%w: %q", ErrReplayUnknownLabel, l)
		}
	}

	rule := req.Rule()
	var trips []screening.ReplayTrip
	for _, sess := range sessions {
		labels := req.Labels
		if len(labels) == 0 {
			labels = sess.labels
		}
		stored, err := s.replaySamples(ctx, sess.epoch, labels)
		if err != nil {
			return out, err
		}
		out.Sessions++
		out.SamplesTaken += sess.taken
		out.SamplesStored += sess.stored
		lo, hi := sess.first, sess.last
		if lo.Before(from) {
			lo = from
		}
		if hi.After(to) {
			hi = to
		}
		if hi.After(lo) {
			out.BroadcastHours += hi.Sub(lo).Hours()
		}
		for _, ep := range screening.ReplaySession(rule, sess.taken, stored) {
			if ep.At.Before(from) || !ep.At.Before(to) {
				continue
			}
			trips = append(trips, screening.ReplayTrip{
				BroadcastKey: sess.key, BroadcastID: sess.broadcastID, PublisherEpoch: sess.epoch,
				At: ep.At.UTC(), PeakScore: ep.PeakScore,
			})
		}
	}
	out.WouldTrips = len(trips)
	if out.BroadcastHours > 0 {
		out.WouldTripsPer100h = float64(out.WouldTrips) * 100 / out.BroadcastHours
	}
	sort.Slice(trips, func(i, j int) bool { return trips[i].At.After(trips[j].At) })
	if len(trips) > screening.MaxReplayTrips {
		trips, out.Truncated = trips[:screening.MaxReplayTrips], true
	}
	out.Trips = append(out.Trips, trips...)
	return out, nil
}

func (s *Store) replaySamples(ctx context.Context, epoch string, labels []string) ([]screening.StoredSample, error) {
	rows, err := s.pool.Query(ctx, `SELECT seq, sampled_at, scores FROM screening_samples
		WHERE publisher_epoch = $1 ORDER BY seq`, epoch)
	if err != nil {
		return nil, fmt.Errorf("store: replay samples: %w", err)
	}
	defer rows.Close()
	var out []screening.StoredSample
	for rows.Next() {
		var (
			sm  screening.StoredSample
			raw []byte
		)
		if err := rows.Scan(&sm.Seq, &sm.At, &raw); err != nil {
			return nil, fmt.Errorf("store: replay samples: %w", err)
		}
		var scores map[string]float64
		if err := json.Unmarshal(raw, &scores); err != nil {
			return nil, fmt.Errorf("store: replay samples: %w", err)
		}
		sm.Score = screening.SampleScore(scores, labels)
		out = append(out, sm)
	}
	return out, rows.Err()
}

// RecordShardPoll notes that a sampler polled the target list for shard
// `shard` of `of` (D22).
func (s *Store) RecordShardPoll(ctx context.Context, shard, of int, samplerID string, now time.Time) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO screening_shard_polls (shard, shard_count, sampler_id, polled_at)
		VALUES ($1,$2,$3,$4) ON CONFLICT (shard) DO UPDATE SET
			shard_count = EXCLUDED.shard_count, sampler_id = EXCLUDED.sampler_id, polled_at = EXCLUDED.polled_at`,
		shard, of, samplerID, now.UTC()); err != nil {
		return fmt.Errorf("store: record shard poll: %w", err)
	}
	return nil
}

// ShardStatus is the screening plane's sampler coverage (D22).
type ShardStatus struct {
	// Expected is the `of` of the most recent poll; 0 before any sampler
	// polled at all.
	Expected int
	// Missing are the indices under Expected with no poll at that `of` within
	// the window. Never nil.
	Missing []int
	// LastPoll is the most recent poll, nil before the first.
	LastPoll *time.Time
}

// ShardStatus reads which shards have polled within window of now. Taking the
// newest poll's `of` as the expected count is what lets a resize re-base the
// set: polls at the old count stop counting the moment one at the new count
// arrives.
func (s *Store) ShardStatus(ctx context.Context, now time.Time, window time.Duration) (ShardStatus, error) {
	out := ShardStatus{Missing: []int{}}
	rows, err := s.pool.Query(ctx, `SELECT shard, shard_count, polled_at FROM screening_shard_polls ORDER BY polled_at DESC`)
	if err != nil {
		return out, fmt.Errorf("store: shard status: %w", err)
	}
	defer rows.Close()
	type poll struct {
		shard, of int
		at        time.Time
	}
	var polls []poll
	for rows.Next() {
		var p poll
		if err := rows.Scan(&p.shard, &p.of, &p.at); err != nil {
			return out, fmt.Errorf("store: shard status: %w", err)
		}
		polls = append(polls, p)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: shard status: %w", err)
	}
	if len(polls) == 0 {
		return out, nil
	}
	last := polls[0].at.UTC()
	out.LastPoll = &last
	out.Expected = polls[0].of
	fresh := map[int]bool{}
	cutoff := now.Add(-window)
	for _, p := range polls {
		if p.of == out.Expected && !p.at.Before(cutoff) {
			fresh[p.shard] = true
		}
	}
	for i := 0; i < out.Expected; i++ {
		if !fresh[i] {
			out.Missing = append(out.Missing, i)
		}
	}
	return out, nil
}

// ScreeningPruned counts what PruneScreening removed.
type ScreeningPruned struct {
	Flags, Events, Samples, Sessions int64
}

// PruneScreening deletes calibration data older than the cutoff (D17):
// resolved observe-mode flags, their content_flag.observed rows with those
// rows' deliveries, the samples, and sessions that have not been sampled
// since. A flag-mode flag is the moderation audit trail and is never
// touched — the WHERE says so and a test holds it.
func (s *Store) PruneScreening(ctx context.Context, olderThan time.Time) (ScreeningPruned, error) {
	cutoff := olderThan.UTC()
	var out ScreeningPruned
	tag, err := s.pool.Exec(ctx, `DELETE FROM content_flags
		WHERE mode = 'observe' AND state = 'resolved' AND raised_at < $1`, cutoff)
	if err != nil {
		return out, fmt.Errorf("store: prune screening flags: %w", err)
	}
	out.Flags = tag.RowsAffected()
	// Deliveries go in the same statement as their events: the foreign key
	// is checked at the end of the statement (the PruneActivityEvents rule).
	tag, err = s.pool.Exec(ctx, `WITH doomed AS (
			SELECT id FROM moderation_events WHERE category = 'screening' AND occurred_at < $1
		), deliveries AS (
			DELETE FROM webhook_deliveries WHERE event_id IN (SELECT id FROM doomed)
		)
		DELETE FROM moderation_events WHERE id IN (SELECT id FROM doomed)`, cutoff)
	if err != nil {
		return out, fmt.Errorf("store: prune screening events: %w", err)
	}
	out.Events = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM screening_samples WHERE sampled_at < $1`, cutoff)
	if err != nil {
		return out, fmt.Errorf("store: prune screening samples: %w", err)
	}
	out.Samples = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM screening_sessions WHERE last_sample_at < $1`, cutoff)
	if err != nil {
		return out, fmt.Errorf("store: prune screening sessions: %w", err)
	}
	out.Sessions = tag.RowsAffected()
	// Expired mutes are dead weight; nothing reads them.
	if _, err := s.pool.Exec(ctx, `DELETE FROM screening_mutes WHERE muted_until < $1`, cutoff); err != nil {
		return out, fmt.Errorf("store: prune screening mutes: %w", err)
	}
	return out, nil
}

// ScreeningSamplesBytes is the samples table's on-disk size with its indexes:
// the number §3.1 says to watch on a small CNPG volume.
func (s *Store) ScreeningSamplesBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT pg_total_relation_size('screening_samples')`).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: samples table size: %w", err)
	}
	return n, nil
}
