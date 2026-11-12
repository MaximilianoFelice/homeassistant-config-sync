"""Button: sync now."""

from __future__ import annotations

from homeassistant.components.button import ButtonEntity
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .api import ConfigSyncBusyError, ConfigSyncError
from .const import DOMAIN
from .coordinator import ConfigSyncConfigEntry
from .entity import ConfigSyncEntity


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigSyncConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Set up the button."""
    async_add_entities([SyncNowButton(entry.runtime_data, "sync_now")])


class SyncNowButton(ConfigSyncEntity, ButtonEntity):
    """Start a sync cycle."""

    async def async_press(self) -> None:
        try:
            await self.coordinator.client.sync()
        except ConfigSyncBusyError as err:
            raise HomeAssistantError(translation_domain=DOMAIN, translation_key="busy") from err
        except ConfigSyncError as err:
            raise HomeAssistantError(
                translation_domain=DOMAIN, translation_key="sync_failed", translation_placeholders={"error": str(err)}
            ) from err
        await self.coordinator.async_request_refresh()
