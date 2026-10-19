package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Uma base é um volume com um data dir consistente e PARADO. Branches são clones dela;
// como ninguém escreve na base, o clone (CoW ou cópia cheia) é sempre consistente.
//
// Ciclo de vida (à prova de Ctrl+C e de dois `create` ao mesmo tempo):
//  1. volume pgb-base-<n> com os labels;
//  2. container pgb_tmp_<n> criado com `docker create`: o nome é único no daemon, então ele é o
//     lock (quem perde não toca em nada); `_` não é válido em nome de branch, então não colide;
//  3. cópia e finalização (máscara, shutdown limpo);
//  4. volume pgb-ready-<n>: só a partir dele a base aparece como pronta e pode ser clonada.

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
	lPort     = "pgb.port"
	lOwner    = "pgb.owner"
)

func baseVolume(name string) string { return "pgb-base-" + name }
func baseReady(name string) string  { return "pgb-ready-" + name }
func baseTmp(name string) string    { return "pgb_tmp_" + name }

// baseBuild guarda o que este processo criou, para desfazer só isso se algo falhar.
type baseBuild struct {
	name   string
	labels map[string]string
	locked bool
	done   bool
}

// lock cria o volume e o container temporário (o lock). createArgs vão para o `docker create`.
func (b *baseBuild) lock(createArgs ...string) error {
	vol := baseVolume(b.name)
	if exists("volume", baseReady(b.name)) {
		return fmt.Errorf("base %q já existe (pgb base rm %s)", b.name, b.name)
	}
	if exists("container", baseTmp(b.name)) || exists("volume", vol) {
		return fmt.Errorf("base %q está sendo criada, ou sobrou de uma criação interrompida (pgb base rm %s)", b.name, b.name)
	}
	if _, err := docker(append([]string{"volume", "create"}, append(labelArgs(b.labels), vol)...)...); err != nil {
		return err
	}
	args := []string{"create", "--name", baseTmp(b.name), "--label", lKind + "=tmp", "--label", lOwner + "=base:" + b.name,
		"-v", vol + ":" + b.labels[lPGData]}
	args = append(append(args, createArgs...), b.labels[lImage])
	if _, err := docker(args...); err != nil {
		if isConflict(err) {
			return fmt.Errorf("base %q está sendo criada por outro processo", b.name)
		}
		return err
	}
	b.locked = true
	return nil
}

// finish marca a base como pronta.
func (b *baseBuild) finish() error {
	args := []string{"volume", "create", "--label", lKind + "=ready", "--label", lName + "=" + b.name}
	if b.labels[lPassword] != "" {
		args = append(args, "--label", lPassword+"="+b.labels[lPassword])
	}
	if _, err := docker(append(args, baseReady(b.name))...); err != nil {
		return err
	}
	b.done = true
	logf("base %q pronta", b.name)
	return nil
}

// release roda no defer: sempre tira o lock; se não terminou, apaga o que este processo criou.
func (b *baseBuild) release() {
	if !b.locked {
		return
	}
	cleanup("rm", "-f", "-v", baseTmp(b.name))
	cleanupHelpers("base:" + b.name)
	if !b.done {
		cleanup("volume", "rm", "-f", baseVolume(b.name))
		logf("base %q desfeita", b.name)
	}
}

// sourceAuth descobre usuário, senha e banco de um container Postgres: labels do pgb (quando a
// origem é uma branch), senão as variáveis da imagem oficial, inclusive POSTGRES_PASSWORD_FILE.
func sourceAuth(info containerInfo) (user, pass, db string) {
	if l := info.Config.Labels; l[lKind] == "branch" {
		return l[lUser], l[lPassword], l[lDB]
	}
	user = info.env("POSTGRES_USER", "postgres")
	pass = info.env("POSTGRES_PASSWORD", "")
	if f := info.env("POSTGRES_PASSWORD_FILE", ""); pass == "" && f != "" {
		pass, _ = docker("exec", info.Name, "cat", f)
	}
	return user, pass, info.env("POSTGRES_DB", user)
}

// baseFromContainer copia um Postgres em Docker que está rodando, sem pará-lo: pg_basebackup
// com -X stream (slot temporário segura o WAL) num container auxiliar na rede da origem.
func baseFromContainer(name, src, maskFile string) error {
	var mask []byte
	if maskFile != "" {
		var err error
		if mask, err = os.ReadFile(maskFile); err != nil {
			return err
		}
	}
	info, err := inspectContainer(src)
	if err != nil {
		return err
	}
	if !info.State.Running {
		return fmt.Errorf("container %s não está rodando (%s)", src, info.State.Status)
	}
	user, pass, db := sourceAuth(info)
	b := &baseBuild{name: name, labels: map[string]string{
		lKind:     "base",
		lName:     name,
		lImage:    info.Config.Image,
		lPGData:   info.env("PGDATA", "/var/lib/postgresql/data"),
		lUser:     user,
		lPassword: pass,
		lDB:       db,
		lSource:   "container:" + src,
		lCreated:  time.Now().UTC().Format(time.RFC3339),
	}}
	if err := b.lock(); err != nil {
		return err
	}
	defer b.release()

	logf("copiando %s com pg_basebackup...", src)
	start := time.Now()
	args := append([]string{"run", "--rm"}, helperArgs("base:"+name)...)
	args = append(args, "--network", "container:"+src, "-e", "PGPASSWORD", "-v", baseVolume(name)+":/dst",
		"--entrypoint", "sh", info.Config.Image, "-c",
		`pg_basebackup -h 127.0.0.1 -p "$2" -U "$1" -D /dst -X stream -c fast -w && chown -R postgres:postgres /dst && chmod 700 /dst`,
		"sh", user, info.env("PGPORT", "5432"))
	if _, err := dockerIO(appCtx, nil, append(os.Environ(), "PGPASSWORD="+pass), args...); err != nil {
		return fmt.Errorf("pg_basebackup: %w", err)
	}
	logf("cópia feita em %s", time.Since(start).Round(time.Millisecond))

	if err := finalizeBase(b, mask); err != nil {
		return err
	}
	return b.finish()
}

// finalizeBase sobe a base uma vez (o Postgres aplica o WAL do backup), garante uma senha para
// a URL das branches, roda o mascaramento opcional e desliga limpo. Cada branch nasce sem
// recovery para fazer.
func finalizeBase(b *baseBuild, mask []byte) error {
	tmp, l := baseTmp(b.name), b.labels
	if _, err := docker("start", tmp); err != nil {
		return err
	}
	if err := waitReady(tmp, l[lUser], l[lDB], 2*time.Minute); err != nil {
		return err
	}
	if l[lPassword] == "" {
		// Origem sem senha conhecida (trust): a base ganha uma, senão a URL não conecta por TCP.
		l[lPassword] = localPassword
		sql := fmt.Sprintf(`ALTER ROLE "%s" PASSWORD '%s'`, strings.ReplaceAll(l[lUser], `"`, `""`), localPassword)
		if err := psqlExec(tmp, l[lUser], l[lDB], []byte(sql)); err != nil {
			return err
		}
		// Labels de volume não mudam depois de criados: finish() grava a senha nova no ready.
	}
	if mask != nil {
		logf("aplicando máscara...")
		if err := psqlExec(tmp, l[lUser], l[lDB], mask); err != nil {
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
	deps := map[string]int{}
	names, _ := listByLabel("container", lKind+"=branch")
	for _, n := range names {
		if info, err := inspectContainer(n); err == nil {
			deps[info.Config.Labels[lBase]]++
		}
	}
	t := newTable("BASE", "STATUS", "ORIGEM", "IMAGEM", "CRIADA", "BRANCHES")
	for _, v := range vols {
		l, err := volumeLabels(v)
		if err != nil {
			continue
		}
		status := "pronta"
		if !exists("volume", baseReady(l[lName])) {
			status = "incompleta"
			if exists("container", baseTmp(l[lName])) {
				status = "criando"
			}
		}
		t.row(l[lName], status, l[lSource], l[lImage], l[lCreated], fmt.Sprint(deps[l[lName]]))
	}
	t.flush()
	return nil
}

func baseRm(name string, force bool) error {
	if !exists("volume", baseVolume(name)) && !exists("volume", baseReady(name)) && !exists("container", baseTmp(name)) {
		return fmt.Errorf("base %q não existe", name)
	}
	deps := branchesOf(name)
	if len(deps) > 0 && !force {
		return fmt.Errorf("base %q tem branches (%s); use --force para apagar tudo", name, strings.Join(deps, ", "))
	}
	var errs []error
	for _, br := range deps {
		errs = append(errs, branchRm(br))
	}
	cleanup("volume", "rm", "-f", baseReady(name))
	cleanup("rm", "-f", "-v", baseTmp(name))
	cleanupHelpers("base:" + name)
	if _, err := dockerIO(appCtx, nil, nil, "volume", "rm", "-f", baseVolume(name)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// branchesOf junta as branches com container e os volumes de branch órfãos da base.
func branchesOf(base string) []string {
	seen := map[string]bool{}
	var out []string
	cs, _ := listByLabel("container", lBase+"="+base)
	vs, _ := listByLabel("volume", lBase+"="+base)
	for _, n := range append(cs, vs...) {
		br := strings.TrimPrefix(strings.TrimPrefix(n, "pgb-br-"), "pgb-")
		if !seen[br] {
			seen[br] = true
			out = append(out, br)
		}
	}
	return out
}

// loadBase devolve os labels de uma base PRONTA.
func loadBase(name string) (map[string]string, error) {
	if name == "" {
		ready, err := listByLabel("volume", lKind+"=ready")
		if err != nil {
			return nil, err
		}
		if len(ready) != 1 {
			return nil, fmt.Errorf("há %d bases prontas; escolha uma com --from", len(ready))
		}
		name = strings.TrimPrefix(ready[0], "pgb-ready-")
	}
	if !exists("volume", baseReady(name)) {
		if exists("volume", baseVolume(name)) {
			return nil, fmt.Errorf("base %q está incompleta (criação interrompida?): pgb base rm %s", name, name)
		}
		return nil, fmt.Errorf("base %q não existe", name)
	}
	l, err := volumeLabels(baseVolume(name))
	if err != nil {
		return nil, fmt.Errorf("base %q: volume de dados sumiu", name)
	}
	if rl, err := volumeLabels(baseReady(name)); err == nil && rl[lPassword] != "" {
		l[lPassword] = rl[lPassword]
	}
	return l, nil
}
