package main

import (
	"context"
	"testing"
	"time"
)

func TestParseAt(t *testing.T) {
	base := map[string]string{lName: "prod", lCreated: "2026-10-18T14:32:00Z"}
	if got, err := parseAt("base", base); err != nil || !got.Equal(time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)) {
		t.Errorf("--at base: %v %v", got, err)
	}
	if _, err := parseAt("base", map[string]string{lName: "velha"}); err == nil {
		t.Error("--at base sem data de criação: esperava erro")
	}

	local := time.Date(2026, 10, 18, 14, 32, 0, 0, time.Local)
	for _, in := range []string{"2026-10-18 14:32", "2026-10-18T14:32", "2026-10-18 14:32:00", "2026-10-18T14:32:00"} {
		if got, err := parseAt(in, nil); err != nil || !got.Equal(local) {
			t.Errorf("parseAt(%q) = %v, %v; want %v (hora local)", in, got, err, local)
		}
	}
	if got, err := parseAt("2026-10-18T14:32:00-03:00", nil); err != nil || !got.Equal(time.Date(2026, 10, 18, 17, 32, 0, 0, time.UTC)) {
		t.Errorf("RFC3339 com fuso: %v %v", got, err)
	}

	future := time.Now().Add(48 * time.Hour).Format("2006-01-02 15:04")
	if _, err := parseAt(future, nil); err == nil {
		t.Error("--at no futuro: esperava erro")
	}
	for _, in := range []string{"ontem", "18/10/2026 14:32", "2026-10-18", ""} {
		if _, err := parseAt(in, nil); err == nil {
			t.Errorf("parseAt(%q): esperava erro", in)
		}
	}
}

func TestFmtShift(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0m", 59 * time.Second: "0m", 42 * time.Minute: "42m", 61 * time.Minute: "1h01m",
		24 * time.Hour: "1d00h00m", 43*time.Hour + 28*time.Minute: "1d19h28m",
	} {
		if got := fmtShift(d); got != want {
			t.Errorf("fmtShift(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestTimeSkip(t *testing.T) {
	skip := timeSkip(map[string]string{lTimeSkip: "audit_*, crm.*,logs"})
	for name, want := range map[string]bool{"audit_log": true, "crm.visits": true, "logs": true, "users": false, "logs_2026": false} {
		if got := skipped(name, skip); got != want {
			t.Errorf("skipped(%q) = %v, want %v", name, got, want)
		}
	}
	if timeSkip(map[string]string{}) != nil {
		t.Error("sem label: nada a pular")
	}
}

func TestShiftTime(t *testing.T) {
	conn, labels := testDB(t)
	ctx := context.Background()
	mustExec(t, conn, `create table ev (id int primary key, at timestamptz, local_at timestamp, day date,
		updated_at timestamptz, next_day date generated always as (day + 1) stored)`)
	mustExec(t, conn, `create function touch() returns trigger language plpgsql as
		$$ begin new.updated_at = now(); return new; end $$`)
	mustExec(t, conn, `create trigger touch before update on ev for each row execute function touch()`)
	mustExec(t, conn, `insert into ev values
		(1, '2026-01-01 10:00+00', '2026-01-01 10:00', '2026-01-01', '2026-01-01 09:00+00'),
		(2, null, null, null, null)`)
	// Um timestamp por segundo numa janela maior que o deslocamento: o UPDATE linha a linha
	// colide com certeza, e só a segunda tentativa (em duas fases) desloca a tabela.
	mustExec(t, conn, `create table uniq (ts timestamptz primary key)`)
	mustExec(t, conn, `insert into uniq select '2026-01-01'::timestamptz + make_interval(secs => g)
		from generate_series(0, 31 * 3600) g`)
	mustExec(t, conn, `create table audit_log (at timestamptz)`)
	mustExec(t, conn, `insert into audit_log values ('2026-01-01 10:00+00')`)
	mustExec(t, conn, `create schema crm`)
	mustExec(t, conn, `create table crm.visits (at timestamptz)`)
	mustExec(t, conn, `insert into crm.visits values ('2026-01-01 10:00+00')`)

	at := time.Now().Add(-30 * time.Hour)
	if err := shiftTime(labels, at, timeSkip(map[string]string{lTimeSkip: "audit_*"})); err != nil {
		t.Fatal(err)
	}

	var shiftedAt time.Time
	if err := conn.QueryRow(ctx, "select at from ev where id = 1").Scan(&shiftedAt); err != nil {
		t.Fatal(err)
	}
	shift := shiftedAt.Sub(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC))
	if (shift-30*time.Hour).Abs() > 5*time.Second || shift%time.Second != 0 {
		t.Fatalf("deslocamento %s, esperava ~30h em segundos inteiros", shift)
	}

	var ok bool
	err := conn.QueryRow(ctx, `select
		local_at = '2026-01-01 10:00'::timestamp + $1::interval
		and day = '2026-01-02'
		and next_day = '2026-01-03'
		and updated_at = '2026-01-01 09:00+00'::timestamptz + $1::interval
		from ev where id = 1`, shift).Scan(&ok)
	if err != nil || !ok {
		t.Errorf("ev: timestamp sem fuso, date (1 dia), coluna gerada ou trigger fora do esperado (%v)", err)
	}
	var nulls int
	conn.QueryRow(ctx, "select num_nulls(at, local_at, day, updated_at) from ev where id = 2").Scan(&nulls)
	if nulls != 4 {
		t.Errorf("null deveria continuar null (%d de 4)", nulls)
	}

	err = conn.QueryRow(ctx, `select count(*) = 31 * 3600 + 1
		and min(ts) = '2026-01-01'::timestamptz + $1::interval
		and max(ts) = '2026-01-01'::timestamptz + interval '31 hours' + $1::interval
		from uniq`, shift).Scan(&ok)
	if err != nil || !ok {
		t.Errorf("uniq (timestamp unique) não foi deslocado inteiro (%v)", err)
	}

	err = conn.QueryRow(ctx, `select (select at from crm.visits) = '2026-01-01 10:00+00'::timestamptz + $1::interval
		and (select at from audit_log) = '2026-01-01 10:00+00'`, shift).Scan(&ok)
	if err != nil || !ok {
		t.Errorf("crm.visits devia andar e audit_log (time_shift.skip) não (%v)", err)
	}
}
