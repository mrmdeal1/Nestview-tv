const http = require("http");

const PORT = process.env.PORT || 10000;

const CLIENT_ID = process.env.GOOGLE_CLIENT_ID;

const CLIENT_SECRET = process.env.GOOGLE_CLIENT_SECRET;

const PROJECT_ID = process.env.DEVICE_ACCESS_PROJECT_ID;

const REDIRECT_URI =

  "https://nestview-tv.onrender.com/oauth/callback";

const server = http.createServer((req, res) => {

  const url = new URL(req.url, `https://${req.headers.host}`);

  if (url.pathname === "/") {

    res.writeHead(200, { "Content-Type": "application/json" });

    res.end(

      JSON.stringify({

        app: "NestView TV",

        status: "online",

        auth: "/auth"

      })

    );

    return;

  }

  if (url.pathname === "/health") {

    res.writeHead(200, { "Content-Type": "application/json" });

    res.end(JSON.stringify({ status: "ok" }));

    return;

  }

  if (url.pathname === "/auth") {

    const authUrl =

      `https://nestservices.google.com/partnerconnections/${PROJECT_ID}/auth` +

      `?redirect_uri=${encodeURIComponent(REDIRECT_URI)}` +

      `&access_type=offline` +

      `&prompt=consent` +

      `&client_id=${encodeURIComponent(CLIENT_ID)}` +

      `&response_type=code` +

      `&scope=${encodeURIComponent(

        "https://www.googleapis.com/auth/sdm.service"

      )}`;

    res.writeHead(302, { Location: authUrl });

    res.end();

    return;

  }

  if (url.pathname === "/oauth/callback") {

    const code = url.searchParams.get("code");

    if (!code) {

      res.writeHead(400, { "Content-Type": "text/plain" });

      res.end("NestView TV did not receive an authorization code.");

      return;

    }

    const body = new URLSearchParams({

      client_id: CLIENT_ID,

      client_secret: CLIENT_SECRET,

      code,

      grant_type: "authorization_code",

      redirect_uri: REDIRECT_URI

    }).toString();

    const request = require("https").request(

      {

        hostname: "oauth2.googleapis.com",

        path: "/token",

        method: "POST",

        headers: {

          "Content-Type": "application/x-www-form-urlencoded",

          "Content-Length": Buffer.byteLength(body)

        }

      },

      (googleRes) => {

        let data = "";

        googleRes.on("data", (chunk) => {

          data += chunk;

        });

        googleRes.on("end", () => {

          if (googleRes.statusCode < 200 || googleRes.statusCode >= 300) {

            res.writeHead(500, { "Content-Type": "text/plain" });

            res.end("Google authorization failed.");

            return;

          }

          res.writeHead(200, { "Content-Type": "text/html" });

          res.end(`

            <html>

              <body style="font-family:Arial;text-align:center;padding:40px">

                <h1>NestView TV Connected</h1>

                <p>Google Nest authorization succeeded.</p>

                <p>You can return to NestView TV.</p>

              </body>

            </html>

          `);

        });

      }

    );

    request.on("error", () => {

      res.writeHead(500, { "Content-Type": "text/plain" });

      res.end("Authorization request failed.");

    });

    request.write(body);

    request.end();

    return;

  }

  res.writeHead(404, { "Content-Type": "application/json" });

  res.end(JSON.stringify({ error: "Not found" }));

});

server.listen(PORT, "0.0.0.0", () => {

  console.log(`NestView TV server running on port ${PORT}`);

});
