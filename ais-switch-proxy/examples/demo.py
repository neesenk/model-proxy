#!/usr/bin/env python3
"""ais-switch-proxy 直接调用 demo（流式渐进输出，支持 Anthropic + codex 协议）。

前置：
  1. 已登录（ais-switch-proxy login 或 --import），或 config 配了 static_key
  2. 代理在跑：ais-switch-proxy serve --config config.yaml
  3. 用 codex 协议 + gpt-5.5：先 ais-switch-proxy codex-login（拿独立 OAuth token）

代理监听 http://127.0.0.1:15721，按 URL 路径前缀路由：
  --protocol anthropic  → POST /v1/messages   (claude 路由 → compass 网关, CQP key)
  --protocol codex      → POST /v1/responses  (codex 路由 → 按 model 分流:
                          gpt-5.5 → chatgpt.com + codex OAuth; 其他 → compass 网关)

代理用真实凭据替换占位 token，按 config model_map 改写 model 字段。

输出顺序：
  1. 渐进打印 thinking（灰色）和 text（正常）—— 边收边打
  2. 最后才打印 stop_reason / usage / 耗时
"""

import argparse
import json
import sys
import time
import urllib.request

DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 15722

# ANSI 颜色（非 tty 时可禁用；这里简单始终用）。
DIM = "\033[2m"
RESET = "\033[0m"


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


def stream_sse(url: str, body: bytes, headers: dict):
    """POST 并流式读取 SSE，yield 解析出的事件 dict。"""
    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=120) as resp:
        buf = b""
        while True:
            chunk = resp.read(4096)
            if not chunk:
                break
            buf += chunk
            while b"\n\n" in buf:
                raw, buf = buf.split(b"\n\n", 1)
                data = parse_sse_event(raw)
                if data is not None:
                    yield data


def stream_anthropic(base: str, model: str, prompt: str, max_tokens: int, thinking_budget: int = 0):
    """Anthropic /v1/messages。yield (kind, text) 增量 + ('meta', dict) 事件。

    kind: 'thinking' | 'text' | 'meta'
    thinking_budget > 0 enables extended thinking with that token budget.
    """
    payload = {
        "model": model,
        "max_tokens": max_tokens,
        "stream": True,
        "messages": [{"role": "user", "content": prompt}],
    }
    if thinking_budget > 0:
        payload["thinking"] = {"type": "enabled", "budget_tokens": thinking_budget}
    body = json.dumps(payload).encode()
    headers = {
        "content-type": "application/json",
        "Authorization": "Bearer ANY",
        "anthropic-version": "2023-06-01",
        "accept": "text/event-stream",
    }
    for ev in stream_sse(f"{base}/v1/messages", body, headers):
        t = ev.get("type")
        if t == "message_start":
            yield ("meta", {"model": ev.get("message", {}).get("model")})
        elif t == "content_block_delta":
            delta = ev.get("delta", {})
            dt = delta.get("type")
            if dt == "thinking_delta":
                yield ("thinking", delta.get("thinking", ""))
            elif dt == "text_delta":
                yield ("text", delta.get("text", ""))
        elif t == "message_delta":
            d = ev.get("delta", {})
            meta = {}
            if d.get("stop_reason"):
                meta["stop_reason"] = d.get("stop_reason")
            if ev.get("usage"):
                meta["usage"] = ev.get("usage")
            if meta:
                yield ("meta", meta)


def stream_codex(base: str, model: str, prompt: str, max_tokens: int, effort: str = ""):
    """codex /v1/responses。yield (kind, text) + ('meta', dict)。

    codex responses SSE 事件类型：response.created, response.output_text.delta,
    response.completed 等。reasoning（思考）在 response.reasoning_text.delta。
    Note: codex backend requires store:false (proxy injects it) and does not
    accept max_tokens/max_output_tokens — omit the limit entirely.
    effort (low|medium|high) sets reasoning effort if non-empty.
    """
    payload = {
        "model": model,
        "stream": True,
        "input": [{"type": "message", "role": "user",
                   "content": [{"type": "input_text", "text": prompt}]}],
    }
    if effort:
        payload["reasoning"] = {"effort": effort}
    body = json.dumps(payload).encode()
    headers = {
        "content-type": "application/json",
        "Authorization": "Bearer ANY",
        "accept": "text/event-stream",
    }
    for ev in stream_sse(f"{base}/v1/responses", body, headers):
        t = ev.get("type")
        if t == "response.created":
            yield ("meta", {"model": ev.get("response", {}).get("model")})
        elif t == "response.output_text.delta":
            yield ("text", ev.get("delta", ""))
        elif t == "response.reasoning_text.delta":
            yield ("thinking", ev.get("delta", ""))
        elif t == "response.completed":
            resp = ev.get("response", {})
            meta = {}
            if resp.get("status"):
                meta["stop_reason"] = resp.get("status")
            if resp.get("usage"):
                meta["usage"] = resp.get("usage")
            if meta:
                yield ("meta", meta)


def main():
    ap = argparse.ArgumentParser(description="ais-switch-proxy 流式 demo")
    ap.add_argument("prompt", nargs="?", default="reply with exactly: pong")
    ap.add_argument("model", nargs="?", default=None,
                    help="模型别名或真实名（默认：anthropic→claude-haiku-4-5, codex→gpt-5.5）")
    ap.add_argument("--protocol", choices=["anthropic", "codex"], default="anthropic",
                    help="协议：anthropic(/v1/messages) 或 codex(/v1/responses)。默认 anthropic")
    ap.add_argument("--max-tokens", type=int, default=1024,
                    help="最大输出 token；thinking 模型建议 ≥1024，复杂问题 2000+（codex 协议忽略）")
    ap.add_argument("--effort", choices=["low", "medium", "high"], default="",
                    help="codex 协议推理等级：low|medium|high（仅 --protocol codex 生效）")
    ap.add_argument("--thinking-budget", type=int, default=0,
                    help="anthropic 协议思考 token 预算（仅 --protocol anthropic 生效，0=不启用/用模型默认）")
    ap.add_argument("--host", default=DEFAULT_HOST, help=f"代理主机（默认 {DEFAULT_HOST}）")
    ap.add_argument("--port", type=int, default=DEFAULT_PORT, help=f"代理端口（默认 {DEFAULT_PORT}）")
    args = ap.parse_args()

    if args.model is None:
        args.model = "gpt-5.5" if args.protocol == "codex" else "claude-haiku-4-5"

    base = f"http://{args.host}:{args.port}"
    path = "/v1/responses" if args.protocol == "codex" else "/v1/messages"
    thinking_info = ""
    if args.protocol == "codex" and args.effort:
        thinking_info = f", effort={args.effort}"
    elif args.protocol == "anthropic" and args.thinking_budget > 0:
        thinking_info = f", thinking_budget={args.thinking_budget}"
    print(f"→ POST {base}{path}  (protocol={args.protocol}, model={args.model}, max_tokens={args.max_tokens}{thinking_info})")
    print(f"→ prompt: {args.prompt!r}")
    print("—" * 60)

    if args.protocol == "codex":
        streamer = lambda base, model, prompt, mt: stream_codex(base, model, prompt, mt, args.effort)
    else:
        streamer = lambda base, model, prompt, mt: stream_anthropic(base, model, prompt, mt, args.thinking_budget)
    start = time.time()
    in_thinking = False
    usage = None
    stop_reason = None
    model = None
    text_chars = 0

    try:
        for kind, payload in streamer(base, args.model, args.prompt, args.max_tokens):
            if kind == "meta":
                if "model" in payload:
                    model = payload["model"]
                if "stop_reason" in payload:
                    stop_reason = payload["stop_reason"]
                if "usage" in payload:
                    usage = payload["usage"]
            elif kind == "thinking":
                if not in_thinking:
                    print(f"{DIM}[thinking] ", end="", flush=True)
                    in_thinking = True
                print(payload, end="", flush=True)
            elif kind == "text":
                if in_thinking:
                    print(f"{RESET}", flush=True)
                    in_thinking = False
                print(payload, end="", flush=True)
                text_chars += len(payload)
    except urllib.error.HTTPError as e:
        print(f"\n✗ HTTP {e.code}: {e.read().decode()[:300]}", file=sys.stderr)
        sys.exit(1)
    except urllib.error.URLError as e:
        print(f"\n✗ 连不上代理 {base}：{e}", file=sys.stderr)
        print("  先启动：ais-switch-proxy serve --config config.yaml", file=sys.stderr)
        sys.exit(1)

    if in_thinking:
        print(f"{RESET}", flush=True)
    elapsed = time.time() - start
    print("\n" + "—" * 60)
    print(f"← model={model}")
    print(f"← stop_reason={stop_reason}")
    print(f"← text chars={text_chars}")
    if usage:
        # Anthropic usage 形如 {input_tokens, output_tokens, output_tokens_details}
        # codex usage 形如 {input_tokens, output_tokens, ...}
        inp = usage.get("input_tokens")
        out = usage.get("output_tokens")
        thinking = (usage.get("output_tokens_details") or {}).get("thinking_tokens")
        cost = usage.get("cost")
        if cost is not None:
            print(f"← usage: input={inp} output={out} thinking={thinking} cost=${cost}")
        else:
            print(f"← usage: input={inp} output={out} thinking={thinking}")
    print(f"← elapsed: {elapsed:.2f}s")

    if stop_reason in ("max_tokens", "incomplete") and text_chars == 0:
        print("\n⚠ text 为空：stop_reason=" + stop_reason +
              "，thinking 模型可能把预算耗在 thinking 阶段了。", file=sys.stderr)
        print("  调大 --max-tokens（如 --max-tokens 2048）重试。", file=sys.stderr)
        sys.exit(2)


if __name__ == "__main__":
    main()
