package fmp4

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- box walking helpers -------------------------------------------------

type parsedBox struct {
	typ     string
	payload []byte
}

// walkBoxes splits a run of sibling boxes, requiring that they tile the
// input exactly. Any leftover or overrun means a size field is wrong, which
// is the failure mode that makes a browser silently refuse the stream.
func walkBoxes(t *testing.T, buf []byte) []parsedBox {
	t.Helper()

	var boxes []parsedBox
	for off := 0; off < len(buf); {
		require.GreaterOrEqual(t, len(buf)-off, 8, "truncated box header at offset %d", off)

		size := int(binary.BigEndian.Uint32(buf[off : off+4]))
		typ := string(buf[off+4 : off+8])

		require.NotEqual(t, 0, size, "box %q uses size-to-EOF, which this muxer must not emit", typ)
		require.NotEqual(t, 1, size, "box %q uses 64-bit size, which this muxer must not emit", typ)
		require.GreaterOrEqual(t, size, 8, "box %q has impossible size %d", typ, size)
		require.LessOrEqual(t, off+size, len(buf),
			"box %q at offset %d claims size %d, overrunning its %d-byte parent", typ, off, size, len(buf))

		boxes = append(boxes, parsedBox{typ: typ, payload: buf[off+8 : off+size]})
		off += size
	}
	return boxes
}

func findBox(t *testing.T, buf []byte, typ string) []byte {
	t.Helper()
	for _, b := range walkBoxes(t, buf) {
		if b.typ == typ {
			return b.payload
		}
	}
	require.FailNowf(t, "box not found", "no %q box in this container", typ)
	return nil
}

// boxTypes lists the sibling box types in order, for order-sensitive asserts.
func boxTypes(t *testing.T, buf []byte) []string {
	t.Helper()
	var types []string
	for _, b := range walkBoxes(t, buf) {
		types = append(types, b.typ)
	}
	return types
}

// --- stream synthesis ----------------------------------------------------

func annexB(nalus ...[]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

var testPPS = []byte{0x68, 0xee, 0x3c, 0x80}

func testSPS1080p() []byte {
	return buildSPS(spsParams{
		profileIdc: 100, levelIdc: 40, chromaFormat: 1,
		widthMbsM1: 119, heightMapM1: 67, frameMbsOnly: 1, cropB: 4,
	})
}

func testSPS720p() []byte {
	return buildSPS(spsParams{
		profileIdc: 100, levelIdc: 31, chromaFormat: 1,
		widthMbsM1: 79, heightMapM1: 44, frameMbsOnly: 1,
	})
}

// idrAccessUnit is what the encoder emits for a keyframe: parameter sets
// followed by the coded slice.
func idrAccessUnit(sps []byte, payload ...byte) []byte {
	slice := append([]byte{0x65}, payload...)
	return annexB(sps, testPPS, slice)
}

func interAccessUnit(payload ...byte) []byte {
	return annexB(append([]byte{0x41}, payload...))
}

// --- init segment --------------------------------------------------------

func TestInitSegmentStructure(t *testing.T) {
	m := NewMuxer()
	require.False(t, m.Ready(), "a fresh muxer has no init segment")

	_, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa, 0xbb), 33*time.Millisecond)
	require.NoError(t, err)
	require.True(t, m.Ready())

	init := m.InitSegment()
	assert.Equal(t, []string{"ftyp", "moov"}, boxTypes(t, init),
		"an init segment is exactly ftyp followed by moov")

	moov := findBox(t, init, "moov")
	assert.Equal(t, []string{"mvhd", "trak", "mvex"}, boxTypes(t, moov))

	// mvhd duration must be zero: that is what selects Chrome's low-delay
	// path instead of treating the stream as a seekable file.
	mvhd := findBox(t, moov, "mvhd")
	require.GreaterOrEqual(t, len(mvhd), 20)
	assert.Equal(t, uint32(0), binary.BigEndian.Uint32(mvhd[16:20]), "mvhd duration must be 0")
	assert.Equal(t, uint32(movieTimescale), binary.BigEndian.Uint32(mvhd[12:16]))

	trak := findBox(t, moov, "trak")
	assert.Equal(t, []string{"tkhd", "mdia"}, boxTypes(t, trak))

	// tkhd carries the display size as 16.16 fixed point.
	tkhd := findBox(t, trak, "tkhd")
	require.Len(t, tkhd, 4+80)
	assert.Equal(t, uint32(1920<<16), binary.BigEndian.Uint32(tkhd[4+72:4+76]), "tkhd width")
	assert.Equal(t, uint32(1080<<16), binary.BigEndian.Uint32(tkhd[4+76:4+80]), "tkhd height")

	mdia := findBox(t, trak, "mdia")
	assert.Equal(t, []string{"mdhd", "hdlr", "minf"}, boxTypes(t, mdia))

	mdhd := findBox(t, mdia, "mdhd")
	assert.Equal(t, uint32(Timescale), binary.BigEndian.Uint32(mdhd[12:16]), "mdhd timescale")

	minf := findBox(t, mdia, "minf")
	assert.Equal(t, []string{"vmhd", "dinf", "stbl"}, boxTypes(t, minf))

	stbl := findBox(t, mdia, "minf")
	stbl = findBox(t, stbl, "stbl")
	assert.Equal(t, []string{"stsd", "stts", "stsc", "stsz", "stco"}, boxTypes(t, stbl))

	// stsd: version/flags + entry_count, then the sample entry.
	stsd := findBox(t, stbl, "stsd")
	require.GreaterOrEqual(t, len(stsd), 8)
	assert.Equal(t, uint32(1), binary.BigEndian.Uint32(stsd[4:8]), "one sample entry")

	avc1 := findBox(t, stsd[8:], "avc1")
	// The avc1 sample entry has a fixed 78-byte prefix before child boxes.
	require.GreaterOrEqual(t, len(avc1), 78)
	assert.Equal(t, uint16(1920), binary.BigEndian.Uint16(avc1[24:26]), "avc1 width")
	assert.Equal(t, uint16(1080), binary.BigEndian.Uint16(avc1[26:28]), "avc1 height")

	avcC := findBox(t, avc1[78:], "avcC")
	sps := testSPS1080p()
	require.GreaterOrEqual(t, len(avcC), 6)
	assert.Equal(t, byte(1), avcC[0], "configurationVersion")
	assert.Equal(t, byte(100), avcC[1], "AVCProfileIndication")
	assert.Equal(t, byte(40), avcC[3], "AVCLevelIndication")
	assert.Equal(t, byte(0xff), avcC[4], "lengthSizeMinusOne must be 3 (4-byte NALU lengths)")
	assert.Equal(t, byte(0xe1), avcC[5], "exactly one SPS")

	spsLen := int(binary.BigEndian.Uint16(avcC[6:8]))
	require.Equal(t, len(sps), spsLen)
	assert.Equal(t, sps, avcC[8:8+spsLen], "avcC must carry the SPS verbatim")

	ppsOff := 8 + spsLen
	assert.Equal(t, byte(1), avcC[ppsOff], "exactly one PPS")
	ppsLen := int(binary.BigEndian.Uint16(avcC[ppsOff+1 : ppsOff+3]))
	require.Equal(t, len(testPPS), ppsLen)
	assert.Equal(t, testPPS, avcC[ppsOff+3:ppsOff+3+ppsLen], "avcC must carry the PPS verbatim")

	mvex := findBox(t, moov, "mvex")
	assert.Equal(t, []string{"trex"}, boxTypes(t, mvex))
}

func TestCodecString(t *testing.T) {
	m := NewMuxer()
	_, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa), 33*time.Millisecond)
	require.NoError(t, err)

	// profile 100 (0x64), constraint 0x00, level 40 (0x28)
	assert.Equal(t, "avc1.640028", m.Codec())
	assert.Equal(t, 1920, m.Width())
	assert.Equal(t, 1080, m.Height())
}

// --- fragments -----------------------------------------------------------

// trunFields pulls the fields back out of a moof, mirroring the flag layout
// the muxer writes.
type trunFields struct {
	sampleCount      uint32
	dataOffset       uint32
	firstSampleFlags uint32
	duration         uint32
	size             uint32
	baseDecodeTime   uint64
	seqNum           uint32
}

func parseMoof(t *testing.T, moof []byte) trunFields {
	t.Helper()

	assert.Equal(t, []string{"mfhd", "traf"}, boxTypes(t, moof))

	mfhd := findBox(t, moof, "mfhd")
	traf := findBox(t, moof, "traf")
	assert.Equal(t, []string{"tfhd", "tfdt", "trun"}, boxTypes(t, traf))

	tfhd := findBox(t, traf, "tfhd")
	// version/flags word: flags must request default-base-is-moof.
	flags := binary.BigEndian.Uint32(tfhd[0:4]) & 0x00ffffff
	assert.Equal(t, uint32(0x020000), flags, "tfhd must set default-base-is-moof")

	tfdt := findBox(t, traf, "tfdt")
	require.Equal(t, byte(1), tfdt[0], "tfdt must be version 1 (64-bit)")

	trun := findBox(t, traf, "trun")
	trunFlags := binary.BigEndian.Uint32(trun[0:4]) & 0x00ffffff
	require.Equal(t, uint32(0x000001|0x000004|0x000100|0x000200), trunFlags)

	return trunFields{
		sampleCount:      binary.BigEndian.Uint32(trun[4:8]),
		dataOffset:       binary.BigEndian.Uint32(trun[8:12]),
		firstSampleFlags: binary.BigEndian.Uint32(trun[12:16]),
		duration:         binary.BigEndian.Uint32(trun[16:20]),
		size:             binary.BigEndian.Uint32(trun[20:24]),
		baseDecodeTime:   binary.BigEndian.Uint64(tfdt[4:12]),
		seqNum:           binary.BigEndian.Uint32(mfhd[4:8]),
	}
}

// splitFragment returns the moof bytes and the mdat payload.
func splitFragment(t *testing.T, frag []byte) (moof []byte, mdat []byte, moofBoxLen int) {
	t.Helper()

	boxes := walkBoxes(t, frag)
	require.Len(t, boxes, 2)
	require.Equal(t, "moof", boxes[0].typ)
	require.Equal(t, "mdat", boxes[1].typ)

	return boxes[0].payload, boxes[1].payload, len(boxes[0].payload) + 8
}

// This is the test that matters most. trun's data_offset is relative to the
// start of the moof; if it is off by even one byte the browser reads garbage
// as its first NALU length and drops the stream without a useful error.
func TestFragmentDataOffsetPointsAtMdatPayload(t *testing.T) {
	m := NewMuxer()

	frag, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa, 0xbb, 0xcc), 33*time.Millisecond)
	require.NoError(t, err)

	_, mdat, moofLen := splitFragment(t, frag)
	f := parseMoof(t, frag[8:moofLen])

	assert.Equal(t, uint32(moofLen+8), f.dataOffset,
		"data_offset must be moof size + mdat header, i.e. the first payload byte")

	// Prove it independently: index into the fragment at data_offset and
	// confirm we land exactly on the start of the mdat payload.
	assert.Equal(t, mdat, frag[f.dataOffset:],
		"bytes at data_offset must be exactly the mdat payload")

	assert.Equal(t, uint32(len(mdat)), f.size, "trun sample size must match the mdat payload")
	assert.Equal(t, uint32(1), f.sampleCount)
}

func TestFragmentSampleIsAVCC(t *testing.T) {
	m := NewMuxer()

	frag, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa, 0xbb, 0xcc), 33*time.Millisecond)
	require.NoError(t, err)

	_, mdat, _ := splitFragment(t, frag)

	// The sample must be length-prefixed, and must contain only the coded
	// slice: SPS and PPS live in avcC, and repeating them in an avc1 track
	// is not permitted.
	require.GreaterOrEqual(t, len(mdat), 4)
	naluLen := int(binary.BigEndian.Uint32(mdat[0:4]))
	assert.Equal(t, len(mdat)-4, naluLen, "the single NALU must fill the sample exactly")
	assert.Equal(t, []byte{0x65, 0xaa, 0xbb, 0xcc}, mdat[4:], "only the IDR slice belongs in the sample")
}

func TestAUDAndFillerAreStripped(t *testing.T) {
	m := NewMuxer()

	aud := []byte{0x09, 0x10}
	filler := []byte{0x0c, 0xff}
	sps := testSPS1080p()
	au := annexB(aud, sps, testPPS, []byte{0x65, 0xaa}, filler)

	frag, err := m.WriteAccessUnit(au, 33*time.Millisecond)
	require.NoError(t, err)

	_, mdat, _ := splitFragment(t, frag)
	assert.Equal(t, []byte{0x65, 0xaa}, mdat[4:], "access unit delimiters and filler must not reach the decoder")
}

func TestSEIIsKept(t *testing.T) {
	m := NewMuxer()

	sei := []byte{0x06, 0x01, 0x02, 0x80}
	au := annexB(testSPS1080p(), testPPS, sei, []byte{0x65, 0xaa})

	frag, err := m.WriteAccessUnit(au, 33*time.Millisecond)
	require.NoError(t, err)

	_, mdat, _ := splitFragment(t, frag)

	// Two length-prefixed NALUs: the SEI then the slice.
	seiLen := int(binary.BigEndian.Uint32(mdat[0:4]))
	assert.Equal(t, sei, mdat[4:4+seiLen])
	rest := mdat[4+seiLen:]
	sliceLen := int(binary.BigEndian.Uint32(rest[0:4]))
	assert.Equal(t, []byte{0x65, 0xaa}, rest[4:4+sliceLen])
}

func TestSampleFlagsDistinguishKeyframes(t *testing.T) {
	m := NewMuxer()

	key, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa), 33*time.Millisecond)
	require.NoError(t, err)
	_, _, keyMoofLen := splitFragment(t, key)
	assert.Equal(t, uint32(sampleFlagsKey), parseMoof(t, key[8:keyMoofLen]).firstSampleFlags,
		"an IDR must be marked as a sync sample")

	inter, err := m.WriteAccessUnit(interAccessUnit(0xbb), 33*time.Millisecond)
	require.NoError(t, err)
	_, _, interMoofLen := splitFragment(t, inter)
	assert.Equal(t, uint32(sampleFlagsNonKey), parseMoof(t, inter[8:interMoofLen]).firstSampleFlags,
		"a P frame must be marked non-sync so the browser does not seek to it")
}

func TestDecodeTimeAndSequenceAdvance(t *testing.T) {
	m := NewMuxer()

	const frameDur = 20 * time.Millisecond
	expectedTicks := uint32(frameDur.Nanoseconds() * Timescale / int64(time.Second))

	var decodeTimes []uint64
	var seqNums []uint32

	au := idrAccessUnit(testSPS1080p(), 0xaa)
	for i := 0; i < 4; i++ {
		frag, err := m.WriteAccessUnit(au, frameDur)
		require.NoError(t, err)
		_, _, moofLen := splitFragment(t, frag)
		f := parseMoof(t, frag[8:moofLen])

		assert.Equal(t, expectedTicks, f.duration, "20ms at 90kHz is 1800 ticks")
		decodeTimes = append(decodeTimes, f.baseDecodeTime)
		seqNums = append(seqNums, f.seqNum)
		au = interAccessUnit(byte(i))
	}

	assert.Equal(t, []uint64{0, 1800, 3600, 5400}, decodeTimes,
		"baseMediaDecodeTime must accumulate, or the browser sees overlapping samples")
	assert.Equal(t, []uint32{1, 2, 3, 4}, seqNums, "fragment sequence numbers must increase")
}

func TestDurationIsClamped(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want uint32
	}{
		{"zero becomes the minimum", 0, uint32(minSampleDuration.Nanoseconds() * Timescale / int64(time.Second))},
		{"negative becomes the minimum", -time.Second, uint32(minSampleDuration.Nanoseconds() * Timescale / int64(time.Second))},
		{"a long stall is capped", 30 * time.Second, uint32(maxSampleDuration.Nanoseconds() * Timescale / int64(time.Second))},
		{"a normal frame passes through", 33 * time.Millisecond, 2970},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMuxer()
			frag, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa), tt.in)
			require.NoError(t, err)
			_, _, moofLen := splitFragment(t, frag)
			assert.Equal(t, tt.want, parseMoof(t, frag[8:moofLen]).duration)
		})
	}
}

// --- lifecycle -----------------------------------------------------------

func TestFramesBeforeParameterSetsAreRejected(t *testing.T) {
	m := NewMuxer()

	_, err := m.WriteAccessUnit(interAccessUnit(0xaa), 33*time.Millisecond)
	assert.ErrorIs(t, err, ErrNoParameterSets,
		"a P frame before the first IDR is undecodable and must be dropped")
	assert.False(t, m.Ready())

	_, err = m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa), 33*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, m.Ready())
}

func TestAccessUnitWithoutSliceIsRejected(t *testing.T) {
	m := NewMuxer()
	_, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa), 33*time.Millisecond)
	require.NoError(t, err)

	// Parameter sets on their own carry no picture.
	_, err = m.WriteAccessUnit(annexB(testSPS1080p(), testPPS), 33*time.Millisecond)
	assert.ErrorIs(t, err, ErrEmptySample)

	_, err = m.WriteAccessUnit(nil, 33*time.Millisecond)
	assert.ErrorIs(t, err, ErrEmptySample)
}

func TestRepeatedParameterSetsDoNotSignalChange(t *testing.T) {
	m := NewMuxer()
	au := idrAccessUnit(testSPS1080p(), 0xaa)

	_, err := m.WriteAccessUnit(au, 33*time.Millisecond)
	require.NoError(t, err)
	assert.False(t, m.ParametersChanged(), "the first parameter sets are not a change")

	// Every IDR repeats SPS/PPS; that must not force a client reset.
	for i := 0; i < 3; i++ {
		_, err = m.WriteAccessUnit(au, 33*time.Millisecond)
		require.NoError(t, err)
		assert.False(t, m.ParametersChanged(), "identical parameter sets are not a change")
	}
}

func TestResolutionChangeSignalsResetAndRestartsTimeline(t *testing.T) {
	m := NewMuxer()

	_, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa), 33*time.Millisecond)
	require.NoError(t, err)
	_, err = m.WriteAccessUnit(interAccessUnit(0xbb), 33*time.Millisecond)
	require.NoError(t, err)
	require.False(t, m.ParametersChanged())
	assert.Equal(t, 1080, m.Height())

	// The HDMI source switches mode: a new SPS arrives.
	frag, err := m.WriteAccessUnit(idrAccessUnit(testSPS720p(), 0xcc), 33*time.Millisecond)
	require.NoError(t, err)

	assert.True(t, m.ParametersChanged(), "a new SPS must tell the client to reset its SourceBuffer")
	assert.False(t, m.ParametersChanged(), "the flag is consumed by reading it")
	assert.Equal(t, 1280, m.Width())
	assert.Equal(t, 720, m.Height())

	// The timeline restarts so the new init segment starts from zero.
	_, _, moofLen := splitFragment(t, frag)
	f := parseMoof(t, frag[8:moofLen])
	assert.Equal(t, uint64(0), f.baseDecodeTime, "decode time must restart after a parameter set change")
	assert.Equal(t, uint32(1), f.seqNum, "fragment sequence must restart after a parameter set change")

	// And the new init segment describes the new size.
	tkhd := findBox(t, findBox(t, findBox(t, m.InitSegment(), "moov"), "trak"), "tkhd")
	assert.Equal(t, uint32(1280<<16), binary.BigEndian.Uint32(tkhd[4+72:4+76]))
	assert.Equal(t, uint32(720<<16), binary.BigEndian.Uint32(tkhd[4+76:4+80]))
}

func TestInitSegmentIsStableAcrossFrames(t *testing.T) {
	m := NewMuxer()

	_, err := m.WriteAccessUnit(idrAccessUnit(testSPS1080p(), 0xaa), 33*time.Millisecond)
	require.NoError(t, err)
	first := append([]byte(nil), m.InitSegment()...)

	for i := 0; i < 5; i++ {
		_, err = m.WriteAccessUnit(interAccessUnit(byte(i)), 33*time.Millisecond)
		require.NoError(t, err)
	}
	assert.Equal(t, first, m.InitSegment(), "the init segment must not drift while the format is unchanged")
}
