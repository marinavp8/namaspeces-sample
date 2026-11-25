#!/bin/bash
# mini-run.sh — mini "contenedor" con unshare + overlayfs sobre Alpine aarch64.
# Ejecutar en Linux (VM arm64), como root.
#
# Uso:
#   sudo ./mini-run.sh init                      # descarga el minirootfs (lower)
#   sudo ./mini-run.sh init-iso <alpine.iso>     # construye lower desde una ISO
#   sudo ./mini-run.sh run <nombre> [--net] [--mem 256M] [-- cmd...]
#   sudo ./mini-run.sh reset <nombre>            # borra la capa de escritura
#   sudo ./mini-run.sh ls
set -euo pipefail

BASE=${MINI_BASE:-/var/lib/mini}
ALPINE_VER=${ALPINE_VER:-3.22}
ALPINE_REL=${ALPINE_REL:-3.22.0}
ARCH=${ARCH:-aarch64}
LOWER=$BASE/lower

[ "$(id -u)" = 0 ] || { echo "Ejecuta como root (sudo)."; exit 1; }
[ "$(uname -s)" = Linux ] || { echo "Esto solo funciona en Linux (usa una VM)."; exit 1; }

cmd=${1:-}; shift || true

case "$cmd" in
init)
  mkdir -p "$LOWER"
  url="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VER}/releases/${ARCH}/alpine-minirootfs-${ALPINE_REL}-${ARCH}.tar.gz"
  echo "Descargando $url"
  curl -fL "$url" | tar -xz -C "$LOWER"
  echo "lower listo en $LOWER"
  ;;

init-iso)
  iso=${1:?falta la ruta de la ISO}
  command -v apk >/dev/null || { echo "Necesitas apk (apk-tools) en el host, o usa 'init'."; exit 1; }
  mkdir -p "$LOWER"
  mnt=$(mktemp -d)
  mount -o loop,ro "$iso" "$mnt"
  trap 'umount "$mnt"; rmdir "$mnt"' EXIT
  apk --root "$LOWER" --initdb --allow-untrusted -X "$mnt/apks" add alpine-base
  echo "lower listo en $LOWER"
  ;;

run)
  name=${1:?falta el nombre}; shift
  net=0; mem=""; prog=(/bin/sh -l)
  while [ $# -gt 0 ]; do
    case "$1" in
      --net) net=1 ;;
      --mem) mem=$2; shift ;;
      --) shift; prog=("$@"); break ;;
      *) echo "opción desconocida: $1"; exit 1 ;;
    esac
    shift
  done
  [ -d "$LOWER/bin" ] || { echo "Falta lower. Ejecuta 'init' primero."; exit 1; }

  C=$BASE/c/$name
  mkdir -p "$C"/{upper,work,merged}

  # namespaces: mount, uts, ipc, pid (+ net opcional, aislada)
  flags=(--mount --uts --ipc --pid --fork)
  [ "$net" = 1 ] && flags+=(--net)

  # Red: veth entre host y contenedor. Se crea tras lanzar y se asigna por PID.
  inner() {
    mount --make-rprivate /
    mount -t overlay overlay \
      -o "lowerdir=$LOWER,upperdir=$C/upper,workdir=$C/work" "$C/merged"
    mount -t proc  proc  "$C/merged/proc"
    mount -t sysfs sys   "$C/merged/sys"
    mount --rbind /dev   "$C/merged/dev"
    cp -L /etc/resolv.conf "$C/merged/etc/resolv.conf" 2>/dev/null || true
    hostname "$name"

    # pivot_root en lugar de chroot
    mkdir -p "$C/merged/.old"
    cd "$C/merged"
    pivot_root . .old
    umount -l /.old && rmdir /.old
    exec "$@"
  }
  export -f inner 2>/dev/null || true

  runner=(unshare "${flags[@]}" bash -c "
    name='$name'; LOWER='$LOWER'; C='$C'
    $(declare -f inner)
    if [ '$net' = 1 ]; then
      # esperar a que el host mueva el veth dentro de este netns
      for i in \$(seq 50); do ip link show veth-c >/dev/null 2>&1 && break; sleep 0.1; done
      ip link set lo up
      ip addr add 10.200.0.2/24 dev veth-c 2>/dev/null || true
      ip link set veth-c up
      ip route add default via 10.200.0.1 2>/dev/null || true
    fi
    inner \"\$@\"
  " _ "${prog[@]}")

  # cgroup v2 opcional para limitar memoria
  if [ -n "$mem" ] && command -v systemd-run >/dev/null; then
    runner=(systemd-run --scope --quiet -p "MemoryMax=$mem" "${runner[@]}")
  fi

  if [ "$net" = 1 ]; then
    "${runner[@]}" &
    pid=$!
    sleep 0.3
    # PID real del unshare hijo (el bash dentro del netns)
    cpid=$(pgrep -P "$pid" | head -1 || echo "$pid")
    ip link add veth-h type veth peer name veth-c
    ip link set veth-c netns "$cpid"
    ip addr add 10.200.0.1/24 dev veth-h
    ip link set veth-h up
    sysctl -qw net.ipv4.ip_forward=1
    iptables -t nat -C POSTROUTING -s 10.200.0.0/24 -j MASQUERADE 2>/dev/null \
      || iptables -t nat -A POSTROUTING -s 10.200.0.0/24 -j MASQUERADE
    wait "$pid" || true
    ip link del veth-h 2>/dev/null || true
  else
    "${runner[@]}"
  fi
  ;;

reset)
  name=${1:?falta el nombre}
  rm -rf "${BASE:?}/c/$name"
  echo "Contenedor $name reseteado."
  ;;

ls)
  ls -1 "$BASE/c" 2>/dev/null || echo "(ninguno)"
  ;;

*)
  sed -n '2,11p' "$0"
  exit 1
  ;;
esac
