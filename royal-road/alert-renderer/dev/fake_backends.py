#!/usr/bin/env python3
"""Fake vmalert, vmselect and Discord for alert-renderer's end-to-end test.

    fake_backends.py PORT CAPTURE_DIR

- /vmalert/api/v1/rules           rules from testdata/prod-exprs.json, filtered by rule_name[]
- /select/0/prometheus/api/v1/query_range
                                  two synthetic series per query, crossing 1.0
- POST /api/webhooks/<id>/<token> Discord: saves each multipart part to
                                  CAPTURE_DIR/<n>/; token "reject-images" answers
                                  400 when files are attached, "rate-limit" answers
                                  429, "down" answers 503, "slow" answers after 40 s
"""
import json
import math
import os
import sys
import time
from email.parser import BytesParser
from email.policy import HTTP
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
RULES = json.load(open(os.path.join(HERE, "..", "testdata", "prod-exprs.json")))
CAPTURE = sys.argv[2]
state = {"n": 0}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        sys.stderr.write("fake: " + fmt % args + "\n")

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        url = urlparse(self.path)
        q = parse_qs(url.query)
        if url.path == "/vmalert/api/v1/rules":
            names = q.get("rule_name[]", [])
            rules = [
                {"name": r["name"], "query": r["expr"], "type": "alerting", "labels": {"severity": r["severity"]}}
                for r in RULES
                if not names or r["name"] in names
            ]
            return self.reply(200, {"status": "success", "data": {"groups": [{"name": "g", "id": "1", "rules": rules}]}})
        if url.path == "/select/0/prometheus/api/v1/query_range":
            start, end, step = int(q["start"][0]), int(q["end"][0]), int(q["step"][0].rstrip("s"))
            result = []
            for i, labels in enumerate([{"namespace": "royal-road", "service": "royalroad-admin", "ingress": "royal-road-admin"},
                                        {"namespace": "royal-road", "service": "royalroad-api", "ingress": "royalroad-api"}]):
                values = []
                for t in range(start, end + 1, step):
                    x = (t - start) / max(end - start, 1)
                    values.append([t, str(round((1.6 if i == 0 else 0.4) * x + 0.1 * math.sin(20 * x), 4))])
                result.append({"metric": labels, "values": values})
            return self.reply(200, {"status": "success", "data": {"resultType": "matrix", "result": result}})
        self.reply(404, {"error": "no route"})

    def do_POST(self):
        url = urlparse(self.path)
        if not url.path.startswith("/api/webhooks/"):
            return self.reply(404, {"error": "no route"})
        token = url.path.rsplit("/", 1)[-1]
        body = self.rfile.read(int(self.headers["Content-Length"]))
        msg = BytesParser(policy=HTTP).parsebytes(
            b"Content-Type: " + self.headers["Content-Type"].encode() + b"\r\n\r\n" + body
        )
        parts = {p.get_param("name", header="content-disposition"): p for p in msg.iter_parts()}
        has_files = any(n.startswith("files[") for n in parts)
        if token == "down":
            return self.reply(503, {"message": "unavailable"})
        if token == "slow":
            time.sleep(40)
        if token == "rate-limit":
            return self.reply(429, {"message": "You are being rate limited.", "retry_after": 0.2, "global": False})
        if token == "reject-images" and has_files:
            return self.reply(400, {"message": "Request entity too large", "code": 40005})
        state["n"] += 1
        out = os.path.join(CAPTURE, str(state["n"]))
        os.makedirs(out, exist_ok=True)
        for name, p in parts.items():
            fname = p.get_filename() or (name + ".json")
            with open(os.path.join(out, fname), "wb") as f:
                f.write(p.get_payload(decode=True))
        self.send_response(204)
        self.end_headers()


ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
