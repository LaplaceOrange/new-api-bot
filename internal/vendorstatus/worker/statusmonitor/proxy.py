"""Dedicated status-source proxy; never changes QQ, New API or translation."""

from contextvars import ContextVar
from urllib.parse import unquote, urlsplit

import aiohttp

status_proxy = ContextVar("status_proxy", default="")


def status_connector(proxy):
    if not proxy:
        return aiohttp.TCPConnector(limit=8)
    from aiohttp_socks import ProxyConnector, ProxyType

    parsed = urlsplit(proxy)
    if (
        parsed.scheme not in {"socks5", "socks5h"}
        or not parsed.hostname
        or not parsed.port
    ):
        raise ValueError("Invalid status proxy configuration")
    return ProxyConnector(
        proxy_type=ProxyType.SOCKS5,
        host=parsed.hostname,
        port=parsed.port,
        username=unquote(parsed.username) if parsed.username is not None else None,
        password=unquote(parsed.password) if parsed.password is not None else None,
        rdns=parsed.scheme == "socks5h",
        limit=8,
    )
