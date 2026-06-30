#!/usr/bin/env python3
"""ais-switch-proxy 直接调用 demo（流式渐进输出）。

前置：
  1. 已登录（ais-switch-proxy login 或 --import），或 config 配了 static_key
  2. 代理在跑：ais-switch-proxy serve --config config.yaml

代理监听 http://127.0.0.1:15721，本 demo 走 Anthropic 协议 POST /v1/messages（stream=true）。
代理用 CQP key 替换占位 token，并按 config 的 model_map 改写 model 字段。

输出顺序：
  1. 渐进打印 thinking（灰色）和 text（正常）—— 边收边打
  2. 最后才打印 stop_reason / usage / 耗时

注意：glm-5.2 / deepseek-v4-pro 这类带 thinking 的模型会先吐 thinking 块，max_tokens 太小
（如 200）会把预算全花在 thinking 上、text 为空（stop_reason=max_tokens）。默认 1024 通常够；
复杂问题建议 --max-tokens 2048+。
"""

import argparse
import json
import sys
import time
import urllib.request

DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 15721

# ANSI 颜色（非 tty 时可禁用；这里简单始终用）。
DIM = "\033[2m"
RESET = "\033[0m"


def stream_messages(base: str, model_alias: str, prompt: str, max_tokens: int):
    """流式 POST /v1/messages，yield 解析出的事件 dict。

    SSE 格式：`event: <type>` 行后跟 `data: <json>` 行，空行分隔。
    """
    body = json.dumps({
        "model": model_alias,
        "max_tokens": max_tokens,
        "stream": True,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request(
        f"{base}/v1/messages",
        data=body,
        headers={
            "content-type": "application/json",
            "Authorization": "Bearer ANY",        # 占位，代理替换为真实 CQP key
            "anthropic-version": "2023-06-01",
            "accept": "text/event-stream",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        buf = b""
        while True:
            chunk = resp.read(4096)
            if not chunk:
                break
            buf += chunk
            # SSE 事件以空行（\n\n）分隔。
            while b"\n\n" in buf:
                raw, buf = buf.split(b"\n\n", 1)
                data = parse_sse_event(raw)
                if data is not None:
                    yield data


def parse_sse_event(raw: bytes):
    """从一个 SSE 事件块里取 `data:` 行的 JSON。无 data 行返回 None。"""
    data_line = None
    for line in raw.split(b"\n"):
        line = line.strip()
        if line.startswith(b"data:"):
            data_line = line[5:].strip()
            break
    if data_line is None:
        return None
    try:
        return json.loads(data_line)
    except json.JSONDecodeError:
        return None


def main():
    ap = argparse.ArgumentParser(description="ais-switch-proxy /v1/messages 流式 demo")
    ap.add_argument("prompt", nargs="?", default="reply with exactly: pong")
    ap.add_argument("model", nargs="?", default="claude-haiku-4-5",
                    help="模型别名（须在 config model_map 里）或真实模型名（如 glm-5.2）")
    ap.add_argument("--max-tokens", type=int, default=1024,
                    help="最大输出 token；thinking 模型建议 ≥1024，复杂问题 2000+")
    ap.add_argument("--host", default=DEFAULT_HOST, help=f"代理主机（默认 {DEFAULT_HOST}）")
    ap.add_argument("--port", type=int, default=DEFAULT_PORT, help=f"代理端口（默认 {DEFAULT_PORT}）")
    args = ap.parse_args()

    base = f"http://{args.host}:{args.port}"
    print(f"→ POST {base}/v1/messages  (model={args.model}, max_tokens={args.max_tokens})")
    print(f"→ prompt: {args.prompt!r}")
    print("—" * 60)

    start = time.time()
    block_type = None          # 当前 content_block 的类型
    usage = None
    stop_reason = None
    model = None
    text_chars = 0

    try:
        for ev in stream_messages(base, args.model, args.prompt, args.max_tokens):
            t = ev.get("type")
            if t == "message_start":
                model = ev.get("message", {}).get("model")
            elif t == "content_block_start":
                block_type = ev.get("content_block", {}).get("type")
                if block_type == "thinking":
                    print(f"{DIM}[thinking] ", end="", flush=True)
            elif t == "content_block_delta":
                delta = ev.get("delta", {})
                dt = delta.get("type")
                if dt == "thinking_delta":
                    # dim 已在 block_start 时开启，这里只打内容，不重复转义。
                    print(delta.get("thinking", ""), end="", flush=True)
                elif dt == "text_delta":
                    print(delta.get("text", ""), end="", flush=True)
                    text_chars += len(delta.get("text", ""))
            elif t == "content_block_stop":
                if block_type == "thinking":
                    print(f"{RESET}", flush=True)  # 结束 thinking 的 dim
                block_type = None
            elif t == "message_delta":
                d = ev.get("delta", {})
                if d.get("stop_reason"):
                    stop_reason = d.get("stop_reason")
                if ev.get("usage"):
                    usage = ev.get("usage")
            elif t == "message_stop":
                pass
    except urllib.error.HTTPError as e:
        print(f"\n✗ HTTP {e.code}: {e.read().decode()[:300]}", file=sys.stderr)
        sys.exit(1)
    except urllib.error.URLError as e:
        print(f"\n✗ 连不上代理 {base}：{e}", file=sys.stderr)
        print("  先启动：ais-switch-proxy serve --config config.yaml", file=sys.stderr)
        sys.exit(1)

    elapsed = time.time() - start
    print("\n" + "—" * 60)
    print(f"← model={model}")
    print(f"← stop_reason={stop_reason}")
    print(f"← text chars={text_chars}")
    if usage:
        # 合并 message_start 和 message_delta 的 usage（后者更全）。
        out = usage.get("output_tokens")
        inp = usage.get("input_tokens")
        thinking = (usage.get("output_tokens_details") or {}).get("thinking_tokens")
        cost = usage.get("cost")
        print(f"← usage: input={inp} output={out} thinking={thinking} cost=${cost}" if cost is not None
              else f"← usage: input={inp} output={out} thinking={thinking}")
    print(f"← elapsed: {elapsed:.2f}s")

    if stop_reason == "max_tokens" and text_chars == 0:
        print("\n⚠ text 为空：stop_reason=max_tokens，thinking 模型把预算耗在 thinking 阶段了。", file=sys.stderr)
        print("  调大 --max-tokens（如 --max-tokens 2048）重试。", file=sys.stderr)
        sys.exit(2)


if __name__ == "__main__":
    main()
