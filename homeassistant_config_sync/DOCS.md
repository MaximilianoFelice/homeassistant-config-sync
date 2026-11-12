# Config Sync

Sincroniza en las dos direcciones la configuración de Home Assistant con un repo git.

- Lo que cambia en Home Assistant (UI, editor de archivos) se commitea en la rama `box` y llega a `main`.
- Lo que se pushea a `main` se aplica solo si pasa el check de config. Si no pasa, se vuelve atrás y te avisa.
- Si las dos puntas tocaron lo mismo, frena y te avisa. Resolvés el merge como siempre (en tu PC o en GitHub).
- `secrets.yaml`, las bases de datos, `.storage` (salvo los dashboards) y las claves nunca salen del box.

## Puesta en marcha

1. Creá un repo (privado) en GitHub.
2. En **Configuración**, poné `repository: git@github.com:vos/tu-repo.git`.
3. Arrancá la app. En el **Registro** aparece la deploy key (`ssh-ed25519 …`).
4. En GitHub: tu repo → Settings → Deploy keys → Add deploy key. Pegala y tildá **Allow write access**.
5. Listo. Si el repo ya tenía contenido, el primer sync sube la rama `box` y espera a que la revises y la mergees en `main`.

## Disparar un sync a mano

Acción `hassio.addon_stdin` con `addon: local_homeassistant_config_sync` e `input: sync`.

## Estado

La app dispara el evento `homeassistant_config_sync` y, cuando hace falta que hagas algo, deja una notificación persistente.

Más detalle en el [diseño](https://github.com/MaximilianoFelice/homeassistant-config-sync/blob/main/docs/design.md).
