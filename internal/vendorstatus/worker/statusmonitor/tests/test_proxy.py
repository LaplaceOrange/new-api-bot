import asyncio
import socket

import aiohttp
import pytest
from curl_cffi import CurlOpt
from curl_cffi.requests import AsyncSession

from statusmonitor.modern_sources import _request
from statusmonitor.proxy import status_connector


@pytest.mark.asyncio
@pytest.mark.parametrize("scheme", ["socks5", "socks5h"])
@pytest.mark.parametrize("transport", ["aiohttp", "curl"])
async def test_authenticated_socks_proxy_routes_requests_and_dns(
    scheme, transport, monkeypatch
):
    monkeypatch.setenv("NO_PROXY", "*")
    monkeypatch.setenv("no_proxy", "*")
    requests = []
    errors = []

    async def handle(reader, writer):
        try:
            version, count = await reader.readexactly(2)
            methods = await reader.readexactly(count)
            assert version == 5 and 2 in methods
            writer.write(b"\x05\x02")
            await writer.drain()
            assert (await reader.readexactly(1)) == b"\x01"
            length = (await reader.readexactly(1))[0]
            username = await reader.readexactly(length)
            length = (await reader.readexactly(1))[0]
            password = await reader.readexactly(length)
            assert username == b"user" and password == b"p@ss:word"
            writer.write(b"\x01\x00")
            await writer.drain()
            version, command, _, atyp = await reader.readexactly(4)
            assert version == 5 and command == 1
            if atyp == 3:
                length = (await reader.readexactly(1))[0]
                host = (await reader.readexactly(length)).decode()
            elif atyp == 1:
                host = socket.inet_ntop(socket.AF_INET, await reader.readexactly(4))
            else:
                host = socket.inet_ntop(socket.AF_INET6, await reader.readexactly(16))
            await reader.readexactly(2)
            writer.write(b"\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x50")
            await writer.drain()
            headers = await reader.readuntil(b"\r\n\r\n")
            requests.append((atyp, host, headers))
            writer.write(
                b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"
            )
            await writer.drain()
        except Exception as exc:
            errors.append(exc)
        finally:
            writer.close()
            await writer.wait_closed()

    server = await asyncio.start_server(handle, "127.0.0.1", 0)
    async with server:
        port = server.sockets[0].getsockname()[1]
        proxy = f"{scheme}://user:p%40ss%3Aword@127.0.0.1:{port}"
        host = "does-not-resolve.invalid" if scheme == "socks5h" else "localhost"
        endpoint = f"http://{host}/api/status"
        if transport == "aiohttp":
            async with aiohttp.ClientSession(
                connector=status_connector(proxy),
                trust_env=False,
                timeout=aiohttp.ClientTimeout(total=5),
            ) as session:
                async with session.get(endpoint) as response:
                    assert await response.text() == "{}"
        else:
            async with AsyncSession(
                proxy=proxy, timeout=5, curl_options={CurlOpt.NOPROXY: ""}
            ) as client:
                assert await _request(client, "GET", endpoint) == "{}"
        assert not errors
        assert len(requests) == 1
        atyp, observed_host, _ = requests[0]
        if scheme == "socks5h":
            assert atyp == 3 and observed_host == host
        else:
            assert atyp in {1, 4}
