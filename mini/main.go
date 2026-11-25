// mini — contenedor mínimo con namespaces + overlayfs sobre Alpine.
// Solo Linux, como root.
//
// Un "contenedor" no es más que un proceso de Linux al que se le aplican:
//   - namespaces (mount, pid, uts, ipc, net): aíslan lo que el proceso ve;
//   - un sistema de ficheros raíz propio (aquí, un overlayfs sobre Alpine);
//   - opcionalmente un cgroup, que limita los recursos que puede consumir.
//
// Uso:
//
//	mini init [--ver 3.22.0]                    descarga el minirootfs (capa lower)
//	mini run [--net] [--mem 256M] <nombre> [-- cmd args...]
//	mini reset <nombre>                         borra la capa de escritura
//	mini ls
package main

import (
	"archive/tar"   // leer el minirootfs de Alpine
	"compress/gzip" // descomprimir el .tar.gz
	"flag"          // opciones --net, --mem, --ver
	"fmt"           // formato de mensajes
	"hash/fnv"      // hash del nombre para elegir subred
	"io"            // copia de streams
	"net/http"      // descarga del minirootfs
	"os"            // ficheros, pipes, variables de entorno
	"os/exec"       // lanzar procesos (este mismo binario, ip, iptables)
	"path/filepath" // construir rutas
	"runtime"       // detectar SO y arquitectura
	"strings"       // manipular texto
	"syscall"       // llamadas al kernel: clone flags, mount, pivot_root...
)

// base es el directorio donde se guarda todo el estado. Estructura:
//
//	<base>/lower            rootfs de Alpine, compartido y de solo lectura
//	<base>/c/<nombre>/upper capa de escritura de cada contenedor
//	<base>/c/<nombre>/work  directorio de trabajo que exige overlayfs
//	<base>/c/<nombre>/merged vista final (lower + upper) = la "/" del contenedor
var base = envOr("MINI_BASE", "/var/lib/mini")

// envOr devuelve la variable de entorno k, o d si está vacía.
func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// die imprime un error con prefijo "mini:" y termina con código 1.
func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "mini: "+f+"\n", a...)
	os.Exit(1)
}

// check termina el programa si err no es nil, indicando en ctx qué se estaba haciendo.
func check(err error, ctx string) {
	if err != nil {
		die("%s: %v", ctx, err)
	}
}

func main() {
	// namespaces, overlayfs y cgroups son cosas de Linux
	if runtime.GOOS != "linux" {
		die("solo funciona en Linux (usa una VM)")
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "uso: mini init|run|reset|ls ...")
		os.Exit(1)
	}
	// montar, crear namespaces y tocar cgroups requiere privilegios
	if os.Geteuid() != 0 {
		die("ejecuta como root")
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "init":
		cmdInit(args)
	case "run":
		cmdRun(args)
	case "child": // interno: se ejecuta ya dentro de los namespaces
		cmdChild(args)
	case "reset":
		if len(args) < 1 {
			die("falta el nombre")
		}
		// borrar el directorio del contenedor elimina su upper (todos sus cambios)
		check(os.RemoveAll(filepath.Join(base, "c", args[0])), "reset")
		fmt.Println("reseteado", args[0])
	case "ls":
		ents, _ := os.ReadDir(filepath.Join(base, "c"))
		for _, e := range ents {
			fmt.Println(e.Name())
		}
	default:
		die("comando desconocido: %s", os.Args[1])
	}
}

// ---------- init: descargar y extraer minirootfs ----------

// cmdInit descarga el minirootfs de Alpine para la arquitectura actual y lo
// extrae en <base>/lower, que será la capa de solo lectura del overlay.
func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	ver := fs.String("ver", "3.22.0", "versión de Alpine (X.Y.Z)")
	fs.Parse(args)

	// Go usa arm64/amd64; Alpine usa aarch64/x86_64
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	if arch == "" {
		die("arquitectura no soportada: %s", runtime.GOARCH)
	}
	// la carpeta de releases usa solo X.Y (3.22), el fichero usa X.Y.Z (3.22.0)
	short := strings.Join(strings.Split(*ver, ".")[:2], ".")
	url := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/releases/%s/alpine-minirootfs-%s-%s.tar.gz", short, arch, *ver, arch)
	fmt.Println("descargando", url)

	resp, err := http.Get(url)
	check(err, "descarga")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		die("HTTP %s", resp.Status)
	}
	lower := filepath.Join(base, "lower")
	check(os.MkdirAll(lower, 0755), "mkdir lower")
	// se extrae en streaming, sin guardar el .tar.gz en disco
	check(untar(resp.Body, lower), "extraer")
	fmt.Println("lower listo en", lower)
}

// untar descomprime un .tar.gz en dest. Soporta directorios, ficheros,
// symlinks y hardlinks, que es todo lo que contiene el minirootfs.
func untar(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, h.Name)
		// seguridad: rechazar entradas tipo "../../etc/passwd" que escaparían de dest
		if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
			return fmt.Errorf("ruta sospechosa en el tar: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, os.FileMode(h.Mode))
		case tar.TypeReg:
			// el tar no garantiza que el directorio padre aparezca antes
			os.MkdirAll(filepath.Dir(target), 0755)
			var f *os.File
			f, err = os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode))
			if err == nil {
				_, err = io.Copy(f, tr)
				f.Close()
			}
		case tar.TypeSymlink:
			os.Remove(target) // por si ya existía (re-ejecución de init)
			err = os.Symlink(h.Linkname, target)
		case tar.TypeLink:
			os.Remove(target)
			// Linkname es relativo a la raíz del tar, no al directorio actual
			err = os.Link(filepath.Join(dest, h.Linkname), target)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
		// conservar propietario (root:root en Alpine); Lchown no sigue symlinks,
		// pero se omite para ellos porque su dueño no importa
		if h.Typeflag != tar.TypeSymlink {
			os.Lchown(target, h.Uid, h.Gid)
		}
	}
}

// ---------- run: lado host ----------

// cmdRun es la parte "del host" de lanzar un contenedor. No puede montar el
// overlay ella misma porque los montajes deben ocurrir dentro del namespace de
// mount nuevo. Por eso vuelve a ejecutar este mismo binario como "child" con
// los namespaces ya creados, y mientras tanto prepara cgroup y red desde fuera.
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	netOn := fs.Bool("net", false, "red aislada con veth + NAT")
	mem := fs.String("mem", "", "límite de memoria (cgroup v2), p.ej. 256M")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) < 1 {
		die("uso: mini run [--net] [--mem 256M] <nombre> [-- cmd...]")
	}
	name := rest[0]
	prog := rest[1:]
	if len(prog) > 0 && prog[0] == "--" {
		prog = prog[1:]
	}
	if len(prog) == 0 {
		prog = []string{"/bin/sh", "-l"} // por defecto, un shell interactivo
	}
	if _, err := os.Stat(filepath.Join(base, "lower", "bin")); err != nil {
		die("falta lower: ejecuta 'mini init'")
	}
	// directorios del overlay para este contenedor (si ya existen se reutilizan,
	// por eso un contenedor conserva sus cambios entre ejecuciones)
	for _, d := range []string{"upper", "work", "merged"} {
		check(os.MkdirAll(filepath.Join(base, "c", name, d), 0755), "mkdir "+d)
	}

	// Pipe de sincronización: el hijo se queda bloqueado leyendo hasta que el
	// host haya metido su PID en el cgroup y creado el veth. Sin esto el hijo
	// podría arrancar antes de tener límites o red.
	pr, pw, err := os.Pipe()
	check(err, "pipe")

	// Cada flag CLONE_NEW* crea un namespace nuevo para el proceso hijo:
	//   NEWNS  montajes propios (el overlay desaparece al salir)
	//   NEWUTS hostname propio
	//   NEWIPC colas de mensajes y memoria compartida propias
	//   NEWPID el hijo es el PID 1 de un árbol de procesos nuevo
	//   NEWNET pila de red vacía (solo con --net)
	flags := uintptr(syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWPID)
	if *netOn {
		flags |= syscall.CLONE_NEWNET
	}
	n := subnet(name)

	// /proc/self/exe es este mismo binario: se relanza con el subcomando "child"
	cmd := exec.Command("/proc/self/exe", append([]string{"child", name, "--"}, prog...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{pr} // fd 3 en el hijo: extremo de lectura del pipe
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags}
	if *netOn {
		// el hijo sabe así que debe configurar red y qué subred usar
		cmd.Env = append(os.Environ(), fmt.Sprintf("MINI_NET=%d", n))
	}
	check(cmd.Start(), "start")
	pr.Close()             // el host ya no lee del pipe
	pid := cmd.Process.Pid // PID visto desde el host (dentro del contenedor será 1)

	// preparar, desde el host, lo que el hijo no puede hacerse a sí mismo
	var cg string
	if *mem != "" {
		cg = setupCgroup(name, *mem, pid)
	}
	var vh string
	if *netOn {
		vh = setupNet(pid, n)
	}
	pw.Write([]byte{1}) // desbloquea al hijo
	pw.Close()

	err = cmd.Wait()
	// limpieza al terminar el contenedor
	if vh != "" {
		exec.Command("ip", "link", "del", vh).Run()
	}
	if cg != "" {
		os.Remove(cg) // el cgroup ya está vacío, así que rmdir funciona
	}
	// propagar el código de salida del programa del contenedor
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
}

// subnet devuelve un número 1..250 derivado del nombre, para usar la subred
// 10.200.N.0/24. Es determinista, aunque dos nombres podrían colisionar.
func subnet(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32()%250) + 1
}

// setupCgroup crea un cgroup v2 con límite de memoria y mete en él al proceso
// pid. Los hijos del proceso heredan el cgroup. Devuelve la ruta para borrarla.
func setupCgroup(name, mem string, pid int) string {
	root := "/sys/fs/cgroup"
	// activar el controlador de memoria para los cgroups hijos; si ya estaba
	// activo o no hace falta, el error se ignora a propósito
	os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+memory"), 0644)
	cg := filepath.Join(root, "mini-"+name)
	check(os.MkdirAll(cg, 0755), "cgroup (¿cgroup v2?)")
	// memory.max acepta sufijos K, M, G (p.ej. "256M")
	check(os.WriteFile(filepath.Join(cg, "memory.max"), []byte(mem), 0644), "memory.max")
	check(os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte(fmt.Sprint(pid)), 0644), "cgroup.procs")
	return cg
}

// run ejecuta un comando externo y termina el programa mostrando su salida si falla.
func run(name string, a ...string) {
	out, err := exec.Command(name, a...).CombinedOutput()
	if err != nil {
		die("%s %s: %v\n%s", name, strings.Join(a, " "), err, out)
	}
}

// setupNet conecta el contenedor con el host mediante un par veth:
//
//	host: vh<pid> 10.200.N.1  <──────>  contenedor: vc<pid> → eth0 10.200.N.2
//
// y activa NAT para que el contenedor salga a Internet a través del host.
// Devuelve el nombre del extremo del host, para borrarlo al terminar.
func setupNet(pid, n int) string {
	vh, vc := fmt.Sprintf("vh%d", pid), fmt.Sprintf("vc%d", pid)
	sub := fmt.Sprintf("10.200.%d", n)
	run("ip", "link", "add", vh, "type", "veth", "peer", "name", vc)
	// mover un extremo al netns del hijo, identificado por su PID
	run("ip", "link", "set", vc, "netns", fmt.Sprint(pid))
	run("ip", "addr", "add", sub+".1/24", "dev", vh)
	run("ip", "link", "set", vh, "up")
	// permitir que el host reenvíe paquetes entre interfaces
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
	// -C comprueba si la regla ya existe; si no, -A la añade (evita duplicados)
	rule := []string{"-t", "nat", "POSTROUTING", "-s", sub + ".0/24", "-j", "MASQUERADE"}
	if exec.Command("iptables", append([]string{"-t", "nat", "-C"}, rule[2:]...)...).Run() != nil {
		run("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", sub+".0/24", "-j", "MASQUERADE")
	}
	return vh
}

// ---------- child: dentro de los namespaces ----------

// cmdChild corre ya dentro de los namespaces nuevos (es el PID 1 del
// contenedor). Monta el overlay, cambia la raíz y se convierte en el
// programa pedido mediante exec.
func cmdChild(args []string) {
	name := args[0]
	prog := args[2:] // args[1] == "--"
	c := filepath.Join(base, "c", name)
	merged := filepath.Join(c, "merged")

	// esperar a que el host configure cgroup/red (lee el fd 3, el pipe)
	buf := make([]byte, 1)
	os.NewFile(3, "sync").Read(buf)

	// Red: se configura antes del pivot_root porque aún están disponibles las
	// herramientas del host (ip). Después, el rootfs de Alpine no las garantiza.
	if n := os.Getenv("MINI_NET"); n != "" {
		setupChildNet(n)
	}

	// Hacer todos los montajes "privados": sin esto, lo que se monte aquí se
	// propagaría al namespace del host y se ensuciaría el sistema.
	check(syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""), "make-rprivate")

	// overlayfs: lower (solo lectura) + upper (escritura) = merged
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		filepath.Join(base, "lower"), filepath.Join(c, "upper"), filepath.Join(c, "work"))
	check(syscall.Mount("overlay", merged, "overlay", 0, opts), "mount overlay")
	// sistemas de ficheros virtuales que necesita cualquier userland
	check(syscall.Mount("proc", merged+"/proc", "proc", 0, ""), "mount proc")
	check(syscall.Mount("sysfs", merged+"/sys", "sysfs", 0, ""), "mount sys")
	// /dev del host (bind recursivo): da acceso a /dev/null, /dev/tty, etc.
	check(syscall.Mount("/dev", merged+"/dev", "", syscall.MS_BIND|syscall.MS_REC, ""), "bind /dev")
	os.WriteFile(merged+"/etc/resolv.conf", resolvConf(), 0644)
	check(syscall.Sethostname([]byte(name)), "sethostname")

	// pivot_root intercambia la raíz: merged pasa a ser "/" y la antigua raíz
	// queda en /.old. Es más seguro que chroot, que se puede escapar.
	old := filepath.Join(merged, ".old")
	check(os.MkdirAll(old, 0700), "mkdir .old")
	check(syscall.PivotRoot(merged, old), "pivot_root")
	check(os.Chdir("/"), "chdir /")
	// desmontar el host: MNT_DETACH lo hace aunque haya montajes anidados ocupados
	check(syscall.Unmount("/.old", syscall.MNT_DETACH), "umount .old")
	os.Remove("/.old")

	// entorno limpio: no se hereda nada del host, salvo TERM para el terminal
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "TERM=" + envOr("TERM", "xterm")}
	path := prog[0]
	// syscall.Exec no busca en PATH, así que se resuelve a mano dentro del nuevo rootfs
	if !strings.Contains(path, "/") {
		p, err := lookIn(env[0][5:], path) // env[0][5:] = valor de PATH sin "PATH="
		check(err, path)
		path = p
	}
	// exec reemplaza este proceso por el programa pedido: queda como PID 1
	check(syscall.Exec(path, prog, env), "exec "+path)
}

// lookIn busca el ejecutable file en los directorios de pathVar (formato PATH).
func lookIn(pathVar, file string) (string, error) {
	for _, d := range strings.Split(pathVar, ":") {
		p := filepath.Join(d, file)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("no encontrado en PATH")
}

// setupChildNet configura la red dentro del netns: lo + veth renombrada a eth0.
func setupChildNet(n string) {
	// Se busca la interfaz con `ip -o link` y no en /sys/class/net, porque
	// /sys se montó en el namespace antiguo y mostraría las interfaces del host.
	// El nombre vc<pid> lleva el PID del host, que el hijo no conoce (aquí es 1).
	out, _ := exec.Command("ip", "-o", "link").Output()
	vc := ""
	for _, l := range strings.Split(string(out), "\n") {
		// formato de línea: "5: vc1234@if6: <BROADCAST,...> ..."
		f := strings.Fields(l)
		if len(f) > 1 {
			if nm := strings.TrimSuffix(strings.SplitN(f[1], "@", 2)[0], ":"); strings.HasPrefix(nm, "vc") {
				vc = nm
			}
		}
	}
	if vc == "" {
		die("no se encontró la interfaz veth dentro del netns")
	}
	sub := "10.200." + n
	run("ip", "link", "set", "lo", "up")
	run("ip", "link", "set", vc, "name", "eth0") // renombrar exige que esté apagada
	run("ip", "addr", "add", sub+".2/24", "dev", "eth0")
	run("ip", "link", "set", "eth0", "up")
	// el host (extremo .1 del veth) es la puerta de enlace
	run("ip", "route", "add", "default", "via", sub+".1")
}

// resolvConf devuelve un resolv.conf utilizable dentro del contenedor: si el
// del host apunta a loopback (stub de systemd-resolved) no sirve en otro netns.
func resolvConf() []byte {
	// el primer fichero es el que systemd-resolved genera con los DNS reales
	for _, f := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		b, err := os.ReadFile(f)
		if err == nil && !strings.Contains(string(b), "nameserver 127.") {
			return b
		}
	}
	// último recurso: DNS públicos
	return []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n")
}
