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

	// VERSION 104: keep V56's proven HTTP redirect path, but redirect Roku
	// to NestView's media playlist instead of Apple's control stream.
	http.HandleFunc(
		"/live/index.m3u8",
		liveMasterPlaylistHandler,
	)
	http.HandleFunc(
		"/apple-segment",
		appleSegmentTraceHandler,
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
		"VERSION 104 listening on %s",
		address,
	)

	if err := http.ListenAndServe(
		address,
		nil,
	); err != nil {

		log.Fatal(err)
	}
}
