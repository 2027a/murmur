#!/usr/bin/env python3
"""Exercise the installed bar without capturing audio, sending text or changing config.

python3 helpers/quickshell-preview.py
python3 helpers/quickshell-preview.py --screenshots /tmp/opencode --geometry '3000,0 840x24'
"""
import argparse
import json
import math
import os
from pathlib import Path
import socket
import subprocess
import time
import uuid


def send(path, state):
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(2)
        conn.connect(str(path))
        conn.sendall(json.dumps(state).encode() + b"\n")
        if conn.recv(1) != b"\x01":
            raise RuntimeError("Le module QML n'a pas répondu")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--screenshots", type=Path, help="répertoire existant")
    parser.add_argument("--geometry", default="0,0 1920x24")
    args = parser.parse_args()
    runtime = Path(os.environ.get("XDG_RUNTIME_DIR", f"/run/user/{os.getuid()}")) / "murmur"
    if args.screenshots and not args.screenshots.is_dir():
        parser.error("le répertoire des captures doit exister")
    status = json.loads(subprocess.check_output(["murmur", "status", "--json"]))
    if status["active"]:
        parser.error("une dictée est active")
    state = {"v": 1, "session_id": "preview-" + uuid.uuid4().hex, "state": "starting", "cleanup": True, "delivery": "paste"}
    latencies = []
    cases = [("starting", {}), ("recording", {}), ("silence", {}), ("clipping", {}),
             ("stopping", {}), ("transcribing", {}), ("processing", {}), ("delivering", {}),
             ("completed", {"result": "completed"}),
             ("degraded", {"result": "completed", "warning": "cleanup: timeout"}),
             ("cancelled", {"result": "cancelled"}),
             ("failed", {"result": "failed", "error_class": "timeout"})]
    for name, extra in cases:
        if "result" in extra:
            state["session_id"] = "preview-" + uuid.uuid4().hex
            state["finished_ms"] = round(time.time() * 1000)
        state.update(state="idle" if extra else "recording" if name in ("silence", "clipping") else name,
                     result="", warning="", error_class="", level={})
        state.update(extra)
        if name == "recording":
            state["capture_ms"] = round(time.time() * 1000)
        start = time.monotonic()
        captured = False
        duration = 3.8 if name == "silence" else 1.5
        while time.monotonic() - start < duration:
            if state["state"] == "recording":
                rms = 0 if name == "silence" else .1 + .09 * math.sin(time.monotonic() * 9)
                state["level"] = {"rms": rms, "peak": 1 if name == "clipping" else min(1, rms * 2),
                                  "clipping": name == "clipping", "updated_ms": round(time.time() * 1000)}
            then = time.monotonic()
            send(runtime / "ui.sock", state)
            latencies.append(time.monotonic() - then)
            if args.screenshots and not captured and time.monotonic() - start > duration - .4:
                subprocess.run(["grim", "-g", args.geometry, str(args.screenshots / f"murmur-{name}.png")], check=True)
                captured = True
            time.sleep(max(0, .05 - (time.monotonic() - then)))
        print(name)
    state.update(session_id="preview-" + uuid.uuid4().hex, result="completed", error_class="", warning="", finished_ms=round(time.time()*1000))
    send(runtime / "ui.sock", state)
    print(f"{len(latencies)} trames; ACK moyen {sum(latencies)/len(latencies)*1000:.1f} ms, max {max(latencies)*1000:.1f} ms")


if __name__ == "__main__":
    main()
