const http = require("http");
const https = require("https");
const { spawn } = require("child_process");
const fs = require("fs");
const path = require("path");
const wrtc = require("@roamhq/wrtc");

const PORT = process.env.PORT || 10000;

const NEST_BACKEND =
  process.env.NEST_BACKEND ||
  "https://nestview-tv.onrender.com";

const STREAM_DIR = "/tmp/nestview-hls";

let session = null;

fs.mkdirSync(STREAM_DIR, { recursive: true });

function jsonRequest(url, method = "GET", body = null) {
  return new Promise((resolve, reject) => {
    const target = new URL(url);
    const data = body ? JSON.stringify(body) : null;

    const client =
      target.protocol === "http:" ? http : https;

    const req = client.request(
      {
        hostname: target.hostname,
        port:
          target.port ||
          (target.protocol === "http:" ? 80 : 443),
        path: target.pathname + target.search,
        method,
        headers: data
          ? {
              "Content-Type": "application/json",
              "Content-Length": Buffer.byteLength(data),
            }
          : {},
      },
      (res) => {
        let raw = "";

        res.on("data", (chunk) => {
          raw += chunk;
        });

        res.on("end", () => {
          let parsed = {};

          try {
            parsed = raw ? JSON.parse(raw) : {};
          } catch (_) {
            parsed = { raw };
          }

          if (
            res.statusCode < 200 ||
            res.statusCode >= 300
          ) {
            reject(
              new Error(
                "Backend returned " +
                  res.statusCode +
                  ": " +
                  raw
              )
            );
            return;
          }

          resolve(parsed);
        });
      }
    );

    req.on("error", reject);

    if (data) {
      req.write(data);
    }

    req.end();
  });
}

function sendJson(res, status, data) {
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
  });

  res.end(JSON.stringify(data));
}

function stopSession() {
  if (!session) return;

  console.log("Stopping current camera session");

  try {
    if (session.sink) {
      session.sink.stop();
    }
  } catch (_) {}

  try {
    if (session.ffmpeg) {
      session.ffmpeg.kill("SIGKILL");
    }
  } catch (_) {}

  try {
    if (session.pc) {
      session.pc.close();
    }
  } catch (_) {}

  session = null;
}

function clearHls() {
  try {
    const files = fs.readdirSync(STREAM_DIR);

    for (const file of files) {
      fs.unlinkSync(path.join(STREAM_DIR, file));
    }
  } catch (_) {}
}

function waitForIce(pc) {
  return new Promise((resolve) => {
    if (pc.iceGatheringState === "complete") {
      resolve();
      return;
    }

    const timeout = setTimeout(resolve, 5000);

    pc.onicegatheringstatechange = () => {
      if (pc.iceGatheringState === "complete") {
        clearTimeout(timeout);
        resolve();
      }
    };
  });
}

function normalizeCameraList(response) {
  if (Array.isArray(response)) {
    return response;
  }

  if (
    response &&
    Array.isArray(response.cameras)
  ) {
    return response.cameras;
  }

  if (
    response &&
    Array.isArray(response.devices)
  ) {
    return response.devices;
  }

  if (
    response &&
    Array.isArray(response.results)
  ) {
    return response.results;
  }

  return [];
}

function getCameraDevice(camera) {
  if (!camera || typeof camera !== "object") {
    return null;
  }

  return (
    camera.device ||
    camera.deviceId ||
    camera.resourceName ||
    camera.id ||
    null
  );
}

function getCameraLabel(camera, index) {
  if (!camera || typeof camera !== "object") {
    return "Camera " + (index + 1);
  }

  return (
    camera.displayName ||
    camera.label ||
    camera.customName ||
    camera.roomName ||
    camera.name ||
    "Camera " + (index + 1)
  );
}

async function resolveCamera(cameraIndex) {
  console.log(
    "Fetching cameras from " +
      NEST_BACKEND +
      "/api/cameras"
  );

  let response;

  try {
    response = await jsonRequest(
      NEST_BACKEND + "/api/cameras"
    );
  } catch (err) {
    const error = new Error(
      "Could not fetch camera list: " +
        err.message
    );

    error.statusCode = 502;
    throw error;
  }

  const cameras = normalizeCameraList(response);

  if (!cameras.length) {
    const error = new Error(
      "Nest backend returned no cameras"
    );

    error.statusCode = 502;
    throw error;
  }

  const index = Number(cameraIndex);

  if (
    !Number.isInteger(index) ||
    index < 0 ||
    index >= cameras.length
  ) {
    const error = new Error(
      "Invalid camera index. Use 0-" +
        (cameras.length - 1)
    );

    error.statusCode = 400;
    throw error;
  }

  const selected = cameras[index];
  const device = getCameraDevice(selected);

  if (!device) {
    const error = new Error(
      "Selected camera has no device identifier"
    );

    error.statusCode = 502;
    throw error;
  }

  const label = getCameraLabel(
    selected,
    index
  );

  console.log(
    "Camera selected:",
    index,
    label
  );

  return {
    index,
    device,
    label,
    total: cameras.length,
  };
}

async function startCamera(cameraInfo) {
  stopSession();
  clearHls();

  console.log(
    "Starting camera:",
    cameraInfo.index,
    cameraInfo.label
  );

  console.log(
    "Device:",
    cameraInfo.device
  );

  const pc = new wrtc.RTCPeerConnection({
    sdpSemantics: "unified-plan",
  });

  pc.addTransceiver("audio", {
    direction: "recvonly",
  });

  const videoTransceiver = pc.addTransceiver("video", {

  direction: "recvonly",

});

const videoCapabilities =

  wrtc.RTCRtpSender.getCapabilities("video");

if (

  videoCapabilities &&

  Array.isArray(videoCapabilities.codecs)

) {

  const h264Codecs =

    videoCapabilities.codecs.filter(

      (codec) =>

        codec.mimeType &&

        codec.mimeType.toLowerCase() ===

          "video/h264"

    );

  if (h264Codecs.length > 0) {

    console.log(

      "H264 codecs available:",

      h264Codecs.length

    );

    if (

      typeof videoTransceiver.setCodecPreferences ===

      "function"

    ) {

      videoTransceiver.setCodecPreferences(

        h264Codecs

      );

      console.log(

        "H264 selected for Nest WebRTC offer"

      );

    }

  } else {

    console.log(

      "WARNING: H264 codec not available in wrtc"

    );

  }

}

  pc.createDataChannel("nest");

  let videoTrack = null;

  const trackPromise = new Promise(
    (resolve) => {
      pc.ontrack = (event) => {
        console.log(
          "Received track:",
          event.track.kind
        );

        if (
          event.track.kind === "video" &&
          !videoTrack
        ) {
          videoTrack = event.track;
          resolve(event.track);
        }
      };
    }
  );

  const offer = await pc.createOffer();

  await pc.setLocalDescription(offer);

  await waitForIce(pc);

  let offerSdp = pc.localDescription.sdp;

  if (!offerSdp.endsWith("\n")) {
    offerSdp += "\r\n";
  }

  console.log(
    "Sending WebRTC offer to Nest backend"
  );

  const response = await jsonRequest(
    NEST_BACKEND + "/api/webrtc",
    "POST",
    {
      device: cameraInfo.device,
      offerSdp,
    }
  );

  const result =
    response.results || response;

  if (!result.answerSdp) {
    throw new Error(
      "Nest backend did not return answerSdp"
    );
  }

  console.log(
    "Received WebRTC answer from Nest backend"
  );

  await pc.setRemoteDescription({
    type: "answer",
    sdp: result.answerSdp,
  });

  const track = await Promise.race([
    trackPromise,

    new Promise((_, reject) =>
      setTimeout(
        () =>
          reject(
            new Error(
              "Timed out waiting for Nest video"
            )
          ),
        15000
      )
    ),
  ]);

  console.log("Nest video track connected");

  const sink =
    new wrtc.nonstandard.RTCVideoSink(track);

  session = {
    cameraIndex: cameraInfo.index,
    cameraLabel: cameraInfo.label,
    device: cameraInfo.device,
    totalCameras: cameraInfo.total,
    pc,
    sink,
    ffmpeg: null,
    startedAt: Date.now(),
    expiresAt: result.expiresAt || null,
  };

  sink.onframe = ({ frame }) => {
    if (!session) {
      return;
    }

    if (!session.ffmpeg) {
      const width = frame.width;
      const height = frame.height;

      console.log(
        "Nest video frame:",
        width + "x" + height
      );

      const ffmpeg = spawn(
        "ffmpeg",
        [
          "-hide_banner",
          "-loglevel",
          "warning",

          "-f",
          "rawvideo",

          "-pix_fmt",
          "yuv420p",

          "-s",
          width + "x" + height,

          "-r",
          "15",

          "-i",
          "pipe:0",

          "-an",

          "-c:v",
          "libx264",

          "-preset",
          "veryfast",

          "-tune",
          "zerolatency",

          "-profile:v",
          "main",

          "-pix_fmt",
          "yuv420p",

          "-g",
          "30",

          "-keyint_min",
          "30",

          "-f",
          "hls",

          "-hls_time",
          "2",

          "-hls_list_size",
          "4",

          "-hls_flags",
          "delete_segments+append_list",

          path.join(
            STREAM_DIR,
            "index.m3u8"
          ),
        ],
        {
          stdio: [
            "pipe",
            "inherit",
            "inherit",
          ],
        }
      );

      ffmpeg.on("error", (err) => {
        console.error(
          "FFmpeg error:",
          err.message
        );
      });

      ffmpeg.on("exit", (code) => {
        console.log(
          "FFmpeg exited:",
          code
        );
      });

      session.ffmpeg = ffmpeg;

      console.log("FFmpeg started");
    }

    const ffmpeg = session.ffmpeg;

    if (
      ffmpeg &&
      ffmpeg.stdin &&
      !ffmpeg.stdin.destroyed
    ) {
      try {
        ffmpeg.stdin.write(frame.data);
      } catch (_) {}
    }
  };

  return {
    camera: cameraInfo.index,
    name: cameraInfo.label,
    total: cameraInfo.total,
    expiresAt: result.expiresAt || null,
  };
}

function serveHls(req, res) {
  const filename =
    req.url === "/live/index.m3u8"
      ? "index.m3u8"
      : path.basename(req.url);

  const filePath = path.join(
    STREAM_DIR,
    filename
  );

  if (!fs.existsSync(filePath)) {
    res.writeHead(404, {
      "Content-Type": "text/plain",
      "Cache-Control": "no-store",
      "Access-Control-Allow-Origin": "*",
    });

    res.end("Stream not ready");
    return;
  }

  const ext = path.extname(filePath);

  const type =
    ext === ".m3u8"
      ? "application/vnd.apple.mpegurl"
      : "video/mp2t";

  res.writeHead(200, {
    "Content-Type": type,
    "Cache-Control": "no-store",
    "Access-Control-Allow-Origin": "*",
  });

  fs.createReadStream(filePath).pipe(res);
}

const server = http.createServer(
  async (req, res) => {
    try {
      if (
        req.method === "OPTIONS"
      ) {
        res.writeHead(204, {
          "Access-Control-Allow-Origin": "*",
          "Access-Control-Allow-Methods":
            "GET,POST,OPTIONS",
          "Access-Control-Allow-Headers":
            "Content-Type",
        });

        res.end();
        return;
      }

      if (
        req.method === "GET" &&
        req.url === "/"
      ) {
        sendJson(res, 200, {
          app: "NestView TV Media Bridge",
          version: "2",
          status: "online",
          streaming: !!session,
        });

        return;
      }

      if (
        req.method === "GET" &&
        req.url === "/health"
      ) {
        sendJson(res, 200, {
          status: "ok",

          version: "2",

          streaming: !!session,

          ffmpeg: session
            ? !!session.ffmpeg
            : false,

          camera: session
            ? session.cameraIndex
            : null,

          name: session
            ? session.cameraLabel
            : null,

          total: session
            ? session.totalCameras
            : null,

          startedAt: session
            ? session.startedAt
            : null,

          expiresAt: session
            ? session.expiresAt
            : null,
        });

        return;
      }

      if (
        req.method === "POST" &&
        req.url === "/start"
      ) {
        let raw = "";

        req.on("data", (chunk) => {
          raw += chunk;
        });

        req.on("end", async () => {
          try {
            const body = JSON.parse(
              raw || "{}"
            );

            let cameraIndex = null;

            if (
              body.camera !== undefined &&
              body.camera !== null
            ) {
              cameraIndex = body.camera;
            } else if (
              body.cameraIndex !==
                undefined &&
              body.cameraIndex !== null
            ) {
              cameraIndex =
                body.cameraIndex;
            } else if (
              body.index !== undefined &&
              body.index !== null
            ) {
              cameraIndex = body.index;
            }

            if (cameraIndex === null) {
              sendJson(res, 400, {
                error:
                  "camera required. Use camera 0-6.",
              });

              return;
            }

            let cameraInfo;

            try {
              cameraInfo =
                await resolveCamera(
                  cameraIndex
                );
            } catch (err) {
              console.error(
                "Camera selection failed:",
                err.message
              );

              sendJson(
                res,
                err.statusCode || 502,
                {
                  error: err.message,
                }
              );

              return;
            }

            try {
              const result =
                await startCamera(
                  cameraInfo
                );

              sendJson(res, 200, {
                ok: true,
                ...result,
                hls: "/live/index.m3u8",
              });
            } catch (err) {
              console.error(
                "Camera start failed:",
                err.message
              );

              stopSession();

              sendJson(res, 502, {
                error: err.message,
              });
            }
          } catch (err) {
            console.error(
              "Invalid start request:",
              err.message
            );

            sendJson(res, 400, {
              error: "Invalid JSON request",
            });
          }
        });

        return;
      }

      if (
        req.method === "POST" &&
        req.url === "/stop"
      ) {
        stopSession();
        clearHls();

        sendJson(res, 200, {
          ok: true,
        });

        return;
      }

      if (
        req.method === "GET" &&
        req.url.startsWith("/live/")
      ) {
        serveHls(req, res);
        return;
      }

      sendJson(res, 404, {
        error: "Not found",
      });
    } catch (err) {
      console.error(
        "Server error:",
        err.message
      );

      sendJson(res, 500, {
        error: err.message,
      });
    }
  }
);

server.listen(
  PORT,
  "0.0.0.0",
  () => {
    console.log(
      "NestView TV Media Bridge VERSION 2 running on port " +
        PORT
    );
  }
);
