#!/usr/bin/env python3
"""Renders alert-renderer output as an HTML page that looks like Discord's
dark theme, to review a message without posting it.

    preview.py OUT.html TITLE DIR NOW [TITLE DIR NOW ...]

DIR holds `message.json` (from `alert-renderer render`) or
`payload_json.json` (captured by dev/fake_backends.py) and the charts.
NOW (RFC 3339) is when the message is shown as posted; relative timestamps
count from it.
Image paths are written relative to OUT.html.
"""
import datetime as dt
import html
import json
import os
import re
import sys

CSS = """
body { margin: 0; background: #313338; color: #dbdee1; font: 16px/1.375 "gg sans", "Noto Sans", "Helvetica Neue", Arial, sans-serif; }
.case { padding: 20px 24px 28px; border-bottom: 1px solid #3f4147; }
.case > h2 { margin: 0 0 14px; font-size: 13px; font-weight: 600; color: #949ba4; text-transform: uppercase; letter-spacing: .02em; }
.msg { display: grid; grid-template-columns: 40px 1fr; column-gap: 16px; }
.avatar { width: 40px; height: 40px; border-radius: 50%; background: #5865f2; display: grid; place-items: center; font-weight: 700; color: #fff; }
.head { font-size: 16px; }
.name { color: #f2f3f5; font-weight: 500; }
.app { background: #5865f2; color: #fff; font-size: 10px; font-weight: 600; border-radius: 3px; padding: 1px 4px; margin: 0 4px; vertical-align: 2px; }
.when { color: #949ba4; font-size: 12px; }
.embed { margin-top: 8px; max-width: 520px; background: #2b2d31; border-radius: 4px; border-left: 4px solid; padding: 8px 16px 16px 12px; box-sizing: border-box; }
.author { font-size: 14px; font-weight: 600; color: #f2f3f5; margin-top: 8px; }
.title { font-size: 16px; font-weight: 600; margin-top: 8px; }
.title a, .desc a, .value a { color: #00a8fc; text-decoration: none; }
.desc { font-size: 14px; margin-top: 8px; white-space: normal; word-wrap: break-word; }
.fields { display: grid; grid-template-columns: repeat(3, 1fr); gap: 8px; margin-top: 8px; }
.field { font-size: 14px; min-width: 0; }
.field.wide { grid-column: 1 / -1; }
.fname { font-weight: 600; color: #f2f3f5; margin-bottom: 2px; }
.value { word-wrap: break-word; }
code { background: #1e1f22; border-radius: 4px; padding: 0 .2em; font: 85% Consolas, "Noto Sans Mono", monospace; }
pre { background: #1e1f22; border: 1px solid #1e1f22; border-radius: 4px; padding: 7px; white-space: pre-wrap; font: 12px Consolas, "Noto Sans Mono", monospace; margin: 6px 0 0; }
.ts { background: #3f4147; border-radius: 3px; padding: 0 2px; }
.image { margin-top: 16px; }
.image img { max-width: 100%; border-radius: 4px; display: block; }
.footer { margin-top: 8px; font-size: 12px; color: #b5bac1; }
"""


def relative(epoch, now):
    d = now - epoch
    future = d < 0
    d = abs(d)
    for unit, secs in (("day", 86400), ("hour", 3600), ("minute", 60)):
        if d >= secs:
            n = int(d // secs)
            s = f"{n} {unit}{'s' if n != 1 else ''}"
            return f"in {s}" if future else f"{s} ago"
    return "just now"


def markdown(text, now):
    """The subset of Discord markdown alert-renderer emits."""
    escapes = []

    def keep(m):
        escapes.append(m.group(1))
        return f"\x00{len(escapes) - 1}\x00"

    text = re.sub(r"\\([^\w\s])", keep, text)
    blocks = []

    def block(m):
        blocks.append(f"<pre>{html.escape(m.group(1).strip())}</pre>")
        return f"\x01{len(blocks) - 1}\x01"

    text = re.sub(r"```(?:\w+\n)?(.*?)```", block, text, flags=re.S)
    text = html.escape(text, quote=False)
    text = re.sub(r"`([^`]+)`", r"<code>\1</code>", text)
    text = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", text)
    text = re.sub(r"\[([^\]]+)\]\((https?://[^)\s]+)\)", r'<a href="\2">\1</a>', text)
    text = re.sub(r"&lt;t:(\d+):R&gt;", lambda m: f'<span class="ts">{relative(int(m.group(1)), now)}</span>', text)
    text = text.replace("\n", "<br>")
    text = re.sub("\x01(\\d+)\x01", lambda m: blocks[int(m.group(1))], text)
    return re.sub("\x00(\\d+)\x00", lambda m: html.escape(escapes[int(m.group(1))]), text)


def clock(iso):
    t = dt.datetime.fromisoformat(iso.replace("Z", "+00:00"))
    return t.strftime("%Y-%m-%d %H:%M UTC")


def embed(e, base, now):
    colour = "#%06x" % e.get("color", 0x202225)
    out = [f'<div class="embed" style="border-left-color:{colour}">']
    if "author" in e:
        out.append(f'<div class="author">{html.escape(e["author"]["name"])}</div>')
    title = html.escape(e.get("title", ""))
    if e.get("url"):
        title = f'<a href="{html.escape(e["url"])}">{title}</a>'
    out.append(f'<div class="title">{title}</div>')
    if e.get("description"):
        out.append(f'<div class="desc">{markdown(e["description"], now)}</div>')
    fields = e.get("fields", [])
    if fields:
        out.append('<div class="fields">')
        for f in fields:
            wide = "" if f.get("inline") else " wide"
            out.append(
                f'<div class="field{wide}"><div class="fname">{markdown(f["name"], now)}</div>'
                f'<div class="value">{markdown(f["value"], now)}</div></div>'
            )
        out.append("</div>")
    if "image" in e:
        name = e["image"]["url"].removeprefix("attachment://")
        out.append(f'<div class="image"><img src="{html.escape(os.path.join(base, name))}"></div>')
    footer = [html.escape(e["footer"]["text"])] if "footer" in e else []
    if "timestamp" in e:
        footer.append(clock(e["timestamp"]))
    if footer:
        out.append(f'<div class="footer">{" • ".join(footer)}</div>')
    out.append("</div>")
    return "".join(out)


def main():
    out, rest = sys.argv[1], sys.argv[2:]
    cases = []
    for title, d, now_s in zip(rest[::3], rest[1::3], rest[2::3]):
        now = dt.datetime.fromisoformat(now_s.replace("Z", "+00:00")).timestamp()
        path = next(p for p in (os.path.join(d, "message.json"), os.path.join(d, "payload_json.json")) if os.path.exists(p))
        msg = json.load(open(path))
        base = os.path.relpath(d, os.path.dirname(os.path.abspath(out)))
        embeds = "".join(embed(e, base, now) for e in msg["embeds"])
        cases.append(
            f'<section class="case"><h2>{html.escape(title)}</h2><div class="msg"><div class="avatar">A</div><div>'
            f'<div class="head"><span class="name">Alertmanager</span><span class="app">APP</span>'
            f'<span class="when">{clock(now_s)}</span></div>{embeds}</div></div></section>'
        )
    with open(out, "w") as f:
        f.write(f"<!doctype html><meta charset=utf-8><title>alert-renderer preview</title><style>{CSS}</style>{''.join(cases)}")


main()
