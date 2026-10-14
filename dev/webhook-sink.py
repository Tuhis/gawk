"""A webhook receiver for the local stack's screening lane (R40, docs/71 §9.7).

TEST ONLY. It stands in for the operator's pager: gawk-admin's two static
webhooks in dev/compose.screening.yml post here, and every delivery becomes
one JSON line on stdout — the path it arrived on (`/pager` or `/observed`)
and the CloudEvent's time, type, subject and data — which is what
`docker compose logs webhook-sink` shows a developer and what
e2e/screening-assert.sh reads.

It verifies no signature and keeps nothing: a receiver that checked the HMAC
would need the secret, and this one exists to make the delivery visible, not
to model a real pager. Standard library only, so the image is a stock
python:alpine with this file mounted.
"""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Sink(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)
        try:
            event = json.loads(body)
        except ValueError:
            event = {"unparsed": body.decode("utf-8", "replace")}
        print(
            json.dumps(
                {
                    "path": self.path,
                    "time": event.get("time"),
                    "type": event.get("type"),
                    "subject": event.get("subject"),
                    "data": event.get("data"),
                }
            ),
            flush=True,
        )
        self.send_response(204)
        self.end_headers()

    def do_GET(self):
        # The healthcheck's target.
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *args):
        # The JSON line above is the log; the default access line would
        # double every delivery.
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("", 8080), Sink).serve_forever()
