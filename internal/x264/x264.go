// Package x264 is a minimal cgo wrapper around libx264 that supports changing
// the target bitrate mid-stream (x264_encoder_reconfig) and forcing IDR frames,
// which is what a WebRTC sender driven by a congestion controller needs.
package x264

/*
#cgo pkg-config: x264
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <x264.h>

typedef struct {
	x264_t *h;
	x264_param_t param;
	x264_picture_t pic_in;
	int64_t pts;
	int width;
	int height;
} enc_t;

static void enc_set_rc(enc_t *e, int kbps) {
	e->param.rc.i_rc_method = X264_RC_ABR;
	e->param.rc.i_bitrate = kbps;
	e->param.rc.i_vbv_max_bitrate = kbps;
	e->param.rc.i_vbv_buffer_size = kbps; // ~1 s of buffer
}

static enc_t *enc_new(int w, int h, int fps_num, int fps_den, int kbps, int keyint, int threads, const char *preset) {
	enc_t *e = calloc(1, sizeof(enc_t));
	if (!e) return NULL;
	if (x264_param_default_preset(&e->param, preset, "zerolatency") < 0) goto fail;
	e->param.i_log_level = X264_LOG_WARNING;
	e->param.i_width = w;
	e->param.i_height = h;
	e->param.i_csp = X264_CSP_I420;
	e->param.i_fps_num = fps_num;
	e->param.i_fps_den = fps_den;
	e->param.i_timebase_num = fps_den;
	e->param.i_timebase_den = fps_num;
	e->param.b_vfr_input = 0;
	e->param.i_threads = threads;
	e->param.i_bframe = 0;
	e->param.b_repeat_headers = 1;
	e->param.b_annexb = 1;
	e->param.i_keyint_max = keyint > 0 ? keyint : X264_KEYINT_MAX_INFINITE;
	e->param.i_keyint_min = 1;
	e->param.i_scenecut_threshold = 0;
	enc_set_rc(e, kbps);
	if (x264_param_apply_profile(&e->param, "baseline") < 0) goto fail;
	e->h = x264_encoder_open(&e->param);
	if (!e->h) goto fail;
	x264_picture_init(&e->pic_in);
	e->pic_in.img.i_csp = X264_CSP_I420;
	e->pic_in.img.i_plane = 3;
	e->pic_in.img.i_stride[0] = w;
	e->pic_in.img.i_stride[1] = w / 2;
	e->pic_in.img.i_stride[2] = w / 2;
	e->width = w;
	e->height = h;
	return e;
fail:
	free(e);
	return NULL;
}

// frame points to a contiguous I420 buffer of size w*h*3/2.
static int enc_encode(enc_t *e, uint8_t *frame, int force_idr, uint8_t **out, int *is_key) {
	int ysz = e->width * e->height;
	e->pic_in.img.plane[0] = frame;
	e->pic_in.img.plane[1] = frame + ysz;
	e->pic_in.img.plane[2] = frame + ysz + ysz / 4;
	e->pic_in.i_pts = e->pts++;
	e->pic_in.i_type = force_idr ? X264_TYPE_IDR : X264_TYPE_AUTO;
	x264_nal_t *nals = NULL;
	int n = 0;
	x264_picture_t pic_out;
	int size = x264_encoder_encode(e->h, &nals, &n, &e->pic_in, &pic_out);
	if (size > 0 && n > 0) {
		// x264 guarantees the payloads of all NALs are contiguous.
		*out = nals[0].p_payload;
		*is_key = pic_out.b_keyframe;
	}
	return size;
}

static int enc_set_bitrate(enc_t *e, int kbps) {
	enc_set_rc(e, kbps);
	return x264_encoder_reconfig(e->h, &e->param);
}

static void enc_close(enc_t *e) {
	if (!e) return;
	if (e->h) x264_encoder_close(e->h);
	free(e);
}

static int x264_build_version(void) { return X264_BUILD; }
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// Config for a new encoder.
type Config struct {
	Width, Height  int
	FPSNum, FPSDen int
	BitrateBps     int
	KeyInt         int // frames between forced IDRs; 0 = only on request
	Threads        int // 0 = x264 auto
	Preset         string
}

// Encoder is safe for concurrent SetBitrate / Encode calls.
type Encoder struct {
	mu      sync.Mutex
	e       *C.enc_t
	cfg     Config
	bitrate int
}

// Frame is one encoded access unit in Annex-B format.
type Frame struct {
	Data     []byte
	Keyframe bool
}

// Build returns the X264_BUILD number of the linked library headers.
func Build() int { return int(C.x264_build_version()) }

// New opens an x264 encoder.
func New(cfg Config) (*Encoder, error) {
	if cfg.Width%2 != 0 || cfg.Height%2 != 0 {
		return nil, fmt.Errorf("x264: dimensions must be even, got %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Preset == "" {
		cfg.Preset = "veryfast"
	}
	preset := C.CString(cfg.Preset)
	defer C.free(unsafe.Pointer(preset))
	kbps := cfg.BitrateBps / 1000
	if kbps < 1 {
		kbps = 1
	}
	e := C.enc_new(C.int(cfg.Width), C.int(cfg.Height), C.int(cfg.FPSNum), C.int(cfg.FPSDen),
		C.int(kbps), C.int(cfg.KeyInt), C.int(cfg.Threads), preset)
	if e == nil {
		return nil, errors.New("x264: failed to open encoder")
	}
	return &Encoder{e: e, cfg: cfg, bitrate: kbps * 1000}, nil
}

// FrameSize is the number of bytes in one I420 input frame.
func (enc *Encoder) FrameSize() int { return enc.cfg.Width * enc.cfg.Height * 3 / 2 }

// Encode encodes one I420 frame. It may return a nil Frame if the encoder
// produced no output for this input.
func (enc *Encoder) Encode(i420 []byte, forceIDR bool) (*Frame, error) {
	if len(i420) < enc.FrameSize() {
		return nil, fmt.Errorf("x264: short frame %d < %d", len(i420), enc.FrameSize())
	}
	enc.mu.Lock()
	defer enc.mu.Unlock()
	if enc.e == nil {
		return nil, errors.New("x264: encoder closed")
	}
	var out *C.uint8_t
	var key C.int
	idr := C.int(0)
	if forceIDR {
		idr = 1
	}
	size := C.enc_encode(enc.e, (*C.uint8_t)(unsafe.Pointer(&i420[0])), idr, &out, &key)
	if size < 0 {
		return nil, errors.New("x264: encode failed")
	}
	if size == 0 || out == nil {
		return nil, nil
	}
	return &Frame{Data: C.GoBytes(unsafe.Pointer(out), size), Keyframe: key != 0}, nil
}

// SetBitrate changes the target (and VBV max) bitrate in bits per second.
func (enc *Encoder) SetBitrate(bps int) error {
	kbps := bps / 1000
	if kbps < 1 {
		kbps = 1
	}
	enc.mu.Lock()
	defer enc.mu.Unlock()
	if enc.e == nil {
		return errors.New("x264: encoder closed")
	}
	if C.enc_set_bitrate(enc.e, C.int(kbps)) < 0 {
		return errors.New("x264: reconfig failed")
	}
	enc.bitrate = kbps * 1000
	return nil
}

// Bitrate returns the currently configured target bitrate in bps.
func (enc *Encoder) Bitrate() int {
	enc.mu.Lock()
	defer enc.mu.Unlock()
	return enc.bitrate
}

// Close releases the encoder.
func (enc *Encoder) Close() {
	enc.mu.Lock()
	defer enc.mu.Unlock()
	C.enc_close(enc.e)
	enc.e = nil
}
