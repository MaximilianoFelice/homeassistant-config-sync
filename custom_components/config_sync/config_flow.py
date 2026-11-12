"""Config flow: set up from Supervisor discovery, or by hand."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

import voluptuous as vol

from homeassistant.config_entries import ConfigFlow, ConfigFlowResult
from homeassistant.const import CONF_HOST, CONF_PORT, CONF_TOKEN
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.service_info.hassio import HassioServiceInfo

from .api import ConfigSyncAuthError, ConfigSyncClient, ConfigSyncError
from .const import DEFAULT_NAME, DEFAULT_PORT, DOMAIN

USER_SCHEMA = vol.Schema(
    {
        vol.Required(CONF_HOST): str,
        vol.Required(CONF_PORT, default=DEFAULT_PORT): int,
        vol.Required(CONF_TOKEN): str,
    }
)


class ConfigSyncConfigFlow(ConfigFlow, domain=DOMAIN):
    """One entry per app: the app announces itself through discovery."""

    VERSION = 1

    def __init__(self) -> None:
        self._discovered: dict[str, Any] = {}

    async def _validate(self, data: dict[str, Any]) -> str | None:
        """Return an error key, or None if the app answers with that token."""
        client = ConfigSyncClient(
            async_get_clientsession(self.hass), data[CONF_HOST], data[CONF_PORT], data[CONF_TOKEN]
        )
        try:
            await client.status()
        except ConfigSyncAuthError:
            return "invalid_auth"
        except ConfigSyncError:
            return "cannot_connect"
        return None

    async def async_step_user(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Set up by hand: host, port and the app's API token."""
        errors: dict[str, str] = {}
        if user_input is not None:
            await self.async_set_unique_id(DOMAIN)
            self._abort_if_unique_id_configured()
            if (error := await self._validate(user_input)) is None:
                return self.async_create_entry(title=DEFAULT_NAME, data=user_input)
            errors["base"] = error
        return self.async_show_form(
            step_id="user",
            data_schema=self.add_suggested_values_to_schema(USER_SCHEMA, user_input or {}),
            errors=errors,
        )

    async def async_step_hassio(self, discovery_info: HassioServiceInfo) -> ConfigFlowResult:
        """The app announced itself through Supervisor discovery."""
        cfg = discovery_info.config
        self._discovered = {
            CONF_HOST: cfg["host"],
            CONF_PORT: int(cfg.get("port", DEFAULT_PORT)),
            CONF_TOKEN: cfg["token"],
        }
        await self.async_set_unique_id(DOMAIN)
        # If the app moved or its token changed, update the existing entry.
        self._abort_if_unique_id_configured(updates=self._discovered)
        self.context["title_placeholders"] = {"name": discovery_info.name or DEFAULT_NAME}
        return await self.async_step_hassio_confirm()

    async def async_step_reauth(self, entry_data: Mapping[str, Any]) -> ConfigFlowResult:
        """The app rejected the token (it was regenerated)."""
        return await self.async_step_reauth_confirm()

    async def async_step_reauth_confirm(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Ask for the new API token."""
        entry = self._get_reauth_entry()
        errors: dict[str, str] = {}
        if user_input is not None:
            data = {**entry.data, CONF_TOKEN: user_input[CONF_TOKEN]}
            if (error := await self._validate(data)) is None:
                return self.async_update_reload_and_abort(entry, data=data)
            errors["base"] = error
        return self.async_show_form(
            step_id="reauth_confirm", data_schema=vol.Schema({vol.Required(CONF_TOKEN): str}), errors=errors
        )

    async def async_step_hassio_confirm(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Confirm the discovered app."""
        errors: dict[str, str] = {}
        if user_input is not None:
            if (error := await self._validate(self._discovered)) is None:
                return self.async_create_entry(title=DEFAULT_NAME, data=self._discovered)
            errors["base"] = error
        return self.async_show_form(step_id="hassio_confirm", errors=errors)
