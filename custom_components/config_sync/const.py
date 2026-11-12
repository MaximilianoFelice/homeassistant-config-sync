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
    "rejected",
    "blocked_secrets",
    "bootstrap_review",
    "suspicious_snapshot",
    "error",
]

# States that need the user to do something.
ATTENTION_STATES: Final = frozenset(
    {"conflict", "rejected", "blocked_secrets", "bootstrap_review", "suspicious_snapshot", "error"}
)
