"""Framework-independent status engine, ported from upstream 1.2.2.

Go owns scheduling, QQ routing, bbolt checkpoints and delivery acknowledgements.
The source parsing, reconciliation, translation and rendering algorithms retain
upstream behavior. No AstrBot runtime or platform adapter is imported.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from datetime import datetime
from typing import Any
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from .monitor_state import (
    cleanup_resolved,
    delivery_fingerprint,
    mark_health_delivered,
    plan_health_notices,
    presentation_result,
    reconcile_source,
)
from .renderer import build_alert_fallback, render_alert_card, render_overview
from .sources import Issue, SourceResult, build_source_specs, fetch_all_sources
from .translation import TranslationService, normalize_language

logger = logging.getLogger(__name__)
STATE_KEY = "monitor_state_v1"
STATE_VERSION = 1
TRANSLATION_CACHE_KEY = "translation_cache_v1"
MAX_EVENTS_PER_CARD = 5
MAX_EVENTS_PER_CYCLE = 20
SEND_TIMEOUT_SECONDS = 30


@dataclass
class Image:
    data: bytes

    @classmethod
    def fromBytes(cls, data: bytes):
        return cls(data)


@dataclass
class Plain:
    text: str


class MessageChain(list):
    """Local transport envelope, independent of any chatbot framework."""


class GlobalStatusMonitor:
    def __init__(self, context: Any, config: dict[str, Any]) -> None:
        self.context = context
        self.config = config
        self._session = None
        self._fetch_lock = asyncio.Lock()
        self._state = self._empty_state()
        self._last_results = []
        self._translator = TranslationService(context)

    async def put_kv_data(self, key: str, value: Any) -> None:
        await self.context.checkpoint(key, value)

    async def _report_progress(self, stage, text="", completed=0, total=0):
        callback = getattr(self.context, "progress", None)
        if callback is not None:
            await callback(stage, text, completed, total)

    def _targets(self, groups: list[str]) -> dict[str, str]:
        # Config/commands validate official QQ group_openid values in Go.
        return {group: group for group in groups}

    @staticmethod
    def _empty_state() -> dict[str, Any]:
        return {
            "version": STATE_VERSION,
            "sources": {},
            "deliveries": {},
            "initialized_sources": [],
            "health": {},
        }

    def _source_specs(self):
        source_config = self.config.get("sources", {})
        custom_sources = self.config.get("custom_statuspage_sources", [])
        return build_source_specs(
            source_config if isinstance(source_config, dict) else {},
            custom_sources if isinstance(custom_sources, list) else [],
        )

    def _display_timezone_name(self) -> str:
        """Return the plugin timezone override or the host's timezone."""
        configured = str(self.config.get("timezone", "") or "").strip()
        if configured:
            return configured
        try:
            host_config = self.context.get_config()
        except (AttributeError, TypeError):
            return ""
        return str(host_config.get("timezone", "") or "").strip()

    def _display_datetime(self) -> datetime:
        """Return the current time in the configured display timezone."""
        timezone_name = self._display_timezone_name()
        if not timezone_name:
            return datetime.now().astimezone()
        try:
            return datetime.now(ZoneInfo(timezone_name))
        except (ValueError, ZoneInfoNotFoundError):
            logger.warning(
                "Invalid status image timezone %r; using the system timezone.",
                timezone_name,
            )
            return datetime.now().astimezone()

    def _bounded_int(self, key: str, default: int, minimum: int, maximum: int) -> int:
        try:
            return max(minimum, min(maximum, int(self.config.get(key, default))))
        except (TypeError, ValueError):
            return default

    def _history_hours(self) -> int:
        return self._bounded_int("history_lookback_hours", 24, 0, 168)

    async def _fetch_sources(self) -> list[SourceResult]:
        """Fetch all enabled sources under a lock shared with the query command."""
        if self._session is None or self._session.closed:
            raise RuntimeError("Status monitor HTTP session is not available")
        async with self._fetch_lock:
            specs = self._source_specs()
            await self._report_progress(
                "fetch",
                f"正在收集 {specs[0].name} 等提供商的信息……"
                if specs
                else "没有启用的提供商。",
                0,
                len(specs),
            )
            kwargs = {}
            if getattr(self.context, "progress", None) is not None:
                kwargs["progress"] = self._report_progress
            results = await fetch_all_sources(
                self._session,
                specs,
                bool(self.config.get("notify_maintenance", False)),
                self._history_hours(),
                **kwargs,
            )
            self._last_results = [
                presentation_result(result, self._state) for result in results
            ]
            for result in results:
                if not result.success or not result.complete:
                    logger.warning(
                        "Status source %s incomplete: %s",
                        result.spec.name,
                        result.error,
                    )
            return results

    def _configured_groups(self) -> list[str]:
        """Normalize group_whitelist entries: trim, deduplicate, and return.

        Entries may be pure digit group IDs, group_openid strings, or full
        UMO strings (platform_id:MessageType:session_id). Format-specific
        validation is deferred to _targets().
        """
        raw_groups = self.config.get("group_whitelist", [])
        if not isinstance(raw_groups, list):
            return []
        entries: list[str] = []
        for raw_group in raw_groups:
            entry = str(raw_group).strip()
            if not entry or entry in entries:
                continue
            entries.append(entry)
        return entries

    def _prune_disabled_sources(self, enabled_source_ids: set[str]) -> None:
        health = self._state.setdefault("health", {})
        disabled = (set(self._state["sources"]) | set(health)) - enabled_source_ids
        for source_id in disabled:
            self._state["sources"].pop(source_id, None)
            health.pop(source_id, None)
        self._state["initialized_sources"] = [
            source_id
            for source_id in self._state.get("initialized_sources", [])
            if source_id in enabled_source_ids
        ]
        for delivered in self._state["deliveries"].values():
            for issue_key in list(delivered):
                if issue_key.split(":", 1)[0] in disabled:
                    delivered.pop(issue_key, None)

    def _prune_removed_groups(self, groups: list[str]) -> None:
        allowed = set(groups)
        ledgers = [self._state["deliveries"]]
        ledgers.extend(
            x.get("deliveries", {}) for x in self._state.get("health", {}).values()
        )
        for deliveries in ledgers:
            for target_key in list(deliveries):
                if target_key not in allowed:
                    deliveries.pop(target_key, None)

    def _reconcile_source(
        self,
        result: SourceResult,
        targets: dict[str, str],
    ) -> dict[str, list[tuple[str, Issue]]]:
        """Reconcile active incidents and bounded catch-up without guessing recoveries."""
        return reconcile_source(self._state, result, targets, self._history_hours())

    async def _send_events(
        self,
        unified_message_origin: str,
        source_name: str,
        events: list[tuple[str, Issue]],
        translations: dict[str, str] | None = None,
    ) -> bool:
        """Render one batch and bound the wait for its platform adapter.

        Args:
            unified_message_origin: Full destination session identifier.
            source_name: Vendor name displayed on the card.
            events: Incident stages and details to deliver.
            translations: Prepared translations, or None to resolve them here.

        Returns:
            Whether the adapter reported success before the timeout.
        """
        language = normalize_language(self.config.get("display_language", "bilingual"))
        if translations is None:
            translations = await self._translator.translate_issues(
                (issue for _, issue in events),
                bool(self.config.get("enable_ai_translation", True))
                and language != "en-US",
                str(self.config.get("translation_provider_id", "")).strip(),
            )
        try:
            generated_at = self._display_datetime()
            png = await asyncio.to_thread(
                render_alert_card,
                source_name,
                events,
                translations,
                language,
                generated_at.tzinfo,
                str(self.config.get("card_theme", "paper")),
                generated_at,
            )
            chain = MessageChain([Image.fromBytes(png)])
        except Exception:
            logger.exception("Failed to render status alert card; using text fallback.")
            chain = MessageChain(
                [
                    Plain(
                        build_alert_fallback(
                            source_name,
                            events,
                            translations,
                            language,
                        )
                    )
                ]
            )
        try:
            sent = await asyncio.wait_for(
                self.context.send_message(unified_message_origin, chain),
                timeout=SEND_TIMEOUT_SECONDS,
            )
            if not sent:
                logger.warning(
                    "No platform matched status alert target %s.",
                    unified_message_origin,
                )
            return bool(sent)
        except Exception:
            logger.exception(
                "Failed to send status alert to %s.",
                unified_message_origin,
            )
            return False

    def _mark_delivered(
        self,
        target_key: str,
        events: list[tuple[str, Issue]],
    ) -> None:
        delivered = self._state["deliveries"].setdefault(target_key, {})
        for stage, issue in events:
            delivered[issue.key] = delivery_fingerprint(stage, issue)

    def _cleanup_recoveries(
        self,
        source_id: str,
        target_keys: set[str],
        has_configured_groups: bool,
    ) -> None:
        cleanup_resolved(self._state, source_id, target_keys, has_configured_groups)

    @staticmethod
    def _event_batches(
        events: list[tuple[str, Issue]],
    ) -> list[list[tuple[str, Issue]]]:
        """Bound each image and drain large backlogs over subsequent cycles."""
        return [
            events[offset : offset + MAX_EVENTS_PER_CARD]
            for offset in range(
                0, min(len(events), MAX_EVENTS_PER_CYCLE), MAX_EVENTS_PER_CARD
            )
        ]

    async def _run_cycle(self) -> None:
        """Persist the baseline and each acknowledged batch before advancing."""
        results = await self._fetch_sources()
        successful_results = [result for result in results if result.success]
        self._prune_disabled_sources({result.spec.source_id for result in results})
        groups = self._configured_groups()
        self._prune_removed_groups(groups)
        targets = self._targets(groups)

        reconciled: list[tuple[SourceResult, dict[str, list[tuple[str, Issue]]]]] = []
        initialized_sources = set(self._state.get("initialized_sources", []))
        notify_existing = bool(
            self.config.get("notify_existing_on_first_startup", True)
        )
        for result in successful_results:
            pending = self._reconcile_source(result, targets)
            if not notify_existing and result.spec.source_id not in initialized_sources:
                for target_key, events in pending.items():
                    self._mark_delivered(target_key, events)
                pending = {}
                logger.info(
                    "Recorded initial status baseline for %s without notification.",
                    result.spec.name,
                )
            initialized_sources.add(result.spec.source_id)
            reconciled.append((result, pending))
        self._state["initialized_sources"] = sorted(initialized_sources)
        # Commit the baseline before external sends, including silent startup.
        await self.put_kv_data(STATE_KEY, self._state)

        language = normalize_language(self.config.get("display_language", "bilingual"))
        changed_issues = [
            issue
            for _, pending in reconciled
            for events in pending.values()
            for _, issue in events[:MAX_EVENTS_PER_CYCLE]
        ]
        translations = await self._translator.translate_issues(
            changed_issues,
            bool(self.config.get("enable_ai_translation", True))
            and language != "en-US",
            str(self.config.get("translation_provider_id", "")).strip(),
        )

        for result, pending in reconciled:
            for target_key, events in pending.items():
                for batch in self._event_batches(events):
                    if not await self._send_events(
                        targets[target_key],
                        result.spec.name,
                        batch,
                        translations,
                    ):
                        break
                    self._mark_delivered(target_key, batch)
                    # A reload during a later delivery must not replay this batch.
                    await self.put_kv_data(STATE_KEY, self._state)
            self._cleanup_recoveries(
                result.spec.source_id,
                set(targets),
                bool(groups),
            )
        health_pending: dict[str, list[tuple[str, Issue]]] = {}
        for result in results:
            notices = plan_health_notices(
                self._state,
                result,
                targets,
                bool(self.config.get("notify_source_failures", True)),
                self._bounded_int("source_failure_threshold", 3, 1, 100),
                self._bounded_int("source_failure_cooldown_seconds", 3600, 60, 86400),
            )
            for target_key, events in notices.items():
                health_pending.setdefault(target_key, []).extend(events)
        await self.put_kv_data(STATE_KEY, self._state)
        for target_key, events in health_pending.items():
            for batch in self._event_batches(events):
                if not await self._send_events(
                    targets[target_key], "Status monitor", batch, {}
                ):
                    break
                mark_health_delivered(self._state, target_key, batch)
                await self.put_kv_data(STATE_KEY, self._state)
        self._last_results = [
            presentation_result(result, self._state) for result in results
        ]
        await self.put_kv_data(STATE_KEY, self._state)
        if self._translator.dirty:
            await self.put_kv_data(
                TRANSLATION_CACHE_KEY,
                self._translator.dump_cache(),
            )
            self._translator.dirty = False

    async def vendor_status(self, event: Any):
        """Query all enabled vendor sources and return a current status image."""
        try:
            results = [
                presentation_result(result, self._state)
                for result in await self._fetch_sources()
            ]
            issues = [
                issue
                for result in results
                if result.success
                for issue in result.issues.values()
            ]
            await self._report_progress("translate", "正在整理事件信息及翻译缓存……")
            translations = await self._translator.translate_issues(
                issues,
                bool(self.config.get("enable_ai_translation", True))
                and normalize_language(self.config.get("display_language", "bilingual"))
                != "en-US",
                str(self.config.get("translation_provider_id", "")).strip(),
            )
            await self._report_progress(
                "render",
                "正在生成状态总览图片……",
            )
            png = await asyncio.to_thread(
                render_overview,
                results,
                translations,
                normalize_language(self.config.get("display_language", "bilingual")),
                self._display_datetime(),
                str(self.config.get("card_theme", "paper")),
            )
            if self._translator.dirty:
                await self._report_progress("encode", "正在保存翻译缓存……")
                await self.put_kv_data(
                    TRANSLATION_CACHE_KEY,
                    self._translator.dump_cache(),
                )
                self._translator.dirty = False
            await self._report_progress("encode", "正在准备发送状态总览图片……")
            yield event.chain_result([Image.fromBytes(png)])
        except Exception as exc:
            logger.exception("Failed to build on-demand vendor status overview.")
            yield event.plain_result(
                f"厂商状态查询失败：{type(exc).__name__}: {str(exc)[:1500]}"
            )
