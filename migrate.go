package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

// Ensaio de migration numa branch descartável, antes do deploy. Responde três perguntas:
// a migration pega locks que travam as escritas do app? quanto tempo leva? o que mudou no schema?
//
//  1. branch mig-<hex> da base (TTL 1h de rede de segurança), apagada no fim em qualquer saída;
//  2. pg_dump -s antes, e as tabelas que já existiam (as que a migration cria não importam);
//  3. uma conexão pgb-monitor olha o pg_locks a cada 100 ms enquanto o comando roda;
//  4. pg_dump -s depois, diff -u, e o relatório. Exit 2 se algum lock passou do limite (CI).

const pollEvery = 100 * time.Millisecond

// Modos que bloqueiam INSERT/UPDATE/DELETE (RowExclusiveLock). ShareUpdateExclusive (CREATE
// INDEX CONCURRENTLY, VACUUM, VALIDATE CONSTRAINT) não bloqueia, então não entra.
var writeBlocking = []string{"ShareLock", "ShareRowExclusiveLock", "ExclusiveLock", "AccessExclusiveLock"}

type migrateOpts struct {
	from      string
	threshold time.Duration
	keep      bool
	noDiff    bool
}

func migrateCmd() *cobra.Command {
	var o migrateOpts
	cmd := &cobra.Command{
		Use:   "migrate-test [--from <base>] [--lock-threshold 1s] [--keep] [--no-diff] -- <comando...>",
		Short: "Ensaia uma migration numa branch descartável: locks, duração e diff do schema",
		Long: `Cria uma branch mig-<hex> da base, roda o comando com DATABASE_URL apontando para ela e
mostra os locks que bloqueiam escrita em tabelas que já existiam, quanto duraram e o diff do
schema (no stdout; a saída do comando vai para o stderr). Exit: o do comando se falhar; 2 se
algum lock passou de --lock-threshold; senão 0.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 || len(args) < 1 {
				return fmt.Errorf("uso: pgb migrate-test [--from <base>] -- <comando...>")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if o.threshold < 0 {
				return fmt.Errorf("--lock-threshold não pode ser negativo")
			}
			code, err := migrateTest(o, args)
			if err != nil {
				fmt.Fprintln(os.Stderr, "erro:", err)
				if appCtx.Err() != nil {
					code = 130 // Ctrl+C antes do comando rodar, como no main
				}
			}
			os.Exit(code)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.from, "from", "", "base de origem (opcional se só houver uma)")
	f.DurationVar(&o.threshold, "lock-threshold", time.Second, "lock de escrita mais longo que isso dá exit 2")
	f.BoolVar(&o.keep, "keep", false, "mantém a branch no fim (TTL 1h)")
	f.BoolVar(&o.noDiff, "no-diff", false, "não imprime o diff do schema")
	return cmd
}

func migrateTest(o migrateOpts, argv []string) (int, error) {
	base, err := loadBase(o.from)
	if err != nil {
		return 1, err
	}
	b := make([]byte, 3)
	rand.Read(b)
	name := "mig-" + hex.EncodeToString(b)

	// Como no branch exec: a URL do create vai para o stderr.
	stdout := os.Stdout
	os.Stdout = os.Stderr
	err = createBranch(name, base, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "", "")
	os.Stdout = stdout
	if err != nil {
		return 1, err
	}
	// branchRm usa cleanup(): roda mesmo depois do Ctrl+C.
	if o.keep {
		defer logf("branch %q mantida (expira em 1h): pgb branch url %s / pgb branch rm %s", name, name, name)
	} else {
		defer branchRm(name)
	}
	u, err := branchURL(name)
	if err != nil {
		return 1, err
	}
	c := branchContainer(name)

	dir, err := os.MkdirTemp("", "pgb-migrate-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(dir)
	before, after := filepath.Join(dir, "antes.sql"), filepath.Join(dir, "depois.sql")
	if err := dumpSchemaTo(c, base[lUser], base[lDB], before); err != nil {
		return 1, err
	}

	mon, err := startMonitor(appCtx, u)
	if err != nil {
		return 1, err
	}
	logf("rodando %s na branch %q...", strings.Join(argv, " "), name)
	start := time.Now()
	code, runErr := runWithURL(name, u, argv, os.Stderr)
	took := time.Since(start)
	locks := mon.finish()
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "erro:", runErr)
	}

	// Interrompido: o diff não sai (o appCtx já foi cancelado), mas o que o monitor viu sim.
	var dumpErr error
	if appCtx.Err() == nil {
		if dumpErr = dumpSchemaTo(c, base[lUser], base[lDB], after); dumpErr == nil && !o.noDiff {
			printDiff(before, after)
		}
	}

	slow := report(code, took, locks, o.threshold)
	switch {
	case appCtx.Err() != nil && code == 0:
		return 130, nil
	case code != 0:
		return code, nil
	case dumpErr != nil:
		return 1, dumpErr // o ensaio não terminou: não dá para dizer que passou
	case slow:
		logf("lock de escrita acima de %s em tabela que já existia: em prod o app fica travado esse tempo, ou mais.", o.threshold)
		logf("considere CREATE INDEX CONCURRENTLY, ADD CONSTRAINT ... NOT VALID + VALIDATE CONSTRAINT em outro passo,")
		logf("lock_timeout curto com retry, ou quebrar a migration em passos menores (uma transação longa segura tudo até o commit).")
		return 2, nil
	}
	return 0, nil
}

// dumpSchemaTo grava o schema da branch num arquivo, sem as linhas \restrict/\unrestrict que o
// pg_dump novo (16.10+) gera com uma chave aleatória a cada execução e que sujariam o diff.
func dumpSchemaTo(container, user, db, file string) error {
	out, err := dockerIO(appCtx, nil, nil, "exec", container, "pg_dump", "-s", "--no-owner", "--no-privileges", "-U", user, "-d", db)
	if err != nil {
		return fmt.Errorf("pg_dump do schema: %w", err)
	}
	lines := strings.SplitAfter(string(out), "\n")
	var b strings.Builder
	for _, l := range lines {
		if !strings.HasPrefix(l, `\restrict `) && !strings.HasPrefix(l, `\unrestrict `) {
			b.WriteString(l)
		}
	}
	return os.WriteFile(file, []byte(b.String()), 0o600)
}

// printDiff usa o diff externo (macOS, Linux e WSL têm) e imprime no stdout, para dar
// `> schema.diff` no CI.
func printDiff(before, after string) {
	cmd := exec.Command("diff", "-u", "-L", "antes", "-L", "depois", before, after)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		logf("schema não mudou")
	case errors.Is(err, exec.ErrNotFound):
		logf("diff não está instalado: sem o diff do schema")
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		// 1 = arquivos diferentes, já impresso
	default:
		logf("diff falhou: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Monitor de locks
// ---------------------------------------------------------------------------

type relInfo struct {
	display string
	tuples  float64 // reltuples local; -1 = nunca analisada
}

type lockKey struct {
	pid  int32
	rel  uint32
	mode string
}

type lockSeen struct {
	lockKey
	table       relInfo
	first, last time.Time
	query       string
}

func (l *lockSeen) duration() time.Duration { return l.last.Sub(l.first) }

type lockMonitor struct {
	conn   *pgx.Conn
	tables map[uint32]relInfo
	seen   map[lockKey]*lockSeen
	cancel context.CancelFunc
	done   chan struct{}
}

// startMonitor conecta como pgb-monitor, guarda as tabelas que existem agora e começa a olhar
// o pg_locks. Só lê catálogo e views de estatística: não pega lock que atrapalhe a migration.
func startMonitor(ctx context.Context, u string) (*lockMonitor, error) {
	pc, err := pgx.ParseConfig(u)
	if err != nil {
		return nil, err
	}
	pc.RuntimeParams["application_name"] = "pgb-monitor"
	conn, err := pgx.ConnectConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("conectando o monitor: %w", err)
	}
	m := &lockMonitor{conn: conn, tables: map[uint32]relInfo{}, seen: map[lockKey]*lockSeen{},
		done: make(chan struct{})}
	rows, err := conn.Query(ctx, `
		select c.oid, n.nspname, c.relname, c.reltuples::float8
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where c.relkind in ('r', 'p', 'm')
		  and n.nspname not in ('pg_catalog', 'information_schema') and n.nspname not like 'pg\_%'`)
	if err != nil {
		conn.Close(context.Background())
		return nil, err
	}
	for rows.Next() {
		var oid uint32
		var schema, rel string
		var r relInfo
		if err := rows.Scan(&oid, &schema, &rel, &r.tuples); err != nil {
			rows.Close()
			conn.Close(context.Background())
			return nil, err
		}
		r.display = rel
		if schema != "public" {
			r.display = schema + "." + rel
		}
		m.tables[oid] = r
	}
	if err := rows.Err(); err != nil {
		conn.Close(context.Background())
		return nil, err
	}
	// O polling não usa o appCtx: depois de um Ctrl+C o comando ainda pode segurar o lock por
	// até 10 s (repasse do sinal + kill), e isso também conta. Quem para o monitor é o finish.
	pctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.run(pctx)
	return m, nil
}

func (m *lockMonitor) run(ctx context.Context) {
	defer close(m.done)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		if err := m.poll(ctx); err != nil {
			if ctx.Err() == nil {
				logf("aviso: monitor de locks parou: %v", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// poll: OIDs só valem dentro de um banco, então filtra pelo banco atual. O pid do próprio
// monitor fica de fora.
func (m *lockMonitor) poll(ctx context.Context) error {
	rows, err := m.conn.Query(ctx, `
		select l.pid, l.relation, l.mode, coalesce(left(a.query, 200), '')
		from pg_locks l left join pg_stat_activity a on a.pid = l.pid
		where l.locktype = 'relation' and l.granted and l.pid is not null and l.pid <> pg_backend_pid()
		  and l.database = (select oid from pg_database where datname = current_database())
		  and l.mode = any($1)`, writeBlocking)
	if err != nil {
		return err
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var k lockKey
		var query string
		if err := rows.Scan(&k.pid, &k.rel, &k.mode, &query); err != nil {
			return err
		}
		t, ok := m.tables[k.rel]
		if !ok {
			continue // tabela criada pela própria migration (ou catálogo)
		}
		if s := m.seen[k]; s != nil {
			s.last = now
		} else {
			m.seen[k] = &lockSeen{lockKey: k, table: t, first: now, last: now, query: query}
		}
	}
	return rows.Err()
}

// finish para o monitor e devolve os locks vistos, do mais longo para o mais curto.
func (m *lockMonitor) finish() []*lockSeen {
	m.cancel()
	<-m.done
	m.conn.Close(context.Background())
	var out []*lockSeen
	for _, s := range m.seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].duration() != out[j].duration() {
			return out[i].duration() > out[j].duration()
		}
		return out[i].table.display < out[j].table.display
	})
	return out
}

// report imprime o resumo no stderr e diz se algum lock passou do limite.
func report(code int, took time.Duration, locks []*lockSeen, threshold time.Duration) bool {
	fmt.Fprintln(os.Stderr)
	logf("comando terminou com exit %d em %s", code, took.Round(time.Millisecond))
	slow := false
	if len(locks) == 0 {
		logf("nenhum lock que bloqueie escrita em tabelas que já existiam")
	} else {
		w := newTableTo(os.Stderr, "TABELA", "LOCK", "BLOQUEIA", "DURAÇÃO", "LINHAS", "COMANDO")
		for _, l := range locks {
			blocks := "escrita"
			if l.mode == "AccessExclusiveLock" {
				blocks = "leitura e escrita"
			}
			dur := "<" + pollEvery.String()
			if d := l.duration(); d > 0 {
				dur = d.Round(time.Millisecond).String()
			}
			if l.duration() > threshold {
				slow = true
				dur += " (!)"
			}
			rows := "?"
			if l.table.tuples >= 0 {
				rows = fmt.Sprintf("%.0f", l.table.tuples)
			}
			w.row(l.table.display, l.mode, blocks, dur, rows, strings.Join(strings.Fields(l.query), " "))
		}
		w.flush()
	}
	logf("tempos medidos na branch local, a cada %s (lock mais curto pode não aparecer); em prod as tabelas", pollEvery)
	logf("são maiores e há tráfego, então o mesmo lock tende a durar mais")
	return slow
}
