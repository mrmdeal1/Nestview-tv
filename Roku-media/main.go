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
	"time"

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

func waitForICE(pc *webrtc.PeerConnection) {
	done := webrtc.GatheringCompletePromise(pc)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

func startCamera(camera Camera) (string, error) {
	mediaEngine := &webrtc.MediaEngine{}

	// OPUS audio
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
		return "", err
	}

	// H264 video
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
		return "", err
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
	)

	pc, err := api.NewPeerConnection(
		webrtc.Configuration{},
	)
	if err != nil {
		return "", err
	}
	defer pc.Close()

	// Nest requires:
	// audio -> video -> application

	_, err = pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	)
	if err != nil {
		return "", err
	}

	_, err = pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	)
	if err != nil {
		return "", err
	}

	// Creates application m-line.
	_, err = pc.CreateDataChannel(
		"nestview",
		nil,
	)
	if err != nil {
		return "", err
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return "", err
	}

	if err = pc.SetLocalDescription(offer); err != nil {
		return "", err
	}

	waitForICE(pc)

	local := pc.LocalDescription()
	if local == nil {
		return "", fmt.Errorf("local SDP missing")
	}

	upperSDP := strings.ToUpper(local.SDP)

	if !strings.Contains(
		upperSDP,
		"OPUS/48000",
	) {
		return "", fmt.Errorf(
			"generated SDP does not contain OPUS/48000",
		)
	}

	if !strings.Contains(
		upperSDP,
		"H264/90000",
	) {
		return "", fmt.Errorf(
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
		return "", fmt.Errorf(
			"SDP missing audio, video, or application m-line",
		)
	}

	if !(audioPos < videoPos && videoPos < appPos) {
		return "", fmt.Errorf(
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
		return "", err
	}

	resp, err := http.Post(
		nestBackend+"/api/webrtc",
		"application/json",
		bytes.NewReader(data),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {
		return "", fmt.Errorf(
			"Nest backend returned %d: %s",
			resp.StatusCode,
			string(body),
		)
	}

	var result map[string]interface{}

	if err := json.Unmarshal(
		body,
		&result,
	); err != nil {
		return "", err
	}

	answer := firstString(
		result,
		"answerSdp",
		"answer",
		"sdp",
	)

	if answer == "" {
		return "", fmt.Errorf(
			"Nest response did not contain answer SDP",
		)
	}

	err = pc.SetRemoteDescription(
		webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  answer,
		},
	)
	if err != nil {
		return "", err
	}

	log.Println(
		"Nest accepted Pion H264 offer",
	)

	return answer, nil
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
			"bridge":  "pion-h264",
			"version": 4,
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

	// Decode into a generic map so we can normalize
	// accidental whitespace in Shortcut field names.
	var rawRequest map[string]interface{}

	if err := json.Unmarshal(
		body,
		&rawRequest,
	); err != nil {
		writeJSON(
			w,
			400,
			map[string]string{
				"error": "invalid JSON: " +
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

			_, err := fmt.Sscanf(
				v,
				"%d",
				&parsed,
			)

			if err == nil {
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
				"error": "invalid camera index",
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

	_, err = startCamera(camera)

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

	writeJSON(
		w,
		200,
		map[string]interface{}{
			"status": "connected",
			"camera": cameraIndex,
			"name":   camera.Name,
			"total":  len(cameras),
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
					"name": "NestView TV Roku Media Bridge",
					"engine": "Pion WebRTC",
					"h264": true,
					"opus": true,
					"version": 4,
				},
			)
		},
	)

	log.Println(
		"NestView TV Pion H264 Bridge VERSION 4 " +
			"running on port " + port,
	)

	log.Fatal(
		http.ListenAndServe(
			":"+port,
			nil,
		),
	)
}
