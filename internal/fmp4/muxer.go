// Package fmp4 turns the H.264 Annex-B elementary stream produced by the
// capture chip into fragmented MP4, so it can be played by Media Source
// Extensions in a browser over an ordinary WebSocket.
//
// Only what a live, single-track, video-only stream needs is implemented:
// one init segment (ftyp+moov) describing the track, then one self-contained
// moof+mdat fragment per access unit. There is no indexing, no edit list and
// no fragment accumulation — a fragment is emitted the moment a frame
// arrives, which is what keeps the end-to-end latency low.
package fmp4

import (
	"errors"
	"fmt"
	"time"
)

// Timescale is the media timescale of the video track. 90 kHz is the usual
// choice for H.264 and divides cleanly into common frame durations.
const Timescale = 90000

// movieTimescale is the timescale of the movie header. It only matters for
// the (always zero) movie duration.
const movieTimescale = 1000

const trackID = 1

// maxSampleDuration caps the duration written into a fragment. The duration
// we are handed is the wall-clock gap since the previous frame, so a stall
// (HDMI re-lock, sleep mode, a paused session) would otherwise write a huge
// gap into the timeline and strand the browser's playback position far
// behind the buffer.
const maxSampleDuration = 500 * time.Millisecond

// minSampleDuration keeps the timeline strictly monotonic when frames arrive
// back-to-back. A zero-duration sample makes Chrome's buffered range
// collapse and playback stall.
const minSampleDuration = time.Millisecond

var (
	// ErrNoParameterSets is returned when a fragment is requested before any
	// SPS/PPS have been seen. The caller should drop frames until the first
	// IDR arrives.
	ErrNoParameterSets = errors.New("fmp4: no SPS/PPS seen yet")

	// ErrEmptySample is returned for an access unit that contains no coded
	// slice — parameter-set-only or SEI-only units carry no picture.
	ErrEmptySample = errors.New("fmp4: access unit contains no coded slice")
)

// Muxer converts a sequence of Annex-B access units into fMP4 fragments.
// It is not safe for concurrent use; the stream fan-out calls it from a
// single goroutine.
type Muxer struct {
	sps     []byte
	pps     []byte
	spsInfo SPSInfo

	seqNum       uint32
	decodeTime   uint64
	initSegment  []byte
	paramsChange bool
}

// NewMuxer returns a muxer awaiting its first parameter sets.
func NewMuxer() *Muxer {
	return &Muxer{seqNum: 1}
}

// Ready reports whether an init segment is available.
func (m *Muxer) Ready() bool { return m.initSegment != nil }

// InitSegment returns the ftyp+moov bytes describing the track. It is only
// valid once Ready reports true. The returned slice must not be modified.
func (m *Muxer) InitSegment() []byte { return m.initSegment }

// Width and Height report the coded dimensions of the current track.
func (m *Muxer) Width() int  { return m.spsInfo.Width }
func (m *Muxer) Height() int { return m.spsInfo.Height }

// Codec returns the RFC 6381 codec string for the current track, suitable
// for MediaSource.isTypeSupported.
func (m *Muxer) Codec() string {
	return fmt.Sprintf("avc1.%02X%02X%02X",
		m.spsInfo.ProfileIdc, m.spsInfo.ConstraintSet, m.spsInfo.LevelIdc)
}

// ParametersChanged reports whether the most recent WriteAccessUnit replaced
// the parameter sets, which means a new init segment was built and the
// consumer has to reset its SourceBuffer. The flag is cleared by this call.
func (m *Muxer) ParametersChanged() bool {
	changed := m.paramsChange
	m.paramsChange = false
	return changed
}

// WriteAccessUnit converts one Annex-B access unit into an fMP4 fragment.
//
// duration is the time elapsed since the previous frame; it is clamped to a
// sane range before being written to the timeline. The returned slice is
// freshly allocated and owned by the caller.
//
// Frames that arrive before the first SPS/PPS return ErrNoParameterSets and
// must be dropped — the browser cannot decode them anyway.
func (m *Muxer) WriteAccessUnit(au []byte, duration time.Duration) ([]byte, error) {
	nalus := splitAnnexB(au)
	if len(nalus) == 0 {
		return nil, ErrEmptySample
	}

	var (
		sample   []byte
		keyframe bool
		hasSlice bool
		newSPS   []byte
		newPPS   []byte
	)

	for _, nalu := range nalus {
		switch nalType(nalu) {
		case nalTypeSPS:
			newSPS = nalu
			continue // carried in avcC, not in the sample
		case nalTypePPS:
			newPPS = nalu
			continue
		case nalTypeAUD, nalTypeFillerD:
			continue // no value to the decoder here
		case nalTypeIDR:
			keyframe = true
			hasSlice = true
		case nalTypeNonIDR:
			hasSlice = true
		case nalTypeSEI:
			// keep: may carry picture timing the decoder wants
		}

		// Annex-B start codes become 4-byte big-endian lengths (AVCC).
		var hdr [4]byte
		n := uint32(len(nalu))
		hdr[0] = byte(n >> 24)
		hdr[1] = byte(n >> 16)
		hdr[2] = byte(n >> 8)
		hdr[3] = byte(n)
		sample = append(sample, hdr[:]...)
		sample = append(sample, nalu...)
	}

	if newSPS != nil && newPPS != nil {
		if err := m.setParameterSets(newSPS, newPPS); err != nil {
			return nil, err
		}
	}

	if m.initSegment == nil {
		return nil, ErrNoParameterSets
	}
	if !hasSlice || len(sample) == 0 {
		return nil, ErrEmptySample
	}

	if duration < minSampleDuration {
		duration = minSampleDuration
	} else if duration > maxSampleDuration {
		duration = maxSampleDuration
	}
	sampleDuration := uint32(duration.Nanoseconds() * Timescale / int64(time.Second))
	if sampleDuration == 0 {
		sampleDuration = 1
	}

	frag := m.fragment(sample, sampleDuration, keyframe)
	m.decodeTime += uint64(sampleDuration)
	m.seqNum++
	return frag, nil
}

// setParameterSets rebuilds the init segment when the SPS or PPS changes.
// Identical parameter sets (every IDR repeats them) are ignored so the
// consumer is not told to reset on every keyframe.
func (m *Muxer) setParameterSets(sps, pps []byte) error {
	if bytesEqual(m.sps, sps) && bytesEqual(m.pps, pps) {
		return nil
	}

	info, err := parseSPS(sps)
	if err != nil {
		return err
	}

	first := m.initSegment == nil

	m.sps = append([]byte(nil), sps...)
	m.pps = append([]byte(nil), pps...)
	m.spsInfo = info
	m.initSegment = m.buildInitSegment()

	// A mid-stream change invalidates everything the browser has buffered.
	// The very first set is not a "change" — there is nothing to reset.
	if !first {
		m.paramsChange = true
		m.decodeTime = 0
		m.seqNum = 1
	}
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *Muxer) buildInitSegment() []byte {
	out := make([]byte, 0, 1024)
	out = append(out, m.ftyp()...)
	out = append(out, m.moov()...)
	return out
}

func (m *Muxer) ftyp() []byte {
	w := &writer{}
	w.str("isom")
	w.u32(0x200)
	for _, brand := range []string{"isom", "iso2", "avc1", "mp41"} {
		w.str(brand)
	}
	return box("ftyp", w.bytes())
}

func (m *Muxer) moov() []byte {
	return box("moov", m.mvhd(), m.trak(), m.mvex())
}

func (m *Muxer) mvhd() []byte {
	w := &writer{}
	w.u32(0) // creation_time
	w.u32(0) // modification_time
	w.u32(movieTimescale)
	// Duration 0 marks the movie as unbounded. Chrome uses this to select
	// its low-delay playback path; a non-zero duration makes it buffer as
	// if the stream were seekable.
	w.u32(0)
	w.u32(0x00010000) // rate 1.0
	w.u16(0x0100)     // volume 1.0
	w.u16(0)          // reserved
	w.zeros(8)        // reserved
	writeMatrix(w)
	w.zeros(24)        // pre_defined
	w.u32(trackID + 1) // next_track_ID
	return fullBox("mvhd", 0, 0, w.bytes())
}

func (m *Muxer) trak() []byte {
	return box("trak", m.tkhd(), m.mdia())
}

func (m *Muxer) tkhd() []byte {
	w := &writer{}
	w.u32(0) // creation_time
	w.u32(0) // modification_time
	w.u32(trackID)
	w.u32(0) // reserved
	w.u32(0) // duration
	w.zeros(8)
	w.u16(0) // layer
	w.u16(0) // alternate_group
	w.u16(0) // volume (0 for video)
	w.u16(0) // reserved
	writeMatrix(w)
	w.u32(uint32(m.spsInfo.Width) << 16) // 16.16 fixed point
	w.u32(uint32(m.spsInfo.Height) << 16)
	// flags: track_enabled | track_in_movie
	return fullBox("tkhd", 0, 0x000003, w.bytes())
}

func (m *Muxer) mdia() []byte {
	return box("mdia", m.mdhd(), m.hdlr(), m.minf())
}

func (m *Muxer) mdhd() []byte {
	w := &writer{}
	w.u32(0) // creation_time
	w.u32(0) // modification_time
	w.u32(Timescale)
	w.u32(0)      // duration
	w.u16(0x55c4) // language "und"
	w.u16(0)      // pre_defined
	return fullBox("mdhd", 0, 0, w.bytes())
}

func (m *Muxer) hdlr() []byte {
	w := &writer{}
	w.u32(0) // pre_defined
	w.str("vide")
	w.zeros(12) // reserved
	w.str("VideoHandler")
	w.u8(0) // null terminator
	return fullBox("hdlr", 0, 0, w.bytes())
}

func (m *Muxer) minf() []byte {
	vmhd := fullBox("vmhd", 0, 1, []byte{0, 0, 0, 0, 0, 0, 0, 0})
	dref := fullBox("dref", 0, 0, func() []byte {
		w := &writer{}
		w.u32(1)                     // entry_count
		w.raw(fullBox("url ", 0, 1)) // flags 1 = data is in this file
		return w.bytes()
	}())
	dinf := box("dinf", dref)
	return box("minf", vmhd, dinf, m.stbl())
}

func (m *Muxer) stbl() []byte {
	empty := func(typ string) []byte {
		return fullBox(typ, 0, 0, []byte{0, 0, 0, 0}) // entry_count 0
	}
	stsz := fullBox("stsz", 0, 0, []byte{
		0, 0, 0, 0, // sample_size
		0, 0, 0, 0, // sample_count
	})
	return box("stbl",
		m.stsd(),
		empty("stts"),
		empty("stsc"),
		stsz,
		empty("stco"),
	)
}

func (m *Muxer) stsd() []byte {
	w := &writer{}
	w.u32(1) // entry_count
	w.raw(m.avc1())
	return fullBox("stsd", 0, 0, w.bytes())
}

func (m *Muxer) avc1() []byte {
	w := &writer{}
	w.zeros(6) // reserved
	w.u16(1)   // data_reference_index
	w.u16(0)   // pre_defined
	w.u16(0)   // reserved
	w.zeros(12)
	w.u16(uint16(m.spsInfo.Width))
	w.u16(uint16(m.spsInfo.Height))
	w.u32(0x00480000) // horizresolution 72 dpi
	w.u32(0x00480000) // vertresolution 72 dpi
	w.u32(0)          // reserved
	w.u16(1)          // frame_count
	w.zeros(32)       // compressorname
	w.u16(0x0018)     // depth
	w.u16(0xffff)     // pre_defined (-1)
	w.raw(m.avcC())
	return box("avc1", w.bytes())
}

func (m *Muxer) avcC() []byte {
	w := &writer{}
	w.u8(1) // configurationVersion
	w.u8(m.spsInfo.ProfileIdc)
	w.u8(m.spsInfo.ConstraintSet)
	w.u8(m.spsInfo.LevelIdc)
	w.u8(0xff) // 6 bits reserved + lengthSizeMinusOne = 3 (4-byte lengths)
	w.u8(0xe1) // 3 bits reserved + numOfSequenceParameterSets = 1
	w.u16(uint16(len(m.sps)))
	w.raw(m.sps)
	w.u8(1) // numOfPictureParameterSets
	w.u16(uint16(len(m.pps)))
	w.raw(m.pps)
	return box("avcC", w.bytes())
}

func (m *Muxer) mvex() []byte {
	w := &writer{}
	w.u32(trackID)
	w.u32(1) // default_sample_description_index
	w.u32(0) // default_sample_duration
	w.u32(0) // default_sample_size
	w.u32(0) // default_sample_flags
	return box("mvex", fullBox("trex", 0, 0, w.bytes()))
}

// Sample flags for trun's first_sample_flags. A sync sample does not depend
// on others and is a random access point; everything else does and is not.
const (
	sampleFlagsKey    = 0x02000000
	sampleFlagsNonKey = 0x01010000
)

func (m *Muxer) fragment(sample []byte, duration uint32, keyframe bool) []byte {
	// trun's data_offset is relative to the start of the moof, so the moof
	// has to be built before the offset is known. Its size does not depend
	// on the offset value, so building it twice is not necessary: the
	// offset is the moof size plus the mdat header.
	moofSize := len(m.moof(sample, duration, keyframe, 0))
	moof := m.moof(sample, duration, keyframe, uint32(moofSize+8))

	out := make([]byte, 0, len(moof)+len(sample)+8)
	out = append(out, moof...)
	out = append(out, box("mdat", sample)...)
	return out
}

func (m *Muxer) moof(sample []byte, duration uint32, keyframe bool, dataOffset uint32) []byte {
	mfhd := func() []byte {
		w := &writer{}
		w.u32(m.seqNum)
		return fullBox("mfhd", 0, 0, w.bytes())
	}()

	tfhd := func() []byte {
		w := &writer{}
		w.u32(trackID)
		// default-base-is-moof: offsets are relative to the moof, which is
		// what every fragmented-MP4 consumer expects for live streams.
		return fullBox("tfhd", 0, 0x020000, w.bytes())
	}()

	tfdt := func() []byte {
		w := &writer{}
		w.u64(m.decodeTime)
		return fullBox("tfdt", 1, 0, w.bytes())
	}()

	trun := func() []byte {
		w := &writer{}
		w.u32(1) // sample_count
		w.u32(dataOffset)
		if keyframe {
			w.u32(sampleFlagsKey)
		} else {
			w.u32(sampleFlagsNonKey)
		}
		w.u32(duration)
		w.u32(uint32(len(sample)))
		// data-offset-present | first-sample-flags-present |
		// sample-duration-present | sample-size-present
		const flags = 0x000001 | 0x000004 | 0x000100 | 0x000200
		return fullBox("trun", 0, flags, w.bytes())
	}()

	traf := box("traf", tfhd, tfdt, trun)
	return box("moof", mfhd, traf)
}
