package main

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config é o pgb.yaml do projeto: de onde copiar, que tabelas/linhas trazer e como mascarar.
type Config struct {
	Image    string                       `yaml:"image"`
	Source   SourceConfig                 `yaml:"source"`
	Since    string                       `yaml:"since"`
	MaxSize  string                       `yaml:"max_size"`
	Tables   map[string]TableRule         `yaml:"tables"`
	Mask     map[string]map[string]string `yaml:"mask"`
	Presets  map[string]string            `yaml:"presets"`
	Profiles map[string]Profile           `yaml:"profiles"`
	Audit    AuditConfig                  `yaml:"audit"`
}

type SourceConfig struct {
	URL         string `yaml:"url"`
	EnvFile     string `yaml:"env_file"` // lê a URL (só usuário/senha/banco no caso do Cloud SQL) deste .env
	URLVar      string `yaml:"url_var"`
	CloudSQL    string `yaml:"cloudsql"`
	Impersonate string `yaml:"impersonate"`
}

// Profile sobrepõe tables (pela mesma chave) e exige parâmetros passados com --set.
// Scope filtra toda tabela que tenha a coluna (ex.: tenant_id = {{tenant}}), menos as que o
// profile cita pelo nome exato em tables.
type Profile struct {
	Description string               `yaml:"description"`
	Params      []string             `yaml:"params"`
	Scope       map[string]string    `yaml:"scope"`
	Tables      map[string]TableRule `yaml:"tables"`
}

// selection é o resultado de aplicar o profile: regras por tabela + scope.
type selection struct {
	rules    map[string]TableRule
	scope    map[string]string
	explicit map[string]bool // tabelas citadas pelo nome no profile: o scope não mexe nelas
}

// TableRule: "all", "schema-only" ou {where: "..."}.
type TableRule struct {
	Mode  string // all | schema-only | where
	Where string
}

func (r *TableRule) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value != "all" && n.Value != "schema-only" {
			return fmt.Errorf("linha %d: regra %q inválida (use all, schema-only ou {where: ...})", n.Line, n.Value)
		}
		r.Mode = n.Value
		return nil
	}
	var m struct {
		Where string `yaml:"where"`
	}
	if err := n.Decode(&m); err != nil || m.Where == "" {
		return fmt.Errorf("linha %d: esperado {where: ...}", n.Line)
	}
	r.Mode, r.Where = "where", m.Where
	return nil
}

func loadConfig(file string, required bool) (*Config, error) {
	cfg := &Config{}
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) && !required {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return cfg, nil
}

// selection aplica o profile por cima das tabelas base.
func (c *Config) selection(profile string, params map[string]string) (*selection, error) {
	sel := &selection{rules: map[string]TableRule{}, explicit: map[string]bool{}}
	for k, v := range c.Tables {
		sel.rules[k] = v
	}
	if profile == "" {
		return sel, nil
	}
	p, ok := c.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("profile %q não existe no pgb.yaml", profile)
	}
	for _, name := range p.Params {
		if _, ok := params[name]; !ok {
			return nil, fmt.Errorf("profile %q exige --set %s=...", profile, name)
		}
	}
	for k, v := range p.Tables {
		sel.rules[k] = v
		if !strings.ContainsAny(k, "*?[") {
			sel.explicit[k] = true
		}
	}
	sel.scope = p.Scope
	return sel, nil
}

// ruleFor: nome exato vence; entre globs, o padrão mais longo vence; sem regra = all.
// Tabelas fora do schema public casam pelo nome qualificado (schema.tabela).
func ruleFor(rules map[string]TableRule, name string) (TableRule, string) {
	if r, ok := rules[name]; ok {
		return r, name
	}
	best := ""
	for pat := range rules {
		if ok, _ := path.Match(pat, name); ok && len(pat) > len(best) {
			best = pat
		}
	}
	if best == "" {
		return TableRule{Mode: "all"}, ""
	}
	return rules[best], best
}

var placeholder = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// expand troca {{since}} por um timestamp e {{param}} por um literal SQL com aspas escapadas.
func expand(sql string, since time.Duration, params map[string]string) (string, error) {
	var bad error
	out := placeholder.ReplaceAllStringFunc(sql, func(m string) string {
		key := placeholder.FindStringSubmatch(m)[1]
		if key == "since" {
			if since <= 0 {
				bad = fmt.Errorf("%q usa {{since}}: passe --since ou defina since no pgb.yaml", sql)
			}
			return fmt.Sprintf("(now() - interval '%d seconds')", int64(since.Seconds()))
		}
		v, ok := params[key]
		if !ok {
			bad = fmt.Errorf("%q usa {{%s}}: passe --set %s=...", sql, key, key)
		}
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	})
	return out, bad
}

// parseSince aceita 30d, 2w, 12h, 90m.
func parseSince(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	n, err := strconv.Atoi(s[:len(s)-1])
	u, ok := unit[s[len(s)-1]]
	if err != nil || !ok || n <= 0 {
		return 0, fmt.Errorf("since inválido %q (ex.: 30d, 2w, 12h)", s)
	}
	return time.Duration(n) * u, nil
}

// parseSize aceita 500MB, 20GB, 1TB.
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	for _, u := range []struct {
		suf string
		mul int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if strings.HasSuffix(s, u.suf) {
			n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suf)), 64)
			if err != nil || n <= 0 {
				break
			}
			return int64(n * float64(u.mul)), nil
		}
	}
	return 0, fmt.Errorf("tamanho inválido %q (ex.: 500MB, 20GB)", s)
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// maskExpr: "null", "hash" ou uma expressão SQL onde {{col}} é a coluna e {{salt}} o sal da cópia.
// O sal é aleatório por cópia e descartado no fim: o mesmo valor vira o mesmo hash em todas as
// tabelas (joins continuam batendo), mas sem o sal não dá para reverter por força bruta
// (telefone tem poucos dígitos; md5 puro de telefone não anonimiza).
// Um nome de presets (pgb.yaml) também vale, para a mesma regra servir em várias tabelas.
func maskExpr(rule, col, salt string, presets map[string]string) string {
	if p, ok := presets[rule]; ok {
		rule = p
	}
	switch rule {
	case "null":
		return "NULL"
	case "hash":
		rule = "md5({{salt}} || {{col}}::text)"
	}
	return strings.NewReplacer("{{col}}", col, "{{salt}}", "'"+salt+"'").Replace(rule)
}
