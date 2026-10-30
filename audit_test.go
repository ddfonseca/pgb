package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// As regexes rodam no Postgres (sintaxe ARE), então é lá que elas são testadas.
func TestPIIPatterns(t *testing.T) {
	conn, _ := testDB(t)
	cases := []struct {
		pattern, text string
		match         bool
	}{
		{"whatsapp", "5511987654321@wa.gw.msging.net", true},
		{"whatsapp", "0123456789abcdef0123456789abcdef@wa.gw.msging.net", false}, // preset chat
		{"telefone", "ligar +55 11 98765-4321 amanhã", true},
		{"telefone", "+5511987654321", true},
		{"telefone", "(11) 98765-4321", true},
		{"telefone", "(11) 3456-7890", true},
		{"telefone", "5511987654321", true},
		{"telefone", "11987654321", false},    // 11 dígitos soltos
		{"telefone", "1729530000000", false},  // timestamp em ms
		{"telefone", "55119876543210", false}, // número maior que contém o padrão
		{"telefone", "pedido 551198765432", false},
		{"email", "Ana.Silva+promo@gmail.com", true},
		{"email", `{"contato": "ana@empresa.com.br"}`, true},
		{"email", "user@exemplo.invalid", false}, // preset email
		{"email", "5511987654321@wa.gw.msging.net", false},
		{"email", "bot@msging.net", false},
		{"email", "nome@localhost", false},
		{"cpf", "cpf 123.456.789-09.", true},
		{"cpf", "12345678909", false},
		{"cpf", "1123.456.789-09", false},
	}
	re := map[string]string{}
	for _, p := range piiPatterns {
		re[p.name] = p.re
	}
	for _, c := range cases {
		var got bool
		if err := conn.QueryRow(context.Background(), "select $1::text ~ $2::text", c.text, re[c.pattern]).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		if got != c.match {
			t.Errorf("%s ~ %q: got %v, want %v", c.pattern, c.text, got, c.match)
		}
	}
}

func TestAuditDB(t *testing.T) {
	conn, _ := testDB(t)
	ctx := context.Background()
	mustExec(t, conn, `create table users (id int, email text, phone varchar(20), obs text, bio text,
		created bigint, prefs jsonb, tags text[])`)
	mustExec(t, conn, `insert into users values
		(1, 'ana@gmail.com', '+55 11 98765-4321', 'cpf 123.456.789-09', 'gosta de café', 5511987654321,
		 '{"contato": "zeca@empresa.com.br", "n": 1}', array['vip', '123.456.789-09']),
		(2, 'bia@gmail.com', null, null, null, null, null, null),
		(3, 'x@exemplo.invalid', null, null, null, null, null, null)`)
	mustExec(t, conn, `create schema crm`)
	mustExec(t, conn, `create table crm.leads (fone text)`)
	mustExec(t, conn, `insert into crm.leads values ('(11) 98765-4321'), ('sem telefone')`)
	mustExec(t, conn, `create table ev (id int, msg text) partition by range (id)`)
	mustExec(t, conn, `create table ev_1 partition of ev for values from (1) to (100)`)
	mustExec(t, conn, `insert into ev values (1, 'escreveu para c@d.com')`)

	cfg := &Config{
		Mask:  map[string]map[string]string{"users": {"phone": "null"}},
		Audit: AuditConfig{Ignore: []string{"users.obs", "nada.*"}},
	}
	got := auditSummary(t, conn, cfg, true)
	want := []string{
		"crm.leads.fone telefone 1 (11) 98765-4321",
		"ev.msg email 1 c@d.com",
		"users.email email 2 ana@gmail.com",
		"users.prefs email 1 zeca@empresa.com.br",
		"users.tags cpf 1 123.456.789-09",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("skipMasked=true:\n got %q\nwant %q", got, want)
	}

	// Sem pular as mascaradas (pgb audit numa base --from-container), o phone aparece.
	got = auditSummary(t, conn, cfg, false)
	want = append(want, "users.phone telefone 1 +55 11 98765-4321")
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("skipMasked=false:\n got %q\nwant %q", got, want)
	}

	if _, err := auditDB(ctx, conn, &Config{Audit: AuditConfig{Ignore: []string{"users.["}}}, true); err == nil {
		t.Error("audit.ignore com glob inválido deveria dar erro")
	}
}

// auditSummary: "coluna padrão linhas exemplo", ordenado.
func auditSummary(t *testing.T, conn *pgx.Conn, cfg *Config, skipMasked bool) []string {
	t.Helper()
	fs, err := auditDB(context.Background(), conn, cfg, skipMasked)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s %s %d %s", f.column, f.pattern, f.rows, f.sample))
	}
	sort.Strings(out)
	return out
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"ana.silva@gmail.com":    "an….com (19)",
		"  ana   silva\n@x.com ": "an….com (16)",
		"123.456.789-09":         "12…9-09 (14)",
		"(11) 9876":              "… (9)",
		"çãéíõüñàèì":             "çã…ñàèì (10)",
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	if s := redact("ana.silva@gmail.com"); strings.Contains(s, "silva") || strings.Contains(s, "gmail") {
		t.Errorf("redact vazou o dado: %q", s)
	}
}

func TestMatchIgnore(t *testing.T) {
	pats := []string{"crm.*.fone", "users.obs*", "tenants.sac_phone"}
	for col, want := range map[string]string{
		"crm.leads.fone":    "crm.*.fone",
		"users.observacao":  "users.obs*",
		"tenants.sac_phone": "tenants.sac_phone",
		"users.email":       "",
		"leads.fone":        "",
	} {
		if got := matchIgnore(pats, col); got != want {
			t.Errorf("matchIgnore(%q) = %q, want %q", col, got, want)
		}
	}
}
