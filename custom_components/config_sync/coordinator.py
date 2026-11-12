"""Data coordinator: the app's status, refreshed on its events."""

from __future__ import annotations

import asyncio
import logging
from typing import Any

from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import ConfigEntryAuthFailed
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed

from .api import ConfigSyncAuthError, ConfigSyncClient, ConfigSyncError
from .const import DOMAIN, UPDATE_INTERVAL

_LOGGER = logging.getLogger(__name__)

# Reconnect delays for the event stream (seconds).
_BACKOFF = (5, 10, 30, 60)

type ConfigSyncConfigEntry = ConfigEntry[ConfigSyncCoordinator]


class ConfigSyncCoordinator(DataUpdateCoordinator[dict[str, Any]]):
    """Polls GET /status and refreshes immediately on job events."""

    config_entry: ConfigSyncConfigEntry

    def __init__(self, hass: HomeAssistant, entry: ConfigSyncConfigEntry, client: ConfigSyncClient) -> None:
        super().__init__(hass, _LOGGER, config_entry=entry, name=DOMAIN, update_interval=UPDATE_INTERVAL)
        self.client = client

    async def _async_update_data(self) -> dict[str, Any]:
        try:
            return await self.client.status()
        except ConfigSyncAuthError as err:
            raise ConfigEntryAuthFailed(str(err)) from err
        except ConfigSyncError as err:
            raise UpdateFailed(str(err)) from err

    async def listen_events(self) -> None:
        """Refresh on every job event; reconnect with backoff. Runs until cancelled."""
        attempt = 0
        while True:
            try:
                async for event, _ in self.client.events():
                    attempt = 0
                    if event == "job":
                        await self.async_request_refresh()
            except ConfigSyncAuthError:
                _LOGGER.warning("Git Config Sync rejected the API token; event stream stopped")
                return
            except ConfigSyncError as err:
                _LOGGER.debug("Event stream interrupted: %s", err)
            await asyncio.sleep(_BACKOFF[min(attempt, len(_BACKOFF) - 1)])
            attempt += 1
