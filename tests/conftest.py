"""Shared fixtures for the config_sync integration tests."""

from __future__ import annotations

import pytest

from homeassistant.const import CONF_HOST, CONF_PORT, CONF_TOKEN

pytest_plugins = "pytest_homeassistant_custom_component"

DATA = {CONF_HOST: "local-homeassistant-config-sync", CONF_PORT: 8099, CONF_TOKEN: "test-token"}
BASE = "http://local-homeassistant-config-sync:8099/api/v1"

STATUS_IDLE = {
    "state": "idle",
    "syncing": False,
    "last_sync": "2026-11-12T02:00:00Z",
    "box_head": "a" * 40,
    "main_head": "a" * 40,
}


@pytest.fixture(autouse=True)
def auto_enable_custom_integrations(enable_custom_integrations):
    """Load integrations from custom_components/."""
    return
