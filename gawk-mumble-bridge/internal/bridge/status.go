package bridge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// RoomKey is how a room appears in logs and /statusz (D24): the first 12 hex
// characters of HMAC-SHA256(-stats-key, code), never the code, which is a
// joinable secret.
func RoomKey(statsKey []byte, code string) string {
	m := hmac.New(sha256.New, statsKey)
	m.Write([]byte(code))
	return hex.EncodeToString(m.Sum(nil))[:12]
}

// Status is the /statusz document.
type Status struct {
	Version          string                 `json:"version"`
	Ready            bool                   `json:"ready"`
	Draining         bool                   `json:"draining"`
	VoiceSessions    int                    `json:"voiceSessions"`
	MaxVoiceSessions int                    `json:"maxVoiceSessions"`
	Observers        int                    `json:"observers"`
	TempChannels     int                    `json:"tempChannels"`
	MaxTempChannels  int                    `json:"maxTempChannels"`
	Rooms            map[string]*RoomStatus `json:"rooms"`
	MumbleFailures   map[string]uint64      `json:"mumbleFailures"`
	LastFailure      *FailureStatus         `json:"lastMumbleFailure,omitempty"`
}

// RoomStatus is one room, keyed by RoomKey.
type RoomStatus struct {
	VoiceSessions int             `json:"voiceSessions"`
	Observer      *ObserverStatus `json:"observer,omitempty"`
}

// ObserverStatus is a room's observer as this pod runs it.
type ObserverStatus struct {
	Temporary bool   `json:"temporary"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Users     int    `json:"users"`
}

// FailureStatus is the most recent Mumble failure.
type FailureStatus struct {
	Reason string    `json:"reason"`
	Text   string    `json:"text"`
	Room   string    `json:"room,omitempty"`
	At     time.Time `json:"at"`
}

// Status snapshots the service.
func (s *Server) Status() Status {
	st := Status{
		Version:          s.opts.Version,
		Ready:            s.Ready(),
		Draining:         s.draining.Load(),
		MaxVoiceSessions: s.opts.MaxVoiceSessions,
		MaxTempChannels:  s.opts.MaxTempChannels,
		Rooms:            map[string]*RoomStatus{},
		MumbleFailures:   map[string]uint64{},
	}
	room := func(k string) *RoomStatus {
		r := st.Rooms[k]
		if r == nil {
			r = &RoomStatus{}
			st.Rooms[k] = r
		}
		return r
	}
	s.mu.Lock()
	st.VoiceSessions = s.activeSessions
	for vs := range s.sessions {
		if k := vs.roomKey(); k != "" {
			room(k).VoiceSessions++
		}
	}
	st.Observers = len(s.observers)
	st.TempChannels = s.tempChannels
	for ob := range s.observers {
		os := ob.status()
		room(ob.key).Observer = &os
	}
	for r, n := range s.failureCounts {
		st.MumbleFailures[r] = n
	}
	if s.lastFailure != nil {
		lf := *s.lastFailure
		st.LastFailure = &lf
	}
	s.mu.Unlock()
	return st
}

func (s *Server) handleStatusz(w http.ResponseWriter, _ *http.Request) {
	st := s.Status()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(st)
}

// recordFailure counts a Mumble failure (D18).
func (s *Server) recordFailure(reason, roomKey string) {
	s.metrics.failures.WithLabelValues(reason).Inc()
	s.mu.Lock()
	s.failureCounts[reason]++
	s.lastFailure = &FailureStatus{Reason: reason, Text: failureText[reason], Room: roomKey, At: s.now().UTC()}
	s.mu.Unlock()
}
