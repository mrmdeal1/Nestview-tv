const http = require("http");

const https = require("https");

const PORT = process.env.PORT || 10000;

const CLIENT_ID = process.env.GOOGLE_CLIENT_ID;

const CLIENT_SECRET = process.env.GOOGLE_CLIENT_SECRET;

const PROJECT_ID = process.env.DEVICE_ACCESS_PROJECT_ID;

const REDIRECT_URI =

  "https://nestview-tv.onrender.com/oauth/callback";

let accessToken = null;

let refreshToken = process.env.GOOGLE_REFRESH_TOKEN || null;

function sendJson(res, status, data) {

  res.writeHead(status, {

    "Content-Type": "application/json",

    "Access-Control-Allow-Origin": "*"

  });

  res.end(JSON.stringify(data, null, 2));

}

function postForm(hostname, path, formData) {

  return new Promise((resolve, reject) => {

    const body = new URLSearchParams(formData).toString();

    const options = {

      hostname: hostname,

      path: path,

      method: "POST",

      headers: {

        "Content-Type": "application/x-www-form-urlencoded",

        "Content-Length": Buffer.byteLength(body)

      }

    };

    const request = https.request(options, response => {

      let data = "";

      response.on("data", chunk => {

        data += chunk;

      });

      response.on("end", () => {

        resolve({

          status: response.statusCode,

          data: data

        });

      });

    });

    request.on("error", reject);

    request.write(body);

    request.end();

  });

}

function googleGet(path, token) {

  return new Promise((resolve, reject) => {

    const options = {

      hostname: "smartdevicemanagement.googleapis.com",

      path: path,

      method: "GET",

      headers: {

        Authorization: "Bearer " + token

      }

    };

    const request = https.request(options, response => {

      let data = "";

      response.on("data", chunk => {

        data += chunk;

      });

      response.on("end", () => {

        resolve({

          status: response.statusCode,

          data: data

        });

      });

    });

    request.on("error", reject);

    request.end();

  });

}
function googlePost(path, token, bodyObject) {

  return new Promise((resolve, reject) => {

    const body = JSON.stringify(bodyObject);

    const options = {

      hostname: "smartdevicemanagement.googleapis.com",

      path: path,

      method: "POST",

      headers: {

        Authorization: "Bearer " + token,

        "Content-Type": "application/json",

        "Content-Length": Buffer.byteLength(body)

      }

    };

    const request = https.request(options, response => {

      let data = "";

      response.on("data", chunk => {

        data += chunk;

      });

      response.on("end", () => {

        resolve({

          status: response.statusCode,

          data: data

        });

      });

    });

    request.on("error", reject);

    request.write(body);

    request.end();

  });

}
async function getAccessToken() {

  if (refreshToken) {

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

    if (result.status === 200) {

      const tokenData = JSON.parse(result.data);

      accessToken = tokenData.access_token;

    }

  }

  return accessToken;

}

const server = http.createServer(async (req, res) => {
console.log("REQUEST:", req.method, req.url);
  const host = req.headers.host || "nestview-tv.onrender.com";

  const url = new URL(req.url, "https://" + host);

  if (url.pathname === "/") {

    sendJson(res, 200, {

      app: "NestView TV",

      status: "online",

      auth: "/auth",

      devices: "/devices"

    });

    return;

  }
if (url.pathname === "/token-status") {

  sendJson(res, 200, {

    refreshTokenLoaded: !!refreshToken,

    accessTokenLoaded: !!accessToken

  });

  return;

}
  
  if (url.pathname === "/health") {

    sendJson(res, 200, {

      status: "ok"

    });

    return;

  }

  if (url.pathname === "/auth") {

    if (!CLIENT_ID || !CLIENT_SECRET || !PROJECT_ID) {

      sendJson(res, 500, {

        error: "Missing environment variables"

      });

      return;

    }

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

    res.end();

    return;

  }

  if (url.pathname === "/oauth/callback") {

    const code = url.searchParams.get("code");

    if (!code) {

      sendJson(res, 400, {

        error: "Authorization code missing"

      });

      return;

    }

    try {

      const result = await postForm(

        "oauth2.googleapis.com",

        "/token",

        {

          client_id: CLIENT_ID,

          client_secret: CLIENT_SECRET,

          code: code,

          grant_type: "authorization_code",

          redirect_uri: REDIRECT_URI

        }

      );

      if (result.status !== 200) {

        sendJson(res, 400, {

          error: "Token exchange failed",

          details: result.data

        });

        return;

      }

      const tokenData = JSON.parse(result.data);

      accessToken = tokenData.access_token;

      if (tokenData.refresh_token) {

        refreshToken = tokenData.refresh_token;

console.log("REFRESH_TOKEN_CAPTURED"); 
        
      }
return;

}
    if (url.pathname === "/save-refresh-token") {

  if (!refreshToken) {

    sendJson(res, 404, { error: "No refresh token loaded" });

    return;

  }

  res.writeHead(200, {

    "Content-Type": "text/plain",

    "Cache-Control": "no-store"

  });

  res.end(refreshToken);

  return;

}
      res.writeHead(200, {

        "Content-Type": "text/html"

      });

      res.end(`

        <!doctype html>

        <html>

          <head>

            <meta name="viewport" content="width=device-width, initial-scale=1">

            <title>NestView TV</title>

          </head>

          <body style="font-family:Arial;text-align:center;padding:50px;">

            <h2>NestView TV Connected</h2>

            <p>Google Nest authorization succeeded.</p>

            <p>Your cameras are ready.</p>

            <p><a href="/devices">Check Cameras</a></p>

          </body>

        </html>

      `);

      return;

    } catch (error) {

      sendJson(res, 500, {

        error: "OAuth error",

        details: error.message

      });

      return;

    }

  }

  if (url.pathname === "/devices") {

    try {

      const token = await getAccessToken();

      if (!token) {

        res.writeHead(302, {

          Location: "/auth"

        });

        res.end();

        return;

      }

      const result = await googleGet(

        "/v1/enterprises/" +

        PROJECT_ID +

        "/devices",

        token

      );

      let data;

      try {

        data = JSON.parse(result.data);

      } catch {

        data = {

          raw: result.data

        };

      }

      sendJson(res, result.status, data);

      return;

    } catch (error) {

      sendJson(res, 500, {

        error: "Unable to retrieve devices",

        details: error.message

      });

      return;

    }

  }
    if (url.pathname === "/api/webrtc" && req.method === "POST") {

    try {

      let body = "";

      req.on("data", chunk => {

        body += chunk;

      });

      req.on("end", async () => {

        try {

          const input = JSON.parse(body);

          const device = input.device;

          const offerSdp = input.offerSdp;

          if (!device || !offerSdp) {

            sendJson(res, 400, {

              error: "device and offerSdp are required"

            });

            return;

          }

          const token = await getAccessToken();

          if (!token) {

            sendJson(res, 401, {

              error: "Nest authorization required"

            });

            return;

          }

          const result = await googlePost(

            "/v1/" + device + ":executeCommand",

            token,

            {

              command: "sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream",

              params: {

                offerSdp: offerSdp

              }

            }

          );

          let data;

          try {

            data = JSON.parse(result.data);

          } catch {

            data = {

              raw: result.data

            };

          }

          sendJson(res, result.status, data);

        } catch (error) {

          sendJson(res, 500, {

            error: "WebRTC request failed",

            details: error.message

          });

        }

      });

      return;

    } catch (error) {

      sendJson(res, 500, {

        error: "WebRTC route failed",

        details: error.message

      });

      return;

    }

  }if (url.pathname === "/viewer") {

    res.writeHead(200, {

      "Content-Type": "text/html"

    });

    res.end(`

      <!doctype html>

      <html>

        <head>

          <meta name="viewport" content="width=device-width, initial-scale=1">

          <title>NestView TV</title>

        </head>

        <body style="background:#000;color:#fff;font-family:Arial;text-align:center;padding:40px;">

          <h1>NestView TV</h1>

          <video

  id="camera"

  autoplay

  playsinline

  muted

  style="width:100%;max-width:1000px;background:#111;"

></video>

<p id="status">Ready to connect.</p>

<button

  id="startButton"

  style="font-size:22px;padding:14px 28px;"

>

  Start Camera

</button>
<div style="margin-top:18px;">

  <button id="prevButton" style="font-size:20px;padding:12px 20px;">Previous</button>

  <span id="cameraName" style="margin:0 15px;font-size:20px;">Camera 1</span>

  <button id="nextButton" style="font-size:20px;padding:12px 20px;">Next</button>

</div>

<script>

const startButton = document.getElementById("startButton");

const statusText = document.getElementById("status");

const video = document.getElementById("camera");
const prevButton = document.getElementById("prevButton");

const nextButton = document.getElementById("nextButton");

const cameraName = document.getElementById("cameraName");

let cameras = [];

let currentCameraIndex = 0;
let activePeer = null;
startButton.addEventListener("click", async function () {

  startButton.disabled = true;

  statusText.textContent = "Finding cameras...";

  try {

    const devicesResponse = await fetch("/devices");

    const devicesData = await devicesResponse.json();

    if (!devicesData.devices || devicesData.devices.length === 0) {

      throw new Error("No cameras found.");

    }
    cameras = devicesData.devices.filter(function (device) {

  return device.type === "sdm.devices.types.CAMERA";

});

if (cameras.length === 0) {

  throw new Error("No compatible cameras found.");

}

const camera = cameras[currentCameraIndex];

cameraName.textContent = "Camera " + (currentCameraIndex + 1) + " of " + cameras.length;
if (video.srcObject) {

  video.srcObject.getTracks().forEach(track => track.stop());

  video.srcObject = null;

}
    statusText.textContent = "Starting live camera...";
if (activePeer) {

  activePeer.close();

  activePeer = null;
}
    const peer = new RTCPeerConnection();
activePeer = peer;
    
    peer.addTransceiver("audio", {

  direction: "recvonly"

});

peer.addTransceiver("video", {

  direction: "recvonly"

});

peer.createDataChannel("dataSendChannel");

peer.ontrack = function (event) {

  if (event.streams && event.streams[0]) {

    video.srcObject = event.streams[0];

  }

};
      



    const offer = await peer.createOffer();

    await peer.setLocalDescription(offer);

    const response = await fetch("/api/webrtc", {

      method: "POST",

      headers: {

        "Content-Type": "application/json"

      },

      body: JSON.stringify({

        device: camera.name,

        offerSdp: peer.localDescription.sdp

      })

    });

    const result = await response.json();

    if (!response.ok) {

      throw new Error(JSON.stringify(result));

    }

    if (!result.results || !result.results.answerSdp) {

      throw new Error("Google did not return a WebRTC answer.");

    }

    await peer.setRemoteDescription({

      type: "answer",

      sdp: result.results.answerSdp

    });

    statusText.textContent = "Live";
startButton.disabled = false;
  } catch (error) {

    statusText.textContent = "Error: " + error.message;

    startButton.disabled = false;

  }

});
nextButton.addEventListener("click", function () {

  currentCameraIndex = (currentCameraIndex + 1) % cameras.length;

  cameraName.textContent = "Camera " + (currentCameraIndex + 1);

  startButton.click();

});

prevButton.addEventListener("click", function () {

  currentCameraIndex = (currentCameraIndex - 1 + cameras.length) % cameras.length;

  cameraName.textContent = "Camera " + (currentCameraIndex + 1);

  startButton.click();

});
</script>

        </body>

      </html>

    `);

    return;

  }
  sendJson(res, 404, {

    error: "Not found"

  });

});

server.listen(PORT, "0.0.0.0", () => {

  console.log("NestView TV server running on port " + PORT);

});
