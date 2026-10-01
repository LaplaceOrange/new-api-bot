"""Cached incident translation through the host's configured chat provider."""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import re
from collections.abc import Iterable
from typing import Any

from .sources import Issue

logger = logging.getLogger(__name__)

SUPPORTED_LANGUAGES = {"zh-CN", "en-US", "bilingual"}
MAX_CACHE_ENTRIES = 2000
TRANSLATION_TIMEOUT_SECONDS = 60
TRANSLATION_BATCH_SIZE = 24

BUILTIN_TRANSLATIONS = {
    "Component status degradation": "组件状态降级",
    "Google Cloud incident": "Google Cloud 服务事件",
    "Service incident": "服务事件",
    "Service status degradation": "服务状态降级",
    "Service status event": "服务状态事件",
}


def normalize_language(value: object) -> str:
    """Return a supported display language value.

    Args:
        value: Raw configuration value.

    Returns:
        Normalized language identifier.
    """
    language = str(value or "bilingual").strip()
    return language if language in SUPPORTED_LANGUAGES else "bilingual"


def _cache_key(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def _needs_translation(text: str) -> bool:
    if not text or re.search(r"[\u3400-\u9fff]", text):
        return False
    if text.startswith(("http://", "https://")):
        return False
    return bool(re.search(r"[A-Za-z]", text))


class TranslationService:
    """Translate incident text to Simplified Chinese with a persistent cache."""

    def __init__(self, context: Any) -> None:
        self.context = context
        self._cache: dict[str, dict[str, str]] = {}
        self._lock = asyncio.Lock()
        self.dirty = False

    def load_cache(self, value: object) -> None:
        """Load validated translations from plugin KV data.

        Args:
            value: Raw KV payload.
        """
        if not isinstance(value, dict):
            return
        cache: dict[str, dict[str, str]] = {}
        for key, entry in value.items():
            if not isinstance(key, str) or not isinstance(entry, dict):
                continue
            source = entry.get("source")
            translated = entry.get("zh_cn")
            if isinstance(source, str) and isinstance(translated, str) and translated:
                if _cache_key(source) == key:
                    cache[key] = {"source": source, "zh_cn": translated}
        self._cache = dict(list(cache.items())[-MAX_CACHE_ENTRIES:])
        self.dirty = False

    def dump_cache(self) -> dict[str, dict[str, str]]:
        """Return a bounded serializable translation cache."""
        if len(self._cache) > MAX_CACHE_ENTRIES:
            self._cache = dict(list(self._cache.items())[-MAX_CACHE_ENTRIES:])
        return dict(self._cache)

    async def translate_issues(
        self,
        issues: Iterable[Issue],
        enabled: bool,
        provider_id: str = "",
    ) -> dict[str, str]:
        """Translate unique incident fields and return source-to-Chinese mappings.

        Args:
            issues: Issues whose title, detail, and service names may be displayed.
            enabled: Whether model translation is enabled.
            provider_id: Optional explicit chat provider ID. Empty uses the default.

        Returns:
            Original strings mapped to available Simplified Chinese translations.
        """
        texts: list[str] = []
        for issue in issues:
            for text in (issue.title, issue.detail, *issue.affected_services):
                normalized = " ".join(str(text or "").split())
                if normalized and normalized not in texts:
                    texts.append(normalized)

        translations: dict[str, str] = {}
        for text in texts:
            builtin = BUILTIN_TRANSLATIONS.get(text)
            if builtin:
                translations[text] = builtin
                continue
            entry = self._cache.get(_cache_key(text))
            if entry and entry.get("source") == text:
                translations[text] = entry["zh_cn"]

        missing = [
            text
            for text in texts
            if text not in translations and _needs_translation(text)
        ]
        if not enabled or not missing:
            return translations

        async with self._lock:
            # Another concurrent query may have populated the cache while waiting.
            uncached: list[str] = []
            for text in missing:
                entry = self._cache.get(_cache_key(text))
                if entry and entry.get("source") == text:
                    translations[text] = entry["zh_cn"]
                else:
                    uncached.append(text)
            if not uncached:
                return translations

            try:
                provider = None
                if provider_id:
                    getter = getattr(self.context, "get_provider_by_id", None)
                    if getter:
                        provider = getter(provider_id)
                else:
                    getter = getattr(self.context, "get_using_provider", None)
                    if getter:
                        provider = getter()
                if provider is None or not hasattr(provider, "text_chat"):
                    logger.warning(
                        "Incident translation skipped because no chat provider is available."
                    )
                    return translations

                for offset in range(0, len(uncached), TRANSLATION_BATCH_SIZE):
                    batch = uncached[offset : offset + TRANSLATION_BATCH_SIZE]
                    request_items = [
                        {"id": _cache_key(text)[:16], "text": text[:5000]}
                        for text in batch
                    ]
                    prompt = json.dumps(
                        {"items": request_items},
                        ensure_ascii=False,
                        separators=(",", ":"),
                    )
                    response = await asyncio.wait_for(
                        provider.text_chat(
                            prompt=prompt,
                            system_prompt=(
                                "你是服务状态事件翻译器。把输入 JSON 中每个 text 准确翻译为简体中文；"
                                "保留产品名、API 名、地区代码、数字、URL 和技术缩写，不补充、不总结。"
                                "只输出严格 JSON，格式为 "
                                '{"translations":[{"id":"原 id","zh_cn":"译文"}]}。'
                            ),
                        ),
                        timeout=TRANSLATION_TIMEOUT_SECONDS,
                    )
                    raw = str(getattr(response, "completion_text", "") or "").strip()
                    if raw.startswith("```"):
                        raw = re.sub(r"^```(?:json)?\s*|\s*```$", "", raw).strip()
                    start = raw.find("{")
                    end = raw.rfind("}")
                    if start < 0 or end < start:
                        raise ValueError("translation provider returned no JSON object")
                    payload = json.loads(raw[start : end + 1])
                    returned = payload.get("translations", [])
                    if not isinstance(returned, list):
                        raise ValueError(
                            "translation response has no translations list"
                        )
                    by_id = {
                        str(item.get("id", "")): str(item.get("zh_cn", "")).strip()
                        for item in returned
                        if isinstance(item, dict)
                    }
                    for text in batch:
                        translated = by_id.get(_cache_key(text)[:16], "")
                        if not translated or not re.search(
                            r"[\u3400-\u9fff]", translated
                        ):
                            continue
                        key = _cache_key(text)
                        self._cache.pop(key, None)
                        self._cache[key] = {"source": text, "zh_cn": translated}
                        translations[text] = translated
                        self.dirty = True
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.warning(
                    "Incident translation failed; using original text: %s", exc
                )
        return translations
