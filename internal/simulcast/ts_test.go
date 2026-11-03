package simulcast

import (
	"os"
	"testing"
)

// testdata/low_head.ts is the first 2 s of the sfu-test transcoder's 320x180 @ 250 kbps
// rendition: Constrained Baseline H.264 (IDR every 2 s, 60 fps) plus AAC.

func demuxFile(t *testing.T, drop func(i int) bool) (*Demuxer, []PES) {
	t.Helper()
	data, err := os.ReadFile("testdata/low_head.ts")
	if err != nil {
		t.Fatal(err)
	}
	var out []PES
	d := NewDemuxer(func(p PES) { out = append(out, p) })
	for i := 0; i+tsPacketSize <= len(data); i += tsPacketSize {
		if drop != nil && drop(i/tsPacketSize) {
			continue
		}
		if err := d.Feed(data[i : i+tsPacketSize]); err != nil {
			t.Fatalf("packet %d: %v", i/tsPacketSize, err)
		}
	}
	return d, out
}

func TestDemuxTranscoderOutput(t *testing.T) {
	d, pes := demuxFile(t, nil)
	var video, audio []PES
	for _, p := range pes {
		switch p.StreamType {
		case streamTypeH264:
			video = append(video, p)
		case streamTypeAAC:
			audio = append(audio, p)
		}
	}
	// The last video PES is only emitted when the next one starts, so 119 of 120.
	if len(video) != 119 {
		t.Errorf("video PES = %d, want 119", len(video))
	}
	if len(audio) < 80 {
		t.Errorf("audio PES = %d, want >= 80", len(audio))
	}
	if d.CCErrors != 0 || d.DroppedPES != 0 || d.BadPESHeader != 0 {
		t.Errorf("errors: cc=%d dropped=%d bad=%d", d.CCErrors, d.DroppedPES, d.BadPESHeader)
	}
	for i, v := range video {
		if v.PTS != int64(i)*1500 {
			t.Fatalf("video %d PTS = %d, want %d", i, v.PTS, i*1500)
		}
		info := inspectAU(v.Data)
		if info.key != (i == 0) {
			t.Errorf("video %d key = %v", i, info.key)
		}
		if info.mediaBytes <= 0 || info.mediaBytes > len(v.Data) {
			t.Errorf("video %d mediaBytes = %d of %d", i, info.mediaBytes, len(v.Data))
		}
	}
	first := inspectAU(video[0].Data)
	if first.sps == nil {
		t.Fatal("first access unit has no SPS")
	}
	sps, err := ParseSPS(first.sps)
	if err != nil {
		t.Fatal(err)
	}
	if sps.Width != 320 || sps.Height != 180 || sps.Profile != 66 {
		t.Errorf("SPS = %+v, want 320x180 profile 66", sps)
	}
	for i, a := range audio {
		if len(a.Data) < 7 || a.Data[0] != 0xff || a.Data[1]&0xf0 != 0xf0 {
			t.Fatalf("audio %d is not ADTS", i)
		}
	}
}

func TestDemuxDropsDamagedPES(t *testing.T) {
	data, _ := os.ReadFile("testdata/low_head.ts")
	// Drop one video packet that is not the start of a PES.
	victim := -1
	for i := 0; i+tsPacketSize <= len(data); i += tsPacketSize {
		p := data[i:]
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		if pid == 0x100 && p[1]&0x40 == 0 && i/tsPacketSize > 20 {
			victim = i / tsPacketSize
			break
		}
	}
	if victim < 0 {
		t.Skip("no suitable packet")
	}
	d, pes := demuxFile(t, func(i int) bool { return i == victim })
	if d.CCErrors != 1 || d.DroppedPES != 1 {
		t.Errorf("cc=%d dropped=%d, want 1 and 1", d.CCErrors, d.DroppedPES)
	}
	n := 0
	for _, p := range pes {
		if p.StreamType == streamTypeH264 {
			n++
		}
	}
	if n != 118 {
		t.Errorf("video PES = %d, want 118", n)
	}
}

func TestSplitNALs(t *testing.T) {
	b := []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 1, 0x67, 1, 2, 0, 0, 0, 1, 0x65, 3}
	nals := splitNALs(b)
	if len(nals) != 3 || nals[0][0] != 9 || nals[1][0] != 0x67 || len(nals[1]) != 3 || nals[2][0] != 0x65 {
		t.Fatalf("splitNALs = %x", nals)
	}
	info := inspectAU(b)
	if !info.key || info.mediaBytes != 5 {
		t.Errorf("inspectAU = %+v", info)
	}
}

func TestParseLayers(t *testing.T) {
	l, err := ParseLayers("low=127.0.0.1:5003@250k, high=127.0.0.1:5001@2.5M,mid=:5002@800000")
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 3 || l[0].Name != "high" || l[0].BitrateBps != 2_500_000 || l[1].Name != "mid" || l[2].BitrateBps != 250_000 {
		t.Errorf("layers = %+v", l)
	}
	for _, bad := range []string{"", "x", "a=1.2.3.4@1k", "a=:1@zz", "a=:1@1k,a=:2@2k"} {
		if _, err := ParseLayers(bad); err == nil {
			t.Errorf("ParseLayers(%q) succeeded", bad)
		}
	}
}
