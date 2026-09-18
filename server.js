const http = require("http");

const https = require("https");

const PORT = process.env.PORT || 10000;

const CLIENT_ID = process.env.GOOGLE_CLIENT_ID;

const CLIENT_SECRET = process.env.GOOGLE_CLIENT_SECRET;

const PROJECT_ID = process.env.DEVICE_ACCESS_PROJECT_ID;

const REDIRECT_URI =

  "https://nestview-tv.onrender.com/oauth/callback";

let accessToken = null;

let accessTokenExpiresAt = 0;

let refreshToken =

  process.env.GOOGLE_REFRESH_TOKEN || null;

function sendJson(res, status, data) {

  res.writeHead(status, {

    "Content-Type": "application/json",

    "Cache-Control": "no-store"

  });

  res.end(JSON.stringify(data));

}

function googleRequest(options, body = null) {

  return new Promise((resolve, reject) => {

    const request = https.request(options, response => {

      let data = "";

      response.on("data", chunk => {

        data += chunk;

      });

      response.on("end", () => {

        resolve({

          status: response.statusCode,

          data

        });

      });

    });

    request.on("error", reject);

    if (body) {

      request.write(body);

    }

    request.end();

  });

}
async function refreshAccessToken() {

  if (!refreshToken) {

    return null;

  }

  const body = new URLSearchParams({

    client_id: CLIENT_ID,

    client_secret: CLIENT_SECRET,

    refresh_token: refreshToken,

    grant_type: "refresh_token"

  }).toString();

  const result = await googleRequest({

    hostname: "oauth2.googleapis.com",

    path: "/token",

    method: "POST",

    headers: {

      "Content-Type": "application/x-www-form-urlencoded",

      "Content-Length": Buffer.byteLength(body)

    }

  }, body);

  if (result.status !== 200) {

    console.error("TOKEN_REFRESH_FAILED", result.status);

    return null;

  }

  const data = JSON.parse(result.data);

  accessToken = data.access_token;

  accessTokenExpiresAt =

    Date.now() + Number(data.expires_in || 3600) * 1000;

  return accessToken;

}

async function getAccessToken() {

  if (

    accessToken &&

    Date.now() < accessTokenExpiresAt - 60000

  ) {

    return accessToken;

  }

  return refreshAccessToken();

}
async function nestGet(path) {

  const token = await getAccessToken();

  if (!token) {

    return {

      status: 401,

      data: JSON.stringify({

        error: "Nest authorization required"

      })

    };

  }

  return googleRequest({

    hostname: "smartdevicemanagement.googleapis.com",

    path: path,

    method: "GET",

    headers: {

      Authorization: "Bearer " + token

    }

  });

}

async function nestPost(path, data) {

  const token = await getAccessToken();

  if (!token) {

    return {

      status: 401,

      data: JSON.stringify({

        error: "Nest authorization required"

      })

    };

  }

  const body = JSON.stringify(data);

  return googleRequest({

    hostname: "smartdevicemanagement.googleapis.com",

    path: path,

    method: "POST",

    headers: {

      Authorization: "Bearer " + token,

      "Content-Type": "application/json",

      "Content-Length": Buffer.byteLength(body)

    }

  }, body);

}
const server = http.createServer(async (req, res) => {

  const url = new URL(

    req.url,

    "https://nestview-tv.onrender.com"

  );

  console.log("REQUEST:", req.method, url.pathname);

  if (url.pathname === "/") {

    return sendJson(res, 200, {

      app: "NestView TV",

      edition: "Roku",

      version: "3",

      status: "online"

    });

  }

  if (url.pathname === "/health") {

    return sendJson(res, 200, {

      status: "ok",

      version: "3",

      refreshTokenLoaded: !!refreshToken

    });

  }

  if (url.pathname === "/auth") {

    const authUrl = new URL(

      "https://nestservices.google.com/partnerconnections/" +

      PROJECT_ID +

      "/auth"

    );

    authUrl.searchParams.set("redirect_uri", REDIRECT_URI);

    authUrl.searchParams.set("access_type", "offline");

    authUrl.searchParams.set("prompt", "consent");

    authUrl.searchParams.set("client_id", CLIENT_ID);

    authUrl.searchParams.set("response_type", "code");

    authUrl.searchParams.set(

      "scope",

      "https://www.googleapis.com/auth/sdm.service"

    );

    res.writeHead(302, {

      Location: authUrl.toString()

    });

    return res.end();

  }
    if (url.pathname === "/oauth/callback") {

    const code = url.searchParams.get("code");

    if (!code) {

      return sendJson(res, 400, {

        error: "Missing authorization code"

      });

    }

    try {

      const body = new URLSearchParams({

        client_id: CLIENT_ID,

        client_secret: CLIENT_SECRET,

        code: code,

        grant_type: "authorization_code",

        redirect_uri: REDIRECT_URI

      }).toString();

      const result = await googleRequest({

        hostname: "oauth2.googleapis.com",

        path: "/token",

        method: "POST",

        headers: {

          "Content-Type":

            "application/x-www-form-urlencoded",

          "Content-Length":

            Buffer.byteLength(body)

        }

      }, body);

      if (result.status !== 200) {

        return sendJson(res, 400, {

          error: "Google token exchange failed"

        });

      }

      const data = JSON.parse(result.data);

      accessToken = data.access_token;

      accessTokenExpiresAt =

        Date.now() +

        Number(data.expires_in || 3600) * 1000;

      if (data.refresh_token) {

        refreshToken = data.refresh_token;

        console.log("REFRESH_TOKEN_CAPTURED");

      }

      res.writeHead(302, {

        Location: "/viewer?v=3"

      });

      return res.end();

    } catch (error) {

      return sendJson(res, 500, {

        error: error.message

      });

    }

  }
    if (url.pathname === "/api/cameras") {

    try {

      const result = await nestGet(

        "/v1/enterprises/" +

        PROJECT_ID +

        "/devices"

      );

      if (result.status !== 200) {

        return sendJson(res, result.status, {

          error: "Unable to load Nest cameras",

          authorize: "/auth"

        });

      }

      const data = JSON.parse(result.data);

      const cameras = (data.devices || [])

        .filter(device =>

          device.type ===

          "sdm.devices.types.CAMERA"

        )

        .map((device, index) => {

          const info =

            device.traits &&

            device.traits[

              "sdm.devices.traits.Info"

            ];

          return {

            device: device.name,

            name:

              (info && info.customName) ||

              "Camera " + (index + 1)

          };

        });

      return sendJson(res, 200, {

        count: cameras.length,

        cameras: cameras

      });

    } catch (error) {

      return sendJson(res, 500, {

        error: error.message

      });

    }

  }
    if (

    url.pathname === "/api/webrtc" &&

    req.method === "POST"

  ) {

    let body = "";

    req.on("data", chunk => {

      body += chunk;

    });

    req.on("end", async () => {

      try {

        const input = JSON.parse(body);

        if (!input.device || !input.offerSdp) {

          return sendJson(res, 400, {

            error: "Missing device or offerSdp"

          });

        }

        console.log(

          "START_CAMERA:",

          input.device

        );

        const result = await nestPost(

          "/v1/" +

          input.device +

          ":executeCommand",

          {

            command:

              "sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream",

            params: {

              offerSdp: input.offerSdp

            }

          }

        );

        let data;

        try {

          data = JSON.parse(result.data);

        } catch {

          data = {

            error: "Invalid response from Google"

          };

        }

        return sendJson(

          res,

          result.status,

          data

        );

      } catch (error) {

        return sendJson(res, 500, {

          error: error.message

        });

      }

    });

    return;

  }
    if (url.pathname === "/viewer") {

    res.writeHead(200, {

      "Content-Type": "text/html; charset=utf-8",

      "Cache-Control":

        "no-store, no-cache, must-revalidate"

    });

    return res.end(`<!DOCTYPE html>

<html>

<head>

<meta charset="utf-8">

<meta

  name="viewport"

  content="width=device-width,initial-scale=1"

>

<title>NestView TV</title>

<style>

* {

  box-sizing: border-box;

}

body {

  margin: 0;

  padding: 20px;

  background: #05070b;

  color: white;

  font-family: Arial, sans-serif;

}

.app {

  max-width: 1100px;

  margin: auto;

}

h1 {

  margin-bottom: 4px;

}

.edition {

  color: #9aa7b8;

  margin-bottom: 15px;

}

video {

  display: block;

  width: 100%;

  min-height: 250px;

  max-height: 70vh;

  background: black;

  border-radius: 15px;

}

#cameraName {

  font-size: 26px;

  font-weight: bold;

  margin-top: 14px;

}

#cameraNumber,

#status {

  color: #b7c0cc;

  margin-top: 5px;

}

.controls {

  margin-top: 15px;

}

button,

select {

  min-height: 52px;

  margin: 4px;

  padding: 10px 15px;

  border: 0;

  border-radius: 10px;

  font-size: 16px;

}

</style>

</head>

<body>

<div class="app">

<h1>NestView TV</h1>

<div class="edition">

  Roku Edition • Version 3

</div>

<video

  id="camera"

  autoplay

  playsinline

  muted>

</video>

<div id="cameraName">

  Loading cameras...

</div>

<div id="cameraNumber"></div>

<div class="controls">

  <button id="previous">

    ◀ Previous

  </button>

  <button id="start">

    Start Camera

  </button>

  <button id="next">

    Next ▶

  </button>

  <button id="patrol">

    Patrol OFF

  </button>

  <select id="speed">

    <option value="10000">10 sec</option>

    <option value="15000" selected>

      15 sec

    </option>

    <option value="30000">30 sec</option>

    <option value="60000">60 sec</option>

  </select>

</div>

<div id="status">

  Loading cameras...

</div>
<script>

const video =

  document.getElementById("camera");

const cameraName =

  document.getElementById("cameraName");

const cameraNumber =

  document.getElementById("cameraNumber");

const statusText =

  document.getElementById("status");

const startButton =

  document.getElementById("start");

const nextButton =

  document.getElementById("next");

const previousButton =

  document.getElementById("previous");

const patrolButton =

  document.getElementById("patrol");

const speedSelect =

  document.getElementById("speed");

let cameras = [];

let currentIndex = 0;

let peer = null;

let patrolTimer = null;

let requestNumber = 0;

async function loadCameras() {

  const response = await fetch(

    "/api/cameras?t=" + Date.now(),

    {

      cache: "no-store"

    }

  );

  if (response.status === 401) {

    window.location.href = "/auth";

    return false;

  }

  const data = await response.json();

  if (!response.ok) {

    throw new Error(

      data.error ||

      "Unable to load cameras"

    );

  }

  cameras = data.cameras || [];

  if (!cameras.length) {

    throw new Error(

      "No Nest cameras found"

    );

  }

  showCamera();

  statusText.textContent =

    cameras.length + " cameras ready";

  return true;

}

function showCamera() {

  if (!cameras.length) {

    return;

  }

  cameraName.textContent =

    cameras[currentIndex].name;

  cameraNumber.textContent =

    (currentIndex + 1) +

    " of " +

    cameras.length;

}

function closeCurrentStream() {

  if (peer) {

    try {

      peer.close();

    } catch (error) {}

    peer = null;

  }

  if (video.srcObject) {

    video.srcObject

      .getTracks()

      .forEach(track => {

        try {

          track.stop();

        } catch (error) {}

      });

    video.srcObject = null;

  }

}
async function startCamera() {

  try {

    if (!cameras.length) {

      const loaded = await loadCameras();

      if (!loaded) {

        return;

      }

    }

    const thisRequest =

      ++requestNumber;

    const selectedCamera =

      cameras[currentIndex];

    showCamera();

    statusText.textContent =

      "Opening " +

      selectedCamera.name +

      "...";

    closeCurrentStream();

    const newPeer =

      new RTCPeerConnection();

    peer = newPeer;

    newPeer.addTransceiver(

      "audio",

      {

        direction: "recvonly"

      }

    );

    newPeer.addTransceiver(

      "video",

      {

        direction: "recvonly"

      }

    );

    newPeer.createDataChannel(

      "dataSendChannel"

    );

    newPeer.ontrack = event => {

      if (

        thisRequest === requestNumber &&

        event.streams &&

        event.streams[0]

      ) {

        video.srcObject =

          event.streams[0];

        video.play().catch(() => {});

      }

    };

    const offer =

      await newPeer.createOffer();

    await newPeer.setLocalDescription(

      offer

    );

    const response = await fetch(

      "/api/webrtc",

      {

        method: "POST",

        headers: {

          "Content-Type":

            "application/json"

        },

        body: JSON.stringify({

          device:

            selectedCamera.device,

          offerSdp:

            newPeer.localDescription.sdp

        })

      }

    );

    const result =

      await response.json();

    if (

      thisRequest !== requestNumber

    ) {

      try {

        newPeer.close();

      } catch (error) {}

      return;

    }

    if (!response.ok) {

      throw new Error(

        result.error ||

        "Unable to start camera"

      );

    }

    if (

      !result.results ||

      !result.results.answerSdp

    ) {

      throw new Error(

        "No WebRTC answer received"

      );

    }

    await newPeer.setRemoteDescription({

      type: "answer",

      sdp: result.results.answerSdp

    });

    statusText.textContent =

      selectedCamera.name +

      " • Live";

  } catch (error) {

    console.error(error);

    statusText.textContent =

      "Error: " + error.message;

  }

}
async function nextCamera() {

  if (!cameras.length) return;

  currentIndex =

    (currentIndex + 1) %

    cameras.length;

  showCamera();

  await startCamera();

}

async function previousCamera() {

  if (!cameras.length) return;

  currentIndex =

    (currentIndex - 1 + cameras.length) %

    cameras.length;

  showCamera();

  await startCamera();

}

function stopPatrol() {

  if (patrolTimer) {

    clearInterval(patrolTimer);

    patrolTimer = null;

  }

  patrolButton.textContent =

    "Patrol OFF";

}

function startPatrol() {

  stopPatrol();

  patrolButton.textContent =

    "Patrol ON";

  patrolTimer = setInterval(

    nextCamera,

    Number(speedSelect.value)

  );

}

startButton.onclick = startCamera;

nextButton.onclick = nextCamera;

previousButton.onclick = previousCamera;

patrolButton.onclick = () => {

  if (patrolTimer) {

    stopPatrol();

  } else {

    startPatrol();

  }

};

speedSelect.onchange = () => {

  if (patrolTimer) {

    startPatrol();

  }

};

document.addEventListener(

  "keydown",

  event => {

    if (event.key === "ArrowRight") {

      nextCamera();

    }

    if (event.key === "ArrowLeft") {

      previousCamera();

    }

  }

);

loadCameras().catch(error => {

  statusText.textContent =

    "Nest connection required: " +

    error.message;

});
</script>

</div>

</body>

</html>`);

  }

  return sendJson(res, 404, {

    error: "Not found"

  });

});

server.listen(PORT, "0.0.0.0", () => {

  console.log(

    "NestView TV Roku backend VERSION 3 running on port " +

    PORT

  );

});
