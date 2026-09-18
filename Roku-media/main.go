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
		for _, key := range []string{"cameras", "devices", "results"} {
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

func firstString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
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
			nalu := append([]byte(nil), data[naluStart:naluEnd]...)
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
	_, hasSPS, hasPPS, sps, pps := inspectAccessUnit(accessUnit)

	if hasSPS && len(sps) > 0 {
		if !bytes.Equal(s.sps, sps) {
			s.sps = append([]byte(nil), sps...)
			log.Printf(
				"VERSION 10 cached SPS: %d bytes",
				len(s.sps),
			)
		}
	}

	if hasPPS && len(pps) > 0 {
		if !bytes.Equal(s.pps, pps) {
			s.pps = append([]byte(nil), pps...)
			log.Printf(
				"VERSION 10 cached PPS: %d bytes",
				len(s.pps),
			)
		}
	}
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
		"VERSION 10 HLS SEGMENT READY: sequence=%d duration=%.3f size=%d",
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

func (s *StreamSession) keyframeAccessUnitLocked(accessUnit []byte) []byte {
	hasIDR, hasSPS, hasPPS, _, _ := inspectAccessUnit(accessUnit)

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

	hasIDR, _, _, _, _ := inspectAccessUnit(accessUnit)

	/*
		Version 10:
		The first HLS segment is not started until an IDR
		keyframe arrives. Every following segment is also
		cut immediately before an IDR.
	*/
	if !s.segmentActive {
		if !hasIDR {
			return nil
		}

		if err := s.newSegmentLocked(); err != nil {
			return err
		}

		log.Printf(
			"VERSION 10 HLS started on IDR: SPS=%t PPS=%t",
			len(s.sps) > 0,
			len(s.pps) > 0,
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
			"VERSION 10 new IDR segment: sequence=%d",
			s.NextSequence,
		)
	}

	outputAU := s.keyframeAccessUnitLocked(accessUnit)

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
				MimeType: webrtc.MimeTypeH264,
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
				"VERSION 10 WebRTC state: %s",
				state.String(),
			)

			if state == webrtc.PeerConnectionStateFailed ||
				state == webrtc.PeerConnectionStateClosed {
				log.Println(
					"VERSION 10 WebRTC session ended",
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
				"VERSION 10 incoming track: kind=%s codec=%s payload=%d",
				track.Kind().String(),
				codec.MimeType,
				codec.PayloadType,
			)

			if track.Kind() == webrtc.RTPCodecTypeVideo {
				go func() {
					var depacketizer codecs.H264Packet
					var accessUnit []byte
					var accessUnitPTS int64
					var havePTS bool

					for {
						packet, _, err := track.ReadRTP()

						if err != nil {
							log.Println(
								"VERSION 10 video RTP ended:",
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

						/*
							Ignore empty RTP payloads.
							Version 9 was sending these to
							the H264 depacketizer, causing
							the repeated zero-byte errors.
						*/
						if len(packet.Payload) == 0 {
							atomic.AddUint64(
								&session.Stats.EmptyPackets,
								1,
							)
							continue
						}

						if !havePTS {
							accessUnitPTS = int64(
								packet.Timestamp,
							)
							havePTS = true
						}

						h264Data, err := depacketizer.Unmarshal(
							packet.Payload,
						)

						if err != nil {
							log.Printf(
								"VERSION 10 H264 depacketize error: %v",
								err,
							)

							accessUnit = nil
							havePTS = false
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

							err = session.writeAccessUnit(
								accessUnit,
								accessUnitPTS,
							)

							if err != nil {
								log.Printf(
									"VERSION 10 MPEGTS write error: %v",
									err,
								)
							}

							if units == 1 ||
								units%30 == 0 {

								log.Printf(
									"VERSION 10 H264 AU: units=%d packets=%d size=%d",
									units,
									packets,
									len(accessUnit),
								)
							}

							accessUnit = nil
							havePTS = false
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
								"VERSION 10 audio RTP ended:",
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
		return nil, fmt.Errorf("local SDP missing")
	}

	upperSDP := strings.ToUpper(local.SDP)

	if !strings.Contains(upperSDP, "OPUS/48000") {
		session.Close()

		return nil, fmt.Errorf(
			"generated SDP does not contain OPUS/48000",
		)
	}

	if !strings.Contains(upperSDP, "H264/90000") {
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
		"VERSION 10 SDP confirmed: audio -> video -> application",
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
		"VERSION 10 Nest backend HTTP status: %d",
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
		"VERSION 10 Nest WebRTC session started",
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
		segments = atomic.LoadUint64(
			&session.Stats.HLSSegments,
		)

		streaming = segments > 0
	}

	writeJSON(
		w,
		200,
		map
