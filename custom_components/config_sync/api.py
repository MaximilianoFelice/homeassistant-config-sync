"""Client for the Git Config Sync app's HTTP API."""

from __future__ import annotations

from collections.abc import AsyncIterator
import json
from typing import Any

import aiohttp


class ConfigSyncError(Exception):
    """The app could not be reached or answered with an error."""


class ConfigSyncAuthError(ConfigSyncError):
    """The API token was rejected."""


class ConfigSyncBusyError(ConfigSyncError):
    """A sync is already running."""


class ConfigSyncClient:
    """Talks to the app's API (`/api/v1`) with its bearer token."""

    def __init__(self, session: aiohttp.ClientSession, host: str, port: int, token: str) -> None:
        self._session = session
        self._base = f"http://{host}:{port}/api/v1"
        self._headers = {"Authorization": f"Bearer {token}"}

    async def _request(self, method: str, path: str, payload: dict[str, Any] | None = None) -> Any:
        try:
            async with self._session.request(
                method,
                f"{self._base}/{path}",
                headers=self._headers,
                json=payload,
                timeout=aiohttp.ClientTimeout(total=30),
            ) as resp:
                if resp.status == 401:
                    raise ConfigSyncAuthError("the API token was rejected")
                if resp.status == 409:
                    raise ConfigSyncBusyError("a sync is already running")
                if resp.status >= 400:
                    raise ConfigSyncError(f"{method} {path}: HTTP {resp.status}")
                return await resp.json(content_type=None)
        except (aiohttp.ClientError, TimeoutError) as err:
            raise ConfigSyncError(f"{method} {path}: {err}") from err

    async def status(self) -> dict[str, Any]:
        """Return the engine state (GET /status)."""
        return await self._request("GET", "status")

    async def sync(self, accept_bootstrap: bool = False) -> str:
        """Start a sync cycle and return the job id (POST /sync)."""
        data = await self._request("POST", "sync", {"accept_bootstrap": accept_bootstrap})
        return str(data["job_id"])

    async def events(self) -> AsyncIterator[tuple[str, Any]]:
        """Yield (event, data) from the server-sent events stream until it closes."""
        try:
            async with self._session.get(
                f"{self._base}/events",
                headers=self._headers,
                timeout=aiohttp.ClientTimeout(total=None, sock_connect=10, sock_read=120),
            ) as resp:
                if resp.status == 401:
                    raise ConfigSyncAuthError("the API token was rejected")
                if resp.status >= 400:
                    raise ConfigSyncError(f"GET events: HTTP {resp.status}")
                event, data = "message", ""
                async for raw in resp.content:
                    line = raw.decode("utf-8", "replace").rstrip("\r\n")
                    if not line:
                        if data:
                            try:
                                parsed: Any = json.loads(data)
                            except ValueError:
                                parsed = data
                            yield event, parsed
                        event, data = "message", ""
                    elif line.startswith(":"):
                        continue
                    elif line.startswith("event:"):
                        event = line[6:].strip()
                    elif line.startswith("data:"):
                        data += line[5:].strip()
        except (aiohttp.ClientError, TimeoutError) as err:
            raise ConfigSyncError(f"GET events: {err}") from err
