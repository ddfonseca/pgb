package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"google.golang.org/api/impersonate"
)

// Cópia LÓGICA de um Postgres remoto (Cloud SQL, ou qualquer URL) para uma base local:
// schema inteiro via pg_dump; dados tabela a tabela com COPY (SELECT ...) TO STDOUT, já
// filtrados e mascarados na origem, para que o que não foi pedido nunca saia de lá.
// Tudo numa transação REPEATABLE READ READ ONLY, com o mesmo snapshot no pg_dump.

const localPassword = "pgb"

type remoteOpts struct {
	configFile string
	configSet  bool
	profile    string
	params     map[string]string
	since      string
	maxSize    string
	fromURL    string
	cloudSQL   string
	image      string
	dryRun     bool
	strict     bool
}

type source struct {
	conn     *pgx.Conn
	dumpArgs []string // args do `docker run` + do pg_dump para alcançar a origem
	env      []string // PGPASSWORD para o cliente docker repassar
	label    string
	user, db string
	close    func()
}

type tablePlan struct {
	schema, name, display string
	rule                  TableRule
	pattern               string
	cols                  []string
	selectSQL             string
	bytes, est            int64
	rows                  int64
}

// baseRecipe é como uma base foi criada, guardada no label pgb.recipe para o `base refresh`
// refazer igual. A URL vai sem senha (labels aparecem no docker inspect): no refresh, a senha
// vem de novo do env_file, ou do PGPASSWORD.
type baseRecipe struct {
	Kind     string            `json:"kind"` // container | remote
	From     string            `json:"from,omitempty"`
	Mask     string            `json:"mask,omitempty"`
	Config   string            `json:"config,omitempty"`
	Cwd      string            `json:"cwd,omitempty"` // env_file é relativo ao diretório de onde o create rodou
	Profile  string            `json:"profile,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Since    string            `json:"since,omitempty"`
	MaxSize  string            `json:"max_size,omitempty"`
	FromURL  string            `json:"from_url,omitempty"`
	CloudSQL string            `json:"cloudsql,omitempty"`
	Image    string            `json:"image,omitempty"`
	Strict   bool              `json:"strict,omitempty"`
}

func (o remoteOpts) recipe() baseRecipe {
	r := baseRecipe{Kind: "remote", Profile: o.profile, Params: o.params, Since: o.since, MaxSize: o.maxSize,
		CloudSQL: o.cloudSQL, Image: o.image, Strict: o.strict}
	r.Cwd, _ = os.Getwd()
	if _, err := os.Stat(o.configFile); err == nil {
		r.Config, _ = filepath.Abs(o.configFile)
	}
	if u, err := url.Parse(o.fromURL); err == nil && o.fromURL != "" {
		if u.User != nil {
			u.User = url.User(u.User.Username())
		}
		r.FromURL = u.String()
	}
	return r
}

func (r baseRecipe) remoteOpts() remoteOpts {
	return remoteOpts{configFile: first(r.Config, "pgb.yaml"), configSet: r.Config != "", profile: r.Profile,
		params: r.Params, since: r.Since, maxSize: r.MaxSize, fromURL: r.FromURL, cloudSQL: r.CloudSQL,
		image: r.Image, strict: r.Strict}
}

func baseFromRemote(name, family string, o remoteOpts) error {
	ctx := appCtx
	cfg, err := loadConfig(o.configFile, o.configSet)
	if err != nil {
		return err
	}
	sel, err := cfg.selection(o.profile, o.params)
	if err != nil {
		return err
	}
	since, err := parseSince(first(o.since, cfg.Since))
	if err != nil {
		return err
	}
	maxSize, err := parseSize(first(o.maxSize, cfg.MaxSize, "20GB"))
	if err != nil {
		return err
	}
	image := first(o.image, cfg.Image, "postgres:16")
	if !o.dryRun && (exists("volume", baseReady(name)) || exists("volume", baseVolume(name))) {
		return fmt.Errorf("base %q já existe (pgb base rm %s)", name, name)
	}

	src, err := openSource(ctx, cfg, o)
	if err != nil {
		return err
	}
	defer src.close()
	tx, err := src.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var snapshot, version string
	if err := tx.QueryRow(ctx, "select pg_export_snapshot(), current_setting('server_version')").Scan(&snapshot, &version); err != nil {
		return fmt.Errorf("exportando snapshot: %w", err)
	}
	logf("origem %s (Postgres %s), snapshot %s", src.label, version, snapshot)

	plans, err := planTables(ctx, tx, sel, cfg, since, o.params)
	if err != nil {
		return err
	}
	total := printPlan(plans)
	if total > maxSize {
		return fmt.Errorf("estimativa %s passa do max_size %s: filtre mais tabelas no pgb.yaml ou use --max-size",
			humanSize(total), humanSize(maxSize))
	}
	if o.dryRun {
		return nil
	}

	// O schema sai agora, no mesmo snapshot; o post-data (índices, FKs, triggers) só é aplicado
	// depois dos dados e da limpeza de órfãs.
	preData, err := dumpSchema(src, "base:"+name, image, snapshot, "pre-data")
	if err != nil {
		return err
	}
	postData, err := dumpSchema(src, "base:"+name, image, snapshot, "post-data")
	if err != nil {
		return err
	}
	fks, err := foreignKeys(ctx, tx)
	if err != nil {
		return err
	}

	pgdata, err := imagePGData(image)
	if err != nil {
		return err
	}
	labels := map[string]string{
		lKind: "base", lName: name, lImage: image, lPGData: pgdata,
		lUser: src.user, lPassword: localPassword, lDB: src.db,
		lSource: src.label, lCreated: time.Now().UTC().Format(time.RFC3339),
	}
	if o.profile != "" {
		labels["pgb.profile"] = o.profile
	}
	if len(cfg.TimeShift.Skip) > 0 {
		labels[lTimeSkip] = strings.Join(cfg.TimeShift.Skip, ",")
	}
	recipe, _ := json.Marshal(o.recipe())
	labels[lFamily], labels[lRecipe] = family, string(recipe)
	b := &baseBuild{name: name, labels: labels}
	if err := b.lock("-e", "POSTGRES_USER="+src.user, "-e", "POSTGRES_PASSWORD="+localPassword,
		"-e", "POSTGRES_DB="+src.db, "-p", "127.0.0.1::5432"); err != nil {
		return err
	}
	defer b.release()
	tmp := baseTmp(name)
	if _, err := docker("start", tmp); err != nil {
		return err
	}
	dst, err := connectLocal(ctx, tmp, src.user, src.db)
	if err != nil {
		return err
	}
	defer dst.Close(ctx)
	if err := checkDisk(tmp, pgdata, total); err != nil {
		return err
	}

	logf("aplicando schema (pre-data)...")
	if err := psqlExec(tmp, src.user, src.db, preData); err != nil {
		return fmt.Errorf("schema pre-data: %w", err)
	}
	start := time.Now()
	for _, p := range plans {
		if p.rule.Mode == "schema-only" {
			continue
		}
		t0 := time.Now()
		if err := copyTable(ctx, tx.Conn(), dst, p); err != nil {
			return fmt.Errorf("copiando %s: %w", p.display, err)
		}
		logf("  %-36s %8d linhas  %s", p.display, p.rows, time.Since(t0).Round(time.Millisecond))
	}
	if err := copySequences(ctx, tx, dst); err != nil {
		return err
	}
	tx.Rollback(ctx) // só leitura: libera a origem o quanto antes
	logf("dados copiados em %s", time.Since(start).Round(time.Millisecond))

	if err := cleanOrphans(ctx, dst, plans, fks, o.strict); err != nil {
		return err
	}
	// PII que passou sem máscara (coluna nova sem regra no pgb.yaml) desfaz a base no release.
	if err := auditCopy(ctx, dst, cfg); err != nil {
		return err
	}
	logf("aplicando índices, constraints e triggers (post-data)...")
	if err := psqlExec(tmp, src.user, src.db, postData); err != nil {
		return fmt.Errorf("schema post-data: %w", err)
	}
	if _, err := dst.Exec(ctx, "analyze"); err != nil {
		return err
	}
	dst.Close(ctx)
	if _, err := docker("stop", "-t", "60", tmp); err != nil {
		return err
	}
	return b.finish()
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Origem
// ---------------------------------------------------------------------------

func openSource(ctx context.Context, cfg *Config, o remoteOpts) (*source, error) {
	raw := o.fromURL
	if raw == "" {
		raw = cfg.Source.URL
	}
	if raw == "" && cfg.Source.EnvFile != "" {
		v, err := readEnvVar(cfg.Source.EnvFile, first(cfg.Source.URLVar, "DATABASE_URL"))
		if err != nil {
			return nil, err
		}
		raw = v
	}
	if raw == "" {
		return nil, fmt.Errorf("sem origem: use --from-url, ou source.url / source.env_file no pgb.yaml")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("URL de origem inválida: %w", err)
	}
	src := &source{close: func() {}, user: u.User.Username(), db: strings.TrimPrefix(u.Path, "/")}
	if src.db == "" {
		src.db = src.user
	}
	src.label = "url:" + u.Host + "/" + src.db

	// Cloud SQL: o conector Go abre um túnel local (o mesmo trabalho do cloud-sql-proxy);
	// da URL do .env só aproveitamos usuário, senha e banco.
	instance := first(o.cloudSQL, cfg.Source.CloudSQL)
	if o.fromURL != "" && o.cloudSQL == "" {
		instance = ""
	}
	if instance != "" {
		addr, stop, err := cloudSQLTunnel(ctx, instance, cfg.Source.Impersonate)
		if err != nil {
			return nil, err
		}
		src.close = stop
		u.Host = addr
		q := u.Query()
		q.Set("sslmode", "disable") // o túnel já é TLS ponta a ponta
		u.RawQuery = q.Encode()
		src.label = "cloudsql:" + instance + "/" + src.db
	}

	pc, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, err
	}
	pc.RuntimeParams["default_transaction_read_only"] = "on"
	pc.RuntimeParams["application_name"] = "pgb"
	conn, err := pgx.ConnectConfig(ctx, pc)
	if err != nil {
		src.close()
		return nil, fmt.Errorf("conectando na origem %s: %w", src.label, err)
	}
	stopTunnel := src.close
	src.conn = conn
	src.close = func() { conn.Close(ctx); stopTunnel() }

	pass, ok := u.User.Password()
	if !ok {
		pass = os.Getenv("PGPASSWORD") // URL sem senha (ex.: refresh de uma base criada com --from-url)
	}
	src.env = append(os.Environ(), "PGPASSWORD="+pass)
	net, host := containerNetwork(pc.Host)
	src.dumpArgs = append(net, "-e", "PGPASSWORD")
	src.dumpArgs = append(src.dumpArgs, "--host="+host, "--port="+strconv.Itoa(int(pc.Port)),
		"--username="+pc.User, "--dbname="+pc.Database)
	return src, nil
}

// containerNetwork: como um container alcança o host da origem. No Docker Engine do Linux
// (inclusive dentro do WSL) usa a rede do host; no Docker Desktop, host.docker.internal
// encaminha para o loopback da máquina.
func containerNetwork(host string) ([]string, string) {
	desktop := runtime.GOOS != "linux"
	if osName, err := docker("info", "--format", "{{.OperatingSystem}}"); err == nil && strings.Contains(osName, "Docker Desktop") {
		desktop = true
	}
	if !desktop {
		return []string{"--network", "host"}, host
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		host = "host.docker.internal"
	}
	return []string{"--add-host=host.docker.internal:host-gateway"}, host
}

func cloudSQLTunnel(ctx context.Context, instance, sa string) (string, func(), error) {
	var opts []cloudsqlconn.Option
	if sa != "" {
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: sa,
			Scopes:          []string{"https://www.googleapis.com/auth/cloud-platform"},
		})
		if err != nil {
			return "", nil, fmt.Errorf("impersonando %s: %w", sa, err)
		}
		opts = append(opts, cloudsqlconn.WithTokenSource(ts))
	}
	d, err := cloudsqlconn.NewDialer(ctx, opts...)
	if err != nil {
		return "", nil, fmt.Errorf("conector Cloud SQL (rode gcloud auth application-default login): %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		d.Close()
		return "", nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s, err := d.Dial(ctx, instance)
				if err != nil {
					logf("túnel Cloud SQL: %v", err)
					return
				}
				defer s.Close()
				go io.Copy(s, c)
				io.Copy(c, s)
			}()
		}
	}()
	logf("túnel Cloud SQL %s em %s", instance, ln.Addr())
	return ln.Addr().String(), func() { ln.Close(); d.Close() }, nil
}

func readEnvVar(file, key string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`), nil
		}
	}
	return "", fmt.Errorf("%s não tem %s", file, key)
}

// ---------------------------------------------------------------------------
// Plano: que tabelas, com que filtro e máscara, e quanto vai ocupar
// ---------------------------------------------------------------------------

func planTables(ctx context.Context, tx pgx.Tx, sel *selection, cfg *Config,
	since time.Duration, params map[string]string) ([]*tablePlan, error) {
	masks := cfg.Mask
	// Tabelas de extensões (spatial_ref_sys do PostGIS...) vêm com o CREATE EXTENSION;
	// partições vêm pela tabela-mãe.
	rows, err := tx.Query(ctx, `
		select c.oid, n.nspname, c.relname, pg_total_relation_size(c.oid), greatest(c.reltuples, 0)::bigint
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where c.relkind in ('r', 'p') and not c.relispartition
		  and n.nspname not in ('pg_catalog', 'information_schema') and n.nspname not like 'pg\_%'
		  and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass
		                  and d.objid = c.oid and d.deptype = 'e')
		order by 2, 3`)
	if err != nil {
		return nil, err
	}
	type rel struct {
		oid           uint32
		schema, name  string
		bytes, tuples int64
	}
	var rels []rel
	for rows.Next() {
		var r rel
		if err := rows.Scan(&r.oid, &r.schema, &r.name, &r.bytes, &r.tuples); err != nil {
			return nil, err
		}
		rels = append(rels, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	saltBytes := make([]byte, 16)
	rand.Read(saltBytes)
	salt := hex.EncodeToString(saltBytes)
	seenMask := map[string]bool{}
	var plans []*tablePlan
	for _, r := range rels {
		display := r.name
		if r.schema != "public" {
			display = r.schema + "." + r.name
		}
		p := &tablePlan{schema: r.schema, name: r.name, display: display, bytes: r.bytes}
		p.rule, p.pattern = ruleFor(sel.rules, display)
		if err := loadColumns(ctx, tx, r.oid, p); err != nil {
			return nil, err
		}
		mask := masks[display]
		seenMask[display] = mask != nil
		var cols []string
		known := map[string]bool{}
		for _, c := range p.cols {
			known[c] = true
			q := pgx.Identifier{c}.Sanitize()
			if m, ok := mask[c]; ok {
				cols = append(cols, maskExpr(m, q, salt, cfg.Presets)+" as "+q)
			} else {
				cols = append(cols, q)
			}
		}
		for c := range mask {
			if !known[c] {
				return nil, fmt.Errorf("mask: %s não tem a coluna %q", display, c)
			}
		}
		p.selectSQL = "select " + strings.Join(cols, ", ") + " from " + pgx.Identifier{r.schema, r.name}.Sanitize()
		var conds []string
		if p.rule.Mode == "where" {
			conds = append(conds, "("+p.rule.Where+")")
		}
		if p.rule.Mode != "schema-only" && !sel.explicit[display] {
			for col, val := range sel.scope {
				if known[col] {
					// ::text: a mesma coluna pode ter outro tipo noutra tabela (ex.: customer_id uuid numa e text noutra)
					conds = append(conds, pgx.Identifier{col}.Sanitize()+"::text = "+val)
				}
			}
		}
		if len(conds) > 0 {
			sort.Strings(conds)
			p.rule = TableRule{Mode: "where", Where: strings.Join(conds, " and ")}
		}
		switch p.rule.Mode {
		case "all":
			p.est = r.bytes
		case "where":
			where, err := expand(p.rule.Where, since, params)
			if err != nil {
				return nil, err
			}
			p.selectSQL += " where " + where
			if p.est, err = estimate(ctx, tx, p, where, r.tuples); err != nil {
				return nil, fmt.Errorf("filtro de %s (%s): %w", display, p.rule.Where, err)
			}
		}
		plans = append(plans, p)
	}
	// Tabela que não existe na origem não é copiada, então não vaza nada: só avisa (é o caso de
	// uma migration que ainda não chegou em prod). Coluna inexistente numa tabela que existe
	// continua sendo erro, porque aí a máscara não estaria valendo.
	for t := range masks {
		if _, ok := seenMask[t]; !ok {
			logf("aviso: mask de %q ignorada, a tabela não existe na origem", t)
		}
	}
	return plans, nil
}

func loadColumns(ctx context.Context, tx pgx.Tx, oid uint32, p *tablePlan) error {
	rows, err := tx.Query(ctx, `select attname from pg_attribute
		where attrelid = $1 and attnum > 0 and not attisdropped and attgenerated = '' order by attnum`, oid)
	if err != nil {
		return err
	}
	p.cols, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return err
}

// estimate usa a estimativa do planner (sem ler a tabela) para a fração filtrada.
func estimate(ctx context.Context, tx pgx.Tx, p *tablePlan, where string, tuples int64) (int64, error) {
	var plan []byte
	q := "explain (format json) select 1 from " + pgx.Identifier{p.schema, p.name}.Sanitize() + " where " + where
	if err := tx.QueryRow(ctx, q).Scan(&plan); err != nil {
		return 0, err
	}
	var out []struct {
		Plan struct {
			Rows float64 `json:"Plan Rows"`
		}
	}
	if err := json.Unmarshal(plan, &out); err != nil || len(out) == 0 {
		return p.bytes, nil
	}
	if tuples <= 0 { // nunca analisada: assume o pior caso
		return p.bytes, nil
	}
	frac := out[0].Plan.Rows / float64(tuples)
	if frac > 1 {
		frac = 1
	}
	return int64(frac * float64(p.bytes)), nil
}

func printPlan(plans []*tablePlan) int64 {
	sorted := append([]*tablePlan(nil), plans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].bytes > sorted[j].bytes })
	var total, origin int64
	counts := map[string]int{}
	w := newTableTo(os.Stderr, "TABELA", "REGRA", "NA ORIGEM", "VAI COPIAR")
	for i, p := range sorted {
		total += p.est
		origin += p.bytes
		counts[p.rule.Mode]++
		if i < 15 || p.rule.Mode == "where" {
			rule := p.rule.Mode
			if rule == "where" {
				rule = "where " + p.rule.Where
			}
			w.row(p.display, rule, humanSize(p.bytes), "~"+humanSize(p.est))
		}
	}
	w.flush()
	if len(sorted) > 15 {
		logf("(+%d tabelas menores)", len(sorted)-15)
	}
	logf("%d tabelas: %d inteiras, %d filtradas, %d só schema. Origem %s, estimativa ~%s (índices incluídos)",
		len(plans), counts["all"], counts["where"], counts["schema-only"], humanSize(origin), humanSize(total))
	return total
}

// ---------------------------------------------------------------------------
// Destino
// ---------------------------------------------------------------------------

func dumpSchema(src *source, owner, image, snapshot, section string) ([]byte, error) {
	args := append([]string{"run", "--rm"}, helperArgs(owner)...)
	args = append(args, src.dumpArgs[:len(src.dumpArgs)-4]...)
	args = append(args, image, "pg_dump", "--schema-only", "--section="+section,
		"--no-owner", "--no-privileges", "--snapshot="+snapshot)
	args = append(args, src.dumpArgs[len(src.dumpArgs)-4:]...)
	out, err := dockerIO(appCtx, nil, src.env, args...)
	if err != nil {
		return nil, fmt.Errorf("pg_dump %s: %w", section, err)
	}
	return out, nil
}

func psqlExec(container, user, db string, sql []byte) error {
	_, err := dockerIO(appCtx, strings.NewReader(string(sql)), nil,
		"exec", "-i", container, "psql", "-q", "-X", "-w", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db)
	return err
}

func imagePGData(image string) (string, error) {
	out, err := docker("image", "inspect", "-f", "{{json .Config.Env}}", image)
	if err != nil {
		if _, perr := docker("pull", "-q", image); perr != nil {
			return "", err
		}
		if out, err = docker("image", "inspect", "-f", "{{json .Config.Env}}", image); err != nil {
			return "", err
		}
	}
	var env []string
	json.Unmarshal([]byte(out), &env)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PGDATA="); ok {
			return v, nil
		}
	}
	return "/var/lib/postgresql/data", nil
}

// connectLocal conecta pelo TCP publicado: durante o initdb a imagem oficial sobe um servidor
// temporário só no socket unix, então conseguir conectar por TCP = servidor definitivo no ar.
func connectLocal(ctx context.Context, container, user, db string) (*pgx.Conn, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		port, err := hostPort(container)
		if err == nil {
			u := fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable",
				url.PathEscape(user), localPassword, port, url.PathEscape(db))
			if conn, err := pgx.Connect(ctx, u); err == nil {
				return conn, nil
			}
		}
		if ctx.Err() != nil {
			return nil, errInterrupted
		}
		if time.Now().After(deadline) {
			logs, _ := dockerIO(context.Background(), nil, nil, "logs", "--tail", "20", container)
			return nil, fmt.Errorf("postgres local não subiu:\n%s", logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// checkDisk compara a estimativa com o espaço livre onde o volume mora (na VM, no Docker Desktop).
func checkDisk(container, dir string, need int64) error {
	out, err := docker("exec", container, "df", "-Pk", dir)
	if err != nil {
		return nil // sem df na imagem: segue sem a checagem
	}
	lines := strings.Split(out, "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return nil
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return nil
	}
	if free := kb * 1024; need*12/10 > free {
		return fmt.Errorf("espaço livre %s, a cópia precisa de ~%s", humanSize(free), humanSize(need))
	}
	return nil
}

func copyTable(ctx context.Context, src, dst *pgx.Conn, p *tablePlan) error {
	cols := make([]string, len(p.cols))
	for i, c := range p.cols {
		cols[i] = pgx.Identifier{c}.Sanitize()
	}
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		_, err := src.PgConn().CopyTo(ctx, pw, "copy ("+p.selectSQL+") to stdout")
		pw.CloseWithError(err)
		errc <- err
	}()
	tag, err := dst.PgConn().CopyFrom(ctx, pr,
		"copy "+pgx.Identifier{p.schema, p.name}.Sanitize()+" ("+strings.Join(cols, ", ")+") from stdin")
	pr.CloseWithError(io.ErrClosedPipe) // destrava a origem se o destino falhou no meio
	if srcErr := <-errc; srcErr != nil && err == nil {
		err = srcErr
	}
	if err != nil {
		return err
	}
	p.rows = tag.RowsAffected()
	return nil
}

func copySequences(ctx context.Context, tx pgx.Tx, dst *pgx.Conn) error {
	rows, err := tx.Query(ctx, `select format('%I.%I', schemaname, sequencename), last_value
		from pg_sequences where last_value is not null`)
	if err != nil {
		return err
	}
	type seq struct {
		name string
		val  int64
	}
	seqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (seq, error) {
		var s seq
		return s, r.Scan(&s.name, &s.val)
	})
	if err != nil {
		return err
	}
	for _, s := range seqs {
		if _, err := dst.Exec(ctx, "select setval($1::regclass, $2)", s.name, s.val); err != nil {
			return fmt.Errorf("sequence %s: %w", s.name, err)
		}
	}
	return nil
}

type foreignKey struct {
	name                  string
	child, parent         string // identificadores com aspas (pgx.Identifier.Sanitize)
	childCols, parentCols []string
}

func foreignKeys(ctx context.Context, tx pgx.Tx) ([]foreignKey, error) {
	rows, err := tx.Query(ctx, `
		select con.conname, cn.nspname, c.relname, fn.nspname, f.relname,
		  array(select quote_ident(a.attname) from unnest(con.conkey) with ordinality k(n, o)
		        join pg_attribute a on a.attrelid = con.conrelid and a.attnum = k.n order by k.o),
		  array(select quote_ident(a.attname) from unnest(con.confkey) with ordinality k(n, o)
		        join pg_attribute a on a.attrelid = con.confrelid and a.attnum = k.n order by k.o)
		from pg_constraint con
		join pg_class c on c.oid = con.conrelid join pg_namespace cn on cn.oid = c.relnamespace
		join pg_class f on f.oid = con.confrelid join pg_namespace fn on fn.oid = f.relnamespace
		where con.contype = 'f'`)
	if err != nil {
		return nil, err
	}
	// Os identificadores são montados aqui, do mesmo jeito que em cleanOrphans (Sanitize),
	// para as chaves baterem.
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (foreignKey, error) {
		var fk foreignKey
		var cs, cn, ps, pn string
		err := r.Scan(&fk.name, &cs, &cn, &ps, &pn, &fk.childCols, &fk.parentCols)
		fk.child, fk.parent = pgx.Identifier{cs, cn}.Sanitize(), pgx.Identifier{ps, pn}.Sanitize()
		return fk, err
	})
}

// cleanOrphans apaga as linhas que apontam para pais que não vieram (tabela filtrada ou só
// schema), em cascata, antes de criar as FKs. Com --strict, falha em vez de apagar.
func cleanOrphans(ctx context.Context, dst *pgx.Conn, plans []*tablePlan, fks []foreignKey, strict bool) error {
	partial := map[string]bool{}
	for _, p := range plans {
		if p.rule.Mode != "all" {
			partial[pgx.Identifier{p.schema, p.name}.Sanitize()] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, fk := range fks {
			if !partial[fk.parent] {
				continue
			}
			var notNull, match []string
			for i := range fk.childCols {
				notNull = append(notNull, "c."+fk.childCols[i]+" is not null")
				match = append(match, "p."+fk.parentCols[i]+" = c."+fk.childCols[i])
			}
			cond := strings.Join(notNull, " and ") + " and not exists (select 1 from " + fk.parent +
				" p where " + strings.Join(match, " and ") + ")"
			if strict {
				var n int64
				if err := dst.QueryRow(ctx, "select count(*) from "+fk.child+" c where "+cond).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					return fmt.Errorf("--strict: %d linhas de %s apontam para %s que não veio (FK %s)", n, fk.child, fk.parent, fk.name)
				}
				continue
			}
			tag, err := dst.Exec(ctx, "delete from "+fk.child+" c where "+cond)
			if err != nil {
				return fmt.Errorf("limpando órfãs de %s: %w", fk.name, err)
			}
			if n := tag.RowsAffected(); n > 0 {
				logf("  %d linhas órfãs removidas de %s (FK %s -> %s)", n, fk.child, fk.name, fk.parent)
				if !partial[fk.child] {
					partial[fk.child] = true
				}
				changed = true
			}
		}
	}
	return nil
}
