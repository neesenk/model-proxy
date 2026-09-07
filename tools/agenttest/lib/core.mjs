/**
 * core.mjs — agenttest 与 sweep 共享的无头核心。
 *
 * 从 agenttest.mjs 提取的可复用部分:协议定义、agent 构造、场景定义、
 * 超时与结果读取。交互式渲染(REPL/流式打印)留在 agenttest.mjs。
 */

import { spawn } from "node:child_process";
import { parse as parseYAML } from "yaml";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { streamSimple } from "@earendil-works/pi-ai/compat";
import { Agent } from "@earendil-works/pi-agent-core";
import { Type } from "typebox";
import { quadrantImageContent } from "./testimage.mjs";

// ---------------------------------------------------------------------------
// 协议定义:protocol 名 → pi-ai api + baseUrl 拼法
// anthropic SDK 自己拼 /v1/messages,所以 baseUrl 不带 /v1;
// openai SDK 在 baseUrl 后直接拼 /chat/completions、/responses,所以要带 /v1。
// ---------------------------------------------------------------------------
export const PROTOCOLS = {
  anthropic: { api: "anthropic-messages", baseUrl: (o) => origin(o) },
  chat: { api: "openai-completions", baseUrl: (o) => `${origin(o)}/v1` },
  responses: { api: "openai-responses", baseUrl: (o) => `${origin(o)}/v1` },
};
export const PROTOCOL_NAMES = Object.keys(PROTOCOLS);

export function origin(opts) {
  return `http://${opts.host}:${opts.port}`;
}

export function makeModel(opts, protocol, modelId, reasoning, vision = false) {
  const p = PROTOCOLS[protocol];
  return {
    id: modelId,
    name: modelId,
    api: p.api,
    provider: "model-proxy",
    baseUrl: p.baseUrl(opts),
    reasoning,
    input: vision ? ["text", "image"] : ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: 200000,
    maxTokens: 8192,
    // 一次性 provider 钉死(代理 x-mp-force-provider 头),测试指定后端时用
    ...(opts.forceProvider ? { headers: { "x-mp-force-provider": opts.forceProvider } } : {}),
  };
}

// ---------------------------------------------------------------------------
// 工具:刻意覆盖不同参数形状(无参/字符串/嵌套),真实打满 tool_use 转换。
// baseDir 限定 read_file/write_file 的根目录(编码工程任务在独立副本目录
// 里跑);缺省为 process.cwd(),与历史行为一致。
// ---------------------------------------------------------------------------
export function makeTools(baseDir) {
  const root = baseDir ? path.resolve(baseDir) : process.cwd();
  const inside = (abs) => abs === root || abs.startsWith(root + path.sep);
  return [
    {
      name: "get_time",
      label: "Get Time",
      description: "Get the current date and time.",
      parameters: Type.Object({}),
      execute: async () => ({
        content: [{ type: "text", text: new Date().toISOString() }],
        details: {},
      }),
    },
    {
      name: "calc",
      label: "Calculator",
      description: "Evaluate an arithmetic expression like 2*(3+4).",
      parameters: Type.Object({ expression: Type.String({ description: "arithmetic expression" }) }),
      execute: async (_id, { expression }) => {
        if (!/^[0-9+\-*/().%\s]*$/.test(expression)) throw new Error("only arithmetic allowed");
        const value = Function(`"use strict"; return (${expression})`)();
        return { content: [{ type: "text", text: String(value) }], details: {} };
      },
    },
    {
      name: "read_file",
      label: "Read File",
      description: "Read a UTF-8 text file (relative to the working directory, capped at 8KB).",
      parameters: Type.Object({ path: Type.String({ description: "relative file path" }) }),
      execute: async (_id, { path: p }) => {
        const abs = path.resolve(root, p);
        if (!inside(abs)) throw new Error("path escapes the working directory");
        const buf = fs.readFileSync(abs);
        const text = buf.subarray(0, 8192).toString("utf8");
        return { content: [{ type: "text", text }], details: { bytes: buf.length } };
      },
    },
    {
      name: "write_file",
      label: "Write File",
      description: "Write a UTF-8 text file (relative to the working directory). Creates or overwrites the file; parent directories are not created.",
      parameters: Type.Object({
        path: Type.String({ description: "relative file path" }),
        content: Type.String({ description: "full file content" }),
      }),
      execute: async (_id, { path: p, content }) => {
        if (typeof content !== "string") throw new Error("content must be a string");
        if (content.length > 65536) throw new Error("content too large (64KB cap)");
        const abs = path.resolve(root, p);
        if (!inside(abs)) throw new Error("path escapes the working directory");
        fs.writeFileSync(abs, content, "utf8");
        return { content: [{ type: "text", text: `wrote ${p} (${Buffer.byteLength(content)} bytes)` }], details: {} };
      },
    },
    {
      name: "run_tests",
      label: "Run Functional Tests",
      description:
        "Run the project's functional test suite (node --test test/) in the working directory and return the result. " +
        "Use it to verify your implementation before finishing; iterate until all tests pass.",
      parameters: Type.Object({}),
      execute: async () => {
        if (!fs.existsSync(path.join(root, "test"))) {
          throw new Error("no test/ directory in the working directory");
        }
        const { code, out } = await new Promise((resolve) => {
          const p = spawn(process.execPath, ["--test", "test/"], { cwd: root });
          let out = "";
          const grab = (d) => (out += d);
          p.stdout.on("data", grab);
          p.stderr.on("data", grab);
          const timer = setTimeout(() => p.kill("SIGKILL"), 60000);
          p.on("close", (c) => {
            clearTimeout(timer);
            resolve({ code: c, out });
          });
        });
        // Keep the verdict, the failing-test names and their assertion lines;
        // cap the payload so a wall of passing tests cannot flood the context.
        const lines = out.split("\n");
        const keep = lines.filter((l) =>
          /✖|✔|not ok|^ok|failing|passing|tests |suites |AssertionError|expected|actual|at file|exit code/i.test(l),
        );
        const text = (keep.length ? keep : lines).join("\n").slice(0, 4096);
        return {
          content: [{ type: "text", text: `exit=${code}\n${text}${out.length > 4096 ? "\n…(truncated)" : ""}` }],
          details: { code },
        };
      },
    },
    {
      name: "make_test_image",
      label: "Make Test Image",
      description:
        "Generate a test image: a square split into 4 colored quadrants. Returns the image. " +
        "Use it when asked to look at / analyze a generated test image.",
      parameters: Type.Object({}),
      execute: async () => ({
        content: [{ type: "text", text: "test image attached (4 quadrants)" }, quadrantImageContent()],
        details: {},
      }),
    },
  ];
}

// ---------------------------------------------------------------------------
// Agent 构造(无头;订阅事件由调用方决定)
// ---------------------------------------------------------------------------
export function makeAgent(opts, cfg) {
  let payloadSeq = 0;
  const agent = new Agent({
    initialState: {
      systemPrompt:
        "You are a test agent inside a protocol-conversion test harness. " +
        "Follow instructions exactly and keep answers short. When asked to use a tool, always call it.",
      model: makeModel(opts, cfg.protocol, cfg.model, cfg.thinking !== "off", cfg.vision),
      thinkingLevel: cfg.thinking === "off" ? "minimal" : cfg.thinking,
      tools: cfg.tools ? makeTools(cfg.baseDir) : [],
    },
    streamFn: streamSimple,
    getApiKey: () => "agenttest-placeholder",
    onPayload: opts.dumpDir
      ? (payload) => {
          const f = path.join(opts.dumpDir, `${String(++payloadSeq).padStart(2, "0")}_${cfg.protocol}_payload.json`);
          fs.writeFileSync(f, JSON.stringify(payload, null, 2));
        }
      : opts.raw
        ? (payload) => process.stderr.write(`${payloadSummary(payload)}\n`)
        : undefined,
    onResponse: opts.dumpDir
      ? (response) => {
          const f = path.join(opts.dumpDir, `${String(payloadSeq).padStart(2, "0")}_${cfg.protocol}_response.json`);
          fs.writeFileSync(f, JSON.stringify(response, null, 2));
        }
      : undefined,
  });
  return agent;
}

export async function runPrompt(agent, text, images) {
  const start = Date.now();
  await agent.prompt(text, images);
  await agent.waitForIdle();
  return Date.now() - start;
}

export function allMessages(agent) {
  return agent.state.messages;
}

export function lastAssistant(agent) {
  const msgs = allMessages(agent).filter((m) => m.role === "assistant");
  const last = msgs[msgs.length - 1];
  const text = (last?.content ?? []).filter((c) => c.type === "text").map((c) => c.text).join("");
  return { text, stopReason: last?.stopReason, error: last?.errorMessage };
}

// 上游明确报"模型不支持图片输入"是外部前置条件(路由把别名映射到了非多模态后端),
// 按仓库 live 测试惯例判 SKIP 而非 FAIL —— 它不是协议转换缺陷。
export function isImageUnsupported(r) {
  return /not support image|image input|does not support image|image.*not supported/i.test(String(r.error ?? ""));
}

export function withTimeout(promise, ms) {
  return Promise.race([
    promise,
    new Promise((_, rej) => setTimeout(() => rej(new Error(`timeout ${ms}ms`)), ms)),
  ]);
}

// ---------------------------------------------------------------------------
// 场景定义:与 agenttest 交互模式共用一套语义
// pass 判定读 agent 会话的可观察结果(回复文本/工具调用/思考块),不放宽。
// ---------------------------------------------------------------------------
export const SCENARIOS = [
  {
    name: "ping",
    run: async (agent, nonce) => {
      await runPrompt(agent, `Reply with exactly: pong (test-ref: ${nonce})`);
      const r = lastAssistant(agent);
      return { pass: /pong/i.test(r.text), detail: r.text.slice(0, 80), extra: r };
    },
  },
  {
    name: "tool",
    needsTools: true,
    run: async (agent, nonce) => {
      await runPrompt(agent, `Use the calc tool to compute 6*7, then reply with just the number. (test-ref: ${nonce})`);
      const r = lastAssistant(agent);
      const called = allMessages(agent).some(
        (m) => m.role === "assistant" && (m.content ?? []).some((c) => c.type === "toolCall" && c.name === "calc"),
      );
      return { pass: called && /42/.test(r.text), detail: `toolCalled=${called} text=${r.text.slice(0, 60)}`, extra: r };
    },
  },
  {
    name: "memory",
    run: async (agent, nonce) => {
      await runPrompt(agent, `Remember the number 4242 (test-ref: ${nonce}). Reply with just: ok`);
      await runPrompt(agent, "What number did I ask you to remember? Reply with just the number.");
      const r = lastAssistant(agent);
      return { pass: /4242/.test(r.text), detail: r.text.slice(0, 80), extra: r };
    },
  },
  {
    name: "thinking",
    needsThinking: true,
    run: async (agent, nonce) => {
      await runPrompt(agent, `Think briefly, then reply with exactly: pong (test-ref: ${nonce})`);
      const msgs = allMessages(agent);
      const thought = msgs.some(
        (m) => m.role === "assistant" && (m.content ?? []).some((c) => c.type === "thinking" && c.thinking?.length > 0),
      );
      const r = lastAssistant(agent);
      return { pass: thought && /pong/i.test(r.text), detail: `thinkingBlock=${thought}`, extra: r };
    },
  },
  {
    name: "vision",
    needsVision: true,
    run: async (agent, nonce) => {
      // 用户消息直接带图:测 image block 的入站转换
      await runPrompt(
        agent,
        `This image is a square split into 4 colored quadrants. What color is the TOP-LEFT quadrant? ` +
          `Answer with just the color name. (test-ref: ${nonce})`,
        [quadrantImageContent()],
      );
      const r = lastAssistant(agent);
      if (isImageUnsupported(r)) return { skip: true, detail: `上游不支持图片输入(${String(r.error).slice(0, 60)})` };
      return { pass: /red/i.test(r.text), detail: r.text.slice(0, 80), extra: r };
    },
  },
  {
    name: "vision_tool",
    needsVision: true,
    needsTools: true,
    run: async (agent, nonce) => {
      // 工具结果带图:测 tool_result 内 image content 的转换
      await runPrompt(
        agent,
        `Use the make_test_image tool, look at the image it returns, and tell me the color of the ` +
          `top-left quadrant. Answer with just the color name. (test-ref: ${nonce})`,
      );
      const r = lastAssistant(agent);
      if (isImageUnsupported(r)) return { skip: true, detail: `上游不支持图片输入(${String(r.error).slice(0, 60)})` };
      const called = allMessages(agent).some(
        (m) => m.role === "assistant" && (m.content ?? []).some((c) => c.type === "toolCall" && c.name === "make_test_image"),
      );
      return { pass: called && /red/i.test(r.text), detail: `toolCalled=${called} text=${r.text.slice(0, 60)}`, extra: r };
    },
  },
];

// ---------------------------------------------------------------------------
// 发现:config providers → 目标列表(sweep/codews 共用)
// ---------------------------------------------------------------------------

export function discoverTargets(opts) {
  const cfg = parseYAML(fs.readFileSync(opts.config, "utf8"));
  if (!cfg?.providers || typeof cfg.providers !== "object") {
    throw new Error(`${opts.config} 中没有 providers 配置`);
  }
  const meta = loadCatalogMeta(opts);
  const targets = [];
  const wantProvider = (name) => !opts.providers || opts.providers.some((p) => name.includes(p));
  const wantModel = (m) => !opts.models || opts.models.some((f) => m.includes(f));
  for (const [name, p] of Object.entries(cfg.providers)) {
    if (!wantProvider(name)) continue;
    const models = Array.isArray(p.models) ? p.models : [];
    if (models.length === 0) {
      continue;
    }
    // 客户端可调用的是暴露名:alias 把裸 id 重命名成统一路由键(k3 →
    // kimi-k3),裸 id 本来就不是路由键,按暴露名测。
    const alias = (p.alias && typeof p.alias === "object") ? p.alias : {};
    for (const model of models) {
      const exposed = alias[model] || model;
      if (!wantModel(model) && !wantModel(exposed)) continue;
      targets.push({ provider: name, model: exposed, upstream: model, reasoning: meta.reasoning(exposed), vision: meta.vision(exposed) });
    }
  }
  return targets;
}

// 目录缓存(~/.model-proxy/models_cache.json,`models pull` 刷新)提供
// reasoning 与输入多模态判定;缺失时退回保守启发式。
export function loadCatalogMeta(opts) {
  const cachePath = path.join(opts.home ?? os.homedir(), ".model-proxy", "models_cache.json");
  let byName = {};
  try {
    byName = JSON.parse(fs.readFileSync(cachePath, "utf8")).by_name ?? {};
  } catch {
    // 缺缓存时保守判定
  }
  const heuristicReasoning = /glm|kimi|claude|gpt-|deepseek|doubao|minimax|qwen/i;
  return {
    reasoning: (model) => {
      const m = byName[model];
      if (m) return !!m.reasoning;
      return heuristicReasoning.test(model);
    },
    vision: (model) => {
      const m = byName[model];
      return !!m && Array.isArray(m.in) && m.in.includes("image");
    },
  };
}

// payloadSummary renders a one-line raw dump for --raw mode (stderr, capped).
function payloadSummary(payload) {
  const s = JSON.stringify(payload);
  return `${"\x1b[2m"}[payload] ${s.slice(0, 2000)}${s.length > 2000 ? "…" : ""}${"\x1b[0m"}`;
}
