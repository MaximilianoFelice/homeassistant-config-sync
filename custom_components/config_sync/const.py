"""Constants for the Git Config Sync integration."""

from __future__ import annotations

from datetime import timedelta
from typing import Final

DOMAIN: Final = "config_sync"
DEFAULT_NAME: Final = "Git Config Sync"
DEFAULT_PORT: Final = 8099

# The app pushes job events over SSE; polling is only the safety net.
UPDATE_INTERVAL: Final = timedelta(minutes=2)

# Engine states, as reported by the app's API, plus "syncing" while a cycle runs.
STATES: Final = [
    "idle",
    "syncing",
    "conflict",
    "review",
    "mass_delete",
    "too_large",
    "path_collision",
    "blocked_secrets",
    "check_failed",
    "error",
]

# States that need the user to do something: every state but idle and syncing.
ATTENTION_STATES: Final = frozenset(STATES) - {"idle", "syncing"}
