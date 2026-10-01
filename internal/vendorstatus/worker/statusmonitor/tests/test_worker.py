import io
import json
from types import SimpleNamespace

import pytest

from statusmonitor import worker
from statusmonitor.main import Image, Plain


def test_protocol_does_not_advance_before_host_commit(monkeypatch):
    output = io.StringIO()
    monkeypatch.setattr(worker.sys, "stdout", output)
    monkeypatch.setattr(
        worker.sys, "stdin", SimpleNamespace(buffer=io.BytesIO(b'{"ok":true}\n'))
    )
    protocol = worker.Protocol()
    assert protocol.exchange({"type": "checkpoint", "key": "key", "value": {}})
    assert json.loads(output.getvalue())["type"] == "checkpoint"


@pytest.mark.asyncio
async def test_failed_checkpoint_is_not_acknowledged(monkeypatch):
    monkeypatch.setattr(worker.sys, "stdout", io.StringIO())
    monkeypatch.setattr(
        worker.sys, "stdin", SimpleNamespace(buffer=io.BytesIO(b'{"ok":false}\n'))
    )
    with pytest.raises(OSError, match="commit"):
        await worker.Protocol().checkpoint("monitor_state_v1", {})


@pytest.mark.parametrize("reply", [b"", b'{"ok":"true"}\n', b"{}\n", b"not-json\n"])
def test_invalid_host_acknowledgements_fail_closed(monkeypatch, reply):
    monkeypatch.setattr(worker.sys, "stdout", io.StringIO())
    monkeypatch.setattr(worker.sys, "stdin", SimpleNamespace(buffer=io.BytesIO(reply)))
    with pytest.raises(ValueError):
        worker.Protocol().exchange({"type": "send", "group": "group"})


def test_packets_preserve_png_and_text():
    assert worker.packet_from_chain([Image(b"png"), Plain("fallback")]) == {
        "png": "cG5n",
        "text": "fallback",
    }


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "state",
    [{}, {"version": 2}, {"version": 1, "sources": {}, "deliveries": [], "health": {}}],
)
async def test_corrupt_persisted_state_does_not_replay_alerts(state):
    with pytest.raises(ValueError, match="persisted"):
        await worker.run(
            {"operation": "cycle", "config": {}, "values": {"monitor_state_v1": state}},
            worker.Protocol(),
        )


@pytest.mark.asyncio
async def test_translation_adapter_uses_explicit_non_admin_credentials():
    calls = []

    class Response:
        status = 200

        async def read(self, limit):
            assert limit == worker.MAX_INPUT_BYTES + 1
            return b'{"choices":[{"message":{"content":"translation-json"}}]}'

        @property
        def content(self):
            return self

        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            return False

    class Session:
        def post(self, endpoint, **kwargs):
            calls.append((endpoint, kwargs))
            return Response()

    provider = worker.ChatProvider(
        Session(),
        {
            "base_url": "https://example.test/v1/",
            "api_key": "dedicated-model-key",
            "model": "translator",
        },
    )
    result = await provider.text_chat("prompt", "system")
    assert result.completion_text == "translation-json"
    endpoint, options = calls[0]
    assert endpoint == "https://example.test/v1/chat/completions"
    assert options["headers"] == {"Authorization": "Bearer dedicated-model-key"}
    assert options["json"]["model"] == "translator"
    assert options["json"]["messages"][0]["content"] == "system"
