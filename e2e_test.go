package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// O caminho inteiro pelo binário, como o usuário roda: base de um container, branch isolada da
// origem, changes, ponto de salvamento, reset e exec --rm.
func TestEndToEnd(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "pgb")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	pgb := func(args ...string) (string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(bin, args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("pgb %s: %v", strings.Join(args, " "), err)
		}
		t.Logf("pgb %s\n%s", strings.Join(args, " "), stderr.String())
		return strings.TrimSpace(stdout.String()), cmd.ProcessState.ExitCode()
	}
	must := func(args ...string) string {
		t.Helper()
		out, code := pgb(args...)
		if code != 0 {
			t.Fatalf("pgb %s: exit %d", strings.Join(args, " "), code)
		}
		return out
	}
	connect := func(u string) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(ctx) })
		return conn
	}
	query := func(conn *pgx.Conn, sql string) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, sql).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	const state = "select count(*) || ' ' || string_agg(name, ',' order by id) from items"

	id := randHex(3)
	src := "pgbtest-src-" + id
	port, err := startPostgres(src, "POSTGRES_DB=app")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", src).Run() })
	if err != nil {
		t.Fatal(err)
	}
	srcConn := connectTo(t, map[string]string{lUser: "postgres", lPassword: localPassword, lDB: "app", lPort: port})
	mustExec(t, srcConn, "create table items (id int primary key, name text)")
	mustExec(t, srcConn, "insert into items values (1, 'a'), (2, 'b'), (3, 'c')")

	base, br := "e2e-"+id, "br-"+id
	must("base", "create", base, "--from-container", src)
	t.Cleanup(func() { pgb("base", "rm", base, "--force") })

	u := must("branch", "create", br, "--from", base)
	conn := connect(u)
	if got := query(conn, state); got != "3 a,b,c" {
		t.Fatalf("branch nova: %q, esperava a cópia da origem", got)
	}
	mustExec(t, conn, "insert into items values (4, 'novo')")
	mustExec(t, conn, "update items set name = 'mudou' where id = 1")
	if got := query(srcConn, state); got != "3 a,b,c" {
		t.Errorf("escrever na branch mexeu na origem: %q", got)
	}

	changes := must("branch", "changes", br)
	for _, want := range []string{"INSERT items  id=4 name='novo'", "UPDATE items  id=1 name='mudou'"} {
		if !strings.Contains(changes, want) {
			t.Errorf("branch changes sem %q:\n%s", want, changes)
		}
	}

	must("save", "create", br, "p1")
	conn = connect(u) // o save para a branch durante a cópia
	mustExec(t, conn, "delete from items")
	must("save", "restore", br, "p1")
	if got := must("branch", "url", br); got != u {
		t.Errorf("a URL mudou no restore: %q -> %q", u, got)
	}
	if got := query(connect(u), state); got != "4 mudou,b,c,novo" {
		t.Errorf("depois do restore: %q, esperava o estado salvo", got)
	}

	must("branch", "reset", br)
	if got := query(connect(u), state); got != "3 a,b,c" {
		t.Errorf("depois do reset: %q, esperava a base", got)
	}
	if got := must("branch", "changes", br); got != "" {
		t.Errorf("branch resetada ainda mostra mudanças:\n%s", got)
	}

	ex := "ex-" + id
	if _, code := pgb("branch", "exec", ex, "--create", "--from", base, "--rm", "--",
		"sh", "-c", `case "$DATABASE_URL" in postgres://*) exit 7;; esac; exit 1`); code != 7 {
		t.Errorf("exec: exit %d, esperava o 7 do comando (com DATABASE_URL)", code)
	}
	if _, code := pgb("branch", "url", ex); code == 0 {
		t.Error("exec --rm deixou a branch para trás")
	}
}
