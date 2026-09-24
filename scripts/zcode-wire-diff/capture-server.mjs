// Minimal capture server for the ZCode wire-diff harness: records every request
// (method/url/headers/body) the real ZCode CLI sends, and answers with a minimal
// valid Anthropic SSE stream so the agent loop completes after one model call.
//
// Started by run.sh; not meant to be run standalone.
import { createServer } from "node:http";
import { writeFileSync, mkdirSync } from "node:fs";

const OUT = process.env.CAPTURE_DIR ?? "/tmp/zcode-wire-diff/captured";
const PORT_FILE = process.env.CAPTURE_PORT_FILE ?? "/tmp/zcode-wire-diff/port";
mkdirSync(OUT, { recursive: true });
let n = 0;

const SSE = [
  'event: message_start',
  'data: {"type":"message_start","message":{"id":"msg_diff","type":"message","role":"assistant","model":"diff-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}',
  '',
  'event: content_block_start',
  'data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}',
  '',
  'event: content_block_delta',
  'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}',
  '',
  'event: content_block_stop',
  'data: {"type":"content_block_stop","index":0}',
  '',
  'event: message_delta',
  'data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}',
  '',
  'event: message_stop',
  'data: {"type":"message_stop"}',
  '',
  '',
].join("\n");

// Ephemeral port (0): a stale listener on a fixed port must never make a
// capture silently land somewhere else. The chosen port is published to
// CAPTURE_PORT_FILE for run.sh to template into the provider config.
const server = createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    const body = Buffer.concat(chunks).toString("utf8");
    const rec = {
      seq: ++n,
      method: req.method,
      url: req.url,
      headers: req.headers,
      body: body.length > 8192 ? body.slice(0, 8192) + "…[truncated]" : body,
      at: new Date().toISOString(),
    };
    writeFileSync(`${OUT}/req-${String(n).padStart(3, "0")}.json`, JSON.stringify(rec, null, 2));
    if (req.url.includes("/v1/messages")) {
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
      res.end(SSE);
    } else {
      res.writeHead(404, { "content-type": "application/json" });
      res.end('{"error":"not found"}');
    }
  });
}).listen(0, "127.0.0.1", () => {
  const { port } = server.address();
  writeFileSync(PORT_FILE, String(port));
  console.log(`capture server on 127.0.0.1:${port}`);
});
