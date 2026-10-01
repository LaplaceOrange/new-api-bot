"""Line-delimited, bidirectional JSON protocol with the owning Go process."""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import os
import sys
from types import SimpleNamespace
from typing import Any

import aiohttp

from .main import STATE_KEY, TRANSLATION_CACHE_KEY, GlobalStatusMonitor, Image, Plain
from .proxy import status_connector, status_proxy

MAX_INPUT_BYTES = 24 * 1024 * 1024


def read_json() -> Any:
    line = sys.stdin.buffer.readline(MAX_INPUT_BYTES + 1)
    if not line or len(line) > MAX_INPUT_BYTES or not line.endswith(b"\n"):
        raise ValueError("Invalid host protocol input")
    return json.loads(line)


def emit(value: dict[str, Any]) -> None:
    sys.stdout.write(
        json.dumps(value, ensure_ascii=False, separators=(",", ":")) + "\n"
    )
    sys.stdout.flush()


class Protocol:
    def exchange(self, value: dict[str, Any]) -> bool:
        emit(value)
        # Go enforces delivery/worker deadlines and kills the process on
        # cancellation. A synchronous read avoids abandoned asyncio reader
        # threads consuming another delivery's acknowledgement after timeout.
        reply = read_json()
        if not isinstance(reply, dict) or not isinstance(reply.get("ok"), bool):
            raise ValueError("Invalid host acknowledgement")
        return reply["ok"]

    async def checkpoint(self, key: str, value: Any) -> None:
        if not self.exchange({"type": "checkpoint", "key": key, "value": value}):
            raise OSError("Host did not commit the checkpoint")


def packet_from_chain(chain) -> dict[str, Any]:
    packet: dict[str, Any] = {}
    for component in chain:
        if isinstance(component, Image):
            packet["png"] = base64.b64encode(component.data).decode("ascii")
        elif isinstance(component, Plain):
            packet["text"] = component.text
    return packet


class ChatProvider:
    """Explicitly configured chat-completions translator; no admin-token reuse."""

    def __init__(self, session: aiohttp.ClientSession, config: dict[str, Any]):
        self.session = session
        self.config = config

    async def text_chat(self, prompt: str, system_prompt: str):
        endpoint = self.config["base_url"].rstrip("/") + "/chat/completions"
        async with self.session.post(
            endpoint,
            headers={"Authorization": "Bearer " + self.config["api_key"]},
            json={
                "model": self.config["model"],
                "messages": [
                    {"role": "system", "content": system_prompt},
                    {"role": "user", "content": prompt},
                ],
                "temperature": 0,
            },
            timeout=aiohttp.ClientTimeout(total=60),
        ) as response:
            if response.status != 200:
                raise RuntimeError(f"Translation HTTP {response.status}")
            data = await response.content.read(MAX_INPUT_BYTES + 1)
            if len(data) > MAX_INPUT_BYTES:
                raise ValueError("Translation response is too large")
            payload = json.loads(data)
            return SimpleNamespace(
                completion_text=payload["choices"][0]["message"]["content"]
            )


class HostContext:
    def __init__(
        self,
        protocol: Protocol,
        provider: ChatProvider | None,
        config: dict,
        operation="cycle",
    ):
        self.protocol = protocol
        self.provider = provider
        self.config = config
        self.operation = operation

    async def progress(self, stage, text="", completed=0, total=0):
        if self.operation == "query":
            emit(
                {
                    "type": "progress",
                    "stage": stage,
                    "text": text,
                    "completed": completed,
                    "total": total,
                }
            )

    async def checkpoint(self, key, value):
        await self.protocol.checkpoint(key, value)

    async def send_message(self, group, chain):
        return self.protocol.exchange(
            {"type": "send", "group": group, **packet_from_chain(chain)}
        )

    def get_using_provider(self):
        return self.provider

    def get_provider_by_id(self, _provider_id):
        return self.provider

    def get_config(self):
        return self.config


class QueryEvent:
    @staticmethod
    def chain_result(chain):
        return chain

    @staticmethod
    def plain_result(text):
        return [Plain(text)]


async def run(request: dict[str, Any], protocol: Protocol) -> None:
    config = request["config"]
    values = request.get("values") or {}
    font_path = config.get("font_path")
    if font_path:
        os.environ["VENDOR_STATUS_FONT_PATH"] = font_path
    timeout = aiohttp.ClientTimeout(total=config.get("http_timeout_seconds", 15))
    proxy = config.get("proxy") or ""
    status_proxy.set(proxy)
    async with (
        aiohttp.ClientSession(
            timeout=timeout,
            trust_env=not bool(proxy),
            connector=status_connector(proxy),
            headers={"User-Agent": "new-api-bot-Global-Status/1.2.2"},
        ) as session,
        aiohttp.ClientSession(timeout=timeout, trust_env=True) as translation_session,
    ):
        translation = config.get("translation") or {}
        provider = (
            ChatProvider(translation_session, translation)
            if all(translation.get(key) for key in ("base_url", "api_key", "model"))
            else None
        )
        monitor = GlobalStatusMonitor(
            HostContext(protocol, provider, config, request["operation"]), config
        )
        await monitor._report_progress("startup", "正在准备提供商状态采集……")
        stored = values.get(STATE_KEY)
        if stored is not None:
            if (
                not isinstance(stored, dict)
                or stored.get("version") != 1
                or not isinstance(stored.get("sources"), dict)
                or not isinstance(stored.get("deliveries"), dict)
                or not isinstance(stored.get("health", {}), dict)
            ):
                # Do not silently discard a corrupt dedup ledger and replay
                # all alerts. Host storage must be repaired explicitly.
                raise ValueError("Invalid persisted vendor status state")
            monitor._state = stored
            if not isinstance(stored.get("initialized_sources"), list):
                stored["initialized_sources"] = list(stored["sources"])
        monitor._translator.load_cache(values.get(TRANSLATION_CACHE_KEY, {}))
        monitor._session = session
        if request["operation"] == "cycle":
            await monitor._run_cycle()
            emit({"type": "done"})
        elif request["operation"] == "query":
            async for chain in monitor.vendor_status(QueryEvent()):
                emit({"type": "done", **packet_from_chain(chain)})
        else:
            raise ValueError("Unknown operation")


def main() -> None:
    logging.basicConfig(stream=sys.stderr, level=logging.WARNING)
    try:
        asyncio.run(run(read_json(), Protocol()))
    except Exception as exc:
        logging.exception("Vendor status worker failed")
        # Diagnostic traceback is stderr-only; never put credentials or
        # status endpoint response bodies into the host protocol.
        emit({"type": "error", "error": f"{type(exc).__name__}: {str(exc)[:1500]}"})
        raise SystemExit(1) from None


if __name__ == "__main__":
    main()
