# Plan de implementación del v0: el motor como CLI

> Acompaña a [`design.md`](design.md). Estado: borrador para revisión, 2026-11-11.

## Alcance

El v0 es el motor completo del ciclo de sync (`design.md` §6–§9) empaquetado como un CLI, `hcs`, que corre en local contra:

- una **copia del box** (los directorios `/homeassistant` y `/addon_configs` traídos a un sandbox local, solo lectura sobre el box real);
- un **remoto bare local** (`file://`) en lugar de GitHub;
- un **checker de HA enchufable** en lugar del Supervisor.

Quedan fuera del v0: Dockerfile, `config.yaml` de la app, UI e integración HACS. La API HTTP y el scheduler sí entran (`hcs serve`), porque son chicos y se testean igual de bien en local.

## Lenguaje: Go 1.27

Go 1.27.4, fijado en `mise.toml`.

| Criterio | Go | Python | Rust | Bash (lo que usan las apps existentes) |
|---|---|---|---|---|
| Imagen de la app (v1/v2) | Un binario estático; la imagen es `base` + `git` + `openssh-client`. | Intérprete + venv + dependencias. | Igual que Go. | Mínima. |
| Multi-arch (aarch64 + amd64) | `GOARCH=arm64` desde amd64, sin emulación. | Wheels por arquitectura, QEMU en CI. | Cross-compile posible, toolchain más pesado. | Trivial. |
| Escáner de secretos | gitleaks **es** Go: se embebe como librería. | Hay que invocar un binario externo. | Hay que invocar un binario externo. | Hay que invocar un binario externo. |
| API HTTP + scheduler | `net/http` y goroutines de la stdlib. | aiohttp o FastAPI. | axum o tokio, más ceremonia. | No. |
| Testabilidad | `testing`, `httptest`, fuzzing nativo, `-race`, `testscript` para el CLI. | pytest, muy buena. | Muy buena. | Mala. |
| Velocidad de iteración | Alta. | Alta. | Media. | Alta al principio, baja después. |

Por qué Go: binario estático, multi-arch barato y gitleaks como librería. Python sería la opción natural por cercanía con HA (y la integración de v3 va a ser Python), pero la app no comparte código con la integración: se hablan por HTTP. Rust no compra nada que Go no dé acá. Bash descartado por testabilidad.

El merge **no** se hace con go-git (no implementa merge de 3 vías): se invoca el git CLI (≥ 2.38 por `merge-tree --write-tree`; local tiene 2.53 y Alpine trae versiones recientes).

### Dependencias previstas (mínimas)

| Paquete | Uso |
|---|---|
| `github.com/zricethezav/gitleaks/v8` | Reglas y detector de secretos. Si su API de librería resulta inestable, el plan B es vendorizar solo su `config/gitleaks.toml` y aplicar las regex propias. |
| `gopkg.in/yaml.v3` | Pre-check de YAML. La API de `Node` preserva tags desconocidos (`!secret`, `!include`…). |
| `github.com/bmatcuk/doublestar/v4` | Globs de la whitelist. |
| `github.com/rogpeppe/go-internal/testscript` | Tests del CLI con golden files. |

Todo lo demás, stdlib.

## Estructura del código

```
cmd/hcs/                 main: subcomandos y flags
internal/config/         carga y validación de la config (mismo esquema que options.json de la app)
internal/paths/          mapeos repo↔box, whitelist, denylist dura, dirección por regla
internal/scan/           escaneo del disco vivo → {path: blob, modo}; tamaño, binarios, lecturas inestables; guards
internal/gitx/           wrapper del git CLI: entorno endurecido, fetch, hash-object, índice temporal,
                         write-tree, commit-tree, merge-tree, push --atomic, ls-tree, cat-file
internal/secrets/        gitleaks + reglas de HA; fingerprints de excepciones
internal/yamlcheck/      parse de YAML/JSON con tags de HA
internal/apply/          diff de árboles → operaciones; CAS; escritura atómica; rollback; write-ahead
internal/ha/             interfaz Checker/Notifier/Reloader; impl. Supervisor; impl. noop y comando
internal/engine/         ciclo (máquina de estados), lock, state.json, bootstrap, reintentos
internal/api/            servidor HTTP (§12 del diseño) y scheduler
internal/testkit/        helpers de escenarios: box falso, remoto bare, clon "dev", fake de HA
scripts/                 traer la copia del box al sandbox
```

## Interfaz del CLI

```
hcs init        --config cfg.yaml --state-dir DIR    crea repo bare, deploy key y known_hosts
hcs status      [--json]
hcs plan                                             lo que haría un sync: snapshot, merge y apply, en seco
hcs sync        [--dry-run] [--accept-bootstrap]
hcs serve       [--listen 127.0.0.1:8099]            API + scheduler
hcs deploy-key  [--rotate]
hcs scan-secrets PATH...                             el escáner suelto, útil para pre-commit hooks

Flags globales:
  --config FILE      YAML/JSON con el esquema de options.json
  --state-dir DIR    default /data (en local: .sandbox/state)
  --checker KIND     supervisor | noop | command:'<cmd>'   (default supervisor si existe SUPERVISOR_TOKEN, si no noop)
  --allow-file-remote  habilita file:// (solo v0 y tests)
```

`--checker command:'…'` permite, si hay Docker o Podman en local, validar la copia con la misma versión de HA:
`docker run --rm -v $DIR:/config ghcr.io/home-assistant/home-assistant:2026.11 python -m homeassistant --script check_config -c /config`.
Es opcional: el v0 no depende de tener contenedores.

## Hitos

Cada hito termina con tests en verde y su commit.

| # | Hito | Contenido | Tests principales |
|---|---|---|---|
| M0 | Scaffold | `go.mod`, `cmd/hcs` con subcomandos vacíos, tasks de mise (`test`, `lint`), golangci-lint fijado en `mise.toml`. | `go vet`, lint, `hcs --help`. |
| M1 | Config y paths | Esquema, defaults, validación de la URL (solo SSH), mapeos, whitelist, denylist, dirección. | Tablas de matcheo (incluye `.storage/lovelace*`, `esphome/secrets.yaml`, `custom_components`); fuzz del matcher. |
| M2 | gitx + snapshot | Entorno git endurecido, repo bare, índice temporal, commit en `box` que preserva lo no gestionado, guards (raíz vacía, borrado masivo, tamaño, binarios, symlinks, lecturas inestables), cache de stat para no re-hashear lo que no cambió (el intervalo default es 5 min). | Snapshot idempotente (2 corridas = 1 commit); README del repo preservado; guards; la cache no esconde una edición que conserva el tamaño. |
| M3 | Merge | Casos ancestro/fast-forward, `merge-tree`, conflictos con lista de paths, orden de padres. | Escenarios de merge (ver catálogo). |
| M4 | Apply | Diff B→R filtrado por whitelist y dirección, pre-check YAML/JSON, write-ahead, CAS, escritura atómica, borrados, rollback, recuperación al arrancar. | Inyección de fallas entre pasos; edición concurrente durante el apply. |
| M5 | Secretos | gitleaks embebido + reglas de HA; bloqueo antes del commit; fingerprints. | Corpus de positivos y negativos; ningún valor en logs ni en la salida. |
| M6 | Ciclo completo | Máquina de estados, lock, `state.json`, bootstrap (remoto vacío / `bootstrap_review`), `push --atomic`, reintento ante rechazo, R rechazado recordado. | Catálogo de escenarios completo + test basado en modelo. |
| M7 | Cliente HA | Impl. Supervisor (check_config, reload_all, persistent_notification, eventos) contra `httptest`; impl. `command`. | Requests exactas, headers, manejo de `invalid`, timeouts. |
| M8 | API y scheduler | `hcs serve`, endpoints de §12, jobs, `409` si hay un ciclo corriendo, auth por token. | `httptest` + concurrencia con `-race`. |
| M9 | E2E | `scripts/e2e-sandbox.sh` sobre la config sintética de `testdata/ha-config`, con un remoto bare local sembrado como un repo que ya existía. | Escenario de punta a punta con aserciones exactas. |

## Estrategia de tests

### 1. Unitarios (tablas)

`paths`, `yamlcheck`, `secrets`, el armado de operaciones de `apply`, la transición de estados de `engine`. Sin I/O ni git.

### 2. Integración con git real (el grueso)

Cada test arma en un `t.TempDir()`:

- `box/homeassistant`, `box/addon_configs`: el disco vivo;
- `origin.git`: el remoto bare (`file://`, con `--allow-file-remote`);
- `dev/`: un clon que hace de usuario en local (commitea y pushea a `main`, resuelve conflictos);
- un **fake de HA** (`httptest`) con respuestas programables de `check_config` y registro de las llamadas.

Un DSL chico en `internal/testkit` mantiene los escenarios legibles:

```go
s := testkit.New(t)
s.Box.Write("config/automations.yaml", "...")
s.Dev.Write("config/scripts.yaml", "...").Commit("edit").Push()
s.Sync().ExpectState(engine.Idle)
s.ExpectBoxFile("config/scripts.yaml", "...")
s.ExpectRemote("main").Contains(s.Remote("box"))
```

### 3. Catálogo de escenarios (criterio de listo del v0)

Bootstrap

- [ ] Remoto vacío → commit raíz, `main == box`.
- [ ] `main` existente sin `box` → `bootstrap_review`; no aplica nada; `--accept-bootstrap` destraba; un merge manual de `box` en `main` también.

Flujo normal

- [ ] Solo cambió el box → `main` avanza por fast-forward.
- [ ] Solo cambió `main` → se aplica, se llama a `check_config` y a `reload_all`, y `box` avanza.
- [ ] Cambios en archivos distintos de los dos lados → merge limpio, primer padre `main`.
- [ ] Mismo archivo, hunks distintos → merge limpio.
- [ ] Dos `sync` seguidos sin cambios → ningún commit nuevo (idempotencia).
- [ ] Borrados en los dos sentidos.
- [ ] Archivo ↔ directorio (un path que cambia de tipo).
- [ ] Bit de ejecución preservado; symlinks ignorados y reportados.

Conflictos

- [ ] Mismo hunk de los dos lados → `conflict`, `box` pusheado, nada aplicado, notificación.
- [ ] Durante un conflicto, nuevas ediciones en el box siguen llegando a `box`.
- [ ] El usuario resuelve en `dev` y pushea → el ciclo siguiente sale limpio.

Seguridad y guards

- [ ] `check_config` inválido → rollback exacto, `rejected`, ese R no se reintenta hasta que `main` cambie.
- [ ] YAML roto desde `main` → se frena en el pre-check, sin tocar el disco.
- [ ] Edición en el box entre el snapshot y el apply → el CAS aborta y el reintento la incluye.
- [ ] `main` avanzó entre el fetch y el push → push rechazado, reintento, sin pérdida.
- [ ] Mount vacío o ausente → `suspicious_snapshot`, nada commiteado.
- [ ] Borrado masivo (box o `main`) → frena y pide confirmación.
- [ ] Secreto literal en un YAML nuevo → `blocked_secrets`, **ningún** commit (ni local), el valor no aparece en ninguna salida.
- [ ] Secreto con fingerprint permitido → pasa; si se edita la línea → vuelve a bloquear.
- [ ] `secrets.yaml`, `*.db` o `.storage/auth` presentes → jamás entran, ni forzando la whitelist.
- [ ] Archivo grande o binario → ignorado y reportado.
- [ ] Path de `main` fuera de los mapeos (README) → no baja al box y se preserva en los snapshots.
- [ ] Cambio entrante en `.storage/lovelace*` (`push`) → no se aplica y se reporta.
- [ ] URL `https://` o con credenciales → rechazada en `init`.

Robustez

- [ ] Corte (falla inyectada) en cada paso del apply → el arranque siguiente deja el disco en B o en R, nunca mezclado.
- [ ] Dos `sync` concurrentes → el segundo recibe "ocupado".

### 4. Test basado en modelo

Una secuencia aleatoria (semilla registrada) de operaciones: editar, crear o borrar en el box; commit y push en `dev`; resolver un conflicto; `sync`. Después de cada `sync` se chequean las invariantes:

- en estado `idle`, el disco gestionado coincide con el árbol de `box`, y `box == main` en el remoto;
- ningún contenido escrito en el box o en `dev` se pierde: cada versión es alcanzable desde `box` o desde `main`;
- nunca hubo un push no fast-forward;
- nunca se escribió un path no gestionado.

### 5. Tests del CLI

`testscript` con golden files para `status`, `plan` y los mensajes de error (incluido que la salida de `scan-secrets` no tenga valores).

### 6. Higiene

`go test -race ./...`, `go vet`, golangci-lint; fuzzing de `paths` y `yamlcheck` en el task `fuzz` (fuera del ciclo normal). Objetivo de cobertura: ≥ 80 % en `engine`, `apply` y `paths`. CI en GitHub Actions cuando exista el repo remoto.

## Sandbox en local

```
.sandbox/                  (ignorado por git)
  box/homeassistant/       copia de /homeassistant (sin la denylist)
  box/addon_configs/       copia de /addon_configs (sin la denylist)
  origin.git/              remoto bare local
  state/                   el /data del motor
  config.yaml              config de prueba con mapeos a .sandbox/box/...
```

- La copia del box se trae con `tar` por `ssh <host>`, en modo lectura. El script excluye la denylist **en el origen**, así `secrets.yaml` ni siquiera viaja.
- El v0 nunca escribe en el box real ni en GitHub.
- No se usa el agente SSH ni las claves del usuario: el motor fuerza `IdentityAgent=none` y su propia deploy key. Una prueba opcional contra un repo descartable de GitHub requiere que el usuario cree el repo y cargue la deploy key; se hace solo con su OK.

## Resultado del E2E (M9, 2026-11-11)

`scripts/fetch-box-copy.sh` trae la copia del box por `ssh <host>` en modo lectura. En el add-on de SSH las configs de apps están en `/app_configs`, no en `/addon_configs`. Lo sensible se excluye en el propio box:

- la denylist dura;
- todos los `.git` (un clon puede tener credenciales en `.git/config`);
- `.storage` salvo `lovelace*`;
- las bases de datos, `.cache`, logs y backups.

Un chequeo final aborta si llegó algo de la denylist.

`scripts/e2e-sandbox.sh` corre 13 pasos sobre una copia de trabajo limpia (repetible). Siembra un "GitHub" local con una config de ejemplo en layout de monorepo y verifica:

| Paso | Resultado |
|---|---|
| plan / bootstrap contra un repo existente | Muestra exactamente en qué difiere el box del repo existente. |
| aceptar bootstrap, no-op | `main == box`, segundo sync sin push. |
| local → box, box → main | OK. |
| conflicto + resolución en local | El box no se toca durante el conflicto; la resolución llega. |
| YAML roto desde main | `rejected` por el pre-check, el box intacto; revertir destraba. |
| el mismo box contra un repo vacío | `blocked_secrets` señalando el YAML de ESPHome con la clave escrita a mano. Nada pusheado, ni commits locales. |

Las dos corridas seguidas dieron `E2E OK`. Lo que no cubre: `check_config` real (corrió con `--checker noop`) y SSH contra GitHub (el remoto es `file://`). Las dos cosas son del v1.

## Riesgos y cosas a validar temprano

| Riesgo | Cuándo se valida |
|---|---|
| API de librería de gitleaks inestable o pesada en el binario | Resuelto en M5: `detect.Detect(Fragment)` alcanza y el binario aarch64 queda en ~9 MB. Costo: ~60 dependencias indirectas. Plan B si molesta: vendorizar solo sus reglas. |
| `check_config` vía proxy del Supervisor con `SUPERVISOR_TOKEN` | Antes de v1, con una app mínima en `/addons`. |
| `merge-tree --write-tree` y su salida de conflictos parseable | M3 (se usa `-z --name-only`). |
| Tiempo de `check_config` en una Raspberry Pi | v1 (timeout configurable, default 120 s). |
| Que el esquema de opciones del Supervisor acepte `mappings` (lista de objetos con listas de strings adentro) | v1. Plan B: los mapeos van en un YAML aparte dentro del `addon_config` de la app, con el mismo esquema que lee `config.Load`. |
