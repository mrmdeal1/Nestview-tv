package main

import (
	"bytes"
	"encoding/base64"
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

	AccessUnits  uint64
	HLSSegments  uint64
	EmptyPackets uint64

	ValidatedTS     uint64
	ValidationError uint64

	TimestampDiscontinuities uint64
	CodecChanges             uint64
	PlaylistGenerations      uint64

	DuplicateSPSRemoved uint64
	DuplicatePPSRemoved uint64
	CleanedAccessUnits  uint64

	SliceHeadersParsed uint64
	SliceParseErrors   uint64

	ISlices uint64
	PSlices uint64
	BSlices uint64

	POCBackwardEvents uint64

	/*
		V19 diagnostics.

		These counters tell us exactly what framing is
		being delivered to the MPEG-TS writer.
	*/
	MuxAccessUnits uint64
	MuxNALUnits    uint64
	MuxIDRUnits    uint64

	AnnexBRepairs uint64
}

type TSDiagnostics struct {
	Valid bool `json:"valid"`

	Bytes   int `json:"bytes"`
	Packets int `json:"packets"`

	SyncErrors int `json:"syncErrors"`

	PATPackets int `json:"patPackets"`
	PMTPackets int `json:"pmtPackets"`

	VideoPackets int `json:"videoPackets"`
	NullPackets  int `json:"nullPackets"`

	TransportErrors int `json:"transportErrors"`
	PayloadPackets  int `json:"payloadPackets"`

	AdaptationPackets int `json:"adaptationPackets"`
	PCRPackets        int `json:"pcrPackets"`

	DiscontinuityFlags int `json:"discontinuityFlags"`
	ContinuityErrors   int `json:"continuityErrors"`

	PUSIPackets int `json:"pusiPackets"`

	PESStartCodes int `json:"pesStartCodes"`

	PTSCount int `json:"ptsCount"`
	DTSCount int `json:"dtsCount"`

	FirstPTS int64 `json:"firstPTS"`
	LastPTS  int64 `json:"lastPTS"`
	MinPTS   int64 `json:"minPTS"`
	MaxPTS   int64 `json:"maxPTS"`

	BackwardPTS int `json:"backwardPTS"`

	FirstPCR int64 `json:"firstPCR"`
	LastPCR  int64 `json:"lastPCR"`

	PMTPID     int `json:"pmtPID"`
	VideoPID   int `json:"videoPID"`
	StreamType int `json:"streamType"`

	FirstPacketHex string `json:"firstPacketHex,omitempty"`

	Error string `json:"error,omitempty"`
}

type H264Diagnostics struct {
	Valid bool `json:"valid"`

	Profile    string `json:"profile"`
	ProfileIDC int    `json:"profileIDC"`

	ConstraintFlags int `json:"constraintFlags"`

	Level    string `json:"level"`
	LevelIDC int    `json:"levelIDC"`

	SPSID uint64 `json:"spsID"`

	ChromaFormatIDC uint64 `json:"chromaFormatIDC"`

	SeparateColourPlaneFlag bool `json:"separateColourPlaneFlag"`

	BitDepthLuma   int `json:"bitDepthLuma"`
	BitDepthChroma int `json:"bitDepthChroma"`

	Log2MaxFrameNum int `json:"log2MaxFrameNum"`

	PicOrderCntType uint64 `json:"picOrderCntType"`

	Log2MaxPicOrderCntLSB int `json:"log2MaxPicOrderCntLSB"`

	MaxNumRefFrames uint64 `json:"maxNumRefFrames"`

	PicWidthInMbs       int `json:"picWidthInMbs"`
	PicHeightInMapUnits int `json:"picHeightInMapUnits"`

	FrameMbsOnly bool `json:"frameMbsOnly"`

	FrameCropLeft   uint64 `json:"frameCropLeft"`
	FrameCropRight  uint64 `json:"frameCropRight"`
	FrameCropTop    uint64 `json:"frameCropTop"`
	FrameCropBottom uint64 `json:"frameCropBottom"`

	CodedWidth  int `json:"codedWidth"`
	CodedHeight int `json:"codedHeight"`

	DisplayWidth  int `json:"displayWidth"`
	DisplayHeight int `json:"displayHeight"`

	VUIParametersPresent     bool    `json:"vuiParametersPresent"`
	AspectRatioInfoPresent   bool    `json:"aspectRatioInfoPresent"`
	AspectRatioIDC           uint64  `json:"aspectRatioIDC"`
	SARWidth                 uint64  `json:"sarWidth"`
	SARHeight                uint64  `json:"sarHeight"`
	VideoSignalTypePresent   bool    `json:"videoSignalTypePresent"`
	VideoFormat              uint64  `json:"videoFormat"`
	VideoFullRange           bool    `json:"videoFullRange"`
	ColourDescriptionPresent bool    `json:"colourDescriptionPresent"`
	ColourPrimaries          uint64  `json:"colourPrimaries"`
	TransferCharacteristics  uint64  `json:"transferCharacteristics"`
	MatrixCoefficients       uint64  `json:"matrixCoefficients"`
	TimingInfoPresent        bool    `json:"timingInfoPresent"`
	NumUnitsInTick           uint64  `json:"numUnitsInTick"`
	TimeScale                uint64  `json:"timeScale"`
	FixedFrameRate           bool    `json:"fixedFrameRate"`
	NominalFrameRate         float64 `json:"nominalFrameRate"`
	BitstreamRestriction     bool    `json:"bitstreamRestriction"`
	MaxNumReorderFrames      uint64  `json:"maxNumReorderFrames"`
	MaxDecFrameBuffering     uint64  `json:"maxDecFrameBuffering"`
	VUIParseError            string  `json:"vuiParseError,omitempty"`

	SPSBytes int `json:"spsBytes"`
	PPSBytes int `json:"ppsBytes"`

	SPSHex string `json:"spsHex,omitempty"`
	PPSHex string `json:"ppsHex,omitempty"`

	SafariBaselineCompatible bool `json:"baseline8bit420"`

	Error string `json:"error,omitempty"`
}

type H264SliceDiagnostics struct {
	Valid bool `json:"valid"`

	NALType int `json:"nalType"`

	IDR bool `json:"idr"`

	NALRefIDC int `json:"nalRefIDC"`

	FirstMBInSlice uint64 `json:"firstMBInSlice"`

	SliceTypeRaw uint64 `json:"sliceTypeRaw"`
	SliceType    string `json:"sliceType"`

	PicParameterSetID uint64 `json:"picParameterSetID"`

	FrameNum uint64 `json:"frameNum"`

	FieldPicFlag    bool `json:"fieldPicFlag"`
	BottomFieldFlag bool `json:"bottomFieldFlag"`

	IDRPicID uint64 `json:"idrPicID"`

	PicOrderCntType uint64 `json:"picOrderCntType"`
	PicOrderCntLSB  uint64 `json:"picOrderCntLSB"`

	HasPicOrderCntLSB bool `json:"hasPicOrderCntLSB"`

	PTS int64 `json:"pts"`

	PreviousPOC int64 `json:"previousPOC"`

	POCBackward bool `json:"pocBackward"`

	PotentialReordering bool `json:"potentialReordering"`

	Error string `json:"error,omitempty"`
}

type CodecSignature struct {
	ProfileIDC int `json:"profileIDC"`
	LevelIDC   int `json:"levelIDC"`

	Width  int `json:"width"`
	Height int `json:"height"`

	ChromaFormatIDC uint64 `json:"chromaFormatIDC"`

	BitDepthLuma   int `json:"bitDepthLuma"`
	BitDepthChroma int `json:"bitDepthChroma"`

	SPSHex string `json:"spsHex,omitempty"`
	PPSHex string `json:"ppsHex,omitempty"`
}

type CodecChange struct {
	Number uint64 `json:"number"`

	Time string `json:"time"`

	PTS int64 `json:"pts"`

	From CodecSignature `json:"from"`
	To   CodecSignature `json:"to"`

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

type DebugSegmentSnapshot struct {
	mu sync.RWMutex

	Ready bool

	CreatedAt string

	CameraIndex int

	Segment HLSSegment
}

type FrozenVOD struct {
	mu sync.RWMutex

	Ready bool

	CreatedAt string

	CameraIndex int
	CameraName  string

	Segments []HLSSegment

	TotalDuration float64

	Codec CodecSignature

	Generation uint64
}

type StreamSession struct {
	mu sync.RWMutex

	PC *webrtc.PeerConnection

	Stats *MediaStats

	Camera Camera
	Index  int

	Segments []HLSSegment

	NextSequence int

	currentBuffer *bytes.Buffer
	currentWriter *mpegts.Writer

	segmentStart  time.Time
	segmentActive bool

	sps []byte
	pps []byte

	h264Diagnostics      H264Diagnostics
	h264SliceDiagnostics H264SliceDiagnostics

	h264Logged bool

	activeCodec     CodecSignature
	haveActiveCodec bool

	currentSegmentCodec CodecSignature

	currentGeneration uint64

	pendingDiscontinuity bool

	codecChanges []CodecChange

	timestampStarted bool
	lastRTPTimestamp uint32
	normalizedPTS    int64

	lastNALSummary        string
	lastCleanedNALSummary string

	/*
		V19 records the exact NAL layout handed to
		the MPEG-TS writer.
	*/
	lastMuxNALSummary string
	lastMuxBytes      int
	lastMuxPrefixHex  string

	haveLastPOC bool
	lastPOC     uint64

	ready chan struct{}
	done  chan struct{}

	readyOnce sync.Once
	closeOnce sync.Once
}

var (
	sessionMu sync.RWMutex

	currentSession *StreamSession

	frozenVOD FrozenVOD

	debugSegmentSnapshot DebugSegmentSnapshot
)

func copyHLSSegment(source HLSSegment) HLSSegment {
	copied := source
	copied.Data = append([]byte(nil), source.Data...)
	return copied
}

func buildFrozenVOD(session *StreamSession) (int, float64) {
	if session == nil {
		return 0, 0
	}

	session.mu.RLock()

	if len(session.Segments) == 0 {
		session.mu.RUnlock()
		return 0, 0
	}

	last := session.Segments[len(session.Segments)-1]
	generation := last.Generation
	codec := last.Codec

	copied := make([]HLSSegment, 0, len(session.Segments))
	totalDuration := 0.0

	for _, segment := range session.Segments {
		if segment.Generation != generation {
			continue
		}

		if !segment.Validated {
			continue
		}

		if len(segment.Data) == 0 {
			continue
		}

		frozen := copyHLSSegment(segment)
		frozen.DiscontinuityBefore = false

		copied = append(copied, frozen)
		totalDuration += frozen.Duration
	}

	cameraIndex := session.Index
	cameraName := session.Camera.Name

	session.mu.RUnlock()

	if len(copied) == 0 {
		return 0, 0
	}

	frozenVOD.mu.Lock()

	frozenVOD.Ready = true
	frozenVOD.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	frozenVOD.CameraIndex = cameraIndex
	frozenVOD.CameraName = cameraName
	frozenVOD.Segments = copied
	frozenVOD.TotalDuration = totalDuration
	frozenVOD.Codec = codec
	frozenVOD.Generation = generation

	frozenVOD.mu.Unlock()

	log.Printf(
		"VERSION 58 FROZEN VOD READY: camera=%d name=%s generation=%d segments=%d duration=%.3f codec=[%s]",
		cameraIndex,
		cameraName,
		generation,
		len(copied),
		totalDuration,
		codecSignatureString(codec),
	)

	return len(copied), totalDuration
}

func clearFrozenVOD() {
	frozenVOD.mu.Lock()

	frozenVOD.Ready = false
	frozenVOD.CreatedAt = ""
	frozenVOD.CameraIndex = 0
	frozenVOD.CameraName = ""
	frozenVOD.Segments = nil
	frozenVOD.TotalDuration = 0
	frozenVOD.Codec = CodecSignature{}
	frozenVOD.Generation = 0

	frozenVOD.mu.Unlock()
}

func env(key string, fallback string) string {
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(value)
}

func firstString(
	m map[string]interface{},
	keys ...string,
) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && value != "" {
			return value
		}
	}

	return ""
}

func getCameras() ([]Camera, error) {
	client := &http.Client{
		Timeout: 20 * time.Second,
	}

	var body []byte
	var lastErr error

	for attempt := 1; attempt <= 3; attempt++ {
		log.Printf(
			"VERSION 58 camera backend request attempt=%d url=%s/api/cameras",
			attempt,
			nestBackend,
		)

		resp, err := client.Get(nestBackend + "/api/cameras")

		if err != nil {
			lastErr = err
			log.Printf(
				"VERSION 58 camera backend request failed attempt=%d error=%v",
				attempt,
				err,
			)
		} else {
			responseBody, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()

			if readErr != nil {
				lastErr = readErr
				log.Printf(
					"VERSION 58 camera backend read failed attempt=%d error=%v",
					attempt,
					readErr,
				)
			} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				lastErr = fmt.Errorf(
					"camera backend returned %d: %s",
					resp.StatusCode,
					string(responseBody),
				)

				log.Printf(
					"VERSION 58 camera backend HTTP failure attempt=%d status=%d body=%q",
					attempt,
					resp.StatusCode,
					string(responseBody),
				)
			} else {
				body = responseBody

				log.Printf(
					"VERSION 58 camera backend success attempt=%d status=%d bytes=%d",
					attempt,
					resp.StatusCode,
					len(body),
				)

				break
			}
		}

		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	if len(body) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("camera backend returned no data")
		}

		return nil, fmt.Errorf(
			"camera backend unavailable after 3 attempts: %w",
			lastErr,
		)
	}

	var raw interface{}

	if err := json.Unmarshal(body, &raw); err != nil {
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
			if candidate, ok := value[key].([]interface{}); ok {
				list = candidate
				break
			}
		}
	}

	var cameras []Camera

	for _, item := range list {
		m, ok := item.(map[string]interface{})

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

		if strings.Contains(device, "/devices/") {
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
		return nil, fmt.Errorf("no cameras found")
	}

	return cameras, nil
}

func findAnswerSDP(value interface{}) string {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, item := range v {
			lowerKey := strings.ToLower(key)

			if lowerKey == "answersdp" ||
				lowerKey == "answer_sdp" ||
				lowerKey == "answer" ||
				lowerKey == "sdp" {

				if text, ok := item.(string); ok &&
					strings.Contains(text, "v=0") {

					return text
				}
			}
		}

		for _, item := range v {
			if answer := findAnswerSDP(item); answer != "" {
				return answer
			}
		}

	case []interface{}:
		for _, item := range v {
			if answer := findAnswerSDP(item); answer != "" {
				return answer
			}
		}

	case string:
		if strings.Contains(v, "v=0") &&
			strings.Contains(v, "m=video") {

			return v
		}
	}

	return ""
}

func waitForICE(pc *webrtc.PeerConnection) {
	done := webrtc.GatheringCompletePromise(pc)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

/*
	V19 H264 framing helpers.

	The Pion H264 depacketizer normally returns Annex-B,
	but V19 verifies that assumption before anything reaches
	the MPEG-TS writer.
*/

func hasAnnexBStartCode(data []byte) bool {
	if len(data) >= 4 &&
		data[0] == 0 &&
		data[1] == 0 &&
		data[2] == 0 &&
		data[3] == 1 {

		return true
	}

	if len(data) >= 3 &&
		data[0] == 0 &&
		data[1] == 0 &&
		data[2] == 1 {

		return true
	}

	return false
}

func splitAnnexB(data []byte) [][]byte {
	var nalus [][]byte

	findStart := func(from int) (int, int) {
		for i := from; i < len(data); i++ {
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
		start, prefix := findStart(pos)

		if start < 0 {
			break
		}

		naluStart := start + prefix
		next, _ := findStart(naluStart)

		naluEnd := len(data)

		if next >= 0 {
			naluEnd = next
		}

		if naluStart < naluEnd {
			nalus = append(
				nalus,
				append([]byte(nil), data[naluStart:naluEnd]...),
			)
		}

		if next < 0 {
			break
		}

		pos = next
	}

	return nalus
}

func annexB(nalu []byte) []byte {
	if len(nalu) == 0 {
		return nil
	}

	out := make([]byte, 4+len(nalu))

	out[0] = 0
	out[1] = 0
	out[2] = 0
	out[3] = 1

	copy(out[4:], nalu)

	return out
}

/*
normalizeAnnexBAccessUnit rebuilds every access unit with
one canonical four-byte Annex-B start code before every NAL.

This is the first V19 media-path change.
*/
func normalizeAnnexBAccessUnit(
	data []byte,
) (
	[]byte,
	bool,
) {
	nalus := splitAnnexB(data)

	if len(nalus) == 0 {
		return data, false
	}

	var out []byte

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}

		out = append(out, annexB(nalu)...)
	}

	if len(out) == 0 {
		return data, false
	}

	changed := !bytes.Equal(out, data)

	return out, changed
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
	for _, nalu := range splitAnnexB(data) {
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

func canonicalizeH264AccessUnit(
	data []byte,
) (
	cleaned []byte,
	duplicateSPS int,
	duplicatePPS int,
	changed bool,
) {
	nalus := splitAnnexB(data)

	if len(nalus) == 0 {
		return data, 0, 0, false
	}

	var lastSPS []byte
	var lastPPS []byte

	spsCount := 0
	ppsCount := 0

	otherNALUs := make([][]byte, 0, len(nalus))

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}

		nalType := nalu[0] & 0x1F

		switch nalType {
		case 7:
			spsCount++
			lastSPS = append([]byte(nil), nalu...)

		case 8:
			ppsCount++
			lastPPS = append([]byte(nil), nalu...)

		default:
			otherNALUs = append(otherNALUs, nalu)
		}
	}

	if spsCount > 1 {
		duplicateSPS = spsCount - 1
	}

	if ppsCount > 1 {
		duplicatePPS = ppsCount - 1
	}

	if duplicateSPS == 0 && duplicatePPS == 0 {
		return data, 0, 0, false
	}

	if len(lastSPS) > 0 {
		cleaned = append(cleaned, annexB(lastSPS)...)
	}

	if len(lastPPS) > 0 {
		cleaned = append(cleaned, annexB(lastPPS)...)
	}

	for _, nalu := range otherNALUs {
		cleaned = append(cleaned, annexB(nalu)...)
	}

	if len(cleaned) == 0 {
		return data, 0, 0, false
	}

	return cleaned,
		duplicateSPS,
		duplicatePPS,
		true
}

func naluSummary(data []byte) string {
	nalus := splitAnnexB(data)

	if len(nalus) == 0 {
		return "no-annexb-nalus"
	}

	types := make([]string, 0, len(nalus))

	for _, nalu := range nalus {
		if len(nalu) > 0 {
			types = append(
				types,
				strconv.Itoa(int(nalu[0]&0x1F)),
			)
		}
	}

	if len(types) == 0 {
		return "empty-nalus"
	}

	return strings.Join(types, ",")
}
func stripAnnexBStartCode(data []byte) []byte {
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

func removeEmulationPrevention(data []byte) []byte {
	out := make([]byte, 0, len(data))

	zeroCount := 0

	for _, value := range data {
		if zeroCount >= 2 && value == 0x03 {
			zeroCount = 0
			continue
		}

		out = append(out, value)

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

func (b *bitReader) readBit() (uint64, error) {
	if b.pos >= len(b.data)*8 {
		return 0, io.ErrUnexpectedEOF
	}

	byteIndex := b.pos / 8
	bitIndex := 7 - (b.pos % 8)

	value := uint64(
		(b.data[byteIndex] >> bitIndex) & 1,
	)

	b.pos++

	return value, nil
}

func (b *bitReader) readBits(
	count int,
) (uint64, error) {
	var value uint64

	for i := 0; i < count; i++ {
		bit, err := b.readBit()

		if err != nil {
			return 0, err
		}

		value = (value << 1) | bit
	}

	return value, nil
}

func (b *bitReader) readUE() (uint64, error) {
	zeros := 0

	for {
		bit, err := b.readBit()

		if err != nil {
			return 0, err
		}

		if bit == 1 {
			break
		}

		zeros++

		if zeros > 63 {
			return 0, fmt.Errorf("Exp-Golomb overflow")
		}
	}

	if zeros == 0 {
		return 0, nil
	}

	suffix, err := b.readBits(zeros)

	if err != nil {
		return 0, err
	}

	return (uint64(1) << zeros) - 1 + suffix, nil
}

func (b *bitReader) readSE() (int64, error) {
	value, err := b.readUE()

	if err != nil {
		return 0, err
	}

	if value%2 == 0 {
		return -int64(value / 2), nil
	}

	return int64((value + 1) / 2), nil
}

func skipScalingList(
	b *bitReader,
	size int,
) error {
	lastScale := int64(8)
	nextScale := int64(8)

	for j := 0; j < size; j++ {
		if nextScale != 0 {
			deltaScale, err := b.readSE()

			if err != nil {
				return err
			}

			nextScale = (lastScale + deltaScale + 256) % 256
		}

		if nextScale != 0 {
			lastScale = nextScale
		}
	}

	return nil
}

func profileName(profileIDC int) string {
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

func levelName(levelIDC int) string {
	if levelIDC <= 0 {
		return "unknown"
	}

	major := levelIDC / 10
	minor := levelIDC % 10

	return fmt.Sprintf(
		"%d.%d",
		major,
		minor,
	)
}

func sliceTypeName(sliceType uint64) string {
	switch sliceType % 5 {
	case 0:
		return "P"

	case 1:
		return "B"

	case 2:
		return "I"

	case 3:
		return "SP"

	case 4:
		return "SI"

	default:
		return "unknown"
	}
}

func parseH264SliceHeader(
	nalu []byte,
	h264 H264Diagnostics,
	pts int64,
) H264SliceDiagnostics {
	d := H264SliceDiagnostics{
		PTS:         pts,
		PreviousPOC: -1,
	}

	if len(nalu) < 2 {
		d.Error = "slice NAL too short"
		return d
	}

	nalHeader := nalu[0]
	nalType := int(nalHeader & 0x1F)

	if nalType != 1 && nalType != 5 {
		d.Error = fmt.Sprintf(
			"NAL type %d is not a slice",
			nalType,
		)

		return d
	}

	d.NALType = nalType
	d.IDR = nalType == 5
	d.NALRefIDC = int((nalHeader >> 5) & 0x03)

	rbsp := removeEmulationPrevention(nalu[1:])

	if len(rbsp) == 0 {
		d.Error = "slice RBSP is empty"
		return d
	}

	b := &bitReader{
		data: rbsp,
	}

	firstMB, err := b.readUE()

	if err != nil {
		d.Error = "first_mb_in_slice: " + err.Error()
		return d
	}

	d.FirstMBInSlice = firstMB

	sliceType, err := b.readUE()

	if err != nil {
		d.Error = "slice_type: " + err.Error()
		return d
	}

	d.SliceTypeRaw = sliceType
	d.SliceType = sliceTypeName(sliceType)

	ppsID, err := b.readUE()

	if err != nil {
		d.Error = "pic_parameter_set_id: " + err.Error()
		return d
	}

	d.PicParameterSetID = ppsID

	if h264.SeparateColourPlaneFlag {
		_, err = b.readBits(2)

		if err != nil {
			d.Error = "colour_plane_id: " + err.Error()
			return d
		}
	}

	if h264.Log2MaxFrameNum <= 0 ||
		h264.Log2MaxFrameNum > 32 {

		d.Error = fmt.Sprintf(
			"invalid log2_max_frame_num: %d",
			h264.Log2MaxFrameNum,
		)

		return d
	}

	frameNum, err := b.readBits(h264.Log2MaxFrameNum)

	if err != nil {
		d.Error = "frame_num: " + err.Error()
		return d
	}

	d.FrameNum = frameNum

	if !h264.FrameMbsOnly {
		fieldPicFlag, readErr := b.readBit()

		if readErr != nil {
			d.Error = "field_pic_flag: " + readErr.Error()
			return d
		}

		d.FieldPicFlag = fieldPicFlag != 0

		if d.FieldPicFlag {
			bottomFieldFlag, readErr := b.readBit()

			if readErr != nil {
				d.Error = "bottom_field_flag: " + readErr.Error()
				return d
			}

			d.BottomFieldFlag = bottomFieldFlag != 0
		}
	}

	if d.IDR {
		idrPicID, readErr := b.readUE()

		if readErr != nil {
			d.Error = "idr_pic_id: " + readErr.Error()
			return d
		}

		d.IDRPicID = idrPicID
	}

	d.PicOrderCntType = h264.PicOrderCntType

	if h264.PicOrderCntType == 0 {
		if h264.Log2MaxPicOrderCntLSB <= 0 ||
			h264.Log2MaxPicOrderCntLSB > 32 {

			d.Error = fmt.Sprintf(
				"invalid log2_max_pic_order_cnt_lsb: %d",
				h264.Log2MaxPicOrderCntLSB,
			)

			return d
		}

		pocLSB, readErr := b.readBits(
			h264.Log2MaxPicOrderCntLSB,
		)

		if readErr != nil {
			d.Error = "pic_order_cnt_lsb: " + readErr.Error()
			return d
		}

		d.PicOrderCntLSB = pocLSB
		d.HasPicOrderCntLSB = true
	}

	d.PotentialReordering = d.SliceType == "B"
	d.Valid = true

	return d
}

func (s *StreamSession) inspectSliceOrderingLocked(
	accessUnit []byte,
	pts int64,
) {
	nalus := splitAnnexB(accessUnit)

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}

		nalType := nalu[0] & 0x1F

		if nalType != 1 && nalType != 5 {
			continue
		}

		diagnostic := parseH264SliceHeader(
			nalu,
			s.h264Diagnostics,
			pts,
		)

		if !diagnostic.Valid {
			atomic.AddUint64(
				&s.Stats.SliceParseErrors,
				1,
			)

			s.h264SliceDiagnostics = diagnostic

			log.Printf(
				"VERSION 58 H264 SLICE PARSE ERROR: NAL=%d PTS=%d error=%s",
				nalType,
				pts,
				diagnostic.Error,
			)

			return
		}

		atomic.AddUint64(
			&s.Stats.SliceHeadersParsed,
			1,
		)

		switch diagnostic.SliceType {
		case "I":
			atomic.AddUint64(
				&s.Stats.ISlices,
				1,
			)

		case "P":
			atomic.AddUint64(
				&s.Stats.PSlices,
				1,
			)

		case "B":
			atomic.AddUint64(
				&s.Stats.BSlices,
				1,
			)
		}

		if diagnostic.HasPicOrderCntLSB {
			if s.haveLastPOC {
				diagnostic.PreviousPOC = int64(s.lastPOC)

				if diagnostic.PicOrderCntLSB < s.lastPOC &&
					!diagnostic.IDR {

					diagnostic.POCBackward = true

					atomic.AddUint64(
						&s.Stats.POCBackwardEvents,
						1,
					)
				}
			}

			s.lastPOC = diagnostic.PicOrderCntLSB
			s.haveLastPOC = true
		}

		if diagnostic.IDR {
			s.haveLastPOC = diagnostic.HasPicOrderCntLSB

			if diagnostic.HasPicOrderCntLSB {
				s.lastPOC = diagnostic.PicOrderCntLSB
			}
		}

		s.h264SliceDiagnostics = diagnostic

		if diagnostic.SliceType == "B" ||
			diagnostic.POCBackward {

			log.Printf(
				"VERSION 58 H264 REORDER SIGNAL: slice=%s frameNum=%d POC=%d previousPOC=%d POCBackward=%t IDR=%t PTS=%d",
				diagnostic.SliceType,
				diagnostic.FrameNum,
				diagnostic.PicOrderCntLSB,
				diagnostic.PreviousPOC,
				diagnostic.POCBackward,
				diagnostic.IDR,
				pts,
			)
		}

		return
	}
}

func codecSignatureFromDiagnostics(
	d H264Diagnostics,
) CodecSignature {
	return CodecSignature{
		ProfileIDC: d.ProfileIDC,
		LevelIDC:   d.LevelIDC,
		Width:      d.DisplayWidth,
		Height:     d.DisplayHeight,

		ChromaFormatIDC: d.ChromaFormatIDC,

		BitDepthLuma: d.BitDepthLuma,

		BitDepthChroma: d.BitDepthChroma,

		SPSHex: d.SPSHex,
		PPSHex: d.PPSHex,
	}
}

func sameCodecSignature(
	a CodecSignature,
	b CodecSignature,
) bool {
	return a.ProfileIDC == b.ProfileIDC &&
		a.LevelIDC == b.LevelIDC &&
		a.Width == b.Width &&
		a.Height == b.Height &&
		a.ChromaFormatIDC == b.ChromaFormatIDC &&
		a.BitDepthLuma == b.BitDepthLuma &&
		a.BitDepthChroma == b.BitDepthChroma &&
		a.SPSHex == b.SPSHex &&
		a.PPSHex == b.PPSHex
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
func readBitsValue(b *bitReader, count int) (uint64, error) {
	var value uint64
	for i := 0; i < count; i++ {
		bit, err := b.readBit()
		if err != nil {
			return 0, err
		}
		value = (value << 1) | bit
	}
	return value, nil
}

func skipH264HRD(b *bitReader) error {
	cpbCntMinus1, err := b.readUE()
	if err != nil {
		return err
	}
	if _, err = readBitsValue(b, 4); err != nil {
		return err
	}
	if _, err = readBitsValue(b, 4); err != nil {
		return err
	}
	for i := uint64(0); i <= cpbCntMinus1; i++ {
		if _, err = b.readUE(); err != nil {
			return err
		}
		if _, err = b.readUE(); err != nil {
			return err
		}
		if _, err = b.readBit(); err != nil {
			return err
		}
	}
	for i := 0; i < 4; i++ {
		if _, err = readBitsValue(b, 5); err != nil {
			return err
		}
	}
	return nil
}

func parseH264VUI(b *bitReader, d *H264Diagnostics) error {
	flag, err := b.readBit()
	if err != nil {
		return err
	}
	d.AspectRatioInfoPresent = flag != 0
	if flag != 0 {
		idc, err := readBitsValue(b, 8)
		if err != nil {
			return err
		}
		d.AspectRatioIDC = idc
		if idc == 255 {
			w, err := readBitsValue(b, 16)
			if err != nil {
				return err
			}
			h, err := readBitsValue(b, 16)
			if err != nil {
				return err
			}
			d.SARWidth, d.SARHeight = w, h
		}
	}
	flag, err = b.readBit()
	if err != nil {
		return err
	}
	if flag != 0 {
		if _, err = b.readBit(); err != nil {
			return err
		}
	}
	flag, err = b.readBit()
	if err != nil {
		return err
	}
	d.VideoSignalTypePresent = flag != 0
	if flag != 0 {
		v, err := readBitsValue(b, 3)
		if err != nil {
			return err
		}
		d.VideoFormat = v
		full, err := b.readBit()
		if err != nil {
			return err
		}
		d.VideoFullRange = full != 0
		colour, err := b.readBit()
		if err != nil {
			return err
		}
		d.ColourDescriptionPresent = colour != 0
		if colour != 0 {
			d.ColourPrimaries, err = readBitsValue(b, 8)
			if err != nil {
				return err
			}
			d.TransferCharacteristics, err = readBitsValue(b, 8)
			if err != nil {
				return err
			}
			d.MatrixCoefficients, err = readBitsValue(b, 8)
			if err != nil {
				return err
			}
		}
	}
	flag, err = b.readBit()
	if err != nil {
		return err
	}
	if flag != 0 {
		if _, err = b.readUE(); err != nil {
			return err
		}
		if _, err = b.readUE(); err != nil {
			return err
		}
	}
	flag, err = b.readBit()
	if err != nil {
		return err
	}
	d.TimingInfoPresent = flag != 0
	if flag != 0 {
		d.NumUnitsInTick, err = readBitsValue(b, 32)
		if err != nil {
			return err
		}
		d.TimeScale, err = readBitsValue(b, 32)
		if err != nil {
			return err
		}
		fixed, err := b.readBit()
		if err != nil {
			return err
		}
		d.FixedFrameRate = fixed != 0
		if d.NumUnitsInTick != 0 {
			d.NominalFrameRate = float64(d.TimeScale) / (2.0 * float64(d.NumUnitsInTick))
		}
	}
	nalHRD, err := b.readBit()
	if err != nil {
		return err
	}
	if nalHRD != 0 {
		if err = skipH264HRD(b); err != nil {
			return err
		}
	}
	vclHRD, err := b.readBit()
	if err != nil {
		return err
	}
	if vclHRD != 0 {
		if err = skipH264HRD(b); err != nil {
			return err
		}
	}
	if nalHRD != 0 || vclHRD != 0 {
		if _, err = b.readBit(); err != nil {
			return err
		}
	}
	if _, err = b.readBit(); err != nil {
		return err
	}
	flag, err = b.readBit()
	if err != nil {
		return err
	}
	d.BitstreamRestriction = flag != 0
	if flag != 0 {
		if _, err = b.readBit(); err != nil {
			return err
		}
		for i := 0; i < 4; i++ {
			if _, err = b.readUE(); err != nil {
				return err
			}
		}
		d.MaxNumReorderFrames, err = b.readUE()
		if err != nil {
			return err
		}
		d.MaxDecFrameBuffering, err = b.readUE()
		if err != nil {
			return err
		}
	}
	return nil
}

func parseH264SPS(
	spsAnnexB []byte,
	ppsAnnexB []byte,
) H264Diagnostics {
	d := H264Diagnostics{
		SPSBytes: len(spsAnnexB),
		PPSBytes: len(ppsAnnexB),
	}

	raw := stripAnnexBStartCode(spsAnnexB)

	if len(raw) < 4 {
		d.Error = "SPS too short"
		return d
	}

	if raw[0]&0x1F != 7 {
		d.Error = fmt.Sprintf(
			"expected SPS NAL type 7, got %d",
			raw[0]&0x1F,
		)

		return d
	}

	d.SPSHex = hex.EncodeToString(raw)

	ppsRaw := stripAnnexBStartCode(ppsAnnexB)

	if len(ppsRaw) > 0 {
		d.PPSHex = hex.EncodeToString(ppsRaw)
	}

	rbsp := removeEmulationPrevention(raw[1:])

	if len(rbsp) < 3 {
		d.Error = "SPS RBSP too short"
		return d
	}

	d.ProfileIDC = int(rbsp[0])
	d.Profile = profileName(d.ProfileIDC)
	d.ConstraintFlags = int(rbsp[1])
	d.LevelIDC = int(rbsp[2])
	d.Level = levelName(d.LevelIDC)

	b := &bitReader{
		data: rbsp[3:],
	}

	spsID, err := b.readUE()

	if err != nil {
		d.Error = "SPS id: " + err.Error()
		return d
	}

	d.SPSID = spsID

	chromaFormatIDC := uint64(1)
	bitDepthLumaMinus8 := uint64(0)
	bitDepthChromaMinus8 := uint64(0)

	switch d.ProfileIDC {
	case 100, 110, 122, 244,
		44, 83, 86, 118, 128,
		138, 139, 134, 135:

		chromaFormatIDC, err = b.readUE()

		if err != nil {
			d.Error = "chroma_format_idc: " + err.Error()
			return d
		}

		if chromaFormatIDC == 3 {
			flag, readErr := b.readBit()

			if readErr != nil {
				d.Error = "separate_colour_plane_flag: " + readErr.Error()
				return d
			}

			d.SeparateColourPlaneFlag = flag != 0
		}

		bitDepthLumaMinus8, err = b.readUE()

		if err != nil {
			d.Error = "bit_depth_luma_minus8: " + err.Error()
			return d
		}

		bitDepthChromaMinus8, err = b.readUE()

		if err != nil {
			d.Error = "bit_depth_chroma_minus8: " + err.Error()
			return d
		}

		_, err = b.readBit()

		if err != nil {
			d.Error = "qpprime_y_zero_transform_bypass_flag: " + err.Error()
			return d
		}

		seqScalingMatrixPresent, readErr := b.readBit()

		if readErr != nil {
			d.Error = "seq_scaling_matrix_present_flag: " + readErr.Error()
			return d
		}

		if seqScalingMatrixPresent != 0 {
			scalingCount := 8

			if chromaFormatIDC == 3 {
				scalingCount = 12
			}

			for i := 0; i < scalingCount; i++ {
				present, readErr := b.readBit()

				if readErr != nil {
					d.Error = "seq_scaling_list_present_flag: " + readErr.Error()
					return d
				}

				if present != 0 {
					size := 16

					if i >= 6 {
						size = 64
					}

					if err := skipScalingList(b, size); err != nil {
						d.Error = "scaling list: " + err.Error()
						return d
					}
				}
			}
		}
	}

	d.ChromaFormatIDC = chromaFormatIDC
	d.BitDepthLuma = int(bitDepthLumaMinus8 + 8)
	d.BitDepthChroma = int(bitDepthChromaMinus8 + 8)

	log2MaxFrameNumMinus4, err := b.readUE()

	if err != nil {
		d.Error = "log2_max_frame_num_minus4: " + err.Error()
		return d
	}

	d.Log2MaxFrameNum = int(log2MaxFrameNumMinus4 + 4)

	picOrderCntType, err := b.readUE()

	if err != nil {
		d.Error = "pic_order_cnt_type: " + err.Error()
		return d
	}

	d.PicOrderCntType = picOrderCntType

	if picOrderCntType == 0 {
		log2MaxPicOrderCntLSBMinus4, readErr := b.readUE()

		if readErr != nil {
			d.Error = "log2_max_pic_order_cnt_lsb_minus4: " + readErr.Error()
			return d
		}

		d.Log2MaxPicOrderCntLSB = int(log2MaxPicOrderCntLSBMinus4 + 4)
	}

	if picOrderCntType == 1 {
		_, err = b.readBit()

		if err != nil {
			d.Error = "delta_pic_order_always_zero_flag: " + err.Error()
			return d
		}

		_, err = b.readSE()

		if err != nil {
			d.Error = "offset_for_non_ref_pic: " + err.Error()
			return d
		}

		_, err = b.readSE()

		if err != nil {
			d.Error = "offset_for_top_to_bottom_field: " + err.Error()
			return d
		}

		numRefFramesInPicOrderCntCycle, readErr := b.readUE()

		if readErr != nil {
			d.Error = "num_ref_frames_in_pic_order_cnt_cycle: " + readErr.Error()
			return d
		}

		for i := uint64(0); i < numRefFramesInPicOrderCntCycle; i++ {
			_, readErr = b.readSE()

			if readErr != nil {
				d.Error = "offset_for_ref_frame: " + readErr.Error()
				return d
			}
		}
	}

	maxNumRefFrames, err := b.readUE()

	if err != nil {
		d.Error = "max_num_ref_frames: " + err.Error()
		return d
	}

	d.MaxNumRefFrames = maxNumRefFrames

	_, err = b.readBit()

	if err != nil {
		d.Error = "gaps_in_frame_num_value_allowed_flag: " + err.Error()
		return d
	}

	picWidthInMbsMinus1, err := b.readUE()

	if err != nil {
		d.Error = "pic_width_in_mbs_minus1: " + err.Error()
		return d
	}

	picHeightInMapUnitsMinus1, err := b.readUE()

	if err != nil {
		d.Error = "pic_height_in_map_units_minus1: " + err.Error()
		return d
	}

	d.PicWidthInMbs = int(picWidthInMbsMinus1 + 1)
	d.PicHeightInMapUnits = int(picHeightInMapUnitsMinus1 + 1)

	frameMbsOnlyFlag, err := b.readBit()

	if err != nil {
		d.Error = "frame_mbs_only_flag: " + err.Error()
		return d
	}

	d.FrameMbsOnly = frameMbsOnlyFlag != 0

	if frameMbsOnlyFlag == 0 {
		_, err = b.readBit()

		if err != nil {
			d.Error = "mb_adaptive_frame_field_flag: " + err.Error()
			return d
		}
	}

	_, err = b.readBit()

	if err != nil {
		d.Error = "direct_8x8_inference_flag: " + err.Error()
		return d
	}

	frameCroppingFlag, err := b.readBit()

	if err != nil {
		d.Error = "frame_cropping_flag: " + err.Error()
		return d
	}

	if frameCroppingFlag != 0 {
		d.FrameCropLeft, err = b.readUE()

		if err != nil {
			d.Error = "frame_crop_left_offset: " + err.Error()
			return d
		}

		d.FrameCropRight, err = b.readUE()

		if err != nil {
			d.Error = "frame_crop_right_offset: " + err.Error()
			return d
		}

		d.FrameCropTop, err = b.readUE()

		if err != nil {
			d.Error = "frame_crop_top_offset: " + err.Error()
			return d
		}

		d.FrameCropBottom, err = b.readUE()

		if err != nil {
			d.Error = "frame_crop_bottom_offset: " + err.Error()
			return d
		}
	}

	vuiPresent, err := b.readBit()

	if err == nil {
		d.VUIParametersPresent = vuiPresent != 0
		if d.VUIParametersPresent {
			if vuiErr := parseH264VUI(b, &d); vuiErr != nil {
				d.VUIParseError = vuiErr.Error()
			}
		}
	}

	frameHeightMultiplier := 2

	if d.FrameMbsOnly {
		frameHeightMultiplier = 1
	}

	d.CodedWidth = d.PicWidthInMbs * 16
	d.CodedHeight = d.PicHeightInMapUnits * 16 * frameHeightMultiplier

	cropUnitX := 1
	cropUnitY := 2 - int(frameMbsOnlyFlag)

	if !d.SeparateColourPlaneFlag {
		switch d.ChromaFormatIDC {
		case 0:
			cropUnitX = 1
			cropUnitY = 2 - int(frameMbsOnlyFlag)

		case 1:
			cropUnitX = 2
			cropUnitY = 2 * (2 - int(frameMbsOnlyFlag))

		case 2:
			cropUnitX = 2
			cropUnitY = 2 - int(frameMbsOnlyFlag)

		case 3:
			cropUnitX = 1
			cropUnitY = 2 - int(frameMbsOnlyFlag)
		}
	}

	d.DisplayWidth = d.CodedWidth - int(d.FrameCropLeft+d.FrameCropRight)*cropUnitX
	d.DisplayHeight = d.CodedHeight - int(d.FrameCropTop+d.FrameCropBottom)*cropUnitY

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

	if !d.Valid && d.Error == "" {
		d.Error = "parsed SPS produced invalid dimensions"
	}

	return d
}

func (s *StreamSession) updateH264DiagnosticsLocked(
	accessUnit []byte,
	pts int64,
) {
	hasIDR, hasSPS, hasPPS, newSPS, newPPS := inspectAccessUnit(accessUnit)

	if hasSPS && len(newSPS) > 0 {
		s.sps = append([]byte(nil), newSPS...)
	}

	if hasPPS && len(newPPS) > 0 {
		s.pps = append([]byte(nil), newPPS...)
	}

	if len(s.sps) == 0 {
		return
	}

	diagnostics := parseH264SPS(
		s.sps,
		s.pps,
	)

	s.h264Diagnostics = diagnostics

	if !diagnostics.Valid {
		if hasSPS {
			log.Printf(
				"VERSION 58 H264 SPS PARSE ERROR: %s",
				diagnostics.Error,
			)
		}

		return
	}

	signature := codecSignatureFromDiagnostics(diagnostics)

	if !s.haveActiveCodec {
		s.activeCodec = signature
		s.haveActiveCodec = true
		s.currentGeneration = 0

		log.Printf(
			"VERSION 58 H264 INITIAL CODEC: %s profileName=%s levelName=%s SPS=%d PPS=%d IDR=%t PTS=%d",
			codecSignatureString(signature),
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

	if sameCodecSignature(s.activeCodec, signature) {
		if !s.h264Logged {
			log.Printf(
				"VERSION 58 H264 CODEC: %s",
				codecSignatureString(signature),
			)

			s.h264Logged = true
		}

		return
	}

	oldCodec := s.activeCodec

	changeNumber := atomic.AddUint64(
		&s.Stats.CodecChanges,
		1,
	)

	log.Printf(
		"VERSION 58 CODEC CHANGE DETECTED #%d: FROM [%s] TO [%s] IDR=%t PTS=%d",
		changeNumber,
		codecSignatureString(oldCodec),
		codecSignatureString(signature),
		hasIDR,
		pts,
	)

	if s.segmentActive &&
		s.currentBuffer != nil &&
		s.currentBuffer.Len() > 0 {

		log.Printf(
			"VERSION 58 closing old codec segment before SPS transition: sequence=%d generation=%d",
			s.NextSequence,
			s.currentGeneration,
		)

		s.finishSegmentLocked()
	}

	s.currentGeneration++
	s.pendingDiscontinuity = true
	s.activeCodec = signature
	s.haveLastPOC = false

	change := CodecChange{
		Number: changeNumber,

		Time: time.Now().UTC().Format(time.RFC3339Nano),

		PTS: pts,

		From: oldCodec,
		To:   signature,

		SegmentSequence: s.NextSequence,

		Reason: "SPS/PPS codec signature changed",
	}

	s.codecChanges = append(
		s.codecChanges,
		change,
	)

	if len(s.codecChanges) > 16 {
		s.codecChanges = append([]CodecChange(nil), s.codecChanges[len(s.codecChanges)-16:]...)
	}

	log.Printf(
		"VERSION 58 HLS DISCONTINUITY ARMED: generation=%d nextSequence=%d",
		s.currentGeneration,
		s.NextSequence,
	)
}

func (s *StreamSession) cacheParametersLocked(
	accessUnit []byte,
) {
	_, hasSPS, hasPPS, newSPS, newPPS := inspectAccessUnit(accessUnit)

	if hasSPS &&
		len(newSPS) > 0 &&
		!bytes.Equal(s.sps, newSPS) {

		s.sps = append([]byte(nil), newSPS...)

		log.Printf(
			"VERSION 58 cached SPS: %d bytes",
			len(s.sps),
		)
	}

	if hasPPS &&
		len(newPPS) > 0 &&
		!bytes.Equal(s.pps, newPPS) {

		s.pps = append([]byte(nil), newPPS...)

		log.Printf(
			"VERSION 58 cached PPS: %d bytes",
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
		s.lastRTPTimestamp = rtpTimestamp
		s.normalizedPTS = ptsOffset

		log.Printf(
			"VERSION 58 timestamp clock started: RTP=%d PTS=%d",
			rtpTimestamp,
			s.normalizedPTS,
		)

		return s.normalizedPTS
	}

	delta := uint32(rtpTimestamp - s.lastRTPTimestamp)

	if delta > 90000*10 {
		atomic.AddUint64(
			&s.Stats.TimestampDiscontinuities,
			1,
		)

		log.Printf(
			"VERSION 58 timestamp discontinuity: previous=%d current=%d rawDelta=%d",
			s.lastRTPTimestamp,
			rtpTimestamp,
			delta,
		)

		s.lastRTPTimestamp = rtpTimestamp

		return s.normalizedPTS
	}

	s.normalizedPTS += int64(delta)
	s.lastRTPTimestamp = rtpTimestamp

	return s.normalizedPTS
}

func parsePTS(data []byte) int64 {
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

func parsePCR(packet []byte) int64 {
	if len(packet) < 12 {
		return -1
	}

	adaptationLength := int(packet[4])

	if adaptationLength < 7 ||
		5+adaptationLength > len(packet) {

		return -1
	}

	flags := packet[5]

	if flags&0x10 == 0 {
		return -1
	}

	p := packet[6:12]

	base :=
		(int64(p[0]) << 25) |
			(int64(p[1]) << 17) |
			(int64(p[2]) << 9) |
			(int64(p[3]) << 1) |
			(int64(p[4]) >> 7)

	return base
}
func diagnoseTS(data []byte) TSDiagnostics {
	d := TSDiagnostics{
		Bytes: len(data),

		FirstPTS: -1,
		LastPTS:  -1,
		MinPTS:   -1,
		MaxPTS:   -1,

		FirstPCR: -1,
		LastPCR:  -1,

		PMTPID:   -1,
		VideoPID: -1,
	}

	if len(data) == 0 {
		d.Error = "segment is empty"
		return d
	}

	if len(data) >= 32 {
		d.FirstPacketHex = hex.EncodeToString(data[:32])
	} else {
		d.FirstPacketHex = hex.EncodeToString(data)
	}

	if len(data)%188 != 0 {
		d.Error = fmt.Sprintf(
			"TS size %d is not divisible by 188",
			len(data),
		)

		return d
	}

	d.Packets = len(data) / 188

	continuity := make(map[uint16]uint8)
	continuitySeen := make(map[uint16]bool)

	var previousPTS int64 = -1

	for offset := 0; offset+188 <= len(data); offset += 188 {
		packet := data[offset : offset+188]

		if packet[0] != 0x47 {
			d.SyncErrors++
			continue
		}

		transportError := packet[1]&0x80 != 0

		if transportError {
			d.TransportErrors++
		}

		pusi := packet[1]&0x40 != 0

		if pusi {
			d.PUSIPackets++
		}

		pid := uint16(packet[1]&0x1F)<<8 | uint16(packet[2])

		adaptationControl := (packet[3] >> 4) & 0x03
		counter := packet[3] & 0x0F

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
				adaptationLength := int(packet[4])

				if adaptationLength > 0 &&
					5+adaptationLength <= 188 {

					flags := packet[5]

					if flags&0x80 != 0 {
						d.DiscontinuityFlags++
					}

					if flags&0x10 != 0 {
						pcr := parsePCR(packet)

						if pcr >= 0 {
							d.PCRPackets++

							if d.FirstPCR < 0 {
								d.FirstPCR = pcr
							}

							d.LastPCR = pcr
						}
					}
				}
			}
		}

		if hasPayload {
			d.PayloadPackets++

			if continuitySeen[pid] {
				expected := (continuity[pid] + 1) & 0x0F

				if counter != expected {
					d.ContinuityErrors++
				}
			}

			continuity[pid] = counter
			continuitySeen[pid] = true
		}

		payloadStart := 4

		if hasAdaptation {
			if payloadStart >= 188 {
				continue
			}

			adaptationLength := int(packet[4])

			payloadStart += 1 + adaptationLength
		}

		if !hasPayload || payloadStart >= 188 {
			continue
		}

		payload := packet[payloadStart:]

		if pid == 0 {
			d.PATPackets++

			if pusi && len(payload) > 0 {
				pointer := int(payload[0])
				pos := 1 + pointer

				if pos+8 <= len(payload) &&
					payload[pos] == 0x00 {

					sectionLength := int(
						binary.BigEndian.Uint16(payload[pos+1:pos+3]) & 0x0FFF,
					)

					end := pos + 3 + sectionLength - 4
					programPos := pos + 8

					for programPos+4 <= end &&
						programPos+4 <= len(payload) {

						programNumber := binary.BigEndian.Uint16(
							payload[programPos : programPos+2],
						)

						programPID := int(
							binary.BigEndian.Uint16(payload[programPos+2:programPos+4]) & 0x1FFF,
						)

						if programNumber != 0 {
							d.PMTPID = programPID
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

			if pusi && len(payload) > 0 {
				pointer := int(payload[0])
				pos := 1 + pointer

				if pos+12 <= len(payload) &&
					payload[pos] == 0x02 {

					sectionLength := int(
						binary.BigEndian.Uint16(payload[pos+1:pos+3]) & 0x0FFF,
					)

					programInfoLength := int(
						binary.BigEndian.Uint16(payload[pos+10:pos+12]) & 0x0FFF,
					)

					esPos := pos + 12 + programInfoLength
					end := pos + 3 + sectionLength - 4

					for esPos+5 <= end &&
						esPos+5 <= len(payload) {

						streamType := int(payload[esPos])

						elementaryPID := int(
							binary.BigEndian.Uint16(payload[esPos+1:esPos+3]) & 0x1FFF,
						)

						esInfoLength := int(
							binary.BigEndian.Uint16(payload[esPos+3:esPos+5]) & 0x0FFF,
						)

						if streamType == 0x1B {
							d.StreamType = streamType
							d.VideoPID = elementaryPID
						}

						esPos += 5 + esInfoLength
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

				flags := payload[7]
				headerLength := int(payload[8])

				if flags&0x80 != 0 &&
					headerLength >= 5 &&
					len(payload) >= 14 {

					pts := parsePTS(payload[9:14])

					if pts >= 0 {
						d.PTSCount++

						if d.FirstPTS < 0 {
							d.FirstPTS = pts
						}

						d.LastPTS = pts

						if d.MinPTS < 0 || pts < d.MinPTS {
							d.MinPTS = pts
						}

						if d.MaxPTS < 0 || pts > d.MaxPTS {
							d.MaxPTS = pts
						}

						if previousPTS >= 0 &&
							pts < previousPTS {

							d.BackwardPTS++
						}

						previousPTS = pts
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

	if !d.Valid && d.Error == "" {
		d.Error = "media-level TS diagnostics failed"
	}

	return d
}

type TSReadbackDiagnostics struct {
	Success           bool   `json:"success"`
	AccessUnits       int    `json:"accessUnits"`
	RandomAccessUnits int    `json:"randomAccessUnits"`
	FirstPTS          int64  `json:"firstPTS"`
	LastPTS           int64  `json:"lastPTS"`
	FirstDTS          int64  `json:"firstDTS"`
	LastDTS           int64  `json:"lastDTS"`
	FirstNAL          string `json:"firstNAL"`
	LastNAL           string `json:"lastNAL"`
	Error             string `json:"error,omitempty"`
}

func readbackTS(data []byte) TSReadbackDiagnostics {
	d := TSReadbackDiagnostics{
		FirstPTS: -1,
		LastPTS:  -1,
		FirstDTS: -1,
		LastDTS:  -1,
	}

	reader, err := mpegts.NewReader(bytes.NewReader(data))
	if err != nil {
		d.Error = err.Error()
		return d
	}

	for {
		au, err := reader.NextAccessUnit()
		if err == io.EOF {
			break
		}

		if err != nil {
			d.Error = err.Error()
			return d
		}

		d.AccessUnits++

		if d.FirstPTS < 0 {
			d.FirstPTS = au.PTS
			d.FirstDTS = au.DTS
			d.FirstNAL = naluSummary(au.Data)
		}

		d.LastPTS = au.PTS
		d.LastDTS = au.DTS
		d.LastNAL = naluSummary(au.Data)
	}

	d.Success = d.AccessUnits > 0
	return d
}

type SPSOccurrence struct {
	AUNumber    int             `json:"auNumber"`
	PTS         int64           `json:"pts"`
	DTS         int64           `json:"dts"`
	Hex         string          `json:"hex"`
	Diagnostics H264Diagnostics `json:"diagnostics"`
}

type PPSDiagnostics struct {
	Valid                             bool   `json:"valid"`
	PicParameterSetID                 uint64 `json:"picParameterSetID"`
	SeqParameterSetID                 uint64 `json:"seqParameterSetID"`
	EntropyCodingModeCABAC            bool   `json:"entropyCodingModeCABAC"`
	BottomFieldPicOrderInFramePresent bool   `json:"bottomFieldPicOrderInFramePresent"`
	NumSliceGroupsMinus1              uint64 `json:"numSliceGroupsMinus1"`
	NumRefIdxL0DefaultActiveMinus1    uint64 `json:"numRefIdxL0DefaultActiveMinus1"`
	NumRefIdxL1DefaultActiveMinus1    uint64 `json:"numRefIdxL1DefaultActiveMinus1"`
	WeightedPred                      bool   `json:"weightedPred"`
	WeightedBipredIDC                 uint64 `json:"weightedBipredIDC"`
	PicInitQPMinus26                  int64  `json:"picInitQPMinus26"`
	PicInitQSMinus26                  int64  `json:"picInitQSMinus26"`
	ChromaQPIndexOffset               int64  `json:"chromaQPIndexOffset"`
	DeblockingFilterControlPresent    bool   `json:"deblockingFilterControlPresent"`
	ConstrainedIntraPred              bool   `json:"constrainedIntraPred"`
	RedundantPicCntPresent            bool   `json:"redundantPicCntPresent"`
	Transform8x8Mode                  bool   `json:"transform8x8Mode"`
	PicScalingMatrixPresent           bool   `json:"picScalingMatrixPresent"`
	SecondChromaQPIndexOffset         int64  `json:"secondChromaQPIndexOffset"`
	Error                             string `json:"error,omitempty"`
}

type PPSOccurrence struct {
	AUNumber    int            `json:"auNumber"`
	PTS         int64          `json:"pts"`
	DTS         int64          `json:"dts"`
	Hex         string         `json:"hex"`
	Diagnostics PPSDiagnostics `json:"diagnostics"`
}

type SliceFrameOccurrence struct {
	AUNumber              int    `json:"auNumber"`
	PTS                   int64  `json:"pts"`
	DTS                   int64  `json:"dts"`
	NALType               int    `json:"nalType"`
	IDR                   bool   `json:"idr"`
	NALRefIDC             int    `json:"nalRefIDC"`
	SliceType             string `json:"sliceType"`
	PicParameterSetID     uint64 `json:"picParameterSetID"`
	FrameNum              uint64 `json:"frameNum"`
	PicOrderCntLSB        uint64 `json:"picOrderCntLSB"`
	HasPicOrderCntLSB     bool   `json:"hasPicOrderCntLSB"`
	FrameNumDiscontinuity bool   `json:"frameNumDiscontinuity"`
	POCDiscontinuity      bool   `json:"pocDiscontinuity"`
	Error                 string `json:"error,omitempty"`
}

type H264ReadbackCompatibility struct {
	Success                     bool                   `json:"success"`
	AccessUnits                 int                    `json:"accessUnits"`
	IDRAccessUnits              int                    `json:"idrAccessUnits"`
	NonIDRAccessUnits           int                    `json:"nonIDRAccessUnits"`
	AccessUnitsWithAUD          int                    `json:"accessUnitsWithAUD"`
	SPSNALUnits                 int                    `json:"spsNALUnits"`
	PPSNALUnits                 int                    `json:"ppsNALUnits"`
	IDRWithSPS                  int                    `json:"idrWithSPS"`
	IDRWithPPS                  int                    `json:"idrWithPPS"`
	IDRWithSPSAndPPS            int                    `json:"idrWithSPSAndPPS"`
	IDRMissingSPSOrPPS          int                    `json:"idrMissingSPSOrPPS"`
	AllIDRHaveSPSAndPPS         bool                   `json:"allIDRHaveSPSAndPPS"`
	UniqueSPS                   int                    `json:"uniqueSPS"`
	UniquePPS                   int                    `json:"uniquePPS"`
	SPSChanges                  int                    `json:"spsChanges"`
	PPSChanges                  int                    `json:"ppsChanges"`
	MultipleSPSInSegment        bool                   `json:"multipleSPSInSegment"`
	MultiplePPSInSegment        bool                   `json:"multiplePPSInSegment"`
	MultipleCodecConfigurations bool                   `json:"multipleCodecConfigurations"`
	SPSParseErrors              int                    `json:"spsParseErrors"`
	AllSPSParseValid            bool                   `json:"allSPSParseValid"`
	FirstNAL                    string                 `json:"firstNAL"`
	LastNAL                     string                 `json:"lastNAL"`
	SPS                         []SPSOccurrence        `json:"sps"`
	PPS                         []PPSOccurrence        `json:"pps"`
	NALUnits                    int                    `json:"nalUnits"`
	ForbiddenZeroBitViolations  int                    `json:"forbiddenZeroBitViolations"`
	ReferenceVCLNALUnits        int                    `json:"referenceVCLNALUnits"`
	NonReferenceVCLNALUnits     int                    `json:"nonReferenceVCLNALUnits"`
	IDRWithZeroNALRefIDC        int                    `json:"idrWithZeroNALRefIDC"`
	NALHeadersValid             bool                   `json:"nalHeadersValid"`
	SliceFrames                 []SliceFrameOccurrence `json:"sliceFrames"`
	SliceHeadersParsed          int                    `json:"sliceHeadersParsed"`
	SliceHeaderErrors           int                    `json:"sliceHeaderErrors"`
	FrameNumDiscontinuities     int                    `json:"frameNumDiscontinuities"`
	POCDiscontinuities          int                    `json:"pocDiscontinuities"`
	SliceSequenceValid          bool                   `json:"sliceSequenceValid"`
	Error                       string                 `json:"error,omitempty"`
}

func parseH264PPS(nalu []byte) PPSDiagnostics {
	d := PPSDiagnostics{}
	if len(nalu) < 2 || nalu[0]&0x1F != 8 {
		d.Error = "invalid PPS NAL"
		return d
	}
	b := &bitReader{data: removeEmulationPrevention(nalu[1:])}
	var err error
	if d.PicParameterSetID, err = b.readUE(); err != nil {
		d.Error = err.Error()
		return d
	}
	if d.SeqParameterSetID, err = b.readUE(); err != nil {
		d.Error = err.Error()
		return d
	}
	v, err := b.readBit()
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.EntropyCodingModeCABAC = v == 1
	v, err = b.readBit()
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.BottomFieldPicOrderInFramePresent = v == 1
	if d.NumSliceGroupsMinus1, err = b.readUE(); err != nil {
		d.Error = err.Error()
		return d
	}
	if d.NumSliceGroupsMinus1 != 0 {
		d.Error = "slice groups are present; Version 31 PPS parser intentionally stops before FMO syntax"
		return d
	}
	if d.NumRefIdxL0DefaultActiveMinus1, err = b.readUE(); err != nil {
		d.Error = err.Error()
		return d
	}
	if d.NumRefIdxL1DefaultActiveMinus1, err = b.readUE(); err != nil {
		d.Error = err.Error()
		return d
	}
	v, err = b.readBit()
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.WeightedPred = v == 1
	if d.WeightedBipredIDC, err = b.readBits(2); err != nil {
		d.Error = err.Error()
		return d
	}
	if d.PicInitQPMinus26, err = b.readSE(); err != nil {
		d.Error = err.Error()
		return d
	}
	if d.PicInitQSMinus26, err = b.readSE(); err != nil {
		d.Error = err.Error()
		return d
	}
	if d.ChromaQPIndexOffset, err = b.readSE(); err != nil {
		d.Error = err.Error()
		return d
	}
	v, err = b.readBit()
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.DeblockingFilterControlPresent = v == 1
	v, err = b.readBit()
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.ConstrainedIntraPred = v == 1
	v, err = b.readBit()
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.RedundantPicCntPresent = v == 1
	// If more RBSP payload exists before the stop bit, High-profile PPS extension follows.
	bitsLeft := len(b.data)*8 - b.pos
	if bitsLeft > 1 {
		v, err = b.readBit()
		if err != nil {
			d.Error = err.Error()
			return d
		}
		d.Transform8x8Mode = v == 1
		v, err = b.readBit()
		if err != nil {
			d.Error = err.Error()
			return d
		}
		d.PicScalingMatrixPresent = v == 1
		if d.PicScalingMatrixPresent {
			d.Error = "PPS scaling matrix present; Version 31 does not consume scaling-list payload"
			return d
		}
		if d.SecondChromaQPIndexOffset, err = b.readSE(); err != nil {
			d.Error = err.Error()
			return d
		}
	}
	d.Valid = true
	return d
}

func analyzeReadbackH264(data []byte) H264ReadbackCompatibility {
	d := H264ReadbackCompatibility{
		AllSPSParseValid:   true,
		NALHeadersValid:    true,
		SliceSequenceValid: true,
	}

	reader, err := mpegts.NewReader(bytes.NewReader(data))
	if err != nil {
		d.Error = err.Error()
		return d
	}

	seenSPS := make(map[string]bool)
	seenPPS := make(map[string]bool)
	lastSPSHex := ""
	lastPPSHex := ""
	var latestPPS []byte
	var latestSPSDiag H264Diagnostics
	var haveSPSDiag bool
	var previousFrameNum uint64
	var previousPOC uint64
	var havePreviousSlice bool

	for {
		au, err := reader.NextAccessUnit()
		if err == io.EOF {
			break
		}
		if err != nil {
			d.Error = err.Error()
			return d
		}

		d.AccessUnits++
		auNumber := d.AccessUnits
		summary := naluSummary(au.Data)
		if d.FirstNAL == "" {
			d.FirstNAL = summary
		}
		d.LastNAL = summary

		var auSPSNALs [][]byte
		var auPPSNALs [][]byte
		hasIDR := false
		hasNonIDR := false
		hasAUD := false

		for _, nalu := range splitAnnexB(au.Data) {
			if len(nalu) == 0 {
				continue
			}
			d.NALUnits++
			if nalu[0]&0x80 != 0 {
				d.ForbiddenZeroBitViolations++
				d.NALHeadersValid = false
			}
			nalType := nalu[0] & 0x1F
			nalRefIDC := (nalu[0] >> 5) & 0x03
			if nalType == 1 || nalType == 5 {
				if nalRefIDC == 0 {
					d.NonReferenceVCLNALUnits++
				} else {
					d.ReferenceVCLNALUnits++
				}
			}
			if nalType == 5 && nalRefIDC == 0 {
				d.IDRWithZeroNALRefIDC++
				d.NALHeadersValid = false
			}
			switch nalType {
			case 1:
				hasNonIDR = true
			case 5:
				hasIDR = true
			case 7:
				d.SPSNALUnits++
				auSPSNALs = append(auSPSNALs, append([]byte(nil), nalu...))
			case 8:
				d.PPSNALUnits++
				auPPSNALs = append(auPPSNALs, append([]byte(nil), nalu...))
			case 9:
				hasAUD = true
			}
		}

		if hasAUD {
			d.AccessUnitsWithAUD++
		}
		if hasIDR {
			d.IDRAccessUnits++
			if len(auSPSNALs) > 0 {
				d.IDRWithSPS++
			}
			if len(auPPSNALs) > 0 {
				d.IDRWithPPS++
			}
			if len(auSPSNALs) > 0 && len(auPPSNALs) > 0 {
				d.IDRWithSPSAndPPS++
			} else {
				d.IDRMissingSPSOrPPS++
			}
		} else if hasNonIDR {
			d.NonIDRAccessUnits++
		}

		for _, ppsNAL := range auPPSNALs {
			ppsHex := hex.EncodeToString(ppsNAL)
			if lastPPSHex != "" && ppsHex != lastPPSHex {
				d.PPSChanges++
			}
			lastPPSHex = ppsHex
			latestPPS = annexB(ppsNAL)
			if !seenPPS[ppsHex] {
				seenPPS[ppsHex] = true
				d.PPS = append(d.PPS, PPSOccurrence{AUNumber: auNumber, PTS: au.PTS, DTS: au.DTS, Hex: ppsHex, Diagnostics: parseH264PPS(ppsNAL)})
			}
		}

		for _, spsNAL := range auSPSNALs {
			spsHex := hex.EncodeToString(spsNAL)
			if lastSPSHex != "" && spsHex != lastSPSHex {
				d.SPSChanges++
			}
			lastSPSHex = spsHex

			ppsForParse := latestPPS
			if len(auPPSNALs) > 0 {
				ppsForParse = annexB(auPPSNALs[len(auPPSNALs)-1])
			}
			diagnostic := parseH264SPS(annexB(spsNAL), ppsForParse)
			if diagnostic.Valid {
				latestSPSDiag = diagnostic
				haveSPSDiag = true
			}
			if !diagnostic.Valid {
				d.SPSParseErrors++
				d.AllSPSParseValid = false
			}
			if !seenSPS[spsHex] {
				seenSPS[spsHex] = true
				d.SPS = append(d.SPS, SPSOccurrence{AUNumber: auNumber, PTS: au.PTS, DTS: au.DTS, Hex: spsHex, Diagnostics: diagnostic})
			}
		}

		if haveSPSDiag {
			for _, nalu := range splitAnnexB(au.Data) {
				if len(nalu) == 0 || (nalu[0]&0x1F != 1 && nalu[0]&0x1F != 5) {
					continue
				}
				slice := parseH264SliceHeader(nalu, latestSPSDiag, au.PTS)
				frame := SliceFrameOccurrence{AUNumber: auNumber, PTS: au.PTS, DTS: au.DTS, NALType: slice.NALType, IDR: slice.IDR, NALRefIDC: slice.NALRefIDC, SliceType: slice.SliceType, PicParameterSetID: slice.PicParameterSetID, FrameNum: slice.FrameNum, PicOrderCntLSB: slice.PicOrderCntLSB, HasPicOrderCntLSB: slice.HasPicOrderCntLSB, Error: slice.Error}
				if !slice.Valid {
					d.SliceHeaderErrors++
					d.SliceSequenceValid = false
				} else {
					d.SliceHeadersParsed++
					if havePreviousSlice && !slice.IDR {
						maxFrameNum := uint64(1) << uint(latestSPSDiag.Log2MaxFrameNum)
						expectedFrameNum := (previousFrameNum + 1) % maxFrameNum
						if slice.FrameNum != expectedFrameNum {
							frame.FrameNumDiscontinuity = true
							d.FrameNumDiscontinuities++
							d.SliceSequenceValid = false
						}
						if slice.HasPicOrderCntLSB && latestSPSDiag.Log2MaxPicOrderCntLSB > 0 {
							maxPOC := uint64(1) << uint(latestSPSDiag.Log2MaxPicOrderCntLSB)
							expectedPOC := (previousPOC + 1) % maxPOC
							if slice.PicOrderCntLSB != expectedPOC {
								frame.POCDiscontinuity = true
								d.POCDiscontinuities++
								d.SliceSequenceValid = false
							}
						}
					}
					previousFrameNum = slice.FrameNum
					if slice.HasPicOrderCntLSB {
						previousPOC = slice.PicOrderCntLSB
					}
					havePreviousSlice = true
				}
				d.SliceFrames = append(d.SliceFrames, frame)
				break
			}
		}
	}

	d.UniqueSPS = len(seenSPS)
	d.UniquePPS = len(seenPPS)
	d.MultipleSPSInSegment = d.UniqueSPS > 1
	d.MultiplePPSInSegment = d.UniquePPS > 1
	d.MultipleCodecConfigurations = d.MultipleSPSInSegment || d.MultiplePPSInSegment
	d.AllIDRHaveSPSAndPPS = d.IDRAccessUnits > 0 && d.IDRMissingSPSOrPPS == 0
	d.Success = d.AccessUnits > 0 && d.Error == ""
	return d
}

func (s *StreamSession) newSegmentLocked() error {
	buf := &bytes.Buffer{}

	writer, err := mpegts.NewWriter(
		buf,
		mpegts.WithH264Track(videoPID),
	)

	if err != nil {
		return err
	}

	s.currentBuffer = buf
	s.currentWriter = writer
	s.segmentStart = time.Now()
	s.segmentActive = true

	if s.haveActiveCodec {
		s.currentSegmentCodec = s.activeCodec
	}

	log.Printf(
		"VERSION 58 SEGMENT OPEN: sequence=%d generation=%d codec=[%s] discontinuity=%t",
		s.NextSequence,
		s.currentGeneration,
		codecSignatureString(s.currentSegmentCodec),
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
	diagnostics := diagnoseTS(data)

	if diagnostics.Valid {
		return true, "", diagnostics
	}

	return false, diagnostics.Error, diagnostics
}

func (s *StreamSession) finishSegmentLocked() {
	if !s.segmentActive ||
		s.currentWriter == nil ||
		s.currentBuffer == nil ||
		s.currentBuffer.Len() == 0 {

		return
	}

	duration := time.Since(s.segmentStart).Seconds()

	if duration <= 0 {
		duration = segmentDuration.Seconds()
	}

	data := append(
		[]byte(nil),
		s.currentBuffer.Bytes()...,
	)

	valid, validateErr, diagnostics := validateSegment(data)

	if valid {
		atomic.AddUint64(
			&s.Stats.ValidatedTS,
			1,
		)

		log.Printf(
			"VERSION 58 TS MEDIA VALID: packets=%d bytes=%d PAT=%d PMT=%d videoPID=%d streamType=0x%02x PES=%d PTS=%d DTS=%d PCR=%d continuityErrors=%d firstPTS=%d lastPTS=%d",
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
			diagnostics.FirstPTS,
			diagnostics.LastPTS,
		)
	} else {
		atomic.AddUint64(
			&s.Stats.ValidationError,
			1,
		)

		log.Printf(
			"VERSION 58 TS MEDIA INVALID: error=%s packets=%d PAT=%d PMT=%d videoPID=%d streamType=0x%02x PES=%d PTS=%d backwardPTS=%d PCR=%d continuityErrors=%d transportErrors=%d",
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

	discontinuity := s.pendingDiscontinuity

	segment := HLSSegment{
		Sequence: s.NextSequence,

		Duration: duration,

		Data: data,

		Validated: valid,

		ValidateErr: validateErr,

		Diagnostics: diagnostics,

		Codec: s.currentSegmentCodec,

		Generation: s.currentGeneration,

		DiscontinuityBefore: discontinuity,
	}

	s.pendingDiscontinuity = false
	s.NextSequence++

	s.Segments = append(
		s.Segments,
		segment,
	)

	if len(s.Segments) > maxSegments {
		s.Segments = append([]HLSSegment(nil), s.Segments[len(s.Segments)-maxSegments:]...)
	}

	atomic.AddUint64(
		&s.Stats.HLSSegments,
		1,
	)

	log.Printf(
		"VERSION 58 HLS SEGMENT READY: sequence=%d generation=%d duration=%.3f size=%d mediaValid=%t discontinuityBefore=%t codec=[%s]",
		segment.Sequence,
		segment.Generation,
		segment.Duration,
		len(segment.Data),
		segment.Validated,
		segment.DiscontinuityBefore,
		codecSignatureString(segment.Codec),
	)

	s.readyOnce.Do(
		func() {
			close(s.ready)
		},
	)

	s.currentBuffer = nil
	s.currentWriter = nil
	s.segmentActive = false
}

func (s *StreamSession) keyframeAccessUnitLocked(
	accessUnit []byte,
) []byte {
	hasIDR, hasSPS, hasPPS, _, _ := inspectAccessUnit(accessUnit)

	if !hasIDR {
		return accessUnit
	}

	before := naluSummary(accessUnit)

	cleaned,
		duplicateSPS,
		duplicatePPS,
		changed :=
		canonicalizeH264AccessUnit(accessUnit)

	if changed {
		accessUnit = cleaned

		atomic.AddUint64(
			&s.Stats.CleanedAccessUnits,
			1,
		)

		if duplicateSPS > 0 {
			atomic.AddUint64(
				&s.Stats.DuplicateSPSRemoved,
				uint64(duplicateSPS),
			)
		}

		if duplicatePPS > 0 {
			atomic.AddUint64(
				&s.Stats.DuplicatePPSRemoved,
				uint64(duplicatePPS),
			)
		}

		_, hasSPS, hasPPS, _, _ = inspectAccessUnit(accessUnit)
	}

	var out []byte

	if !hasSPS && len(s.sps) > 0 {
		out = append(out, s.sps...)
	}

	if !hasPPS && len(s.pps) > 0 {
		out = append(out, s.pps...)
	}

	out = append(out, accessUnit...)

	/*
		V19 canonicalizes the final keyframe AU after
		SPS/PPS insertion. Every NAL reaching the muxer
		therefore uses a four-byte Annex-B start code.
	*/
	normalized, repaired := normalizeAnnexBAccessUnit(out)

	if repaired {
		out = normalized

		atomic.AddUint64(
			&s.Stats.AnnexBRepairs,
			1,
		)
	}

	after := naluSummary(out)

	s.lastCleanedNALSummary = after

	if changed {
		log.Printf(
			"VERSION 58 H264 NORMALIZE: before=%s after=%s removedSPS=%d removedPPS=%d",
			before,
			after,
			duplicateSPS,
			duplicatePPS,
		)
	}

	return out
}

/*
V19 prepares every AU immediately before WriteH264.

This gives the MPEG-TS writer one consistent Annex-B
representation regardless of how the RTP depacketizer
represented individual NAL units.
*/
func (s *StreamSession) prepareMuxAccessUnitLocked(
	accessUnit []byte,
) []byte {
	normalized, repaired := normalizeAnnexBAccessUnit(accessUnit)

	if repaired {
		accessUnit = normalized

		atomic.AddUint64(
			&s.Stats.AnnexBRepairs,
			1,
		)
	}

	nalus := splitAnnexB(accessUnit)

	atomic.AddUint64(
		&s.Stats.MuxAccessUnits,
		1,
	)

	atomic.AddUint64(
		&s.Stats.MuxNALUnits,
		uint64(len(nalus)),
	)

	hasIDR := false

	for _, nalu := range nalus {
		if len(nalu) > 0 &&
			nalu[0]&0x1F == 5 {

			hasIDR = true
			break
		}
	}

	if hasIDR {
		atomic.AddUint64(
			&s.Stats.MuxIDRUnits,
			1,
		)
	}

	s.lastMuxNALSummary = naluSummary(accessUnit)
	s.lastMuxBytes = len(accessUnit)
	prefixLength := len(accessUnit)

	if prefixLength > 32 {
		prefixLength = 32
	}

	s.lastMuxPrefixHex = hex.EncodeToString(
		accessUnit[:prefixLength],
	)
	return accessUnit
}

func (s *StreamSession) writeAccessUnit(
	accessUnit []byte,
	pts int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.done:
		return fmt.Errorf("stream session closed")

	default:
	}

	hasIDR, _, _, _, _ := inspectAccessUnit(accessUnit)

	sourceSummary := naluSummary(accessUnit)
	s.lastNALSummary = sourceSummary

	s.updateH264DiagnosticsLocked(
		accessUnit,
		pts,
	)

	s.inspectSliceOrderingLocked(
		accessUnit,
		pts,
	)

	s.cacheParametersLocked(accessUnit)

	if !s.segmentActive {
		if !hasIDR {
			return nil
		}

		if err := s.newSegmentLocked(); err != nil {
			return err
		}

		log.Printf(
			"VERSION 58 HLS started on IDR: generation=%d SPS=%t PPS=%t PTS=%d NAL=%s codec=[%s]",
			s.currentGeneration,
			len(s.sps) > 0,
			len(s.pps) > 0,
			pts,
			sourceSummary,
			codecSignatureString(s.activeCodec),
		)
	}

	if hasIDR &&
		s.currentBuffer != nil &&
		s.currentBuffer.Len() > 0 &&
		time.Since(s.segmentStart) >= segmentDuration {

		s.finishSegmentLocked()

		if err := s.newSegmentLocked(); err != nil {
			return err
		}

		log.Printf(
			"VERSION 58 new IDR segment: sequence=%d generation=%d PTS=%d NAL=%s",
			s.NextSequence,
			s.currentGeneration,
			pts,
			sourceSummary,
		)
	}

	outputAU := s.keyframeAccessUnitLocked(accessUnit)
	outputAU = s.prepareMuxAccessUnitLocked(outputAU)

	if len(outputAU) == 0 {
		return fmt.Errorf("empty H264 access unit before MPEG-TS mux")
	}

	if !hasAnnexBStartCode(outputAU) {
		return fmt.Errorf(
			"H264 access unit is not Annex-B before MPEG-TS mux",
		)
	}

	if hasIDR {
		log.Printf(
			"VERSION 58 MUX IDR: PTS=%d bytes=%d NAL=%s",
			pts,
			len(outputAU),
			s.lastMuxNALSummary,
		)
	}

	// VERSION 58: give Roku an explicit decode timestamp (DTS).
	// Pion omits the DTS field when DTS == PTS; the Nest H.264 stream
	// signals frame reordering, so keep DTS slightly behind PTS.
	dts := pts - 9000 // 100 ms on the 90 kHz MPEG clock
	if dts < 0 {
		dts = 0
	}

	return s.currentWriter.WriteH264(
		videoPID,
		pts,
		dts,
		outputAU,
	)
}

func (s *StreamSession) Close() {
	s.closeOnce.Do(
		func() {
			close(s.done)

			s.mu.Lock()

			if s.segmentActive &&
				s.currentWriter != nil &&
				s.currentBuffer != nil &&
				s.currentBuffer.Len() > 0 {

				s.finishSegmentLocked()
			}

			s.currentWriter = nil
			s.currentBuffer = nil
			s.segmentActive = false

			s.mu.Unlock()

			if s.PC != nil {
				_ = s.PC.Close()
			}
		},
	)
}

func replaceSession(s *StreamSession) {
	sessionMu.Lock()

	old := currentSession
	currentSession = s

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
	mediaEngine := &webrtc.MediaEngine{}

	err := mediaEngine.RegisterCodec(
		webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeOpus,
				ClockRate: 48000,
				Channels:  2,
			},
			PayloadType: 111,
		},
		webrtc.RTPCodecTypeAudio,
	)

	if err != nil {
		return nil, err
	}

	err = mediaEngine.RegisterCodec(
		webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			},
			PayloadType: 102,
		},
		webrtc.RTPCodecTypeVideo,
	)

	if err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
	)

	pc, err := api.NewPeerConnection(
		webrtc.Configuration{},
	)

	if err != nil {
		return nil, err
	}

	session := &StreamSession{
		PC: pc,

		Stats: &MediaStats{},

		Camera: camera,
		Index:  cameraIndex,

		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}

	pc.OnConnectionStateChange(
		func(state webrtc.PeerConnectionState) {
			log.Printf(
				"VERSION 58 WebRTC state: %s",
				state.String(),
			)
		},
	)

	pc.OnTrack(
		func(
			track *webrtc.TrackRemote,
			receiver *webrtc.RTPReceiver,
		) {
			codec := track.Codec()

			log.Printf(
				"VERSION 58 incoming track: kind=%s codec=%s payload=%d",
				track.Kind().String(),
				codec.MimeType,
				codec.PayloadType,
			)

			if track.Kind() == webrtc.RTPCodecTypeVideo {
				go func() {
					var depacketizer codecs.H264Packet

					var accessUnit []byte
					var accessUnitTimestamp uint32
					var haveTimestamp bool

					for {
						packet, _, err := track.ReadRTP()

						if err != nil {
							log.Println(
								"VERSION 58 video RTP ended:",
								err,
							)

							return
						}

						packets := atomic.AddUint64(
							&session.Stats.VideoPackets,
							1,
						)

						atomic.AddUint64(
							&session.Stats.VideoBytes,
							uint64(len(packet.Payload)),
						)

						if len(packet.Payload) == 0 {
							atomic.AddUint64(
								&session.Stats.EmptyPackets,
								1,
							)

							continue
						}

						if !haveTimestamp {
							accessUnitTimestamp = packet.Timestamp
							haveTimestamp = true
						}

						h264Data, err := depacketizer.Unmarshal(
							packet.Payload,
						)

						if err != nil {
							log.Printf(
								"VERSION 58 H264 depacketize error: %v",
								err,
							)

							accessUnit = nil
							haveTimestamp = false

							continue
						}

						if len(h264Data) > 0 {
							accessUnit = append(
								accessUnit,
								h264Data...,
							)
						}

						if packet.Marker &&
							len(accessUnit) > 0 {

							units := atomic.AddUint64(
								&session.Stats.AccessUnits,
								1,
							)

							pts := session.normalizeTimestamp(
								accessUnitTimestamp,
							)

							if err := session.writeAccessUnit(
								accessUnit,
								pts,
							); err != nil {

								log.Printf(
									"VERSION 58 MPEGTS write error: %v",
									err,
								)
							}

							if units == 1 ||
								units%30 == 0 {

								log.Printf(
									"VERSION 58 H264 AU: units=%d packets=%d size=%d PTS=%d NAL=%s",
									units,
									packets,
									len(accessUnit),
									pts,
									naluSummary(accessUnit),
								)
							}

							accessUnit = nil
							haveTimestamp = false
						}
					}
				}()
			}

			if track.Kind() == webrtc.RTPCodecTypeAudio {
				go func() {
					for {
						packet, _, err := track.ReadRTP()

						if err != nil {
							log.Println(
								"VERSION 58 audio RTP ended:",
								err,
							)

							return
						}

						if len(packet.Payload) == 0 {
							continue
						}

						atomic.AddUint64(
							&session.Stats.AudioPackets,
							1,
						)

						atomic.AddUint64(
							&session.Stats.AudioBytes,
							uint64(len(packet.Payload)),
						)
					}
				}()
			}
		},
	)

	_, err = pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	)

	if err != nil {
		session.Close()
		return nil, err
	}

	_, err = pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	)

	if err != nil {
		session.Close()
		return nil, err
	}

	_, err = pc.CreateDataChannel(
		"nestview",
		nil,
	)

	if err != nil {
		session.Close()
		return nil, err
	}

	offer, err := pc.CreateOffer(nil)

	if err != nil {
		session.Close()
		return nil, err
	}

	if err = pc.SetLocalDescription(offer); err != nil {
		session.Close()
		return nil, err
	}

	waitForICE(pc)

	local := pc.LocalDescription()

	if local == nil {
		session.Close()

		return nil, fmt.Errorf(
			"local SDP missing",
		)
	}

	upperSDP := strings.ToUpper(local.SDP)

	if !strings.Contains(
		upperSDP,
		"OPUS/48000",
	) {
		session.Close()

		return nil, fmt.Errorf(
			"generated SDP does not contain OPUS/48000",
		)
	}

	if !strings.Contains(
		upperSDP,
		"H264/90000",
	) {
		session.Close()

		return nil, fmt.Errorf(
			"generated SDP does not contain H264/90000",
		)
	}

	audioPos := strings.Index(
		local.SDP,
		"m=audio",
	)

	videoPos := strings.Index(
		local.SDP,
		"m=video",
	)

	appPos := strings.Index(
		local.SDP,
		"m=application",
	)

	if audioPos == -1 ||
		videoPos == -1 ||
		appPos == -1 {

		session.Close()

		return nil, fmt.Errorf(
			"SDP missing audio, video, or application m-line",
		)
	}

	if !(audioPos < videoPos &&
		videoPos < appPos) {

		session.Close()

		return nil, fmt.Errorf(
			"SDP order is not audio-video-application",
		)
	}

	log.Println(
		"VERSION 58 SDP confirmed: audio -> video -> application",
	)

	payload := map[string]string{
		"device":   camera.Device,
		"offerSdp": local.SDP,
	}

	data, err := json.Marshal(payload)

	if err != nil {
		session.Close()
		return nil, err
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Post(
		nestBackend+"/api/webrtc",
		"application/json",
		bytes.NewReader(data),
	)

	if err != nil {
		session.Close()
		return nil, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)

	if err != nil {
		session.Close()
		return nil, err
	}

	log.Printf(
		"VERSION 58 Nest backend HTTP status: %d",
		resp.StatusCode,
	)

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		session.Close()

		return nil, fmt.Errorf(
			"Nest backend returned %d: %s",
			resp.StatusCode,
			string(body),
		)
	}

	var result interface{}

	if err := json.Unmarshal(
		body,
		&result,
	); err != nil {

		session.Close()

		return nil, fmt.Errorf(
			"could not decode Nest response: %w",
			err,
		)
	}

	answer := findAnswerSDP(result)

	if answer == "" {
		session.Close()

		return nil, fmt.Errorf(
			"Nest response did not contain recognizable answer SDP",
		)
	}

	if err = pc.SetRemoteDescription(
		webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  answer,
		},
	); err != nil {

		session.Close()
		return nil, err
	}

	log.Println(
		"VERSION 58 Nest WebRTC session started",
	)

	return session, nil
}

func health(
	w http.ResponseWriter,
	r *http.Request,
) {
	session := getSession()

	streaming := false

	var segments uint64
	var validated uint64
	var validationErrors uint64
	var discontinuities uint64
	var codecChanges uint64
	var generation uint64

	var duplicateSPS uint64
	var duplicatePPS uint64
	var cleanedAUs uint64

	var sliceHeaders uint64
	var sliceErrors uint64

	var iSlices uint64
	var pSlices uint64
	var bSlices uint64

	var pocBackward uint64

	var muxAccessUnits uint64
	var muxNALUnits uint64
	var muxIDRUnits uint64
	var annexBRepairs uint64

	var lastMuxNAL string
	var lastMuxBytes int

	if session != nil {
		segments = atomic.LoadUint64(
			&session.Stats.HLSSegments,
		)

		validated = atomic.LoadUint64(
			&session.Stats.ValidatedTS,
		)

		validationErrors = atomic.LoadUint64(
			&session.Stats.ValidationError,
		)

		discontinuities = atomic.LoadUint64(
			&session.Stats.TimestampDiscontinuities,
		)

		codecChanges = atomic.LoadUint64(
			&session.Stats.CodecChanges,
		)

		duplicateSPS = atomic.LoadUint64(
			&session.Stats.DuplicateSPSRemoved,
		)

		duplicatePPS = atomic.LoadUint64(
			&session.Stats.DuplicatePPSRemoved,
		)

		cleanedAUs = atomic.LoadUint64(
			&session.Stats.CleanedAccessUnits,
		)

		sliceHeaders = atomic.LoadUint64(
			&session.Stats.SliceHeadersParsed,
		)

		sliceErrors = atomic.LoadUint64(
			&session.Stats.SliceParseErrors,
		)

		iSlices = atomic.LoadUint64(
			&session.Stats.ISlices,
		)

		pSlices = atomic.LoadUint64(
			&session.Stats.PSlices,
		)

		bSlices = atomic.LoadUint64(
			&session.Stats.BSlices,
		)

		pocBackward = atomic.LoadUint64(
			&session.Stats.POCBackwardEvents,
		)

		muxAccessUnits = atomic.LoadUint64(
			&session.Stats.MuxAccessUnits,
		)

		muxNALUnits = atomic.LoadUint64(
			&session.Stats.MuxNALUnits,
		)

		muxIDRUnits = atomic.LoadUint64(
			&session.Stats.MuxIDRUnits,
		)

		annexBRepairs = atomic.LoadUint64(
			&session.Stats.AnnexBRepairs,
		)

		session.mu.RLock()

		generation = session.currentGeneration
		lastMuxNAL = session.lastMuxNALSummary
		lastMuxBytes = session.lastMuxBytes

		session.mu.RUnlock()

		streaming = segments > 0
	}

	frozenVOD.mu.RLock()

	vodReady := frozenVOD.Ready
	vodSegments := len(frozenVOD.Segments)
	vodDuration := frozenVOD.TotalDuration

	frozenVOD.mu.RUnlock()

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status":    "ok",
			"bridge":    "pion-h264-hls",
			"version":   31,
			"streaming": streaming,

			"segments":         segments,
			"validatedTS":      validated,
			"validationErrors": validationErrors,

			"timestampDiscontinuities": discontinuities,

			"codecChanges": codecChanges,

			"generation": generation,

			"duplicateSPSRemoved": duplicateSPS,

			"duplicatePPSRemoved": duplicatePPS,

			"cleanedAccessUnits": cleanedAUs,

			"sliceHeadersParsed": sliceHeaders,

			"sliceParseErrors": sliceErrors,

			"iSlices": iSlices,

			"pSlices": pSlices,

			"bSlices": bSlices,

			"pocBackwardEvents": pocBackward,

			"muxAccessUnits": muxAccessUnits,

			"muxNALUnits": muxNALUnits,

			"muxIDRUnits": muxIDRUnits,

			"annexBRepairs": annexBRepairs,

			"lastMuxNALTypes": lastMuxNAL,

			"lastMuxBytes": lastMuxBytes,

			"frozenVODReady": vodReady,

			"frozenVODSegments": vodSegments,

			"frozenVODDuration": vodDuration,

			"frozenVODPlaylist": "/vod/index.m3u8",

			"annexBCanonicalization": true,

			"sliceOrderingDiagnostics": true,

			"parameterSetDeduplication": true,

			"codecTransitionHandling": true,

			"hlsDiscontinuity": true,

			"timestampOffset": ptsOffset,

			"mediaSelfCheck": true,
		},
	)
}
func start(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.Method != http.MethodPost {
		writeJSON(
			w,
			405,
			map[string]string{
				"error": "POST required",
			},
		)

		return
	}

	body, err := io.ReadAll(r.Body)

	if err != nil {
		writeJSON(
			w,
			400,
			map[string]string{
				"error": "could not read request body",
			},
		)

		return
	}

	log.Printf(
		"VERSION 58 Shortcut body: %q",
		string(body),
	)

	var rawRequest map[string]interface{}

	if err := json.Unmarshal(
		body,
		&rawRequest,
	); err != nil {

		writeJSON(
			w,
			400,
			map[string]string{
				"error": "invalid JSON: " + err.Error(),
			},
		)

		return
	}

	cameraIndex := 0
	cameraFound := false

	for key, value := range rawRequest {
		if strings.TrimSpace(key) != "camera" {
			continue
		}

		switch v := value.(type) {
		case float64:
			cameraIndex = int(v)
			cameraFound = true

		case string:
			v = strings.TrimSpace(v)

			if parsed, parseErr := strconv.Atoi(v); parseErr == nil {
				cameraIndex = parsed
				cameraFound = true
			}
		}
	}

	if !cameraFound {
		writeJSON(
			w,
			400,
			map[string]string{
				"error": "camera field missing or invalid",
			},
		)

		return
	}

	log.Printf(
		"VERSION 58 start request parsed: camera=%d; requesting camera list",
		cameraIndex,
	)

	cameras, err := getCameras()

	if err != nil {
		log.Printf(
			"VERSION 58 start stopped at camera backend: %v",
			err,
		)

		writeJSON(
			w,
			502,
			map[string]string{
				"error": err.Error(),
			},
		)

		return
	}

	log.Printf(
		"VERSION 58 camera list ready: count=%d requested=%d",
		len(cameras),
		cameraIndex,
	)

	if cameraIndex < 0 ||
		cameraIndex >= len(cameras) {

		writeJSON(
			w,
			400,
			map[string]string{
				"error": "invalid camera index",
			},
		)

		return
	}

	camera := cameras[cameraIndex]

	log.Printf(
		"VERSION 58 starting camera %d: %s",
		cameraIndex,
		camera.Name,
	)

	clearFrozenVOD()

	session, err := createStreamSession(
		camera,
		cameraIndex,
	)

	if err != nil {
		log.Println(
			"VERSION 58 camera start failed:",
			err,
		)

		writeJSON(
			w,
			502,
			map[string]string{
				"error": err.Error(),
			},
		)

		return
	}

	replaceSession(session)

	select {
	case <-session.ready:
		log.Println(
			"VERSION 58 HLS READY",
		)

	case <-time.After(25 * time.Second):
		session.Close()

		sessionMu.Lock()

		if currentSession == session {
			currentSession = nil
		}

		sessionMu.Unlock()

		writeJSON(
			w,
			504,
			map[string]string{
				"error": "HLS keyframe segment was not ready within 25 seconds",
			},
		)

		return
	}

	session.mu.RLock()

	var diagnostic interface{}

	if len(session.Segments) > 0 {
		diagnostic = session.Segments[len(session.Segments)-1].Diagnostics
	}

	h264Diagnostic := session.h264Diagnostics
	sliceDiagnostic := session.h264SliceDiagnostics
	generation := session.currentGeneration
	lastOriginal := session.lastNALSummary
	lastCleaned := session.lastCleanedNALSummary

	lastMuxNAL := session.lastMuxNALSummary
	lastMuxBytes := session.lastMuxBytes

	session.mu.RUnlock()

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status": "streaming",

			"camera": cameraIndex,

			"name": camera.Name,

			"total": len(cameras),

			"hls": "/live/index.m3u8",

			"freeze": "/vod/freeze",

			"vod": "/vod/index.m3u8",

			"version": 31,

			"videoPackets": atomic.LoadUint64(
				&session.Stats.VideoPackets,
			),

			"accessUnits": atomic.LoadUint64(
				&session.Stats.AccessUnits,
			),

			"segments": atomic.LoadUint64(
				&session.Stats.HLSSegments,
			),

			"emptyPackets": atomic.LoadUint64(
				&session.Stats.EmptyPackets,
			),

			"validatedTS": atomic.LoadUint64(
				&session.Stats.ValidatedTS,
			),

			"validationErrors": atomic.LoadUint64(
				&session.Stats.ValidationError,
			),

			"timestampDiscontinuities": atomic.LoadUint64(
				&session.Stats.TimestampDiscontinuities,
			),

			"codecChanges": atomic.LoadUint64(
				&session.Stats.CodecChanges,
			),

			"duplicateSPSRemoved": atomic.LoadUint64(
				&session.Stats.DuplicateSPSRemoved,
			),

			"duplicatePPSRemoved": atomic.LoadUint64(
				&session.Stats.DuplicatePPSRemoved,
			),

			"cleanedAccessUnits": atomic.LoadUint64(
				&session.Stats.CleanedAccessUnits,
			),

			"sliceHeadersParsed": atomic.LoadUint64(
				&session.Stats.SliceHeadersParsed,
			),

			"sliceParseErrors": atomic.LoadUint64(
				&session.Stats.SliceParseErrors,
			),

			"iSlices": atomic.LoadUint64(
				&session.Stats.ISlices,
			),

			"pSlices": atomic.LoadUint64(
				&session.Stats.PSlices,
			),

			"bSlices": atomic.LoadUint64(
				&session.Stats.BSlices,
			),

			"pocBackwardEvents": atomic.LoadUint64(
				&session.Stats.POCBackwardEvents,
			),

			"muxAccessUnits": atomic.LoadUint64(
				&session.Stats.MuxAccessUnits,
			),

			"muxNALUnits": atomic.LoadUint64(
				&session.Stats.MuxNALUnits,
			),

			"muxIDRUnits": atomic.LoadUint64(
				&session.Stats.MuxIDRUnits,
			),

			"annexBRepairs": atomic.LoadUint64(
				&session.Stats.AnnexBRepairs,
			),

			"generation": generation,

			"lastOriginalNALTypes": lastOriginal,

			"lastCleanedNALTypes": lastCleaned,

			"lastMuxNALTypes": lastMuxNAL,

			"lastMuxBytes": lastMuxBytes,

			"lastDiagnostic": diagnostic,

			"h264Diagnostic": h264Diagnostic,

			"sliceDiagnostic": sliceDiagnostic,

			"codecTransitions": "/debug/h264",

			"muxDiagnostics": "/debug/mux",
		},
	)
}

func statusHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session := getSession()

	if session == nil {
		writeJSON(
			w,
			200,
			map[string]interface{}{
				"streaming": false,
				"version":   31,
			},
		)

		return
	}

	session.mu.RLock()

	hasSPS := len(session.sps) > 0
	hasPPS := len(session.pps) > 0

	pts := session.normalizedPTS

	nalSummary := session.lastNALSummary
	cleanedNALSummary := session.lastCleanedNALSummary

	lastMuxNAL := session.lastMuxNALSummary
	lastMuxBytes := session.lastMuxBytes
	lastMuxPrefix := session.lastMuxPrefixHex

	h264 := session.h264Diagnostics
	slice := session.h264SliceDiagnostics
	activeCodec := session.activeCodec

	generation := session.currentGeneration

	pendingDiscontinuity :=
		session.pendingDiscontinuity

	changes := append(
		[]CodecChange(nil),
		session.codecChanges...,
	)

	var lastSegment interface{}

	if len(session.Segments) > 0 {
		seg := session.Segments[len(session.Segments)-1]

		lastSegment = map[string]interface{}{
			"sequence":            seg.Sequence,
			"duration":            seg.Duration,
			"bytes":               len(seg.Data),
			"validated":           seg.Validated,
			"validationErr":       seg.ValidateErr,
			"diagnostics":         seg.Diagnostics,
			"codec":               seg.Codec,
			"generation":          seg.Generation,
			"discontinuityBefore": seg.DiscontinuityBefore,
		}
	}

	session.mu.RUnlock()

	frozenVOD.mu.RLock()

	vodReady := frozenVOD.Ready
	vodCreated := frozenVOD.CreatedAt
	vodSegments := len(frozenVOD.Segments)
	vodDuration := frozenVOD.TotalDuration
	vodGeneration := frozenVOD.Generation
	vodCodec := frozenVOD.Codec

	frozenVOD.mu.RUnlock()

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"streaming": true,
			"version":   31,

			"camera": session.Index,

			"name": session.Camera.Name,

			"videoPackets": atomic.LoadUint64(
				&session.Stats.VideoPackets,
			),

			"videoBytes": atomic.LoadUint64(
				&session.Stats.VideoBytes,
			),

			"accessUnits": atomic.LoadUint64(
				&session.Stats.AccessUnits,
			),

			"segments": atomic.LoadUint64(
				&session.Stats.HLSSegments,
			),

			"audioPackets": atomic.LoadUint64(
				&session.Stats.AudioPackets,
			),

			"emptyPackets": atomic.LoadUint64(
				&session.Stats.EmptyPackets,
			),

			"validatedTS": atomic.LoadUint64(
				&session.Stats.ValidatedTS,
			),

			"validationErrors": atomic.LoadUint64(
				&session.Stats.ValidationError,
			),

			"timestampDiscontinuities": atomic.LoadUint64(
				&session.Stats.TimestampDiscontinuities,
			),

			"codecChanges": atomic.LoadUint64(
				&session.Stats.CodecChanges,
			),

			"duplicateSPSRemoved": atomic.LoadUint64(
				&session.Stats.DuplicateSPSRemoved,
			),

			"duplicatePPSRemoved": atomic.LoadUint64(
				&session.Stats.DuplicatePPSRemoved,
			),

			"cleanedAccessUnits": atomic.LoadUint64(
				&session.Stats.CleanedAccessUnits,
			),

			"sliceHeadersParsed": atomic.LoadUint64(
				&session.Stats.SliceHeadersParsed,
			),

			"sliceParseErrors": atomic.LoadUint64(
				&session.Stats.SliceParseErrors,
			),

			"iSlices": atomic.LoadUint64(
				&session.Stats.ISlices,
			),

			"pSlices": atomic.LoadUint64(
				&session.Stats.PSlices,
			),

			"bSlices": atomic.LoadUint64(
				&session.Stats.BSlices,
			),

			"pocBackwardEvents": atomic.LoadUint64(
				&session.Stats.POCBackwardEvents,
			),

			"muxAccessUnits": atomic.LoadUint64(
				&session.Stats.MuxAccessUnits,
			),

			"muxNALUnits": atomic.LoadUint64(
				&session.Stats.MuxNALUnits,
			),

			"muxIDRUnits": atomic.LoadUint64(
				&session.Stats.MuxIDRUnits,
			),

			"annexBRepairs": atomic.LoadUint64(
				&session.Stats.AnnexBRepairs,
			),

			"sps": hasSPS,

			"pps": hasPPS,

			"normalizedPTS": pts,

			"lastNALTypes": nalSummary,

			"lastCleanedNALTypes": cleanedNALSummary,

			"lastMuxNALTypes": lastMuxNAL,

			"lastMuxBytes": lastMuxBytes,

			"lastMuxPrefixHex": lastMuxPrefix,

			"h264": h264,

			"slice": slice,

			"activeCodec": activeCodec,

			"generation": generation,

			"pendingDiscontinuity": pendingDiscontinuity,

			"codecHistory": changes,

			"lastSegment": lastSegment,

			"frozenVODReady": vodReady,

			"frozenVODCreatedAt": vodCreated,

			"frozenVODSegments": vodSegments,

			"frozenVODDuration": vodDuration,

			"frozenVODGeneration": vodGeneration,

			"frozenVODCodec": vodCodec,

			"frozenVODPlaylist": "/vod/index.m3u8",

			"codecTransitions": "/debug/h264",

			"muxDiagnostics": "/debug/mux",
		},
	)
}

func muxDiagnosticsHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session := getSession()

	if session == nil {
		writeJSON(
			w,
			http.StatusNotFound,
			map[string]interface{}{
				"error":   "no active stream",
				"version": 31,
			},
		)

		return
	}

	session.mu.RLock()

	lastOriginal := session.lastNALSummary
	lastCleaned := session.lastCleanedNALSummary
	lastMuxNAL := session.lastMuxNALSummary
	lastMuxBytes := session.lastMuxBytes
	lastMuxPrefix := session.lastMuxPrefixHex

	session.mu.RUnlock()

	writeJSON(
		w,
		http.StatusOK,
		map[string]interface{}{
			"version": 31,

			"camera": session.Index,

			"name": session.Camera.Name,

			"originalNALTypes": lastOriginal,

			"cleanedNALTypes": lastCleaned,

			"muxNALTypes": lastMuxNAL,

			"muxBytes": lastMuxBytes,

			"muxPrefixHex": lastMuxPrefix,

			"muxAccessUnits": atomic.LoadUint64(
				&session.Stats.MuxAccessUnits,
			),

			"muxNALUnits": atomic.LoadUint64(
				&session.Stats.MuxNALUnits,
			),

			"muxIDRUnits": atomic.LoadUint64(
				&session.Stats.MuxIDRUnits,
			),

			"annexBRepairs": atomic.LoadUint64(
				&session.Stats.AnnexBRepairs,
			),

			"expectedFormat": "Annex-B H264 access units with 00 00 00 01 start codes",

			"test": "verify exact H264 access unit presented to MPEG-TS muxer",
		},
	)
}
func readbackDebugHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session := getSession()

	if session == nil {
		writeJSON(
			w,
			http.StatusNotFound,
			map[string]interface{}{
				"error":   "no active stream",
				"version": 31,
			},
		)
		return
	}

	session.mu.RLock()

	if len(session.Segments) == 0 {
		session.mu.RUnlock()

		writeJSON(
			w,
			http.StatusNotFound,
			map[string]interface{}{
				"error":   "no completed TS segments",
				"version": 31,
			},
		)
		return
	}

	segment := copyHLSSegment(
		session.Segments[len(session.Segments)-1],
	)

	session.mu.RUnlock()

	diagnostics := readbackTS(segment.Data)
	compatibility := analyzeReadbackH264(segment.Data)

	writeJSON(
		w,
		http.StatusOK,
		map[string]interface{}{
			"version":           31,
			"sequence":          segment.Sequence,
			"generation":        segment.Generation,
			"duration":          segment.Duration,
			"bytes":             len(segment.Data),
			"diagnostics":       diagnostics,
			"h264Compatibility": compatibility,
			"version31Test":     "Text-safe decoder artifact export: chunked base64 of the exact completed MPEG-TS segment; slice-sequence diagnostics retained",
		},
	)
}
func tsDebugHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session := getSession()

	if session == nil {
		writeJSON(
			w,
			http.StatusNotFound,
			map[string]interface{}{
				"error":   "no active stream",
				"version": 31,
			},
		)

		return
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	if len(session.Segments) == 0 {
		writeJSON(
			w,
			http.StatusNotFound,
			map[string]interface{}{
				"error":   "no completed TS segment",
				"version": 31,
			},
		)

		return
	}

	segment := session.Segments[len(session.Segments)-1]

	writeJSON(
		w,
		http.StatusOK,
		map[string]interface{}{
			"version":             31,
			"sequence":            segment.Sequence,
			"duration":            segment.Duration,
			"bytes":               len(segment.Data),
			"validated":           segment.Validated,
			"validationError":     segment.ValidateErr,
			"diagnostics":         segment.Diagnostics,
			"codec":               segment.Codec,
			"generation":          segment.Generation,
			"discontinuityBefore": segment.DiscontinuityBefore,
		},
	)
}

func tsDownloadHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session := getSession()

	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error":   "no active stream",
			"version": 31,
		})
		return
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	if len(session.Segments) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error":   "no completed TS segment",
			"version": 31,
		})
		return
	}

	segment := session.Segments[len(session.Segments)-1]
	filename := fmt.Sprintf("nestview-v32-camera%d-segment%d.ts", session.Camera, segment.Sequence)

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(segment.Data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-NestView-Version", "31")
	w.Header().Set("X-NestView-Sequence", strconv.Itoa(segment.Sequence))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(segment.Data)
}

func snapshotLatestDebugSegment() (HLSSegment, int, error) {
	session := getSession()

	if session == nil {
		return HLSSegment{}, 0, fmt.Errorf("no active stream")
	}

	session.mu.RLock()

	if len(session.Segments) == 0 {
		session.mu.RUnlock()
		return HLSSegment{}, 0, fmt.Errorf("no completed TS segment")
	}

	segment := copyHLSSegment(session.Segments[len(session.Segments)-1])
	camera := session.Index

	session.mu.RUnlock()

	debugSegmentSnapshot.mu.Lock()
	debugSegmentSnapshot.Ready = true
	debugSegmentSnapshot.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	debugSegmentSnapshot.CameraIndex = camera
	debugSegmentSnapshot.Segment = segment
	debugSegmentSnapshot.mu.Unlock()

	log.Printf(
		"VERSION 58 DEBUG SEGMENT SNAPSHOT: camera=%d generation=%d sequence=%d bytes=%d",
		camera,
		segment.Generation,
		segment.Sequence,
		len(segment.Data),
	)

	return segment, camera, nil
}

func getDebugSegmentSnapshot() (HLSSegment, int, string, bool) {
	debugSegmentSnapshot.mu.RLock()
	defer debugSegmentSnapshot.mu.RUnlock()

	if !debugSegmentSnapshot.Ready || len(debugSegmentSnapshot.Segment.Data) == 0 {
		return HLSSegment{}, 0, "", false
	}

	return copyHLSSegment(debugSegmentSnapshot.Segment),
		debugSegmentSnapshot.CameraIndex,
		debugSegmentSnapshot.CreatedAt,
		true
}

func tsBase64Handler(
	w http.ResponseWriter,
	r *http.Request,
) {
	offset := 0

	if value := r.URL.Query().Get("offset"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error":   "offset must be a non-negative integer",
				"version": 31,
			})
			return
		}
		offset = parsed
	}

	length := 24576

	if value := r.URL.Query().Get("length"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 49152 {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error":   "length must be between 1 and 49152 bytes",
				"version": 31,
			})
			return
		}
		length = parsed
	}

	var segment HLSSegment
	var camera int
	var createdAt string

	/*
		V30 rule:
		offset 0 deliberately creates one immutable snapshot.
		Every later offset reads that same snapshot, even while
		the live HLS segment list continues changing.
	*/
	if offset == 0 {
		var err error

		segment, camera, err = snapshotLatestDebugSegment()
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"error":   err.Error(),
				"version": 31,
			})
			return
		}

		_, _, createdAt, _ = getDebugSegmentSnapshot()
	} else {
		var ok bool

		segment, camera, createdAt, ok = getDebugSegmentSnapshot()
		if !ok {
			writeJSON(w, http.StatusConflict, map[string]interface{}{
				"error":   "no debug snapshot exists; request offset=0 first",
				"version": 31,
			})
			return
		}
	}

	total := len(segment.Data)

	if offset > total {
		writeJSON(w, http.StatusRequestedRangeNotSatisfiable, map[string]interface{}{
			"error":      "offset is beyond end of snapshotted segment",
			"version":    31,
			"totalBytes": total,
		})
		return
	}

	end := offset + length
	if end > total {
		end = total
	}

	chunk := segment.Data[offset:end]

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version":       31,
		"camera":        camera,
		"sequence":      segment.Sequence,
		"generation":    segment.Generation,
		"snapshotAt":    createdAt,
		"totalBytes":    total,
		"offset":        offset,
		"chunkBytes":    len(chunk),
		"nextOffset":    end,
		"complete":      end >= total,
		"encoding":      "base64",
		"data":          base64.StdEncoding.EncodeToString(chunk),
		"version31Test": "Stable immutable TS snapshot for exact external decoder verification",
	})
}

func tsBase64TextHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	segment, camera, err := snapshotLatestDebugSegment()

	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error":   err.Error(),
			"version": 31,
		})
		return
	}

	encoded := base64.StdEncoding.EncodeToString(segment.Data)

	filename := fmt.Sprintf(
		"nestview-v32-camera%d-generation%d-segment%d-base64.txt",
		camera,
		segment.Generation,
		segment.Sequence,
	)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-NestView-Version", "31")
	w.Header().Set("X-NestView-Generation", strconv.FormatUint(segment.Generation, 10))
	w.Header().Set("X-NestView-Sequence", strconv.Itoa(segment.Sequence))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, encoded)
}

func h264DebugHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	session := getSession()

	if session == nil {
		writeJSON(
			w,
			http.StatusNotFound,
			map[string]interface{}{
				"error":   "no active stream",
				"version": 31,
			},
		)

		return
	}

	session.mu.RLock()

	h264 := session.h264Diagnostics
	slice := session.h264SliceDiagnostics
	activeCodec := session.activeCodec
	generation := session.currentGeneration

	changes := append(
		[]CodecChange(nil),
		session.codecChanges...,
	)

	lastNAL := session.lastNALSummary
	lastCleaned := session.lastCleanedNALSummary
	lastMux := session.lastMuxNALSummary
	lastMuxBytes := session.lastMuxBytes
	lastMuxPrefix := session.lastMuxPrefixHex

	session.mu.RUnlock()

	writeJSON(
		w,
		http.StatusOK,
		map[string]interface{}{
			"version": 31,

			"camera": session.Index,

			"name": session.Camera.Name,

			"h264": h264,

			"slice": slice,

			"activeCodec": activeCodec,

			"generation": generation,

			"codecChanges": changes,

			"lastNALTypes": lastNAL,

			"lastCleanedNALTypes": lastCleaned,

			"lastMuxNALTypes": lastMux,

			"lastMuxBytes": lastMuxBytes,

			"lastMuxPrefixHex": lastMuxPrefix,

			"duplicateSPSRemoved": atomic.LoadUint64(
				&session.Stats.DuplicateSPSRemoved,
			),

			"duplicatePPSRemoved": atomic.LoadUint64(
				&session.Stats.DuplicatePPSRemoved,
			),

			"cleanedAccessUnits": atomic.LoadUint64(
				&session.Stats.CleanedAccessUnits,
			),

			"sliceHeadersParsed": atomic.LoadUint64(
				&session.Stats.SliceHeadersParsed,
			),

			"sliceParseErrors": atomic.LoadUint64(
				&session.Stats.SliceParseErrors,
			),

			"iSlices": atomic.LoadUint64(
				&session.Stats.ISlices,
			),

			"pSlices": atomic.LoadUint64(
				&session.Stats.PSlices,
			),

			"bSlices": atomic.LoadUint64(
				&session.Stats.BSlices,
			),

			"pocBackwardEvents": atomic.LoadUint64(
				&session.Stats.POCBackwardEvents,
			),

			"annexBRepairs": atomic.LoadUint64(
				&session.Stats.AnnexBRepairs,
			),
		},
	)
}

func liveMasterPlaylistHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	log.Printf("VERSION 58 ROKU REQUEST STEP=MASTER_REDIRECT method=%s path=%s ua=%q remote=%s", r.Method, r.URL.Path, r.UserAgent(), r.RemoteAddr)

	// Keep the redirect mechanism proven by V56, but redirect back to the
	// NestView media playlist. This separates redirect acceptance from
	// NestView media-playlist/segment acceptance.
	target := "https://nestview-roku-pion-v2.onrender.com/live/media.m3u8"
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
	log.Printf("VERSION 58 ROKU REDIRECT SENT status=302 target=%s", target)
}

func livePlaylistHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	log.Printf("VERSION 58 ROKU REQUEST STEP=MEDIA method=%s path=%s query=%q ua=%q remote=%s", r.Method, r.URL.Path, r.URL.RawQuery, r.UserAgent(), r.RemoteAddr)
	session := getSession()

	if session == nil {
		http.Error(
			w,
			"no active stream",
			http.StatusNotFound,
		)

		return
	}

	session.mu.RLock()

	segments := append(
		[]HLSSegment(nil),
		session.Segments...,
	)

	session.mu.RUnlock()

	if len(segments) == 0 {
		http.Error(
			w,
			"no HLS segments ready",
			http.StatusNotFound,
		)

		return
	}

	// VERSION 58: Roku startup buffer guard. Prefer the newest codec
	// generation only after it has at least 3 complete validated segments.
	// Until then, keep advertising the previous generation if it has enough
	// segments. This prevents Roku from seeing only 1-2 segments and stalling
	// at 33% during Nest's frequent 1080p/360p codec transitions.
	latestGeneration := segments[len(segments)-1].Generation
	selectedGeneration := latestGeneration
	const rokuStartupSegments = 3

	counts := make(map[uint64]int)
	for _, segment := range segments {
		if segment.Validated && len(segment.Data) > 0 {
			counts[segment.Generation]++
		}
	}

	if counts[latestGeneration] < rokuStartupSegments {
		for i := len(segments) - 1; i >= 0; i-- {
			g := segments[i].Generation
			if g != latestGeneration && counts[g] >= rokuStartupSegments {
				selectedGeneration = g
				break
			}
		}
	}

	filtered := make([]HLSSegment, 0, len(segments))
	for _, segment := range segments {
		if segment.Generation != selectedGeneration {
			continue
		}
		if !segment.Validated || len(segment.Data) == 0 {
			continue
		}
		segment.DiscontinuityBefore = false
		filtered = append(filtered, segment)
	}

	log.Printf(
		"VERSION 58 ROKU STARTUP WINDOW: latestGeneration=%d latestCount=%d selectedGeneration=%d selectedCount=%d minimum=%d",
		latestGeneration,
		counts[latestGeneration],
		selectedGeneration,
		len(filtered),
		rokuStartupSegments,
	)

	if len(filtered) == 0 {
		http.Error(
			w,
			"no current-generation HLS segments ready",
			http.StatusNotFound,
		)

		return
	}

	// VERSION 58: Never advertise an undersized startup playlist to Roku.
	// V45 proved Roku requested the media playlist with only 1 segment, then
	// again with only 2 segments, and quit before requesting any TS file.
	// Return 503 until a full 3-segment same-generation startup window exists.
	if len(filtered) < rokuStartupSegments {
		log.Printf(
			"VERSION 58 ROKU MEDIA NOT READY: generation=%d ready=%d required=%d returning=503",
			selectedGeneration,
			len(filtered),
			rokuStartupSegments,
		)
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Retry-After", "1")
		http.Error(
			w,
			"Roku HLS startup window not ready",
			http.StatusServiceUnavailable,
		)
		return
	}

	if len(filtered) != len(segments) {
		log.Printf(
			"VERSION 58 LIVE PLAYLIST GENERATION FILTER: generation=%d kept=%d dropped=%d",
			selectedGeneration,
			len(filtered),
			len(segments)-len(filtered),
		)
	}

	segments = filtered
	targetDuration := 1

	for _, segment := range segments {
		value := int(math.Ceil(segment.Duration))

		if value > targetDuration {
			targetDuration = value
		}
	}

	mediaSequence := segments[0].Sequence

	var playlist strings.Builder

	playlist.WriteString("#EXTM3U\n")
	playlist.WriteString("#EXT-X-VERSION:3\n")

	playlist.WriteString(
		fmt.Sprintf(
			"#EXT-X-TARGETDURATION:%d\n",
			targetDuration,
		),
	)

	playlist.WriteString(
		fmt.Sprintf(
			"#EXT-X-MEDIA-SEQUENCE:%d\n",
			mediaSequence,
		),
	)
	playlist.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")

	for _, segment := range segments {
		if segment.DiscontinuityBefore {
			playlist.WriteString(
				"#EXT-X-DISCONTINUITY\n",
			)
		}

		durationSeconds := int(math.Ceil(segment.Duration))
		if durationSeconds < 1 {
			durationSeconds = 1
		}

		playlist.WriteString(
			fmt.Sprintf(
				"#EXTINF:%d,\n",
				durationSeconds,
			),
		)

		playlist.WriteString(
			fmt.Sprintf(
				"https://nestview-roku-pion-v2.onrender.com/live/segment%d.ts\n",
				segment.Sequence,
			),
		)
	}

	w.Header().Set(
		"Content-Type",
		"application/x-mpegurl",
	)

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate",
	)

	w.Header().Set(
		"Access-Control-Allow-Origin",
		"*",
	)

	log.Printf("VERSION 58 ROKU MEDIA PLAYLIST BODY:\n%s", playlist.String())

	_, _ = io.WriteString(
		w,
		playlist.String(),
	)
}

func liveSegmentHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	log.Printf("VERSION 58 ROKU REQUEST STEP=SEGMENT method=%s path=%s query=%q range=%q ua=%q remote=%s", r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Range"), r.UserAgent(), r.RemoteAddr)
	session := getSession()

	if session == nil {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimPrefix(
		r.URL.Path,
		"/live/segment",
	)

	name = strings.TrimSuffix(
		name,
		".ts",
	)

	sequence, err := strconv.Atoi(name)

	if err != nil {
		http.NotFound(w, r)
		return
	}

	session.mu.RLock()

	var data []byte

	for _, segment := range session.Segments {
		if segment.Sequence == sequence {
			data = append(
				[]byte(nil),
				segment.Data...,
			)

			break
		}
	}

	session.mu.RUnlock()

	if len(data) == 0 {
		http.NotFound(w, r)
		return
	}

	w.Header().Set(
		"Content-Type",
		"video/mp2t",
	)

	w.Header().Set(
		"Cache-Control",
		"no-store",
	)

	w.Header().Set(
		"Access-Control-Allow-Origin",
		"*",
	)

	w.Header().Set(
		"Content-Length",
		strconv.Itoa(len(data)),
	)

	_, _ = w.Write(data)
}

func freezeVODHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.Method != http.MethodGet &&
		r.Method != http.MethodPost {

		writeJSON(
			w,
			http.StatusMethodNotAllowed,
			map[string]interface{}{
				"error":   "GET or POST required",
				"version": 31,
			},
		)

		return
	}

	session := getSession()

	if session == nil {
		writeJSON(
			w,
			http.StatusNotFound,
			map[string]interface{}{
				"error":   "no active stream",
				"version": 31,
			},
		)

		return
	}

	segments, duration := buildFrozenVOD(
		session,
	)

	if segments == 0 {
		writeJSON(
			w,
			http.StatusConflict,
			map[string]interface{}{
				"error":   "no completed validated segments available to freeze",
				"version": 31,
			},
		)

		return
	}

	frozenVOD.mu.RLock()

	codec := frozenVOD.Codec
	generation := frozenVOD.Generation
	camera := frozenVOD.CameraIndex
	name := frozenVOD.CameraName

	frozenVOD.mu.RUnlock()

	writeJSON(
		w,
		http.StatusOK,
		map[string]interface{}{
			"status": "frozen",

			"version": 31,

			"camera": camera,

			"name": name,

			"segments": segments,

			"duration": duration,

			"generation": generation,

			"codec": codec,

			"playlist": "/vod/index.m3u8",
		},
	)
}

func vodStatusHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	frozenVOD.mu.RLock()
	defer frozenVOD.mu.RUnlock()

	writeJSON(
		w,
		http.StatusOK,
		map[string]interface{}{
			"version": 31,

			"ready": frozenVOD.Ready,

			"createdAt": frozenVOD.CreatedAt,

			"camera": frozenVOD.CameraIndex,

			"name": frozenVOD.CameraName,

			"segments": len(frozenVOD.Segments),

			"duration": frozenVOD.TotalDuration,

			"generation": frozenVOD.Generation,

			"codec": frozenVOD.Codec,

			"playlist": "/vod/index.m3u8",
		},
	)
}

func vodPlaylistHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	frozenVOD.mu.RLock()

	ready := frozenVOD.Ready

	segments := append(
		[]HLSSegment(nil),
		frozenVOD.Segments...,
	)

	frozenVOD.mu.RUnlock()

	if !ready || len(segments) == 0 {
		http.Error(
			w,
			"frozen VOD not ready",
			http.StatusNotFound,
		)

		return
	}

	targetDuration := 1

	for _, segment := range segments {
		value := int(math.Ceil(segment.Duration))

		if value > targetDuration {
			targetDuration = value
		}
	}

	var playlist strings.Builder

	playlist.WriteString("#EXTM3U\n")
	playlist.WriteString("#EXT-X-VERSION:3\n")
	playlist.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	playlist.WriteString(
		fmt.Sprintf(
			"#EXT-X-TARGETDURATION:%d\n",
			targetDuration,
		),
	)

	playlist.WriteString(
		fmt.Sprintf(
			"#EXT-X-MEDIA-SEQUENCE:%d\n",
			segments[0].Sequence,
		),
	)

	for _, segment := range segments {
		playlist.WriteString(
			fmt.Sprintf(
				"#EXTINF:%.3f,\n",
				segment.Duration,
			),
		)

		playlist.WriteString(
			fmt.Sprintf(
				"/vod/segment%d.ts\n",
				segment.Sequence,
			),
		)
	}

	playlist.WriteString(
		"#EXT-X-ENDLIST\n",
	)

	w.Header().Set(
		"Content-Type",
		"application/x-mpegurl",
	)

	w.Header().Set(
		"Cache-Control",
		"no-store, no-cache, must-revalidate",
	)

	w.Header().Set(
		"Access-Control-Allow-Origin",
		"*",
	)

	_, _ = io.WriteString(
		w,
		playlist.String(),
	)
}

func vodSegmentHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	name := strings.TrimPrefix(
		r.URL.Path,
		"/vod/segment",
	)

	name = strings.TrimSuffix(
		name,
		".ts",
	)

	sequence, err := strconv.Atoi(name)

	if err != nil {
		http.NotFound(w, r)
		return
	}

	frozenVOD.mu.RLock()

	var data []byte

	if frozenVOD.Ready {
		for _, segment := range frozenVOD.Segments {
			if segment.Sequence == sequence {
				data = append(
					[]byte(nil),
					segment.Data...,
				)

				break
			}
		}
	}

	frozenVOD.mu.RUnlock()

	if len(data) == 0 {
		http.NotFound(w, r)
		return
	}

	w.Header().Set(
		"Content-Type",
		"video/mp2t",
	)

	w.Header().Set(
		"Cache-Control",
		"no-store",
	)

	w.Header().Set(
		"Access-Control-Allow-Origin",
		"*",
	)

	w.Header().Set(
		"Content-Length",
		strconv.Itoa(len(data)),
	)

	_, _ = w.Write(data)
}

func stopHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	sessionMu.Lock()

	session := currentSession
	currentSession = nil

	sessionMu.Unlock()

	if session != nil {
		session.Close()
	}

	writeJSON(
		w,
		http.StatusOK,
		map[string]interface{}{
			"status":  "stopped",
			"version": 31,
		},
	)
}

func rootHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	writeJSON(
		w,
		http.StatusOK,
		map[string]interface{}{
			"name": "NestView Roku Pion Bridge",

			"status": "online",

			"version": 31,

			"media": "H264 -> MPEG-TS -> HLS",

			"v19": "Annex-B mux-input diagnostics",

			"start": "POST /start",

			"statusEndpoint": "/status",

			"livePlaylist": "/live/index.m3u8",

			"freezeVOD": "/vod/freeze",

			"vodPlaylist": "/vod/index.m3u8",

			"vodStatus": "/vod/status",

			"tsDiagnostics": "/debug/ts",

			"tsDownload": "/debug/segment.ts",

			"tsBase64": "/debug/base64?offset=0&length=24576",

			"tsBase64Text": "/debug/segment.txt",

			"h264Diagnostics": "/debug/h264",

			"muxDiagnostics": "/debug/mux",
		},
	)
}

func main() {
	log.Println(
		"NestView TV Pion Roku bridge VERSION 58 starting",
	)

	log.Println(
		"VERSION 58: camera-backend retry and start-boundary diagnostics enabled",
	)

	http.HandleFunc(
		"/",
		rootHandler,
	)

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
		"/debug/ts",
		tsDebugHandler,
	)

	http.HandleFunc(
		"/debug/segment.ts",
		tsDownloadHandler,
	)

	http.HandleFunc(
		"/debug/base64",
		tsBase64Handler,
	)

	http.HandleFunc(
		"/debug/segment.txt",
		tsBase64TextHandler,
	)

	http.HandleFunc(
		"/debug/segment-base64",
		tsBase64Handler,
	)

	http.HandleFunc(
		"/debug/h264",
		h264DebugHandler,
	)

	http.HandleFunc(
		"/debug/mux",
		muxDiagnosticsHandler,
	)
	http.HandleFunc(
		"/debug/readback",
		readbackDebugHandler,
	)

	// VERSION 58: keep V56's proven HTTP redirect path, but redirect Roku
	// to NestView's media playlist instead of Apple's control stream.
	http.HandleFunc(
		"/live/index.m3u8",
		liveMasterPlaylistHandler,
	)

	http.HandleFunc(
		"/live/media.m3u8",
		livePlaylistHandler,
	)

	http.HandleFunc(
		"/live/segment",
		liveSegmentHandler,
	)

	http.HandleFunc(
		"/vod/freeze",
		freezeVODHandler,
	)

	http.HandleFunc(
		"/vod/status",
		vodStatusHandler,
	)

	http.HandleFunc(
		"/vod/index.m3u8",
		vodPlaylistHandler,
	)

	http.HandleFunc(
		"/vod/segment",
		vodSegmentHandler,
	)

	http.HandleFunc(
		"/stop",
		stopHandler,
	)

	address := "0.0.0.0:" + port

	log.Printf(
		"VERSION 58 listening on %s",
		address,
	)

	if err := http.ListenAndServe(
		address,
		nil,
	); err != nil {

		log.Fatal(err)
	}
}
