package main

import (
	"fmt"
	"os"
	"regexp"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
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
	root.AddCommand(baseCmd(), branchCmd(), &cobra.Command{
		Use:   "gc",
		Short: "Apaga as branches com TTL vencido",
		RunE:  func(*cobra.Command, []string) error { return gc() },
	})
	return root
}

func baseCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "base", Short: "Cópias consistentes e paradas, de onde saem as branches"}

	var fromContainer, mask string
	create := &cobra.Command{
		Use:   "create <nome> --from-container <container>",
		Short: "Cria uma base a partir de um Postgres em Docker (sem pará-lo)",
		Args:  nameArg,
		RunE: func(_ *cobra.Command, args []string) error {
			if fromContainer == "" {
				return fmt.Errorf("informe --from-container")
			}
			return baseFromContainer(args[0], fromContainer, mask)
		},
	}
	create.Flags().StringVar(&fromContainer, "from-container", "", "container Postgres de origem")
	create.Flags().StringVar(&mask, "mask", "", "arquivo .sql de mascaramento, aplicado antes de congelar a base")

	var force bool
	rm := &cobra.Command{
		Use:   "rm <nome>",
		Short: "Apaga uma base",
		Args:  nameArg,
		RunE:  func(_ *cobra.Command, args []string) error { return baseRm(args[0], force) },
	}
	rm.Flags().BoolVar(&force, "force", false, "apaga também as branches dela")

	cmd.AddCommand(create, rm, &cobra.Command{
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
		RunE:  func(_ *cobra.Command, args []string) error { return branchCreate(args[0], from, ttl) },
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
		RunE: func(_ *cobra.Command, args []string) error {
			code, err := branchExec(args[0], exCreate, exFrom, exTTL, exRm, args[1:])
			if err != nil {
				return err
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
	cmd.AddCommand(create, exec,
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

func newTable(cols ...string) *table {
	t := &table{tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)}
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
