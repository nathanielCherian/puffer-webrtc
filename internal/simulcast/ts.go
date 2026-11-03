package simulcast

import (
	"errors"
)

// Minimal MPEG-TS demuxer: enough to follow PAT -> PMT and reassemble the PES
// packets of an H.264 and an AAC elementary stream, which is what the
// transcoder's UDP outputs carry.

const tsPacketSize = 188

// Stream types from the PMT that we demux.
const (
	streamTypeH264 = 0x1b
	streamTypeAAC  = 0x0f // ADTS
)

var errNoSync = errors.New("mpeg-ts: missing 0x47 sync byte")

// PES is one reassembled elementary-stream packet.
type PES struct {
	StreamType uint8
	PTS        int64 // 90 kHz, 33-bit as transmitted; -1 if absent
	Data       []byte
}

// Demuxer is fed 188-byte TS packets and calls OnPES for each complete PES.
// It is not safe for concurrent use.
type Demuxer struct {
	OnPES func(PES)

	pmtPID  int // -1 until the PAT names it
	streams map[uint16]uint8
	asm     map[uint16]*pesAssembly

	Packets      int64
	CCErrors     int64 // continuity-counter gaps (lost TS packets)
	DroppedPES   int64 // PES packets discarded because part of them was lost
	BadPESHeader int64
}

type pesAssembly struct {
	buf     []byte
	started bool
	broken  bool
	lastCC  int // -1 = none yet
	total   int // expected PES size from PES_packet_length, 0 = unbounded (video)
}

// NewDemuxer creates a demuxer.
func NewDemuxer(onPES func(PES)) *Demuxer {
	return &Demuxer{OnPES: onPES, pmtPID: -1, streams: map[uint16]uint8{}, asm: map[uint16]*pesAssembly{}}
}

// Feed processes one TS packet (exactly 188 bytes).
func (d *Demuxer) Feed(p []byte) error {
	if len(p) != tsPacketSize || p[0] != 0x47 {
		return errNoSync
	}
	d.Packets++
	if p[1]&0x80 != 0 { // transport_error_indicator
		return nil
	}
	pusi := p[1]&0x40 != 0
	pid := uint16(p[1]&0x1f)<<8 | uint16(p[2])
	afc := (p[3] >> 4) & 3
	cc := int(p[3] & 0x0f)
	off := 4
	discontinuity := false
	if afc == 2 || afc == 3 {
		afLen := int(p[4])
		if afLen > 0 {
			discontinuity = p[5]&0x80 != 0
		}
		off += 1 + afLen
	}
	if afc == 0 || afc == 2 || off >= tsPacketSize {
		return nil // no payload
	}
	payload := p[off:]

	switch {
	case pid == 0:
		if pusi {
			d.parsePAT(payload)
		}
	case int(pid) == d.pmtPID:
		if pusi {
			d.parsePMT(payload)
		}
	default:
		if st, ok := d.streams[pid]; ok {
			d.pes(pid, st, pusi, cc, discontinuity, payload)
		}
	}
	return nil
}

// psiSection returns the section after the pointer field, bounded by section_length
// (without the CRC), or nil if it doesn't fit in this packet.
func psiSection(payload []byte, tableID byte) []byte {
	if len(payload) < 1 {
		return nil
	}
	s := payload[1:]
	if int(payload[0]) >= len(s) {
		return nil
	}
	s = s[payload[0]:]
	if len(s) < 8 || s[0] != tableID {
		return nil
	}
	end := 3 + (int(s[1]&0x0f)<<8 | int(s[2]))
	if end > len(s) || end < 12 {
		return nil
	}
	return s[:end-4]
}

func (d *Demuxer) parsePAT(payload []byte) {
	s := psiSection(payload, 0x00)
	if s == nil {
		return
	}
	for i := 8; i+4 <= len(s); i += 4 {
		program := int(s[i])<<8 | int(s[i+1])
		pid := int(s[i+2]&0x1f)<<8 | int(s[i+3])
		if program != 0 {
			d.pmtPID = pid
			return // first program only
		}
	}
}

func (d *Demuxer) parsePMT(payload []byte) {
	s := psiSection(payload, 0x02)
	if s == nil || len(s) < 12 {
		return
	}
	infoLen := int(s[10]&0x0f)<<8 | int(s[11])
	streams := map[uint16]uint8{}
	for i := 12 + infoLen; i+5 <= len(s); {
		st := s[i]
		pid := uint16(s[i+1]&0x1f)<<8 | uint16(s[i+2])
		esLen := int(s[i+3]&0x0f)<<8 | int(s[i+4])
		if st == streamTypeH264 || st == streamTypeAAC {
			streams[pid] = st
		}
		i += 5 + esLen
	}
	d.streams = streams
}

func (d *Demuxer) pes(pid uint16, st uint8, pusi bool, cc int, discontinuity bool, payload []byte) {
	a := d.asm[pid]
	if a == nil {
		a = &pesAssembly{lastCC: -1}
		d.asm[pid] = a
	}
	if a.lastCC >= 0 && !discontinuity {
		switch {
		case cc == a.lastCC:
			return // duplicate packet
		case cc != (a.lastCC+1)&0x0f:
			d.CCErrors++
			a.broken = true
		}
	}
	a.lastCC = cc

	if pusi {
		if a.started {
			d.emit(st, a) // unbounded (video) PES ends where the next one starts
		}
		a.buf = append(make([]byte, 0, max(len(payload)*8, 4096)), payload...)
		a.started, a.broken, a.total = true, false, 0
		if len(a.buf) >= 6 {
			if n := int(a.buf[4])<<8 | int(a.buf[5]); n > 0 {
				a.total = 6 + n
			}
		}
	} else {
		if !a.started {
			return
		}
		a.buf = append(a.buf, payload...)
	}
	if a.total > 0 && len(a.buf) >= a.total {
		d.emit(st, a)
	}
}

func (d *Demuxer) emit(st uint8, a *pesAssembly) {
	buf, broken := a.buf, a.broken
	a.buf, a.started, a.broken = nil, false, false
	if broken {
		d.DroppedPES++
		return
	}
	if a.total > 0 && len(buf) > a.total {
		buf = buf[:a.total]
	}
	if len(buf) < 9 || buf[0] != 0 || buf[1] != 0 || buf[2] != 1 {
		d.BadPESHeader++
		return
	}
	hdrLen := int(buf[8])
	if 9+hdrLen > len(buf) {
		d.BadPESHeader++
		return
	}
	pts := int64(-1)
	if buf[7]&0x80 != 0 && hdrLen >= 5 {
		pts = parseTimestamp(buf[9:14])
	}
	if d.OnPES != nil {
		d.OnPES(PES{StreamType: st, PTS: pts, Data: buf[9+hdrLen:]})
	}
}

func parseTimestamp(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}
