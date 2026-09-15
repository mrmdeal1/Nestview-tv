const http = require("http");

const https = require("https");

const PORT = process.env.PORT || 10000;

const CLIENT_ID = process.env.GOOGLE_CLIENT_ID;

const CLIENT_SECRET = process.env.GOOGLE_CLIENT_SECRET;

const PROJECT_ID = process.env.DEVICE_ACCESS_PROJECT_ID;

const REDIRECT_URI =

  "https://nestview-tv.onrender.com/oauth/callback";

let accessToken = null;

let refreshToken = null;

function sendJson(res, status, data) {

  res.writeHead(status, {

    "Content-Type": "application/json",

    "Access-Control-Allow-Origin": "*"

  });

  res.end(JSON.stringify(data, null, 2));

}

function requestGoogle(options, body = null) {

  return new Promise((resolve, reject) => {

    const req = https.request(options, response => {

      let result = "";

      response.on("data", chunk => {

        result += chunk;

      });

      response.on("end", () => {

        resolve({

          status: response.statusCode,

          body: result

        });

      });

    });

    req.on("error", reject);

    if (body) {

      req.write(body);

    }

    req.end();

  });

}

async function postForm(hostname, path, data) {

  const body = new URLSearchParams(data).toString();

  return requestGoogle(

    {

      hostname,

      path,

      method: "POST",

      headers: {

        "Content-Type":

          "application/x-www-form-urlencoded",

        "Content-Length":

          Buffer.byteLength(body)

      }

    },

    body

  );

}

async function getGoogleJson(path, token) {

  return requestGoogle({

    hostname:

      "smartdevicemanagement.googleapis.com",

    path,

    method: "GET",

    headers: {

      Authorization: `Bearer ${token}`,

      "Content-Type": "application/json"

    }

  });

}

async function postGoogleJson(path, token, data) {

  const body = JSON.stringify(data);

  return requestGoogle(

    {

      hostname:

        "smartdevicemanagement.googleapis.com",

      path,

      method: "POST",

      headers: {

        Authorization: `Bearer ${token}`,

        "Content-Type": "application/json",

        "Content-Length":

          Buffer.byteLength(body)

      }

    },

    body

  );

}

async function refreshAccessToken() {

  if (!refreshToken) {

    throw new Error("No refresh token available");

  }

  const result = await postForm(

    "oauth2.googleapis.com",

    "/token",

    {

      client_id: CLIENT_ID,

      client_secret: CLIENT_SECRET,

      refresh_token: refreshToken,

      grant_type: "refresh_token"

    }

  );

  const data = JSON.parse(result.body);

  if (result.status !== 200) {

    throw new Error(

      data.error_description ||

      "Token refresh failed"

    );

  }

  accessToken = data.access_token;

  return accessToken;

}

async function ensureAccessToken() {

  if (accessToken) {

    return accessToken;

  }

  if (refreshToken) {

    return refreshAccessToken();

  }

  return null;

}

function readRequestBody(req) {

  return new Promise((resolve, reject) => {

    let body = "";

    req.on("data", chunk => {

      body += chunk;

    });

    req.on("end", () => {

      resolve(body);

    });

    req.on("error", reject);

  });

}

const viewerPage = `

<!doctype html>

<html>

<head>

<meta

  name="viewport"

  content="width=device-width,initial-scale=1"

>

<title>NestView TV</title>

<style>

html,

body {

  margin: 0;

  background: #000;

  color: #fff;

  font-family: Arial, sans-serif;

  height: 100%;

}

#app {

  min-height: 100vh;

  display: flex;

  flex-direction: column;

}

header {

  padding: 16px 20px;

  background: #111;

  display: flex;

  justify-content: space-between;

  align-items: center;

}

h1 {

  font-size: 22px;

  margin: 0;

}

#cameraName {

  font-size: 16px;

  color: #bbb;

}

#videoWrap {

  flex: 1;

  display: flex;

  align-items: center;

  justify-content: center;

  background: #000;

}

video {

  width: 100%;

  max-height: calc(100vh - 150px);

  background: #000;

}

#status {

  text-align: center;

  padding: 20px;

  font-size: 18px;

}

.controls {

  padding: 14px;

  background: #111;

  display: flex;

  gap: 10px;

  justify-content: center;

}

button {

  font-size: 17px;

  padding: 12px 20px;

  border-radius: 8px;

  border: 0;

}

select {

  font-size: 16px;

  padding: 10px;

  max-width: 55%;

}

</style>

</head>

<body>

<div id="app">

<header>

  <h1>NestView TV</h1>

  <div id="cameraName">Loading cameras...</div>

</header>

<div id="videoWrap">

  <video

    id="video"

    autoplay

    playsinline

    muted

  ></video>

  <div id="status">

    Connecting to Nest...

  </div>

</div>

<div class="controls">

  <button id="prev">

    Previous

  </button>

  <select id="cameraSelect"></select>

  <button id="next">

    Next

  </button>

</div>

</div>

<script>

let cameras = [];

let currentIndex = 0;

let peerConnection = null;

const video =

  document.getElementById("video");

const status =

  document.getElementById("status");

const cameraName =

  document.getElementById("cameraName");

const cameraSelect =

  document.getElementById("cameraSelect");

async function loadCameras() {

  status.textContent =

    "Loading Nest cameras...";

  const response =

    await fetch("/api/devices");

  if (response.status === 401) {

    window.location.href = "/auth";

    return;

  }

  if (!response.ok) {

    const text = await response.text();

    status.textContent =

      "Could not load cameras: " + text;

    return;

  }

  const data = await response.json();

  cameras = (data.devices || []).filter(

    device =>

      device.type ===

        "sdm.devices.types.CAMERA" ||

      device.type ===

        "sdm.devices.types.DOORBELL"

  );

  if (!cameras.length) {

    status.textContent =

      "No Nest cameras found.";

    return;

  }

  cameraSelect.innerHTML = "";

  cameras.forEach((camera, index) => {

    const option =

      document.createElement("option");

    const info =

      camera.traits &&

      camera.traits[

        "sdm.devices.traits.Info"

      ];

    option.value = index;

    option.textContent =

      info && info.customName

        ? info.customName

        : "Camera " + (index + 1);

    cameraSelect.appendChild(option);

  });

  await playCamera(0);

}

async function playCamera(index) {

  if (!cameras.length) {

    return;

  }

  if (index < 0) {

    index = cameras.length - 1;

  }

  if (index >= cameras.length) {

    index = 0;

  }

  currentIndex = index;

  cameraSelect.value = index;

  const camera = cameras[index];

  const info =

    camera.traits &&

    camera.traits[

      "sdm.devices.traits.Info"

    ];

  const name =

    info && info.customName

      ? info.customName

      : "Camera " + (index + 1);

  cameraName.textContent = name;

  status.style.display = "block";

  video.style.display = "none";

  status.textContent =

    "Starting " + name + "...";

  if (peerConnection) {

    peerConnection.close();

    peerConnection = null;

  }

  peerConnection =

    new RTCPeerConnection();

  peerConnection.ontrack = event => {

    video.srcObject =

      event.streams[0];

    video.style.display = "block";

    status.style.display = "none";

  };

  peerConnection.addTransceiver(

    "video",

    {

      direction: "recvonly"

    }

  );

  peerConnection.addTransceiver(

    "audio",

    {

      direction: "recvonly"

    }

  );

  const offer =

    await peerConnection.createOffer();

  await peerConnection.setLocalDescription(

    offer

  );

  await waitForIceGathering(

    peerConnection

  );

  const response =

    await fetch("/api/webrtc", {

      method: "POST",

      headers: {

        "Content-Type":

          "application/json"

      },

      body: JSON.stringify({

        device: camera.name,

        offerSdp:

          peerConnection.localDescription.sdp

      })

    });

  if (response.status === 401) {

    window.location.href = "/auth";

    return;

  }

  const result =

    await response.json();

  if (!response.ok) {

    status.textContent =

      "Camera stream error: " +

      JSON.stringify(result);

    return;

  }

  const answerSdp =

    result.results &&

    result.results.answerSdp;

  if (!answerSdp) {

    status.textContent =

      "Nest did not return a video stream.";

    return;

  }

  await peerConnection.setRemoteDescription({

    type: "answer",

    sdp: answerSdp

  });

}

function waitForIceGathering(pc) {

  return new Promise(resolve => {

    if (

      pc.iceGatheringState === "complete"

    ) {

      resolve();

      return;

    }

    const checkState = () => {

      if (

        pc.iceGatheringState ===

        "complete"

      ) {

        pc.removeEventListener(

          "icegatheringstatechange",

          checkState

        );

        resolve();

      }

    };

    pc.addEventListener(

      "icegatheringstatechange",

      checkState

    );

    setTimeout(resolve, 3000);

  });

}

document

  .getElementById("next")

  .addEventListener(

    "click",

    () => {

      playCamera(currentIndex + 1);

    }

  );

document

  .getElementById("prev")

  .addEventListener(

    "click",

    () => {

      playCamera(currentIndex - 1);

    }

  );

cameraSelect.addEventListener(

  "change",

  () => {

    playCamera(

      Number(cameraSelect.value)

    );

  }

);

loadCameras().catch(error => {

  status.textContent =

    "Error: " + error.message;

});

</script>

</body>

</html>

`;

const server =

http.createServer(async (req, res) => {

  try {

    const url = new URL(

      req.url,

      \`https://\${req.headers.host}\`

    );

    if (url.pathname === "/") {

      res.writeHead(200, {

        "Content-Type": "text/html"

      });

      res.end(\`

        <!doctype html>

        <html>

        <head>

          <meta

            name="viewport"

            content="width=device-width,initial-scale=1"

          >

          <title>NestView TV</title>

        </head>

        <body

          style="

            font-family:Arial,sans-serif;

            text-align:center;

            padding:60px 20px;

          "

        >

          <h1>NestView TV</h1>

          <p>Server online.</p>

          <p>

            <a href="/viewer">

              Open Camera Viewer

            </a>

          </p>

          <p>

            <a href="/auth">

              Connect Google Nest

            </a>

          </p>

        </body>

        </html>

      \`);

      return;

    }

    if (url.pathname === "/health") {

      sendJson(res, 200, {

        status: "ok"

      });

      return;

    }

    if (url.pathname === "/viewer") {

      res.writeHead(200, {

        "Content-Type": "text/html"

      });

      res.end(viewerPage);

      return;

    }

    if (url.pathname === "/auth") {

      if (

        !CLIENT_ID ||

        !CLIENT_SECRET ||

        !PROJECT_ID

      ) {

        sendJson(res, 500, {

          error:

            "Missing environment variables"

        });

        return;

      }

      const params =

        new URLSearchParams({

          redirect_uri: REDIRECT_URI,

          access_type: "offline",

          prompt: "consent",

          client_id: CLIENT_ID,

          response_type: "code",

          scope:

            "https://www.googleapis.com/auth/sdm.service"

        });

      const authUrl =

        "https://nestservices.google.com/" +

        "partnerconnections/" +

        PROJECT_ID +

        "/auth?" +

        params.toString();

      res.writeHead(302, {

        Location: authUrl

      });

      res.end();

      return;

    }

    if (

      url.pathname ===

      "/oauth/callback"

    ) {

      const code =

        url.searchParams.get("code");

      const oauthError =

        url.searchParams.get("error");

      if (oauthError) {

        sendJson(res, 400, {

          error: oauthError

        });

        return;

      }

      if (!code) {

        sendJson(res, 400, {

          error:

            "Authorization code missing"

        });

        return;

      }

      const tokenResult =

        await postForm(

          "oauth2.googleapis.com",

          "/token",

          {

            client_id: CLIENT_ID,

            client_secret:

              CLIENT_SECRET,

            code,

            grant_type:

              "authorization_code",

            redirect_uri:

              REDIRECT_URI

          }

        );

      let tokenData;

      try {

        tokenData =

          JSON.parse(

            tokenResult.body

          );

      } catch {

        tokenData = {};

      }

      if (

        tokenResult.status !== 200

      ) {

        sendJson(

          res,

          tokenResult.status,

          {

            error:

              "Token exchange failed",

            details:

              tokenData.error_description ||

              tokenData.error ||

              tokenResult.body

          }

        );

        return;

      }

      accessToken =

        tokenData.access_token;

      if (

        tokenData.refresh_token

      ) {

        refreshToken =

          tokenData.refresh_token;

      }

      res.writeHead(302, {

        Location: "/viewer"

      });

      res.end();

      return;

    }

    if (

      url.pathname === "/devices" ||

      url.pathname === "/api/devices"

    ) {

      const token =

        await ensureAccessToken();

      if (!token) {

        if (

          url.pathname ===

          "/api/devices"

        ) {

          sendJson(res, 401, {

            error:

              "Google Nest authorization required"

          });

        } else {

          res.writeHead(302, {

            Location: "/auth"

          });

          res.end();

        }

        return;

      }

      let result =

        await getGoogleJson(

          \`/v1/enterprises/\${PROJECT_ID}/devices\`,

          token

        );

      if (

        result.status === 401 &&

        refreshToken

      ) {

        await refreshAccessToken();

        result =

          await getGoogleJson(

            \`/v1/enterprises/\${PROJECT_ID}/devices\`,

            accessToken

          );

      }

      let data;

      try {

        data =

          JSON.parse(result.body);

      } catch {

        data = {

          raw: result.body

        };

      }

      sendJson(

        res,

        result.status || 500,

        data

      );

      return;

    }

    if (

      url.pathname ===

      "/api/webrtc" &&

      req.method === "POST"

    ) {

      const token =

        await ensureAccessToken();

      if (!token) {

        sendJson(res, 401, {

          error:

            "Google Nest authorization required"

        });

        return;

      }

      const rawBody =

        await readRequestBody(req);

      let requestData;

      try {

        requestData =

          JSON.parse(rawBody);

      } catch {

        sendJson(res, 400, {

          error:

            "Invalid request"

        });

        return;

      }

      const device =

        requestData.device;

      const offerSdp =

        requestData.offerSdp;

      if (

        !device ||

        !offerSdp

      ) {

        sendJson(res, 400, {

          error:

            "Device and offerSdp are required"

        });

        return;

      }

      const commandPath =

        \`/v1/\${device}:executeCommand\`;

      const command = {

        command:

          "sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream",

        params: {

          offerSdp

        }

      };

      let result =

        await postGoogleJson(

          commandPath,

          token,

          command

        );

      if (

        result.status === 401 &&

        refreshToken

      ) {

        await refreshAccessToken();

        result =

          await postGoogleJson(

            commandPath,

            accessToken,

            command

          );

      }

      let data;

      try {

        data =

          JSON.parse(result.body);

      } catch {

        data = {

          raw: result.body

        };

      }

      sendJson(

        res,

        result.status || 500,

        data

      );

      return;

    }

    sendJson(res, 404, {

      error: "Not found"

    });

  } catch (error) {

    console.error(error);

    sendJson(res, 500, {

      error: "Server error",

      details: error.message

    });

  }

});

server.listen(

  PORT,

  "0.0.0.0",

  () => {

    console.log(

      \`NestView TV server running on port \${PORT}\`

    );

  }

);
