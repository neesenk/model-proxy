/**
 * codetask.mjs — codews/compare 共用的单次编码任务执行。
 * 从 codews.mjs 提取:工程副本创建 → agent 任务 → harness 真实验证。
 */

import fs from "node:fs";
import path from "node:path";
import { makeAgent, runPrompt, lastAssistant, withTimeout, allMessages } from "./core.mjs";
import { createProject, validateProject, STRUTIL_STUB } from "./engproj.mjs";

// runCodeTask runs one full coding task for (provider, model, protocol).
// tag disambiguates repeat runs in the workspace directory (compare mode).
export async function runCodeTask(opts, t, protocol, tag = "") {
  const row = {
    provider: t.provider, model: t.model, protocol, tag,
    status: "FAIL", detail: "", ms: 0, error: null, workspace: null,
  };
  if (opts.dumpDir) fs.mkdirSync(opts.dumpDir, { recursive: true });
  const ws = path.join(
    opts.workDir,
    `${t.provider}__${t.model.replace(/[/\\]/g, "_")}__${protocol}${tag}`,
  );
  fs.rmSync(ws, { recursive: true, force: true });
  createProject(ws);
  row.workspace = ws;

  const agent = makeAgent(
    { ...opts, forceProvider: t.provider },
    { protocol, model: t.model, thinking: "off", tools: true, baseDir: ws },
  );
  const start = Date.now();
  try {
    await withTimeout(
      runPrompt(
        agent,
        "Read SPEC.md in the working directory and complete both jobs it describes. " +
          "Use read_file to read files, write_file to write them, and run_tests to verify your " +
          "implementation (iterate until green). Finish with exactly: DONE",
      ),
      opts.taskTimeoutMs,
    );
  } catch (e) {
    row.ms = Date.now() - start;
    row.usage = collectUsage(agent);
    row.artifacts = artifactSizes(ws);
    row.error = String(e.message ?? e);
    row.detail = `任务执行失败: ${row.error.slice(0, 160)}`;
    return row;
  }
  row.ms = Date.now() - start;
  row.usage = collectUsage(agent);
  row.artifacts = artifactSizes(ws);
  const answer = lastAssistant(agent);
  if (answer.error) {
    row.error = String(answer.error);
    row.detail = `上游错误: ${row.error.slice(0, 160)}`;
    return row;
  }
  const failures = await validateProject(ws);
  const saidDone = /DONE/.test(answer.text.trim().slice(-40));
  if (failures.length > 0) {
    row.detail = failures.join(" ;; ").slice(0, 240);
    return row;
  }
  if (!saidDone) {
    row.detail = `任务完成但未回复 DONE(回复尾段: ${JSON.stringify(answer.text.trim().slice(-40))})`;
    return row;
  }
  row.status = "PASS";
  row.detail = `tests=ok output=exact DONE`;
  return row;
}

// collectUsage sums per-turn usage over every assistant message in the
// session (input counts the full context each turn — that is what the
// provider bills), plus reasoning tokens when the wire reports them.
export function collectUsage(agent) {
  const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0, turns: 0 };
  for (const m of allMessages(agent)) {
    if (m.role !== "assistant" || !m.usage) continue;
    usage.input += m.usage.input ?? 0;
    usage.output += m.usage.output ?? 0;
    usage.cacheRead += m.usage.cacheRead ?? 0;
    usage.cacheWrite += m.usage.cacheWrite ?? 0;
    usage.reasoning += m.usage.reasoning ?? 0;
    usage.turns++;
  }
  return usage;
}

// artifactSizes reports the byte sizes of the produced files.
export function artifactSizes(ws) {
  const sizes = {};
  for (const f of ["src/strutil.js", "output.txt"]) {
    try {
      sizes[f] = fs.statSync(path.join(ws, f)).size;
    } catch {
      sizes[f] = null;
    }
  }
  return sizes;
}

// classifyTaskFailure buckets a failure by whether it implicates the
// conversion layer vs the model's own implementation skill:
//   impl-error    — 功能测试未过/输出不符:模型自己写错了(原生路径同样可能)
//   chain-broken  — 没写任何文件/没读 SPEC:工具链路断了(强转换信号)
//   upstream      — 上游显式错误
//   timeout       — 超时
// 顺序:先判 timeout/upstream(写文件前发生的上游错误/超时不应错归
// chain-broken),最后才判 chain-broken;stub 判定要求 strutil.js 与模板
// stub 逐字节一致(模型任何改动都不算 chain-broken)。
export function classifyTaskFailure(row) {
  const d = row.detail;
  if (/上游错误/.test(d)) return "upstream";
  // Only real timeouts (withTimeout's message) bucket as timeout; other
  // runPrompt failures (client bugs, aborts) are their own bucket.
  if (/任务执行失败: timeout /.test(d)) return "timeout";
  if (/任务执行失败/.test(d)) return "task-error";
  if (row.workspace) {
    const src = path.join(row.workspace, "src", "strutil.js");
    const out = path.join(row.workspace, "output.txt");
    const stub = fs.existsSync(src) && fs.readFileSync(src, "utf8") === STRUTIL_STUB;
    if (stub && !fs.existsSync(out)) return "chain-broken";
  }
  if (/缺 output\.txt/.test(d)) return "chain-broken";
  return "impl-error";
}
