"""The Git Config Sync integration: sensors and a button for the app."""

from __future__ import annotations

from homeassistant.const import CONF_HOST, CONF_PORT, CONF_TOKEN, Platform
from homeassistant.core import HomeAssistant
from homeassistant.helpers.aiohttp_client import async_get_clientsession

from .api import ConfigSyncClient
from .coordinator import ConfigSyncConfigEntry, ConfigSyncCoordinator

PLATFORMS: list[Platform] = [Platform.BINARY_SENSOR, Platform.BUTTON, Platform.SENSOR]


async def async_setup_entry(hass: HomeAssistant, entry: ConfigSyncConfigEntry) -> bool:
    """Set up Git Config Sync from a config entry."""
    client = ConfigSyncClient(
        async_get_clientsession(hass), entry.data[CONF_HOST], entry.data[CONF_PORT], entry.data[CONF_TOKEN]
    )
    coordinator = ConfigSyncCoordinator(hass, entry, client)
    await coordinator.async_config_entry_first_refresh()
    entry.runtime_data = coordinator
    entry.async_create_background_task(hass, coordinator.listen_events(), f"{entry.domain} events")
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    return True


async def async_unload_entry(hass: HomeAssistant, entry: ConfigSyncConfigEntry) -> bool:
    """Unload a config entry (background tasks are cancelled by HA)."""
    return await hass.config_entries.async_unload_platforms(entry, PLATFORMS)
