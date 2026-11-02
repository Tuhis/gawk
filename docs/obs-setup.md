# Streaming to gawk from OBS Studio

OBS Studio 30 and later can stream to gawk over WHIP, a standard way to send
WebRTC that OBS supports without a plugin. Your scenes, overlays and OBS's
audio mixer come with you, and viewers watch the stream like any other gawk
broadcast. It works on servers that run gawk's WHIP gateway
([self-hosting §16](self-hosting.md#16-obs-and-other-whip-senders-r33)); on
any other, the OBS page says the server doesn't take streams from OBS.

If you don't already use OBS, the browser is simpler: **Start a stream** on
the broadcaster page takes one click and needs no other app.

The design behind this is [docs/83](83-whip-gateway.md).

## 1. Get a stream key

Open the broadcaster page, then **Already stream with OBS Studio?** under the
card (or go straight to `#/broadcast/obs`). Press **Create stream key**. The
page shows:

- **Server**: the WHIP URL, `https://<gateway>/whip`.
- **Bearer Token**: your stream key, `gawk1_<CODE>_<token>`. It works like a
  password: anyone who has it can stream on your code. Keep it off screen,
  since an OBS user's screen is often being captured.
- **Your viewers join on**: the six-character code and its link. The code
  stays the same every time you stream with this key.

The key is kept in this browser, next to the server it belongs to. To use it
on another computer, copy it from OBS (Settings → Stream) and use **Already
have a key? Paste it** on that computer's OBS page. **Reset key** gives you a
new key and a new code; the old key keeps working until the server's operator
bans its code, so reset and ask for a ban if the key leaked.

Keep the OBS page open while you stream. OBS only says "could not connect"
when something is wrong; the page shows why (§5).

## 2. Settings → Stream

| Field | Value |
|---|---|
| Service | `WHIP` |
| Server | the Server from the OBS page |
| Bearer Token | the key from the OBS page |
| Simulcast | `Off`, if your OBS shows it |

gawk takes one video layer. With simulcast on, OBS offers several and the
connection is refused.

## 3. Settings → Output

Set **Output Mode** to `Advanced`: Keyframe Interval is not in Simple mode.

| Field | Value |
|---|---|
| Video Encoder | an H.264 one: NVENC H.264, AMF H.264, QuickSync H.264 or x264 |
| Keyframe Interval | `1 s` (not `0`, which means auto) |
| Audio Encoder | Opus |

gawk viewers decode H.264 from OBS. HEVC and AV1 are refused (OBS shows the
connection failing; the OBS page says which codec it saw). OBS turns
B-frames off and repeats the SPS/PPS before every keyframe for WHIP on its
own, and gawk checks both.

### Why 1 s

A viewer who joins, or who loses a packet, sees a frozen picture until the
next keyframe arrives. gawk's own broadcasters send one every 0.5 s. OBS
can't be asked for a keyframe on demand, so the interval you set is the
longest freeze your viewers get: at 1 s it is up to a second, at 2 s up to two.
`0` (auto) often means 4 s or more, depending on the encoder and frame rate.

The gateway measures the interval from the stream itself (the median of the
last five keyframe spacings):

- **Up to 1.1 s**: fine.
- **Over 1.1 s, up to 2.2 s**: accepted. The OBS page shows an amber warning
  with the measured interval.
- **Over 2.2 s**: gawk ends the stream. Viewers stay on the code and pick the
  stream up when you're back. For the next 30 s gawk turns away OBS's
  automatic reconnect, doubling up to 10 minutes on repeats, so viewers
  aren't looped through freezes. After fixing the setting, press **Retry
  now** on the OBS page, then **Start Streaming** in OBS.

Before five spacings exist, two gaps of more than 4.5 s without a keyframe
also end the stream. A stream that sends B-frames, or that gawk can't read
as H.264, is ended the same way, with the same wait.

### Shorter than 1 s (unverified)

OBS's Keyframe Interval field takes whole seconds, so 1 s is the smallest it
offers. A shorter interval needs the encoder's custom options field. From
OBS's source, the spellings are x264's `keyint=30` (in frames: 0.5 s at
60 fps, 1 s at 30 fps) and NVENC's `gopLength=` / `idrPeriod=`. **These are
unverified**: gawk's lab pass with real OBS (docs/83 WG0) has not yet
confirmed which spelling gives a keyframe-only GOP on each encoder. Until it
has, use 1 s.

## 4. Start Streaming

Press **Start Streaming** in OBS. Within a few seconds the OBS page turns to
"You're live from OBS" and shows what gawk measured: resolution and frame
rate, codec, keyframe interval, and upload rate with loss. **Stop Streaming**
in OBS ends the broadcast; viewers who stay on the code see it again the next
time you start with the same key.

If OBS loses its connection (Wi-Fi, a sleeping laptop), it reconnects on its
own when its automatic reconnect is on, and the code stays the same.

## 5. When it doesn't work

OBS never shows the reason a server gives. The OBS page does:

| The OBS page says | What to do |
|---|---|
| Waiting for OBS… | OBS hasn't connected with this key. Check Service, Server and Bearer Token (§2), then Start Streaming. A key mistyped on paste also looks like this. |
| OBS sends a keyframe every N s (amber) | Settings → Output → Keyframe Interval → `1 s`. It applies the next time you start streaming. |
| gawk ended your stream: OBS sent a keyframe only every N s | Keyframe Interval → `1 s` (§3), then **Retry now**. If it says 0 (auto), that is the cause. |
| …B-frames, which gawk can't play live | Set B-frames to 0 in the encoder's settings, then **Retry now**. |
| gawk couldn't read the video OBS sent | Choose another H.264 encoder in Settings → Output, then **Retry now**. |
| OBS is sending AV1 / HEVC | Settings → Output → Video Encoder → an H.264 one. |
| OBS is sending several layers (simulcast) | Turn Simulcast off in Settings → Stream. |
| This server is streaming as many OBS broadcasts as it can | Wait a minute, then Start Streaming again. |
| This server has no room for another stream right now | Try again in a few minutes. |
| The server didn't accept this key | It may have been reset by whoever runs the server. Reset the key on the OBS page and paste the new one into OBS. |
| This code is in use on another gawk server | Reset the key to get a new code. |
| The server's operator has blocked this stream / ended this stream | Contact whoever runs the server. |
| This server is set up wrong: it can't see where your stream comes from | The server's operator needs to fix its gateway configuration ([self-hosting §16](self-hosting.md#16-obs-and-other-whip-senders-r33)). |
| OBS disconnected | If OBS is still open it reconnects on its own; if not, Start Streaming again. Your code stays the same. |
| The server can't read this key | Forget the key on the OBS page and paste it again from OBS, or reset it. |
| This server doesn't take streams from OBS | The server you have selected runs no WHIP gateway. |

If OBS fails to connect and the page still says "Waiting for OBS…", the
server never saw the stream's media: check that nothing on your network
blocks outgoing UDP to the server. The gateway also accepts ICE over TCP on
the same port, but OBS's WebRTC library is expected to use UDP only (not yet
confirmed with a real OBS, docs/83 WG0).

## 6. Other WHIP senders

Any WHIP sender that sends one H.264 layer with no B-frames and Opus audio
works the same way: the Server and the key as the bearer token.

**FFmpeg 8 or later** (its `whip` muxer). This is the command the dev stack's
sender runs (`gawk-whip-gateway/cmd/gawk-whipsim`), so it is tested:

```sh
ffmpeg -re -f lavfi -i testsrc2=size=1280x720:rate=30 \
       -re -f lavfi -i sine=frequency=440:sample_rate=48000 \
       -c:v libx264 -preset veryfast -tune zerolatency -profile:v high -pix_fmt yuv420p \
       -g 30 -keyint_min 30 -sc_threshold 0 -bf 0 -x264-params repeat-headers=1 \
       -b:v 2500k -maxrate 2500k -bufsize 1250k \
       -c:a libopus -b:a 64k -ar 48000 -ac 2 \
       -f whip -authorization 'gawk1_<CODE>_<token>' https://<gateway>/whip
```

`-g` and `-keyint_min` are the keyframe interval in frames (30 at 30 fps is
1 s), `-sc_threshold 0` keeps x264 from adding keyframes at scene cuts, and
`-bf 0` turns B-frames off. Replace the two `lavfi` inputs with your own
sources.

**GStreamer** (gst-plugins-rs `whipsink`). Not run in gawk's CI; the
property names are the plugin's:

```sh
gst-launch-1.0 \
  videotestsrc is-live=true ! video/x-raw,width=1280,height=720,framerate=30/1 ! videoconvert ! \
  x264enc tune=zerolatency bframes=0 key-int-max=30 bitrate=2500 ! \
  rtph264pay config-interval=-1 ! application/x-rtp,media=video,encoding-name=H264,payload=96 ! \
  whip.sink_0 \
  audiotestsrc is-live=true ! audioconvert ! audioresample ! opusenc ! rtpopuspay ! \
  application/x-rtp,media=audio,encoding-name=OPUS,payload=97 ! whip.sink_1 \
  whipsink name=whip whip-endpoint=https://<gateway>/whip auth-token='gawk1_<CODE>_<token>'
```

## 7. Trying it locally

`docker compose --profile whip up` runs a gateway beside the dev stack's
relay and a sender streaming the FFmpeg test pattern above into the room
`devroom`: open <http://localhost:8080/#/room/devroom>. OBS on the same
machine can stream to it too: Server `http://localhost:8180/whip`, and a key
from the dev stack's OBS page (`http://localhost:8080/#/broadcast/obs`).
[docs/41](41-local-dev-stack.md) has the details.
