# homeassistant-config-sync: diseño

> Estado: borrador para revisión, 2026-11-11. Nada de esto está publicado todavía.

## 1. Objetivo

Una app (add-on) de Home Assistant OS que mantiene la configuración del box sincronizada **en las dos direcciones** con un repo git:

- Lo que cambia en el box (ediciones desde la UI de HA, el editor de archivos, otras apps) termina commiteado y pusheado.
- Lo que se pushea a `main` desde otro lado (local, GitHub) termina aplicado en el box, **solo si pasa el check de config de HA**.
- Si las dos puntas tocan lo mismo de forma incompatible, el sync **se frena** y avisa. Nunca se pisa trabajo de nadie en silencio.

Es open source desde el día uno y se distribuye como repo de apps de HA (no por HACS, que no distribuye apps). Una integración HACS opcional llega después, como capa de presentación.

### No-objetivos

- No es un sistema de backups: no versiona `.storage` completo, ni DBs, ni media. Para eso están los backups de HA.
- No reinicia Home Assistant por su cuenta (salvo opt-in explícito).
- No gestiona secretos: `secrets.yaml` nunca sale del box.
- No resuelve conflictos automáticamente en v0/v1.

## 2. Glosario

| Término | Significado |
|---|---|
| **box** | La instancia de HA OS donde corre la app. |
| **disco vivo** | Los directorios montados en la app: `/homeassistant` y `/addon_configs`. |
| **path gestionado** | Un archivo del disco vivo que entra por la whitelist de su mapeo. Solo esos se leen, escriben o borran. |
| **rama `box`** | Rama que refleja el estado gestionado del box. La escribe solo el motor. |
| **rama `main`** | Rama de trabajo humano. La escriben los usuarios; el motor solo la avanza con fast-forward. |
| **ciclo** | Una ejecución completa del sync (manual o programada). |
| **motor** | El binario que implementa el ciclo (CLI en v0, proceso de la app desde v1). |

## 3. Arquitectura

```
                 GitHub (git@github.com:owner/repo.git, deploy key SSH)
                 ┌──────────────────────────────────────────────┐
                 │  main  ◄── pushes humanos (local, web)          │
                 │  box   ◄── solo el motor                      │
                 └───────────────▲──────────────────────────────┘
                                 │ fetch / push --atomic (SSH)
┌──────────────── App homeassistant-config-sync ─────────────────────────┐
│                                                                        │
│  scheduler ─┐                                                          │
│  stdin ─────┼──► motor (ciclo) ──► git CLI (repo bare en /data/repo.git)│
│  API HTTP ──┘        │                                                 │
│                      ├──► escaneo / apply ──► /homeassistant (rw)      │
│                      │                   └──► /addon_configs (rw)      │
│                      └──► HA Core API (http://supervisor/core/api,     │
│                            SUPERVISOR_TOKEN): check_config, reloads,   │
│                            notificaciones, eventos                     │
└────────────────────────────────────────────────────────────────────────┘
```

Decisiones de fondo:

- **El repo es bare y vive en `/data/repo.git`.** El disco vivo no es un working tree de git: el motor arma árboles con plumbing (`hash-object`, índice temporal vía `GIT_INDEX_FILE`, `write-tree`, `commit-tree`, `merge-tree --write-tree`) y escribe en disco solo los paths gestionados. Así un `.git` nunca aparece dentro de `/homeassistant`, y un monorepo puede mapear a varios directorios del box.
- **Se usa el git CLI, no una librería.** El merge de 3 vías de git (`merge-tree --write-tree`, git ≥ 2.38) es la parte más delicada y no conviene reimplementarla.

## 4. Mounts y layout del repo

### Mounts de la app

```yaml
map:
  - type: homeassistant_config   # → /homeassistant
    read_only: false
  - type: all_addon_configs      # → /addon_configs
    read_only: false
```

`/data` (privado de la app) guarda el estado del motor; no hace falta mapearlo.

### Monorepo con mapeo configurable

Por defecto:

| Prefijo en el repo | Directorio en el box |
|---|---|
| `config/` | `/homeassistant` |
| `app_configs/` | `/addon_configs` |

```yaml
mappings:
  - repo: config
    box: /homeassistant
    include: [...]      # ver §5
    push_only: [...]
    exclude: [...]
  - repo: app_configs
    box: /addon_configs
    include: []         # vacío por defecto: hay que elegir apps explícitamente
    push_only: []
    exclude: []
```

Reglas:

- Todo lo que está en el repo **fuera** de un prefijo mapeado (README, `.github/`, docs, `.gitattributes`) es del repo: el motor lo preserva tal cual en sus commits y nunca lo baja al box.
- Un path dentro de un prefijo mapeado que **no** entra por la whitelist se ignora en las dos direcciones y se reporta como "no gestionado".
- Se recomienda `* -text` en `.gitattributes` del repo para que nadie reescriba fines de línea al clonar en Windows. El motor escribe bytes tal cual.

## 5. Whitelist de paths

La whitelist vive **del lado del box** (opciones de la app), nunca en el repo. Si viviera en el repo, cualquiera con permiso de push podría ampliar la whitelist y sacar archivos del box hacia git: eso sería escalar de "escritor del repo" a "lector del box".

### Semántica

Un path es gestionado si y solo si:

1. está bajo el `box` de un mapeo,
2. matchea algún glob de `include` o de `push_only` (sintaxis doublestar: `*`, `**`, `?`, `[...]`; relativos a la raíz del mapeo),
3. no matchea ningún glob de `exclude`,
4. no matchea la **denylist dura**,
5. no es symlink, socket ni dispositivo (los symlinks se ignoran, nunca se siguen),
6. pesa menos que `max_file_size_kb` (default 1024) y no parece binario (byte NUL en los primeros 8 KiB).

Los puntos 5 y 6 producen avisos en "pendientes/ignorados", no errores.

La dirección sale de la lista que matchea:

- `include`: se sincroniza en las dos direcciones.
- `push_only`: solo box → repo. Lo que llegue desde `main` para ese path se ignora y se reporta como "cambio entrante no aplicable". Si un path matchea las dos listas, gana `push_only`.

Son tres listas de strings (y no reglas con un campo de dirección) para que el esquema de opciones del Supervisor las pueda validar.

Además, todo path que llega desde el repo tiene que ser relativo y limpio: sin `..`, `.`, componentes vacíos, barras invertidas, bytes de control ni componentes `.git`. Si no, se descarta como `invalid_path` antes de mirar la whitelist. Es la barrera contra un `main` que intente escribir fuera del mapeo.

### Denylist dura (no se puede desactivar)

Se evalúa en cualquier nivel de profundidad y sin distinguir mayúsculas (`Secrets.YAML` también cae):

```
**/secrets.yaml  **/secrets.yml
**/*.db  **/*.db-shm  **/*.db-wal  **/*.db-journal  **/*.sqlite*
**/.storage/auth*  **/.storage/core.config_entries  **/.storage/http*  **/.storage/cloud
**/.cloud/**  **/.ssh/**
**/id_rsa*  **/id_dsa*  **/id_ecdsa*  **/id_ed25519*  **/*.pem  **/*.key  **/*.p12  **/*.pfx
**/*.log  **/*.log.*
```

La fuente de verdad es `paths.HardDeny` en el código.

### Defaults del mapeo `config/`

```yaml
include:
  - "**/*.yaml"
  - "**/*.yml"
  - "custom_templates/**/*.jinja"
  - "python_scripts/**/*.py"
  - "pyscript/**/*.py"
push_only:
  - ".storage/lovelace*"
exclude:
  - ".cache/**"
  - "deps/**"
  - "tts/**"
  - "backups/**"
  - "custom_components/**" # lo gestiona HACS; opt-in si alguien lo quiere versionar
  - "esphome/.esphome/**"
  - "**/__pycache__/**"
  - "ip_bans.yaml"
```

Como es una whitelist, `.storage` queda afuera salvo lo que se incluye explícitamente (`lovelace*`): no hace falta excluirlo. `exclude` siempre gana sobre `include`, y la denylist dura gana sobre los dos.

**Por qué `.storage/lovelace*` es `push_only`:** HA tiene los dashboards en memoria y reescribe esos JSON cuando se guardan desde la UI. Escribirlos desde git con HA corriendo no se ve hasta un restart, y el próximo guardado desde la UI los pisa. Versionarlos sí sirve (historial, diffs). Bajarlos queda como opción futura con aviso de "requiere restart".

### Guards de seguridad del escaneo

- **Raíz vacía o ausente:** si el directorio de un mapeo no existe o no tiene ningún path gestionado y el árbol `box` sí tenía, el ciclo aborta (`suspicious_snapshot`). Es la protección contra un mount roto que se interpretaría como "borraron todo".
- **Borrado masivo:** si un snapshot borra más de `delete_guard_files` (default 25) paths gestionados, o al menos 5 que además superan el `delete_guard_percent` (default 20 %) de los gestionados, aborta y pide confirmación. El mínimo de 5 evita que borrar 1 de 4 archivos en una config chica frene el sync. Lo mismo aplica a un cambio entrante desde `main`.
- **Saltear nunca es borrar:** un archivo que se saltea (grande, binario, symlink, inestable, ilegible, o un directorio ilegible entero) conserva en `box` la versión que tenía.
- **Cache de stat:** se reusa el blob de un archivo si coinciden tamaño, `mtime`, `ctime`, inode y modo. Un archivo modificado hace menos de 2 s no entra a la cache, y como `ctime` cambia con cualquier escritura, restaurar el `mtime` a mano no esconde una edición.
- **Lecturas inestables:** se hashea cada archivo y se vuelve a hacer stat. Si cambió `mtime` o el tamaño durante la lectura, se reintenta (hasta 3 veces) y si sigue cambiando, ese path se deja para el próximo ciclo.

## 6. Modelo de sync

### Ramas e invariantes

- `box` (local, en `/data/repo.git`) es la verdad sobre **qué hay en el disco vivo** para los paths gestionados, salvo ediciones posteriores al último ciclo.
- `origin/box` es la publicación de `box`. El motor solo la mueve con fast-forward.
- `main` la escriben humanos. El motor solo la mueve con fast-forward, a un commit que ya pasó el check de config y ya está aplicado en el box.
- Nunca hay `push --force`.
- El motor solo modifica paths gestionados en sus commits. El resto del árbol se copia del commit padre.

### El ciclo, paso a paso

```
 0. lock (/data/lock) y recuperación de un apply interrumpido (ver §7)
 1. fetch origin main box
 2. SNAPSHOT  escanear disco vivo → árbol T
              si T ≠ árbol(box):
                 escanear secretos en lo cambiado (§8) → si hay hallazgos: blocked_secrets, fin
                 commit C en box (padre = box)
 3. MERGE     B = box, M = origin/main
              M no existe            → R = B          (bootstrap con remoto vacío)
              M ancestro de B        → R = B          (solo cambió el box)
              B ancestro de M        → R = M          (solo cambió main)
              si no                  → git merge-tree --write-tree B M
                  con conflictos     → push box, estado conflict, notificar, fin
                  limpio             → R = commit(árbol, padres = [M, B])
 4. APPLY     si árbol(R) ≠ árbol(B): aplicar diff B→R en disco (§7)
                  falla el check     → rollback a B, push box, estado rejected, fin
 5. PUBLICAR  box := R (local)
              push --atomic origin R:main R:box
                  rechazado (main avanzó) → volver a 1 (máx. 3 veces por ciclo)
 6. POST      reloads según política (§7), notificación si corresponde, evento
```

```
main:  A───────M1────────────R          (R = merge de main + box, ya aplicado)
        \                  /
box:     B0──C1──C2───────'              (C1, C2 = snapshots del box)
                          después de publicar: box = R
```

Detalles:

- **El primer padre del merge es `main`**, para que `git log --first-parent main` cuente la historia humana.
- **Mover `box` localmente antes de pushear** hace que un push rechazado no rompa nada: el disco ya está en R, `box` dice R, y el próximo intento mergea R contra el `main` nuevo.
- **Durante un conflicto el box se sigue snapshoteando** (los cambios del box nunca se pierden) pero no se aplica nada desde `main`.
- **Un `R` rechazado por el check** se recuerda en `state.json` y no se reintenta hasta que `origin/main` cambie.
- **Los paths `push_only` siempre quedan como en `box`.** En cualquier caso de merge, `R` lleva la versión de `box` de esos paths. Si `main` los había cambiado o borrado, se descarta y se reporta en `Overridden`. En el caso fast-forward eso significa un commit nuevo sobre `main`. Un conflicto de contenido, add/add o modify/delete en el que todos los paths son `push_only` se resuelve solo a favor del box. Los conflictos de estructura (archivo/directorio, renombres) frenan siempre, porque git deja paths auxiliares en el árbol.
- **Historias sin relación** (alguien reescribió `main` desde cero): el merge falla con un error explícito y no se aplica nada.

### Conflictos

1. El motor pushea `box` y queda en estado `conflict` con la lista de paths en conflicto.
2. Notifica (notificación persistente en HA + evento, ver §10).
3. El usuario resuelve como cualquier merge:
   - en local: `git fetch && git switch main && git merge origin/box`, resolver, `git push`;
   - o en GitHub: PR `box → main` y el editor de conflictos.
4. En el ciclo siguiente `main` ya contiene a `box` (o casi) y el merge sale limpio.

No hay estrategia automática (`prefer_main` / `prefer_box`) en v0/v1. Queda como opción futura.

### Bootstrap

| Situación | Comportamiento |
|---|---|
| Remoto vacío | Primer snapshot como commit raíz. `main = box = ese commit`. |
| `main` existe, `box` no | Snapshot con padre `main` (el diff `main..box` muestra exactamente en qué difiere el box del repo). Se pushea `box` y el motor queda en `bootstrap_review`: **no** mergea ni aplica hasta que el usuario (a) mergee `box` en `main` por su cuenta o (b) confirme con `sync --accept-bootstrap`. Sin este freno, el primer merge sería un fast-forward silencioso del box sobre `main`. |
| `main` y `box` existen | Ciclo normal. |

### Modos

`mode: bidirectional` (default) | `push_only` (no aplica nunca desde `main`) | `pull_only` (no snapshotea; útil para un box de staging).

- `push_only` solo publica la rama `box`; `main` queda para los humanos y el box nunca la lee.
- `pull_only` queda fuera del v0: el motor lo rechaza con un error explícito. Falta definir qué hacer con las ediciones locales (¿se descartan?) antes de implementarlo.

### Otros casos del ciclo (implementados en M6)

- **Alguien pusheó a `box`:** si `origin/box` no es ancestro del `box` local, el motor frena con error (`box` diverge) y no pisa nada. `box` es solo del motor.
- **Reintentos:** si el disco cambia entre el snapshot y el apply (CAS) o `main` avanza entre el fetch y el push, el ciclo vuelve a empezar, hasta 3 veces por ejecución.
- **Códigos de salida del CLI:** `0` = `idle`; `1` = estado que pide intervención (conflicto, rechazo, secretos, bootstrap, snapshot frenado); `2` = error.

## 7. Aplicar al box, check, rollback y reloads

### Apply

1. **YAML pre-check** (barato, sin HA): parsear todo `.yaml`/`.yml` cambiado aceptando los tags de HA (`!secret`, `!include`, `!include_dir_*`, `!input`, `!env_var`). Un error de sintaxis frena acá, antes de tocar el disco. Los `.json` se validan igual.
2. **Write-ahead:** se graba en `state.json` `{"applying": {"from": B, "to": R, "paths": [...]}}`.
3. **Compare-and-swap por path:** antes de escribir, cada path a tocar tiene que estar en disco exactamente como en `B` (o no existir, si en `B` no existe). Si alguien lo editó desde el snapshot, se aborta el apply sin escribir nada y se vuelve al paso 1 del ciclo.
4. **Escritura atómica:** archivo temporal en el mismo directorio + `rename`. Se crean directorios padre si faltan. Los borrados eliminan el archivo y los directorios que queden vacíos, solo dentro del mapeo. El bit de ejecución sigue a git; el resto de los permisos se preserva si el archivo ya existía.
5. **Check de config:** si cambió algo bajo el mapeo de `/homeassistant`, `POST /api/config/core/check_config`. En `/addon_configs` no hay un check genérico: solo el pre-check de sintaxis.
6. **Rollback** si el check da `invalid`: se reescriben desde `B` los paths tocados (el CAS garantiza que `B` era el estado previo). Se limpia el write-ahead.

**Ventana de riesgo:** `check_config` valida el directorio vivo, así que los archivos nuevos están en disco unos segundos antes del rollback. HA no relee YAML por su cuenta (salvo integraciones puntuales que vigilan archivos, como pyscript), así que el riesgo es bajo. Un check en un directorio de staging necesitaría la imagen de HA Core dentro de la app, que pesa demasiado. Se documenta y se descarta para v1.

**Recuperación tras un corte:** el journal tiene dos fases: `writing` (escribiendo o validando) y `checked` (el disco está en `R` y el check pasó; falta mover `box`). Al arrancar:

- en fase `checked` y con todos los paths como en `R`: se confirma `R` (`box := R`);
- en cualquier otro caso se vuelve a `B`: un `R` que no llegó a validarse nunca se da por bueno, aunque haya quedado entero en disco. Solo se revierten los paths que quedaron como en `R`; si alguien editó un path después del corte, no se pisa y entra en el próximo snapshot.

**Otros resguardos del apply:**

- Ningún componente entre la raíz del mapeo y el archivo puede ser un symlink, para no escribir fuera del mapeo.
- Un directorio que pasa a ser archivo solo se reemplaza si quedó vacío después de los borrados; si tiene archivos no gestionados, falla y se hace rollback.
- Cada mapeo tiene `check: bool` (default `true` para `config/`, `false` para `app_configs/`): define si un cambio ahí exige `check_config`.

### Reloads y restart

- `reload: auto` (default): si el apply tocó `/homeassistant` y el check pasó, se llama `homeassistant.reload_all` (recarga todo lo que es recargable por YAML).
- `reload: none`: no recarga nada; solo avisa.
- **Restart nunca es automático por defecto.** Si cambiaron paths que suelen requerir restart (`configuration.yaml` fuera de bloques recargables, `packages/**`, `custom_components/**` si alguien los incluyó), el estado pasa a tener `restart_recommended: true` y se manda una notificación. `restart: auto` existe como opt-in explícito para quien lo quiera.
- En `/addon_configs` el motor no reinicia apps. Avisa qué apps tuvieron cambios. Reiniciar apps desde el motor queda para v2, con `hassio_api`.

## 8. Escáner de secretos

Corre **antes de commitear un snapshot**, sobre el contenido agregado o modificado. Si encuentra algo, el snapshot no se commitea (para que el secreto nunca quede ni en el repo local) y el estado pasa a `blocked_secrets`.

Reglas:

- Las reglas por defecto de gitleaks (tokens de proveedores, claves privadas, `generic-api-key`), embebidas como librería (v8.30.1; el binario queda en ~9 MB para aarch64, sin símbolos). Su logger se apaga para que nunca escriba contenido.
- Reglas propias de HA:
  - clave YAML que matchea `password|passwd|token|api_key|apikey|secret|client_secret|access_token|private_key|bearer` con un valor escalar literal (no `!secret`, no `!env_var`, no plantilla) → hallazgo;
  - `webhook_id` literal → advertencia (cualquiera con la URL dispara la automatización). Configurable para tratarla como hallazgo;
  - URL con `user:pass@`.
- Los hallazgos se reportan como `path:línea:regla` y **nunca incluyen el valor**, ni en logs ni en la API.
- Excepciones: lista de fingerprints (`sha256(path + regla + línea normalizada)`) en las opciones de la app (`secret_scan.allow`). Una edición de esa línea o mover el archivo invalida la excepción; mover la línea dentro del archivo, no.
- `hcs scan-secrets PATH...` corre el escáner suelto (sirve como pre-commit hook en local). Sale con 1 si hay hallazgos bloqueantes.

## 9. Autenticación con el remoto

- **Solo SSH con una deploy key.** Nunca PAT, nunca HTTPS.
- La app genera un par ed25519 en `/data/ssh/id_ed25519` (0600) en el primer arranque. Muestra la clave pública en el log (v1), en `GET /api/v1/deploy-key` y en la UI (v2). El usuario la carga en GitHub como deploy key **con permiso de escritura**, limitada a ese repo.
- `known_hosts` fijo: las host keys publicadas de GitHub vienen embebidas. Para otros hosts (Gitea, GitLab) hay una opción `ssh_known_hosts`. Siempre con `StrictHostKeyChecking=yes`; no hay trust-on-first-use.
- Entorno de git endurecido en cada invocación:
  - `GIT_SSH_COMMAND="ssh -i /data/ssh/id_ed25519 -o IdentitiesOnly=yes -o IdentityAgent=none -o UserKnownHostsFile=/data/ssh/known_hosts -o StrictHostKeyChecking=yes"`
  - `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/false`, `-c credential.helper=`
  - `-c protocol.allow=never -c protocol.ssh.allow=always` (`file` habilitado solo en tests)
- Validación de la URL del repo: solo `git@host:owner/repo.git` o `ssh://...`. Se rechaza `http(s)://` y cualquier URL con credenciales embebidas.
- Rotación: `POST /api/v1/deploy-key/rotate` (genera la clave nueva, muestra la pública, y la anterior se borra al primer push exitoso con la nueva).

## 10. API de Home Assistant vía `SUPERVISOR_TOKEN`

Con `homeassistant_api: true` la app recibe `SUPERVISOR_TOKEN` y habla con Core a través del proxy del Supervisor (`http://supervisor/core/api/...`, header `Authorization: Bearer $SUPERVISOR_TOKEN`). No hace falta ningún token de larga duración.

| Uso | Llamada |
|---|---|
| Check de config | `POST /api/config/core/check_config` → `{"result": "valid" \| "invalid", "errors": ...}` |
| Reload | `POST /api/services/homeassistant/reload_all` |
| Notificar | `POST /api/services/persistent_notification/create` con `notification_id` fijo (se actualiza en vez de apilarse). Opcional: `notify_service` (p. ej. `notify.mobile_app_x`). |
| Eventos | `POST /api/events/homeassistant_config_sync` con `{"type": "sync_finished" \| "conflict" \| "rejected" \| "blocked_secrets" \| "error", ...}`. Sirve para automatizaciones desde v1, sin esperar la integración. |

A verificar en v1: que `check_config` funcione con el token del Supervisor a través del proxy. Si no, la alternativa es `POST http://supervisor/core/check` con `hassio_api: true` y `hassio_role: homeassistant`.

Comportamiento implementado (M7, `internal/ha/supervisor.go`):

- **Cada ciclo que hizo algo** dispara el evento `homeassistant_config_sync` con `type`, `title`, `message` y datos extra (`state`, `paths`, `findings`), que no pueden pisar los tres primeros.
- **Los estados que piden intervención** además crean o actualizan una sola notificación persistente (`notification_id: homeassistant_config_sync`) y, si hay `notify_service`, la mandan ahí.
- **Un sync que vuelve a la normalidad** descarta esa notificación (`persistent_notification.dismiss`). Un ciclo sin cambios no manda nada.
- **Timeouts:** 120 s para `check_config` (en una Raspberry Pi tarda) y 30 s para lo demás. Si falla un paso, igual se intentan los otros.
- **Ningún error incluye el token.** Las respuestas se leen con un límite de 1 MiB.
- **El CLI elige solo:** con `SUPERVISOR_TOKEN` en el entorno (dentro de la app) usa el Supervisor para check, reload y notificaciones. En local usa `--checker noop|command:'…'` y no recarga ni notifica.

## 11. Disparadores

- **Programado:** `sync_interval_minutes` (default 5; `0` = solo manual). Si un ciclo sigue corriendo, el tick se saltea. Para que un intervalo corto no cargue el box, el escaneo solo re-hashea los archivos cuyo `mtime`, tamaño o inode cambió respecto del ciclo anterior (cache de stat en `/data/state.json`), y el fetch es un solo round-trip SSH.
- **Manual:**
  - v1: `stdin: true`, así que la acción `hassio.addon_stdin` con `{"input": "sync"}` dispara un ciclo desde una automatización o un botón de dashboard, sin UI propia;
  - API: `POST /api/v1/sync`;
  - v2: botón en la UI por ingress; v3: entidad `button` de la integración.
- Nunca dispara un restart de HA (§7).

## 12. API HTTP

Pensada para la UI de v2 y para la integración HACS de v3. Escucha en un puerto interno (8099), sin exponerlo al host.

| Método y ruta | Respuesta |
|---|---|
| `GET /api/v1/status` | `state` (`idle`, `syncing`, `conflict`, `rejected`, `blocked_secrets`, `bootstrap_review`, `suspicious_snapshot`, `error`), `last_sync`, `last_result`, `box_head`, `main_head`, `ahead`/`behind`, `pending_local`, `pending_incoming`, `restart_recommended`, `last_error`. |
| `GET /api/v1/pending` | Cambios locales sin commitear (escaneo en seco), cambios entrantes de `origin/main` sin aplicar, paths ignorados con su motivo. |
| `GET /api/v1/conflict` | Paths en conflicto, commits de `box` y `main`, y el link al compare de GitHub si el remoto es GitHub. |
| `POST /api/v1/sync` | Body opcional `{"dry_run": bool, "accept_bootstrap": bool}`. `202 {"job_id"}` o `409` si ya hay un ciclo corriendo. |
| `GET /api/v1/jobs/{id}` | Progreso y resultado de un ciclo. |
| `GET /api/v1/deploy-key` | Clave pública. |
| `POST /api/v1/deploy-key/rotate` | Rotación (§9). |
| `GET /api/v1/events` (v2) | Server-Sent Events con los mismos tipos que el evento de HA. |

Autenticación:

- Las requests por ingress llegan desde el proxy del Supervisor (`172.30.32.2`) y se aceptan.
- El resto necesita `Authorization: Bearer <token>`, con el token generado en `/data/api_token`. En v3 la app lo entrega a la integración vía Supervisor discovery (`discovery:` en `config.yaml` + `hassio_api`).

La integración HACS de v3 expone: sensor de estado, sensores de último sync y de cambios pendientes, `binary_sensor` de conflicto, `button` "Sincronizar ahora" y disparadores de dispositivo a partir de los eventos.

## 13. Configuración de la app (borrador de `config.yaml`)

```yaml
name: Config Sync
slug: homeassistant_config_sync
version: 0.1.0
arch: [aarch64, amd64]
startup: application
boot: auto
init: false
homeassistant_api: true
stdin: true
map:
  - type: homeassistant_config
    read_only: false
  - type: all_addon_configs
    read_only: false
options:
  repository: ""                  # git@github.com:owner/repo.git
  main_branch: main
  box_branch: box
  mode: bidirectional
  sync_interval_minutes: 5
  reload: auto
  restart: never
  commit_author: "Home Assistant <ha@box.local>"
  max_file_size_kb: 1024
  delete_guard_percent: 20
  delete_guard_files: 25
  notify_service: ""
  mappings: [ ...defaults de §4 y §5... ]
  secret_scan:
    webhook_id: warn
    allow: []
schema: { ... }                   # validación del Supervisor
```

El CLI de v0 lee exactamente el mismo esquema desde un archivo (`--config`), así el v1 no tiene una configuración aparte.

### Estado en `/data`

```
/data/options.json          lo escribe el Supervisor
/data/repo.git/             repo bare (refs: box, origin/main, origin/box)
/data/ssh/                  id_ed25519, id_ed25519.pub, known_hosts
/data/state.json            último resultado, R rechazado, write-ahead de apply, contadores
/data/api_token
/data/lock
```

## 14. Modelo de amenazas (resumen)

| Amenaza | Mitigación |
|---|---|
| Secretos que salen del box | Denylist dura, whitelist del lado del box, escáner antes del commit. |
| Alguien con push al repo ejecuta código en el box | Solo se escriben paths gestionados; `custom_components` y `python_scripts` quedan fuera o son opt-in. Hay que documentar que **escribir a `main` equivale a administrar la config de HA**: proteger `main` (branch protection, PRs). |
| Alguien con push al repo amplía lo que sale del box | La whitelist no vive en el repo. |
| Mount roto o `main` vaciado borra todo | Guards de raíz vacía y de borrado masivo en las dos direcciones. |
| Robo de credenciales | Una deploy key acotada a un repo, sin agent ni helpers; nada de tokens de HA de larga duración. |
| MITM sobre SSH | `known_hosts` fijo, sin TOFU. |

## 15. Roadmap

| Versión | Alcance | Criterio de listo |
|---|---|---|
| **v0** | Motor como CLI (`hcs`), testeable en local contra una config sintética y un remoto bare local. Checker de HA enchufable (fake / noop / comando). | Catálogo de escenarios de `docs/plan-v0.md` en verde; E2E sobre la config sintética en verde. |
| **v1** | App local en `/addons` del box (Dockerfile multi-stage: build Go → imagen base con `git` y `openssh-client`), sin UI. Disparadores: schedule + `stdin`. Notificaciones y eventos en HA. | Una semana de sync real en una Raspberry Pi sin intervención, con al menos un conflicto resuelto de punta a punta. |
| **v2** | UI por ingress (estado, pendientes, conflicto, deploy key, "sync now"). Builds multi-arch (aarch64 + amd64) en CI, imágenes en GHCR, repo publicable como repo de apps (`repository.yaml`). Reinicio opcional de apps vía `hassio_api`. | Instalable desde la URL del repo en un HA limpio. |
| **v3** | Integración HACS: sensores, botón, eventos y discovery vía Supervisor. | Publicada en HACS (default o custom repo). |

## 16. Qué existe hoy y por qué no alcanza

| Proyecto | Qué hace bien | Por qué no alcanza |
|---|---|---|
| **Git pull** (app oficial) | Simple y mantenida. | Solo pull, pisa el box, sin merge. |
| **HA Git Sync** (a4ng3i) | Buen diseño, el más cercano a este: sync en las dos direcciones. | Sin código público (no se puede auditar ni contribuir), solo `/config` (sin `/addon_configs`), y autentica contra HA con un token de admin de larga duración. |
| **HomeAssistantVersionControl** (saihgupr) | Historial de cambios, fácil de instalar. | Solo push (no aplica nada desde el repo), y la auth con el remoto es un token en la URL HTTPS. |

Lo que este proyecto agrega: sync real en las dos direcciones con merge de 3 vías y freno ante conflictos, apply condicionado al check de config, monorepo `config/` + `app_configs/`, auth solo por deploy key SSH, cero tokens de HA de larga duración y código abierto desde el día uno.

> Fuente de la comparación: un relevamiento de noviembre de 2026. Conviene revalidarla antes de publicar v2.

## 17. Decisiones tomadas (2026-11-11)

1. **Licencia:** MIT. El repo arranca privado y se hace público cuando haya código.
2. **Nombres:** slug `homeassistant_config_sync`, binario `hcs`.
3. **Defaults:** `sync_interval_minutes: 5`, `mode: bidirectional`.
4. **`custom_components/**`:** opt-in, fuera por defecto. Quien lo incluya asume que escribir a `main` es ejecutar código en HA (§14).
5. **Un solo repo** para el motor, la app y la integración:

   ```
   repository.yaml                      repo de apps (v2)
   homeassistant_config_sync/           carpeta de la app: config.yaml, Dockerfile, DOCS.md (v1)
   custom_components/homeassistant_config_sync/   integración (v3)
   hacs.json                            (v3)
   cmd/hcs/, internal/, go.mod          el motor (v0)
   docs/
   ```

   El Supervisor solo mira `repository.yaml` y las carpetas con `config.yaml`; HACS solo mira `custom_components/` y `hacs.json`. El módulo Go en la raíz no molesta a ninguno de los dos.
