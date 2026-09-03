// pi-coding 项目的 setup 钩子:每次运行前渲染 agent/models.json,
// 按协议定义三个指向 model-proxy 的 provider(baseUrl 拼法与 agenttest.mjs 一致)。
// ctx: { projectDir, workspace, protocol, model, origin, log }
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
  ctx.log(`models.json rendered for ${Object.keys(providers).join(", ")}`);
}
