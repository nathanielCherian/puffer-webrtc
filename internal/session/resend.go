package session

import (
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// resendLogger sits between the NACK responder and the wire and logs every
// packet the responder resends ("retransmit" records in rtcp.jsonl, next to
// the "nack" records that caused them).
//
// Without RTX a resend reuses the original SSRC and sequence number, so a
// packet whose sequence number is not newer than the highest one sent on its
// SSRC is a resend. Each SSRC is written in order by one goroutine, so nothing
// else goes backwards.
type resendLogger struct {
	interceptor.NoOp
	log     func(kind string, t time.Time, data any)
	mu      sync.Mutex
	streams map[uint32]*resendStream
}

type resendStream struct {
	started        bool
	highestSeq     uint16
	packets, bytes int64
}

func newResendLogger(log func(kind string, t time.Time, data any)) *resendLogger {
	return &resendLogger{log: log, streams: map[uint32]*resendStream{}}
}

// NewInterceptor implements interceptor.Factory.
func (r *resendLogger) NewInterceptor(string) (interceptor.Interceptor, error) { return r, nil }

// BindLocalStream wraps the stream's writer to spot resends.
func (r *resendLogger) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	r.mu.Lock()
	st := &resendStream{}
	r.streams[info.SSRC] = st
	r.mu.Unlock()
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		resend := false
		r.mu.Lock()
		switch {
		case !st.started:
			st.started, st.highestSeq = true, header.SequenceNumber
		case int16(header.SequenceNumber-st.highestSeq) > 0:
			st.highestSeq = header.SequenceNumber
		default:
			resend = true
			st.packets++
			st.bytes += int64(header.MarshalSize() + len(payload))
		}
		r.mu.Unlock()
		if resend {
			r.log("retransmit", time.Now(), map[string]any{
				"ssrc": header.SSRC, "seq": header.SequenceNumber, "ts": header.Timestamp, "m": header.Marker,
				"bytes": header.MarshalSize() + len(payload),
			})
		}
		return writer.Write(header, payload, attributes)
	})
}

// Snapshot returns the resend counts per SSRC.
func (r *resendLogger) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{}
	for ssrc, st := range r.streams {
		out[itoa(ssrc)] = map[string]any{"packets": st.packets, "bytes": st.bytes}
	}
	return out
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
