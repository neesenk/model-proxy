#!/usr/bin/env node
/**
 * e2e — model-proxy 端到端整合测试 runner。
 *
 * 一个命令串起整条验证链:
 *   1. preflight: 代理可达性 + 运行日志/内部数据快照
 *   2. projects/: 逐个真实 agent 项目 × 协议 × 模型跑真实任务(可插拔,见 README)
 *   3. matrix: agenttest.mjs 的协议转换批量自检(可 --skip-matrix)
 *   4. 后端分析: 运行日志新行(每请求 proto/provider/status/延迟)、
 *      /api/status 计数器 delta、/api/requests(若 request_log 开启)
 *   5. 汇总判定 + 退出码
 *
 * 用法:
 *   node e2e.mjs                                   # 全量:所有项目 × 3 协议 + matrix
 *   node e2e.mjs --protocols anthropic --models claude-haiku-4-5 --skip-matrix
 *   node e2e.mjs --projects pi-coding --with-thinking
 */

import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const DIM = "\x1b[2m";
const BOLD = "\x1b[1m";
const RED = "\x1b[31m";
const GREEN = "\x1b[32m";
const YELLOW = "\x1b[33m";
const CYAN = "\x1b[36m";
const RESET = "\x1b[0m";

const AGENTTEST_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(AGENTTEST_DIR, "../..");
const PROJECTS_DIR = path.join(AGENTTEST_DIR, "projects");
const PROTOCOL_NAMES = ["anthropic", "chat", "responses"];

const out = (s = "") => process.stdout.write(s + "\n");
const section = (s) => out(`\n${BOLD}== ${s} ==${RESET}`);

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------
function parseArgs(argv) {
  const o = {
    host: "127.0.0.1",
    port: 15722,
    models: ["claude-haiku-4-5"],
    protocols: [...PROTOCOL_NAMES],
    projects: null,
    skipMatrix: false,
    withThinking: false,
    withVision: false,
    forceProvider: null,
    logFile: null,
    config: path.join(REPO_ROOT, "config.yaml"),
    keepWorkspace: false,
  };
  for (let i = 2; i < argv.length; i++) {
    const next = () => argv[++i];
    switch (argv[i]) {
      case "--host": o.host = next(); break;
      case "--port": o.port = Number(next()); break;
      case "--models": o.models = next().split(",").map((s) => s.trim()); break;
      case "--protocols": o.protocols = next().split(",").map((s) => s.trim()); break;
      case "--projects": o.projects = next().split(",").map((s) => s.trim()); break;
      case "--skip-matrix": o.skipMatrix = true; break;
      case "--with-thinking": o.withThinking = true; break;
      case "--with-vision": o.withVision = true; break;
      case "--force-provider": o.forceProvider = next(); break;
      case "--log-file": o.logFile = next(); break;
      case "--config": o.config = next(); break;
      case "--keep-workspace": o.keepWorkspace = true; break;
      case "-h":
      case "--help":
        out(`用法: node e2e.mjs [选项]
  --host/--port       代理地址(默认 127.0.0.1:15722)
  --models a,b        模型/路由别名(默认 claude-haiku-4-5)
  --protocols a,b     ${PROTOCOL_NAMES.join("|")}(默认全跑)
  --projects a,b      只跑指定项目(默认 projects/ 下全部)
  --skip-matrix       跳过 agenttest matrix
  --with-thinking     matrix 加跑 thinking 场景
  --with-vision       matrix 加跑 vision 场景 + 放开 requires:["vision"] 的项目
  --force-provider P  matrix 钉死后端 provider(x-mp-force-provider)
  --log-file PATH     代理运行日志(默认: config log_file > $TMPDIR/model-proxy.log)
  --config PATH       model-proxy config(用于解析 log_file,默认仓库根 config.yaml)
  --keep-workspace    不清理项目 workspace(调试用)
`);
        process.exit(0);
      default:
        process.stderr.write(`未知参数 ${argv[i]}\n`);
        process.exit(2);
    }
  }
  for (const p of o.protocols) {
    if (!PROTOCOL_NAMES.includes(p)) {
      process.stderr.write(`未知协议 ${p}\n`);
      process.exit(2);
    }
  }
  return o;
}

// ---------------------------------------------------------------------------
// 代理侧数据访问
// ---------------------------------------------------------------------------
function origin(o) {
  return `http://${o.host}:${o.port}`;
}

async function apiGet(o, p) {
  const resp = await fetch(`${origin(o)}${p}`, { signal: AbortSignal.timeout(5000) });
  if (!resp.ok) throw new Error(`GET ${p} → HTTP ${resp.status}`);
  return resp.json();
}

function resolveLogFile(o) {
  if (o.logFile) return o.logFile;
  try {
    const text = fs.readFileSync(o.config, "utf8");
    const m = text.match(/^log_file:\s*(\S+)\s*$/m);
    if (m) return m[1].replace(/^~/, process.env.HOME ?? "~");
  } catch { /* config 不可读就用默认 */ }
  const tmp = process.env.TMPDIR || "/tmp";
  return path.join(tmp, "model-proxy.log");
}

// ---------------------------------------------------------------------------
// 项目发现与执行(可插拔契约见 README)
// ---------------------------------------------------------------------------
function discoverProjects(o) {
  const dirs = fs.readdirSync(PROJECTS_DIR, { withFileTypes: true })
    .filter((d) => d.isDirectory())
    .map((d) => d.name)
    .filter((n) => fs.existsSync(path.join(PROJECTS_DIR, n, "project.json")))
    .sort();
  const selected = o.projects ?? dirs;
  const missing = selected.filter((n) => !dirs.includes(n));
  if (missing.length) throw new Error(`项目不存在: ${missing.join(", ")} (可选: ${dirs.join(", ") || "无"})`);
  return selected.map((n) => ({
    name: n,
    dir: path.join(PROJECTS_DIR, n),
    manifest: JSON.parse(fs.readFileSync(path.join(PROJECTS_DIR, n, "project.json"), "utf8")),
  }));
}

function renderTemplate(s, vars) {
  return s.replace(/\{\{(\w+)\}\}/g, (_, k) => {
    if (!(k in vars)) throw new Error(`未知模板变量 {{${k}}}`);
    return vars[k];
  });
}

function runCommand(argv, { cwd, env, timeoutMs, logFile }) {
  return new Promise((resolve) => {
    const logStream = fs.createWriteStream(logFile);
    const child = spawn(argv[0], argv.slice(1), {
      cwd,
      env: { ...process.env, ...env },
      // stdin 置 ignore:子进程若误等交互输入(trust/approval 提示)会立即拿到 EOF,
      // 而不是在永不关闭的 pipe 上挂死
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    const timer = setTimeout(() => {
      child.kill("SIGTERM");
      setTimeout(() => child.kill("SIGKILL"), 5000).unref();
    }, timeoutMs);
    child.stdout.on("data", (d) => {
      stdout += d;
      logStream.write(d);
    });
    child.stderr.on("data", (d) => logStream.write(d));
    child.on("error", (e) => {
      clearTimeout(timer);
      logStream.end();
      resolve({ code: -1, stdout, error: e.message });
    });
    child.on("close", (code, signal) => {
      clearTimeout(timer);
      logStream.end();
      resolve({ code: code ?? -1, signal, stdout });
    });
  });
}

async function runProject(project, o, protocol, model) {
  const { manifest } = project;
  const wsName = `${protocol}--${model}`;
  const workspace = path.join(project.dir, "workspace", wsName);
  if (!o.keepWorkspace) fs.rmSync(workspace, { recursive: true, force: true });
  fs.mkdirSync(workspace, { recursive: true });

  const prompt = manifest.taskFile
    ? fs.readFileSync(path.join(project.dir, manifest.taskFile), "utf8")
    : manifest.task ?? "";
  const vars = {
    node: process.execPath,
    agenttest: AGENTTEST_DIR,
    project: project.dir,
    workspace,
    protocol,
    model,
    origin: origin(o),
    prompt: prompt.trim(),
  };
  const ctx = { projectDir: project.dir, workspace, protocol, model, origin: origin(o), log: (m) => out(`${DIM}  [setup] ${m}${RESET}`) };

  const setupPath = path.join(project.dir, "setup.mjs");
  if (fs.existsSync(setupPath)) {
    const setup = (await import(pathToFileURL(setupPath))).default;
    await setup(ctx);
  }

  const argv = manifest.command.map((a) => renderTemplate(a, vars));
  const env = Object.fromEntries(Object.entries(manifest.env ?? {}).map(([k, v]) => [k, renderTemplate(v, vars)]));
  const timeoutMs = (manifest.timeoutSec ?? 300) * 1000;

  const result = await runCommand(argv, {
    cwd: workspace,
    env,
    timeoutMs,
    logFile: path.join(workspace, "run.log"),
  });

  const failures = [];
  if (result.code !== 0) failures.push(`exit=${result.code}${result.signal ? ` signal=${result.signal}` : ""}`);
  for (const f of manifest.assert?.files ?? []) {
    const p = path.join(workspace, f.path);
    if (!fs.existsSync(p)) {
      failures.push(`缺文件 ${f.path}`);
    } else if (f.match && !new RegExp(f.match, "m").test(fs.readFileSync(p, "utf8"))) {
      failures.push(`${f.path} 不匹配 /${f.match}/`);
    }
  }
  if (manifest.assert?.stdoutMatch && !new RegExp(manifest.assert.stdoutMatch, "im").test(result.stdout)) {
    failures.push(`stdout 不含 /${manifest.assert.stdoutMatch}/`);
  }
  return { failures, stdout: result.stdout, workspace };
}

// ---------------------------------------------------------------------------
// 后端日志与内部数据分析
// ---------------------------------------------------------------------------
// 运行日志每请求一行:
//   2026/09/03 21:21:44.315965 [proto=responses provider=aqp] POST /v1/responses model=a→b status=200 839ms bytes=1078
const REQ_LINE = /\[proto=(\S+) provider=(\S+)\] (\w+) (\S+) model=(\S+) status=(\d+) (\d+)ms/;
const SUSPICIOUS = /panic|FAILED|level=error|\bfatal\b/i;

function analyzeProxy(o, snapshot, modelSet) {
  const report = { failures: [], warnings: [], lines: [] };

  // --- 运行日志新增内容 ---
  let newLog = "";
  try {
    const size = fs.statSync(snapshot.logFile).size;
    const fd = fs.openSync(snapshot.logFile, "r");
    const buf = Buffer.alloc(Math.max(0, size - snapshot.logOffset));
    fs.readSync(fd, buf, 0, buf.length, snapshot.logOffset);
    fs.closeSync(fd);
    newLog = buf.toString("utf8");
  } catch (e) {
    report.warnings.push(`运行日志不可读(${snapshot.logFile}): ${e.message}`);
  }

  const reqStats = new Map(); // key: proto status → count
  const badLines = [];
  let oursTotal = 0;
  let otherLines = 0;
  for (const line of newLog.split("\n")) {
    const m = REQ_LINE.exec(line);
    if (!m) {
      if (SUSPICIOUS.test(line)) badLines.push(line.trim().slice(0, 200));
      continue;
    }
    const [, proto, provider, , reqPath, modelMap, status] = m;
    const clientModel = modelMap.split("→")[0];
    if (!modelSet.has(clientModel)) {
      otherLines++;
      continue;
    }
    oursTotal++;
    const key = `${proto} ${reqPath} ${status}`;
    reqStats.set(key, (reqStats.get(key) ?? 0) + 1);
    if (Number(status) >= 400) badLines.push(line.trim().slice(0, 200));
    if (Number(status) >= 500) report.failures.push(`上游 ${status}: ${proto} ${reqPath} model=${modelMap} provider=${provider}`);
    else if (Number(status) >= 400) report.warnings.push(`上游 ${status}: ${proto} ${reqPath} model=${modelMap}`);
  }
  report.lines.push(`窗口内我们的请求 ${oursTotal} 条(其它流量 ${otherLines} 条):`);
  for (const [k, n] of [...reqStats.entries()].sort()) report.lines.push(`  ${n}× ${k}`);
  if (badLines.length) {
    report.lines.push("可疑日志行(前 10 条):");
    for (const l of badLines.slice(0, 10)) report.lines.push(`  ${YELLOW}${l}${RESET}`);
  }

  // --- /api/status 计数器 delta ---
  try {
    return apiGet(o, "/api/status").then((after) => {
      const before = snapshot.status?.counters ?? {};
      const deltas = [];
      for (const [prov, c] of Object.entries(after.counters ?? {})) {
        const b = before[prov] ?? {};
        const dReq = (c.requests ?? 0) - (b.requests ?? 0);
        const dFail = (c.failures ?? 0) - (b.failures ?? 0);
        const d429 = (c.rate_limited_429 ?? 0) - (b.rate_limited_429 ?? 0);
        if (dReq || dFail || d429) {
          deltas.push(`  ${prov}: requests +${dReq}, failures +${dFail}, 429 +${d429}`);
          if (dFail > 0) report.failures.push(`provider ${prov} failures +${dFail}`);
          if (d429 > 0) report.warnings.push(`provider ${prov} rate_limited_429 +${d429}`);
        }
      }
      report.lines.push(`计数器 delta:${deltas.length ? "" : " 无变化"}`);
      report.lines.push(...deltas);
      return report;
    });
  } catch (e) {
    report.warnings.push(`/api/status 读取失败: ${e.message}`);
    return Promise.resolve(report);
  }
}

async function checkRequestLog(o) {
  try {
    const r = await apiGet(o, "/api/requests?limit=1");
    if (r?.enabled === false) {
      return `request_log 未开启(细粒度请求体审计不可用;开启方式见 config.yaml request_log 段,需重启 daemon)`;
    }
    return `request_log 已开启,可经 GET /api/requests 或 Web UI Requests 页逐条核查`;
  } catch {
    return `request_log 状态未知(/api/requests 不可达)`;
  }
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------
async function main() {
  const o = parseArgs(process.argv);

  section("preflight");
  let statusBefore = null;
  try {
    statusBefore = await apiGet(o, "/api/status");
    out(`proxy ${origin(o)} ok — uptime=${statusBefore.uptime ?? "?"} version=${statusBefore.version ?? "?"}`);
  } catch (e) {
    process.stderr.write(`${RED}✗ 代理不可达: ${e.message}\n  先启动: model-proxy serve --config config.yaml${RESET}\n`);
    process.exit(1);
  }
  const logFile = resolveLogFile(o);
  let logOffset = 0;
  try {
    logOffset = fs.statSync(logFile).size;
    out(`运行日志: ${logFile} (offset=${logOffset})`);
  } catch {
    out(`${YELLOW}运行日志不存在: ${logFile}(日志分析将降级)${RESET}`);
  }
  const snapshot = { logFile, logOffset, status: statusBefore };

  const projects = discoverProjects(o);
  out(`项目: ${projects.map((p) => p.name).join(", ")} | 协议: ${o.protocols.join(",")} | 模型: ${o.models.join(",")}`);

  // --- 真实 agent 项目 ---
  const results = [];
  for (const project of projects) {
    const reqs = project.manifest.requires ?? [];
    if (reqs.includes("vision") && !o.withVision) {
      section(`skip ${project.name} (requires vision,加 --with-vision 放开)`);
      continue;
    }
    // manifest.protocols 与 CLI --protocols 取交集(项目可声明自己只覆盖部分协议)
    const projectProtocols = o.protocols.filter((p) => (project.manifest.protocols ?? PROTOCOL_NAMES).includes(p));
    for (const protocol of projectProtocols) {
      for (const model of o.models) {
        const label = `${project.name}/${protocol}/${model}`;
        section(`run ${label}`);
        const start = Date.now();
        try {
          const r = await runProject(project, o, protocol, model);
          const ms = Date.now() - start;
          if (r.failures.length === 0) {
            out(`${GREEN}PASS${RESET} ${(ms / 1000).toFixed(1)}s — 产物校验通过(日志: ${path.join(r.workspace, "run.log")})`);
          } else {
            out(`${RED}FAIL${RESET} ${(ms / 1000).toFixed(1)}s — ${r.failures.join("; ")}`);
            out(`${DIM}stdout 尾部:${RESET}\n${r.stdout.split("\n").slice(-15).join("\n")}`);
          }
          results.push({ label, pass: r.failures.length === 0 });
        } catch (e) {
          out(`${RED}FAIL${RESET} — ${e.message}`);
          results.push({ label, pass: false });
        }
      }
    }
  }

  // --- matrix ---
  let matrixPass = null;
  if (!o.skipMatrix) {
    section("matrix (agenttest)");
    const args = [
      path.join(AGENTTEST_DIR, "agenttest.mjs"),
      "--matrix",
      "--host", o.host,
      "--port", String(o.port),
      "--protocols", o.protocols.join(","),
      "--models", o.models.join(","),
    ];
    if (o.withThinking) args.push("--with-thinking");
    if (o.withVision) args.push("--with-vision");
    if (o.forceProvider) args.push("--force-provider", o.forceProvider);
    const r = await runCommand([process.execPath, ...args], {
      cwd: AGENTTEST_DIR,
      env: {},
      timeoutMs: 30 * 60 * 1000,
      logFile: path.join(AGENTTEST_DIR, "matrix-last.log"),
    });
    process.stdout.write(r.stdout.split("\n").slice(-8).join("\n") + "\n");
    matrixPass = r.code === 0;
    out(matrixPass ? `${GREEN}matrix ok${RESET}` : `${RED}matrix failed(完整输出: matrix-last.log)${RESET}`);
  }

  // --- 后端分析 ---
  section("proxy 后端分析");
  const modelSet = new Set(o.models);
  const report = await analyzeProxy(o, snapshot, modelSet);
  for (const l of report.lines) out(l);
  out(await checkRequestLog(o));
  for (const w of report.warnings) out(`${YELLOW}warn: ${w}${RESET}`);
  for (const f of report.failures) out(`${RED}backend-fail: ${f}${RESET}`);

  // --- 汇总 ---
  section("汇总");
  const failedRuns = results.filter((r) => !r.pass);
  out(`projects: ${results.length - failedRuns.length}/${results.length} 通过` +
    (matrixPass === null ? "" : ` | matrix: ${matrixPass ? "ok" : "FAILED"}`) +
    ` | 后端: ${report.failures.length === 0 ? "正常" : `${report.failures.length} 项异常`}`);
  for (const r of failedRuns) out(`${RED}  FAIL ${r.label}${RESET}`);
  const ok = failedRuns.length === 0 && matrixPass !== false && report.failures.length === 0;
  out(ok ? `${GREEN}${BOLD}E2E PASS${RESET}` : `${RED}${BOLD}E2E FAIL${RESET}`);
  process.exit(ok ? 0 : 1);
}

main().catch((e) => {
  process.stderr.write(`${RED}✗ ${e.stack ?? e.message}${RESET}\n`);
  process.exit(1);
});
