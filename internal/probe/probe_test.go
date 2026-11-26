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

// parseOneByte reads an RFC 8285 one-byte extension block by the RFC's rules
// (independently of pion): it returns the elements and the count of padding
// bytes (value 0) between and after them.
func parseOneByte(t *testing.T, block []byte) (elems map[uint8][]byte, padding int) {
	t.Helper()
	elems = map[uint8][]byte{}
	for i := 0; i < len(block); {
		if block[i] == 0 {
			padding++
			i++
			continue
		}
		id, n := block[i]>>4, int(block[i]&0x0f)+1
		if id == 15 {
			t.Fatalf("reserved id 15 at offset %d", i)
		}
		if i+1+n > len(block) {
			t.Fatalf("element %d overruns the block at offset %d", id, i)
		}
		elems[id] = block[i+1 : i+1+n]
		i += 1 + n
	}
	return elems, padding
}

func TestFillerOnTheWire(t *testing.T) {
	for _, tc := range []struct{ probe, filler, pad int }{
		{200, 0, 200}, {255, 0, 255}, {256, 256, 0}, {600, 512, 88}, {MaxBytes, MaxBytes, 0},
	} {
		rec, out := &sizeRecorder{}, &wire{}
		wr := WithPaddingProbes(rec).AddStream(&interceptor.StreamInfo{MimeType: "video/H264"}, out)
		in := rtp.Header{Version: 2, PayloadType: 102, SequenceNumber: 7, Timestamp: 9000, SSRC: 0xabcdef01}
		twcc, mid := []byte{0x12, 0x34}, []byte{'1'}
		if err := in.SetExtension(3, twcc); err != nil {
			t.Fatal(err)
		}
		if err := in.SetExtension(5, mid); err != nil {
			t.Fatal(err)
		}
		before := in.MarshalSize()
		if _, err := wr.Write(&in, Payload(tc.probe), nil); err != nil {
			t.Fatal(err)
		}
		if rec.sizes[0] != tc.probe {
			t.Fatalf("probe %d: estimator saw %d bytes", tc.probe, rec.sizes[0])
		}
		if in.MarshalSize() != before {
			t.Fatalf("probe %d: the caller's header was modified", tc.probe)
		}
		h := out.headers[0]
		if len(out.payloads[0]) != 0 || h.Padding != (tc.pad > 0) || int(h.PaddingSize) != tc.pad {
			t.Fatalf("probe %d: payload=%d padding=%v/%d, want 0 and %d", tc.probe, len(out.payloads[0]), h.Padding, h.PaddingSize, tc.pad)
		}
		buf := make([]byte, 1500)
		n, err := rtp.MarshalPacketTo(buf, &h, nil) //nolint:staticcheck // same call SRTP makes
		if err != nil {
			t.Fatal(err)
		}
		if n != h.MarshalSize()+tc.pad {
			t.Fatalf("probe %d: wrote %d bytes, MarshalSize says %d + %d", tc.probe, n, h.MarshalSize(), tc.pad)
		}
		// Whatever the split, the packet grew by the probe (rounded up to the
		// block's 32-bit words).
		if grew := n - before; grew < tc.probe || grew > tc.probe+3 {
			t.Fatalf("probe %d: packet grew by %d bytes", tc.probe, grew)
		}
		pkt := buf[:n]
		if pkt[0]&0x10 == 0 || pkt[12] != 0xBE || pkt[13] != 0xDE {
			t.Fatalf("probe %d: not a one-byte extension header: % x", tc.probe, pkt[:16])
		}
		words := int(pkt[14])<<8 | int(pkt[15])
		block := pkt[16 : 16+4*words]
		if len(pkt) != 16+len(block)+tc.pad {
			t.Fatalf("probe %d: %d bytes after the extension block, want %d of padding", tc.probe, len(pkt)-16-len(block), tc.pad)
		}
		elems, padding := parseOneByte(t, block)
		if len(elems) != 2 || string(elems[3]) != string(twcc) || string(elems[5]) != string(mid) {
			t.Fatalf("probe %d: extensions %v", tc.probe, elems)
		}
		if padding < tc.filler || padding > tc.filler+3 {
			t.Fatalf("probe %d: %d padding bytes in the block, want %d", tc.probe, padding, tc.filler)
		}
		// And pion reads back the same packet.
		var back rtp.Packet
		if err := back.Unmarshal(pkt); err != nil {
			t.Fatalf("probe %d: %v", tc.probe, err)
		}
		if len(back.Payload) != 0 || int(back.Header.PaddingSize) != tc.pad || back.SequenceNumber != 7 ||
			string(back.GetExtension(3)) != string(twcc) || string(back.GetExtension(5)) != string(mid) {
			t.Fatalf("probe %d: round trip %+v", tc.probe, back.Header)
		}
	}
}
