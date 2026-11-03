package probe

import (
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// sizeRecorder stands in for GCC: it records the payload size it accounts for,
// then forwards to the downstream writer like SendSideBWE does.
type sizeRecorder struct{ sizes []int }

func (r *sizeRecorder) AddStream(_ *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(h *rtp.Header, p []byte, a interceptor.Attributes) (int, error) {
		r.sizes = append(r.sizes, len(p))
		return w.Write(h, p, a)
	})
}
func (r *sizeRecorder) WriteRTCP([]rtcp.Packet, interceptor.Attributes) error { return nil }
func (r *sizeRecorder) GetTargetBitrate() int                                 { return 0 }
func (r *sizeRecorder) OnTargetBitrateChange(func(int))                       {}
func (r *sizeRecorder) GetStats() map[string]any                              { return nil }
func (r *sizeRecorder) Close() error                                          { return nil }

var _ cc.BandwidthEstimator = (*sizeRecorder)(nil)

type wire struct {
	headers  []rtp.Header
	payloads [][]byte
}

func (w *wire) Write(h *rtp.Header, p []byte, _ interceptor.Attributes) (int, error) {
	w.headers = append(w.headers, *h)
	w.payloads = append(w.payloads, p)
	return len(p), nil
}

func TestPaddingProbes(t *testing.T) {
	rec := &sizeRecorder{}
	out := &wire{}
	wr := WithPaddingProbes(rec).AddStream(&interceptor.StreamInfo{MimeType: "video/H264"}, out)
	media := []byte{0x7c, 0x85, 1, 2, 3} // FU-A
	_, _ = wr.Write(&rtp.Header{SequenceNumber: 1}, media, nil)
	_, _ = wr.Write(&rtp.Header{SequenceNumber: 2}, Payload(200), nil)

	if len(rec.sizes) != 2 || rec.sizes[0] != 5 || rec.sizes[1] != 200 {
		t.Fatalf("estimator saw sizes %v, want [5 200]", rec.sizes)
	}
	if out.headers[0].Padding || len(out.payloads[0]) != 5 {
		t.Errorf("media packet altered: %+v %x", out.headers[0], out.payloads[0])
	}
	h := out.headers[1]
	if !h.Padding || h.PaddingSize != 200 || len(out.payloads[1]) != 0 {
		t.Errorf("probe not converted: padding=%v size=%d payload=%d", h.Padding, h.PaddingSize, len(out.payloads[1]))
	}
	// On the wire it must be a valid padding-only packet.
	buf := make([]byte, 1500)
	n, err := rtp.MarshalPacketTo(buf, &h, nil) //nolint:staticcheck // same call SRTP makes
	if err != nil {
		t.Fatal(err)
	}
	var back rtp.Packet
	if err := back.Unmarshal(buf[:n]); err != nil || len(back.Payload) != 0 || back.PaddingSize != 200 {
		t.Errorf("round trip: err=%v payload=%d padding=%d", err, len(back.Payload), back.PaddingSize)
	}

	// Audio is never converted.
	rec2, out2 := &sizeRecorder{}, &wire{}
	aw := WithPaddingProbes(rec2).AddStream(&interceptor.StreamInfo{MimeType: "audio/opus"}, out2)
	_, _ = aw.Write(&rtp.Header{}, []byte{0, 1, 2}, nil)
	if out2.headers[0].Padding || len(out2.payloads[0]) != 3 {
		t.Error("opus payload starting with 0x00 was converted")
	}
}
