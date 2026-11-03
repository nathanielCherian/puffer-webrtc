// Package probe implements padding probes for senders that cannot raise their
// own media rate.
//
// A sender that forwards fixed-rate simulcast layers can't raise its own rate to
// test the network, and Pion's GCC never lets its estimate grow past 1.5x the
// rate it sees acknowledged. So a viewer parked on a low layer would never find
// out that a higher one fits. The fix, as in libwebrtc, is to send padding up to
// the estimate.
//
// RTP padding-only packets are the right thing on the wire (receivers discard
// them), but Pion's GCC sizes packets by len(payload) and would count them as
// empty. So probes are written as an ordinary payload of zero bytes (no real H.264
// RTP payload starts with 0x00, whose NAL type 0 is unused), counted by GCC like
// media, and turned into padding-only packets just after GCC, before SRTP.
// Ported from github.com/nathanielCherian/webrtc-test (internal/ratecontrol).
package probe

import (
	"strings"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtp"
)

// MaxBytes is the largest probe (RTP padding length is one byte).
const MaxBytes = 255

var zeroProbe = make([]byte, MaxBytes)

// Payload returns an n-byte probe payload (1..MaxBytes).
func Payload(n int) []byte {
	return zeroProbe[:max(1, min(n, MaxBytes))]
}

// IsPayload reports whether payload is a probe written with Payload.
func IsPayload(payload []byte) bool {
	return len(payload) > 0 && len(payload) <= MaxBytes && payload[0] == 0
}

// IsH264 reports whether a stream can carry probes: only H.264 streams do (an
// Opus payload may legitimately start with 0x00).
func IsH264(mimeType string) bool { return strings.EqualFold(mimeType, "video/H264") }

// PaddingProbes wraps a bandwidth estimator so probe payloads count towards its
// rate measurements and leave as RTP padding.
type PaddingProbes struct {
	cc.BandwidthEstimator
}

// WithPaddingProbes wraps e.
func WithPaddingProbes(e cc.BandwidthEstimator) *PaddingProbes { return &PaddingProbes{e} }

// Unwrap returns the wrapped estimator.
func (p *PaddingProbes) Unwrap() cc.BandwidthEstimator { return p.BandwidthEstimator }

// AddStream converts probes after the estimator has seen them.
func (p *PaddingProbes) AddStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if !IsH264(info.MimeType) {
		return p.BandwidthEstimator.AddStream(info, writer)
	}
	return p.BandwidthEstimator.AddStream(info, interceptor.RTPWriterFunc(
		func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
			if !IsPayload(payload) {
				return writer.Write(header, payload, attributes)
			}
			h := *header
			h.Padding = true
			h.PaddingSize = byte(len(payload))
			return writer.Write(&h, nil, attributes)
		}))
}
