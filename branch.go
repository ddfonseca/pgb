package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"time"
)

func branchContainer(name string) string { return "pgb-" + name }
func branchVolume(name string) string    { return "pgb-br-" + name }

// cloneScript tenta CoW (reflink: btrfs, XFS, APFS via virtiofs...) e cai para cópia cheia.
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
	return createBranch(name, base, expires)
}

func createBranch(name string, base map[string]string, expires string) error {
	c, vol := branchContainer(name), branchVolume(name)
	if exists("container", c) || exists("volume", vol) {
		return fmt.Errorf("branch %q já existe", name)
	}
	labels := map[string]string{
		lKind: "branch", lName: name, lBase: base[lName],
		lUser: base[lUser], lPassword: base[lPassword], lDB: base[lDB],
		lCreated: time.Now().UTC().Format(time.RFC3339),
	}
	if expires != "" {
		labels[lExpires] = expires
	}
	if _, err := docker(append([]string{"volume", "create"}, append(labelArgs(labels), vol)...)...); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			docker("rm", "-f", c)
			docker("volume", "rm", "-f", vol)
		}
	}()

	start := time.Now()
	mode, err := docker("run", "--rm",
		"-v", baseVolume(base[lName])+":/src:ro", "-v", vol+":/dst",
		base[lImage], "sh", "-c", cloneScript)
	if err != nil {
		return fmt.Errorf("clonando a base: %w", err)
	}
	cloned := time.Since(start)

	args := append([]string{"run", "-d", "--name", c}, labelArgs(labels)...)
	args = append(args, "-p", "127.0.0.1::5432", "-v", vol+":"+base[lPGData], base[lImage])
	if _, err := docker(args...); err != nil {
		return err
	}
	if err := waitReady(c, base[lUser], base[lDB], time.Minute); err != nil {
		return err
	}
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
	if info.State.Status != "running" {
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
	for _, n := range names {
		info, err := inspectContainer(n)
		if err != nil {
			continue
		}
		l := info.Config.Labels
		u, _ := branchURL(l[lName])
		t.row(l[lName], l[lBase], info.State.Status, orDash(l[lExpires]), orDash(u))
	}
	t.flush()
	return nil
}

func branchRm(name string) error {
	c, vol := branchContainer(name), branchVolume(name)
	if !exists("container", c) && !exists("volume", vol) {
		return fmt.Errorf("branch %q não existe", name)
	}
	docker("rm", "-f", c)
	if exists("volume", vol) {
		if _, err := docker("volume", "rm", vol); err != nil {
			return err
		}
	}
	logf("branch %q removida", name)
	return nil
}

// branchReset descarta tudo o que a branch mudou e reclona da mesma base (mesmo TTL).
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
	return createBranch(name, base, l[lExpires])
}

// branchExec roda um comando com DATABASE_URL apontando para a branch e devolve o exit code dele.
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
	if rm {
		defer branchRm(name)
	}
	u, err := branchURL(name)
	if err != nil {
		return 1, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+u, "PGB_BRANCH="+name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// gc apaga as branches com TTL vencido.
func gc() error {
	names, err := listByLabel("container", lKind+"=branch")
	if err != nil {
		return err
	}
	for _, n := range names {
		info, err := inspectContainer(n)
		if err != nil {
			continue
		}
		l := info.Config.Labels
		exp, err := time.Parse(time.RFC3339, l[lExpires])
		if err == nil && time.Now().After(exp) {
			if err := branchRm(l[lName]); err != nil {
				return err
			}
		}
	}
	return nil
}
