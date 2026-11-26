// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package cc

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"puffer-webrtc-v2/internal/pion/ntp"
)

// TwccExtensionAttributesKey identifies the TWCC value in the attribute collection
// so we don't need to reparse.
const TwccExtensionAttributesKey = iota

var (
	errMissingTWCCExtension = errors.New("missing transport layer cc header extension")
	errInvalidFeedback      = errors.New("invalid feedback")
)

// HistoryWindow is how long a sent packet is remembered so that feedback can
// be matched to it.
//
// Changed from pion/interceptor v0.1.49, which kept the last 250 packets
// whatever their age: at 2500 packets per second that is 100 ms, less than the
// time feedback takes to come back, and the packets it had already forgotten
// were then reported as lost. A window of time holds at any rate (libwebrtc
// keeps 60 s); 10 s of 2500 packets per second is 25,000 small entries, and it
// stays below the 65,536 packets after which sequence numbers repeat up to
// about 6,500 packets per second (past that a newer packet replaces the older
// one with the same number).
const HistoryWindow = 10 * time.Second

// FeedbackAdapter converts incoming RTCP Packets (TWCC and RFC8888) into Acknowledgments.
// Acknowledgments are the common format that Congestion Controllers in Pion understand.
type FeedbackAdapter struct {
	lock    sync.Mutex
	history *feedbackHistory
}

// NewFeedbackAdapter returns a new FeedbackAdapter.
func NewFeedbackAdapter() *FeedbackAdapter {
	return &FeedbackAdapter{history: newFeedbackHistory(HistoryWindow)}
}

func (f *FeedbackAdapter) onSentRFC8888(ts time.Time, header *rtp.Header, size int) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.history.add(Acknowledgment{
		SequenceNumber: header.SequenceNumber,
		SSRC:           header.SSRC,
		Size:           size,
		Departure:      ts,
		Arrival:        time.Time{},
		ECN:            0,
	})

	return nil
}

func (f *FeedbackAdapter) onSentTWCC(ts time.Time, extID uint8, header *rtp.Header, size int) error {
	sequenceNumber := header.GetExtension(extID)
	var tccExt rtp.TransportCCExtension
	err := tccExt.Unmarshal(sequenceNumber)
	if err != nil {
		return errMissingTWCCExtension
	}

	f.lock.Lock()
	defer f.lock.Unlock()
	f.history.add(Acknowledgment{
		SequenceNumber: tccExt.TransportSequence,
		SSRC:           0,
		Size:           header.MarshalSize() + size,
		Departure:      ts,
		Arrival:        time.Time{},
		ECN:            0,
	})

	return nil
}

// OnSent records that and when an outgoing packet was sent for later mapping to
// acknowledgments.
func (f *FeedbackAdapter) OnSent(ts time.Time, header *rtp.Header, size int, attributes interceptor.Attributes) error {
	hdrExtensionID := attributes.Get(TwccExtensionAttributesKey)
	id, ok := hdrExtensionID.(uint8)
	if ok && hdrExtensionID != 0 {
		return f.onSentTWCC(ts, id, header, size)
	}

	return f.onSentRFC8888(ts, header, size)
}

// OnTransportCCFeedback converts incoming TWCC RTCP packet feedback to
// Acknowledgments.
//
// Changed from pion/interceptor v0.1.49 (see HistoryWindow for the other half):
//   - A status for a packet that is not in the history is skipped. Upstream
//     returned an empty Acknowledgment for it, which the loss estimator counted
//     as a lost packet (zero arrival time) and the delay estimator as a packet
//     sent at time zero. Its receive delta is still consumed, so the arrival
//     times of the packets after it stay right (upstream skipped the delta too).
//   - Only PacketStatusCount statuses are read. A status vector chunk always
//     carries 7 or 14 symbols, and the ones past the count are filler that reads
//     as "not received"; upstream reported the packets sent just after the
//     feedback's range as lost.
func (f *FeedbackAdapter) OnTransportCCFeedback(
	_ time.Time, feedback *rtcp.TransportLayerCC,
) ([]Acknowledgment, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	result := []Acknowledgment{}
	sequenceNumber := feedback.BaseSequenceNumber
	refTime := time.Time{}.Add(time.Duration(feedback.ReferenceTime) * 64 * time.Millisecond)
	recvDeltas := feedback.RecvDeltas
	remaining := int(feedback.PacketStatusCount)

	status := func(symbol uint16) error {
		received := symbol != rtcp.TypeTCCPacketNotReceived
		if received {
			if len(recvDeltas) == 0 {
				return errInvalidFeedback
			}
			refTime = refTime.Add(time.Duration(recvDeltas[0].Delta) * time.Microsecond)
			recvDeltas = recvDeltas[1:]
		}
		if ack, ok := f.history.get(feedbackHistoryKey{ssrc: 0, sequenceNumber: sequenceNumber}); ok {
			if received {
				ack.Arrival = refTime
			}
			result = append(result, ack)
		}
		sequenceNumber++
		remaining--

		return nil
	}

	for _, chunk := range feedback.PacketChunks {
		switch chunk := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for i := 0; i < int(chunk.RunLength) && remaining > 0; i++ {
				if err := status(chunk.PacketStatusSymbol); err != nil {
					return nil, err
				}
			}
		case *rtcp.StatusVectorChunk:
			for i := 0; i < len(chunk.SymbolList) && remaining > 0; i++ {
				if err := status(chunk.SymbolList[i]); err != nil {
					return nil, err
				}
			}
		default:
			return nil, errInvalidFeedback
		}
	}

	return result, nil
}

// OnRFC8888Feedback converts incoming Congestion Control Feedback RTCP packet
// to Acknowledgments.
func (f *FeedbackAdapter) OnRFC8888Feedback(_ time.Time, feedback *rtcp.CCFeedbackReport) []Acknowledgment {
	f.lock.Lock()
	defer f.lock.Unlock()

	result := []Acknowledgment{}
	referenceTime := ntp.ToTime(uint64(feedback.ReportTimestamp) << 16)
	for _, rb := range feedback.ReportBlocks {
		for i, mb := range rb.MetricBlocks {
			sequenceNumber := rb.BeginSequence + uint16(i) //nolint:gosec // G115
			key := feedbackHistoryKey{
				ssrc:           rb.MediaSSRC,
				sequenceNumber: sequenceNumber,
			}
			if ack, ok := f.history.get(key); ok {
				if mb.Received {
					delta := time.Duration((float64(mb.ArrivalTimeOffset) / 1024.0) * float64(time.Second))
					ack.Arrival = referenceTime.Add(-delta)
					ack.ECN = mb.ECN
				}
				result = append(result, ack)
			}
		}
	}

	return result
}

type feedbackHistoryKey struct {
	ssrc           uint32
	sequenceNumber uint16
}

// feedbackHistory holds the packets sent in the last window, newest at the
// front of evictList.
type feedbackHistory struct {
	window    time.Duration
	evictList *list.List
	items     map[feedbackHistoryKey]*list.Element
}

func newFeedbackHistory(window time.Duration) *feedbackHistory {
	return &feedbackHistory{
		window:    window,
		evictList: list.New(),
		items:     make(map[feedbackHistoryKey]*list.Element),
	}
}

func (f *feedbackHistory) get(key feedbackHistoryKey) (Acknowledgment, bool) {
	ent, ok := f.items[key]
	if ok {
		if ack, ok := ent.Value.(Acknowledgment); ok {
			return ack, true
		}
	}

	return Acknowledgment{}, false
}

func (f *feedbackHistory) add(ack Acknowledgment) {
	key := feedbackHistoryKey{
		ssrc:           ack.SSRC,
		sequenceNumber: ack.SequenceNumber,
	}
	// Check for existing
	if ent, ok := f.items[key]; ok {
		f.evictList.MoveToFront(ent)
		ent.Value = ack

		return
	}
	// Add new
	ent := f.evictList.PushFront(ack)
	f.items[key] = ent
	// Evict what has left the window
	for ent := f.evictList.Back(); ent != nil; ent = f.evictList.Back() {
		oldest, ok := ent.Value.(Acknowledgment)
		if ok && ack.Departure.Sub(oldest.Departure) <= f.window {
			break
		}
		f.removeOldest()
	}
}

func (f *feedbackHistory) removeOldest() {
	if ent := f.evictList.Back(); ent != nil {
		f.evictList.Remove(ent)
		if ack, ok := ent.Value.(Acknowledgment); ok {
			key := feedbackHistoryKey{
				ssrc:           ack.SSRC,
				sequenceNumber: ack.SequenceNumber,
			}
			delete(f.items, key)
		}
	}
}
