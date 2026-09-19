package main

import (
	"bytes"
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
	nestBackend = env("NEST_BACKEND", "https://nestview-tv.onrender.com")
	port        = env("PORT", "10000")
)

const (
	videoPID       uint16 = 256
	segmentDuration       = 2 * time.Second
	maxSegments           = 6
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
}

type HLSSegment struct {
	Sequence int
	Duration float64
	Data     []byte
}

type StreamSession struct {
	mu sync.RWMutex

	PC     *webrtc.PeerConnection
	Stats  *MediaStats
	Camera Camera
	Index  int

	Segments     []HLSSegment
	NextSequence int

	currentBuffer *bytes.Buffer
	currentWriter *mpegts.Writer
	segmentStart  time.Time
	segmentActive bool

	sps []byte
	pps []byte

	/*
		Version 11 timestamp state.

		RTP H264 uses a 90 kHz uint32 clock. We convert
		that clock to a continuous int64 timeline starting
		at zero and preserve continuity across wraparound.
	*/
	timestampStarted bool
	lastRTPTimestamp uint32
	normalizedPTS    int64

	ready chan struct{}
	done  chan struct{}

	readyOnce sync.Once
	closeOnce sync.Once
}

var (
	sessionMu      sync.RWMutex
	currentSession *StreamSession
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func getCameras() ([]Camera, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Get(nestBackend + "/api/cameras")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf(
			"camera backend returned %d: %s",
			resp.StatusCode,
			string(body),
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
			cameras = append(cameras, Camera{
				Device: device,
				Name:   label,
			})
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

				if text, ok := item.(string); ok {
					if strings.Contains(text, "v=0") {
						return text
					}
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
			nalu := append(
				[]byte(nil),
				data[naluStart:naluEnd]...,
			)

			nalus = append(nalus, nalu)
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

func inspectAccessUnit(data []byte) (
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

func (s *StreamSession) cacheParametersLocked(accessUnit []byte) {
	_, hasSPS, hasPPS, sps, pps :=
		inspectAccessUnit(accessUnit)

	if hasSPS && len(sps) > 0 {
		if !bytes.Equal(s.sps, sps) {
			s.sps = append([]byte(nil), sps...)

			log.Printf(
				"VERSION 11 cached SPS: %d bytes",
				len(s.sps),
			)
		}
	}

	if hasPPS && len(pps) > 0 {
		if !bytes.Equal(s.pps, pps) {
			s.pps = append([]byte(nil), pps...)

			log.Printf(
				"VERSION 11 cached PPS: %d bytes",
				len(s.pps),
			)
		}
	}
}

/*
	normalizeTimestamp converts the camera's uint32 RTP
	timestamp into a continuous 90 kHz timeline.

	uint32 subtraction intentionally handles normal RTP
	timestamp wraparound.

	The first access unit starts at PTS 0.
*/
func (s *StreamSession) normalizeTimestamp(
	rtpTimestamp uint32,
) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.timestampStarted {
		s.timestampStarted = true
		s.lastRTPTimestamp = rtpTimestamp
		s.normalizedPTS = 0

		log.Printf(
			"VERSION 11 timestamp clock started: RTP=%d PTS=0",
			rtpTimestamp,
		)

		return 0
	}

	delta := uint32(
		rtpTimestamp - s.lastRTPTimestamp,
	)

	/*
		A huge delta normally means an out-of-order or
		discontinuous timestamp rather than real elapsed
		video time. Ignore it instead of corrupting HLS.
	*/
	if delta > 90000*10 {
		log.Printf(
			"VERSION 11 timestamp discontinuity ignored: previous=%d current=%d delta=%d",
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

	return nil
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

	segment := HLSSegment{
		Sequence: s.NextSequence,
		Duration: duration,
		Data:     data,
	}

	s.NextSequence++

	s.Segments = append(
		s.Segments,
		segment,
	)

	if len(s.Segments) > maxSegments {
		s.Segments = append(
			[]HLSSegment(nil),
			s.Segments[len(s.Segments)-maxSegments:]...,
		)
	}

	atomic.AddUint64(
		&s.Stats.HLSSegments,
		1,
	)

	log.Printf(
		"VERSION 11 HLS SEGMENT READY: sequence=%d duration=%.3f size=%d",
		segment.Sequence,
		segment.Duration,
		len(segment.Data),
	)

	s.readyOnce.Do(func() {
		close(s.ready)
	})

	s.currentBuffer = nil
	s.currentWriter = nil
	s.segmentActive = false
}

func (s *StreamSession) keyframeAccessUnitLocked(
	accessUnit []byte,
) []byte {
	hasIDR, hasSPS, hasPPS, _, _ :=
		inspectAccessUnit(accessUnit)

	if !hasIDR {
		return accessUnit
	}

	var out []byte

	if !hasSPS && len(s.sps) > 0 {
		out = append(out, s.sps...)
	}

	if !hasPPS && len(s.pps) > 0 {
		out = append(out, s.pps...)
	}

	out = append(out, accessUnit...)

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
		return fmt.Errorf("stream session closed")
	default:
	}

	s.cacheParametersLocked(accessUnit)

	hasIDR, _, _, _, _ :=
		inspectAccessUnit(accessUnit)

	if !s.segmentActive {
		if !hasIDR {
			return nil
		}

		if err := s.newSegmentLocked(); err != nil {
			return err
		}

		log.Printf(
			"VERSION 11 HLS started on IDR: SPS=%t PPS=%t PTS=%d",
			len(s.sps) > 0,
			len(s.pps) > 0,
			pts,
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
			"VERSION 11 new IDR segment: sequence=%d PTS=%d",
			s.NextSequence,
			pts,
		)
	}

	outputAU :=
		s.keyframeAccessUnitLocked(accessUnit)

	return s.currentWriter.WriteH264(
		videoPID,
		pts,
		pts,
		outputAU,
	)
}

func (s *StreamSession) Close() {
	s.closeOnce.Do(func() {
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
	})
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
				MimeType:  webrtc.MimeTypeH264,
				ClockRate: 90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;" +
					"packetization-mode=1;" +
					"profile-level-id=42e01f",
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
		PC:     pc,
		Stats:  &MediaStats{},
		Camera: camera,
		Index:  cameraIndex,
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}

	pc.OnConnectionStateChange(
		func(state webrtc.PeerConnectionState) {
			log.Printf(
				"VERSION 11 WebRTC state: %s",
				state.String(),
			)

			if state == webrtc.PeerConnectionStateFailed ||
				state == webrtc.PeerConnectionStateClosed {

				log.Println(
					"VERSION 11 WebRTC session ended",
				)
			}
		},
	)

	pc.OnTrack(
		func(
			track *webrtc.TrackRemote,
			receiver *webrtc.RTPReceiver,
		) {
			codec := track.Codec()

			log.Printf(
				"VERSION 11 incoming track: kind=%s codec=%s payload=%d",
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
								"VERSION 11 video RTP ended:",
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
							accessUnitTimestamp =
								packet.Timestamp

							haveTimestamp = true
						}

						h264Data, err :=
							depacketizer.Unmarshal(
								packet.Payload,
							)

						if err != nil {
							log.Printf(
								"VERSION 11 H264 depacketize error: %v",
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

							units :=
								atomic.AddUint64(
									&session.Stats.AccessUnits,
									1,
								)

							pts :=
								session.normalizeTimestamp(
									accessUnitTimestamp,
								)

							err =
								session.writeAccessUnit(
									accessUnit,
									pts,
								)

							if err != nil {
								log.Printf(
									"VERSION 11 MPEGTS write error: %v",
									err,
								)
							}

							if units == 1 ||
								units%30 == 0 {

								log.Printf(
									"VERSION 11 H264 AU: units=%d packets=%d size=%d PTS=%d",
									units,
									packets,
									len(accessUnit),
									pts,
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
						packet, _, err :=
							track.ReadRTP()

						if err != nil {
							log.Println(
								"VERSION 11 audio RTP ended:",
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
			Direction:
				webrtc.RTPTransceiverDirectionRecvonly,
		},
	)
	if err != nil {
		session.Close()
		return nil, err
	}

	_, err = pc.AddTransceiverFromKind(
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
		return nil, fmt.Errorf("local SDP missing")
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
		"VERSION 11 SDP confirmed: audio -> video -> application",
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
		"VERSION 11 Nest backend HTTP status: %d",
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

	err = pc.SetRemoteDescription(
		webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  answer,
		},
	)

	if err != nil {
		session.Close()
		return nil, err
	}

	log.Println(
		"VERSION 11 Nest WebRTC session started",
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

	if session != nil {
		segments =
			atomic.LoadUint64(
				&session.Stats.HLSSegments,
			)

		streaming = segments > 0
	}

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status":     "ok",
			"bridge":     "pion-h264-hls",
			"version":    11,
			"streaming":  streaming,
			"segments":   segments,
			"timestamps": "normalized-90khz",
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
		"VERSION 11 Shortcut body: %q",
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
				"error":
					"invalid JSON: " +
						err.Error(),
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

			var parsed int

			_, parseErr := fmt.Sscanf(
				v,
				"%d",
				&parsed,
			)

			if parseErr == nil {
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
				"error":
					"camera field missing or invalid",
			},
		)
		return
	}

	cameras, err := getCameras()

	if err != nil {
		writeJSON(
			w,
			502,
			map[string]string{
				"error": err.Error(),
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

	camera := cameras[cameraIndex]

	log.Printf(
		"VERSION 11 starting camera %d: %s",
		cameraIndex,
		camera.Name,
	)

	session, err := createStreamSession(
		camera,
		cameraIndex,
	)

	if err != nil {
		log.Println(
			"VERSION 11 camera start failed:",
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
			"VERSION 11 HLS READY",
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
				"error":
					"HLS keyframe segment was not ready within 25 seconds",
			},
		)
		return
	}

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status": "streaming",
			"camera": cameraIndex,
			"name":   camera.Name,
			"total":  len(cameras),
			"hls":    "/live/index.m3u8",
			"version": 11,

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
				"version":   11,
			},
		)
		return
	}

	session.mu.RLock()

	hasSPS := len(session.sps) > 0
	hasPPS := len(session.pps) > 0
	pts := session.normalizedPTS

	session.mu.RUnlock()

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"streaming": true,
			"version":   11,
			"camera":    session.Index,
			"name":      session.Camera.Name,

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

			"sps":           hasSPS,
			"pps":           hasPPS,
			"normalizedPTS": pts,
		},
	)
}

func playlistHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
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

	for _, segment := range session.Segments {
		duration :=
			int(math.Ceil(segment.Duration))

		if duration > targetDuration {
			targetDuration = duration
		}
	}

	var playlist strings.Builder

	playlist.WriteString("#EXTM3U\n")
	playlist.WriteString("#EXT-X-VERSION:3\n")

	playlist.WriteString(
		"#EXT-X-TARGETDURATION:" +
			strconv.Itoa(targetDuration) +
			"\n",
	)

	playlist.WriteString(
		"#EXT-X-MEDIA-SEQUENCE:" +
			strconv.Itoa(firstSequence) +
			"\n",
	)

	playlist.WriteString(
		"#EXT-X-INDEPENDENT-SEGMENTS\n",
	)

	for _, segment := range session.Segments {
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

	_, _ = w.Write(
		[]byte(playlist.String()),
	)
}

func segmentHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
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

	sequence, err :=
		strconv.Atoi(name)

	if err != nil {
		http.NotFound(w, r)
		return
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	for _, segment := range session.Segments {
		if segment.Sequence == sequence {
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
					len(segment.Data),
				),
			)

			_, _ = w.Write(
				segment.Data,
			)

			return
		}
	}

	http.NotFound(w, r)
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
		200,
		map[string]interface{}{
			"status":  "stopped",
			"version": 11,
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

					"version":
						11,
				},
			)
		},
	)

	log.Println(
		"NestView TV Pion HLS Bridge VERSION 11 running on port " +
			port,
	)

	log.Fatal(
		http.ListenAndServe(
			":"+port,
			nil,
		),
	)
}
