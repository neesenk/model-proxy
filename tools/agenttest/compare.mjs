#!/usr/bin/env node
/**
 * compare — 同一模型「provider 原生协议」vs「经 proxy 转换的协议」的编码
 * 任务质量对比:找出转换层在实现上引入的问题。
 *
 * 方法:单次采样有随机性(slugify 这类任务一次写错很常见),所以:
 *   1. 从 model_caps.json(~/.model-proxy/model_caps.json)读每个
 *      (provider, model) 的原生协议(verdict=yes 的腿);
 *   2. 对每个可疑组合做多次复测:原生协议 ×2(能力基线)+ 每个转换协议 ×3;
 *   3. 判定:转换 0/3 且原生 ≥1/2 → 强转换问题信号;转换有通过 → 首跑
 *      噪声;两边都挂 → 模型能力不足。
 *   失败按类别分桶:impl-error(模型实现错,两路径等概率)/ chain-broken
 *   (工具链路断裂:没写文件没读 SPEC——转换层强信号)/ upstream / timeout。
 *
 * 用法:
 *   node compare.mjs                          # 自动:重读 codews-last.json 的
 *                                             # native-pass + converted-fail 组合
 *   node compare.mjs --providers zhipu --models glm-5.2   # 指定目标(跑其全部协议)
 *   node compare.mjs --repeats 3 --native-repeats 2
 *
 * 退出码:0 = 无转换问题信号;1 = 存在;2 = 用法错误。
 */

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { PROTOCOL_NAMES, discoverTargets } from "./lib/core.mjs";
import { runCodeTask, classifyTaskFailure } from "./lib/codetask.mjs";

const DIM = "\x1b[2m";
const BOLD = "\x1b[1m";
const RED = "\x1b[31m";
const GREEN = "\x1b[32m";
const YELLOW = "\x1b[33m";
const RESET = "\x1b[0m";

const TOOL_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_DIR = path.resolve(TOOL_DIR, "../..");

function parseArgs(argv) {
  const opts = {
    config: path.join(REPO_DIR, "config.yaml"),
    dumpDir: null,
    host: "127.0.0.1",
    port: 15722,
    providers: null,
    models: null,
    repeats: 3,
    nativeRepeats: 2,
    taskTimeoutMs: 420000,
    report: path.join(TOOL_DIR, "compare-last.json"),
    home: os.homedir(),
    workDir: path.join(TOOL_DIR, ".codews-work"),
    prior: path.join(TOOL_DIR, "codews-last.json"),
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
      case "--repeats": opts.repeats = Math.max(1, Number(next())); break;
      case "--native-repeats": opts.nativeRepeats = Math.max(1, Number(next())); break;
      case "--task-timeout": opts.taskTimeoutMs = Number(next()); break;
      case "--report": opts.report = next(); break;
      case "--dump-dir": opts.dumpDir = next(); break;
      default: throw new Error(`未知参数 ${a}`);
    }
  }
  return opts;
}

// model_caps.json → (provider, model) 的原生协议集合。
function loadNativeProtos(opts) {
  const capsPath = path.join(opts.home, ".model-proxy", "model_caps.json");
  const caps = JSON.parse(fs.readFileSync(capsPath, "utf8")).providers ?? {};
  const native = new Map();
  for (const [prov, entry] of Object.entries(caps)) {
    for (const [model, legs] of Object.entries(entry.models ?? {})) {
      const protos = [];
      if (legs.anthropic === "yes") protos.push("anthropic");
      if (legs.chat === "yes") protos.push("chat");
      if (legs.responses === "yes") protos.push("responses");
      native.set(`${prov}\u0000${model}`, protos);
    }
  }
  return native;
}

// 可疑组合:prior run 里 native PASS 且存在 converted FAIL。
// model_caps.json 的 models 键是上游裸 id,而 codews-last.json 记的 model
// 是暴露名(alias 重命名后);先经 discoverTargets 建 暴露名→上游id 映射再查。
function suspectsFromPrior(opts, native) {
  const prior = JSON.parse(fs.readFileSync(opts.prior, "utf8")).results ?? [];
  const exposedToUpstream = new Map();
  try {
    for (const t of discoverTargets(opts)) {
      exposedToUpstream.set(`${t.provider}\u0000${t.model}`, t.upstream ?? t.model);
    }
  } catch { /* config 不可读时退回按暴露名直查 */ }
  const bym = new Map();
  for (const r of prior) {
    const k = `${r.provider}\u0000${r.model}`;
    if (!bym.has(k)) bym.set(k, {});
    bym.get(k)[r.protocol] = r.status === "PASS";
  }
  const out = [];
  for (const [k, cells] of bym) {
    const [provider, model] = k.split("\u0000");
    const upstream = exposedToUpstream.get(k) ?? model;
    const nats = native.get(`${provider}\u0000${upstream}`) ?? [];
    if (!nats.length) continue;
    if (!nats.some((n) => cells[n])) continue; // 原生都没过:能力问题,不比
    const convFails = PROTOCOL_NAMES.filter((p) => p in cells && !nats.includes(p) && !cells[p]);
    if (convFails.length) out.push({ provider, model, nats, convFails });
  }
  return out;
}

async function main() {
  let opts;
  try {
    opts = parseArgs(process.argv.slice(2));
  } catch (e) {
    process.stderr.write(`✗ ${e.message}\n`);
    process.exit(2);
  }
  let native;
  try {
    native = loadNativeProtos(opts);
  } catch (e) {
    process.stderr.write(`✗ 读 model_caps.json 失败: ${e.message}\n`);
    process.exit(2);
  }

  let suspects;
  if (opts.providers || opts.models) {
    const targets = discoverTargets(opts);
    suspects = targets
      .map((t) => {
        // model_caps.json 的 models 键是上游裸 id:alias 重命名模型要用
        // t.upstream 查,否则 nats 为空被过滤掉。
        const nats = native.get(`${t.provider}\u0000${t.upstream ?? t.model}`) ?? [];
        const convFails = nats.length ? PROTOCOL_NAMES.filter((p) => !nats.includes(p)) : [];
        return { provider: t.provider, model: t.model, nats, convFails };
      })
      .filter((s) => s.nats.length > 0);
  } else {
    try {
      suspects = suspectsFromPrior(opts, native);
    } catch (e) {
      process.stderr.write(`✗ 读 ${opts.prior} 失败(先跑 codews.mjs 或用 --providers 指定): ${e.message}\n`);
      process.exit(2);
    }
  }
  if (!suspects.length) {
    process.stdout.write("没有可比组合(需要原生协议有通过记录)\n");
    process.exit(0);
  }

  const jobs = [];
  for (const s of suspects) {
    for (const n of s.nats.slice(0, 1)) {
      for (let i = 1; i <= opts.nativeRepeats; i++) jobs.push({ s, protocol: n, kind: "native", i });
    }
    for (const p of s.convFails) {
      for (let i = 1; i <= opts.repeats; i++) jobs.push({ s, protocol: p, kind: "converted", i });
    }
  }
  process.stdout.write(
    `${DIM}${suspects.length} 个模型 × ${jobs.length} 次任务(原生 ×${opts.nativeRepeats} 对照 + 转换 ×${opts.repeats})${RESET}\n\n`,
  );

  const rows = [];
  let cursor = 0;
  const worker = async () => {
    while (cursor < jobs.length) {
      const job = jobs[cursor++];
      const label = `${job.s.provider}/${job.s.model} ${job.protocol}(${job.kind})#${job.i}`;
      process.stdout.write(`${DIM}▶ ${label}${RESET}\n`);
      const row = await runCodeTask(opts, job.s, job.protocol, `-${job.kind}${job.i}`);
      row.kind = job.kind;
      row.failureClass = row.status === "PASS" ? null : classifyTaskFailure(row);
      rows.push(row);
      const v = row.status === "PASS" ? GREEN + "PASS" : RED + "FAIL";
      const fc = row.failureClass ? YELLOW + ` [${row.failureClass}]` : "";
      process.stdout.write(`  ${v}${RESET}${fc} ${(row.ms / 1000).toFixed(0)}s ${DIM}${row.detail.slice(0, 120)}${RESET}\n`);
    }
  };
  await Promise.all(Array.from({ length: Math.min(3, jobs.length) }, worker));

  // 汇总
  process.stdout.write(`\n${BOLD}═══ 原生 vs 转换对比 ═══${RESET}\n`);
  const signals = [];
  const byTarget = new Map();
  for (const r of rows) {
    const k = `${r.provider}/${r.model}`;
    if (!byTarget.has(k)) byTarget.set(k, {});
    byTarget.get(k)[`${r.protocol}:${r.kind}`] = (byTarget.get(k)[`${r.protocol}:${r.kind}`] ?? 0) + (r.status === "PASS" ? 1 : 0);
    // count map needs totals too
  }
  const totals = new Map();
  for (const r of rows) {
    const k = `${r.provider}/${r.model}`;
    if (!totals.has(k)) totals.set(k, {});
    const cell = `${r.protocol}:${r.kind}`;
    totals.get(k)[cell] = (totals.get(k)[cell] ?? 0) + 1;
  }
  for (const [k, passed] of byTarget) {
    const t = totals.get(k);
    const parts = Object.keys(passed).map((cell) => {
      const [proto, kind] = cell.split(":");
      const mark = passed[cell] === 0 ? RED : passed[cell] === t[cell] ? GREEN : YELLOW;
      return `${proto}(${kind}):${mark}${passed[cell]}/${t[cell]}${RESET}`;
    });
    process.stdout.write(`  ${k.padEnd(36)} ${parts.join("  ")}\n`);
    const s = suspects.find((x) => `${x.provider}/${x.model}` === k);
    for (const conv of s?.convFails ?? []) {
      const convPass = passed[`${conv}:converted`] ?? 0;
      const natPass = Object.entries(passed)
        .filter(([cell]) => cell.endsWith(":native"))
        .reduce((a, [, v]) => a + v, 0);
      if (convPass === 0 && natPass >= 1) {
        const convRows = rows.filter((r) => r.provider + "/" + r.model === k && r.protocol === conv && r.kind === "converted");
        const classes = [...new Set(convRows.map((r) => r.failureClass))];
        signals.push({ target: k, protocol: conv, classes });
      }
    }
  }

  process.stdout.write(`\n${BOLD}结论:${RESET}`);
  if (!signals.length) {
    process.stdout.write(`${GREEN}无转换问题信号:所有转换协议都有通过样本,首跑失败为采样噪声或模型能力差异${RESET}\n`);
  } else {
    process.stdout.write(`${RED}${signals.length} 个强信号(转换 0 通过而原生有通过):${RESET}\n`);
    for (const sig of signals) {
      process.stdout.write(`  ${sig.target} @${sig.protocol} — 失败类型 ${sig.classes.join(",")}${RESET}\n`);
    }
  }

  fs.writeFileSync(opts.report, JSON.stringify({ startedAt: new Date().toISOString(), signals, results: rows }, null, 2));
  process.stdout.write(`${DIM}报告已写入 ${opts.report}${RESET}\n`);
  process.exit(signals.length ? 1 : 0);
}

main().catch((e) => {
  process.stderr.write(`✗ ${e.stack ?? e.message}\n`);
  process.exit(1);
});
