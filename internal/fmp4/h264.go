package fmp4

import (
	"errors"
	"fmt"
)

// NAL unit types we care about when turning the capture chip's Annex-B
// elementary stream into fMP4 samples.
const (
	nalTypeNonIDR  = 1
	nalTypeIDR     = 5
	nalTypeSEI     = 6
	nalTypeSPS     = 7
	nalTypePPS     = 8
	nalTypeAUD     = 9
	nalTypeFillerD = 12
)

var errShortSPS = errors.New("fmp4: SPS too short")

// splitAnnexB returns the NAL units contained in an Annex-B access unit,
// without their start codes. The returned slices alias buf.
//
// The RV1106 encoder emits one complete access unit per VENC pack with
// 4-byte start codes, but 3-byte start codes are accepted too so the parser
// does not depend on that detail.
func splitAnnexB(buf []byte) [][]byte {
	var nalus [][]byte

	start := -1
	i := 0
	for i+2 < len(buf) {
		if buf[i] != 0 || buf[i+1] != 0 {
			// No start code can begin here; skip ahead. A start code needs
			// two zero bytes, so the next candidate is after the non-zero.
			if buf[i+1] != 0 {
				i += 2
			} else {
				i++
			}
			continue
		}

		var scLen int
		switch {
		case buf[i+2] == 1:
			scLen = 3
		case i+3 < len(buf) && buf[i+2] == 0 && buf[i+3] == 1:
			scLen = 4
		default:
			i++
			continue
		}

		if start >= 0 {
			if nalu := trimTrailingZeros(buf[start:i]); len(nalu) > 0 {
				nalus = append(nalus, nalu)
			}
		}
		i += scLen
		start = i
	}

	if start >= 0 && start < len(buf) {
		if nalu := trimTrailingZeros(buf[start:]); len(nalu) > 0 {
			nalus = append(nalus, nalu)
		}
	}

	return nalus
}

// trimTrailingZeros drops trailing_zero_8bits that some encoders pad between
// NAL units. Leaving them in would inflate the sample and, for the last NALU
// in an access unit, confuse strict decoders.
func trimTrailingZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

func nalType(nalu []byte) byte {
	if len(nalu) == 0 {
		return 0
	}
	return nalu[0] & 0x1f
}

// IsKeyframe reports whether an Annex-B access unit carries an IDR slice,
// i.e. whether a decoder can start here. The frame fan-out uses it to resume
// cleanly after dropping frames on a congested link.
func IsKeyframe(au []byte) bool {
	for _, nalu := range splitAnnexB(au) {
		if nalType(nalu) == nalTypeIDR {
			return true
		}
	}
	return false
}

// unescapeRBSP removes emulation prevention bytes (the 0x03 in 00 00 03)
// so the result can be read as a plain RBSP bit stream.
func unescapeRBSP(nalu []byte) []byte {
	out := make([]byte, 0, len(nalu))
	zeros := 0
	for _, b := range nalu {
		if zeros == 2 && b == 0x03 {
			zeros = 0
			continue
		}
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, b)
	}
	return out
}

// bitReader reads an H.264 RBSP bit by bit.
type bitReader struct {
	buf []byte
	pos int // bit position
}

func (r *bitReader) bit() (uint, error) {
	if r.pos >= len(r.buf)*8 {
		return 0, errShortSPS
	}
	b := (r.buf[r.pos/8] >> (7 - uint(r.pos%8))) & 1
	r.pos++
	return uint(b), nil
}

func (r *bitReader) bits(n int) (uint, error) {
	var v uint
	for i := 0; i < n; i++ {
		b, err := r.bit()
		if err != nil {
			return 0, err
		}
		v = v<<1 | b
	}
	return v, nil
}

// ue reads an unsigned Exp-Golomb coded value.
func (r *bitReader) ue() (uint, error) {
	lead := 0
	for {
		b, err := r.bit()
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		lead++
		if lead > 32 {
			return 0, errors.New("fmp4: invalid Exp-Golomb code")
		}
	}
	if lead == 0 {
		return 0, nil
	}
	rest, err := r.bits(lead)
	if err != nil {
		return 0, err
	}
	return (1 << uint(lead)) - 1 + rest, nil
}

// se reads a signed Exp-Golomb coded value.
func (r *bitReader) se() (int, error) {
	v, err := r.ue()
	if err != nil {
		return 0, err
	}
	if v%2 == 0 {
		return -int(v / 2), nil
	}
	return int((v + 1) / 2), nil
}

// SPSInfo is the subset of the sequence parameter set the muxer needs to
// build the sample entry.
type SPSInfo struct {
	ProfileIdc    byte
	ConstraintSet byte
	LevelIdc      byte
	Width         int
	Height        int
}

// parseSPS extracts dimensions and profile/level from an SPS NAL unit
// (including its one-byte NAL header).
func parseSPS(nalu []byte) (SPSInfo, error) {
	var info SPSInfo

	if len(nalu) < 4 {
		return info, errShortSPS
	}

	rbsp := unescapeRBSP(nalu)
	if len(rbsp) < 4 {
		return info, errShortSPS
	}

	info.ProfileIdc = rbsp[1]
	info.ConstraintSet = rbsp[2]
	info.LevelIdc = rbsp[3]

	r := &bitReader{buf: rbsp, pos: 4 * 8} // skip NAL header + profile/constraint/level

	if _, err := r.ue(); err != nil { // seq_parameter_set_id
		return info, err
	}

	chromaFormatIdc := uint(1) // 4:2:0 when not present
	separateColourPlane := uint(0)

	switch info.ProfileIdc {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		var err error
		if chromaFormatIdc, err = r.ue(); err != nil {
			return info, err
		}
		if chromaFormatIdc == 3 {
			if separateColourPlane, err = r.bit(); err != nil {
				return info, err
			}
		}
		if _, err = r.ue(); err != nil { // bit_depth_luma_minus8
			return info, err
		}
		if _, err = r.ue(); err != nil { // bit_depth_chroma_minus8
			return info, err
		}
		if _, err = r.bit(); err != nil { // qpprime_y_zero_transform_bypass_flag
			return info, err
		}
		scalingMatrix, err := r.bit()
		if err != nil {
			return info, err
		}
		if scalingMatrix == 1 {
			lists := 8
			if chromaFormatIdc == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				present, err := r.bit()
				if err != nil {
					return info, err
				}
				if present == 0 {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := 8, 8
				for j := 0; j < size; j++ {
					if next != 0 {
						delta, err := r.se()
						if err != nil {
							return info, err
						}
						next = (last + delta + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}

	if _, err := r.ue(); err != nil { // log2_max_frame_num_minus4
		return info, err
	}

	picOrderCntType, err := r.ue()
	if err != nil {
		return info, err
	}
	switch picOrderCntType {
	case 0:
		if _, err := r.ue(); err != nil { // log2_max_pic_order_cnt_lsb_minus4
			return info, err
		}
	case 1:
		if _, err := r.bit(); err != nil { // delta_pic_order_always_zero_flag
			return info, err
		}
		if _, err := r.se(); err != nil { // offset_for_non_ref_pic
			return info, err
		}
		if _, err := r.se(); err != nil { // offset_for_top_to_bottom_field
			return info, err
		}
		cycle, err := r.ue()
		if err != nil {
			return info, err
		}
		for i := uint(0); i < cycle; i++ {
			if _, err := r.se(); err != nil {
				return info, err
			}
		}
	}

	if _, err := r.ue(); err != nil { // max_num_ref_frames
		return info, err
	}
	if _, err := r.bit(); err != nil { // gaps_in_frame_num_value_allowed_flag
		return info, err
	}

	widthMbsMinus1, err := r.ue()
	if err != nil {
		return info, err
	}
	heightMapUnitsMinus1, err := r.ue()
	if err != nil {
		return info, err
	}
	frameMbsOnly, err := r.bit()
	if err != nil {
		return info, err
	}
	if frameMbsOnly == 0 {
		if _, err := r.bit(); err != nil { // mb_adaptive_frame_field_flag
			return info, err
		}
	}
	if _, err := r.bit(); err != nil { // direct_8x8_inference_flag
		return info, err
	}

	var cropLeft, cropRight, cropTop, cropBottom uint
	cropping, err := r.bit()
	if err != nil {
		return info, err
	}
	if cropping == 1 {
		if cropLeft, err = r.ue(); err != nil {
			return info, err
		}
		if cropRight, err = r.ue(); err != nil {
			return info, err
		}
		if cropTop, err = r.ue(); err != nil {
			return info, err
		}
		if cropBottom, err = r.ue(); err != nil {
			return info, err
		}
	}

	width := int(widthMbsMinus1+1) * 16
	height := int(2-frameMbsOnly) * int(heightMapUnitsMinus1+1) * 16

	// Crop units depend on the chroma sampling format (7-4.2.1 in the spec).
	cropUnitX, cropUnitY := 1, int(2-frameMbsOnly)
	if separateColourPlane == 0 && chromaFormatIdc != 0 {
		subWidthC, subHeightC := 2, 2 // 4:2:0
		switch chromaFormatIdc {
		case 3: // 4:4:4
			subWidthC, subHeightC = 1, 1
		case 2: // 4:2:2
			subHeightC = 1
		}
		cropUnitX = subWidthC
		cropUnitY = subHeightC * int(2-frameMbsOnly)
	}

	width -= int(cropLeft+cropRight) * cropUnitX
	height -= int(cropTop+cropBottom) * cropUnitY

	if width <= 0 || height <= 0 {
		return info, fmt.Errorf("fmp4: implausible SPS dimensions %dx%d", width, height)
	}

	info.Width = width
	info.Height = height
	return info, nil
}
