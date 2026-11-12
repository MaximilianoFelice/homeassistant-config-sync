# Config de ejemplo para el E2E

Una configuración de Home Assistant **sintética** con la forma de un box real: packages, includes, `!secret`, ESPHome, themes, `python_scripts`, un dashboard en `.storage/lovelace`, una integración de HACS (que no se versiona) y la config de una app en `app_configs/`.

`scripts/e2e-sandbox.sh` la copia a un sandbox y en el momento agrega lo que **no** puede estar en el repo:
- `secrets.yaml` falso (`scripts/fake-secrets.sh`);
- una base de datos, un log y un binario de mentira, para probar que nunca viajan;
- un YAML de ESPHome con una clave escrita a mano, para probar el escáner de secretos.
