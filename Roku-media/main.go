package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

var (
	nestBackend = env("NEST_BACKEND", "https://nestview-tv.onrender.com")
	port        = env("PORT", "10000")
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
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
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

func startCamera(camera Camera) (*MediaStats, error) {
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

	defer pc.Close()

	stats := &MediaStats{}

	videoSeen := make(chan struct{}, 1)
	audioSeen := make(chan struct{}, 1)

	pc.OnConnectionStateChange(
		func(state webrtc.PeerConnectionState) {
			log.Printf(
				"WebRTC connection state: %s",
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
				"Incoming track: kind=%s codec=%s payload=%d",
				track.Kind().String(),
				codec.MimeType,
				codec.PayloadType,
			)

			if track.Kind() == webrtc.RTPCodecTypeVideo {
				select {
				case videoSeen <- struct{}{}:
				default:
				}

				go func() {
					var depacketizer codecs.H264Packet
					var accessUnit []byte

					for {
						packet, _, err := track.ReadRTP()
						if err != nil {
							log.Println(
								"Video RTP ended:",
								err,
							)
							return
						}

						packets := atomic.AddUint64(
							&stats.VideoPackets,
							1,
						)

						bytesReceived := atomic.AddUint64(
							&stats.VideoBytes,
							uint64(len(packet.Payload)),
						)

						h264Data, err := depacketizer.Unmarshal(
							packet.Payload,
						)

						if err != nil {
							log.Printf(
								"H264 depacketize error: %v",
								err,
							)
							continue
						}

						if len(h264Data) > 0 {
							accessUnit = append(
								accessUnit,
								h264Data...,
							)
						}

						if packet.Marker && len(accessUnit) > 0 {
							units := atomic.AddUint64(
								&stats.AccessUnits,
								1,
							)

							if units == 1 || units%30 == 0 {
								log.Printf(
									"H264 ACCESS UNIT COMPLETE: units=%d size=%d RTPpackets=%d bytes=%d",
									units,
									len(accessUnit),
									packets,
									bytesReceived,
								)
							}

							accessUnit = nil
						}

						if packets == 1 || packets%100 == 0 {
							log.Printf(
								"H264 RTP CONTINUOUS: packets=%d bytes=%d accessUnits=%d",
								packets,
								bytesReceived,
								atomic.LoadUint64(
									&stats.AccessUnits,
								),
							)
						}
					}
				}()
			}

			if track.Kind() == webrtc.RTPCodecTypeAudio {
				select {
				case audioSeen <- struct{}{}:
				default:
				}

				go func() {
					for {
						packet, _, err := track.ReadRTP()
						if err != nil {
							log.Println(
								"Audio RTP ended:",
								err,
							)
							return
						}

						atomic.AddUint64(
							&stats.AudioPackets,
							1,
						)

						atomic.AddUint64(
							&stats.AudioBytes,
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
		return nil, err
	}

	_, err = pc.CreateDataChannel(
		"nestview",
		nil,
	)
	if err != nil {
		return nil, err
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, err
	}

	if err = pc.SetLocalDescription(offer); err != nil {
		return nil, err
	}

	waitForICE(pc)

	local := pc.LocalDescription()
	if local == nil {
		return nil, fmt.Errorf("local SDP missing")
	}

	upperSDP := strings.ToUpper(local.SDP)

	if !strings.Contains(upperSDP, "OPUS/48000") {
		return nil, fmt.Errorf(
			"generated SDP does not contain OPUS/48000",
		)
	}

	if !strings.Contains(upperSDP, "H264/90000") {
		return nil, fmt.Errorf(
			"generated SDP does not contain H264/90000",
		)
	}

	audioPos := strings.Index(local.SDP, "m=audio")
	videoPos := strings.Index(local.SDP, "m=video")
	appPos := strings.Index(local.SDP, "m=application")

	if audioPos == -1 ||
		videoPos == -1 ||
		appPos == -1 {

		return nil, fmt.Errorf(
			"SDP missing audio, video, or application m-line",
		)
	}

	if !(audioPos < videoPos && videoPos < appPos) {
		return nil, fmt.Errorf(
			"SDP order is not audio-video-application",
		)
	}

	log.Println(
		"OPUS/48000 confirmed in Pion offer",
	)

	log.Println(
		"H264/90000 confirmed in Pion offer",
	)

	log.Println(
		"SDP order confirmed: audio -> video -> application",
	)

	payload := map[string]string{
		"device":   camera.Device,
		"offerSdp": local.SDP,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(
		nestBackend+"/api/webrtc",
		"application/json",
		bytes.NewReader(data),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	log.Printf(
		"Nest backend HTTP status: %d",
		resp.StatusCode,
	)

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		return nil, fmt.Errorf(
			"Nest backend returned %d: %s",
			resp.StatusCode,
			string(body),
		)
	}

	var result interface{}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf(
			"could not decode Nest response: %w",
			err,
		)
	}

	answer := findAnswerSDP(result)

	if answer == "" {
		return nil, fmt.Errorf(
			"Nest response did not contain recognizable answer SDP",
		)
	}

	log.Println(
		"Nest answer SDP found",
	)

	err = pc.SetRemoteDescription(
		webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  answer,
		},
	)
	if err != nil {
		return nil, err
	}

	log.Println(
		"Nest accepted Pion H264 offer and remote SDP",
	)

	log.Println(
		"VERSION 7: waiting for continuous H264 access units...",
	)

	deadline := time.NewTimer(
		20 * time.Second,
	)
	defer deadline.Stop()

	ticker := time.NewTicker(
		1 * time.Second,
	)
	defer ticker.Stop()

	confirmed := false

	for {
		select {
		case <-videoSeen:
			log.Println(
				"VIDEO TRACK RECEIVED FROM NEST",
			)

		case <-audioSeen:
			log.Println(
				"AUDIO TRACK RECEIVED FROM NEST",
			)

		case <-ticker.C:
			videoPackets := atomic.LoadUint64(
				&stats.VideoPackets,
			)

			videoBytes := atomic.LoadUint64(
				&stats.VideoBytes,
			)

			audioPackets := atomic.LoadUint64(
				&stats.AudioPackets,
			)

			accessUnits := atomic.LoadUint64(
				&stats.AccessUnits,
			)

			if accessUnits > 0 {
				if !confirmed {
					log.Println(
						"VERSION 7 H264 DEPACKETIZATION CONFIRMED",
					)
					confirmed = true
				}

				log.Printf(
					"VERSION 7 CONTINUOUS H264: packets=%d bytes=%d accessUnits=%d audioPackets=%d",
					videoPackets,
					videoBytes,
					accessUnits,
					audioPackets,
				)
			}

		case <-deadline.C:
			accessUnits := atomic.LoadUint64(
				&stats.AccessUnits,
			)

			if accessUnits == 0 {
				return nil, fmt.Errorf(
					"Nest connected but no complete H264 access units received within 20 seconds",
				)
			}

			log.Printf(
				"VERSION 7 TEST COMPLETE: accessUnits=%d",
				accessUnits,
			)

			return stats, nil
		}
	}
}

func health(
	w http.ResponseWriter,
	r *http.Request,
) {
	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status":  "ok",
			"bridge":  "pion-h264-depacketizer",
			"version": 7,
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
		"Shortcut body: %q",
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
		"Starting camera %d: %s",
		cameraIndex,
		camera.Name,
	)

	stats, err := startCamera(camera)

	if err != nil {
		log.Println(
			"Camera start failed:",
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

	videoPackets := atomic.LoadUint64(
		&stats.VideoPackets,
	)

	videoBytes := atomic.LoadUint64(
		&stats.VideoBytes,
	)

	audioPackets := atomic.LoadUint64(
		&stats.AudioPackets,
	)

	audioBytes := atomic.LoadUint64(
		&stats.AudioBytes,
	)

	accessUnits := atomic.LoadUint64(
		&stats.AccessUnits,
	)

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status":
				"h264_depacketized",
			"camera":
				cameraIndex,
			"name":
				camera.Name,
			"total":
				len(cameras),
			"videoPackets":
				videoPackets,
			"videoBytes":
				videoBytes,
			"accessUnits":
				accessUnits,
			"audioPackets":
				audioPackets,
			"audioBytes":
				audioBytes,
			"h264":
				videoPackets > 0,
			"depacketized":
				accessUnits > 0,
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
						"Pion WebRTC",
					"h264":
						true,
					"depacketizer":
						true,
					"version":
						7,
				},
			)
		},
	)

	log.Println(
		"NestView TV Pion H264 Bridge VERSION 7 " +
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
