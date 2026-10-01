import asyncio

import pytest

from statusmonitor import main, sources
from statusmonitor.sources import SourceResult, SourceSpec
from statusmonitor.translation import TranslationService
from statusmonitor.worker import HostContext

from .test_main import _issue, _plugin, _spec


@pytest.mark.asyncio
async def test_fetch_progress_counts_finished_sources_and_reports_partial_failure(
    monkeypatch,
):
    specs = [
        SourceSpec("first", "First", "statuspage", "endpoint", "https://first.test"),
        SourceSpec("second", "Second", "statuspage", "endpoint", "https://second.test"),
    ]
    packets = []

    async def fetch(session, spec, maintenance, history):
        await asyncio.sleep(0)
        return SourceResult(spec, spec.source_id == "first", error="TimeoutError")

    async def progress(stage, text, completed, total):
        packets.append((stage, text, completed, total))

    monkeypatch.setattr(sources, "fetch_source", fetch)
    results = await sources.fetch_all_sources(None, specs, False, progress=progress)
    assert len(results) == 2
    counts = [(done, total) for stage, _, done, total in packets if stage == "fetch"]
    assert counts == [(1, 2), (2, 2)]
    assert any(
        stage == "warning" and "Second" in text and "TimeoutError" in text
        for stage, text, _, _ in packets
    )


@pytest.mark.asyncio
async def test_query_reports_stages_and_preserves_render_error(monkeypatch):
    plugin = _plugin()
    packets = []

    async def progress(stage, text="", completed=0, total=0):
        packets.append((stage, text))

    async def fetch():
        return [SourceResult(_spec(), True)]

    def render(*args):
        raise RuntimeError("PNG encoding failed")

    class Event:
        @staticmethod
        def plain_result(text):
            return text

    monkeypatch.setattr(plugin.context, "progress", progress, raising=False)
    monkeypatch.setattr(plugin, "_fetch_sources", fetch)
    monkeypatch.setattr(main, "render_overview", render)
    responses = [value async for value in plugin.vendor_status(Event())]
    assert "RuntimeError" in responses[0] and "PNG encoding failed" in responses[0]
    assert [stage for stage, _ in packets] == ["translate", "translate", "render"]


@pytest.mark.asyncio
async def test_translation_failure_is_reported_and_falls_back():
    packets = []

    class Provider:
        async def text_chat(self, **kwargs):
            raise TimeoutError("translator timed out")

    class Context:
        def get_using_provider(self):
            return Provider()

        async def progress(self, stage, text="", completed=0, total=0):
            packets.append((stage, text, completed, total))

    result = await TranslationService(Context()).translate_issues([_issue()], True)
    assert isinstance(result, dict)
    assert packets[0][0] == "translate"
    assert any(
        stage == "warning" and "TimeoutError" in text for stage, text, _, _ in packets
    )


@pytest.mark.asyncio
async def test_background_poll_does_not_emit_user_query_progress(monkeypatch):
    emitted = []
    monkeypatch.setattr("statusmonitor.worker.emit", emitted.append)
    await HostContext(None, None, {}, "cycle").progress("fetch", "no public progress")
    assert not emitted
    await HostContext(None, None, {}, "query").progress("fetch", "collecting", 3, 20)
    assert emitted == [
        {
            "type": "progress",
            "stage": "fetch",
            "text": "collecting",
            "completed": 3,
            "total": 20,
        }
    ]
