package fmp4

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bitWriter is an independent Exp-Golomb encoder used to synthesise SPS NAL
// units for the tests. Writing the bitstream with different code than the
// parser reads it with is the point: a shared helper would hide a mistake in
// the bit ordering.
type bitWriter struct {
	buf  []byte
	cur  byte
	nBit uint
}

func (w *bitWriter) bit(v uint) {
	w.cur = w.cur<<1 | byte(v&1)
	w.nBit++
	if w.nBit == 8 {
		w.buf = append(w.buf, w.cur)
		w.cur, w.nBit = 0, 0
	}
}

func (w *bitWriter) bits(v uint, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bit(v >> uint(i))
	}
}

// ue writes an unsigned Exp-Golomb value: the value+1 in binary, prefixed by
// one fewer zero than its bit length.
func (w *bitWriter) ue(v uint) {
	code := v + 1
	n := 0
	for t := code; t > 1; t >>= 1 {
		n++
	}
	for i := 0; i < n; i++ {
		w.bit(0)
	}
	w.bits(code, n+1)
}

// flush emits rbsp_trailing_bits: a stop bit then zero padding to a byte.
func (w *bitWriter) flush() []byte {
	w.bit(1)
	for w.nBit != 0 {
		w.bit(0)
	}
	return w.buf
}

type spsParams struct {
	profileIdc    byte
	constraintSet byte
	levelIdc      byte
	widthMbsM1    uint
	heightMapM1   uint
	frameMbsOnly  uint
	chromaFormat  uint
	cropL         uint
	cropR         uint
	cropT         uint
	cropB         uint
}

// buildSPS synthesises a syntactically valid SPS NAL unit, header byte
// included.
func buildSPS(p spsParams) []byte {
	w := &bitWriter{}
	w.ue(0) // seq_parameter_set_id

	isHighProfile := false
	switch p.profileIdc {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		isHighProfile = true
	}
	if isHighProfile {
		w.ue(p.chromaFormat)
		w.ue(0)  // bit_depth_luma_minus8
		w.ue(0)  // bit_depth_chroma_minus8
		w.bit(0) // qpprime_y_zero_transform_bypass_flag
		w.bit(0) // seq_scaling_matrix_present_flag
	}

	w.ue(0) // log2_max_frame_num_minus4
	w.ue(0) // pic_order_cnt_type == 0
	w.ue(0) // log2_max_pic_order_cnt_lsb_minus4
	w.ue(1) // max_num_ref_frames
	w.bit(0)

	w.ue(p.widthMbsM1)
	w.ue(p.heightMapM1)
	w.bit(p.frameMbsOnly)
	if p.frameMbsOnly == 0 {
		w.bit(0) // mb_adaptive_frame_field_flag
	}
	w.bit(1) // direct_8x8_inference_flag

	cropping := p.cropL|p.cropR|p.cropT|p.cropB != 0
	if cropping {
		w.bit(1)
		w.ue(p.cropL)
		w.ue(p.cropR)
		w.ue(p.cropT)
		w.ue(p.cropB)
	} else {
		w.bit(0)
	}
	w.bit(0) // vui_parameters_present_flag

	payload := w.flush()

	nalu := []byte{0x67, p.profileIdc, p.constraintSet, p.levelIdc}
	return append(nalu, payload...)
}

func TestParseSPSDimensions(t *testing.T) {
	tests := []struct {
		name       string
		params     spsParams
		wantWidth  int
		wantHeight int
	}{
		{
			// 1080p is the interesting case: 1080 is not a multiple of 16,
			// so the encoder codes 1088 lines and crops 4 chroma rows off
			// the bottom (4 * cropUnitY(2) == 8 luma lines).
			name: "1920x1080 high profile with bottom crop",
			params: spsParams{
				profileIdc: 100, levelIdc: 40, chromaFormat: 1,
				widthMbsM1: 119, heightMapM1: 67, frameMbsOnly: 1,
				cropB: 4,
			},
			wantWidth: 1920, wantHeight: 1080,
		},
		{
			name: "1280x720 high profile no crop",
			params: spsParams{
				profileIdc: 100, levelIdc: 31, chromaFormat: 1,
				widthMbsM1: 79, heightMapM1: 44, frameMbsOnly: 1,
			},
			wantWidth: 1280, wantHeight: 720,
		},
		{
			// Baseline skips the chroma_format_idc block entirely; the
			// parser has to not read those fields or everything after
			// shifts.
			name: "640x480 baseline profile",
			params: spsParams{
				profileIdc: 66, levelIdc: 30,
				widthMbsM1: 39, heightMapM1: 29, frameMbsOnly: 1,
			},
			wantWidth: 640, wantHeight: 480,
		},
		{
			name: "1920x1080 with left/right crop",
			params: spsParams{
				profileIdc: 100, levelIdc: 40, chromaFormat: 1,
				widthMbsM1: 119, heightMapM1: 67, frameMbsOnly: 1,
				cropL: 2, cropR: 2, cropB: 4,
			},
			wantWidth: 1912, wantHeight: 1080,
		},
		{
			// Interlaced: height is doubled and cropUnitY doubles with it.
			name: "interlaced 720x480",
			params: spsParams{
				profileIdc: 100, levelIdc: 30, chromaFormat: 1,
				widthMbsM1: 44, heightMapM1: 14, frameMbsOnly: 0,
			},
			wantWidth: 720, wantHeight: 480,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sps := buildSPS(tt.params)
			info, err := parseSPS(sps)
			require.NoError(t, err)

			assert.Equal(t, tt.wantWidth, info.Width, "width")
			assert.Equal(t, tt.wantHeight, info.Height, "height")
			assert.Equal(t, tt.params.profileIdc, info.ProfileIdc)
			assert.Equal(t, tt.params.levelIdc, info.LevelIdc)
		})
	}
}

func TestParseSPSRejectsGarbage(t *testing.T) {
	_, err := parseSPS([]byte{0x67, 0x64})
	assert.Error(t, err, "a truncated SPS must not be accepted")

	_, err = parseSPS(nil)
	assert.Error(t, err)
}

func TestSplitAnnexB(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want [][]byte
	}{
		{
			name: "four byte start codes",
			in:   []byte{0, 0, 0, 1, 0x67, 0xaa, 0, 0, 0, 1, 0x68, 0xbb},
			want: [][]byte{{0x67, 0xaa}, {0x68, 0xbb}},
		},
		{
			name: "three byte start codes",
			in:   []byte{0, 0, 1, 0x67, 0xaa, 0, 0, 1, 0x65, 0xbb},
			want: [][]byte{{0x67, 0xaa}, {0x65, 0xbb}},
		},
		{
			name: "mixed start code lengths",
			in:   []byte{0, 0, 0, 1, 0x67, 0xaa, 0, 0, 1, 0x65, 0xbb},
			want: [][]byte{{0x67, 0xaa}, {0x65, 0xbb}},
		},
		{
			name: "trailing zero padding is stripped",
			in:   []byte{0, 0, 0, 1, 0x65, 0xaa, 0, 0, 0},
			want: [][]byte{{0x65, 0xaa}},
		},
		{
			name: "payload containing a zero byte survives",
			in:   []byte{0, 0, 0, 1, 0x65, 0x00, 0xaa},
			want: [][]byte{{0x65, 0x00, 0xaa}},
		},
		{
			name: "no start code yields nothing",
			in:   []byte{0x65, 0xaa, 0xbb},
			want: nil,
		},
		{
			name: "empty input",
			in:   nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitAnnexB(tt.in)
			require.Equal(t, len(tt.want), len(got), "NALU count")
			for i := range tt.want {
				assert.Equal(t, tt.want[i], got[i], "NALU %d", i)
			}
		})
	}
}

func TestUnescapeRBSP(t *testing.T) {
	// 00 00 03 01 -> 00 00 01: the 0x03 is an emulation prevention byte.
	assert.Equal(t,
		[]byte{0x67, 0x00, 0x00, 0x01, 0xaa},
		unescapeRBSP([]byte{0x67, 0x00, 0x00, 0x03, 0x01, 0xaa}))

	// A 0x03 that is not preceded by two zeros is real data.
	assert.Equal(t,
		[]byte{0x67, 0x01, 0x03, 0xaa},
		unescapeRBSP([]byte{0x67, 0x01, 0x03, 0xaa}))

	// Consecutive escapes.
	assert.Equal(t,
		[]byte{0x00, 0x00, 0x00, 0x00, 0x00},
		unescapeRBSP([]byte{0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00}))
}

func TestNALType(t *testing.T) {
	assert.Equal(t, byte(nalTypeSPS), nalType([]byte{0x67}))
	assert.Equal(t, byte(nalTypePPS), nalType([]byte{0x68}))
	assert.Equal(t, byte(nalTypeIDR), nalType([]byte{0x65}))
	assert.Equal(t, byte(nalTypeNonIDR), nalType([]byte{0x41}))
	assert.Equal(t, byte(0), nalType(nil))
}
