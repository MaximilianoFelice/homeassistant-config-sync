"""The API client against a real aiohttp server (status codes and the SSE stream)."""

from __future__ import annotations

import asyncio

from aiohttp import ClientSession, web
import pytest

from custom_components.config_sync.api import (
    ConfigSyncAuthError,
    ConfigSyncBusyError,
    ConfigSyncClient,
    ConfigSyncError,
)

# These tests talk to a real local HTTP server; the HA harness blocks sockets
# unless a test opts in.
pytestmark = pytest.mark.usefixtures("socket_enabled")


async def _server(aiohttp_server, handlers):
    app = web.Application()
    for method, path, handler in handlers:
        app.router.add_route(method, path, handler)
    return await aiohttp_server(app)


def _auth(request: web.Request) -> bool:
    return request.headers.get("Authorization") == "Bearer tok"


async def test_status_and_errors(aiohttp_server) -> None:
    async def status(request):
        if not _auth(request):
            return web.json_response({"error": "no"}, status=401)
        return web.json_response({"state": "idle"})

    async def sync(request):
        return web.json_response({"error": "busy"}, status=409)

    srv = await _server(aiohttp_server, [("GET", "/api/v1/status", status), ("POST", "/api/v1/sync", sync)])
    async with ClientSession() as session:
        client = ConfigSyncClient(session, srv.host, srv.port, "tok")
        assert (await client.status())["state"] == "idle"
        with pytest.raises(ConfigSyncBusyError):
            await client.sync()
        with pytest.raises(ConfigSyncAuthError):
            await ConfigSyncClient(session, srv.host, srv.port, "wrong").status()
        with pytest.raises(ConfigSyncError):
            await ConfigSyncClient(session, "127.0.0.1", 1, "tok").status()


async def test_sync_sends_only_fields_the_app_accepts(aiohttp_server) -> None:
    async def sync(request):
        # The app rejects unknown fields (its body is {"dry_run": bool}).
        body = await request.json()
        if set(body) - {"dry_run"}:
            return web.json_response({"error": f"unknown fields {sorted(body)}"}, status=400)
        return web.json_response({"job_id": "j1"}, status=202)

    srv = await _server(aiohttp_server, [("POST", "/api/v1/sync", sync)])
    async with ClientSession() as session:
        assert await ConfigSyncClient(session, srv.host, srv.port, "tok").sync() == "j1"


async def test_event_stream(aiohttp_server) -> None:
    async def events(request):
        if not _auth(request):
            return web.Response(status=401)
        resp = web.StreamResponse(headers={"Content-Type": "text/event-stream"})
        await resp.prepare(request)
        await resp.write(b"event: hello\ndata: {}\n\n")
        await resp.write(b": keepalive\n\n")
        await resp.write(b'event: job\ndata: {"id": "j1", "status": "running"}\n\n')
        await resp.write(b'event: job\ndata: {"id": "j1", "status": "done"}\n\n')
        await asyncio.sleep(0)
        return resp

    srv = await _server(aiohttp_server, [("GET", "/api/v1/events", events)])
    async with ClientSession() as session:
        got = [e async for e in ConfigSyncClient(session, srv.host, srv.port, "tok").events()]
        assert got == [
            ("hello", {}),
            ("job", {"id": "j1", "status": "running"}),
            ("job", {"id": "j1", "status": "done"}),
        ]
        with pytest.raises(ConfigSyncAuthError):
            async for _ in ConfigSyncClient(session, srv.host, srv.port, "wrong").events():
                pass
