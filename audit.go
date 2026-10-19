package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

// Auditoria de PII numa cópia: procura dado pessoal que passou sem máscara (o caso típico é uma
// migration que cria uma coluna nova e ninguém atualiza o pgb.yaml). Falha fechado: achou, é erro.
// Roda sozinha no fim do `base create` lógico (antes do post-data) e sob demanda com `pgb audit`.

// AuditConfig é a seção audit do pgb.yaml. Ignore são globs "tabela.coluna" (path.Match);
// fora do public, "schema.tabela.coluna". Serve para dado de negócio que parece pessoal (ex.:
// telefone de SAC da loja na config do tenant).
type AuditConfig struct {
	Ignore []string `yaml:"ignore"`
}

// Padrões em sintaxe ARE do Postgres. Só grupos não capturantes (?:...): o substring() devolve o
// primeiro grupo capturado, e o que queremos de exemplo é o trecho inteiro.
var piiPatterns = []struct{ name, re string }{
	// Identidade real do WhatsApp é só dígitos; o preset chat gera 32 hex @domínio e não casa.
	{"whatsapp", `\m\d{10,15}@wa\.gw\.msging\.net`},
	// +55 com DDD, (DDD) com hífen, ou 55+DDD+9+8 dígitos colado. Número solto de 11 dígitos e
	// timestamp em ms ficam de fora, senão o falso positivo afoga o resto.
	{"telefone", `(?:\+55\s?\(?\d{2}\)?\s?9?\d{4}[-\s]?\d{4}|\(\d{2}\)\s?9?\d{4}-\d{4}|\m55\d{2}9\d{8}\M)`},
	// Menos o preset email (@exemplo.invalid) e as identidades de WhatsApp de provedor (*.msging.net).
	{"email", `[[:alnum:]._%+-]+@(?!exemplo\.invalid\M)(?!(?:[[:alnum:]-]+\.)*msging\.net\M)[[:alnum:]-]+(?:\.[[:alnum:]-]+)*\.[[:alpha:]]{2,}`},
	{"cpf", `\m\d{3}\.\d{3}\.\d{3}-\d{2}\M`},
}

type finding struct {
	column  string // tabela.coluna (schema.tabela.coluna fora do public)
	pattern string
	rows    int64
	sample  string // sem redação: só sai impresso por redact()
}

// auditDB varre toda coluna de texto (categoria S, json, jsonb e arrays deles) das tabelas do
// usuário, com uma query por tabela. Com skipMasked, colunas com mask no pgb.yaml ficam de fora:
// o preset phone gera '+55119...' de propósito, então checar o padrão nelas só daria falso positivo.
func auditDB(ctx context.Context, conn *pgx.Conn, cfg *Config, skipMasked bool) ([]finding, error) {
	for _, pat := range cfg.Audit.Ignore {
		if _, err := path.Match(pat, ""); err != nil {
			return nil, fmt.Errorf("audit.ignore: padrão inválido %q", pat)
		}
	}
	// Mesmo recorte do planTables: sem schemas do sistema, tabelas de extensão e partições (a
	// tabela-mãe já lê as partições).
	rows, err := conn.Query(ctx, `
		select n.nspname, c.relname, a.attname
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		join pg_attribute a on a.attrelid = c.oid and a.attnum > 0 and not a.attisdropped
		join pg_type t on t.oid = a.atttypid
		left join pg_type e on e.oid = t.typelem and t.typcategory = 'A'
		where c.relkind in ('r', 'p') and not c.relispartition
		  and n.nspname not in ('pg_catalog', 'information_schema') and n.nspname not like 'pg\_%'
		  and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass
		                  and d.objid = c.oid and d.deptype = 'e')
		  and (t.typcategory = 'S' or e.typcategory = 'S'
		       or 'json'::regtype in (t.oid, t.typbasetype, e.oid) or 'jsonb'::regtype in (t.oid, t.typbasetype, e.oid))
		order by 1, 2, a.attnum`)
	if err != nil {
		return nil, err
	}
	type col struct{ schema, table, name string }
	cols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (col, error) {
		var c col
		return c, r.Scan(&c.schema, &c.table, &c.name)
	})
	if err != nil {
		return nil, err
	}

	// Agrupa por tabela, já tirando as mascaradas e as ignoradas.
	type tbl struct {
		schema, name, display string
		cols                  []string
	}
	var tables []*tbl
	usedIgnore := map[string]bool{}
	scanned, masked, ignored := 0, 0, 0
	for _, c := range cols {
		display := c.table
		if c.schema != "public" {
			display = c.schema + "." + c.table
		}
		pat := matchIgnore(cfg.Audit.Ignore, display+"."+c.name)
		usedIgnore[pat] = true
		if _, ok := cfg.Mask[display][c.name]; ok && skipMasked {
			masked++
			continue
		}
		if pat != "" {
			ignored++
			continue
		}
		if len(tables) == 0 || tables[len(tables)-1].display != display {
			tables = append(tables, &tbl{schema: c.schema, name: c.table, display: display})
		}
		t := tables[len(tables)-1]
		t.cols = append(t.cols, c.name)
		scanned++
	}
	for _, pat := range cfg.Audit.Ignore {
		if !usedIgnore[pat] {
			logf("aviso: audit.ignore %q não casou com nenhuma coluna de texto", pat)
		}
	}

	// Os padrões vão como parâmetros ($1..$n), iguais para todas as colunas.
	params := make([]any, len(piiPatterns))
	for i, p := range piiPatterns {
		params[i] = p.re
	}
	start := time.Now()
	var out []finding
	for _, t := range tables {
		var exprs []string
		for _, c := range t.cols {
			q := pgx.Identifier{c}.Sanitize() + "::text"
			for i := range piiPatterns {
				exprs = append(exprs, fmt.Sprintf("count(*) filter (where %s ~ $%d::text), min(substring(%s from $%d::text))", q, i+1, q, i+1))
			}
		}
		counts := make([]int64, len(t.cols)*len(piiPatterns))
		samples := make([]*string, len(counts))
		dest := make([]any, 0, 2*len(counts))
		for i := range counts {
			dest = append(dest, &counts[i], &samples[i])
		}
		q := "select " + strings.Join(exprs, ", ") + " from " + pgx.Identifier{t.schema, t.name}.Sanitize()
		if err := conn.QueryRow(ctx, q, params...).Scan(dest...); err != nil {
			return nil, fmt.Errorf("audit de %s: %w", t.display, err)
		}
		for i, n := range counts {
			if n == 0 {
				continue
			}
			f := finding{column: t.display + "." + t.cols[i/len(piiPatterns)], pattern: piiPatterns[i%len(piiPatterns)].name, rows: n}
			if samples[i] != nil {
				f.sample = *samples[i]
			}
			out = append(out, f)
		}
	}
	how := fmt.Sprintf("%d colunas de texto em %d tabelas", scanned, len(tables))
	if skipMasked {
		how += fmt.Sprintf(", %d mascaradas", masked)
	}
	logf("audit: %s (%d ignoradas) em %s", how, ignored, time.Since(start).Round(time.Millisecond))
	if len(out) == 0 {
		logf("audit: nenhuma PII encontrada em %d colunas", scanned)
	}
	return out, nil
}

// matchIgnore devolve o padrão de audit.ignore que casa com tabela.coluna, ou "".
func matchIgnore(patterns []string, column string) string {
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, column); ok {
			return pat
		}
	}
	return ""
}

// redact mostra só os 2 primeiros e os 4 últimos caracteres, mais o tamanho: dá para achar o
// registro sem reimprimir o dado pessoal.
func redact(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) < 10 {
		return fmt.Sprintf("… (%d)", len(r))
	}
	return fmt.Sprintf("%s…%s (%d)", string(r[:2]), string(r[len(r)-4:]), len(r))
}

func printFindings(w io.Writer, fs []finding) {
	t := newTableTo(w, "COLUNA", "PADRÃO", "LINHAS", "EXEMPLO")
	for _, f := range fs {
		t.row(f.column, f.pattern, fmt.Sprint(f.rows), redact(f.sample))
	}
	t.flush()
}

const auditHint = "mascare no pgb.yaml (mask:) ou, se for dado de negócio e não pessoal, ponha em audit.ignore"

// auditCopy é a auditoria automática do `base create`: com PII, devolve erro (e a base é desfeita).
func auditCopy(ctx context.Context, conn *pgx.Conn, cfg *Config) error {
	fs, err := auditDB(ctx, conn, cfg, true)
	if err != nil || len(fs) == 0 {
		return err
	}
	var b strings.Builder
	printFindings(&b, fs)
	return fmt.Errorf("audit: PII sem máscara em %d coluna(s):\n%s%s", countColumns(fs), b.String(), auditHint)
}

func countColumns(fs []finding) int {
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.column] = true
	}
	return len(seen)
}

func auditCmd() *cobra.Command {
	var configFile string
	cmd := &cobra.Command{
		Use:   "audit <branch> [-c pgb.yaml]",
		Short: "Procura PII sem máscara numa branch (falha se achar)",
		Long: `Varre as colunas de texto, json e arrays de texto atrás de WhatsApp, telefone, e-mail e CPF.
Numa base lógica, as colunas com mask no pgb.yaml são puladas; numa base física (--from-container)
a config não foi aplicada, então tudo é varrido. audit.ignore do pgb.yaml vale nos dois casos.`,
		Args: nameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(configFile, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			return auditBranch(args[0], cfg)
		},
	}
	cmd.Flags().StringVarP(&configFile, "config", "c", "pgb.yaml", "arquivo de configuração")
	return cmd
}

func auditBranch(name string, cfg *Config) error {
	ctx := appCtx
	info, err := inspectContainer(branchContainer(name))
	if err != nil {
		return fmt.Errorf("branch %q não existe", name)
	}
	// Na dúvida varre tudo: só pula as mascaradas se a base for uma cópia lógica, que aplicou o mask.
	skipMasked := false
	if base, err := loadBase(info.Config.Labels[lBase]); err != nil {
		logf("aviso: %v; varrendo todas as colunas", err)
	} else if src := base[lSource]; strings.HasPrefix(src, "container:") {
		logf("base %q é cópia física (%s): o mask do pgb.yaml não foi aplicado, varrendo todas as colunas", base[lName], src)
	} else {
		skipMasked = true
	}
	u, err := branchURL(name)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	fs, err := auditDB(ctx, conn, cfg, skipMasked)
	if err != nil || len(fs) == 0 {
		return err
	}
	printFindings(os.Stdout, fs)
	return fmt.Errorf("audit: PII em %d coluna(s) da branch %q: %s", countColumns(fs), name, auditHint)
}
