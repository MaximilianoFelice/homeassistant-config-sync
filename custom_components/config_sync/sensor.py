"""Sensors: sync state and last sync time."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from homeassistant.components.sensor import SensorDeviceClass, SensorEntity
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback
from homeassistant.util import dt as dt_util

from .const import STATES
from .coordinator import ConfigSyncConfigEntry
from .entity import ConfigSyncEntity


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigSyncConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Set up the sensors."""
    coordinator = entry.runtime_data
    async_add_entities([StateSensor(coordinator, "state"), LastSyncSensor(coordinator, "last_sync")])


class StateSensor(ConfigSyncEntity, SensorEntity):
    """What the engine is doing or waiting for."""

    _attr_device_class = SensorDeviceClass.ENUM
    _attr_options = STATES

    @property
    def native_value(self) -> str:
        return self.state_key if self.state_key in STATES else "error"

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        data = self.coordinator.data or {}
        attrs: dict[str, Any] = {
            "box_head": data.get("box_head"),
            "main_head": data.get("main_head"),
        }
        if data.get("last_error"):
            attrs["last_error"] = data["last_error"]
        if data.get("conflict_paths"):
            attrs["conflict_paths"] = data["conflict_paths"]
        if data.get("findings"):
            attrs["findings"] = [f"{f['path']}:{f['line']} ({f['rule']})" for f in data["findings"]]
        return attrs


class LastSyncSensor(ConfigSyncEntity, SensorEntity):
    """When the last cycle finished."""

    _attr_device_class = SensorDeviceClass.TIMESTAMP

    @property
    def native_value(self) -> datetime | None:
        raw = (self.coordinator.data or {}).get("last_sync")
        if not raw or str(raw).startswith("0001-"):
            return None
        return dt_util.parse_datetime(str(raw))
