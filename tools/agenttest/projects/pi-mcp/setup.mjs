// pi-mcp 项目的 setup 钩子:渲染 pi 的 models.json(与 pi-coding 相同的三个 provider)
// 和 pi-mcp-adapter 的 mcp.json(本地 stdio server,directTools 直挂为一级工具)。
import fs from "node:fs";
import path from "node:path";

const PROTOCOL_DEFS = {
  anthropic: { api: "anthropic-messages", baseUrl: (o) => o },
  chat: { api: "openai-completions", baseUrl: (o) => `${o}/v1` },
  responses: { api: "openai-responses", baseUrl: (o) => `${o}/v1` },
};

export default async function setup(ctx) {
  const agentDir = path.join(ctx.projectDir, "agent");
  fs.mkdirSync(agentDir, { recursive: true });
  const providers = {};
  for (const [proto, def] of Object.entries(PROTOCOL_DEFS)) {
    providers[`mp-${proto}`] = {
      name: `model-proxy (${proto})`,
      baseUrl: def.baseUrl(ctx.origin),
      api: def.api,
      apiKey: "agenttest-placeholder",
      models: [
        {
          id: ctx.model,
          name: ctx.model,
          reasoning: true,
          input: ["text"],
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
          contextWindow: 200000,
          maxTokens: 8192,
        },
      ],
    };
  }
  fs.writeFileSync(path.join(agentDir, "models.json"), JSON.stringify({ providers }, null, 2));

  const mcpConfig = {
    mcpServers: {
      e2e: {
        command: process.execPath,
        args: [path.join(ctx.projectDir, "mcp-server.mjs")],
        directTools: true,
      },
    },
  };
  fs.writeFileSync(path.join(agentDir, "mcp.json"), JSON.stringify(mcpConfig, null, 2));
  ctx.log("models.json + mcp.json rendered");
}
