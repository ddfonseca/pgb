package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
	lBranch   = "pgb.branch"   // ponto de salvamento: a branch dona
	lFamily   = "pgb.family"   // bases versionadas: prod.20261020153000 é da família prod
	lRecipe   = "pgb.recipe"   // como a base foi criada (JSON), para o `base refresh` refazer
	lAt       = "pgb.at"       // branch: --at, reaplicado no reset (ver timeshift.go)
	lTimeSkip = "pgb.timeskip" // base: time_shift.skip do pgb.yaml, globs separados por vírgula
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
func baseFromContainer(name, family, src, maskFile string) error {
	var mask []byte
	if maskFile != "" {
		var err error
		if mask, err = os.ReadFile(maskFile); err != nil {
			return err
		}
		if maskFile, err = filepath.Abs(maskFile); err != nil {
			return err
		}
	}
	recipe, _ := json.Marshal(baseRecipe{Kind: "container", From: src, Mask: maskFile})
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
		lFamily:   family,
		lRecipe:   string(recipe),
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
	current := map[string]string{} // família -> versão atual
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
		} else if fam := familyOf(l); len(baseVersions(fam)) > 1 {
			if _, ok := current[fam]; !ok {
				current[fam], _ = resolveBase(fam)
			}
			if current[fam] == l[lName] {
				status = "pronta (atual de " + fam + ")"
			}
		}
		t.row(l[lName], status, l[lSource], l[lImage], l[lCreated], fmt.Sprint(deps[l[lName]]))
	}
	t.flush()
	return nil
}

func baseRm(name string, force bool) error {
	if !exists("volume", baseVolume(name)) && !exists("volume", baseReady(name)) && !exists("container", baseTmp(name)) {
		if vs := baseVersions(name); len(vs) > 0 {
			return fmt.Errorf("%q é uma família de bases; apague uma versão: %s", name, strings.Join(vs, ", "))
		}
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

// loadBase devolve os labels de uma base PRONTA. O nome pode ser uma versão exata
// (prod.20261020153000) ou uma família (prod), que vira a versão pronta mais nova.
// Sem nome, vale se só houver uma família pronta.
func loadBase(name string) (map[string]string, error) {
	if name == "" {
		fams := map[string]bool{}
		ready, err := listByLabel("volume", lKind+"=ready")
		if err != nil {
			return nil, err
		}
		for _, r := range ready {
			if l, err := volumeLabels(baseVolume(strings.TrimPrefix(r, "pgb-ready-"))); err == nil {
				fams[familyOf(l)] = true
			}
		}
		if len(fams) != 1 {
			return nil, fmt.Errorf("há %d bases prontas; escolha uma com --from", len(fams))
		}
		for f := range fams {
			name = f
		}
	}
	exact, err := resolveBase(name)
	if err != nil {
		return nil, err
	}
	l, err := volumeLabels(baseVolume(exact))
	if err != nil {
		return nil, fmt.Errorf("base %q: volume de dados sumiu", exact)
	}
	if rl, err := volumeLabels(baseReady(exact)); err == nil && rl[lPassword] != "" {
		l[lPassword] = rl[lPassword]
	}
	if l[lFamily] == "" {
		l[lFamily] = exact
	}
	return l, nil
}

var baseRef = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}(\.[0-9]{14})?$`)

// baseRefArg valida nomes de base que podem ser uma versão (com o sufixo .AAAAMMDDhhmmss).
func baseRefArg(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("informe um nome")
	}
	if !baseRef.MatchString(args[0]) {
		return fmt.Errorf("nome de base inválido %q", args[0])
	}
	return nil
}

func familyOf(l map[string]string) string { return first(l[lFamily], l[lName]) }

// baseVersions lista as bases (prontas ou não) de uma família, da mais nova para a mais velha.
// Inclui a base com o próprio nome da família, criada antes do versionamento ou por `base create`.
func baseVersions(family string) []string {
	type v struct{ name, created string }
	var vs []v
	seen := map[string]bool{}
	add := func(vol string) {
		l, err := volumeLabels(vol)
		if err != nil || seen[l[lName]] || familyOf(l) != family {
			return
		}
		seen[l[lName]] = true
		vs = append(vs, v{l[lName], l[lCreated]})
	}
	if exists("volume", baseVolume(family)) {
		add(baseVolume(family))
	}
	vols, _ := listByLabel("volume", lFamily+"="+family)
	for _, vol := range vols {
		if strings.HasPrefix(vol, "pgb-base-") {
			add(vol)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].created > vs[j].created })
	out := make([]string, len(vs))
	for i, x := range vs {
		out[i] = x.name
	}
	return out
}

// resolveBase devolve o nome exato da base pronta: o próprio nome se for uma versão, senão a
// versão pronta mais nova da família.
func resolveBase(name string) (string, error) {
	if strings.Contains(name, ".") {
		if exists("volume", baseReady(name)) {
			return name, nil
		}
	} else {
		for _, v := range baseVersions(name) {
			if exists("volume", baseReady(v)) {
				return v, nil
			}
		}
	}
	if exists("volume", baseVolume(name)) {
		return "", fmt.Errorf("base %q está incompleta (criação interrompida?): pgb base rm %s", name, name)
	}
	return "", fmt.Errorf("base %q não existe", name)
}

// baseRefresh recria uma família de base com a receita da versão atual, numa versão nova, e
// apaga as versões velhas que passaram do --keep e não têm branches.
func baseRefresh(family string, keep int, dryRun bool) error {
	if keep < 1 {
		return fmt.Errorf("--keep precisa ser pelo menos 1")
	}
	cur, err := loadBase(family)
	if err != nil {
		return err
	}
	if familyOf(cur) != family {
		return fmt.Errorf("%q é uma versão da base %q: use pgb base refresh %s", family, familyOf(cur), familyOf(cur))
	}
	var r baseRecipe
	if err := json.Unmarshal([]byte(cur[lRecipe]), &r); err != nil || r.Kind == "" {
		return fmt.Errorf("base %q foi criada antes do refresh existir: recrie com pgb base create", cur[lName])
	}
	name := family + "." + time.Now().UTC().Format("20060102150405")
	logf("refresh de %q (atual: %s) como %s", family, cur[lName], name)
	switch r.Kind {
	case "container":
		if dryRun {
			return fmt.Errorf("--dry-run só vale para bases de origem remota")
		}
		err = baseFromContainer(name, family, r.From, r.Mask)
	case "remote":
		if r.Cwd != "" {
			if err := os.Chdir(r.Cwd); err != nil {
				return fmt.Errorf("diretório de onde a base foi criada sumiu: %w", err)
			}
		}
		o := r.remoteOpts()
		o.dryRun = dryRun
		if o.fromURL != "" && os.Getenv("PGPASSWORD") == "" {
			logf("a URL de origem foi guardada sem a senha; se a conexão falhar, rode com PGPASSWORD=...")
		}
		err = baseFromRemote(name, family, o)
	default:
		return fmt.Errorf("receita desconhecida %q", r.Kind)
	}
	if err != nil || dryRun {
		return err
	}

	var errs []error
	for i, v := range baseVersions(family) {
		if i < keep || v == name {
			continue
		}
		if deps := branchesOf(v); len(deps) > 0 {
			logf("versão %s mantida: tem branches (%s); pgb branch reset passa elas para a nova", v, strings.Join(deps, ", "))
			continue
		}
		if err := baseRm(v, false); err != nil {
			errs = append(errs, err)
		} else {
			logf("versão antiga %s apagada", v)
		}
	}
	return errors.Join(errs...)
}
