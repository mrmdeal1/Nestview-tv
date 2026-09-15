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

  sendJson(res, 404, {

    error: "Not found"

  });

});

server.listen(PORT, "0.0.0.0", () => {

  console.log("NestView TV server running on port " + PORT);

});
