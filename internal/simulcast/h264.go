package simulcast

import "errors"

// H.264 NAL unit types used here.
const (
	nalIDR    = 5
	nalSPS    = 7
	nalAUD    = 9
	nalFiller = 12
)

// splitNALs returns the NAL units of an Annex-B byte stream (without start codes).
func splitNALs(b []byte) [][]byte {
	var nals [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		if start >= 0 {
			end := i
			if end > start && b[end-1] == 0 { // 4-byte start code
				end--
			}
			nals = append(nals, b[start:end])
		}
		start = i + 3
		i += 2
	}
	if start >= 0 && start < len(b) {
		nals = append(nals, b[start:])
	}
	return nals
}

// auInfo summarises one access unit.
type auInfo struct {
	key        bool
	mediaBytes int    // NAL bytes that go on the wire (AUD and filler are dropped by the RTP payloader)
	sps        []byte // first SPS, if any
}

func inspectAU(b []byte) auInfo {
	var info auInfo
	for _, n := range splitNALs(b) {
		if len(n) == 0 {
			continue
		}
		switch t := n[0] & 0x1f; t {
		case nalAUD, nalFiller:
			continue
		case nalIDR:
			info.key = true
		case nalSPS:
			if info.sps == nil {
				info.sps = n
			}
		}
		info.mediaBytes += len(n)
	}
	return info
}

// SPSInfo is what we read from a sequence parameter set.
type SPSInfo struct {
	Profile, Level int
	Width, Height  int
}

var errShortSPS = errors.New("h264: truncated SPS")

// ParseSPS decodes profile, level and the cropped picture size from an SPS NAL
// unit (including its one-byte NAL header).
func ParseSPS(nal []byte) (SPSInfo, error) {
	var info SPSInfo
	if len(nal) < 4 {
		return info, errShortSPS
	}
	// Remove emulation prevention bytes (00 00 03 -> 00 00).
	rbsp := make([]byte, 0, len(nal))
	zeros := 0
	for _, c := range nal[1:] {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		rbsp = append(rbsp, c)
	}
	r := &bitReader{b: rbsp}
	info.Profile = int(r.bits(8))
	r.bits(8) // constraint flags
	info.Level = int(r.bits(8))
	r.ue() // seq_parameter_set_id
	chroma := uint(1)
	switch info.Profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = r.ue()
		if chroma == 3 {
			r.bits(1) // separate_colour_plane_flag
		}
		r.ue()              // bit_depth_luma_minus8
		r.ue()              // bit_depth_chroma_minus8
		r.bits(1)           // qpprime_y_zero_transform_bypass_flag
		if r.bits(1) == 1 { // seq_scaling_matrix_present_flag
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.bits(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := 8, 8
					for j := 0; j < size; j++ {
						if next != 0 {
							next = (last + r.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	r.ue()          // log2_max_frame_num_minus4
	switch r.ue() { // pic_order_cnt_type
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.bits(1)
		r.se()
		r.se()
		for n := r.ue(); n > 0 && !r.err; n-- {
			r.se()
		}
	}
	r.ue()    // max_num_ref_frames
	r.bits(1) // gaps_in_frame_num_value_allowed_flag
	wMbs := int(r.ue()) + 1
	hMaps := int(r.ue()) + 1
	frameMbsOnly := int(r.bits(1))
	if frameMbsOnly == 0 {
		r.bits(1) // mb_adaptive_frame_field_flag
	}
	r.bits(1) // direct_8x8_inference_flag
	var cl, cr, ct, cb int
	if r.bits(1) == 1 { // frame_cropping_flag
		cl, cr, ct, cb = int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
	}
	if r.err {
		return info, errShortSPS
	}
	cropX, cropY := 1, 2-frameMbsOnly
	switch chroma {
	case 1:
		cropX, cropY = 2, 2*(2-frameMbsOnly)
	case 2:
		cropX = 2
	}
	info.Width = wMbs*16 - cropX*(cl+cr)
	info.Height = (2-frameMbsOnly)*hMaps*16 - cropY*(ct+cb)
	return info, nil
}

type bitReader struct {
	b   []byte
	pos int // in bits
	err bool
}

func (r *bitReader) bits(n int) uint {
	var v uint
	for i := 0; i < n; i++ {
		if r.pos >= len(r.b)*8 {
			r.err = true
			return 0
		}
		v = v<<1 | uint(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

// ue reads an unsigned Exp-Golomb code.
func (r *bitReader) ue() uint {
	zeros := 0
	for r.bits(1) == 0 {
		if r.err || zeros > 31 {
			r.err = true
			return 0
		}
		zeros++
	}
	return (1<<zeros - 1) + r.bits(zeros)
}

// se reads a signed Exp-Golomb code.
func (r *bitReader) se() int {
	k := r.ue()
	if k%2 == 1 {
		return int((k + 1) / 2)
	}
	return -int(k / 2)
}
