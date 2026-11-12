"""Setup, entities and the Sync now button, against a mocked app API."""

from __future__ import annotations

from http import HTTPStatus
from unittest.mock import AsyncMock, patch

import pytest
from pytest_homeassistant_custom_component.common import MockConfigEntry
from pytest_homeassistant_custom_component.test_util.aiohttp import AiohttpClientMocker

from custom_components.config_sync.const import DOMAIN
from homeassistant.config_entries import ConfigEntryState
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError

from .conftest import BASE, DATA, STATUS_IDLE

CONFLICT = {
    **STATUS_IDLE,
    "state": "conflict",
    "conflict_paths": ["homeassistant/automations.yaml"],
    "restart_recommended": True,
    "last_error": "",
}


@pytest.fixture(autouse=True)
def no_event_stream():
    """The SSE listener is tested in test_api.py; here it would just loop."""
    with patch("custom_components.config_sync.coordinator.ConfigSyncCoordinator.listen_events", AsyncMock()):
        yield


async def _setup(hass: HomeAssistant) -> MockConfigEntry:
    entry = MockConfigEntry(domain=DOMAIN, unique_id=DOMAIN, data=DATA)
    entry.add_to_hass(hass)
    await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()
    return entry


async def test_entities(hass: HomeAssistant, aioclient_mock: AiohttpClientMocker) -> None:
    aioclient_mock.get(f"{BASE}/status", json=CONFLICT)
    entry = await _setup(hass)
    assert entry.state is ConfigEntryState.LOADED

    state = hass.states.get("sensor.git_config_sync_sync_state")
    assert state.state == "conflict"
    assert state.attributes["conflict_paths"] == ["homeassistant/automations.yaml"]
    assert hass.states.get("binary_sensor.git_config_sync_needs_attention").state == "on"
    assert hass.states.get("binary_sensor.git_config_sync_restart_recommended").state == "on"
    assert hass.states.get("sensor.git_config_sync_last_sync").state == "2026-11-12T02:00:00+00:00"

    # The token is sent on every call.
    assert aioclient_mock.mock_calls[0][3]["Authorization"] == "Bearer test-token"

    assert await hass.config_entries.async_unload(entry.entry_id)
    assert entry.state is ConfigEntryState.NOT_LOADED


async def test_review_and_secret_findings(hass: HomeAssistant, aioclient_mock: AiohttpClientMocker) -> None:
    finding = {"path": "homeassistant/a.yaml", "line": 3, "rule": "github-pat", "fingerprint": "homeassistant/a.yaml:github-pat:3"}
    aioclient_mock.get(f"{BASE}/status", json={**STATUS_IDLE, "state": "review", "review": "first_sync", "findings": [finding]})
    entry = await _setup(hass)

    state = hass.states.get("sensor.git_config_sync_sync_state")
    assert state.state == "review"
    assert state.attributes["review"] == "first_sync"
    assert hass.states.get("binary_sensor.git_config_sync_needs_attention").state == "on"
    secrets = hass.states.get("sensor.git_config_sync_possible_secrets")
    assert secrets.state == "1"
    assert secrets.attributes["findings"] == ["homeassistant/a.yaml:3 (github-pat)"]
    assert secrets.attributes["fingerprints"] == ["homeassistant/a.yaml:github-pat:3"]
    await hass.config_entries.async_unload(entry.entry_id)


async def test_syncing_overrides_state(hass: HomeAssistant, aioclient_mock: AiohttpClientMocker) -> None:
    aioclient_mock.get(f"{BASE}/status", json={**STATUS_IDLE, "syncing": True})
    entry = await _setup(hass)
    assert hass.states.get("sensor.git_config_sync_sync_state").state == "syncing"
    assert hass.states.get("binary_sensor.git_config_sync_needs_attention").state == "off"
    await hass.config_entries.async_unload(entry.entry_id)


async def test_sync_now_button(hass: HomeAssistant, aioclient_mock: AiohttpClientMocker) -> None:
    aioclient_mock.get(f"{BASE}/status", json=STATUS_IDLE)
    aioclient_mock.post(f"{BASE}/sync", json={"job_id": "j1"})
    entry = await _setup(hass)
    await hass.services.async_call("button", "press", {"entity_id": "button.git_config_sync_sync_now"}, blocking=True)
    posts = [c for c in aioclient_mock.mock_calls if c[0] == "POST"]
    assert len(posts) == 1
    assert str(posts[0][1]).endswith("/api/v1/sync")
    await hass.config_entries.async_unload(entry.entry_id)


async def test_sync_now_when_busy(hass: HomeAssistant, aioclient_mock: AiohttpClientMocker) -> None:
    aioclient_mock.get(f"{BASE}/status", json=STATUS_IDLE)
    aioclient_mock.post(f"{BASE}/sync", status=HTTPStatus.CONFLICT, json={"error": "a sync is already running"})
    entry = await _setup(hass)
    with pytest.raises(HomeAssistantError):
        await hass.services.async_call(
            "button", "press", {"entity_id": "button.git_config_sync_sync_now"}, blocking=True
        )
    await hass.config_entries.async_unload(entry.entry_id)


async def test_unreachable_app_retries(hass: HomeAssistant, aioclient_mock: AiohttpClientMocker) -> None:
    aioclient_mock.get(f"{BASE}/status", exc=TimeoutError())
    entry = await _setup(hass)
    assert entry.state is ConfigEntryState.SETUP_RETRY


async def test_rejected_token_starts_reauth(hass: HomeAssistant, aioclient_mock: AiohttpClientMocker) -> None:
    aioclient_mock.get(f"{BASE}/status", status=HTTPStatus.UNAUTHORIZED)
    entry = await _setup(hass)
    assert entry.state is ConfigEntryState.SETUP_ERROR
    flows = hass.config_entries.flow.async_progress()
    assert any(f["context"]["source"] == "reauth" for f in flows)
