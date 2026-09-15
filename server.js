
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

function postForm(hostname, path, data) {

  return new Promise((resolve, reject) => {

    const body = new URLSearchParams(data).toString();

    const req = https.request(

      {

        hostname,

        path,

        method: "POST",

        headers: {

          "Content-Type": "application/x-www-form-urlencoded",

          "Content-Length": Buffer.byteLength(body)

        }

      },

      (response) => {

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

      }

    );

    req.on("error", reject);

    req.write(body);

    req.end();

  });

}

function getJson(hostname, path, token) {

  return new Promise((resolve, reject) => {

    const req = https.request(

      {

        hostname,

        path,

        method: "GET",

        headers: {

          Authorization: `Bearer ${token}`,

          "Content-Type": "application/json"

        }

      },

      (response) => {

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

      }

    );

    req.on("error", reject);

    req.end();

  });

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

      data.error_description || "Token refresh failed"

    );

  }

  accessToken = data.access_token;

  return accessToken;

}

const server = http.createServer(async (req, res) => {

  try {

    const url = new URL(

      req.url,

      `https://${req.headers.host}`

    );

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

      const params = new URLSearchParams({

        redirect_uri: REDIRECT_URI,

        access_type: "offline",

        prompt: "consent",

        client_id: CLIENT_ID,

        response_type: "code",

        scope:

          "https://www.googleapis.com/auth/sdm.service"

      });

      const authUrl =

        `https://nestservices.google.com/partnerconnections/` +

        `${PROJECT_ID}/auth?${params.toString()}`;

      res.writeHead(302, {

        Location: authUrl

      });

      res.end();

      return;

    }

    if (url.pathname === "/oauth/callback") {

      const code = url.searchParams.get("code");

      const error = url.searchParams.get("error");

      if (error) {

        sendJson(res, 400, {

          error

        });

        return;

      }

      if (!code) {

        sendJson(res, 400, {

          error: "Authorization code missing"

        });

        return;

      }

      const tokenResult = await postForm(

        "oauth2.googleapis.com",

        "/token",

        {

          client_id: CLIENT_ID,

          client_secret: CLIENT_SECRET,

          code,

          grant_type: "authorization_code",

          redirect_uri: REDIRECT_URI

        }

      );

      let tokenData;

      try {

        tokenData = JSON.parse(tokenResult.body);

      } catch {

        tokenData = {

          error: tokenResult.body

        };

      }

      if (tokenResult.status !== 200) {

        sendJson(res, tokenResult.status, {

          error: "Token exchange failed",

          details:

            tokenData.error_description ||

            tokenData.error ||

            "Unknown error"

        });

        return;

      }

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

            <h2>NestView TV Connected</h2>

            <p>Google Nest authorization succeeded.</p>

            <p>Your cameras are ready to be checked.</p>

            <p>

              <a href="/devices">

                Check cameras

              </a>

            </p>

          </body>

        </html>

      `);

      return;

    }

    if (url.pathname === "/devices") {

      if (!accessToken) {

        if (refreshToken) {

          await refreshAccessToken();

        } else {

          res.writeHead(302, {

            Location: "/auth"

          });

          res.end();

          return;

        }

      }

      let result = await getJson(

        "smartdevicemanagement.googleapis.com",

        `/v1/enterprises/${PROJECT_ID}/devices`,

        accessToken

      );

      if (

        result.status === 401 &&

        refreshToken

      ) {

        await refreshAccessToken();

        result = await getJson(

          "smartdevicemanagement.googleapis.com",

          `/v1/enterprises/${PROJECT_ID}/devices`,

          accessToken

        );

      }

      let data;

      try {

        data = JSON.parse(result.body);

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

server.listen(PORT, "0.0.0.0", () => {

  console.log(

    `NestView TV server running on port ${PORT}`

  );

});
    
