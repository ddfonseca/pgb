package main

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

// `pgb branch changes`: tudo o que foi escrito na branch (INSERT/UPDATE/DELETE), por transação.
// Cada branch nasce com wal_level=logical e um slot de replicação lógica com o test_decoding
// (vem com o Postgres). O slot guarda o WAL desde a criação; o peek lê sem consumir.

const changesSlot = "pgb_changes"

func changesCmd() *cobra.Command {
	var o changesOpts
	cmd := &cobra.Command{
		Use:   "changes <nome>",
		Short: "Mostra os INSERT/UPDATE/DELETE gravados na branch, por transação",
		Long: `Lê o slot de replicação lógica que toda branch ganha ao nascer (test_decoding).
Sem --consume não consome nada: mostra tudo desde a criação. Com --consume, a próxima chamada
mostra só o que veio depois (e libera o WAL acumulado). Só DML: DDL não aparece.`,
		Args: nameArg,
		RunE: func(_ *cobra.Command, args []string) error { return branchChanges(args[0], o) },
	}
	f := cmd.Flags()
	f.BoolVar(&o.consume, "consume", false, "consome: a próxima chamada mostra só as mais novas (de todas as tabelas, mesmo com --table)")
	f.BoolVar(&o.summary, "summary", false, "só a contagem por tabela e operação")
	f.StringVar(&o.table, "table", "", "só as tabelas que casam com o glob (ex.: 'order*'; fora do public: schema.tabela)")
	f.BoolVar(&o.full, "full", false, "não trunca os valores (padrão: ~80 caracteres)")
	return cmd
}

// changesConf vai no fim do cloneScript (no mesmo container auxiliar, antes do primeiro start).
// O PGDATA é a raiz do volume, então o arquivo é /dst/postgresql.auto.conf. O teto de WAL
// protege o disco: sem ele um slot que ninguém consome seguraria WAL para sempre; passou
// disso, o Postgres invalida o slot (a branch continua funcionando).
const changesConf = ` && printf "wal_level = 'logical'\nmax_slot_wal_keep_size = '2GB'\n" >> /dst/postgresql.auto.conf &&
chown "$(stat -c %u:%g /dst/PG_VERSION)" /dst/postgresql.auto.conf`

// createChangesSlot roda depois do waitReady. Se falhar, só avisa: a branch funciona sem.
// Conecta direto pela porta dos labels: um `docker exec psql` custaria ~150 ms a mais na criação.
func createChangesSlot(labels map[string]string) {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(labels[lUser], labels[lPassword]),
		Host:     "127.0.0.1:" + labels[lPort],
		Path:     "/" + labels[lDB],
		RawQuery: "sslmode=disable",
	}
	err := func() error {
		conn, err := pgx.Connect(appCtx, u.String())
		if err != nil {
			return err
		}
		defer conn.Close(appCtx)
		_, err = conn.Exec(appCtx, "select pg_create_logical_replication_slot($1, 'test_decoding')", changesSlot)
		return err
	}()
	if err != nil {
		logf("aviso: branch sem o slot de mudanças (pgb branch changes não vai funcionar): %v", err)
	}
}

type changesOpts struct {
	consume, summary, full bool
	table                  string
}

// change é uma linha do test_decoding já interpretada.
type change struct {
	table, op string
	cols      []colVal // tupla nova (INSERT/UPDATE) ou chave (DELETE)
	oldKey    []colVal // UPDATE que mudou a chave (ou REPLICA IDENTITY FULL)
	raw       string   // o que não deu para interpretar (TRUNCATE, message...), como veio
}

type colVal struct {
	name, val string
	quoted    bool // literal entre aspas no test_decoding (texto, jsonb, timestamp...)
}

func branchChanges(name string, o changesOpts) error {
	u, err := branchURL(name)
	if err != nil {
		return err
	}
	ctx := appCtx
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		return fmt.Errorf("conectando na branch %q: %w", name, err)
	}
	defer conn.Close(ctx)

	var walStatus string
	err = conn.QueryRow(ctx, "select coalesce(wal_status, '') from pg_replication_slots where slot_name = $1",
		changesSlot).Scan(&walStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("branch %q não tem o slot de mudanças (foi criada antes do `pgb branch changes`): "+
			"recrie a branch, ou pgb branch reset %s (descarta os dados dela e recria já com o slot)", name, name)
	}
	if err != nil {
		return err
	}
	if walStatus == "lost" {
		return fmt.Errorf("o slot de mudanças da branch %q foi invalidado: acumulou mais de 2GB de WAL sem "+
			"ninguém consumir (max_slot_wal_keep_size), e o Postgres descartou o histórico para proteger o disco. "+
			"Para voltar a registrar a partir de agora: psql \"$(pgb branch url %s)\" -c \"select pg_drop_replication_slot('%s'); "+
			"select pg_create_logical_replication_slot('%s', 'test_decoding')\" (ou pgb branch reset %s). "+
			"Com --consume de tempos em tempos o WAL não acumula", name, name, changesSlot, changesSlot, name)
	}

	fn := "pg_logical_slot_peek_changes"
	if o.consume {
		fn = "pg_logical_slot_get_changes"
	}
	rows, err := conn.Query(ctx, "select data from "+fn+"($1, null, null, 'include-timestamp', '1', 'skip-empty-xacts', '1')",
		changesSlot)
	if err != nil {
		return err
	}
	defer rows.Close()

	counts := map[string]map[string]int{}
	var xid, ts string
	var pending []change
	nx := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(line, "BEGIN "):
			xid, ts, pending = strings.TrimPrefix(line, "BEGIN "), "", nil
		case strings.HasPrefix(line, "COMMIT "):
			ts = commitTime(line)
			if len(pending) == 0 {
				continue
			}
			nx++
			if o.summary {
				for _, c := range pending {
					if c.table == "" {
						continue // message de pg_logical_emit_message: não é DML
					}
					if counts[c.table] == nil {
						counts[c.table] = map[string]int{}
					}
					counts[c.table][c.op]++
				}
				continue
			}
			fmt.Printf("── xid %s  %s  (%d)\n", xid, ts, len(pending))
			for _, c := range pending {
				fmt.Println("  " + c.format(o.full))
			}
		default:
			c := parseChange(line)
			if o.table != "" {
				if ok, _ := path.Match(o.table, c.table); !ok {
					continue
				}
			}
			pending = append(pending, c)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if o.summary {
		printChangesSummary(counts)
	}
	what := "desde a criação da branch (ou do último --consume)"
	if o.consume {
		what = "consumidas: o próximo `changes` mostra só as mais novas"
	}
	plural := "transações"
	if nx == 1 {
		plural = "transação"
	}
	logf("%d %s, %s", nx, plural, what)
	return nil
}

func printChangesSummary(counts map[string]map[string]int) {
	ops := []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"}
	var tables []string
	for t := range counts {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	t := newTable(append([]string{"TABELA"}, ops...)...)
	for _, name := range tables {
		row := []string{name}
		for _, op := range ops {
			row = append(row, fmt.Sprint(counts[name][op]))
		}
		t.row(row...)
	}
	t.flush()
}

// commitTime tira o horário de "COMMIT 123 (at 2026-10-19 12:00:00.123456+00)" e passa para
// o fuso local (o mesmo dos logs do app).
func commitTime(line string) string {
	_, at, ok := strings.Cut(line, "(at ")
	if !ok {
		return ""
	}
	at = strings.TrimSuffix(at, ")")
	for _, layout := range []string{"2006-01-02 15:04:05.999999-07", "2006-01-02 15:04:05.999999-07:00"} {
		if t, err := time.Parse(layout, at); err == nil {
			return t.Local().Format("2006-01-02 15:04:05.000")
		}
	}
	return at
}

func (c change) format(full bool) string {
	if c.raw != "" {
		return c.raw
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-6s %s ", c.op, c.table)
	if len(c.oldKey) > 0 {
		b.WriteString(" [antes: " + formatCols(c.oldKey, full) + "]")
	}
	if len(c.cols) > 0 {
		b.WriteString(" " + formatCols(c.cols, full))
	}
	return strings.TrimRight(b.String(), " ")
}

const maxValue = 80

func formatCols(cols []colVal, full bool) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		v := c.val
		if !full {
			v = strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(v)
			if r := []rune(v); len(r) > maxValue {
				v = string(r[:maxValue]) + "…"
			}
		}
		if c.quoted {
			v = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		parts[i] = c.name + "=" + v
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------------------
// Tokenizer do test_decoding
// ---------------------------------------------------------------------------
//
// Formato (contrib/test_decoding):
//   table public.t: INSERT: id[integer]:1 nome[text]:'O''Neil: a [b]' tags[text[]]:'{a,b}'
//   table public.t: UPDATE: old-key: id[integer]:1 new-tuple: id[integer]:2 ...
//   table public.t: DELETE: id[integer]:2          (ou "(no-tuple-data)" sem replica identity)
//   table public.t: TRUNCATE: (no-flags)
// Nomes de tabela e coluna vêm com quote_identifier (aspas duplas se precisar, "" escapa);
// valores de texto entre aspas simples com '' escapando; números/bool/null/unchanged-toast-datum
// sem aspas. Nome de tipo pode ter espaço e colchete (timestamp with time zone, text[]).

type scanner struct {
	s string
	i int
}

func (sc *scanner) eof() bool { return sc.i >= len(sc.s) }

func (sc *scanner) peek(p string) bool { return strings.HasPrefix(sc.s[sc.i:], p) }

func (sc *scanner) eat(p string) bool {
	if sc.peek(p) {
		sc.i += len(p)
		return true
	}
	return false
}

func (sc *scanner) skipSpaces() {
	for !sc.eof() && sc.s[sc.i] == ' ' {
		sc.i++
	}
}

// quoted lê um literal entre q (aspas simples ou duplas), com qq como escape. Supõe s[i] == q.
func (sc *scanner) quoted(q byte) (string, bool) {
	var b strings.Builder
	for sc.i++; sc.i < len(sc.s); sc.i++ {
		if sc.s[sc.i] == q {
			if sc.i+1 < len(sc.s) && sc.s[sc.i+1] == q {
				b.WriteByte(q)
				sc.i++
				continue
			}
			sc.i++
			return b.String(), true
		}
		b.WriteByte(sc.s[sc.i])
	}
	return b.String(), false
}

// ident lê um identificador (com ou sem aspas duplas) até um dos stop.
func (sc *scanner) ident(stop string) (string, bool) {
	if !sc.eof() && sc.s[sc.i] == '"' {
		return sc.quoted('"')
	}
	start := sc.i
	for !sc.eof() && !strings.ContainsRune(stop, rune(sc.s[sc.i])) {
		sc.i++
	}
	return sc.s[start:sc.i], sc.i > start
}

// qualified lê schema.tabela e devolve sem o "public.".
func (sc *scanner) qualified() (string, bool) {
	schema, ok := sc.ident(".:")
	if !ok || !sc.eat(".") {
		return "", false
	}
	rel, ok := sc.ident(":")
	if !ok {
		return "", false
	}
	if schema == "public" {
		return rel, true
	}
	return schema + "." + rel, true
}

// typeName pula o [tipo]: termina no ']' seguido de ':' que não está entre aspas duplas.
func (sc *scanner) typeName() bool {
	if !sc.eat("[") {
		return false
	}
	for !sc.eof() {
		switch {
		case sc.s[sc.i] == '"':
			sc.quoted('"')
		case sc.eat("]:"):
			return true
		default:
			sc.i++
		}
	}
	return false
}

func (sc *scanner) value() (colVal, bool) {
	if sc.eof() {
		return colVal{}, false
	}
	if sc.s[sc.i] == '\'' {
		v, ok := sc.quoted('\'')
		return colVal{val: v, quoted: true}, ok
	}
	if sc.peek("B'") { // bit/varbit: B'0101'
		sc.i++
		v, ok := sc.quoted('\'')
		return colVal{val: "B'" + v + "'"}, ok
	}
	start := sc.i
	for !sc.eof() && sc.s[sc.i] != ' ' {
		sc.i++
	}
	return colVal{val: sc.s[start:sc.i]}, true
}

// tuple lê "col[tipo]:valor ..." até o fim ou até a palavra-chave stop (ex.: "new-tuple:").
func (sc *scanner) tuple(stop string) ([]colVal, bool) {
	var cols []colVal
	for {
		sc.skipSpaces()
		if sc.eof() || (stop != "" && sc.peek(stop)) {
			return cols, true
		}
		if sc.eat("(no-tuple-data)") {
			continue
		}
		start := sc.i
		if _, ok := sc.ident("["); !ok {
			return cols, false
		}
		name := sc.s[start:sc.i] // como veio: "Col Estranha" continua com aspas
		if !sc.typeName() {
			return cols, false
		}
		v, ok := sc.value()
		if !ok {
			return cols, false
		}
		v.name = name
		cols = append(cols, v)
	}
}

func parseChange(line string) change {
	bad := change{raw: line}
	sc := &scanner{s: line}
	if !sc.eat("table ") {
		return bad // ex.: "message: transactional: ..." de pg_logical_emit_message
	}
	table, ok := sc.qualified()
	if !ok || !sc.eat(": ") {
		return bad
	}
	c := change{table: table}
	if c.op, ok = sc.ident(":"); !ok || !sc.eat(":") {
		return bad
	}
	switch c.op {
	case "INSERT", "UPDATE", "DELETE":
	default: // TRUNCATE: o resto são flags (restart_seqs, cascade, no-flags)
		c.raw = fmt.Sprintf("%-6s %s %s", c.op, table, strings.TrimSpace(sc.s[sc.i:]))
		return c
	}
	sc.skipSpaces()
	if sc.eat("old-key:") {
		if c.oldKey, ok = sc.tuple("new-tuple:"); !ok || !sc.eat("new-tuple:") {
			return bad
		}
	}
	if c.cols, ok = sc.tuple(""); !ok {
		return bad
	}
	return c
}
