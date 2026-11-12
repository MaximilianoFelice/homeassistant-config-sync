"""Prints the config flows in progress as JSON, through Home Assistant's
websocket API (the REST listing was removed). Runs inside the Core container,
which has aiohttp: `docker exec -i -e HA_TOKEN=... homeassistant python3 - < scripts/ha-flows.py`.
"""

import asyncio
import json
import os

import aiohttp


async def main() -> None:
    async with aiohttp.ClientSession() as session:
        async with session.ws_connect("http://127.0.0.1:80/api/websocket") as ws:
            await ws.receive_json()  # auth_required
            await ws.send_json({"type": "auth", "access_token": os.environ["HA_TOKEN"]})
            auth = await ws.receive_json()
            if auth.get("type") != "auth_ok":
                raise SystemExit(f"auth failed: {auth}")
            await ws.send_json({"id": 1, "type": "config_entries/flow/progress"})
            while True:
                msg = await ws.receive_json()
                if msg.get("id") == 1:
                    print(json.dumps(msg.get("result", [])))
                    return


asyncio.run(main())
