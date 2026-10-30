package main

import (
	"context"
	"testing"
	"time"
)

// Uma "migration" de verdade numa conexão ao lado: o monitor tem que ver os locks que travam
// escrita em tabelas que já existiam, e só eles.
func TestLockMonitor(t *testing.T) {
	conn, labels := testDB(t)
	ctx := context.Background()
	mustExec(t, conn, "create table orders (id int)")
	mustExec(t, conn, "create table users (id int)")
	mustExec(t, conn, "create table logs (id int)")

	m, err := startMonitor(ctx, labelsURL(labels))
	if err != nil {
		t.Fatal(err)
	}
	mig := connectTo(t, labels)
	run := func(hold time.Duration, sqls ...string) {
		t.Helper()
		tx, err := mig.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, sql := range append(sqls, "select pg_sleep($1)") {
			var args []any
			if sql == "select pg_sleep($1)" {
				args = []any{hold.Seconds()}
			}
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	run(600*time.Millisecond,
		"alter table orders add column total numeric",    // AccessExclusive
		"lock table logs in share update exclusive mode", // não bloqueia escrita
		"create table novo (id int)",                     // tabela da própria migration
		"lock table novo in access exclusive mode")
	run(300*time.Millisecond, "create index on users (id)") // Share
	locks := m.finish()

	got := map[string]time.Duration{}
	for _, l := range locks {
		got[l.table.display+" "+l.mode] = l.duration()
	}
	if len(got) != 2 {
		t.Fatalf("esperava só orders e users, veio %v", got)
	}
	if d, ok := got["orders AccessExclusiveLock"]; !ok || d < 400*time.Millisecond || d > 2*time.Second {
		t.Errorf("orders AccessExclusiveLock: %v (presente=%v), esperava ~600ms", d, ok)
	}
	if d, ok := got["users ShareLock"]; !ok || d > 2*time.Second {
		t.Errorf("users ShareLock: %v (presente=%v)", d, ok)
	}
	if locks[0].table.display != "orders" {
		t.Errorf("o lock mais longo deve vir primeiro, veio %s", locks[0].table.display)
	}

	if !report(0, time.Second, locks, 300*time.Millisecond) {
		t.Error("lock de ~600ms com limite de 300ms deveria estourar (exit 2)")
	}
	if report(0, time.Second, locks, 5*time.Second) {
		t.Error("nenhum lock passou de 5s: não deveria estourar")
	}
}
