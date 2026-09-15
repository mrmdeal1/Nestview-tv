const http = require("http");

const PORT = process.env.PORT || 10000;

const server = http.createServer((req, res) => {

  res.setHeader("Content-Type", "application/json");

  res.setHeader("Access-Control-Allow-Origin", "*");

  if (req.url === "/") {

    res.writeHead(200);

    res.end(JSON.stringify({

      app: "NestView TV",

      status: "online"

    }));

    return;

  }

  if (req.url === "/health") {

    res.writeHead(200);

    res.end(JSON.stringify({

      status: "ok"

    }));

    return;

  }

  res.writeHead(404);

  res.end(JSON.stringify({

    error: "Not found"

  }));

});

server.listen(PORT, "0.0.0.0", () => {

  console.log(`NestView TV server running on port ${PORT}`);

});
