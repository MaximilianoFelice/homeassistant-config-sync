# Sample config for the E2E

A **synthetic** Home Assistant configuration shaped like a real box: packages, includes, `!secret`, ESPHome, themes, `python_scripts`, a dashboard in `.storage/lovelace`, a HACS integration (which is not versioned), and an app's config in `app_configs/`.

`scripts/e2e-sandbox.sh` copies it into a sandbox and adds, at run time, what **cannot** be in the repo:
- a fake `secrets.yaml` (`scripts/fake-secrets.sh`);
- a dummy database, log, and binary, to prove they never travel;
- an ESPHome YAML with a hand-written key, to exercise the secret scanner.
