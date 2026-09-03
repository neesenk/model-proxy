// pi-mcp 项目用的最小 MCP stdio server。
// 两个工具覆盖两类 MCP 流量:数值参数调用 + 文件内容返回。
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";

const server = new McpServer({ name: "e2e-test", version: "0.1.0" });

server.registerTool(
  "add",
  { description: "Add two numbers.", inputSchema: { a: z.number(), b: z.number() } },
  async ({ a, b }) => ({ content: [{ type: "text", text: String(a + b) }] }),
);

server.registerTool(
  "workspace_note",
  { description: "Read the seeded note file (note.txt) bundled with this MCP server.", inputSchema: {} },
  async () => {
    const note = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "note.txt"), "utf8");
    return { content: [{ type: "text", text: note }] };
  },
);

await server.connect(new StdioServerTransport());
