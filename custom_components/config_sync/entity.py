"""Base entity: one device for the app."""

from __future__ import annotations

from homeassistant.helpers.device_registry import DeviceEntryType, DeviceInfo
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import DEFAULT_NAME, DOMAIN
from .coordinator import ConfigSyncCoordinator


class ConfigSyncEntity(CoordinatorEntity[ConfigSyncCoordinator]):
    """Entity bound to the app's status."""

    _attr_has_entity_name = True

    def __init__(self, coordinator: ConfigSyncCoordinator, key: str) -> None:
        super().__init__(coordinator)
        entry_id = coordinator.config_entry.entry_id
        self._attr_unique_id = f"{entry_id}_{key}"
        self._attr_translation_key = key
        self._attr_device_info = DeviceInfo(
            identifiers={(DOMAIN, entry_id)},
            name=DEFAULT_NAME,
            manufacturer=DEFAULT_NAME,
            model="Home Assistant app",
            entry_type=DeviceEntryType.SERVICE,
        )

    @property
    def state_key(self) -> str:
        """Engine state, or "syncing" while a cycle runs."""
        data = self.coordinator.data or {}
        return "syncing" if data.get("syncing") else str(data.get("state") or "idle")
