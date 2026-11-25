# namaspeces-sample — un contenedor en ~300 líneas de Go

Un "contenedor" es solo un proceso de Linux con tres cosas añadidas: **namespaces** que aíslan lo que ve, un **sistema de ficheros raíz propio** y, opcionalmente, un **cgroup** que limita sus recursos. Este repo lo demuestra sin Docker ni runc, usando Alpine Linux, `unshare`, overlayfs y `pivot_root`.

📖 Explicación completa con diagramas: [artifact](https://claude.ai/artifact/PPa5W917iJTbP9KYbCyKSd)

## Contenido

| Fichero | Descripción |
|---|---|
| `mini/main.go` | Implementación en Go (solo librería estándar, sin dependencias). |
| `mini/go.mod` | Módulo Go. |
| `mini/mini-linux-amd64` | Binario estático para x86_64 (el que se probó). |
| `mini/mini-linux-arm64` | Binario estático para ARM64 (compila, sin probar en ARM). |
| `mini-run.sh` | Versión equivalente en shell (`unshare` + `mount` + `pivot_root`). Referencia, no probada en Linux. |

## Cómo funciona

El sistema de ficheros del contenedor es un overlay de dos capas:

```
merged/                 lo que el contenedor ve como "/"
c/<nombre>/upper        escrituras de este contenedor
lower/                  Alpine minirootfs, compartido y de solo lectura
```

Varios contenedores comparten `lower`; cada uno tiene su `upper`. Resetear un contenedor es borrar su `upper`.

`mini run` se ejecuta en dos fases, relanzando el mismo binario:

1. **host**: crea los directorios del overlay, relanza `/proc/self/exe child` con los flags `CLONE_NEW*`, mete el PID en un cgroup (`--mem`) y crea el par veth + NAT (`--net`). Después desbloquea al hijo mediante un pipe.
2. **child** (ya dentro de los namespaces): espera al pipe, configura `eth0`, monta overlay, `/proc`, `/sys` y `/dev`, fija el hostname, hace `pivot_root`, desmonta el host y hace `exec` del programa, que queda como PID 1.

Mecanismos del kernel usados: `CLONE_NEWNS` (montajes), `CLONE_NEWPID`, `CLONE_NEWUTS`, `CLONE_NEWIPC`, `CLONE_NEWNET` (con `--net`), overlayfs, `pivot_root`, cgroup v2 (`memory.max`) y veth + `MASQUERADE` en iptables.

## Requisitos

- Linux, ejecutando como **root**.
- cgroup v2 (para `--mem`).
- `ip` e `iptables` en el host (solo para `--net`).

## Uso

```bash
sudo ./mini init                          # descarga el minirootfs a /var/lib/mini/lower
sudo ./mini run web                       # shell en el contenedor "web"
sudo ./mini run --net --mem 256M web      # con red aislada y límite de RAM
sudo ./mini run db -- apk add redis       # ejecuta un comando concreto
sudo ./mini reset web                     # borra la capa de escritura
sudo ./mini ls                            # lista contenedores
```

El estado se guarda en `/var/lib/mini` (cambiable con `MINI_BASE`). `mini init --ver 3.22.0` elige la versión de Alpine.

### Compilar

```bash
cd mini
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o mini-linux-amd64 .
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o mini-linux-arm64 .
```

## Estado de las pruebas (x86_64, Ubuntu, kernel 7.0, cgroup v2)

- ✅ Descarga y extracción del minirootfs
- ✅ Aislamiento: PID 1, hostname propio, `ps` limitado
- ✅ Overlay: lo escrito va a `upper`, `lower` queda intacto
- ✅ Red: `eth0` con IP propia, ping, DNS, `apk add`, `curl https`
- ✅ Dos contenedores con red a la vez
- ✅ Limpieza al salir (veths, mounts, cgroups)
- ⚠️ Límite de memoria: `memory.max` se aplica, pero con swap activo el kernel mueve páginas al swap en vez de matar el proceso.

## Limitaciones

- Con swap, el límite de memoria es blando; para hacerlo estricto, escribir también `memory.swap.max=0` en `setupCgroup`.
- El programa lanzado es PID 1 y no recoge procesos zombi (haría falta un mini-init o `tini`).
- La subred `10.200.N.0/24` sale de un hash del nombre; dos nombres podrían colisionar.
- Solo root, sin modo rootless (user namespaces).
- Usa el minirootfs de Alpine, no la ISO.
