package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func branchContainer(name string) string { return "pgb-" + name }
func branchVolume(name string) string    { return "pgb-br-" + name }

// cloneScript tenta CoW (reflink: btrfs, XFS, APFS via virtiofs...) e cai para cópia cheia.
// O /dst é sempre um volume recém-criado por este processo, então o find -delete é seguro.
const cloneScript = `if cp -a --reflink=always /src/. /dst/ 2>/dev/null; then echo cow; else
find /dst -mindepth 1 -delete; cp -a /src/. /dst/ && echo full; fi`

func branchCreate(name, from string, ttl time.Duration) error {
	base, err := loadBase(from)
	if err != nil {
		return err
	}
	expires := ""
	if ttl > 0 {
		expires = time.Now().Add(ttl).UTC().Format(time.RFC3339)
	}
	return createBranch(name, base, expires, "")
}

// createBranch: o `docker create` do container da branch é o lock (nome único no daemon).
// Quem perde a corrida sai sem tocar em nada; quem ganha só desfaz o que ele mesmo criou.
func createBranch(name string, base map[string]string, expires, port string) error {
	c, vol := branchContainer(name), branchVolume(name)
	if exists("container", c) {
		return fmt.Errorf("branch %q já existe", name)
	}
	if exists("volume", vol) {
		return fmt.Errorf("sobrou o volume de uma branch %q interrompida: pgb branch rm %s", name, name)
	}
	var err error
	if port == "" {
		if port, err = freePort(); err != nil {
			return err
		}
	}
	labels := map[string]string{
		lKind: "branch", lName: name, lBase: base[lName],
		lUser: base[lUser], lPassword: base[lPassword], lDB: base[lDB],
		lCreated: time.Now().UTC().Format(time.RFC3339), lPort: port,
	}
	if expires != "" {
		labels[lExpires] = expires
	}
	if _, err := docker(append([]string{"volume", "create"}, append(labelArgs(labels), vol)...)...); err != nil {
		return err
	}
	args := append([]string{"create", "--name", c}, labelArgs(labels)...)
	args = append(args, "-p", "127.0.0.1:"+port+":5432", "-v", vol+":"+base[lPGData], base[lImage])
	if _, err := docker(args...); err != nil {
		if isConflict(err) {
			return fmt.Errorf("branch %q já existe (criada por outro processo agora)", name)
		}
		cleanup("volume", "rm", "-f", vol)
		return err
	}
	ok := false
	defer func() {
		if !ok {
			cleanup("rm", "-f", "-v", c)
			cleanupHelpers("branch:" + name)
			cleanup("volume", "rm", "-f", vol)
		}
	}()

	start := time.Now()
	clone := append([]string{"run", "--rm"}, helperArgs("branch:"+name)...)
	clone = append(clone, "-v", baseVolume(base[lName])+":/src:ro", "-v", vol+":/dst", base[lImage], "sh", "-c", cloneScript+changesConf)
	mode, err := docker(clone...)
	if err != nil {
		return fmt.Errorf("clonando a base: %w", err)
	}
	cloned := time.Since(start)
	if _, err := docker("start", c); err != nil {
		return err
	}
	if err := waitReady(c, base[lUser], base[lDB], time.Minute); err != nil {
		return err
	}
	createChangesSlot(labels)
	ok = true
	how := map[string]string{"cow": "copy-on-write", "full": "cópia cheia (fs sem reflink)"}[mode]
	logf("branch %q de %q: clone %s em %s, pronta em %s", name, base[lName], how,
		cloned.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	u, err := branchURL(name)
	if err != nil {
		return err
	}
	fmt.Println(u)
	return nil
}

func branchURL(name string) (string, error) {
	info, err := inspectContainer(branchContainer(name))
	if err != nil {
		return "", fmt.Errorf("branch %q não existe", name)
	}
	if !info.State.Running {
		return "", fmt.Errorf("branch %q está %s (pgb branch start %s)", name, info.State.Status, name)
	}
	port, err := hostPort(info.Name)
	if err != nil {
		return "", err
	}
	l := info.Config.Labels
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(l[lUser], l[lPassword]),
		Host:     "127.0.0.1:" + port,
		Path:     "/" + l[lDB],
		RawQuery: "sslmode=disable",
	}
	return u.String(), nil
}

func branchStart(name string) error {
	info, err := inspectContainer(branchContainer(name))
	if err != nil {
		return fmt.Errorf("branch %q não existe", name)
	}
	if _, err := docker("start", info.Name); err != nil {
		return err
	}
	l := info.Config.Labels
	if err := waitReady(info.Name, l[lUser], l[lDB], time.Minute); err != nil {
		return err
	}
	u, err := branchURL(name)
	if err != nil {
		return err
	}
	fmt.Println(u)
	return nil
}

func branchList() error {
	names, err := listByLabel("container", lKind+"=branch")
	if err != nil {
		return err
	}
	t := newTable("BRANCH", "BASE", "STATUS", "EXPIRA", "URL")
	seen := map[string]bool{}
	for _, n := range names {
		info, err := inspectContainer(n)
		if err != nil {
			continue
		}
		l := info.Config.Labels
		seen[l[lName]] = true
		u, _ := branchURL(l[lName])
		t.row(l[lName], l[lBase], info.State.Status, orDash(l[lExpires]), orDash(u))
	}
	vols, _ := listByLabel("volume", lKind+"=branch")
	for _, v := range vols {
		if l, err := volumeLabels(v); err == nil && !seen[l[lName]] {
			t.row(l[lName], l[lBase], "sem container (pgb branch rm "+l[lName]+")", orDash(l[lExpires]), "-")
		}
	}
	t.flush()
	return nil
}

// branchRm usa cleanup(): precisa terminar mesmo depois de um Ctrl+C.
func branchRm(name string) error {
	c, vol := branchContainer(name), branchVolume(name)
	if !exists("container", c) && !exists("volume", vol) {
		return fmt.Errorf("branch %q não existe", name)
	}
	cleanup("rm", "-f", "-v", c)
	cleanupHelpers("branch:" + name)
	cleanup("volume", "rm", "-f", vol)
	if exists("volume", vol) {
		return fmt.Errorf("não consegui remover o volume %s", vol)
	}
	logf("branch %q removida", name)
	return nil
}

// branchReset descarta tudo o que a branch mudou e reclona da mesma base (mesmo TTL e porta).
func branchReset(name string) error {
	info, err := inspectContainer(branchContainer(name))
	if err != nil {
		return fmt.Errorf("branch %q não existe", name)
	}
	l := info.Config.Labels
	base, err := loadBase(l[lBase])
	if err != nil {
		return err
	}
	if err := branchRm(name); err != nil {
		return err
	}
	return createBranch(name, base, l[lExpires], l[lPort])
}

// branchExec roda um comando com DATABASE_URL apontando para a branch e devolve o exit code
// dele (128+N se morreu pelo sinal N, 127 se não existe). Sinais recebidos pelo pgb são
// repassados ao comando; com --rm, a branch é apagada em qualquer saída.
func branchExec(name string, create bool, from string, ttl time.Duration, rm bool, argv []string) (int, error) {
	if create {
		// A URL impressa pelo create vai para o stderr aqui, o stdout fica só para o comando.
		stdout := os.Stdout
		os.Stdout = os.Stderr
		err := branchCreate(name, from, ttl)
		os.Stdout = stdout
		if err != nil {
			return 1, err
		}
	}
	u, err := branchURL(name)
	if err != nil {
		if create && rm {
			branchRm(name)
		}
		return 1, err
	}
	if rm {
		defer branchRm(name)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+u, "PGB_BRANCH="+name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return 127, err
		}
		return 1, err
	}
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigc)
	go func() {
		// Como o docker stop: repassa o sinal e, se o comando ignorar, mata em 10 s. Sem isso um
		// `sh` que só recebeu o SIGINT (kill no PID do pgb, típico de CI) fica esperando o filho.
		var kill <-chan time.Time
		for {
			select {
			case s := <-sigc:
				cmd.Process.Signal(s)
				if kill == nil {
					kill = time.After(10 * time.Second)
				}
			case <-kill:
				cmd.Process.Kill()
				return
			}
		}
	}()
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// gc apaga as branches com TTL vencido (inclusive volumes órfãos), sem parar no primeiro erro.
func gc() error {
	var errs []error
	seen := map[string]bool{}
	names, _ := listByLabel("container", lKind+"=branch")
	vols, _ := listByLabel("volume", lKind+"=branch")
	check := func(l map[string]string) {
		if seen[l[lName]] {
			return
		}
		seen[l[lName]] = true
		if exp, err := time.Parse(time.RFC3339, l[lExpires]); err == nil && time.Now().After(exp) {
			if err := branchRm(l[lName]); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, n := range names {
		if info, err := inspectContainer(n); err == nil {
			check(info.Config.Labels)
		}
	}
	for _, v := range vols {
		if l, err := volumeLabels(v); err == nil {
			check(l)
		}
	}
	return errors.Join(errs...)
}
