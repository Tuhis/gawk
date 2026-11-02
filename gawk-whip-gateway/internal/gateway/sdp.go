package gateway

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
)

// offer is what the gateway takes from an acceptable WHIP offer (D5): one
// H.264 packetization-mode 1 video layer and, optionally, Opus.
type offer struct {
	video webrtc.RTPCodecParameters
	audio *webrtc.RTPCodecParameters
	// sps and pps come from the video fmtp's sprop-parameter-sets, if any.
	sps, pps []byte
}

// refusal is a 406 the gateway records (D9).
type refusal struct {
	reason string
	codec  string
}

var errBadSDP = errors.New("gateway: unparsable SDP offer")

// gawk's feedback: NACK (D6) and PLI (D7's two events). No transport-cc and
// no REMB: OBS has no congestion control to feed (§3.3).
var videoFeedback = []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}

// inspectOffer checks an offer against D5 and picks the codecs to answer
// with. It refuses simulcast (several video m-sections, RIDs, a=simulcast,
// an SSRC SIM group) and an offer without H.264 packetization-mode 1, naming
// HEVC or AV1 when that is what the sender offered instead.
func inspectOffer(raw []byte) (offer, *refusal, error) {
	var sd sdp.SessionDescription
	if err := sd.UnmarshalString(string(raw)); err != nil {
		return offer{}, nil, errBadSDP
	}
	var o offer
	videoSections := 0
	var video *sdp.MediaDescription
	for _, md := range sd.MediaDescriptions {
		if md.MediaName.Port.Value == 0 {
			continue // rejected by the offerer itself
		}
		switch md.MediaName.Media {
		case "video":
			videoSections++
			video = md
			for _, a := range md.Attributes {
				switch a.Key {
				case "rid", "simulcast":
					return offer{}, &refusal{reason: status.ReasonSimulcast}, nil
				case "ssrc-group":
					if strings.HasPrefix(a.Value, "SIM ") {
						return offer{}, &refusal{reason: status.ReasonSimulcast}, nil
					}
				}
			}
		case "audio":
			if o.audio == nil {
				if c, ok := findCodec(md, "opus", nil); ok {
					c.MimeType = webrtc.MimeTypeOpus
					c.ClockRate = 48000
					c.Channels = 2
					o.audio = &c
				}
			}
		}
	}
	if videoSections > 1 {
		return offer{}, &refusal{reason: status.ReasonSimulcast}, nil
	}
	if video == nil {
		return offer{}, &refusal{reason: status.ReasonCodec}, nil
	}
	c, ok := findCodec(video, "h264", func(fmtp map[string]string) bool {
		return fmtp["packetization-mode"] == "1"
	})
	if !ok {
		ref := &refusal{reason: status.ReasonCodec}
		if _, hevc := findCodec(video, "h265", nil); hevc {
			ref.codec = status.CodecHEVC
		} else if _, av1 := findCodec(video, "av1", nil); av1 {
			ref.codec = status.CodecAV1
		}
		return offer{}, ref, nil
	}
	c.MimeType = webrtc.MimeTypeH264
	c.ClockRate = 90000
	c.RTCPFeedback = videoFeedback
	o.video = c
	if sprop := parseFmtp(c.SDPFmtpLine)["sprop-parameter-sets"]; sprop != "" {
		parts := strings.Split(sprop, ",")
		if len(parts) >= 2 {
			sps, err1 := base64.StdEncoding.DecodeString(parts[0])
			pps, err2 := base64.StdEncoding.DecodeString(parts[1])
			if err1 == nil && err2 == nil {
				o.sps, o.pps = sps, pps
			}
		}
	}
	return o, nil, nil
}

// findCodec finds the first payload type in md whose encoding name matches
// (case-insensitively) and whose fmtp passes accept.
func findCodec(md *sdp.MediaDescription, name string, accept func(map[string]string) bool) (webrtc.RTPCodecParameters, bool) {
	fmtps := map[string]string{}
	for _, a := range md.Attributes {
		if a.Key == "fmtp" {
			if pt, params, ok := strings.Cut(a.Value, " "); ok {
				fmtps[pt] = strings.TrimSpace(params)
			}
		}
	}
	for _, a := range md.Attributes {
		if a.Key != "rtpmap" {
			continue
		}
		pt, enc, ok := strings.Cut(a.Value, " ")
		if !ok {
			continue
		}
		codec, _, _ := strings.Cut(enc, "/")
		if !strings.EqualFold(codec, name) {
			continue
		}
		n, err := strconv.ParseUint(pt, 10, 8)
		if err != nil {
			continue
		}
		line := fmtps[pt]
		if accept != nil && !accept(parseFmtp(line)) {
			continue
		}
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{SDPFmtpLine: line},
			PayloadType:        webrtc.PayloadType(n),
		}, true
	}
	return webrtc.RTPCodecParameters{}, false
}

func parseFmtp(line string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(line, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		if k != "" {
			m[strings.ToLower(k)] = v
		}
	}
	return m
}
