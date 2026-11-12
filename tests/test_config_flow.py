"""Config flow: by hand, from Supervisor discovery, and reauth."""

from __future__ import annotations

from unittest.mock import AsyncMock, patch

import pytest
from pytest_homeassistant_custom_component.common import MockConfigEntry

from custom_components.config_sync.api import ConfigSyncAuthError, ConfigSyncError
from custom_components.config_sync.const import DOMAIN
from homeassistant import config_entries
from homeassistant.const import CONF_TOKEN
from homeassistant.core import HomeAssistant
from homeassistant.data_entry_flow import FlowResultType
from homeassistant.helpers.service_info.hassio import HassioServiceInfo

from .conftest import DATA, STATUS_IDLE

STATUS = "custom_components.config_sync.config_flow.ConfigSyncClient.status"
SETUP = "custom_components.config_sync.async_setup_entry"


async def test_user_flow(hass: HomeAssistant) -> None:
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    assert result["type"] is FlowResultType.FORM
    with patch(STATUS, AsyncMock(return_value=STATUS_IDLE)), patch(SETUP, AsyncMock(return_value=True)):
        result = await hass.config_entries.flow.async_configure(result["flow_id"], DATA)
    assert result["type"] is FlowResultType.CREATE_ENTRY
    assert result["title"] == "Git Config Sync"
    assert result["data"] == DATA


@pytest.mark.parametrize(("error", "key"), [(ConfigSyncAuthError("x"), "invalid_auth"), (ConfigSyncError("x"), "cannot_connect")])
async def test_user_flow_errors(hass: HomeAssistant, error: Exception, key: str) -> None:
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    with patch(STATUS, AsyncMock(side_effect=error)):
        result = await hass.config_entries.flow.async_configure(result["flow_id"], DATA)
    assert result["type"] is FlowResultType.FORM
    assert result["errors"] == {"base": key}

    # Fixing it completes the flow.
    with patch(STATUS, AsyncMock(return_value=STATUS_IDLE)), patch(SETUP, AsyncMock(return_value=True)):
        result = await hass.config_entries.flow.async_configure(result["flow_id"], DATA)
    assert result["type"] is FlowResultType.CREATE_ENTRY


async def test_only_one_entry(hass: HomeAssistant) -> None:
    MockConfigEntry(domain=DOMAIN, unique_id=DOMAIN, data=DATA).add_to_hass(hass)
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    with patch(STATUS, AsyncMock(return_value=STATUS_IDLE)):
        result = await hass.config_entries.flow.async_configure(result["flow_id"], DATA)
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "already_configured"


def _discovery(token: str = "test-token") -> HassioServiceInfo:
    return HassioServiceInfo(
        config={"host": "local-homeassistant-config-sync", "port": 8099, "token": token},
        name="Git Config Sync",
        slug="local_homeassistant_config_sync",
        uuid="0123456789abcdef",
    )


async def test_hassio_discovery(hass: HomeAssistant) -> None:
    result = await hass.config_entries.flow.async_init(
        DOMAIN, context={"source": config_entries.SOURCE_HASSIO}, data=_discovery()
    )
    assert result["type"] is FlowResultType.FORM
    assert result["step_id"] == "hassio_confirm"
    with patch(STATUS, AsyncMock(return_value=STATUS_IDLE)), patch(SETUP, AsyncMock(return_value=True)):
        result = await hass.config_entries.flow.async_configure(result["flow_id"], {})
    assert result["type"] is FlowResultType.CREATE_ENTRY
    assert result["data"] == DATA


async def test_hassio_discovery_updates_existing_entry(hass: HomeAssistant) -> None:
    entry = MockConfigEntry(domain=DOMAIN, unique_id=DOMAIN, data={**DATA, CONF_TOKEN: "old"})
    entry.add_to_hass(hass)
    result = await hass.config_entries.flow.async_init(
        DOMAIN, context={"source": config_entries.SOURCE_HASSIO}, data=_discovery("new")
    )
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "already_configured"
    assert entry.data[CONF_TOKEN] == "new"


async def test_reauth(hass: HomeAssistant) -> None:
    entry = MockConfigEntry(domain=DOMAIN, unique_id=DOMAIN, data={**DATA, CONF_TOKEN: "old"})
    entry.add_to_hass(hass)
    result = await entry.start_reauth_flow(hass)
    assert result["step_id"] == "reauth_confirm"
    with patch(STATUS, AsyncMock(side_effect=ConfigSyncAuthError("x"))):
        result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_TOKEN: "wrong"})
    assert result["errors"] == {"base": "invalid_auth"}
    with patch(STATUS, AsyncMock(return_value=STATUS_IDLE)), patch(SETUP, AsyncMock(return_value=True)):
        result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_TOKEN: "fresh"})
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "reauth_successful"
    assert entry.data[CONF_TOKEN] == "fresh"
