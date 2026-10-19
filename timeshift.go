package main

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// `pgb branch create x --at <momento>`: soma (agora - momento) em toda coluna timestamp,
// timestamptz e date da branch, para o momento virar "agora". Regras que olham o tempo passado
// (sessão de 24h, nudge, janela de 7 dias, teto diário) se comportam como naquele momento.
//
//   - date anda em dias inteiros (o deslocamento arredondado);
//   - roda com session_replication_role = replica: triggers (updated_at = now()) e checagem de
//     FK ficam de fora;
//   - cada tabela num savepoint: se falhar (unique num timestamp, que o UPDATE linha a linha pode
//     violar no meio), tenta de novo em duas fases; se ainda falhar, fica como estava, com aviso;
//   - roda antes do slot de mudanças, então não aparece no `pgb branch changes`;
//   - não mexe em datas dentro de texto ou JSON, nem no relógio do app: horário do dia
//     ("aberto agora", virada do dia) segue o relógio de verdade.

// parseAt: "base" é o momento da cópia da base; o resto é hora local ou RFC3339.
func parseAt(at string, base map[string]string) (time.Time, error) {
	if at == "base" {
		t, err := time.Parse(time.RFC3339, base[lCreated])
		if err != nil {
			return t, fmt.Errorf("base %q sem data de criação", base[lName])
		}
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, at, time.Local); err == nil {
			return checkAt(t)
		}
	}
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return checkAt(t)
	}
	return time.Time{}, fmt.Errorf("--at inválido %q (ex.: base, \"2026-10-18 14:32\", 2026-10-18T14:32:00-03:00)", at)
}

func checkAt(t time.Time) (time.Time, error) {
	if t.After(time.Now()) {
		return t, fmt.Errorf("--at %s está no futuro", t.Format("2006-01-02 15:04"))
	}
	return t, nil
}

// timeSkip: globs de tabelas que o deslocamento não toca (time_shift.skip do pgb.yaml, gravado na base).
func timeSkip(base map[string]string) []string {
	if base[lTimeSkip] == "" {
		return nil
	}
	return strings.Split(base[lTimeSkip], ",")
}

type timeCol struct{ schema, table, col, typ string }

func shiftTime(labels map[string]string, at time.Time, skip []string) error {
	shift := time.Since(at).Round(time.Second)
	days := int((shift + 12*time.Hour) / (24 * time.Hour))
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(labels[lUser], labels[lPassword]),
		Host:     "127.0.0.1:" + labels[lPort],
		Path:     "/" + labels[lDB],
		RawQuery: "sslmode=disable",
	}
	conn, err := pgx.Connect(appCtx, u.String())
	if err != nil {
		return err
	}
	defer conn.Close(appCtx)

	rows, err := conn.Query(appCtx, `
select n.nspname, c.relname, a.attname, t.typname
  from pg_attribute a
  join pg_class c on c.oid = a.attrelid
  join pg_namespace n on n.oid = c.relnamespace
  join pg_type t on t.oid = a.atttypid
 where c.relkind in ('r', 'p') and not c.relispartition
   and a.attnum > 0 and not a.attisdropped and a.attgenerated = ''
   and t.typname in ('timestamptz', 'timestamp', 'date')
   and n.nspname not in ('pg_catalog', 'information_schema') and n.nspname not like 'pg\_%'
   and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass
                     and d.objid = c.oid and d.deptype = 'e')
 order by 1, 2, a.attnum`)
	if err != nil {
		return err
	}
	cols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (timeCol, error) {
		var c timeCol
		return c, r.Scan(&c.schema, &c.table, &c.col, &c.typ)
	})
	if err != nil {
		return err
	}
	byTable := map[string][]timeCol{}
	var tables []string
	for _, c := range cols {
		name := c.table
		if c.schema != "public" {
			name = c.schema + "." + c.table
		}
		if skipped(name, skip) || (c.typ == "date" && days == 0) {
			continue
		}
		if byTable[name] == nil {
			tables = append(tables, name)
		}
		byTable[name] = append(byTable[name], c)
	}
	sort.Strings(tables)

	start := time.Now()
	if _, err := conn.Exec(appCtx, "set session_replication_role = replica"); err != nil {
		logf("aviso: sem superusuário, os triggers rodam no deslocamento: %v", err)
	}
	tx, err := conn.Begin(appCtx)
	if err != nil {
		return err
	}
	defer tx.Rollback(appCtx)
	var total int64
	var failed []string
	for _, name := range tables {
		cs := byTable[name]
		table := pgx.Identifier{cs[0].schema, cs[0].table}.Sanitize()
		update := func(sec int64, days int) string {
			var sets []string
			for _, c := range cs {
				col := pgx.Identifier{c.col}.Sanitize()
				if c.typ == "date" {
					sets = append(sets, fmt.Sprintf("%s = %s + %d", col, col, days))
				} else {
					sets = append(sets, fmt.Sprintf("%s = %s + interval '%d seconds'", col, col, sec))
				}
			}
			return fmt.Sprintf("update %s set %s", table, strings.Join(sets, ", "))
		}
		sec := int64(shift.Seconds())
		// Unique num timestamp: o UPDATE checa linha a linha e pode bater num valor que ainda não
		// andou. Na segunda tentativa tudo vai para +1000 anos (longe de qualquer valor antigo) e volta.
		const far = 1000 * 365
		n, err := execSavepoint(tx, update(sec, days))
		if err != nil {
			n, err = execSavepoint(tx, update(sec+far*86400, days+far), update(-far*86400, -far))
		}
		if err != nil {
			failed = append(failed, name)
			logf("aviso: %s ficou sem deslocar: %v", name, err)
			continue
		}
		total += n
	}
	if err := tx.Commit(appCtx); err != nil {
		return err
	}
	logf("tempo deslocado em +%s (%s virou agora): %d tabelas, %d linhas em %s",
		fmtShift(shift), at.Local().Format("2006-01-02 15:04"), len(tables)-len(failed), total,
		time.Since(start).Round(time.Millisecond))
	return nil
}

// execSavepoint roda os comandos num savepoint; se um falhar, desfaz todos e devolve o erro.
func execSavepoint(tx pgx.Tx, sqls ...string) (int64, error) {
	if _, err := tx.Exec(appCtx, "savepoint shift"); err != nil {
		return 0, err
	}
	var n int64
	for _, sql := range sqls {
		tag, err := tx.Exec(appCtx, sql)
		if err != nil {
			if _, rerr := tx.Exec(appCtx, "rollback to savepoint shift"); rerr != nil {
				return 0, rerr
			}
			return 0, err
		}
		n = tag.RowsAffected()
	}
	return n, nil
}

func skipped(name string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(strings.TrimSpace(g), name); ok {
			return true
		}
	}
	return false
}

// fmtShift: 1d19h28m, 3h05m, 42m.
func fmtShift(d time.Duration) string {
	m := int64(d.Minutes())
	days, h, min := m/(24*60), m/60%24, m%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%02dh%02dm", days, h, min)
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, min)
	}
	return fmt.Sprintf("%dm", min)
}
