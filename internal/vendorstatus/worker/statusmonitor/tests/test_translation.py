import json
from types import SimpleNamespace

import pytest

from statusmonitor.sources import Issue
from statusmonitor.translation import (
    TranslationService,
    normalize_language,
)


class DummyProvider:
    def __init__(self):
        self.calls = 0

    async def text_chat(self, prompt, system_prompt):
        self.calls += 1
        request = json.loads(prompt)
        translations = [
            {"id": item["id"], "zh_cn": f"中文译文：{item['text']}"}
            for item in request["items"]
        ]
        return SimpleNamespace(
            completion_text=json.dumps(
                {"translations": translations}, ensure_ascii=False
            )
        )


class FailingProvider:
    async def text_chat(self, prompt, system_prompt):
        raise RuntimeError("provider unavailable")


class DummyContext:
    def __init__(self, provider=None):
        self.provider = provider
        self.requested_provider_id = None

    def get_using_provider(self):
        return self.provider

    def get_provider_by_id(self, provider_id):
        self.requested_provider_id = provider_id
        return self.provider


def _issue():
    return Issue(
        "openai",
        "OpenAI",
        "incident_1",
        "warning",
        "Elevated API errors",
        affected_services=("Responses API",),
        detail="We are investigating elevated error rates.",
    )


@pytest.mark.asyncio
async def test_translation_uses_default_provider_and_persistent_cache():
    provider = DummyProvider()
    service = TranslationService(DummyContext(provider))

    first = await service.translate_issues([_issue()], True)
    second = await service.translate_issues([_issue()], True)

    assert provider.calls == 1
    assert first == second
    assert first["Elevated API errors"].startswith("中文译文")
    assert service.dirty

    restored = TranslationService(DummyContext())
    restored.load_cache(service.dump_cache())
    cached = await restored.translate_issues([_issue()], True)
    assert cached == first
    assert not restored.dirty


@pytest.mark.asyncio
async def test_translation_can_select_provider_and_falls_back_without_one():
    provider = DummyProvider()
    context = DummyContext(provider)
    service = TranslationService(context)

    translated = await service.translate_issues([_issue()], True, "translator")

    assert context.requested_provider_id == "translator"
    assert translated["Elevated API errors"].startswith("中文译文")

    unavailable = TranslationService(DummyContext())
    assert await unavailable.translate_issues([_issue()], True) == {}

    failing = TranslationService(DummyContext(FailingProvider()))
    assert await failing.translate_issues([_issue()], True) == {}


def test_display_language_normalization():
    assert normalize_language("zh-CN") == "zh-CN"
    assert normalize_language("en-US") == "en-US"
    assert normalize_language("bilingual") == "bilingual"
    assert normalize_language("invalid") == "bilingual"


def test_translations_do_not_change_issue_fingerprint():
    issue = _issue()
    before = issue.fingerprint
    translations = {
        issue.title: "API 错误率升高",
        issue.detail: "我们正在调查错误率升高的问题。",
    }

    assert translations
    assert issue.fingerprint == before
