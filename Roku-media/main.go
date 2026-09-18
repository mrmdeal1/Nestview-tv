package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	resp, err := http.Get(nestBackend + "/api/cameras")
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

func containsIDR(data []byte) bool {
	for i := 0; i+4 < len(data); i++ {
		start := -1

		if i+3 < len(data) &&
			data[i] == 0 &&
			data[i+1] == 0 &&
			data[i+2] == 1 {
			start = i + 3
		}

		if i+4 < len(data) &&
			data[i] == 0 &&
			data[i+1] == 0 &&
			data[i+2] == 0 &&
			data[i+3] == 1 {
			start = i + 4
		}

		if start >= 0 && start < len(data) {
			naluType := data[start] & 0x1F
			if naluType == 5 {
				return true
			}
		}
	}

	return false
}

func (s *StreamSession) newSegmentLocked() error {
	buf := &bytes.Buffer{}

	writer, err := mpegts.NewWriter(
		buf,
		mpegts.WithTrack(videoPID, mpegts.CodecH264),
	)
	if err != nil {
		return err
	}

	s.currentBuffer = buf
	s.currentWriter = writer
	s.segmentStart = time.Now()

	return nil
}

func (s *StreamSession) finishSegmentLocked() {
	if s.currentWriter == nil ||
		s.currentBuffer == nil ||
		s.currentBuffer.Len() == 0 {
		return
	}

	duration := time.Since(s.segmentStart).Seconds()

	if duration <= 0 {
		duration = 2.0
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
	s.Segments = append(s.Segments, segment)

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
		"VERSION 8 HLS SEGMENT READY: sequence=%d duration=%.2f size=%d",
		segment.Sequence,
		segment.Duration,
		len(segment.Data),
	)

	s.readyOnce.Do(func() {
		close(s.ready)
	})
}

func (s *StreamSession) writeAccessUnit(
	accessUnit []byte,
	pts int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.currentWriter == nil {
		if err := s.newSegmentLocked(); err != nil {
			return err
		}
	}

	isIDR := containsIDR(accessUnit)

	if isIDR &&
		s.currentBuffer != nil &&
		s.currentBuffer.Len() > 0 &&
		time.Since(s.segmentStart) >= segmentDuration {

		s.finishSegmentLocked()

		if err := s.newSegmentLocked(); err != nil {
			return err
		}
	}

	return s.currentWriter.WriteH264(
		videoPID,
		pts,
		pts,
		accessUnit,
	)
}

func (s *StreamSession) Close() {
	s.closeOnce.Do(func() {
		close(s.done)

		s.mu.Lock()

		if s.currentWriter != nil {
			_ = s.currentWriter.Close()
		}

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
				"VERSION 8 WebRTC state: %s",
				state.String(),
			)

			if state ==
				webrtc.PeerConnectionStateFailed ||
				state ==
					webrtc.PeerConnectionStateClosed {

				log.Println(
					"VERSION 8 WebRTC session ended",
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
				"VERSION 8 incoming track: kind=%s codec=%s payload=%d",
				track.Kind().String(),
				codec.MimeType,
				codec.PayloadType,
			)

			if track.Kind() ==
				webrtc.RTPCodecTypeVideo {

				go func() {
					var depacketizer codecs.H264Packet
					var accessUnit []byte
					var accessUnitPTS int64
					var havePTS bool

					for {
						packet, _, err := track.ReadRTP()
						if err != nil {
							log.Println(
								"VERSION 8 video RTP ended:",
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

						if !havePTS {
							accessUnitPTS =
								int64(packet.Timestamp)
							havePTS = true
						}

						h264Data, err :=
							depacketizer.Unmarshal(
								packet.Payload,
							)

						if err != nil {
							log.Printf(
								"VERSION 8 H264 depacketize error: %v",
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

							units :=
								atomic.AddUint64(
									&session.Stats.AccessUnits,
									1,
								)

							err = session.writeAccessUnit(
								accessUnit,
								accessUnitPTS,
							)

							if err != nil {
								log.Printf(
									"VERSION 8 MPEGTS write error: %v",
									err,
								)
							}

							if units == 1 ||
								units%30 == 0 {

								log.Printf(
									"VERSION 8 H264 AU: units=%d packets=%d size=%d",
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

			if track.Kind() ==
				webrtc.RTPCodecTypeAudio {

				go func() {
					for {
						packet, _, err := track.ReadRTP()
						if err != nil {
							log.Println(
								"VERSION 8 audio RTP ended:",
								err,
							)
							return
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

	audioPos := strings.Index(local.SDP, "m=audio")
	videoPos := strings.Index(local.SDP, "m=video")
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
		"VERSION 8 SDP confirmed: audio -> video -> application",
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

	resp, err := http.Post(
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
		"VERSION 8 Nest backend HTTP status: %d",
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
		"VERSION 8 Nest WebRTC session started",
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
		map[string]interface{}{
			"status":    "ok",
			"bridge":    "pion-h264-hls",
			"version":   8,
			"streaming": streaming,
			"segments":  segments,
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
		"VERSION 8 Shortcut body: %q",
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
		"VERSION 8 starting camera %d: %s",
		cameraIndex,
		camera.Name,
	)

	session, err := createStreamSession(
		camera,
		cameraIndex,
	)

	if err != nil {
		log.Println(
			"VERSION 8 camera start failed:",
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
			"VERSION 8 HLS READY",
		)

	case <-time.After(20 * time.Second):
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
					"HLS segment was not ready within 20 seconds",
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
				"version":   8,
			},
		)
		return
	}

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"streaming": true,
			"version":   8,
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

	targetDuration := 3

	var playlist strings.Builder

	playlist.WriteString(
		"#EXTM3U\n",
	)
	playlist.WriteString(
		"#EXT-X-VERSION:3\n",
	)
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
		"no-cache",
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

	sequence, err := strconv.Atoi(name)
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
				"no-cache",
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
			"version": 8,
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
						"Pion WebRTC -> MPEG-TS -> HLS",
					"h264":
						true,
					"hls":
						true,
					"version":
						8,
				},
			)
		},
	)

	log.Println(
		"NestView TV Pion HLS Bridge VERSION 8 " +
			"running on port " +
			port,
	)

	log.Fatal(
		http.ListenAndServe(
			":"+port,
			nil,
		),
	)
}
