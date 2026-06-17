#!/usr/bin/env python3
"""Redeem an Agent Room temporary join code and optionally start Bridge."""

from __future__ import annotations

import argparse
import json
import os
import re
import secrets
import shutil
import socket
import stat
import subprocess
import sys
import time
from pathlib import Path
from typing import Any
from urllib import error, parse, request


DEFAULT_RELAY = "http://127.0.0.1:8080"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--room", required=True, help="Room ID or Agent Room URL")
    parser.add_argument("--code", required=True, help="Temporary authorization code, or '-' to read stdin")
    parser.add_argument("--relay", default="", help="Relay HTTP origin when --room is a bare ID")
    parser.add_argument("--agent-id", default="", help="Agent id to register; stable codex id is used when omitted")
    parser.add_argument("--agent-label", default="", help="Display label for this temporary agent")
    parser.add_argument("--provider", default="codex-skill", help="Provider metadata sent during redemption")
    parser.add_argument("--agent-room-bin", default="", help="Path to agent-room binary; defaults to PATH lookup")
    parser.add_argument("--state-dir", default="", help="State dir; defaults to AGENT_ROOM_STATE_DIR or ~/.agent-room")
    parser.add_argument("--start-bridge", action="store_true", help="Start agent-room bridge in the background if needed")
    parser.add_argument("--print-token", action="store_true", help="Include the returned temporary token in output")
    return parser.parse_args()


def state_dir(raw: str) -> Path:
    base = raw or os.environ.get("AGENT_ROOM_STATE_DIR") or str(Path.home() / ".agent-room")
    path = Path(base).expanduser()
    path.mkdir(parents=True, exist_ok=True)
    return path


def sanitize_part(value: str) -> str:
    value = value.strip().lower()
    value = re.sub(r"[^a-z0-9_]+", "-", value)
    return value.strip("-") or "local"


def stable_agent_id(path: Path) -> str:
    file = path / "skill-agent-id"
    if file.exists():
        existing = file.read_text(encoding="utf-8").strip()
        if existing:
            return existing
    host = sanitize_part(socket.gethostname())
    user = sanitize_part(os.environ.get("USER") or os.environ.get("USERNAME") or "user")
    agent_id = f"codex-{host}-{user}-{secrets.token_hex(4)}"
    file.write_text(agent_id + "\n", encoding="utf-8")
    try:
        file.chmod(stat.S_IRUSR | stat.S_IWUSR)
    except OSError:
        pass
    return agent_id


def derive_room_and_relay(room_value: str, relay_value: str) -> tuple[str, str]:
    room_value = room_value.strip()
    relay_value = relay_value.strip()
    parsed = parse.urlparse(room_value)

    room_id = ""
    relay = relay_value
    if parsed.scheme and parsed.netloc:
        query_room = parse.parse_qs(parsed.query).get("room", [""])[0].strip()
        if query_room:
            room_id = query_room
        else:
            parts = [parse.unquote(p) for p in parsed.path.strip("/").split("/") if p]
            for idx, part in enumerate(parts):
                if part == "rooms" and idx + 1 < len(parts):
                    room_id = parts[idx + 1]
                    break
        if not relay:
            relay = f"{parsed.scheme}://{parsed.netloc}"
    else:
        room_id = room_value

    if not room_id:
        raise SystemExit("Could not determine room id from --room")
    if not relay:
        relay = os.environ.get("AGENT_ROOM_RELAY") or os.environ.get("AGENT_ROOM_RELAY_URL") or DEFAULT_RELAY
    return room_id, normalize_relay_origin(relay)


def normalize_relay_origin(raw: str) -> str:
    relay = raw.strip().rstrip("/")
    if relay.startswith("ws://"):
        relay = "http://" + relay[len("ws://") :]
    elif relay.startswith("wss://"):
        relay = "https://" + relay[len("wss://") :]
    parsed = parse.urlparse(relay)
    if parsed.scheme and parsed.netloc:
        return f"{parsed.scheme}://{parsed.netloc}"
    return relay


def read_code(raw: str) -> str:
    if raw == "-":
        raw = sys.stdin.read()
    code = raw.strip()
    if not code:
        raise SystemExit("Authorization code is empty")
    return code


def post_json(url: str, payload: dict[str, Any]) -> dict[str, Any]:
    data = json.dumps(payload).encode("utf-8")
    req = request.Request(
        url,
        data=data,
        headers={"content-type": "application/json", "accept": "application/json"},
        method="POST",
    )
    try:
        with request.urlopen(req, timeout=20) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        raise SystemExit(f"Redeem failed: HTTP {exc.code}: {body}") from exc
    except error.URLError as exc:
        raise SystemExit(f"Redeem failed: {exc}") from exc


def redact(result: dict[str, Any], print_token: bool) -> dict[str, Any]:
    safe = dict(result)
    if not print_token and safe.get("agent_token"):
        safe["agent_token"] = "<redacted>"
    if not print_token and safe.get("command"):
        safe["command"] = "<redacted: use --print-token for a manual command>"
    return safe


def find_binary(raw: str) -> str:
    if raw:
        return str(Path(raw).expanduser())
    return shutil.which("agent-room") or ""


def start_bridge(binary: str, relay: str, token: str, agent_id: str, label: str, state: Path) -> dict[str, Any]:
    if not binary:
        return {
            "started": False,
            "reason": "agent-room binary not found on PATH",
            "install": "Install agent-room, then rerun with --start-bridge or run the returned command manually.",
        }

    log_path = state / "skill-bridge.log"
    pid_path = state / "skill-bridge.pid"
    env = os.environ.copy()
    env["AGENT_ROOM_AGENT_TOKEN"] = token
    cmd = [
        binary,
        "bridge",
        "-relay",
        relay,
        "-agent-id",
        agent_id,
        "-agent-label",
        label,
        "-agent-capabilities",
        "Temporary Codex skill participant in Agent Room",
    ]
    log_file = log_path.open("ab")
    proc = subprocess.Popen(cmd, stdout=log_file, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL, env=env)
    time.sleep(0.4)
    if proc.poll() is not None:
        return {
            "started": False,
            "exit_code": proc.returncode,
            "log": str(log_path),
            "reason": "agent-room bridge exited during startup",
        }
    pid_path.write_text(str(proc.pid) + "\n", encoding="utf-8")
    return {"started": True, "pid": proc.pid, "log": str(log_path), "pid_file": str(pid_path)}


def main() -> int:
    args = parse_args()
    state = state_dir(args.state_dir)
    room_id, relay = derive_room_and_relay(args.room, args.relay)
    agent_id = args.agent_id.strip() or stable_agent_id(state)
    label = args.agent_label.strip() or f"Codex on {socket.gethostname()}"
    code = read_code(args.code)

    result = post_json(
        f"{relay}/v1/rooms/{parse.quote(room_id, safe='')}/agent-join-codes/redeem",
        {
            "code": code,
            "agent_id": agent_id,
            "agent_label": label,
            "provider": args.provider,
        },
    )

    bridge: dict[str, Any] = {"started": False}
    if args.start_bridge:
        if result.get("delivered") is True:
            bridge = {"started": False, "reason": "join_room delivered to an existing control connection"}
        else:
            bridge = start_bridge(find_binary(args.agent_room_bin), relay, str(result["agent_token"]), agent_id, label, state)

    output = {
        "room_id": room_id,
        "relay": relay,
        "agent_id": agent_id,
        "redeem": redact(result, args.print_token),
        "bridge": bridge,
    }
    print(json.dumps(output, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
