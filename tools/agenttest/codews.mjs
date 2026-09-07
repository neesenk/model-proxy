#!/usr/bin/env node
/**
 * codews — 对 config 全部 provider × model 跑一个真实的"代码实现工程":
 * 读 SPEC → 实现 strutil.js(write_file)→ 读 input.txt 写 output.txt,
 * harness 在工程副本里真实运行 node:test 功能测试并逐字节比对产物。
 *
 * 复用 agenttest 共享核心(lib/core.mjs):同一 pi agent loop、
 * read_file/write_file 工具、force-provider 钉死;发现逻辑与 sweep 共用。
 *
 * 用法:
 *   node codews.mjs                                # 全 provider/model × 3 协议
 *   node codews.mjs --providers zhipu --models glm-5.3
 *   node codews.mjs --protocols anthropic          # 只测一种 ingress
 *
 * 判定(全部 harness 侧,不信模型自评):
 *   1. `node --test test/` 在工程副本真实执行,exit 0;
 *   2. output.txt 与期望逐字节一致;
 *   3. agent 回复 DONE。
 *
 * 报告:逐行结果 + provider/model 汇总表,明细落 codews-last.json;
 * 工程副本保留在 .codews-work/ 供检查(--clean 清理)。
 * 退出码:0 = 全过;1 = 有失败;2 = 用法/环境错误。
 *
 * 前置: model-proxy serve 在跑(默认 127.0.0.1:15722),已 login。
 */

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  PROTOCOLS,
  PROTOCOL_NAMES,
  discoverTargets,
} from "./lib/core.mjs";
import { runCodeTask } from "./lib/codetask.mjs";

const DIM = "\x1b[2m";
const BOLD = "\x1b[1m";
const RED = "\x1b[31m";
const GREEN = "\x1b[32m";
const YELLOW = "\x1b[33m";
const RESET = "\x1b[0m";

const TOOL_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_DIR = path.resolve(TOOL_DIR, "../..");
const WORK_DIR = path.join(TOOL_DIR, ".codews-work");

function parseArgs(argv) {
  const opts = {
    config: path.join(REPO_DIR, "config.yaml"),
    host: "127.0.0.1",
    port: 15722,
    providers: null,
    models: null,
    protocols: [...PROTOCOL_NAMES],
    concurrency: 3,
    taskTimeoutMs: 420000,
    report: path.join(TOOL_DIR, "codews-last.json"),
    home: os.homedir(),
    clean: false,
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
      case "--concurrency": opts.concurrency = Math.max(1, Number(next())); break;
      case "--task-timeout": opts.taskTimeoutMs = Number(next()); break;
      case "--report": opts.report = next(); break;
      case "--clean": opts.clean = true; break;
      default: throw new Error(`未知参数 ${a}`);
    }
  }
  return opts;
}

// 跑一个组合:工程副本 → agent 任务 → 真实验证(共享实现,含 usage/产物采集)。
async function runOne(opts, t, protocol) {
  const row = await runCodeTask({ ...opts, workDir: WORK_DIR }, t, protocol, "");
  row.workspace = path.relative(REPO_DIR, row.workspace);
  return row;
}

function report(opts, results) {
  const pass = results.filter((r) => r.status === "PASS");
  const fail = results.filter((r) => r.status === "FAIL");

  process.stdout.write(`\n${BOLD}═══ 结果汇总 ═══${RESET}\n`);
  process.stdout.write(`总 ${results.length}:PASS ${pass.length},FAIL ${fail.length}\n\n`);

  const byPair = new Map();
  for (const r of results) {
    const k = `${r.provider}/${r.model}`;
    if (!byPair.has(k)) byPair.set(k, {});
    const cells = byPair.get(k);
    cells[r.protocol] = r.status === "PASS" ? "PASS" : "FAIL";
  }
  for (const [pair, cells] of byPair) {
    const parts = PROTOCOL_NAMES.filter((p) => cells[p]).map((p) => {
      const mark = cells[p] === "PASS" ? GREEN + "ok" : RED + "xx";
      return `${p}:${mark}${RESET}`;
    });
    const allPass = PROTOCOL_NAMES.filter((p) => cells[p]).every((p) => cells[p] === "PASS");
    process.stdout.write(`  ${allPass ? GREEN : RED}${pair.padEnd(36)}${RESET} ${parts.join("  ")}\n`);
  }

  if (fail.length) {
    process.stdout.write(`\n${BOLD}失败明细:${RESET}\n`);
    for (const r of fail) {
      process.stdout.write(`  ${RED}${r.provider}/${r.model} [${r.protocol}]${RESET} ${(r.ms / 1000).toFixed(0)}s\n    ${DIM}${r.detail}${RESET}\n`);
    }
  }

  process.stdout.write(`\n${BOLD}结论:${RESET}`);
  if (fail.length === 0) {
    process.stdout.write(`${GREEN}全部通过:每个组合都完成了真实文件读写且功能测试全绿${RESET}\n`);
  } else {
    const pairs = [...new Set(fail.map((r) => `${r.provider}/${r.model}@${r.protocol}`))];
    process.stdout.write(`${RED}${fail.length} 个失败,涉及 ${pairs.length} 个组合${RESET}\n`);
  }

  fs.writeFileSync(opts.report, JSON.stringify({
    startedAt: new Date().toISOString(),
    target: `http://${opts.host}:${opts.port}`,
    task: "strutil 实现 + input.txt→output.txt 读写往返 + node:test 功能测试",
    totals: { pass: pass.length, fail: fail.length },
    results,
  }, null, 2));
  process.stdout.write(`${DIM}报告已写入 ${opts.report};工程副本在 ${path.relative(REPO_DIR, WORK_DIR)}/${RESET}\n`);
  return fail.length === 0 ? 0 : 1;
}

async function main() {
  let opts;
  try {
    opts = parseArgs(process.argv.slice(2));
  } catch (e) {
    process.stderr.write(`✗ ${e.message}\n`);
    process.exit(2);
  }
  if (opts.clean) {
    fs.rmSync(WORK_DIR, { recursive: true, force: true });
    process.stdout.write("已清理工程副本目录\n");
    // --clean alone is a maintenance invocation — do NOT fall through into
    // a full billed sweep. Combine with --providers/--models to run after
    // cleaning.
    if (!opts.providers && !opts.models) process.exit(0);
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

  const jobs = [];
  for (const t of targets) {
    for (const protocol of opts.protocols) {
      jobs.push({ t, protocol });
    }
  }
  process.stdout.write(
    `${DIM}${jobs.length} 个组合(目标 ${targets.length} × 协议 ${opts.protocols.length}),` +
    `每组合一次多轮真实编码任务(读 SPEC → 实现 → 读写往返),并发 ${opts.concurrency}${RESET}\n\n`,
  );

  const results = [];
  let cursor = 0;
  const worker = async () => {
    while (cursor < jobs.length) {
      const job = jobs[cursor++];
      const { t, protocol } = job;
      process.stdout.write(`${DIM}▶ ${t.provider}/${t.model} [${protocol}]${RESET}\n`);
      const row = await runOne(opts, t, protocol);
      results.push(row);
      const v = row.status === "PASS" ? GREEN + "PASS" : RED + "FAIL";
      process.stdout.write(`  ${v}${RESET} ${(row.ms / 1000).toFixed(0)}s ${DIM}${row.detail.slice(0, 140)}${RESET}\n`);
    }
  };
  await Promise.all(Array.from({ length: Math.min(opts.concurrency, jobs.length) }, worker));
  const code = report(opts, results);
  process.exit(code);
}

main().catch((e) => {
  process.stderr.write(`✗ ${e.stack ?? e.message}\n`);
  process.exit(1);
});
