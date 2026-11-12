"""Sensors: sync state, last sync time and the secret scanner's findings."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from homeassistant.components.sensor import SensorDeviceClass, SensorEntity, SensorStateClass
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
    async_add_entities(
        [
            StateSensor(coordinator, "state"),
            LastSyncSensor(coordinator, "last_sync"),
            SecretsSensor(coordinator, "secret_findings"),
        ]
    )


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
        for key in ("review", "last_error", "check_errors", "conflict_paths", "too_large", "collisions"):
            if data.get(key):
                attrs[key] = data[key]
        if data.get("deleted"):
            attrs["deleted_count"] = len(data["deleted"])
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


class SecretsSensor(ConfigSyncEntity, SensorEntity):
    """How many possible secrets the scanner sees in the repo (never the values)."""

    _attr_state_class = SensorStateClass.MEASUREMENT

    @property
    def native_value(self) -> int:
        return len((self.coordinator.data or {}).get("findings") or [])

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        findings = (self.coordinator.data or {}).get("findings") or []
        return {
            "findings": [f"{f['path']}:{f['line']} ({f['rule']})" for f in findings],
            "fingerprints": [f["fingerprint"] for f in findings],
        }
