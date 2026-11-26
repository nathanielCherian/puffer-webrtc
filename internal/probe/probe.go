// Package probe implements padding probes for senders that cannot raise their
// own media rate.
//
// A sender that forwards fixed-rate simulcast layers can't raise its own rate to
// test the network, and Pion's GCC never lets its estimate grow past 1.5x the
// rate it sees acknowledged. So a viewer parked on a low layer would never find
// out that a higher one fits. The fix, as in libwebrtc, is to send padding up to
// the estimate.
//
// Receivers must discard the probes, and RTP has two places for bytes a
// receiver skips:
//
//   - RTP padding (RFC 3550): the P bit and a one-byte length at the end of the
//     packet, so at most 255 bytes per packet.
//   - Header-extension filler (RFC 8285 section 4.1.1): the extension block has
//     a 16-bit length in 32-bit words, and bytes of value 0 between or after its
//     elements are padding the receiver skips. That allows probes of a full
//     packet, so the same padding rate costs a quarter of the packets.
//
// A probe of n bytes leaves as a packet with no payload, n/256*256 bytes of
// extension filler and the remaining n%256 bytes as RTP padding; so anything
// up to 255 bytes is a classic padding-only packet.
//
// Pion's GCC sizes packets by len(payload) and would count either as empty. So
// probes are written as an ordinary payload of zero bytes (no real H.264 RTP
// payload starts with 0x00, whose NAL type 0 is unused), counted by GCC like
// media, and converted just after GCC, before SRTP.
// Ported from github.com/nathanielCherian/webrtc-test (internal/ratecontrol).
package probe

import (
	"strings"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtp"
)

const (
	// MaxPadding is the most RTP padding one packet can carry (its length is
	// one byte).
	MaxPadding = 255
	// FillerUnit is the granularity of header-extension filler; see withFiller.
	FillerUnit = 256
	// MaxBytes is the largest probe: with the headers it stays within the
	// 1200-byte packets the media uses.
	MaxBytes = 4 * FillerUnit
)

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
// rate measurements and leave as extension filler and RTP padding.
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
			h, filler := withFiller(header, len(payload)/FillerUnit*FillerUnit)
			if pad := min(len(payload)-filler, MaxPadding); pad > 0 {
				h.Padding, h.PaddingSize = true, byte(pad)
			}
			return writer.Write(&h, nil, attributes)
		}))
}

// withFiller returns a copy of h whose extension block carries n more bytes of
// RFC 8285 padding (n a multiple of FillerUnit), and how many bytes it added:
// 0 when h has no one-byte extension block to put them in, in which case the
// caller is left with RTP padding alone.
//
// rtp.Header has no way to ask for padding in the block, but it writes a
// one-byte element as (id<<4 | uint8(len)-1) followed by the whole payload.
// So the filler is appended to the first element's payload: a multiple of 256
// leaves the length nibble of that element as it was, and a receiver reads the
// element and then n zero bytes, which is padding. The remaining elements
// follow it. TestFillerOnTheWire parses the result by the RFC's rules.
func withFiller(h *rtp.Header, n int) (rtp.Header, int) {
	out := *h
	ids := h.GetExtensionIDs()
	if n <= 0 || n%FillerUnit != 0 || !h.Extension || h.ExtensionProfile != rtp.ExtensionProfileOneByte || len(ids) == 0 {
		return out, 0
	}
	first := h.GetExtension(ids[0])
	if len(first) == 0 || len(first) > 16 {
		return out, 0
	}
	buf := make([]byte, len(first)+n)
	copy(buf, first)
	// SetExtension only checks sizes when the header already has extensions.
	out.Extension, out.ExtensionProfile, out.Extensions = false, 0, nil
	_ = out.SetExtension(ids[0], buf)
	out.ExtensionProfile = rtp.ExtensionProfileOneByte
	for _, id := range ids[1:] {
		if err := out.SetExtension(id, h.GetExtension(id)); err != nil {
			return *h, 0
		}
	}
	return out, n
}
