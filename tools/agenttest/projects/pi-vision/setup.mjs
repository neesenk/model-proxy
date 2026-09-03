// pi-vision 项目的 setup 钩子:
//  1. 渲染 agent/models.json(provider 名 mpv-*,模型声明 image 输入 + 钉 aqp)
//  2. 往 workspace 播种测试文件: image.png(四象限)、digits.png(数字 42)、data.csv(合计 187)
import fs from "node:fs";
import path from "node:path";
import { quadrantPng, digitPng } from "../../lib/testimage.mjs";

const PROTOCOL_DEFS = {
  chat: { api: "openai-completions", baseUrl: (o) => `${o}/v1` },
  responses: { api: "openai-responses", baseUrl: (o) => `${o}/v1` },
};

export default async function setup(ctx) {
  const agentDir = path.join(ctx.projectDir, "agent");
  fs.mkdirSync(agentDir, { recursive: true });
  const providers = {};
  for (const [proto, def] of Object.entries(PROTOCOL_DEFS)) {
    providers[`mpv-${proto}`] = {
      name: `model-proxy vision (${proto})`,
      baseUrl: def.baseUrl(ctx.origin),
      api: def.api,
      apiKey: "agenttest-placeholder",
      models: [
        {
          id: ctx.model,
          name: ctx.model,
          reasoning: true,
          input: ["text", "image"],
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
          contextWindow: 200000,
          maxTokens: 8192,
          headers: { "x-mp-force-provider": "aqp" },
        },
      ],
    };
  }
  fs.writeFileSync(path.join(agentDir, "models.json"), JSON.stringify({ providers }, null, 2));

  fs.writeFileSync(path.join(ctx.workspace, "image.png"), quadrantPng());
  fs.writeFileSync(path.join(ctx.workspace, "digits.png"), digitPng("42"));
  fs.writeFileSync(
    path.join(ctx.workspace, "data.csv"),
    "name,value\nalpha,100\nbeta,80\ngamma,7\n",
  );
  ctx.log("models.json + image.png/digits.png/data.csv seeded");
}
