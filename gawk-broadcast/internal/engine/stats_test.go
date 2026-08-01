package engine

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// Stats is serialized straight into an R28 telemetry batch (docs/33 TM2), and
// every consumer on the other side — schema.BroadcasterFields, the rollup's
// curated series, all fifteen diagnose() rules, the dip detector, the live
// dashboard — is written in the browser's lowerCamelCase spelling.
//
// Without tags, Go marshals exported field names: the native broadcaster sent
// `EncoderFps` where every reader looked for `encoderFps`, so its samples were
// stored faithfully and then matched nothing. A real session on 2026-07-27 was
// recorded with a full funnel and still diagnosed as `unknown`, because not one
// field it reported could be read — the producer whose findings fill docs/19
// and docs/28 was the one producer invisible to the telemetry built to watch it.
//
// So the JSON key set is a contract, and this pins it. A new field is fine;
// adding it here is the whole cost. A capitalized key is not: that is the bug
// coming back.
func TestStatsJSONKeysAreTheCanonicalSpelling(t *testing.T) {
	b, err := json.Marshal(Stats{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Only fields carrying `omitempty` may be absent from a zero Stats; every
	// other key must always be present, because an absent key and a zero are
	// different claims and the readers cannot tell them apart.
	want := []string{
		"captureFpsAvailable",
		"encodedFrames", "keyframes",
		"keyframeIntervalAvailable", "keyframeIntervalMs",
		"sentFrames", "encoderFps", "sentFps",
		"datagramsSent", "bytesSent", "configsSent",
		"keyframeStreamsSent", "keyframeStreamsFailed",
		"keyframeStreamsSuperseded", "keyframeBytesSent",
		"framesDroppedAtSend",
		"timeSyncAvailable", "timeSyncRttMs", "timeSyncOffsetUs",
		"viewerCountAvailable", "viewerCount",
		"resumes", "resuming",
		"audioPacketsSent", "audioBytesSent", "audioConfigsSent",
		"audioPacketsDropped",
		// R29 forward parity (docs/34): the level the relay advertised, and
		// what this producer actually emitted at it.
		"parityLevel", "parityChunksSent", "parityBytesSent",
	}
	gotKeys := slices.Sorted(maps.Keys(got))
	slices.Sort(want)
	if !slices.Equal(gotKeys, want) {
		t.Errorf("zero-value JSON keys:\n got  %v\n want %v", gotKeys, want)
	}

	// A populated Stats adds the omitempty fields, and the funnel/target names
	// must be exactly the browser's — these are the ones the rules read.
	b, err = json.Marshal(Stats{
		Encoder: "nvh264enc", Codec: "avc1.42E02A",
		Width: 1920, Height: 1080, Fps: 30, BitrateBps: 10_000_000,
		CapturePath: "zero-copy",
		EncoderFps:  30, SentFps: 30,
		AudioState: AudioActive, AudioSource: "pipewire-monitor",
		AudioCodec: "opus", AudioSampleRate: 48000, AudioChannels: 2,
		AudioBitrateBps: 128000,
		// R35: what was shared and whose audio went out.
		ShareMode: "window", AudioApp: "steam_app_12345",
	})
	if err != nil {
		t.Fatalf("marshal populated: %v", err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal populated: %v", err)
	}
	for _, key := range []string{
		"encoder", "codec", "capturePath",
		"targetWidth", "targetHeight", "targetFps", "targetBitrateBps",
		"audioState", "audioSource", "audioCodec", "audioSampleRate",
		"audioChannels", "audioBitrateBps",
		"shareMode", "audioApp",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("populated Stats is missing key %q", key)
		}
	}
	for key := range got {
		if key == "" || (key[0] >= 'A' && key[0] <= 'Z') {
			t.Errorf("key %q is capitalized; telemetry reads lowerCamelCase only", key)
		}
	}
}
