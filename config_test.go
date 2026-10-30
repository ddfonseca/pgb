package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "pgb.yaml")
	if err := os.WriteFile(f, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `
tables:
  users: all
  logs: schema-only
  orders: {where: "created_at > {{since}}"}
mask:
  users: {email: email, phone: "null"}
`), true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]TableRule{"users": {Mode: "all"}, "logs": {Mode: "schema-only"},
		"orders": {Mode: "where", Where: "created_at > {{since}}"}}
	for k, w := range want {
		if cfg.Tables[k] != w {
			t.Errorf("tables.%s = %+v, want %+v", k, cfg.Tables[k], w)
		}
	}
	if cfg.Mask["users"]["phone"] != "null" {
		t.Errorf("mask: %+v", cfg.Mask)
	}

	// Erro de digitação tem que falhar, não virar "copia tudo sem filtro".
	for name, yaml := range map[string]string{
		"campo desconhecido": "tabels:\n  users: all\n",
		"regra inválida":     "tables:\n  users: everything\n",
		"where vazio":        "tables:\n  users: {where: \"\"}\n",
		"chave errada":       "tables:\n  users: {wher: \"x\"}\n",
	} {
		if _, err := loadConfig(writeConfig(t, yaml), true); err == nil {
			t.Errorf("%s: esperava erro", name)
		}
	}

	missing := filepath.Join(t.TempDir(), "nao-existe.yaml")
	if cfg, err := loadConfig(missing, false); err != nil || cfg == nil {
		t.Errorf("arquivo opcional ausente: cfg=%v err=%v", cfg, err)
	}
	if _, err := loadConfig(missing, true); err == nil {
		t.Error("arquivo obrigatório ausente: esperava erro")
	}
}

func TestRuleFor(t *testing.T) {
	rules := map[string]TableRule{
		"events":          {Mode: "where", Where: "exato"},
		"event*":          {Mode: "schema-only"},
		"event_log*":      {Mode: "where", Where: "mais longo"},
		"audit.*":         {Mode: "schema-only"},
		"[ab]ccounts":     {Mode: "all"},
		"*":               {Mode: "where", Where: "curinga"},
		"crm.leads":       {Mode: "where", Where: "qualificado"},
		"public_lookalik": {Mode: "schema-only"},
	}
	cases := []struct{ table, pattern, where string }{
		{"events", "events", "exato"},
		{"event_logs_2026", "event_log*", "mais longo"},
		{"eventos", "event*", ""},
		{"audit.trail", "audit.*", ""},
		{"crm.leads", "crm.leads", "qualificado"},
		{"accounts", "[ab]ccounts", ""},
		{"users", "*", "curinga"},
	}
	for _, c := range cases {
		r, pat := ruleFor(rules, c.table)
		if pat != c.pattern || r.Where != c.where {
			t.Errorf("ruleFor(%q) = %+v via %q, want via %q", c.table, r, pat, c.pattern)
		}
	}
	if r, pat := ruleFor(map[string]TableRule{"x*": {Mode: "schema-only"}}, "users"); pat != "" || r.Mode != "all" {
		t.Errorf("sem regra deveria ser all, veio %+v via %q", r, pat)
	}
}

func TestSelection(t *testing.T) {
	cfg := &Config{
		Tables: map[string]TableRule{"users": {Mode: "all"}, "logs": {Mode: "schema-only"}},
		Profiles: map[string]Profile{"tenant": {
			Params: []string{"tenant"},
			Scope:  map[string]string{"tenant_id": "{{tenant}}"},
			Tables: map[string]TableRule{"users": {Mode: "where", Where: "id = 1"}, "tmp_*": {Mode: "schema-only"}},
		}},
	}
	sel, err := cfg.selection("tenant", map[string]string{"tenant": "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.rules["users"].Where != "id = 1" || sel.rules["logs"].Mode != "schema-only" || sel.rules["tmp_*"].Mode != "schema-only" {
		t.Errorf("rules: %+v", sel.rules)
	}
	if !sel.explicit["users"] || sel.explicit["tmp_*"] || sel.explicit["logs"] {
		t.Errorf("explicit (só nomes exatos do profile): %+v", sel.explicit)
	}
	if sel.scope["tenant_id"] != "{{tenant}}" {
		t.Errorf("scope: %+v", sel.scope)
	}
	if cfg.Tables["users"].Mode != "all" {
		t.Error("selection alterou cfg.Tables: o próximo profile herdaria a regra")
	}

	if _, err := cfg.selection("tenant", nil); err == nil || !strings.Contains(err.Error(), "--set tenant=") {
		t.Errorf("profile sem o parâmetro: %v", err)
	}
	if _, err := cfg.selection("nao-existe", nil); err == nil {
		t.Error("profile inexistente: esperava erro")
	}
	if sel, err := cfg.selection("", nil); err != nil || sel.scope != nil || len(sel.explicit) != 0 {
		t.Errorf("sem profile: %+v %v", sel, err)
	}
}

func TestExpandErrors(t *testing.T) {
	if _, err := expand("created_at > {{since}}", 0, nil); err == nil {
		t.Error("{{since}} sem --since: esperava erro")
	}
	if _, err := expand("tenant_id = {{tenant}}", time.Hour, map[string]string{"outro": "x"}); err == nil || !strings.Contains(err.Error(), "--set tenant=") {
		t.Errorf("parâmetro ausente: %v", err)
	}
	if out, err := expand("{{ tenant }} {{tenant}}", 0, map[string]string{"tenant": "a"}); err != nil || out != "'a' 'a'" {
		t.Errorf("espaços no placeholder: %q %v", out, err)
	}
}

// expand e maskExpr geram SQL que vai para a origem: o oráculo é o Postgres executando o SQL.
func TestExpandAndMaskInPostgres(t *testing.T) {
	conn, _ := testDB(t)
	ctx := context.Background()

	evil := `acme'); drop table x; --` + "\n\\'"
	sql, err := expand("select {{tenant}}::text, {{since}}", 30*24*time.Hour, map[string]string{"tenant": evil})
	if err != nil {
		t.Fatal(err)
	}
	var tenant string
	var since time.Time
	if err := conn.QueryRow(ctx, sql).Scan(&tenant, &since); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if tenant != evil {
		t.Errorf("--set não chegou literal: got %q, want %q", tenant, evil)
	}
	if d := time.Since(since) - 30*24*time.Hour; d.Abs() > time.Minute {
		t.Errorf("{{since}} = %s, esperava 30 dias atrás", since)
	}

	mustExec(t, conn, `create table people (id int, "e-mail" text, phone text)`)
	mustExec(t, conn, `insert into people values (1, 'ana@x.com', '+5511987654321'), (2, 'ana@x.com', '+5511912345678'), (3, null, null)`)
	presets := map[string]string{"email": "'user' || md5({{salt}} || {{col}}) || '@exemplo.invalid'"}
	masked := func(rule, salt string) []*string {
		t.Helper()
		col := `"e-mail"`
		if rule == "phone-null" {
			rule, col = "null", "phone"
		}
		rows, err := conn.Query(ctx, "select "+maskExpr(rule, col, salt, presets)+" from people order by id")
		if err != nil {
			t.Fatalf("maskExpr(%q): %v", rule, err)
		}
		defer rows.Close()
		var out []*string
		for rows.Next() {
			var v *string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}

	h1, h2 := masked("hash", "s1"), masked("hash", "s2")
	if *h1[0] != *h1[1] {
		t.Error("hash: o mesmo valor com o mesmo sal deve dar o mesmo hash (joins entre tabelas)")
	}
	if *h1[0] == *h2[0] {
		t.Error("hash: sal diferente deveria dar hash diferente")
	}
	var plain string
	conn.QueryRow(ctx, "select md5('ana@x.com')").Scan(&plain)
	if *h1[0] == plain || strings.Contains(*h1[0], "ana") {
		t.Errorf("hash sem sal é reversível por dicionário: %q", *h1[0])
	}
	if h1[2] != nil {
		t.Error("hash de null deveria continuar null")
	}

	e := masked("email", "s1")
	if !strings.HasSuffix(*e[0], "@exemplo.invalid") || *e[0] != *e[1] {
		t.Errorf("preset email: %q %q", *e[0], *e[1])
	}
	for _, v := range masked("phone-null", "s1") {
		if v != nil {
			t.Errorf("mask null deixou valor: %q", *v)
		}
	}
}

func TestParseSince(t *testing.T) {
	ok := map[string]time.Duration{"": 0, "90m": 90 * time.Minute, "12h": 12 * time.Hour, "30d": 30 * 24 * time.Hour, "2w": 14 * 24 * time.Hour}
	for in, want := range ok {
		if got, err := parseSince(in); err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"30", "d", "0d", "-1d", "1.5d", "30s", "1y", "abc"} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q): esperava erro", in)
		}
	}
}

func TestParseSize(t *testing.T) {
	ok := map[string]int64{"500MB": 500 << 20, "20GB": 20 << 30, "1TB": 1 << 40, " 1.5 gb ": 3 << 29, "64kb": 64 << 10}
	for in, want := range ok {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"20", "GB", "0GB", "-1GB", "20 GiB", "x MB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q): esperava erro", in)
		}
	}
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB",
		1<<20 - 1: "1024.0 KB", 1 << 20: "1.0 MB", 20 << 30: "20.0 GB", 3 << 40: "3.0 TB"} {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
	// ida e volta com o parseSize (o max_size do pgb.yaml aparece assim nas mensagens)
	for _, s := range []string{"500.0 MB", "20.0 GB", "1.5 TB"} {
		n, _ := parseSize(strings.ReplaceAll(s, " ", ""))
		if got := humanSize(n); got != s {
			t.Errorf("humanSize(parseSize(%q)) = %q", s, got)
		}
	}
}
