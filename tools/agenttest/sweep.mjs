#!/usr/bin/env node
/**
 * sweep — 对 config.yaml 配置的全部 provider × model,按协议 × thinking 参数
 * 矩阵发起真实 agent 调用,汇总结果并分析失败原因。
 *
 * 复用 agenttest 的共享核心(lib/core.mjs):同一套 pi agent loop、场景判定、
 * force-provider 钉死,只是把输入从 CLI 参数换成 config 全量枚举。
 *
 * 用法:
 *   node sweep.mjs                                  # 全 provider/model,ping 场景,3 协议
 *   node sweep.mjs --providers zhipu,volcengine     # 子集(provider 配置名子串)
 *   node sweep.mjs --models glm-5.3,kimi-k3         # 子集(model id 子串)
 *   node sweep.mjs --scenarios ping,tool,memory,thinking --thinking off,high
 *
 * 参数矩阵:每个 (provider, model) × 每个协议 × 每个 thinking 档 × 每个场景,
 * 一次真实请求(nonce 撞开响应 cache)。thinking 非 off 档只对目录判定
 * reasoning=true 的模型执行,其余 SKIP。
 *
 * 失败分析:错误文本分类(限流/鉴权/模型不存在/特性拒绝/超时/…),并与
 * daemon 落盘的探测 verdict(model_caps.json、quota_state.json 的 wire_caps)
 * 关联——探测已判 no 的腿上的失败标记为"预期内",其余为"意外失败"。
 *
 * 退出码:0 = 无失败;1 = 存在失败;2 = 用法/环境错误。
 *
 * 前置: model-proxy serve 在跑(默认 127.0.0.1:15722),已 login,目录缓存
 * (models pull)为 新。注意:全部组合是真实计费请求,--scenarios 全开前先估
 * 一下规模(默认只跑 ping,约 3×模型数 次)。
 */

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  PROTOCOLS,
  PROTOCOL_NAMES,
  discoverTargets,
  makeAgent,
  SCENARIOS,
  withTimeout,
  lastAssistant,
} from "./lib/core.mjs";

const DIM = "\x1b[2m";
const BOLD = "\x1b[1m";
const RED = "\x1b[31m";
const GREEN = "\x1b[32m";
const YELLOW = "\x1b[33m";
const RESET = "\x1b[0m";

const TOOL_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_DIR = path.resolve(TOOL_DIR, "../..");

// ---------------------------------------------------------------------------
// 参数
// ---------------------------------------------------------------------------
function parseArgs(argv) {
  const opts = {
    config: path.join(REPO_DIR, "config.yaml"),
    host: "127.0.0.1",
    port: 15722,
    providers: null,
    models: null,
    protocols: [...PROTOCOL_NAMES],
    scenarios: ["ping"],
    thinking: ["off"],
    concurrency: 3,
    scenarioTimeoutMs: 90000,
    forceProvider: null,
    report: path.join(TOOL_DIR, "sweep-last.json"),
    home: os.homedir(),
  };
  const next = () => {
    if (i >= argv.length) throw new Error(`${argv[i - 1]} 缺少值`);
    return argv[i++];
  };
  let i = 0;
  while (i < argv.length) {
    const a = argv[i++];
    switch (a) {
      case "--config": opts.config = next(); break;
      case "--host": opts.host = next(); break;
      case "--port": opts.port = Number(next()); break;
      case "--providers": opts.providers = next().split(",").map((s) => s.trim()).filter(Boolean); break;
      case "--models": opts.models = next().split(",").map((s) => s.trim()).filter(Boolean); break;
      case "--protocols": {
        opts.protocols = next().split(",").map((s) => s.trim()).filter(Boolean);
        for (const p of opts.protocols) if (!PROTOCOLS[p]) throw new Error(`未知协议 ${p}(可用: ${PROTOCOL_NAMES.join("|")})`);
        break;
      }
      case "--scenarios": {
        const names = new Set(next().split(",").map((s) => s.trim()).filter(Boolean));
        for (const n of names) if (!SCENARIOS.some((s) => s.name === n)) throw new Error(`未知场景 ${n}(可用: ${SCENARIOS.map((s) => s.name).join("|")})`);
        opts.scenarios = SCENARIOS.filter((s) => names.has(s.name)).map((s) => s.name);
        break;
      }
      case "--thinking": {
        const levels = next().split(",").map((s) => s.trim()).filter(Boolean);
        for (const l of levels) if (!["off", "minimal", "low", "medium", "high", "xhigh", "max"].includes(l)) throw new Error(`未知 thinking 档 ${l}`);
        opts.thinking = levels;
        break;
      }
      case "--concurrency": opts.concurrency = Math.max(1, Number(next())); break;
      case "--scenario-timeout": opts.scenarioTimeoutMs = Number(next()); break;
      case "--force-provider": opts.forceProvider = next(); break;
      case "--report": opts.report = next(); break;
      default: throw new Error(`未知参数 ${a}`);
    }
  }
  return opts;
}

// ---------------------------------------------------------------------------
// 探测 verdict(daemon 落盘)——失败归因用
// ---------------------------------------------------------------------------
function loadProbeVerdicts(opts) {
  const dir = path.join(opts.home, ".model-proxy");
  const out = { modelCaps: {}, wireCaps: {} };
  try {
    const mc = JSON.parse(fs.readFileSync(path.join(dir, "model_caps.json"), "utf8"));
    for (const [name, entry] of Object.entries(mc.providers ?? {})) out.modelCaps[name] = entry.models ?? {};
  } catch { /* 缺文件 = 无模型级 verdict */ }
  try {
    const qs = JSON.parse(fs.readFileSync(path.join(dir, "quota_state.json"), "utf8"));
    out.wireCaps = qs.wire_caps ?? {};
  } catch { /* 缺文件 = 无 provider 级 verdict */ }
  return out;
}

const LEG_OF_PROTOCOL = { chat: "chat", responses: "responses", anthropic: "anthropic" };

// ---------------------------------------------------------------------------
// 失败归因:错误文本分类 + probe verdict 关联
// ---------------------------------------------------------------------------
function classifyFailure(text) {
  const t = String(text ?? "");
  if (/429|rate.?limit|too many requests|quota/i.test(t)) return "rate-limit";
  if (/401|403|unauthorized|forbidden|invalid.*key|api key|authentication/i.test(t)) return "auth";
  if (/model not found|does not exist|unknown model|invalid model|no such model/i.test(t)) return "model-not-found";
  if (/messages\.role|invalid value: `?developer`?|role.*not valid|unsupported.*role/i.test(t)) return "role-rejection";
  if (/not supported for .* in|function tools.*not supported/i.test(t)) return "feature-rejection";
  if (/not support image|image input|does not support image|image.*not supported/i.test(t)) return "image-unsupported";
  if (/timeout|timed out|ETIMEDOUT/i.test(t)) return "timeout";
  if (/abort/i.test(t)) return "aborted";
  if (/ECONNREFUSED|ECONNRESET|ENOTFOUND|fetch failed|network|socket/i.test(t)) return "network";
  return "other";
}

function attributeFailure(row, probes) {
  // thinking 场景的失败分两种:带上游错误文本的先按错误分类(typical:pi 对
  // reasoning 模型发 developer role 被上游 400),只有"回复正常但没有思考块"
  // 才是 thinking-unreported——上游没回 reasoning 项(如 volcengine
  // /v1/responses 带 system 输入时不返回 summary)。
  if (row.scenario === "thinking" && /thinkingBlock=false/.test(String(row.detail)) && !row.error) {
    return { cause: "thinking-unreported", verdictNote: "" };
  }
  const cause = classifyFailure(`${row.detail} ${row.error ?? ""}`);
  // verdict 只作诊断上下文,不构成"预期内失败":runtime 会按 verdict 把请求
  // 转换到可用腿,客户端协议腿的 no 解释不了失败(转换层自己的失败恰恰是
  // 要暴露的 bug)。仅当错误文本显示请求确实打进了判 no 的腿时才算违例。
  const leg = LEG_OF_PROTOCOL[row.protocol];
  const notes = [];
  let violation = false;
  // model_caps.json 的 models 键是上游裸 id:alias 重命名模型要用
  // row.upstream 查,否则 verdict 关联不上。
  const mv = probes.modelCaps[row.provider]?.[row.upstream ?? row.model]?.[leg];
  if (mv === "no") {
    notes.push(`model_caps 判 ${leg}=no`);
    if (/404|not found in routes/i.test(`${row.detail} ${row.error ?? ""}`)) violation = true;
  }
  if (leg === "chat" || leg === "responses") {
    const wv = probes.wireCaps[row.provider]?.[leg];
    if (wv === "no") notes.push(`wire_caps 判 ${leg}=no`);
  }
  return { cause, verdictNote: notes.join("; "), violation };
}

// ---------------------------------------------------------------------------
// 执行:worker 池并发跨 (provider, model) 对,对内场景串行
// ---------------------------------------------------------------------------
async function runOne(opts, probes, t, protocol, thinking, scenario) {
  const row = {
    provider: t.provider, model: t.model, upstream: t.upstream ?? t.model, protocol, thinking,
    scenario: scenario.name, status: "FAIL", detail: "", ms: 0, error: null,
  };
  // thinking 场景只在非 off 档有意义;非 off 档要求目录判定 reasoning
  if (scenario.needsThinking && thinking === "off") {
    row.status = "SKIP";
    row.detail = "thinking 场景需要非 off 档";
    return row;
  }
  if (thinking !== "off" && !t.reasoning) {
    row.status = "SKIP";
    row.detail = "目录判定非 reasoning 模型";
    return row;
  }
  if (scenario.needsVision && !t.vision) {
    row.status = "SKIP";
    row.detail = "目录判定无 image 输入";
    return row;
  }
  const cfg = {
    protocol,
    model: t.model,
    thinking,
    tools: !!scenario.needsTools,
    vision: !!scenario.needsVision,
  };
  const agent = makeAgent({ ...opts, forceProvider: opts.forceProvider ?? t.provider }, cfg);
  const nonce = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
  const start = Date.now();
  let res;
  try {
    res = await withTimeout(scenario.run(agent, nonce), opts.scenarioTimeoutMs);
  } catch (e) {
    row.ms = Date.now() - start;
    row.detail = `error: ${String(e.message ?? e).slice(0, 160)}`;
    row.error = String(e.message ?? e);
    return row;
  }
  row.ms = Date.now() - start;
  if (res.skip) {
    row.status = "SKIP";
    row.detail = res.detail;
    return row;
  }
  const r = res.extra ?? {};
  if (r.error) {
    row.status = "FAIL";
    row.error = String(r.error);
    row.detail = `${res.detail} | stop=${r.stopReason} err=${String(r.error).slice(0, 160)}`;
    return row;
  }
  row.status = res.pass ? "PASS" : "FAIL";
  row.detail = res.detail;
  return row;
}

async function runSweep(opts, targets, probes) {
  const scenarios = SCENARIOS.filter((s) => opts.scenarios.includes(s.name));
  const queue = [];
  for (const t of targets) {
    for (const protocol of opts.protocols) {
      for (const thinking of opts.thinking) {
        for (const sc of scenarios) {
          queue.push({ t, protocol, thinking, scenario: sc });
        }
      }
    }
  }
  process.stdout.write(`${DIM}${queue.length} 个组合(目标 ${targets.length} × 协议 ${opts.protocols.length} × thinking ${opts.thinking.length} × 场景 ${scenarios.length}),并发 ${opts.concurrency}${RESET}\n\n`);

  const results = [];
  let cursor = 0;
  let lastLabel = "";
  const worker = async () => {
    while (cursor < queue.length) {
      const job = queue[cursor++];
      const { t, protocol, thinking, scenario } = job;
      const label = `${t.provider}/${t.model} [${protocol}${thinking !== "off" ? ` th=${thinking}` : ""}] ${scenario.name}`;
      if (label !== lastLabel) {
        process.stdout.write(`${DIM}▶ ${label}${RESET}\n`);
        lastLabel = label;
      }
      const row = await runOne(opts, probes, t, protocol, thinking, scenario);
      results.push(row);
      const v = row.status === "SKIP" ? YELLOW + "SKIP" : row.status === "PASS" ? GREEN + "PASS" : RED + "FAIL";
      process.stdout.write(`  ${v}${RESET} ${(row.ms / 1000).toFixed(1)}s ${DIM}${row.detail.slice(0, 120)}${RESET}\n`);
    }
  };
  await Promise.all(Array.from({ length: Math.min(opts.concurrency, queue.length) }, worker));
  return results;
}

// ---------------------------------------------------------------------------
// 报告:结果表 + 失败分析
// ---------------------------------------------------------------------------
function report(opts, targets, results, probes) {
  const pass = results.filter((r) => r.status === "PASS");
  const fail = results.filter((r) => r.status === "FAIL");
  const skip = results.filter((r) => r.status === "SKIP");

  process.stdout.write(`\n${BOLD}═══ 结果汇总 ═══${RESET}\n`);
  process.stdout.write(`总 ${results.length}:PASS ${pass.length},FAIL ${fail.length},SKIP ${skip.length}\n\n`);

  // 每 (provider, model) × 协议 一行
  const byPair = new Map();
  for (const r of results) {
    const k = `${r.provider}/${r.model}`;
    if (!byPair.has(k)) byPair.set(k, {});
    const cell = byPair.get(k);
    const ck = `${r.protocol}${r.thinking !== "off" ? `+th` : ""}`;
    cell[ck] = cell[ck] ?? { pass: 0, fail: 0, skip: 0 };
    cell[ck][r.status.toLowerCase()]++;
  }
  for (const [pair, cells] of byPair) {
    const parts = Object.entries(cells).map(([k, c]) => {
      const total = c.pass + c.fail + c.skip;
      const mark = c.fail === 0 ? GREEN + "ok" : c.pass === 0 ? RED + "xx" : YELLOW + "~-";
      return `${k}:${mark}${RESET}(${c.pass}/${total}${c.skip ? `,skip${c.skip}` : ""})`;
    });
    process.stdout.write(`  ${pair.padEnd(36)} ${parts.join("  ")}\n`);
  }

  // 失败分析:所有失败一律列出,按成因聚类;verdict 只作诊断上下文,不构
  // 成"预期内"——runtime 会按 verdict 把请求转换到可用腿,失败本身就说明
  // 转换层/上游有问题,正是要暴露的。
  process.stdout.write(`\n${BOLD}═══ 失败分析 ═══${RESET}\n`);
  for (const r of fail) {
    const { cause, verdictNote, violation } = attributeFailure(r, probes);
    r.cause = cause;
    r.verdictNote = verdictNote;
    r.legViolation = violation;
  }
  const byCause = new Map();
  for (const r of fail) {
    if (!byCause.has(r.cause)) byCause.set(r.cause, []);
    byCause.get(r.cause).push(r);
  }
  for (const [cause, rows] of byCause) {
    process.stdout.write(`${RED}\n[${cause}] ×${rows.length}${RESET}${DIM} — ${[...new Set(rows.map((r) => r.provider))].join(",")}${RESET}\n`);
    for (const r of rows) {
      const ctx = r.legViolation ? ` ${YELLOW}[请求打进了判 no 的腿——选择违例]${RESET}` : r.verdictNote ? ` ${DIM}[${r.verdictNote}]${RESET}` : "";
      process.stdout.write(`  ${r.provider}/${r.model} ${r.protocol}${r.thinking !== "off" ? ` th=${r.thinking}` : ""} ${r.scenario}${ctx}\n    ${DIM}${r.detail.slice(0, 200)}${RESET}\n`);
    }
  }
  if (skip.length) {
    const byReason = new Map();
    for (const r of skip) {
      const k = r.detail;
      byReason.set(k, (byReason.get(k) ?? 0) + 1);
    }
    process.stdout.write(`${DIM}\nSKIP ${skip.length}:${RESET}\n`);
    for (const [reason, n] of byReason) process.stdout.write(`  ×${n} — ${reason}\n`);
  }

  // 结论
  process.stdout.write(`\n${BOLD}结论:${RESET}`);
  if (fail.length === 0) {
    process.stdout.write(`${GREEN}全部通过,无失败${RESET}\n`);
  } else {
    const violations = fail.filter((r) => r.legViolation);
    const pairs = [...new Set(fail.map((r) => `${r.provider}/${r.model}@${r.protocol}`))];
    process.stdout.write(`${RED}${fail.length} 个失败,涉及 ${pairs.length} 个 provider/model@协议${violations.length ? `;其中 ${violations.length} 个请求打进了探测判 no 的腿(选择违例)` : ""}${RESET}\n`);
  }

  fs.writeFileSync(opts.report, JSON.stringify({
    startedAt: new Date().toISOString(),
    target: `http://${opts.host}:${opts.port}`,
    config: opts.config,
    totals: { pass: pass.length, fail: fail.length, skip: skip.length },
    results,
  }, null, 2));
  process.stdout.write(`${DIM}报告已写入 ${opts.report}${RESET}\n`);
  return fail.length === 0 ? 0 : 1;
}

// ---------------------------------------------------------------------------
async function main() {
  let opts;
  try {
    opts = parseArgs(process.argv.slice(2));
  } catch (e) {
    process.stderr.write(`✗ ${e.message}\n`);
    process.exit(2);
  }
  let targets;
  try {
    targets = discoverTargets(opts);
  } catch (e) {
    process.stderr.write(`✗ 发现目标失败: ${e.message}\n`);
    process.exit(2);
  }
  if (targets.length === 0) {
    process.stderr.write("✗ 没有匹配的 provider/model 目标\n");
    process.exit(2);
  }
  const probes = loadProbeVerdicts(opts);
  const results = await runSweep(opts, targets, probes);
  const code = report(opts, targets, results, probes);
  process.exit(code);
}

main().catch((e) => {
  process.stderr.write(`✗ ${e.stack ?? e.message}\n`);
  process.exit(1);
});
