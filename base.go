package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Uma base é um volume com um data dir consistente e PARADO. Branches são clones dela;
// como ninguém escreve na base, o clone (CoW ou cópia cheia) é sempre consistente.

const (
	lKind     = "pgb.kind"
	lName     = "pgb.name"
	lBase     = "pgb.base"
	lImage    = "pgb.image"
	lPGData   = "pgb.pgdata"
	lUser     = "pgb.user"
	lPassword = "pgb.password"
	lDB       = "pgb.db"
	lSource   = "pgb.source"
	lCreated  = "pgb.created"
	lExpires  = "pgb.expires"
)

func baseVolume(name string) string { return "pgb-base-" + name }

// baseFromContainer copia um Postgres em Docker que está rodando, sem pará-lo:
// pg_basebackup em tar pelo stdout (com o WAL necessário), extraído direto no volume da base.
func baseFromContainer(name, src, maskFile string) error {
	vol := baseVolume(name)
	if exists("volume", vol) {
		return fmt.Errorf("base %q já existe (pgb base rm %s)", name, name)
	}
	info, err := inspectContainer(src)
	if err != nil {
		return err
	}
	if info.State.Status != "running" {
		return fmt.Errorf("container %s não está rodando (%s)", src, info.State.Status)
	}
	user := info.env("POSTGRES_USER", "postgres")
	labels := map[string]string{
		lKind:     "base",
		lName:     name,
		lImage:    info.Config.Image,
		lPGData:   info.env("PGDATA", "/var/lib/postgresql/data"),
		lUser:     user,
		lPassword: info.env("POSTGRES_PASSWORD", ""),
		lDB:       info.env("POSTGRES_DB", user),
		lSource:   "container:" + src,
		lCreated:  time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := docker(append([]string{"volume", "create"}, append(labelArgs(labels), vol)...)...); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			docker("volume", "rm", "-f", vol)
		}
	}()

	logf("copiando %s com pg_basebackup...", src)
	start := time.Now()
	err = dockerPipe(
		[]string{"exec", "-u", "postgres", src,
			"pg_basebackup", "-U", user, "-D", "-", "-Ft", "-X", "fetch", "--checkpoint=fast"},
		[]string{"run", "--rm", "-i", "-v", vol + ":/dst", info.Config.Image,
			"sh", "-c", "tar -x -C /dst && chown -R postgres:postgres /dst && chmod 700 /dst"},
	)
	if err != nil {
		return fmt.Errorf("pg_basebackup: %w", err)
	}
	logf("cópia feita em %s", time.Since(start).Round(time.Millisecond))

	if err := finalizeBase(name, labels, maskFile); err != nil {
		return err
	}
	ok = true
	logf("base %q pronta", name)
	return nil
}

// finalizeBase sobe a base uma vez (o Postgres aplica o WAL do backup), roda o mascaramento
// opcional e desliga limpo. Assim cada branch já nasce sem recovery para fazer.
func finalizeBase(name string, l map[string]string, maskFile string) error {
	tmp := "pgb-tmp-" + name
	docker("rm", "-f", tmp)
	defer docker("rm", "-f", tmp)
	if _, err := docker("run", "-d", "--name", tmp, "-v", baseVolume(name)+":"+l[lPGData], l[lImage]); err != nil {
		return err
	}
	if err := waitReady(tmp, l[lUser], l[lDB], 2*time.Minute); err != nil {
		return err
	}
	if maskFile != "" {
		f, err := os.Open(maskFile)
		if err != nil {
			return err
		}
		defer f.Close()
		logf("aplicando %s...", maskFile)
		cmd := exec.Command("docker", "exec", "-i", tmp,
			"psql", "-q", "-v", "ON_ERROR_STOP=1", "-U", l[lUser], "-d", l[lDB])
		cmd.Stdin, cmd.Stdout, cmd.Stderr = f, os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("mascaramento falhou: %w", err)
		}
	}
	// A imagem oficial usa STOPSIGNAL SIGINT (fast shutdown com checkpoint).
	_, err := docker("stop", "-t", "60", tmp)
	return err
}

func baseList() error {
	vols, err := listByLabel("volume", lKind+"=base")
	if err != nil {
		return err
	}
	branches := map[string]int{}
	names, _ := listByLabel("container", lKind+"=branch")
	for _, n := range names {
		if info, err := inspectContainer(n); err == nil {
			branches[info.Config.Labels[lBase]]++
		}
	}
	t := newTable("BASE", "ORIGEM", "IMAGEM", "CRIADA", "BRANCHES")
	for _, v := range vols {
		l, err := volumeLabels(v)
		if err != nil {
			continue
		}
		t.row(l[lName], l[lSource], l[lImage], l[lCreated], fmt.Sprint(branches[l[lName]]))
	}
	t.flush()
	return nil
}

func baseRm(name string, force bool) error {
	vol := baseVolume(name)
	if !exists("volume", vol) {
		return fmt.Errorf("base %q não existe", name)
	}
	deps, _ := listByLabel("container", lBase+"="+name)
	if len(deps) > 0 && !force {
		return fmt.Errorf("base %q tem branches (%s); use --force para apagar tudo", name, strings.Join(deps, ", "))
	}
	for _, c := range deps {
		if err := branchRm(strings.TrimPrefix(c, "pgb-")); err != nil {
			return err
		}
	}
	_, err := docker("volume", "rm", vol)
	return err
}

func loadBase(name string) (map[string]string, error) {
	if name == "" {
		vols, err := listByLabel("volume", lKind+"=base")
		if err != nil {
			return nil, err
		}
		if len(vols) != 1 {
			return nil, fmt.Errorf("há %d bases; escolha uma com --from", len(vols))
		}
		return volumeLabels(vols[0])
	}
	l, err := volumeLabels(baseVolume(name))
	if err != nil {
		return nil, fmt.Errorf("base %q não existe", name)
	}
	return l, nil
}
