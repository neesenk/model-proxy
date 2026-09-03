#!/usr/bin/env node
/**
 * agenttest — model-proxy 交互式真实调用测试工具。
 *
 * 基于 pi 的 agent 接口(@earendil-works/pi-agent-core + pi-ai)驱动真实
 * agent loop(多轮 + 工具调用 + thinking + 流式),从三种 ingress 协议打进
 * model-proxy,真实验证协议转换链路:
 *
 *   --protocol anthropic → POST /v1/messages        (api: anthropic-messages)
 *   --protocol chat      → POST /v1/chat/completions (api: openai-completions)
 *   --protocol responses → POST /v1/responses        (api: openai-responses)
 *
 * 用法:
 *   node agenttest.mjs                          # 交互式 REPL
 *   node agenttest.mjs --prompt "ping"          # 单发模式
 *   node agenttest.mjs --matrix --models a,b    # 协议×模型批量自检
 *
 * 前置: model-proxy serve 在跑(默认 127.0.0.1:15722),且已 login。
 */

import readline from "node:readline";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { streamSimple } from "@earendil-works/pi-ai/compat";
import { Agent } from "@earendil-works/pi-agent-core";
import { Type } from "typebox";
import { quadrantImageContent } from "./lib/testimage.mjs";

const DIM = "\x1b[2m";
const BOLD = "\x1b[1m";
const RED = "\x1b[31m";
const GREEN = "\x1b[32m";
const YELLOW = "\x1b[33m";
const CYAN = "\x1b[36m";
const RESET = "\x1b[0m";

const TOOL_DIR = path.dirname(fileURLToPath(import.meta.url));

// ---------------------------------------------------------------------------
// 协议定义:protocol 名 → pi-ai api + baseUrl 拼法
// anthropic SDK 自己拼 /v1/messages,所以 baseUrl 不带 /v1;
// openai SDK 在 baseUrl 后直接拼 /chat/completions、/responses,所以要带 /v1。
// ---------------------------------------------------------------------------
const PROTOCOLS = {
  anthropic: { api: "anthropic-messages", baseUrl: (o) => origin(o) },
  chat: { api: "openai-completions", baseUrl: (o) => `${origin(o)}/v1` },
  responses: { api: "openai-responses", baseUrl: (o) => `${origin(o)}/v1` },
};
const PROTOCOL_NAMES = Object.keys(PROTOCOLS);

function origin(opts) {
  return `http://${opts.host}:${opts.port}`;
}

function makeModel(opts, protocol, modelId, reasoning, vision = false) {
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
// ---------------------------------------------------------------------------
function makeTools() {
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
      description: "Read a UTF-8 text file (relative to the current working directory, capped at 8KB).",
      parameters: Type.Object({ path: Type.String({ description: "relative file path" }) }),
      execute: async (_id, { path: p }) => {
        const abs = path.resolve(process.cwd(), p);
        if (!abs.startsWith(process.cwd() + path.sep)) throw new Error("path escapes cwd");
        const buf = fs.readFileSync(abs);
        const text = buf.subarray(0, 8192).toString("utf8");
        return { content: [{ type: "text", text }], details: { bytes: buf.length } };
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
// 输出渲染
// ---------------------------------------------------------------------------
function makeRenderer(opts) {
  let inThinking = false;
  let sawThinking = false;
  const state = { text: "", thinking: false, toolCalls: [], usage: null, stopReason: null, error: null };
  const out = (s) => process.stdout.write(s);

  function handleAssistantEvent(ev) {
    if (ev.type === "thinking_delta") {
      if (!inThinking) {
        out(`${DIM}[thinking] `);
        inThinking = true;
        sawThinking = true;
      }
      out(ev.delta);
    } else if (ev.type === "text_delta") {
      if (inThinking) {
        out(RESET);
        inThinking = false;
      }
      state.text += ev.delta;
      out(ev.delta);
    } else if (ev.type === "toolcall_start") {
      if (inThinking) {
        out(RESET);
        inThinking = false;
      }
      const tc = ev.partial?.content?.[ev.contentIndex];
      out(`\n${CYAN}⚙ tool_call ${tc?.name ?? "?"}${RESET}\n`);
    } else if (ev.type === "toolcall_end") {
      const tc = ev.partial?.content?.[ev.contentIndex] ?? ev.toolCall;
      if (tc) {
        state.toolCalls.push({ name: tc.name, args: tc.arguments });
        out(`${DIM}  args: ${JSON.stringify(tc.arguments)}${RESET}\n`);
      }
    }
  }

  function handleEvent(event) {
    if (event.type === "message_update" && event.assistantMessageEvent) {
      handleAssistantEvent(event.assistantMessageEvent);
    } else if (event.type === "message_end" && event.message?.role === "assistant") {
      const m = event.message;
      if (m.usage) state.usage = m.usage;
      if (m.stopReason) state.stopReason = m.stopReason;
      if (m.errorMessage) state.error = m.errorMessage;
    } else if (event.type === "tool_execution_start") {
      out(`${CYAN}→ exec ${event.toolName ?? event.toolCall?.name ?? "tool"}${RESET}\n`);
    } else if (event.type === "tool_execution_end") {
      const r = event.result;
      const text = (r?.content ?? []).filter((c) => c.type === "text").map((c) => c.text).join(" ");
      const err = event.isError ? `${RED}ERROR ` : "";
      out(`${CYAN}← result${RESET} ${err}${DIM}${text.slice(0, 200)}${RESET}\n`);
    }
  }

  function finish(elapsedMs) {
    if (inThinking) out(RESET);
    out(`\n${DIM}${"—".repeat(50)}${RESET}\n`);
    if (state.usage) {
      const u = state.usage;
      out(`← stop=${state.stopReason ?? "?"} input=${u.input ?? "?"} output=${u.output ?? "?"}` +
        (u.cacheRead ? ` cacheRead=${u.cacheRead}` : "") + ` ${(elapsedMs / 1000).toFixed(1)}s\n`);
    } else {
      out(`← stop=${state.stopReason ?? "?"} ${(elapsedMs / 1000).toFixed(1)}s\n`);
    }
    if (state.error) out(`${RED}← error: ${state.error}${RESET}\n`);
  }

  return { state, handleEvent, finish, sawThinking: () => sawThinking };
}

// ---------------------------------------------------------------------------
// Agent 构造
// ---------------------------------------------------------------------------
function makeAgent(opts, cfg, renderer) {
  let payloadSeq = 0;
  const agent = new Agent({
    initialState: {
      systemPrompt:
        "You are a test agent inside a protocol-conversion test harness. " +
        "Follow instructions exactly and keep answers short. When asked to use a tool, always call it.",
      model: makeModel(opts, cfg.protocol, cfg.model, cfg.thinking !== "off", cfg.vision),
      thinkingLevel: cfg.thinking === "off" ? "minimal" : cfg.thinking,
      tools: cfg.tools ? makeTools() : [],
    },
    streamFn: streamSimple,
    getApiKey: () => "agenttest-placeholder",
    onPayload: opts.dumpDir
      ? (payload) => {
          const f = path.join(opts.dumpDir, `${String(++payloadSeq).padStart(2, "0")}_${cfg.protocol}_payload.json`);
          fs.writeFileSync(f, JSON.stringify(payload, null, 2));
        }
      : opts.raw
        ? (payload) => process.stderr.write(`${DIM}[payload] ${JSON.stringify(payload).slice(0, 2000)}${RESET}\n`)
        : undefined,
    onResponse: opts.dumpDir
      ? (response) => {
          const f = path.join(opts.dumpDir, `${String(payloadSeq).padStart(2, "0")}_${cfg.protocol}_response.json`);
          fs.writeFileSync(f, JSON.stringify(response, null, 2));
        }
      : undefined,
  });
  if (renderer) agent.subscribe((event) => renderer.handleEvent(event));
  return agent;
}

async function runPrompt(agent, text, images) {
  const start = Date.now();
  await agent.prompt(text, images);
  await agent.waitForIdle();
  return Date.now() - start;
}

// ---------------------------------------------------------------------------
// matrix 批量自检:protocol × model × scenario
// ---------------------------------------------------------------------------
const SCENARIOS = [
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

function allMessages(agent) {
  return agent.state.messages;
}

function lastAssistant(agent) {
  const msgs = allMessages(agent).filter((m) => m.role === "assistant");
  const last = msgs[msgs.length - 1];
  const text = (last?.content ?? []).filter((c) => c.type === "text").map((c) => c.text).join("");
  return { text, stopReason: last?.stopReason, error: last?.errorMessage };
}

// 上游明确报"模型不支持图片输入"是外部前置条件(路由把别名映射到了非多模态后端),
// 按仓库 live 测试惯例判 SKIP 而非 FAIL —— 它不是协议转换缺陷。
function isImageUnsupported(r) {
  return /not support image|image input|does not support image|image.*not supported/i.test(String(r.error ?? ""));
}

async function runMatrix(opts, protocols, models) {
  const scenarios = SCENARIOS.filter(
    (s) => (!s.needsThinking || opts.withThinking) && (!s.needsVision || opts.withVision),
  );
  const results = [];
  for (const protocol of protocols) {
    for (const model of models) {
      for (const sc of scenarios) {
        const cfg = {
          protocol,
          model,
          thinking: sc.needsThinking ? "high" : "off",
          tools: !!sc.needsTools,
          vision: !!sc.needsVision,
        };
        const label = `${protocol.padEnd(10)} ${model.padEnd(24)} ${sc.name}`;
        process.stdout.write(`${DIM}▶ ${label}${RESET}\n`);
        const agent = makeAgent(opts, cfg, null);
        // nonce 撞开代理响应 cache,保证每次场景真实打到上游转换链路
        const nonce = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
        let res;
        const start = Date.now();
        try {
          res = await withTimeout(sc.run(agent, nonce), opts.scenarioTimeoutMs);
        } catch (e) {
          res = { pass: false, detail: `error: ${e.message?.slice(0, 120)}` };
        }
        const ms = Date.now() - start;
        if (!res.skip && res.extra?.error) {
          res.pass = false;
          res.detail = `${res.detail} | stop=${res.extra.stopReason} err=${String(res.extra.error).slice(0, 120)}`;
        }
        results.push({ protocol, model, scenario: sc.name, pass: !!res.pass, skip: !!res.skip, detail: res.detail, ms });
        const verdict = res.skip ? YELLOW + "SKIP" : res.pass ? GREEN + "PASS" : RED + "FAIL";
        process.stdout.write(`  ${verdict}${RESET} ${(ms / 1000).toFixed(1)}s ${DIM}${res.detail}${RESET}\n`);
      }
    }
  }
  const failed = results.filter((r) => !r.pass && !r.skip);
  const skipped = results.filter((r) => r.skip);
  process.stdout.write(
    `\n${BOLD}matrix: ${results.length - failed.length - skipped.length}/${results.length - skipped.length} passed` +
      (skipped.length ? `, ${skipped.length} skipped` : "") +
      `${RESET}\n`,
  );
  if (failed.length) {
    for (const f of failed) process.stdout.write(`${RED}  FAIL ${f.protocol} ${f.model} ${f.scenario}${RESET}\n`);
  }
  return failed.length === 0;
}

function withTimeout(promise, ms) {
  return Promise.race([
    promise,
    new Promise((_, rej) => setTimeout(() => rej(new Error(`timeout ${ms}ms`)), ms)),
  ]);
}

// ---------------------------------------------------------------------------
// REPL
// ---------------------------------------------------------------------------
const HELP = `${BOLD}命令:${RESET}
  /protocol <${PROTOCOL_NAMES.join("|")}>   切换 ingress 协议(重置会话)
  /model <id>                              切换模型/路由别名(重置会话)
  /thinking <off|minimal|low|medium|high|xhigh|max>
  /tools [on|off]                          开关工具(默认 on)
  /raw [on|off]                            控制台 dump 请求 payload
  /vision [prompt]                         发一条带四象限测试图的消息(默认问左上角颜色)
  /state                                   当前配置
  /matrix                                  以当前配置跑批量自检
  /reset                                   清空会话
  /help, /quit
其他输入直接作为 prompt 发送(多轮会话,工具调用自动执行)。`;

async function repl(opts, cfg) {
  // 单个持久订阅转发到可替换的 current renderer,避免重复订阅。
  let current = makeRenderer(opts);
  let agent = null;
  const newAgent = () => {
    agent = makeAgent(opts, cfg, null);
    agent.subscribe((event) => current.handleEvent(event));
  };
  newAgent();

  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  let closed = false;
  const ask = () => {
    if (closed) return;
    const t = cfg.thinking !== "off" ? ` th=${cfg.thinking}` : "";
    rl.setPrompt(`${BOLD}${cfg.protocol}${RESET}:${cfg.model}${t}${cfg.tools ? "" : " [no-tools]"} > `);
    rl.prompt();
  };

  process.stdout.write(`${DIM}target ${origin(opts)} — /help 查看命令${RESET}\n`);
  ask();

  rl.on("line", (line) => {
    // 串行化:处理期间暂停 line 事件,处理完再 resume,防止管道输入并发触发。
    rl.pause();
    handleLine(line.trim())
      .catch((e) => process.stdout.write(`${RED}${e.message}${RESET}\n`))
      .finally(() => {
        if (!closed) {
          rl.resume();
          ask();
        }
      });
  });

  async function handleLine(input) {
    if (!input) return;
    if (input.startsWith("/")) {
      const [cmd, ...rest] = input.split(/\s+/);
      const arg = rest.join(" ");
      try {
        switch (cmd) {
          case "/quit":
          case "/exit":
            closed = true;
            rl.close();
            return;
          case "/help":
            process.stdout.write(HELP + "\n");
            break;
          case "/state":
            process.stdout.write(`${JSON.stringify({ ...cfg, target: origin(opts) }, null, 2)}\n`);
            break;
          case "/protocol":
            if (!PROTOCOLS[arg]) throw new Error(`协议必须是 ${PROTOCOL_NAMES.join("|")}`);
            cfg.protocol = arg;
            newAgent();
            process.stdout.write(`→ protocol=${arg} (会话已重置)\n`);
            break;
          case "/model":
            if (!arg) throw new Error("用法: /model <id>");
            cfg.model = arg;
            newAgent();
            process.stdout.write(`→ model=${arg} (会话已重置)\n`);
            break;
          case "/thinking":
            cfg.thinking = arg || "off";
            newAgent();
            process.stdout.write(`→ thinking=${cfg.thinking}\n`);
            break;
          case "/tools":
            cfg.tools = arg ? arg === "on" : !cfg.tools;
            newAgent();
            process.stdout.write(`→ tools=${cfg.tools ? "on" : "off"}\n`);
            break;
          case "/raw":
            opts.raw = arg ? arg === "on" : !opts.raw;
            newAgent();
            process.stdout.write(`→ raw=${opts.raw ? "on" : "off"}\n`);
            break;
          case "/reset":
            newAgent();
            process.stdout.write("→ 会话已清空\n");
            break;
          case "/vision": {
            if (!cfg.vision) {
              cfg.vision = true;
              newAgent(); // 重建 model.input 带 image
            }
            const q =
              arg ||
              "This image is a square split into 4 colored quadrants. What color is the TOP-LEFT quadrant? Answer with just the color name.";
            current = makeRenderer(opts);
            try {
              const ms = await runPrompt(agent, q, [quadrantImageContent()]);
              current.finish(ms);
            } catch (e) {
              process.stdout.write(`\n${RED}✗ ${e.message}${RESET}\n`);
            }
            break;
          }
          case "/matrix": {
            const models = arg ? arg.split(",").map((s) => s.trim()) : [cfg.model];
            await runMatrix(opts, [cfg.protocol], models);
            newAgent(); // matrix 跑过的 agent 不复用
            break;
          }
          default:
            process.stdout.write(`未知命令 ${cmd},/help 查看\n`);
        }
      } catch (e) {
        process.stdout.write(`${RED}${e.message}${RESET}\n`);
      }
      return;
    }

    // 普通 prompt:换一个新 renderer 重置 per-turn 状态
    current = makeRenderer(opts);
    try {
      const ms = await runPrompt(agent, input);
      current.finish(ms);
    } catch (e) {
      process.stdout.write(`\n${RED}✗ ${e.message}${RESET}\n`);
    }
  }


  rl.on("SIGINT", () => {
    if (agent.state.isStreaming) {
      agent.abort();
      process.stdout.write(`\n${YELLOW}aborted${RESET}\n`);
    } else {
      closed = true;
      rl.close();
    }
  });

  await new Promise((resolve) => rl.on("close", resolve));
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------
function parseArgs(argv) {
  const opts = {
    host: "127.0.0.1",
    port: 15722,
    raw: false,
    dumpDir: null,
    forceProvider: null,
    prompt: null,
    matrix: false,
    withThinking: false,
    withVision: false,
    vision: false,
    models: null,
    protocols: null,
    scenarioTimeoutMs: 180000,
  };
  const cfg = { protocol: "anthropic", model: null, thinking: "off", tools: true, vision: false };
  for (let i = 2; i < argv.length; i++) {
    const a = argv[i];
    const next = () => argv[++i];
    switch (a) {
      case "--host": opts.host = next(); break;
      case "--port": opts.port = Number(next()); break;
      case "--protocol": cfg.protocol = next(); break;
      case "--model": cfg.model = next(); break;
      case "--thinking": cfg.thinking = next(); break;
      case "--no-tools": cfg.tools = false; break;
      case "--raw": opts.raw = true; break;
      case "--dump-dir": opts.dumpDir = next(); break;
      case "--force-provider": opts.forceProvider = next(); break;
      case "--prompt": opts.prompt = next(); break;
      case "--matrix": opts.matrix = true; break;
      case "--with-thinking": opts.withThinking = true; break;
      case "--with-vision": opts.withVision = true; break;
      case "--vision": cfg.vision = true; opts.vision = true; break;
      case "--models": opts.models = next().split(",").map((s) => s.trim()); break;
      case "--protocols": opts.protocols = next().split(",").map((s) => s.trim()); break;
      case "--timeout": opts.scenarioTimeoutMs = Number(next()) * 1000; break;
      case "-h":
      case "--help":
        process.stdout.write(__doc__usage());
        process.exit(0);
      default:
        process.stderr.write(`未知参数 ${a}\n${__doc__usage()}`);
        process.exit(2);
    }
  }
  if (!PROTOCOLS[cfg.protocol]) {
    process.stderr.write(`--protocol 必须是 ${PROTOCOL_NAMES.join("|")}\n`);
    process.exit(2);
  }
  if (!cfg.model) cfg.model = cfg.protocol === "anthropic" ? "claude-haiku-4-5" : "gpt-5.5";
  if (opts.dumpDir) fs.mkdirSync(opts.dumpDir, { recursive: true });
  return { opts, cfg };
}

function __doc__usage() {
  return `用法: node agenttest.mjs [选项]
  --host/--port        代理地址(默认 127.0.0.1:15722)
  --protocol P         ${PROTOCOL_NAMES.join("|")}(默认 anthropic)
  --model M            模型/路由别名(默认 anthropic→claude-haiku-4-5, 其他→gpt-5.5)
  --thinking L         off|minimal|low|medium|high|xhigh|max(默认 off)
  --no-tools           不带工具
  --raw                控制台 dump 出站 payload
  --dump-dir DIR       每个请求/响应写文件(跨协议 diff 用)
  --force-provider P   钉死后端 provider(x-mp-force-provider 头)
  --prompt "..."       单发模式,跑完退出
  --vision             单发/REPL 带图(单发给 --prompt 附四象限测试图)
  --matrix             批量自检: --protocols a,b --models x,y [--with-thinking] [--with-vision]
  --timeout N          matrix 单场景超时秒数(默认 180)
`;
}

async function main() {
  const { opts, cfg } = parseArgs(process.argv);

  if (opts.matrix) {
    const protocols = opts.protocols ?? [cfg.protocol];
    const models = opts.models ?? [cfg.model];
    for (const p of protocols) {
      if (!PROTOCOLS[p]) {
        process.stderr.write(`--protocols 含未知协议 ${p}\n`);
        process.exit(2);
      }
    }
    const ok = await runMatrix(opts, protocols, models);
    process.exit(ok ? 0 : 1);
  }

  if (opts.prompt !== null) {
    const renderer = makeRenderer(opts);
    const agent = makeAgent(opts, cfg, renderer);
    try {
      const ms = await runPrompt(agent, opts.prompt, cfg.vision ? [quadrantImageContent()] : undefined);
      renderer.finish(ms);
      process.exit(renderer.state.error ? 1 : 0);
    } catch (e) {
      process.stderr.write(`${RED}✗ ${e.message}${RESET}\n`);
      process.exit(1);
    }
  }

  await repl(opts, cfg);
}

main().catch((e) => {
  process.stderr.write(`${RED}✗ ${e.stack ?? e.message}${RESET}\n`);
  process.exit(1);
});
