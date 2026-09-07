/**
 * engproj.mjs — codews 的"代码实现工程"载体:模板创建 + 真实验证。
 *
 * 工程是一个最小 JS 模块:strutil.js 的三个纯函数(带 stub)+ node:test
 * 功能测试 + 一次真实的文件读写往返(读 input.txt 按规则变换写
 * output.txt)。验证不信任模型自评:harness 在工程副本里真实运行
 * `node --test`,并逐行比对 output.txt。
 */
import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

// 任务说明写得极具体(精确签名+示例),把"模型理解歧义"从失败面里剔除:
// 失败应来自协议转换/模型能力,而不是 spec 含糊。
const SPEC_MD = `# Task: finish strutil.js and process input.txt

This project has a stub module and a failing test suite. Complete two jobs.

## Job 1 — implement src/strutil.js

Replace the stubs so the functions behave EXACTLY as specified (the tests in
test/strutil.test.mjs are the source of truth):

- \`reverseWords(s)\`: reverse the ORDER of whitespace-separated words.
  Example: "hello big world" -> "world big hello".
- \`countVowels(s)\`: count lowercase and uppercase English vowels (a e i o u).
  Example: "A quick fox" -> 4.
- \`slugify(s)\`: lowercase, trim, then DELETE (not replace) every character
  that is not [a-z0-9-] and not whitespace, then collapse each run of
  whitespace into a single hyphen. Existing hyphens are kept as-is.
  Examples: "  Hello, Big World! " -> "hello-big-world";
  "GLM-5.3 & Claude-Opus" -> "glm-53-claude-opus" ("." and "&" are removed,
  the "-" in "GLM-5.3" and "Claude-Opus" stays, spaces become hyphens).

Keep the exact export names: reverseWords, countVowels, slugify (named
exports). Use \`export function ...\` so the tests can import them.

## Job 2 — file processing

1. Read \`input.txt\` (5 lines, each is a phrase).
2. For each line apply \`slugify(line)\` (after trimming the trailing newline).
3. Write the results to \`output.txt\`, one slug per line, in the same order,
   with a trailing newline after the last line.

You have a run_tests tool: it runs the project's functional test suite
(node --test test/) and returns the result. VERIFY your implementation with
run_tests before finishing, and fix the code until every test passes. Also
re-check output.txt when you change slugify.

When both jobs are done and run_tests is green, reply with exactly: DONE
`;

const INPUT_TXT = `Hello, Proxy World
Fast AND Reliable
code-first testing
  Spaces   Everywhere
GLM-5.3 & Claude-Opus
`;

const EXPECTED_OUTPUT = `hello-proxy-world
fast-and-reliable
code-first-testing
spaces-everywhere
glm-53-claude-opus
`;

export const STRUTIL_STUB = `// Implement the three functions per SPEC.md. The tests in test/ fail until
// the implementation is correct.
export function reverseWords(s) {
  throw new Error("not implemented");
}

export function countVowels(s) {
  throw new Error("not implemented");
}

export function slugify(s) {
  throw new Error("not implemented");
}
`;

const TEST_MJS = `import { test } from "node:test";
import assert from "node:assert/strict";
import { reverseWords, countVowels, slugify } from "../src/strutil.js";

test("reverseWords reverses word order", () => {
  assert.equal(reverseWords("hello big world"), "world big hello");
  assert.equal(reverseWords("single"), "single");
  assert.equal(reverseWords(""), "");
});

test("countVowels counts both cases", () => {
  assert.equal(countVowels("A quick fox"), 4);
  assert.equal(countVowels("xyz"), 0);
  assert.equal(countVowels("AEIOU aeiou"), 10);
});

test("slugify normalizes to slugs", () => {
  assert.equal(slugify("  Hello, Big World! "), "hello-big-world");
  assert.equal(slugify("Fast   AND   Reliable"), "fast-and-reliable");
  assert.equal(slugify("GLM-5.3 & Claude-Opus"), "glm-53-claude-opus");
});
`;

export function createProject(dir) {
  fs.mkdirSync(path.join(dir, "src"), { recursive: true });
  fs.mkdirSync(path.join(dir, "test"), { recursive: true });
  fs.writeFileSync(path.join(dir, "SPEC.md"), SPEC_MD);
  fs.writeFileSync(path.join(dir, "input.txt"), INPUT_TXT);
  fs.writeFileSync(path.join(dir, "src", "strutil.js"), STRUTIL_STUB);
  fs.writeFileSync(path.join(dir, "test", "strutil.test.mjs"), TEST_MJS);
  return {
    spec: "SPEC.md",
    entry: "src/strutil.js",
    output: "output.txt",
  };
}

// validateProject runs the REAL functional test suite (node --test) in the
// project copy and byte-compares output.txt. Returns a list of failure
// reasons (empty = pass).
export async function validateProject(dir) {
  const failures = [];
  try {
    // node --test prints its failure summary to STDOUT — collect both pipes.
    // 60s SIGKILL guard (same as the run_tests tool): a model-written
    // infinite loop must not hang the worker forever.
    const { code, out } = await new Promise((resolve) => {
      const p = spawn(process.execPath, ["--test", "test/"], { cwd: dir });
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
    if (code !== 0) {
      const lines = out.split("\n").filter((l) => /✖|not ok|failing|expected|actual|AssertionError/i.test(l));
      const tail = (lines.length ? lines : out.trim().split("\n")).slice(-6).join(" | ").slice(0, 240);
      failures.push(`功能测试失败(exit=${code}): ${tail}`);
    }
  } catch (e) {
    failures.push(`测试无法执行: ${e.message}`);
  }
  const outPath = path.join(dir, "output.txt");
  if (!fs.existsSync(outPath)) {
    failures.push("缺 output.txt(未完成文件写回)");
  } else {
    const got = fs.readFileSync(outPath, "utf8");
    if (got !== EXPECTED_OUTPUT) {
      const diffAt = [...got].findIndex((c, i) => c !== EXPECTED_OUTPUT[i]);
      failures.push(
        `output.txt 不符(首个差异@${diffAt}): got=${JSON.stringify(got.slice(0, 80))} want=${JSON.stringify(EXPECTED_OUTPUT.slice(0, 80))}`,
      );
    }
  }
  return failures;
}

export function expectedOutput() {
  return EXPECTED_OUTPUT;
}
