package main

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseChangeTrickyLines(t *testing.T) {
	cases := []struct {
		name, line string
		want       change
	}{
		{
			name: "texto com aspas, dois-pontos e colchetes",
			line: `table public.t: INSERT: id[integer]:1 nome[text]:'O''Neil: a [b]' n[numeric]:-1.5e3`,
			want: change{table: "t", op: "INSERT", cols: []colVal{
				{name: "id", val: "1"}, {name: "nome", val: "O'Neil: a [b]", quoted: true}, {name: "n", val: "-1.5e3"}}},
		},
		{
			name: "schema, tabela e coluna com aspas duplas",
			line: `table "Meu ""Schema"""."a.b c": INSERT: "Col ]: X"[text]:'v' id[bigint]:7`,
			want: change{table: `Meu "Schema".a.b c`, op: "INSERT", cols: []colVal{
				{name: `"Col ]: X"`, val: "v", quoted: true}, {name: "id", val: "7"}}},
		},
		{
			name: "tipo com espaço e tipo entre aspas com ]: dentro",
			line: `table public.t: INSERT: at[timestamp with time zone]:'2026-10-18 14:32:00+00' s["st]:atus"]:'ok' b[bit varying]:B'0101'`,
			want: change{table: "t", op: "INSERT", cols: []colVal{
				{name: "at", val: "2026-10-18 14:32:00+00", quoted: true}, {name: "s", val: "ok", quoted: true}, {name: "b", val: "B'0101'"}}},
		},
		{
			name: "UPDATE que muda a chave, com new-tuple: dentro de um valor",
			line: `table public.t: UPDATE: old-key: id[integer]:1 new-tuple: id[integer]:2 nota[text]:'x new-tuple: y'`,
			want: change{table: "t", op: "UPDATE", oldKey: []colVal{{name: "id", val: "1"}}, cols: []colVal{
				{name: "id", val: "2"}, {name: "nota", val: "x new-tuple: y", quoted: true}}},
		},
		{
			name: "DELETE sem replica identity",
			line: `table public.t: DELETE: (no-tuple-data)`,
			want: change{table: "t", op: "DELETE"},
		},
		{
			name: "null, texto vazio e TOAST não alterado",
			line: `table public.t: UPDATE: id[integer]:1 a[text]:null b[text]:'' c[text]:unchanged-toast-datum`,
			want: change{table: "t", op: "UPDATE", cols: []colVal{
				{name: "id", val: "1"}, {name: "a", val: "null"}, {name: "b", val: "", quoted: true}, {name: "c", val: "unchanged-toast-datum"}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseChange(c.line)
			assertChange(t, got, c.want)
		})
	}
}

func TestParseChangeKeepsUnparseableLinesRaw(t *testing.T) {
	for _, line := range []string{
		`message: transactional: 1 prefix: x, sz: 1 content:y`,
		`table public.t: INSERT: id[integer]:'sem fim`,
		`table public.t: INSERT: id[integer`,
		`table semponto: INSERT: id[integer]:1`,
	} {
		got := parseChange(line)
		if got.raw != line || got.format(false) != line {
			t.Errorf("parseChange(%q): esperava a linha crua, veio %+v", line, got)
		}
	}
	tr := parseChange(`table public.t: TRUNCATE: restart_seqs cascade`)
	if tr.table != "t" || tr.op != "TRUNCATE" || tr.format(false) != "TRUNCATE t restart_seqs cascade" {
		t.Errorf("TRUNCATE: %+v / %q", tr, tr.format(false))
	}
}

func TestChangeFormat(t *testing.T) {
	long := strings.Repeat("é", 100)
	c := change{table: "t", op: "UPDATE",
		oldKey: []colVal{{name: "id", val: "1"}},
		cols:   []colVal{{name: "id", val: "2"}, {name: "nota", val: "it's\nfine", quoted: true}, {name: "longo", val: long, quoted: true}}}

	short := c.format(false)
	want := "UPDATE t  [antes: id=1] id=2 nota='it''s\\nfine' longo='" + strings.Repeat("é", maxValue) + "…'"
	if short != want {
		t.Errorf("format(false):\n got %q\nwant %q", short, want)
	}
	full := c.format(true)
	if !strings.Contains(full, "nota='it''s\nfine'") || !strings.Contains(full, "longo='"+long+"'") {
		t.Errorf("format(true) truncou ou escapou: %q", full)
	}
}

func TestCommitTime(t *testing.T) {
	got := commitTime("COMMIT 739 (at 2026-10-19 12:00:00.5+00)")
	want := time.Date(2026, 10, 19, 12, 0, 0, 5e8, time.UTC).Local().Format("2006-01-02 15:04:05.000")
	if got != want {
		t.Errorf("got %q, want %q (no fuso local)", got, want)
	}
	if got := commitTime("COMMIT 739 (at amanhã)"); got != "amanhã" {
		t.Errorf("horário que não parseia deve sair como veio, saiu %q", got)
	}
	if got := commitTime("COMMIT 739"); got != "" {
		t.Errorf("sem horário: %q", got)
	}
}

// O oráculo é o próprio Postgres: o que o parser extrai da saída real do test_decoding tem que
// ser igual ao valor da coluna convertido para texto.
func TestChangesParseRealDecoding(t *testing.T) {
	conn, _ := testDB(t)
	ctx := context.Background()
	mustExec(t, conn, `create type "st]:atus" as enum ('ok', 'it''s: [x]')`)
	mustExec(t, conn, `create schema "crm x"`)
	mustExec(t, conn, `create table "crm x"."lead.s" (
		id int primary key, "Nome Completo" text, nota text, tags text[], dados jsonb,
		quando timestamptz, valor numeric, ativo boolean, bits bit varying(8), status "st]:atus", vazio text)`)
	mustExec(t, conn, "select pg_create_logical_replication_slot($1, 'test_decoding')", changesSlot)

	mustExec(t, conn, `insert into "crm x"."lead.s" values
		(1, 'O''Neil: a [b]', e'linha 1\nlinha 2 new-tuple: x\\y', array['a b', 'c''d', 'e"f'],
		 '{"k": "v''\"]:"}', '2026-10-18 14:32:00.123456+00', 12.50, true, B'0101', 'it''s: [x]', ''),
		(2, null, null, null, null, null, null, null, null, null, null)`)
	cols := []string{"id", `"Nome Completo"`, "nota", "tags", "dados", "quando", "valor", "ativo", "bits", "status", "vazio"}
	unquoted := map[string]bool{"id": true, "valor": true, "ativo": true}
	want := map[string][]colVal{}
	for _, id := range []string{"1", "2"} {
		var texts []*string
		for range cols {
			texts = append(texts, new(string))
		}
		dest := make([]any, len(cols))
		for i := range cols {
			dest[i] = &texts[i]
		}
		q := "select " + strings.Join(cols, "::text, ") + `::text from "crm x"."lead.s" where id = ` + id
		if err := conn.QueryRow(ctx, q).Scan(dest...); err != nil {
			t.Fatal(err)
		}
		for i, name := range cols {
			v := colVal{name: name}
			switch {
			case texts[i] == nil:
				v.val = "null"
			case name == "bits":
				v.val = "B'" + *texts[i] + "'"
			default:
				v.val, v.quoted = *texts[i], !unquoted[name]
			}
			want[id] = append(want[id], v)
		}
	}
	mustExec(t, conn, `update "crm x"."lead.s" set id = 10 where id = 1`)
	mustExec(t, conn, `delete from "crm x"."lead.s" where id = 2`)
	mustExec(t, conn, `truncate "crm x"."lead.s"`)

	rows, err := conn.Query(ctx, "select data from pg_logical_slot_peek_changes($1, null, null, 'include-timestamp', '1', 'skip-empty-xacts', '1')", changesSlot)
	if err != nil {
		t.Fatal(err)
	}
	var got []change
	var commits []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasPrefix(line, "BEGIN "):
		case strings.HasPrefix(line, "COMMIT "):
			commits = append(commits, commitTime(line))
		default:
			got = append(got, parseChange(line))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("esperava 2 INSERT, UPDATE, DELETE e TRUNCATE; veio %d mudanças: %+v", len(got), got)
	}
	table := "crm x.lead.s"
	updated := append([]colVal{{name: "id", val: "10"}}, want["1"][1:]...)
	assertChange(t, got[0], change{table: table, op: "INSERT", cols: want["1"]})
	assertChange(t, got[1], change{table: table, op: "INSERT", cols: want["2"]})
	assertChange(t, got[2], change{table: table, op: "UPDATE", oldKey: []colVal{{name: "id", val: "1"}}, cols: updated})
	assertChange(t, got[3], change{table: table, op: "DELETE", cols: []colVal{{name: "id", val: "2"}}})
	if got[4].op != "TRUNCATE" || got[4].table != table {
		t.Errorf("TRUNCATE: %+v", got[4])
	}

	stamp := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3}$`)
	for _, c := range commits {
		at, err := time.ParseInLocation("2006-01-02 15:04:05.000", c, time.Local)
		if !stamp.MatchString(c) || err != nil || time.Since(at).Abs() > time.Minute {
			t.Errorf("horário do COMMIT %q não virou a hora local de agora", c)
		}
	}
}

func assertChange(t *testing.T, got, want change) {
	t.Helper()
	if got.raw != "" {
		t.Errorf("não interpretou a linha: %q", got.raw)
		return
	}
	if got.table != want.table || got.op != want.op {
		t.Errorf("tabela/op: got %q %q, want %q %q", got.table, got.op, want.table, want.op)
	}
	assertCols(t, "oldKey", got.oldKey, want.oldKey)
	assertCols(t, "cols", got.cols, want.cols)
}

func assertCols(t *testing.T, what string, got, want []colVal) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d colunas %+v, want %d %+v", what, len(got), got, len(want), want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: got %+v, want %+v", what, i, got[i], want[i])
		}
	}
}
