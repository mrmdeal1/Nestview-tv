package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/format/mpegts"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

var (
	nestBackend = env(
		"NEST_BACKEND",
		"https://nestview-tv.onrender.com",
	)

	port = env("PORT", "10000")
)

const (
	videoPID uint16 = 256

	segmentDuration = 2 * time.Second
	maxSegments     = 6

	ptsOffset int64 = 126000
)

type Camera struct {
	Device string `json:"device"`
	Name   string `json:"name"`
}

type MediaStats struct {
	VideoPackets uint64
	VideoBytes   uint64

	AudioPackets uint64
	AudioBytes   uint64

	AccessUnits uint64
	HLSSegments uint64
	EmptyPackets uint64

	ValidatedTS     uint64
	ValidationError uint64

	TimestampDiscontinuities uint64

	CodecChanges uint64

	PlaylistGenerations uint64
}

type TSDiagnostics struct {
	Valid bool `json:"valid"`

	Bytes   int `json:"bytes"`
	Packets int `json:"packets"`

	SyncErrors int `json:"syncErrors"`

	PATPackets int `json:"patPackets"`

	PMTPackets int `json:"pmtPackets"`

	VideoPackets int `json:"videoPackets"`

	NullPackets int `json:"nullPackets"`

	TransportErrors int `json:"transportErrors"`

	PayloadPackets int `json:"payloadPackets"`

	AdaptationPackets int `json:"adaptationPackets"`

	PCRPackets int `json:"pcrPackets"`

	DiscontinuityFlags int `json:"discontinuityFlags"`

	ContinuityErrors int `json:"continuityErrors"`

	PUSIPackets int `json:"pusiPackets"`

	PESStartCodes int `json:"pesStartCodes"`

	PTSCount int `json:"ptsCount"`

	DTSCount int `json:"dtsCount"`

	FirstPTS int64 `json:"firstPTS"`

	LastPTS int64 `json:"lastPTS"`

	MinPTS int64 `json:"minPTS"`

	MaxPTS int64 `json:"maxPTS"`

	BackwardPTS int `json:"backwardPTS"`

	FirstPCR int64 `json:"firstPCR"`

	LastPCR int64 `json:"lastPCR"`

	PMTPID int `json:"pmtPID"`

	VideoPID int `json:"videoPID"`

	StreamType int `json:"streamType"`

	FirstPacketHex string `json:"firstPacketHex,omitempty"`

	Error string `json:"error,omitempty"`
}

type H264Diagnostics struct {
	Valid bool `json:"valid"`

	Profile string `json:"profile"`

	ProfileIDC int `json:"profileIDC"`

	ConstraintFlags int `json:"constraintFlags"`

	Level string `json:"level"`

	LevelIDC int `json:"levelIDC"`

	SPSID uint64 `json:"spsID"`

	ChromaFormatIDC uint64 `json:"chromaFormatIDC"`

	SeparateColourPlaneFlag bool `json:"separateColourPlaneFlag"`

	BitDepthLuma int `json:"bitDepthLuma"`

	BitDepthChroma int `json:"bitDepthChroma"`

	Log2MaxFrameNum int `json:"log2MaxFrameNum"`

	PicOrderCntType uint64 `json:"picOrderCntType"`

	Log2MaxPicOrderCntLSB int `json:"log2MaxPicOrderCntLSB"`

	MaxNumRefFrames uint64 `json:"maxNumRefFrames"`

	PicWidthInMbs int `json:"picWidthInMbs"`

	PicHeightInMapUnits int `json:"picHeightInMapUnits"`

	FrameMbsOnly bool `json:"frameMbsOnly"`

	FrameCropLeft uint64 `json:"frameCropLeft"`

	FrameCropRight uint64 `json:"frameCropRight"`

	FrameCropTop uint64 `json:"frameCropTop"`

	FrameCropBottom uint64 `json:"frameCropBottom"`

	CodedWidth int `json:"codedWidth"`

	CodedHeight int `json:"codedHeight"`

	DisplayWidth int `json:"displayWidth"`

	DisplayHeight int `json:"displayHeight"`

	VUIParametersPresent bool `json:"vuiParametersPresent"`

	SPSBytes int `json:"spsBytes"`

	PPSBytes int `json:"ppsBytes"`

	SPSHex string `json:"spsHex,omitempty"`

	PPSHex string `json:"ppsHex,omitempty"`

	SafariBaselineCompatible bool `json:"baseline8bit420"`

	Error string `json:"error,omitempty"`
}

type CodecSignature struct {
	ProfileIDC int `json:"profileIDC"`

	LevelIDC int `json:"levelIDC"`

	Width int `json:"width"`

	Height int `json:"height"`

	ChromaFormatIDC uint64 `json:"chromaFormatIDC"`

	BitDepthLuma int `json:"bitDepthLuma"`

	BitDepthChroma int `json:"bitDepthChroma"`

	SPSHex string `json:"spsHex,omitempty"`

	PPSHex string `json:"ppsHex,omitempty"`
}

type CodecChange struct {
	Number uint64 `json:"number"`

	Time string `json:"time"`

	PTS int64 `json:"pts"`

	From CodecSignature `json:"from"`

	To CodecSignature `json:"to"`

	SegmentSequence int `json:"segmentSequence"`

	Reason string `json:"reason"`
}

type HLSSegment struct {
	Sequence int

	Duration float64

	Data []byte

	Validated bool

	ValidateErr string

	Diagnostics TSDiagnostics

	Codec CodecSignature

	Generation uint64

	DiscontinuityBefore bool
}

type StreamSession struct {
	mu sync.RWMutex

	PC *webrtc.PeerConnection

	Stats *MediaStats

	Camera Camera

	Index int

	Segments []HLSSegment

	NextSequence int

	currentBuffer *bytes.Buffer

	currentWriter *mpegts.Writer

	segmentStart time.Time

	segmentActive bool

	sps []byte
	pps []byte

	h264Diagnostics H264Diagnostics

	h264Logged bool

	activeCodec CodecSignature

	haveActiveCodec bool

	currentSegmentCodec CodecSignature

	currentGeneration uint64

	pendingDiscontinuity bool

	codecChanges []CodecChange

	timestampStarted bool

	lastRTPTimestamp uint32

	normalizedPTS int64

	lastNALSummary string

	ready chan struct{}

	done chan struct{}

	readyOnce sync.Once

	closeOnce sync.Once
}

var (
	sessionMu sync.RWMutex

	currentSession *StreamSession
)

func env(
	key string,
	fallback string,
) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func writeJSON(
	w http.ResponseWriter,
	status int,
	value interface{},
) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	w.Header().Set(
		"Cache-Control",
		"no-store",
	)

	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(value)
}

func firstString(
	m map[string]interface{},
	keys ...string,
) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok &&
			value != "" {

			return value
		}
	}

	return ""
}

func getCameras() ([]Camera, error) {
	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	resp, err := client.Get(
		nestBackend + "/api/cameras",
	)

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)

	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		return nil, fmt.Errorf(
			"camera backend returned %d: %s",
			resp.StatusCode,
			string(body),
		)
	}

	var raw interface{}

	if err := json.Unmarshal(
		body,
		&raw,
	); err != nil {

		return nil, err
	}

	var list []interface{}

	switch value := raw.(type) {

	case []interface{}:
		list = value

	case map[string]interface{}:

		for _, key := range []string{
			"cameras",
			"devices",
			"results",
		} {
			if candidate, ok :=
				value[key].([]interface{});
				ok {

				list = candidate
				break
			}
		}
	}

	var cameras []Camera

	for _, item := range list {
		m, ok :=
			item.(map[string]interface{})

		if !ok {
			continue
		}

		device := firstString(
			m,
			"device",
			"name",
			"deviceName",
			"id",
		)

		label := firstString(
			m,
			"label",
			"cameraName",
			"displayName",
			"customName",
			"roomName",
		)

		if label == "" {
			label = "Camera"
		}

		if strings.Contains(
			device,
			"/devices/",
		) {
			cameras = append(
				cameras,
				Camera{
					Device: device,
					Name:   label,
				},
			)
		}
	}

	if len(cameras) == 0 {
		return nil,
			fmt.Errorf("no cameras found")
	}

	return cameras, nil
}

func findAnswerSDP(
	value interface{},
) string {
	switch v := value.(type) {

	case map[string]interface{}:

		for key, item := range v {
			lowerKey :=
				strings.ToLower(key)

			if lowerKey == "answersdp" ||
				lowerKey == "answer_sdp" ||
				lowerKey == "answer" ||
				lowerKey == "sdp" {

				if text, ok :=
					item.(string);
					ok &&
						strings.Contains(
							text,
							"v=0",
						) {

					return text
				}
			}
		}

		for _, item := range v {
			if answer :=
				findAnswerSDP(item);
				answer != "" {

				return answer
			}
		}

	case []interface{}:

		for _, item := range v {
			if answer :=
				findAnswerSDP(item);
				answer != "" {

				return answer
			}
		}

	case string:

		if strings.Contains(
			v,
			"v=0",
		) &&
			strings.Contains(
				v,
				"m=video",
			) {

			return v
		}
	}

	return ""
}

func waitForICE(
	pc *webrtc.PeerConnection,
) {
	done :=
		webrtc.GatheringCompletePromise(
			pc,
		)

	select {

	case <-done:

	case <-time.After(
		10 * time.Second,
	):
	}
}

func splitAnnexB(
	data []byte,
) [][]byte {
	var nalus [][]byte

	findStart :=
		func(
			from int,
		) (int, int) {

			for i := from;
				i < len(data);
				i++ {

				if i+4 <= len(data) &&
					data[i] == 0 &&
					data[i+1] == 0 &&
					data[i+2] == 0 &&
					data[i+3] == 1 {

					return i, 4
				}

				if i+3 <= len(data) &&
					data[i] == 0 &&
					data[i+1] == 0 &&
					data[i+2] == 1 {

					return i, 3
				}
			}

			return -1, 0
		}

	pos := 0

	for {
		start, prefix :=
			findStart(pos)

		if start < 0 {
			break
		}

		naluStart :=
			start + prefix

		next, _ :=
			findStart(naluStart)

		naluEnd :=
			len(data)

		if next >= 0 {
			naluEnd = next
		}

		if naluStart < naluEnd {
			nalus = append(
				nalus,
				append(
					[]byte(nil),
					data[naluStart:naluEnd]...,
				),
			)
		}

		if next < 0 {
			break
		}

		pos = next
	}

	return nalus
}

func annexB(
	nalu []byte,
) []byte {
	if len(nalu) == 0 {
		return nil
	}

	out :=
		make(
			[]byte,
			4+len(nalu),
		)

	out[3] = 1

	copy(
		out[4:],
		nalu,
	)

	return out
}

func inspectAccessUnit(
	data []byte,
) (
	hasIDR bool,
	hasSPS bool,
	hasPPS bool,
	sps []byte,
	pps []byte,
) {
	for _, nalu :=
		range splitAnnexB(data) {

		if len(nalu) == 0 {
			continue
		}

		switch nalu[0] & 0x1F {

		case 5:
			hasIDR = true

		case 7:
			hasSPS = true
			sps = annexB(nalu)

		case 8:
			hasPPS = true
			pps = annexB(nalu)
		}
	}

	return
}

func naluSummary(
	data []byte,
) string {
	nalus :=
		splitAnnexB(data)

	if len(nalus) == 0 {
		return "no-annexb-nalus"
	}

	types :=
		make(
			[]string,
			0,
			len(nalus),
		)

	for _, nalu := range nalus {
		if len(nalu) > 0 {
			types = append(
				types,
				strconv.Itoa(
					int(
						nalu[0] & 0x1F,
					),
				),
			)
		}
	}

	if len(types) == 0 {
		return "empty-nalus"
	}

	return strings.Join(
		types,
		",",
	)
}

func stripAnnexBStartCode(
	data []byte,
) []byte {
	if len(data) >= 4 &&
		data[0] == 0 &&
		data[1] == 0 &&
		data[2] == 0 &&
		data[3] == 1 {

		return data[4:]
	}

	if len(data) >= 3 &&
		data[0] == 0 &&
		data[1] == 0 &&
		data[2] == 1 {

		return data[3:]
	}

	return data
}

func removeEmulationPrevention(
	data []byte,
) []byte {
	out :=
		make(
			[]byte,
			0,
			len(data),
		)

	zeroCount := 0

	for _, value :=
		range data {

		if zeroCount >= 2 &&
			value == 0x03 {

			zeroCount = 0
			continue
		}

		out =
			append(
				out,
				value,
			)

		if value == 0 {
			zeroCount++
		} else {
			zeroCount = 0
		}
	}

	return out
}

type bitReader struct {
	data []byte
	pos  int
}

func (b *bitReader) readBit() (
	uint64,
	error,
) {
	if b.pos >=
		len(b.data)*8 {

		return 0,
			io.ErrUnexpectedEOF
	}

	byteIndex :=
		b.pos / 8

	bitIndex :=
		7 - (b.pos % 8)

	value :=
		uint64(
			(b.data[byteIndex] >>
				bitIndex) & 1,
		)

	b.pos++

	return value, nil
}

func (b *bitReader) readBits(
	count int,
) (uint64, error) {
	var value uint64

	for i := 0;
		i < count;
		i++ {

		bit, err :=
			b.readBit()

		if err != nil {
			return 0, err
		}

		value =
			(value << 1) |
				bit
	}

	return value, nil
}

func (b *bitReader) readUE() (
	uint64,
	error,
) {
	zeros := 0

	for {
		bit, err :=
			b.readBit()

		if err != nil {
			return 0, err
		}

		if bit == 1 {
			break
		}

		zeros++

		if zeros > 63 {
			return 0,
				fmt.Errorf(
					"Exp-Golomb overflow",
				)
		}
	}

	if zeros == 0 {
		return 0, nil
	}

	suffix, err :=
		b.readBits(
			zeros,
		)

	if err != nil {
		return 0, err
	}

	return (uint64(1)<<zeros) -
			1 +
			suffix,
		nil
}

func (b *bitReader) readSE() (
	int64,
	error,
) {
	value, err :=
		b.readUE()

	if err != nil {
		return 0, err
	}

	if value%2 == 0 {
		return -int64(
			value / 2,
		), nil
	}

	return int64(
		(value + 1) / 2,
	), nil
}

func skipScalingList(
	b *bitReader,
	size int,
) error {
	lastScale := int64(8)
	nextScale := int64(8)

	for j := 0;
		j < size;
		j++ {

		if nextScale != 0 {
			deltaScale, err :=
				b.readSE()

			if err != nil {
				return err
			}

			nextScale =
				(lastScale +
					deltaScale +
					256) %
					256
		}

		if nextScale != 0 {
			lastScale =
				nextScale
		}
	}

	return nil
}

func profileName(
	profileIDC int,
) string {
	switch profileIDC {

	case 66:
		return "Baseline"

	case 77:
		return "Main"

	case 88:
		return "Extended"

	case 100:
		return "High"

	case 110:
		return "High 10"

	case 122:
		return "High 4:2:2"

	case 244:
		return "High 4:4:4"

	default:
		return fmt.Sprintf(
			"Profile-%d",
			profileIDC,
		)
	}
}

func levelName(
	levelIDC int,
) string {
	if levelIDC <= 0 {
		return "unknown"
	}

	major :=
		levelIDC / 10

	minor :=
		levelIDC % 10

	return fmt.Sprintf(
		"%d.%d",
		major,
		minor,
	)
}

func codecSignatureFromDiagnostics(
	d H264Diagnostics,
) CodecSignature {
	return CodecSignature{
		ProfileIDC:
			d.ProfileIDC,

		LevelIDC:
			d.LevelIDC,

		Width:
			d.DisplayWidth,

		Height:
			d.DisplayHeight,

		ChromaFormatIDC:
			d.ChromaFormatIDC,

		BitDepthLuma:
			d.BitDepthLuma,

		BitDepthChroma:
			d.BitDepthChroma,

		SPSHex:
			d.SPSHex,

		PPSHex:
			d.PPSHex,
	}
}

func sameCodecSignature(
	a CodecSignature,
	b CodecSignature,
) bool {
	return a.ProfileIDC ==
			b.ProfileIDC &&
		a.LevelIDC ==
			b.LevelIDC &&
		a.Width ==
			b.Width &&
		a.Height ==
			b.Height &&
		a.ChromaFormatIDC ==
			b.ChromaFormatIDC &&
		a.BitDepthLuma ==
			b.BitDepthLuma &&
		a.BitDepthChroma ==
			b.BitDepthChroma &&
		a.SPSHex ==
			b.SPSHex &&
		a.PPSHex ==
			b.PPSHex
}

func codecSignatureString(
	c CodecSignature,
) string {
	return fmt.Sprintf(
		"profile=%d level=%d size=%dx%d chroma=%d bitDepth=%d/%d",
		c.ProfileIDC,
		c.LevelIDC,
		c.Width,
		c.Height,
		c.ChromaFormatIDC,
		c.BitDepthLuma,
		c.BitDepthChroma,
	)
}

func parseH264SPS(
	spsAnnexB []byte,
	ppsAnnexB []byte,
) H264Diagnostics {
	d :=
		H264Diagnostics{
			SPSBytes:
				len(spsAnnexB),

			PPSBytes:
				len(ppsAnnexB),
		}

	raw :=
		stripAnnexBStartCode(
			spsAnnexB,
		)

	if len(raw) < 4 {
		d.Error =
			"SPS too short"

		return d
	}

	if raw[0]&0x1F != 7 {
		d.Error =
			fmt.Sprintf(
				"expected SPS NAL type 7, got %d",
				raw[0]&0x1F,
			)

		return d
	}

	d.SPSHex =
		hex.EncodeToString(raw)

	ppsRaw :=
		stripAnnexBStartCode(
			ppsAnnexB,
		)

	if len(ppsRaw) > 0 {
		d.PPSHex =
			hex.EncodeToString(
				ppsRaw,
			)
	}

	rbsp :=
		removeEmulationPrevention(
			raw[1:],
		)

	if len(rbsp) < 3 {
		d.Error =
			"SPS RBSP too short"

		return d
	}

	d.ProfileIDC =
		int(rbsp[0])

	d.Profile =
		profileName(
			d.ProfileIDC,
		)

	d.ConstraintFlags =
		int(rbsp[1])

	d.LevelIDC =
		int(rbsp[2])

	d.Level =
		levelName(
			d.LevelIDC,
		)

	b :=
		&bitReader{
			data:
				rbsp[3:],
		}

	spsID, err :=
		b.readUE()

	if err != nil {
		d.Error =
			"SPS id: " +
				err.Error()

		return d
	}

	d.SPSID =
		spsID

	chromaFormatIDC :=
		uint64(1)

	bitDepthLumaMinus8 :=
		uint64(0)

	bitDepthChromaMinus8 :=
		uint64(0)

	switch d.ProfileIDC {

	case 100, 110, 122, 244,
		44, 83, 86, 118, 128,
		138, 139, 134, 135:

		chromaFormatIDC, err =
			b.readUE()

		if err != nil {
			d.Error =
				"chroma_format_idc: " +
					err.Error()

			return d
		}

		if chromaFormatIDC == 3 {
			flag, readErr :=
				b.readBit()

			if readErr != nil {
				d.Error =
					"separate_colour_plane_flag: " +
						readErr.Error()

				return d
			}

			d.SeparateColourPlaneFlag =
				flag != 0
		}

		bitDepthLumaMinus8, err =
			b.readUE()

		if err != nil {
			d.Error =
				"bit_depth_luma_minus8: " +
					err.Error()

			return d
		}

		bitDepthChromaMinus8, err =
			b.readUE()

		if err != nil {
			d.Error =
				"bit_depth_chroma_minus8: " +
					err.Error()

			return d
		}

		_, err =
			b.readBit()

		if err != nil {
			d.Error =
				"qpprime_y_zero_transform_bypass_flag: " +
					err.Error()

			return d
		}

		seqScalingMatrixPresent,
			readErr :=
			b.readBit()

		if readErr != nil {
			d.Error =
				"seq_scaling_matrix_present_flag: " +
					readErr.Error()

			return d
		}

		if seqScalingMatrixPresent != 0 {
			scalingCount := 8

			if chromaFormatIDC == 3 {
				scalingCount = 12
			}

			for i := 0;
				i < scalingCount;
				i++ {

				present, readErr :=
					b.readBit()

				if readErr != nil {
					d.Error =
						"seq_scaling_list_present_flag: " +
							readErr.Error()

					return d
				}

				if present != 0 {
					size := 16

					if i >= 6 {
						size = 64
					}

					if err :=
						skipScalingList(
							b,
							size,
						);
						err != nil {

						d.Error =
							"scaling list: " +
								err.Error()

						return d
					}
				}
			}
		}
	}

	d.ChromaFormatIDC =
		chromaFormatIDC

	d.BitDepthLuma =
		int(
			bitDepthLumaMinus8 +
				8,
		)

	d.BitDepthChroma =
		int(
			bitDepthChromaMinus8 +
				8,
		)

	log2MaxFrameNumMinus4,
		err :=
		b.readUE()

	if err != nil {
		d.Error =
			"log2_max_frame_num_minus4: " +
				err.Error()

		return d
	}

	d.Log2MaxFrameNum =
		int(
			log2MaxFrameNumMinus4 +
				4,
		)

	picOrderCntType,
		err :=
		b.readUE()

	if err != nil {
		d.Error =
			"pic_order_cnt_type: " +
				err.Error()

		return d
	}

	d.PicOrderCntType =
		picOrderCntType

	if picOrderCntType == 0 {
		log2MaxPicOrderCntLSBMinus4,
			readErr :=
			b.readUE()

		if readErr != nil {
			d.Error =
				"log2_max_pic_order_cnt_lsb_minus4: " +
					readErr.Error()

			return d
		}

		d.Log2MaxPicOrderCntLSB =
			int(
				log2MaxPicOrderCntLSBMinus4 +
					4,
			)
	}

	if picOrderCntType == 1 {
		_, err =
			b.readBit()

		if err != nil {
			d.Error =
				"delta_pic_order_always_zero_flag: " +
					err.Error()

			return d
		}

		_, err =
			b.readSE()

		if err != nil {
			d.Error =
				"offset_for_non_ref_pic: " +
					err.Error()

			return d
		}

		_, err =
			b.readSE()

		if err != nil {
			d.Error =
				"offset_for_top_to_bottom_field: " +
					err.Error()

			return d
		}

		numRefFramesInPicOrderCntCycle,
			readErr :=
			b.readUE()

		if readErr != nil {
			d.Error =
				"num_ref_frames_in_pic_order_cnt_cycle: " +
					readErr.Error()

			return d
		}

		for i := uint64(0);
			i <
				numRefFramesInPicOrderCntCycle;
			i++ {

			_, readErr =
				b.readSE()

			if readErr != nil {
				d.Error =
					"offset_for_ref_frame: " +
						readErr.Error()

				return d
			}
		}
	}

	maxNumRefFrames,
		err :=
		b.readUE()

	if err != nil {
		d.Error =
			"max_num_ref_frames: " +
				err.Error()

		return d
	}

	d.MaxNumRefFrames =
		maxNumRefFrames

	_, err =
		b.readBit()

	if err != nil {
		d.Error =
			"gaps_in_frame_num_value_allowed_flag: " +
				err.Error()

		return d
	}

	picWidthInMbsMinus1,
		err :=
		b.readUE()

	if err != nil {
		d.Error =
			"pic_width_in_mbs_minus1: " +
				err.Error()

		return d
	}

	picHeightInMapUnitsMinus1,
		err :=
		b.readUE()

	if err != nil {
		d.Error =
			"pic_height_in_map_units_minus1: " +
				err.Error()

		return d
	}

	d.PicWidthInMbs =
		int(
			picWidthInMbsMinus1 +
				1,
		)

	d.PicHeightInMapUnits =
		int(
			picHeightInMapUnitsMinus1 +
				1,
		)

	frameMbsOnlyFlag,
		err :=
		b.readBit()

	if err != nil {
		d.Error =
			"frame_mbs_only_flag: " +
				err.Error()

		return d
	}

	d.FrameMbsOnly =
		frameMbsOnlyFlag != 0

	if frameMbsOnlyFlag == 0 {
		_, err =
			b.readBit()

		if err != nil {
			d.Error =
				"mb_adaptive_frame_field_flag: " +
					err.Error()

			return d
		}
	}

	_, err =
		b.readBit()

	if err != nil {
		d.Error =
			"direct_8x8_inference_flag: " +
				err.Error()

		return d
	}

	frameCroppingFlag,
		err :=
		b.readBit()

	if err != nil {
		d.Error =
			"frame_cropping_flag: " +
				err.Error()

		return d
	}

	if frameCroppingFlag != 0 {
		d.FrameCropLeft, err =
			b.readUE()

		if err != nil {
			d.Error =
				"frame_crop_left_offset: " +
					err.Error()

			return d
		}

		d.FrameCropRight, err =
			b.readUE()

		if err != nil {
			d.Error =
				"frame_crop_right_offset: " +
					err.Error()

			return d
		}

		d.FrameCropTop, err =
			b.readUE()

		if err != nil {
			d.Error =
				"frame_crop_top_offset: " +
					err.Error()

			return d
		}

		d.FrameCropBottom, err =
			b.readUE()

		if err != nil {
			d.Error =
				"frame_crop_bottom_offset: " +
					err.Error()

			return d
		}
	}

	vuiPresent,
		err :=
		b.readBit()

	if err == nil {
		d.VUIParametersPresent =
			vuiPresent != 0
	}

	frameHeightMultiplier := 2

	if d.FrameMbsOnly {
		frameHeightMultiplier = 1
	}

	d.CodedWidth =
		d.PicWidthInMbs * 16

	d.CodedHeight =
		d.PicHeightInMapUnits *
			16 *
			frameHeightMultiplier

	cropUnitX := 1

	cropUnitY :=
		2 -
			int(
				frameMbsOnlyFlag,
			)

	if !d.SeparateColourPlaneFlag {
		switch d.ChromaFormatIDC {

		case 0:
			cropUnitX = 1

			cropUnitY =
				2 -
					int(
						frameMbsOnlyFlag,
					)

		case 1:
			cropUnitX = 2

			cropUnitY =
				2 *
					(2 -
						int(
							frameMbsOnlyFlag,
						))

		case 2:
			cropUnitX = 2

			cropUnitY =
				2 -
					int(
						frameMbsOnlyFlag,
					)

		case 3:
			cropUnitX = 1

			cropUnitY =
				2 -
					int(
						frameMbsOnlyFlag,
					)
		}
	}

	d.DisplayWidth =
		d.CodedWidth -
			int(
				d.FrameCropLeft+
					d.FrameCropRight,
			)*
				cropUnitX

	d.DisplayHeight =
		d.CodedHeight -
			int(
				d.FrameCropTop+
					d.FrameCropBottom,
			)*
				cropUnitY

	d.SafariBaselineCompatible =
		d.ProfileIDC == 66 &&
			d.BitDepthLuma == 8 &&
			d.BitDepthChroma == 8 &&
			d.ChromaFormatIDC == 1

	d.Valid =
		d.DisplayWidth > 0 &&
			d.DisplayHeight > 0 &&
			d.BitDepthLuma > 0 &&
			d.BitDepthChroma > 0

	if !d.Valid &&
		d.Error == "" {

		d.Error =
			"parsed SPS produced invalid dimensions"
	}

	return d
}
	func (s *StreamSession) updateH264DiagnosticsLocked(
	accessUnit []byte,
	pts int64,
) {
	hasIDR, hasSPS, hasPPS, newSPS, newPPS :=
		inspectAccessUnit(accessUnit)

	if hasSPS &&
		len(newSPS) > 0 {

		s.sps =
			append(
				[]byte(nil),
				newSPS...,
			)
	}

	if hasPPS &&
		len(newPPS) > 0 {

		s.pps =
			append(
				[]byte(nil),
				newPPS...,
			)
	}

	if len(s.sps) == 0 {
		return
	}

	diagnostics :=
		parseH264SPS(
			s.sps,
			s.pps,
		)

	s.h264Diagnostics =
		diagnostics

	if !diagnostics.Valid {
		if hasSPS {
			log.Printf(
				"VERSION 15 H264 SPS PARSE ERROR: %s",
				diagnostics.Error,
			)
		}

		return
	}

	signature :=
		codecSignatureFromDiagnostics(
			diagnostics,
		)

	if !s.haveActiveCodec {
		s.activeCodec =
			signature

		s.haveActiveCodec =
			true

		s.currentGeneration = 0

		log.Printf(
			"VERSION 15 H264 INITIAL CODEC: %s profileName=%s levelName=%s SPS=%d PPS=%d IDR=%t PTS=%d",
			codecSignatureString(
				signature,
			),
			diagnostics.Profile,
			diagnostics.Level,
			diagnostics.SPSBytes,
			diagnostics.PPSBytes,
			hasIDR,
			pts,
		)

		s.h264Logged = true

		return
	}

	if sameCodecSignature(
		s.activeCodec,
		signature,
	) {
		if !s.h264Logged {
			log.Printf(
				"VERSION 15 H264 CODEC: %s",
				codecSignatureString(
					signature,
				),
			)

			s.h264Logged = true
		}

		return
	}

	oldCodec :=
		s.activeCodec

	changeNumber :=
		atomic.AddUint64(
			&s.Stats.CodecChanges,
			1,
		)

	log.Printf(
		"VERSION 15 CODEC CHANGE DETECTED #%d: FROM [%s] TO [%s] IDR=%t PTS=%d",
		changeNumber,
		codecSignatureString(
			oldCodec,
		),
		codecSignatureString(
			signature,
		),
		hasIDR,
		pts,
	)

	if s.segmentActive &&
		s.currentBuffer != nil &&
		s.currentBuffer.Len() > 0 {

		log.Printf(
			"VERSION 15 closing old codec segment before SPS transition: sequence=%d generation=%d",
			s.NextSequence,
			s.currentGeneration,
		)

		s.finishSegmentLocked()
	}

	s.currentGeneration++

	s.pendingDiscontinuity = true

	s.activeCodec =
		signature

	change :=
		CodecChange{
			Number:
				changeNumber,

			Time:
				time.Now().UTC().Format(
					time.RFC3339Nano,
				),

			PTS:
				pts,

			From:
				oldCodec,

			To:
				signature,

			SegmentSequence:
				s.NextSequence,

			Reason:
				"SPS/PPS codec signature changed",
		}

	s.codecChanges =
		append(
			s.codecChanges,
			change,
		)

	if len(s.codecChanges) > 16 {
		s.codecChanges =
			append(
				[]CodecChange(nil),
				s.codecChanges[len(s.codecChanges)-16:]...,
			)
	}

	log.Printf(
		"VERSION 15 HLS DISCONTINUITY ARMED: generation=%d nextSequence=%d",
		s.currentGeneration,
		s.NextSequence,
	)
}

func (s *StreamSession) cacheParametersLocked(
	accessUnit []byte,
) {
	_, hasSPS, hasPPS, newSPS, newPPS :=
		inspectAccessUnit(accessUnit)

	if hasSPS &&
		len(newSPS) > 0 &&
		!bytes.Equal(
			s.sps,
			newSPS,
		) {

		s.sps =
			append(
				[]byte(nil),
				newSPS...,
			)

		log.Printf(
			"VERSION 15 cached SPS: %d bytes",
			len(s.sps),
		)
	}

	if hasPPS &&
		len(newPPS) > 0 &&
		!bytes.Equal(
			s.pps,
			newPPS,
		) {

		s.pps =
			append(
				[]byte(nil),
				newPPS...,
			)

		log.Printf(
			"VERSION 15 cached PPS: %d bytes",
			len(s.pps),
		)
	}
}

func (s *StreamSession) normalizeTimestamp(
	rtpTimestamp uint32,
) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.timestampStarted {
		s.timestampStarted = true

		s.lastRTPTimestamp =
			rtpTimestamp

		s.normalizedPTS =
			ptsOffset

		log.Printf(
			"VERSION 15 timestamp clock started: RTP=%d PTS=%d",
			rtpTimestamp,
			s.normalizedPTS,
		)

		return s.normalizedPTS
	}

	delta :=
		uint32(
			rtpTimestamp -
				s.lastRTPTimestamp,
		)

	if delta > 90000*10 {
		atomic.AddUint64(
			&s.Stats.TimestampDiscontinuities,
			1,
		)

		log.Printf(
			"VERSION 15 timestamp discontinuity: previous=%d current=%d rawDelta=%d",
			s.lastRTPTimestamp,
			rtpTimestamp,
			delta,
		)

		s.lastRTPTimestamp =
			rtpTimestamp

		return s.normalizedPTS
	}

	s.normalizedPTS +=
		int64(delta)

	s.lastRTPTimestamp =
		rtpTimestamp

	return s.normalizedPTS
}

func parsePTS(
	data []byte,
) int64 {
	if len(data) < 5 {
		return -1
	}

	pts :=
		(int64(data[0]&0x0E) << 29) |
			(int64(data[1]) << 22) |
			(int64(data[2]&0xFE) << 14) |
			(int64(data[3]) << 7) |
			(int64(data[4]&0xFE) >> 1)

	return pts
}

func parsePCR(
	packet []byte,
) int64 {
	if len(packet) < 12 {
		return -1
	}

	adaptationLength :=
		int(packet[4])

	if adaptationLength < 7 ||
		5+adaptationLength > len(packet) {
		return -1
	}

	flags :=
		packet[5]

	if flags&0x10 == 0 {
		return -1
	}

	p :=
		packet[6:12]

	base :=
		(int64(p[0]) << 25) |
			(int64(p[1]) << 17) |
			(int64(p[2]) << 9) |
			(int64(p[3]) << 1) |
			(int64(p[4]) >> 7)

	return base
}

func diagnoseTS(
	data []byte,
) TSDiagnostics {
	d :=
		TSDiagnostics{
			Bytes:
				len(data),

			FirstPTS:
				-1,

			LastPTS:
				-1,

			MinPTS:
				-1,

			MaxPTS:
				-1,

			FirstPCR:
				-1,

			LastPCR:
				-1,

			PMTPID:
				-1,

			VideoPID:
				-1,
		}

	if len(data) == 0 {
		d.Error =
			"segment is empty"

		return d
	}

	if len(data) >= 32 {
		d.FirstPacketHex =
			hex.EncodeToString(
				data[:32],
			)
	} else {
		d.FirstPacketHex =
			hex.EncodeToString(
				data,
			)
	}

	if len(data)%188 != 0 {
		d.Error =
			fmt.Sprintf(
				"TS size %d is not divisible by 188",
				len(data),
			)

		return d
	}

	d.Packets =
		len(data) / 188

	continuity :=
		make(
			map[uint16]uint8,
		)

	continuitySeen :=
		make(
			map[uint16]bool,
		)

	var previousPTS int64 = -1

	for offset := 0;
		offset+188 <= len(data);
		offset += 188 {

		packet :=
			data[offset : offset+188]

		if packet[0] != 0x47 {
			d.SyncErrors++
			continue
		}

		transportError :=
			packet[1]&0x80 != 0

		if transportError {
			d.TransportErrors++
		}

		pusi :=
			packet[1]&0x40 != 0

		if pusi {
			d.PUSIPackets++
		}

		pid :=
			uint16(packet[1]&0x1F)<<8 |
				uint16(packet[2])

		adaptationControl :=
			(packet[3] >> 4) & 0x03

		counter :=
			packet[3] & 0x0F

		hasAdaptation :=
			adaptationControl == 2 ||
				adaptationControl == 3

		hasPayload :=
			adaptationControl == 1 ||
				adaptationControl == 3

		if pid == 0x1FFF {
			d.NullPackets++
		}

		if hasAdaptation {
			d.AdaptationPackets++

			if len(packet) > 5 {
				adaptationLength :=
					int(packet[4])

				if adaptationLength > 0 &&
					5+adaptationLength <= 188 {

					flags :=
						packet[5]

					if flags&0x80 != 0 {
						d.DiscontinuityFlags++
					}

					if flags&0x10 != 0 {
						pcr :=
							parsePCR(
								packet,
							)

						if pcr >= 0 {
							d.PCRPackets++

							if d.FirstPCR < 0 {
								d.FirstPCR =
									pcr
							}

							d.LastPCR =
								pcr
						}
					}
				}
			}
		}

		if hasPayload {
			d.PayloadPackets++

			if continuitySeen[pid] {
				expected :=
					(continuity[pid] + 1) &
						0x0F

				if counter != expected {
					d.ContinuityErrors++
				}
			}

			continuity[pid] =
				counter

			continuitySeen[pid] =
				true
		}

		payloadStart := 4

		if hasAdaptation {
			if payloadStart >= 188 {
				continue
			}

			adaptationLength :=
				int(packet[4])

			payloadStart +=
				1 +
					adaptationLength
		}

		if !hasPayload ||
			payloadStart >= 188 {

			continue
		}

		payload :=
			packet[payloadStart:]

		if pid == 0 {
			d.PATPackets++

			if pusi &&
				len(payload) > 0 {

				pointer :=
					int(payload[0])

				pos :=
					1 + pointer

				if pos+8 <= len(payload) &&
					payload[pos] == 0x00 {

					sectionLength :=
						int(
							binary.BigEndian.Uint16(
								payload[pos+1:
									pos+3],
							) & 0x0FFF,
						)

					end :=
						pos +
							3 +
							sectionLength -
							4

					programPos :=
						pos + 8

					for programPos+4 <= end &&
						programPos+4 <= len(payload) {

						programNumber :=
							binary.BigEndian.Uint16(
								payload[
									programPos:
									programPos+2],
							)

						programPID :=
							int(
								binary.BigEndian.Uint16(
									payload[
										programPos+2:
										programPos+4],
								) & 0x1FFF,
							)

						if programNumber != 0 {
							d.PMTPID =
								programPID

							break
						}

						programPos += 4
					}
				}
			}
		}

		if d.PMTPID >= 0 &&
			int(pid) == d.PMTPID {

			d.PMTPackets++

			if pusi &&
				len(payload) > 0 {

				pointer :=
					int(payload[0])

				pos :=
					1 + pointer

				if pos+12 <= len(payload) &&
					payload[pos] == 0x02 {

					sectionLength :=
						int(
							binary.BigEndian.Uint16(
								payload[pos+1:
									pos+3],
							) & 0x0FFF,
						)

					programInfoLength :=
						int(
							binary.BigEndian.Uint16(
								payload[pos+10:
									pos+12],
							) & 0x0FFF,
						)

					esPos :=
						pos +
							12 +
							programInfoLength

					end :=
						pos +
							3 +
							sectionLength -
							4

					for esPos+5 <= end &&
						esPos+5 <= len(payload) {

						streamType :=
							int(
								payload[esPos],
							)

						elementaryPID :=
							int(
								binary.BigEndian.Uint16(
									payload[
										esPos+1:
										esPos+3],
								) & 0x1FFF,
							)

						esInfoLength :=
							int(
								binary.BigEndian.Uint16(
									payload[
										esPos+3:
										esPos+5],
								) & 0x0FFF,
							)

						if streamType == 0x1B {
							d.StreamType =
								streamType

							d.VideoPID =
								elementaryPID
						}

						esPos +=
							5 +
								esInfoLength
					}
				}
			}
		}

		if d.VideoPID >= 0 &&
			int(pid) == d.VideoPID {

			d.VideoPackets++

			if pusi &&
				len(payload) >= 9 &&
				payload[0] == 0x00 &&
				payload[1] == 0x00 &&
				payload[2] == 0x01 {

				d.PESStartCodes++

				flags :=
					payload[7]

				headerLength :=
					int(payload[8])

				if flags&0x80 != 0 &&
					headerLength >= 5 &&
					len(payload) >= 14 {

					pts :=
						parsePTS(
							payload[9:14],
						)

					if pts >= 0 {
						d.PTSCount++

						if d.FirstPTS < 0 {
							d.FirstPTS =
								pts
						}

						d.LastPTS =
							pts

						if d.MinPTS < 0 ||
							pts < d.MinPTS {

							d.MinPTS =
								pts
						}

						if d.MaxPTS < 0 ||
							pts > d.MaxPTS {

							d.MaxPTS =
								pts
						}

						if previousPTS >= 0 &&
							pts < previousPTS {

							d.BackwardPTS++
						}

						previousPTS =
							pts
					}
				}

				if flags&0x40 != 0 {
					d.DTSCount++
				}
			}
		}
	}

	d.Valid =
		d.SyncErrors == 0 &&
			d.TransportErrors == 0 &&
			d.PATPackets > 0 &&
			d.PMTPackets > 0 &&
			d.VideoPID >= 0 &&
			d.StreamType == 0x1B &&
			d.VideoPackets > 0 &&
			d.PESStartCodes > 0 &&
			d.PTSCount > 0 &&
			d.BackwardPTS == 0

	if !d.Valid &&
		d.Error == "" {

		d.Error =
			"media-level TS diagnostics failed"
	}

	return d
}

func (s *StreamSession) newSegmentLocked() error {
	buf :=
		&bytes.Buffer{}

	writer, err :=
		mpegts.NewWriter(
			buf,
			mpegts.WithH264Track(
				videoPID,
			),
		)

	if err != nil {
		return err
	}

	s.currentBuffer =
		buf

	s.currentWriter =
		writer

	s.segmentStart =
		time.Now()

	s.segmentActive =
		true

	if s.haveActiveCodec {
		s.currentSegmentCodec =
			s.activeCodec
	}

	log.Printf(
		"VERSION 15 SEGMENT OPEN: sequence=%d generation=%d codec=[%s] discontinuity=%t",
		s.NextSequence,
		s.currentGeneration,
		codecSignatureString(
			s.currentSegmentCodec,
		),
		s.pendingDiscontinuity,
	)

	return nil
}

func validateSegment(
	data []byte,
) (
	bool,
	string,
	TSDiagnostics,
) {
	diagnostics :=
		diagnoseTS(
			data,
		)

	if diagnostics.Valid {
		return true,
			"",
			diagnostics
	}

	return false,
		diagnostics.Error,
		diagnostics
}

func (s *StreamSession) finishSegmentLocked() {
	if !s.segmentActive ||
		s.currentWriter == nil ||
		s.currentBuffer == nil ||
		s.currentBuffer.Len() == 0 {

		return
	}

	duration :=
		time.Since(
			s.segmentStart,
		).Seconds()

	if duration <= 0 {
		duration =
			segmentDuration.Seconds()
	}

	data :=
		append(
			[]byte(nil),
			s.currentBuffer.Bytes()...,
		)

	valid,
		validateErr,
		diagnostics :=
		validateSegment(
			data,
		)

	if valid {
		atomic.AddUint64(
			&s.Stats.ValidatedTS,
			1,
		)

		log.Printf(
			"VERSION 15 TS MEDIA VALID: packets=%d bytes=%d PAT=%d PMT=%d videoPID=%d streamType=0x%02x PES=%d PTS=%d DTS=%d PCR=%d continuityErrors=%d",
			diagnostics.Packets,
			diagnostics.Bytes,
			diagnostics.PATPackets,
			diagnostics.PMTPackets,
			diagnostics.VideoPID,
			diagnostics.StreamType,
			diagnostics.PESStartCodes,
			diagnostics.PTSCount,
			diagnostics.DTSCount,
			diagnostics.PCRPackets,
			diagnostics.ContinuityErrors,
		)
	} else {
		atomic.AddUint64(
			&s.Stats.ValidationError,
			1,
		)

		log.Printf(
			"VERSION 15 TS MEDIA INVALID: error=%s packets=%d PAT=%d PMT=%d videoPID=%d streamType=0x%02x PES=%d PTS=%d backwardPTS=%d PCR=%d continuityErrors=%d transportErrors=%d",
			validateErr,
			diagnostics.Packets,
			diagnostics.PATPackets,
			diagnostics.PMTPackets,
			diagnostics.VideoPID,
			diagnostics.StreamType,
			diagnostics.PESStartCodes,
			diagnostics.PTSCount,
			diagnostics.BackwardPTS,
			diagnostics.PCRPackets,
			diagnostics.ContinuityErrors,
			diagnostics.TransportErrors,
		)
	}

	discontinuity :=
		s.pendingDiscontinuity

	segment :=
		HLSSegment{
			Sequence:
				s.NextSequence,

			Duration:
				duration,

			Data:
				data,

			Validated:
				valid,

			ValidateErr:
				validateErr,

			Diagnostics:
				diagnostics,

			Codec:
				s.currentSegmentCodec,

			Generation:
				s.currentGeneration,

			DiscontinuityBefore:
				discontinuity,
		}

	s.pendingDiscontinuity =
		false

	s.NextSequence++

	s.Segments =
		append(
			s.Segments,
			segment,
		)

	if len(s.Segments) >
		maxSegments {

		s.Segments =
			append(
				[]HLSSegment(nil),
				s.Segments[len(s.Segments)-maxSegments:]...,
			)
	}

	atomic.AddUint64(
		&s.Stats.HLSSegments,
		1,
	)

	log.Printf(
		"VERSION 15 HLS SEGMENT READY: sequence=%d generation=%d duration=%.3f size=%d mediaValid=%t discontinuityBefore=%t codec=[%s]",
		segment.Sequence,
		segment.Generation,
		segment.Duration,
		len(segment.Data),
		segment.Validated,
		segment.DiscontinuityBefore,
		codecSignatureString(
			segment.Codec,
		),
	)

	s.readyOnce.Do(
		func() {
			close(
				s.ready,
			)
		},
	)

	s.currentBuffer =
		nil

	s.currentWriter =
		nil

	s.segmentActive =
		false
}

func (s *StreamSession) keyframeAccessUnitLocked(
	accessUnit []byte,
) []byte {
	hasIDR,
		hasSPS,
		hasPPS,
		_,
		_ :=
		inspectAccessUnit(
			accessUnit,
		)

	if !hasIDR {
		return accessUnit
	}

	var out []byte

	if !hasSPS &&
		len(s.sps) > 0 {

		out =
			append(
				out,
				s.sps...,
			)
	}

	if !hasPPS &&
		len(s.pps) > 0 {

		out =
			append(
				out,
				s.pps...,
			)
	}

	out =
		append(
			out,
			accessUnit...,
		)

	return out
}

func (s *StreamSession) writeAccessUnit(
	accessUnit []byte,
	pts int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {

	case <-s.done:
		return fmt.Errorf(
			"stream session closed",
		)

	default:
	}

	hasIDR,
		_,
		_,
		_,
		_ :=
		inspectAccessUnit(
			accessUnit,
		)

	s.lastNALSummary =
		naluSummary(
			accessUnit,
		)

	/*
		V15's critical change:

		Inspect the SPS/PPS and detect a codec/resolution
		change BEFORE writing this access unit into the
		current MPEG-TS segment.

		If Nest changes from 640x360 Level 3.0 to
		1920x1080 Level 4.0, the previous segment is
		closed first and the new codec generation begins
		on a fresh segment.
	*/
	s.updateH264DiagnosticsLocked(
		accessUnit,
		pts,
	)

	s.cacheParametersLocked(
		accessUnit,
	)

	if !s.segmentActive {
		if !hasIDR {
			return nil
		}

		if err :=
			s.newSegmentLocked();
			err != nil {

			return err
		}

		log.Printf(
			"VERSION 15 HLS started on IDR: generation=%d SPS=%t PPS=%t PTS=%d NAL=%s codec=[%s]",
			s.currentGeneration,
			len(s.sps) > 0,
			len(s.pps) > 0,
			pts,
			s.lastNALSummary,
			codecSignatureString(
				s.activeCodec,
			),
		)
	}

	if hasIDR &&
		s.currentBuffer != nil &&
		s.currentBuffer.Len() > 0 &&
		time.Since(
			s.segmentStart,
		) >= segmentDuration {

		s.finishSegmentLocked()

		if err :=
			s.newSegmentLocked();
			err != nil {

			return err
		}

		log.Printf(
			"VERSION 15 new IDR segment: sequence=%d generation=%d PTS=%d NAL=%s",
			s.NextSequence,
			s.currentGeneration,
			pts,
			s.lastNALSummary,
		)
	}

	outputAU :=
		s.keyframeAccessUnitLocked(
			accessUnit,
		)

	return s.currentWriter.WriteH264(
		videoPID,
		pts,
		pts,
		outputAU,
	)
}

func (s *StreamSession) Close() {
	s.closeOnce.Do(
		func() {
			close(
				s.done,
			)

			s.mu.Lock()

			if s.segmentActive &&
				s.currentWriter != nil &&
				s.currentBuffer != nil &&
				s.currentBuffer.Len() > 0 {

				s.finishSegmentLocked()
			}

			s.currentWriter =
				nil

			s.currentBuffer =
				nil

			s.segmentActive =
				false

			s.mu.Unlock()

			if s.PC != nil {
				_ =
					s.PC.Close()
			}
		},
	)
}

func replaceSession(
	s *StreamSession,
) {
	sessionMu.Lock()

	old :=
		currentSession

	currentSession =
		s

	sessionMu.Unlock()

	if old != nil {
		old.Close()
	}
}

func getSession() *StreamSession {
	sessionMu.RLock()
	defer sessionMu.RUnlock()

	return currentSession
}
	func createStreamSession(
	camera Camera,
	cameraIndex int,
) (*StreamSession, error) {
	mediaEngine :=
		&webrtc.MediaEngine{}

	err :=
		mediaEngine.RegisterCodec(
			webrtc.RTPCodecParameters{
				RTPCodecCapability:
					webrtc.RTPCodecCapability{
						MimeType:
							webrtc.MimeTypeOpus,

						ClockRate:
							48000,

						Channels:
							2,
					},

				PayloadType:
					111,
			},
			webrtc.RTPCodecTypeAudio,
		)

	if err != nil {
		return nil, err
	}

	err =
		mediaEngine.RegisterCodec(
			webrtc.RTPCodecParameters{
				RTPCodecCapability:
					webrtc.RTPCodecCapability{
						MimeType:
							webrtc.MimeTypeH264,

						ClockRate:
							90000,

						SDPFmtpLine:
							"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
					},

				PayloadType:
					102,
			},
			webrtc.RTPCodecTypeVideo,
		)

	if err != nil {
		return nil, err
	}

	api :=
		webrtc.NewAPI(
			webrtc.WithMediaEngine(
				mediaEngine,
			),
		)

	pc, err :=
		api.NewPeerConnection(
			webrtc.Configuration{},
		)

	if err != nil {
		return nil, err
	}

	session :=
		&StreamSession{
			PC:
				pc,

			Stats:
				&MediaStats{},

			Camera:
				camera,

			Index:
				cameraIndex,

			ready:
				make(chan struct{}),

			done:
				make(chan struct{}),
		}

	pc.OnConnectionStateChange(
		func(
			state webrtc.PeerConnectionState,
		) {
			log.Printf(
				"VERSION 15 WebRTC state: %s",
				state.String(),
			)
		},
	)

	pc.OnTrack(
		func(
			track *webrtc.TrackRemote,
			receiver *webrtc.RTPReceiver,
		) {
			codec :=
				track.Codec()

			log.Printf(
				"VERSION 15 incoming track: kind=%s codec=%s payload=%d",
				track.Kind().String(),
				codec.MimeType,
				codec.PayloadType,
			)

			if track.Kind() ==
				webrtc.RTPCodecTypeVideo {

				go func() {
					var depacketizer codecs.H264Packet

					var accessUnit []byte

					var accessUnitTimestamp uint32

					var haveTimestamp bool

					for {
						packet, _, err :=
							track.ReadRTP()

						if err != nil {
							log.Println(
								"VERSION 15 video RTP ended:",
								err,
							)

							return
						}

						packets :=
							atomic.AddUint64(
								&session.Stats.VideoPackets,
								1,
							)

						atomic.AddUint64(
							&session.Stats.VideoBytes,
							uint64(
								len(
									packet.Payload,
								),
							),
						)

						if len(
							packet.Payload,
						) == 0 {

							atomic.AddUint64(
								&session.Stats.EmptyPackets,
								1,
							)

							continue
						}

						if !haveTimestamp {
							accessUnitTimestamp =
								packet.Timestamp

							haveTimestamp =
								true
						}

						h264Data, err :=
							depacketizer.Unmarshal(
								packet.Payload,
							)

						if err != nil {
							log.Printf(
								"VERSION 15 H264 depacketize error: %v",
								err,
							)

							accessUnit = nil

							haveTimestamp =
								false

							continue
						}

						if len(h264Data) > 0 {
							accessUnit =
								append(
									accessUnit,
									h264Data...,
								)
						}

						if packet.Marker &&
							len(accessUnit) > 0 {

							units :=
								atomic.AddUint64(
									&session.Stats.AccessUnits,
									1,
								)

							pts :=
								session.normalizeTimestamp(
									accessUnitTimestamp,
								)

							if err :=
								session.writeAccessUnit(
									accessUnit,
									pts,
								);
								err != nil {

								log.Printf(
									"VERSION 15 MPEGTS write error: %v",
									err,
								)
							}

							if units == 1 ||
								units%30 == 0 {

								log.Printf(
									"VERSION 15 H264 AU: units=%d packets=%d size=%d PTS=%d NAL=%s",
									units,
									packets,
									len(accessUnit),
									pts,
									naluSummary(
										accessUnit,
									),
								)
							}

							accessUnit = nil

							haveTimestamp =
								false
						}
					}
				}()
			}

			if track.Kind() ==
				webrtc.RTPCodecTypeAudio {

				go func() {
					for {
						packet, _, err :=
							track.ReadRTP()

						if err != nil {
							log.Println(
								"VERSION 15 audio RTP ended:",
								err,
							)

							return
						}

						if len(
							packet.Payload,
						) == 0 {

							continue
						}

						atomic.AddUint64(
							&session.Stats.AudioPackets,
							1,
						)

						atomic.AddUint64(
							&session.Stats.AudioBytes,
							uint64(
								len(
									packet.Payload,
								),
							),
						)
					}
				}()
			}
		},
	)

	_, err =
		pc.AddTransceiverFromKind(
			webrtc.RTPCodecTypeAudio,
			webrtc.RTPTransceiverInit{
				Direction:
					webrtc.RTPTransceiverDirectionRecvonly,
			},
		)

	if err != nil {
		session.Close()
		return nil, err
	}

	_, err =
		pc.AddTransceiverFromKind(
			webrtc.RTPCodecTypeVideo,
			webrtc.RTPTransceiverInit{
				Direction:
					webrtc.RTPTransceiverDirectionRecvonly,
			},
		)

	if err != nil {
		session.Close()
		return nil, err
	}

	_, err =
		pc.CreateDataChannel(
			"nestview",
			nil,
		)

	if err != nil {
		session.Close()
		return nil, err
	}

	offer, err :=
		pc.CreateOffer(nil)

	if err != nil {
		session.Close()
		return nil, err
	}

	if err =
		pc.SetLocalDescription(
			offer,
		);
		err != nil {

		session.Close()
		return nil, err
	}

	waitForICE(pc)

	local :=
		pc.LocalDescription()

	if local == nil {
		session.Close()

		return nil,
			fmt.Errorf(
				"local SDP missing",
			)
	}

	upperSDP :=
		strings.ToUpper(
			local.SDP,
		)

	if !strings.Contains(
		upperSDP,
		"OPUS/48000",
	) {
		session.Close()

		return nil,
			fmt.Errorf(
				"generated SDP does not contain OPUS/48000",
			)
	}

	if !strings.Contains(
		upperSDP,
		"H264/90000",
	) {
		session.Close()

		return nil,
			fmt.Errorf(
				"generated SDP does not contain H264/90000",
			)
	}

	audioPos :=
		strings.Index(
			local.SDP,
			"m=audio",
		)

	videoPos :=
		strings.Index(
			local.SDP,
			"m=video",
		)

	appPos :=
		strings.Index(
			local.SDP,
			"m=application",
		)

	if audioPos == -1 ||
		videoPos == -1 ||
		appPos == -1 {

		session.Close()

		return nil,
			fmt.Errorf(
				"SDP missing audio, video, or application m-line",
			)
	}

	if !(audioPos < videoPos &&
		videoPos < appPos) {

		session.Close()

		return nil,
			fmt.Errorf(
				"SDP order is not audio-video-application",
			)
	}

	log.Println(
		"VERSION 15 SDP confirmed: audio -> video -> application",
	)

	payload :=
		map[string]string{
			"device":
				camera.Device,

			"offerSdp":
				local.SDP,
		}

	data, err :=
		json.Marshal(
			payload,
		)

	if err != nil {
		session.Close()
		return nil, err
	}

	client :=
		&http.Client{
			Timeout:
				30 * time.Second,
		}

	resp, err :=
		client.Post(
			nestBackend+"/api/webrtc",
			"application/json",
			bytes.NewReader(
				data,
			),
		)

	if err != nil {
		session.Close()
		return nil, err
	}

	defer resp.Body.Close()

	body, err :=
		io.ReadAll(
			resp.Body,
		)

	if err != nil {
		session.Close()
		return nil, err
	}

	log.Printf(
		"VERSION 15 Nest backend HTTP status: %d",
		resp.StatusCode,
	)

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		session.Close()

		return nil,
			fmt.Errorf(
				"Nest backend returned %d: %s",
				resp.StatusCode,
				string(body),
			)
	}

	var result interface{}

	if err :=
		json.Unmarshal(
			body,
			&result,
		);
		err != nil {

		session.Close()

		return nil,
			fmt.Errorf(
				"could not decode Nest response: %w",
				err,
			)
	}

	answer :=
		findAnswerSDP(
			result,
		)

	if answer == "" {
		session.Close()

		return nil,
			fmt.Errorf(
				"Nest response did not contain recognizable answer SDP",
			)
	}

	if err =
		pc.SetRemoteDescription(
			webrtc.SessionDescription{
				Type:
					webrtc.SDPTypeAnswer,

				SDP:
					answer,
			},
		);
		err != nil {

		session.Close()
		return nil, err
	}

	log.Println(
		"VERSION 15 Nest WebRTC session started",
	)

	return session, nil
}

func health(
	w http.ResponseWriter,
	r *http.Request,
) {
	session :=
		getSession()

	streaming := false

	var segments uint64
	var validated uint64
	var validationErrors uint64
	var discontinuities uint64
	var codecChanges uint64
	var generation uint64

	if session != nil {
		segments =
			atomic.LoadUint64(
				&session.Stats.HLSSegments,
			)

		validated =
			atomic.LoadUint64(
				&session.Stats.ValidatedTS,
			)

		validationErrors =
			atomic.LoadUint64(
				&session.Stats.ValidationError,
			)

		discontinuities =
			atomic.LoadUint64(
				&session.Stats.TimestampDiscontinuities,
			)

		codecChanges =
			atomic.LoadUint64(
				&session.Stats.CodecChanges,
			)

		session.mu.RLock()

		generation =
			session.currentGeneration

		session.mu.RUnlock()

		streaming =
			segments > 0
	}

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status":
				"ok",

			"bridge":
				"pion-h264-hls",

			"version":
				15,

			"streaming":
				streaming,

			"segments":
				segments,

			"validatedTS":
				validated,

			"validationErrors":
				validationErrors,

			"timestampDiscontinuities":
				discontinuities,

			"codecChanges":
				codecChanges,

			"generation":
				generation,

			"codecTransitionHandling":
				true,

			"hlsDiscontinuity":
				true,

			"timestampOffset":
				ptsOffset,

			"mediaSelfCheck":
				true,
		},
	)
}

func start(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.Method !=
		http.MethodPost {

		writeJSON(
			w,
			405,
			map[string]string{
				"error":
					"POST required",
			},
		)

		return
	}

	body, err :=
		io.ReadAll(
			r.Body,
		)

	if err != nil {
		writeJSON(
			w,
			400,
			map[string]string{
				"error":
					"could not read request body",
			},
		)

		return
	}

	log.Printf(
		"VERSION 15 Shortcut body: %q",
		string(body),
	)

	var rawRequest map[string]interface{}

	if err :=
		json.Unmarshal(
			body,
			&rawRequest,
		);
		err != nil {

		writeJSON(
			w,
			400,
			map[string]string{
				"error":
					"invalid JSON: " +
						err.Error(),
			},
		)

		return
	}

	cameraIndex := 0
	cameraFound := false

	for key, value :=
		range rawRequest {

		if strings.TrimSpace(
			key,
		) != "camera" {

			continue
		}

		switch v := value.(type) {

		case float64:

			cameraIndex =
				int(v)

			cameraFound =
				true

		case string:

			v =
				strings.TrimSpace(
					v,
				)

			if parsed, parseErr :=
				strconv.Atoi(
					v,
				);
				parseErr == nil {

				cameraIndex =
					parsed

				cameraFound =
					true
			}
		}
	}

	if !cameraFound {
		writeJSON(
			w,
			400,
			map[string]string{
				"error":
					"camera field missing or invalid",
			},
		)

		return
	}

	cameras, err :=
		getCameras()

	if err != nil {
		writeJSON(
			w,
			502,
			map[string]string{
				"error":
					err.Error(),
			},
		)

		return
	}

	if cameraIndex < 0 ||
		cameraIndex >= len(cameras) {

		writeJSON(
			w,
			400,
			map[string]string{
				"error":
					"invalid camera index",
			},
		)

		return
	}

	camera :=
		cameras[cameraIndex]

	log.Printf(
		"VERSION 15 starting camera %d: %s",
		cameraIndex,
		camera.Name,
	)

	session, err :=
		createStreamSession(
			camera,
			cameraIndex,
		)

	if err != nil {
		log.Println(
			"VERSION 15 camera start failed:",
			err,
		)

		writeJSON(
			w,
			502,
			map[string]string{
				"error":
					err.Error(),
			},
		)

		return
	}

	replaceSession(
		session,
	)

	select {

	case <-session.ready:

		log.Println(
			"VERSION 15 HLS READY",
		)

	case <-time.After(
		25 * time.Second,
	):

		session.Close()

		sessionMu.Lock()

		if currentSession ==
			session {

			currentSession =
				nil
		}

		sessionMu.Unlock()

		writeJSON(
			w,
			504,
			map[string]string{
				"error":
					"HLS keyframe segment was not ready within 25 seconds",
			},
		)

		return
	}

	session.mu.RLock()

	var diagnostic interface{}
	var h264Diagnostic interface{}

	if len(session.Segments) > 0 {
		diagnostic =
			session.Segments[len(session.Segments)-1].Diagnostics
	}

	h264Diagnostic =
		session.h264Diagnostics

	generation :=
		session.currentGeneration

	session.mu.RUnlock()

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status":
				"streaming",

			"camera":
				cameraIndex,

			"name":
				camera.Name,

			"total":
				len(cameras),

			"hls":
				"/live/index.m3u8",

			"version":
				15,

			"videoPackets":
				atomic.LoadUint64(
					&session.Stats.VideoPackets,
				),

			"accessUnits":
				atomic.LoadUint64(
					&session.Stats.AccessUnits,
				),

			"segments":
				atomic.LoadUint64(
					&session.Stats.HLSSegments,
				),

			"emptyPackets":
				atomic.LoadUint64(
					&session.Stats.EmptyPackets,
				),

			"validatedTS":
				atomic.LoadUint64(
					&session.Stats.ValidatedTS,
				),

			"validationErrors":
				atomic.LoadUint64(
					&session.Stats.ValidationError,
				),

			"timestampDiscontinuities":
				atomic.LoadUint64(
					&session.Stats.TimestampDiscontinuities,
				),

			"codecChanges":
				atomic.LoadUint64(
					&session.Stats.CodecChanges,
				),

			"generation":
				generation,

			"lastDiagnostic":
				diagnostic,

			"h264Diagnostic":
				h264Diagnostic,

			"codecTransitions":
				"/debug/h264",
		},
	)
}

func statusHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session :=
		getSession()

	if session == nil {
		writeJSON(
			w,
			200,
			map[string]interface{}{
				"streaming":
					false,

				"version":
					15,
			},
		)

		return
	}

	session.mu.RLock()

	hasSPS :=
		len(session.sps) > 0

	hasPPS :=
		len(session.pps) > 0

	pts :=
		session.normalizedPTS

	nalSummary :=
		session.lastNALSummary

	h264 :=
		session.h264Diagnostics

	activeCodec :=
		session.activeCodec

	generation :=
		session.currentGeneration

	pendingDiscontinuity :=
		session.pendingDiscontinuity

	changes :=
		append(
			[]CodecChange(nil),
			session.codecChanges...,
		)

	var lastSegment interface{}

	if len(session.Segments) > 0 {
		seg :=
			session.Segments[len(session.Segments)-1]

		lastSegment =
			map[string]interface{}{
				"sequence":
					seg.Sequence,

				"duration":
					seg.Duration,

				"bytes":
					len(seg.Data),

				"validated":
					seg.Validated,

				"validationErr":
					seg.ValidateErr,

				"diagnostics":
					seg.Diagnostics,

				"codec":
					seg.Codec,

				"generation":
					seg.Generation,

				"discontinuityBefore":
					seg.DiscontinuityBefore,
			}
	}

	session.mu.RUnlock()

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"streaming":
				true,

			"version":
				15,

			"camera":
				session.Index,

			"name":
				session.Camera.Name,

			"videoPackets":
				atomic.LoadUint64(
					&session.Stats.VideoPackets,
				),

			"videoBytes":
				atomic.LoadUint64(
					&session.Stats.VideoBytes,
				),

			"accessUnits":
				atomic.LoadUint64(
					&session.Stats.AccessUnits,
				),

			"segments":
				atomic.LoadUint64(
					&session.Stats.HLSSegments,
				),

			"audioPackets":
				atomic.LoadUint64(
					&session.Stats.AudioPackets,
				),

			"emptyPackets":
				atomic.LoadUint64(
					&session.Stats.EmptyPackets,
				),

			"validatedTS":
				atomic.LoadUint64(
					&session.Stats.ValidatedTS,
				),

			"validationErrors":
				atomic.LoadUint64(
					&session.Stats.ValidationError,
				),

			"timestampDiscontinuities":
				atomic.LoadUint64(
					&session.Stats.TimestampDiscontinuities,
				),

			"codecChanges":
				atomic.LoadUint64(
					&session.Stats.CodecChanges,
				),

			"sps":
				hasSPS,

			"pps":
				hasPPS,

			"normalizedPTS":
				pts,

			"lastNALTypes":
				nalSummary,

			"h264":
				h264,

			"activeCodec":
				activeCodec,

			"generation":
				generation,

			"pendingDiscontinuity":
				pendingDiscontinuity,

			"codecHistory":
				changes,

			"lastSegment":
				lastSegment,

			"codecTransitions":
				"/debug/h264",
		},
	)
}

func h264DiagnosticsHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session :=
		getSession()

	if session == nil {
		writeJSON(
			w,
			404,
			map[string]interface{}{
				"error":
					"no active stream",

				"version":
					15,
			},
		)

		return
	}

	session.mu.RLock()

	h264 :=
		session.h264Diagnostics

	activeCodec :=
		session.activeCodec

	generation :=
		session.currentGeneration

	changes :=
		append(
			[]CodecChange(nil),
			session.codecChanges...,
		)

	segments :=
		make(
			[]map[string]interface{},
			0,
			len(session.Segments),
		)

	for _, segment :=
		range session.Segments {

		segments =
			append(
				segments,
				map[string]interface{}{
					"sequence":
						segment.Sequence,

					"duration":
						segment.Duration,

					"bytes":
						len(segment.Data),

					"codec":
						segment.Codec,

					"generation":
						segment.Generation,

					"discontinuityBefore":
						segment.DiscontinuityBefore,

					"validated":
						segment.Validated,
				},
			)
	}

	session.mu.RUnlock()

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"version":
				15,

			"camera":
				session.Index,

			"name":
				session.Camera.Name,

			"activeCodec":
				activeCodec,

			"h264":
				h264,

			"generation":
				generation,

			"codecChanges":
				atomic.LoadUint64(
					&session.Stats.CodecChanges,
				),

			"history":
				changes,

			"segments":
				segments,
		},
	)
}

func diagnosticsHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session :=
		getSession()

	if session == nil {
		writeJSON(
			w,
			404,
			map[string]interface{}{
				"error":
					"no active stream",

				"version":
					15,
			},
		)

		return
	}

	session.mu.RLock()

	if len(session.Segments) == 0 {
		session.mu.RUnlock()

		writeJSON(
			w,
			503,
			map[string]interface{}{
				"error":
					"no completed segment yet",

				"version":
					15,
			},
		)

		return
	}

	segments :=
		make(
			[]map[string]interface{},
			0,
			len(session.Segments),
		)

	for _, segment :=
		range session.Segments {

		segments =
			append(
				segments,
				map[string]interface{}{
					"sequence":
						segment.Sequence,

					"duration":
						segment.Duration,

					"bytes":
						len(segment.Data),

					"validated":
						segment.Validated,

					"validationErr":
						segment.ValidateErr,

					"diagnostics":
						segment.Diagnostics,

					"codec":
						segment.Codec,

					"generation":
						segment.Generation,

					"discontinuityBefore":
						segment.DiscontinuityBefore,

					"url":
						fmt.Sprintf(
							"/live/segment%d.ts",
							segment.Sequence,
						),
				},
			)
	}

	last :=
		session.Segments[len(session.Segments)-1]

	session.mu.RUnlock()

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"version":
				15,

			"camera":
				session.Index,

			"name":
				session.Camera.Name,

			"segments":
				segments,

			"latestSequence":
				last.Sequence,

			"playlist":
				"/live/index.m3u8",

			"validatedTS":
				atomic.LoadUint64(
					&session.Stats.ValidatedTS,
				),

			"validationErrors":
				atomic.LoadUint64(
					&session.Stats.ValidationError,
				),

			"codecChanges":
				atomic.LoadUint64(
					&session.Stats.CodecChanges,
				),
		},
	)
}

func playlistHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session :=
		getSession()

	if session == nil {
		http.Error(
			w,
			"no active stream",
			http.StatusNotFound,
		)

		return
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	if len(session.Segments) == 0 {
		http.Error(
			w,
			"HLS not ready",
			http.StatusServiceUnavailable,
		)

		return
	}

	firstSequence :=
		session.Segments[0].Sequence

	targetDuration := 1

	for _, segment :=
		range session.Segments {

		duration :=
			int(
				math.Ceil(
					segment.Duration,
				),
			)

		if duration >
			targetDuration {

			targetDuration =
				duration
		}
	}

	var playlist strings.Builder

	playlist.WriteString(
		"#EXTM3U\n",
	)

	playlist.WriteString(
		"#EXT-X-VERSION:3\n",
	)

	playlist.WriteString(
		"#EXT-X-TARGETDURATION:" +
			strconv.Itoa(
				targetDuration,
			) +
			"\n",
	)

	playlist.WriteString(
		"#EXT-X-MEDIA-SEQUENCE:" +
			strconv.Itoa(
				firstSequence,
			) +
			"\n",
	)

	playlist.WriteString(
		"#EXT-X-INDEPENDENT-SEGMENTS\n",
	)

	for _, segment :=
		range session.Segments {

		if segment.DiscontinuityBefore {
			playlist.WriteString(
				"#EXT-X-DISCONTINUITY\n",
			)
		}

		playlist.WriteString(
			fmt.Sprintf(
				"#EXTINF:%.3f,\n",
				segment.Duration,
			),
		)

		playlist.WriteString(
			fmt.Sprintf(
				"segment%d.ts\n",
				segment.Sequence,
			),
		)
	}

	atomic.AddUint64(
		&session.Stats.PlaylistGenerations,
		1,
	)

	w.Header().Set(
		"Content-Type",
		"application/vnd.apple.mpegurl",
	)

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate",
	)

	w.Header().Set(
		"Access-Control-Allow-Origin",
		"*",
	)

	_, _ =
		w.Write(
			[]byte(
				playlist.String(),
			),
		)
}

func segmentHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session :=
		getSession()

	if session == nil {
		http.NotFound(
			w,
			r,
		)

		return
	}

	name :=
		strings.TrimPrefix(
			r.URL.Path,
			"/live/segment",
		)

	name =
		strings.TrimSuffix(
			name,
			".ts",
		)

	sequence, err :=
		strconv.Atoi(
			name,
		)

	if err != nil {
		http.NotFound(
			w,
			r,
		)

		return
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	for _, segment :=
		range session.Segments {

		if segment.Sequence ==
			sequence {

			w.Header().Set(
				"Content-Type",
				"video/mp2t",
			)

			w.Header().Set(
				"Cache-Control",
				"no-store, no-cache, must-revalidate",
			)

			w.Header().Set(
				"Access-Control-Allow-Origin",
				"*",
			)

			w.Header().Set(
				"Content-Length",
				strconv.Itoa(
					len(
						segment.Data,
					),
				),
			)

			_, _ =
				w.Write(
					segment.Data,
				)

			return
		}
	}

	http.NotFound(
		w,
		r,
	)
}

func stopHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	sessionMu.Lock()

	session :=
		currentSession

	currentSession =
		nil

	sessionMu.Unlock()

	if session != nil {
		session.Close()
	}

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status":
				"stopped",

			"version":
				15,
		},
	)
}

func main() {
	http.HandleFunc(
		"/health",
		health,
	)

	http.HandleFunc(
		"/start",
		start,
	)

	http.HandleFunc(
		"/status",
		statusHandler,
	)

	http.HandleFunc(
		"/stop",
		stopHandler,
	)

	http.HandleFunc(
		"/debug/ts",
		diagnosticsHandler,
	)

	http.HandleFunc(
		"/debug/h264",
		h264DiagnosticsHandler,
	)

	http.HandleFunc(
		"/live/index.m3u8",
		playlistHandler,
	)

	http.HandleFunc(
		"/live/segment",
		segmentHandler,
	)

	http.HandleFunc(
		"/",
		func(
			w http.ResponseWriter,
			r *http.Request,
		) {
			writeJSON(
				w,
				200,
				map[string]interface{}{
					"name":
						"NestView TV Roku Media Bridge",

					"engine":
						"Pion WebRTC -> H264 -> MPEG-TS -> HLS",

					"h264":
						true,

					"hls":
						true,

					"keyframeSegments":
						true,

					"parameterSetCache":
						true,

					"normalizedTimestamps":
						true,

					"codecTransitionHandling":
						true,

					"hlsDiscontinuity":
						true,

					"codecDiagnostics":
						"/debug/h264",

					"version":
						15,
				},
			)
		},
	)

	log.Println(
		"NestView TV Pion HLS Bridge VERSION 15 running on port " +
			port,
	)

	log.Fatal(
		http.ListenAndServe(
			":"+port,
			nil,
		),
	)
}
