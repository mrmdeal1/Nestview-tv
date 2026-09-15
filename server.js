const http = require("http");

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

async function getAccessToken() {

  if (!refreshToken) {

    throw new Error("NestView TV is not authorized yet.");

  }

  const response = await fetch(

    "https://oauth2.googleapis.com/token",

    {

      method: "POST",

      headers: {

        "Content-Type": "application/x-www-form-urlencoded"

      },

      body: new URLSearchParams({

        client_id: CLIENT_ID,

        client_secret: CLIENT_SECRET,

        refresh_token: refreshToken,

        grant_type: "refresh_token"

      })

    }

  );

  const data = await response.json();

  if (!response.ok) {

    console.log("Google refresh error:", {

      error: data.error,

      error_description: data.error_description

    });

    throw new Error(

      data.error_description || data.error || "Could not refresh Google access token."

    );

  }

  accessToken = data.access_token;

  return accessToken;

}

const server = http.createServer(async (req, res) => {

  try {

    const url = new URL(

      req.url,

      `https://${req.headers.host || "nestview-tv.onrender.com"}`

    );

    if (url.pathname === "/") {

      return sendJson(res, 200, {

        app: "NestView TV",

        status: "online",

        auth: "/auth",

        devices: "/devices"

      });

    }

    if (url.pathname === "/health") {

      return sendJson(res, 200, {

        status: "ok"

      });

    }

    if (url.pathname === "/auth") {

      const params = new URLSearchParams({

        redirect_uri: REDIRECT_URI,

        access_type: "offline",

        prompt: "consent",

        client_id: CLIENT_ID,

        response_type: "code",

        scope: "https://www.googleapis.com/auth/sdm.service"

      });

      const authUrl =

        `https://nestservices.google.com/partnerconnections/${PROJECT_ID}/auth?${params.toString()}`;

      res.writeHead(302, {

        Location: authUrl

      });

      return res.end();

    }

    if (url.pathname === "/oauth/callback") {

      const code = url.searchParams.get("code");

      if (!code) {

        return sendJson(res, 400, {

          error: "Authorization code missing"

        });

      }

      const tokenResponse = await fetch(

        "https://oauth2.googleapis.com/token",

        {

          method: "POST",

          headers: {

            "Content-Type": "application/x-www-form-urlencoded"

          },

          body: new URLSearchParams({

            client_id: CLIENT_ID,

            client_secret: CLIENT_SECRET,

            code: code,

            grant_type: "authorization_code",

            redirect_uri: REDIRECT_URI

          })

        }

      );

      const tokens = await tokenResponse.json();

      if (!tokenResponse.ok) {

        console.log("Google token exchange error:", {

          error: tokens.error,

          error_description: tokens.error_description

        });

        return sendJson(res, tokenResponse.status, {

          error: "Token exchange failed",

          google_error: tokens.error,

          details: tokens.error_description

        });

      }

      accessToken = tokens.access_token;

      if (tokens.refresh_token) {

        refreshToken = tokens.refresh_token;

      }

      console.log("Google Nest authorization succeeded.");

      res.writeHead(200, {

        "Content-Type": "text/html"

      });

      return res.end(`

        <html>

          <head>

            <meta name="viewport" content="width=device-width, initial-scale=1">

          </head>

          <body style="font-family:Arial;text-align:center;padding:50px">

            <h2>NestView TV Connected</h2>

            <p>Google Nest authorization succeeded.</p>

            <p>Your cameras are ready to be checked.</p>

          </body>

        </html>

      `);

    }

    if (url.pathname === "/devices") {

      let token = accessToken;

      if (!token) {

        token = await getAccessToken();

      }

      let response = await fetch(

        `https://smartdevicemanagement.googleapis.com/v1/enterprises/${PROJECT_ID}/devices`,

        {

          headers: {

            Authorization: `Bearer ${token}`

          }

        }

      );

      if (response.status === 401 && refreshToken) {

        token = await getAccessToken();

        response = await fetch(

          `https://smartdevicemanagement.googleapis.com/v1/enterprises/${PROJECT_ID}/devices`,

          {

            headers: {

              Authorization: `Bearer ${token}`

            }

          }

        );

      }

      const data = await response.json();

      if (!response.ok) {

        console.log("Google SDM devices error:", {

          status: response.status,

          error: data.error

        });

        return sendJson(res, response.status, data);

      }

      return sendJson(res, 200, data);

    }

    return sendJson(res, 404, {

      error: "Not found"

    });

  } catch (error) {

    console.error("NestView server error:", error.message);

    
