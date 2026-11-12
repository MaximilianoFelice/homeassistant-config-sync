"""Binary sensors: something needs you; a restart is recommended."""

from __future__ import annotations

from homeassistant.components.binary_sensor import BinarySensorDeviceClass, BinarySensorEntity
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import ATTENTION_STATES
from .coordinator import ConfigSyncConfigEntry
from .entity import ConfigSyncEntity


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigSyncConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Set up the binary sensors."""
    coordinator = entry.runtime_data
    async_add_entities([AttentionSensor(coordinator, "attention"), RestartSensor(coordinator, "restart_recommended")])


class AttentionSensor(ConfigSyncEntity, BinarySensorEntity):
    """On when the sync is stopped waiting for you (conflict, rejection, …)."""

    _attr_device_class = BinarySensorDeviceClass.PROBLEM

    @property
    def is_on(self) -> bool:
        return self.state_key in ATTENTION_STATES


class RestartSensor(ConfigSyncEntity, BinarySensorEntity):
    """On when applied changes need a Home Assistant restart."""

    @property
    def is_on(self) -> bool:
        return bool((self.coordinator.data or {}).get("restart_recommended"))
