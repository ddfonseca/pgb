package main

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"os"
	"regexp"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func main() {
	// Ctrl+C / kill / fechar o terminal cancelam o appCtx em vez de matar o processo: os defers
	// de limpeza rodam e nada fica pela metade.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	appCtx = ctx
	err := rootCmd().Execute()
	interrupted := ctx.Err() != nil
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
	}
	switch {
	case interrupted:
		os.Exit(130)
	case err != nil:
		os.Exit(1)
	}
}

// Progresso vai para o stderr; o stdout fica só com a URL, para `$(pgb branch create x)` funcionar.
func logf(format string, a ...any) { fmt.Fprintf(os.Stderr, "pgb: "+format+"\n", a...) }

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func nameArg(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("informe um nome")
	}
	if !validName.MatchString(args[0]) {
		return fmt.Errorf("nome inválido %q: use a-z, 0-9 e '-', até 40 caracteres", args[0])
	}
	return nil
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "pgb",
		Short:         "Branches descartáveis de Postgres, em Docker, com copy-on-write quando o disco permite",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(baseCmd(), branchCmd(), saveCmd(), auditCmd(), &cobra.Command{
		Use:   "gc",
		Short: "Apaga as branches com TTL vencido",
		RunE:  func(*cobra.Command, []string) error { return gc() },
	})
	return root
}

func baseCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "base", Short: "Cópias consistentes e paradas, de onde saem as branches"}

	var fromContainer, mask string
	var ro remoteOpts
	create := &cobra.Command{
		Use:   "create <nome> (--from-container <c> | --from-url <url> | --cloudsql <instância> | source no pgb.yaml)",
		Short: "Cria uma base a partir de um Postgres em Docker, de uma URL ou do Cloud SQL",
		Long: `Com --from-container: cópia física (pg_basebackup) de um container rodando, sem pará-lo.
Com --from-url/--cloudsql/source do pgb.yaml: cópia lógica, com as regras de tabelas, filtros
(where) e máscaras do pgb.yaml aplicadas na origem. Use --dry-run para ver o plano e o tamanho.`,
		Args: nameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromContainer != "" {
				return baseFromContainer(args[0], args[0], fromContainer, mask)
			}
			ro.configSet = cmd.Flags().Changed("config")
			return baseFromRemote(args[0], args[0], ro)
		},
	}
	f := create.Flags()
	f.StringVar(&fromContainer, "from-container", "", "container Postgres de origem (cópia física)")
	f.StringVar(&mask, "mask", "", "com --from-container: .sql de mascaramento aplicado antes de congelar a base")
	f.StringVar(&ro.fromURL, "from-url", "", "URL do Postgres de origem (cópia lógica)")
	f.StringVar(&ro.cloudSQL, "cloudsql", "", "instância Cloud SQL (projeto:região:instância)")
	f.StringVarP(&ro.configFile, "config", "c", "pgb.yaml", "arquivo de configuração")
	f.StringVarP(&ro.profile, "profile", "p", "", "profile do pgb.yaml (ex.: tenant, thread)")
	f.StringToStringVar(&ro.params, "set", nil, "parâmetros do profile, ex.: --set tenant=abc")
	f.StringVar(&ro.since, "since", "", "janela para {{since}}, ex.: 30d (sobrepõe o pgb.yaml)")
	f.StringVar(&ro.maxSize, "max-size", "", "aborta se a estimativa passar disso (padrão 20GB)")
	f.StringVar(&ro.image, "image", "", "imagem do Postgres local (padrão: image do pgb.yaml ou postgres:16)")
	f.BoolVar(&ro.dryRun, "dry-run", false, "só mostra o plano e a estimativa de tamanho")
	f.BoolVar(&ro.strict, "strict", false, "falha em vez de apagar linhas órfãs de FKs")
	create.MarkFlagsMutuallyExclusive("from-container", "from-url")
	create.MarkFlagsMutuallyExclusive("from-container", "cloudsql")

	var force bool
	rm := &cobra.Command{
		Use:   "rm <nome>",
		Short: "Apaga uma base (ou uma versão dela)",
		Args:  baseRefArg,
		RunE:  func(_ *cobra.Command, args []string) error { return baseRm(args[0], force) },
	}
	rm.Flags().BoolVar(&force, "force", false, "apaga também as branches dela")

	var keep int
	var refreshDry bool
	refresh := &cobra.Command{
		Use:   "refresh <nome>",
		Short: "Recria a base com dados novos, como uma versão nova; as branches abertas continuam na antiga",
		Long: `Refaz a base do mesmo jeito que ela foi criada (mesma origem, pgb.yaml, profile e --set),
como <nome>.AAAAMMDDhhmmss. Branches novas e o reset passam a usar a versão nova; as abertas
continuam na antiga até o reset. Ficam as --keep versões mais novas, e as que ainda têm branches.`,
		Args: nameArg,
		RunE: func(_ *cobra.Command, args []string) error { return baseRefresh(args[0], keep, refreshDry) },
	}
	refresh.Flags().IntVar(&keep, "keep", 2, "quantas versões manter")
	refresh.Flags().BoolVar(&refreshDry, "dry-run", false, "só mostra o plano e a estimativa de tamanho")

	cmd.AddCommand(create, rm, refresh, &cobra.Command{
		Use:   "list",
		Short: "Lista as bases",
		RunE:  func(*cobra.Command, []string) error { return baseList() },
	})
	return cmd
}

func branchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "branch", Short: "Postgres descartáveis clonados de uma base"}

	var from string
	var ttl time.Duration
	create := &cobra.Command{
		Use:   "create <nome>",
		Short: "Clona a base e sobe um Postgres; imprime a DATABASE_URL",
		Args:  nameArg,
		RunE: func(_ *cobra.Command, args []string) error {
			if ttl < 0 {
				return fmt.Errorf("--ttl não pode ser negativo")
			}
			return branchCreate(args[0], from, ttl)
		},
	}
	create.Flags().StringVar(&from, "from", "", "base de origem (opcional se só houver uma)")
	create.Flags().DurationVar(&ttl, "ttl", 0, "validade, ex.: 1h; `pgb gc` apaga as vencidas")

	var exCreate, exRm bool
	var exFrom string
	var exTTL time.Duration
	exec := &cobra.Command{
		Use:   "exec <nome> [--create] [--rm] -- <comando...>",
		Short: "Roda um comando com DATABASE_URL apontando para a branch",
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
				return fmt.Errorf("uso: pgb branch exec <nome> -- <comando...>")
			}
			return nameArg(cmd, args[:1])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !exCreate && (cmd.Flags().Changed("from") || cmd.Flags().Changed("ttl")) {
				return fmt.Errorf("--from e --ttl só valem com --create")
			}
			if exTTL < 0 {
				return fmt.Errorf("--ttl não pode ser negativo")
			}
			code, err := branchExec(args[0], exCreate, exFrom, exTTL, exRm, args[1:])
			if err != nil {
				fmt.Fprintln(os.Stderr, "erro:", err)
			}
			os.Exit(code)
			return nil
		},
	}
	exec.Flags().BoolVar(&exCreate, "create", false, "cria a branch antes")
	exec.Flags().StringVar(&exFrom, "from", "", "base de origem, com --create")
	exec.Flags().DurationVar(&exTTL, "ttl", 0, "validade, com --create")
	exec.Flags().BoolVar(&exRm, "rm", false, "apaga a branch no fim, mesmo se o comando falhar")

	one := func(use, short string, fn func(string) error) *cobra.Command {
		return &cobra.Command{Use: use + " <nome>", Short: short, Args: nameArg,
			RunE: func(_ *cobra.Command, args []string) error { return fn(args[0]) }}
	}
	cmd.AddCommand(create, exec, changesCmd(),
		&cobra.Command{Use: "list", Short: "Lista as branches",
			RunE: func(*cobra.Command, []string) error { return branchList() }},
		one("url", "Imprime a DATABASE_URL da branch", func(n string) error {
			u, err := branchURL(n)
			if err == nil {
				fmt.Println(u)
			}
			return err
		}),
		one("start", "Religa uma branch parada (ex.: depois de reiniciar a máquina)", branchStart),
		one("reset", "Descarta as mudanças e reclona da base", branchReset),
		one("rm", "Apaga a branch", branchRm),
	)
	return cmd
}

type table struct{ w *tabwriter.Writer }

func newTable(cols ...string) *table { return newTableTo(os.Stdout, cols...) }

func newTableTo(w io.Writer, cols ...string) *table {
	t := &table{tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)}
	t.row(cols...)
	return t
}

func (t *table) row(cols ...string) {
	for i, c := range cols {
		if i > 0 {
			fmt.Fprint(t.w, "\t")
		}
		fmt.Fprint(t.w, c)
	}
	fmt.Fprintln(t.w)
}

func (t *table) flush() { t.w.Flush() }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
