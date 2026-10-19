package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// purge apaga tudo o que é do pgb (todo container e volume com o label pgb.kind): bases, versões,
// branches, pontos de salvamento e helpers que sobraram. Imagens e containers de fora não entram.
func purge(yes bool) error {
	bases, _ := listByLabel("volume", lKind+"=base")
	branches, _ := listByLabel("container", lKind+"=branch")
	saves, _ := listByLabel("volume", lKind+"=save")
	containers, err := docker("ps", "-aq", "--filter", "label="+lKind)
	if err != nil {
		return err
	}
	volumes, err := docker("volume", "ls", "-q", "--filter", "label="+lKind)
	if err != nil {
		return err
	}
	cs, vs := strings.Fields(containers), strings.Fields(volumes)
	if len(cs) == 0 && len(vs) == 0 {
		logf("nada do pgb para apagar")
		return nil
	}
	names := func(items []string, prefix string) string {
		var out []string
		for _, s := range items {
			out = append(out, strings.TrimPrefix(s, prefix))
		}
		return orDash(strings.Join(out, ", "))
	}
	logf("vai apagar %d containers e %d volumes:", len(cs), len(vs))
	logf("  bases:    %s", names(bases, "pgb-base-"))
	logf("  branches: %s", names(branches, "pgb-"))
	logf("  pontos:   %s", names(saves, "pgb-save-"))
	if !yes {
		if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return fmt.Errorf("sem terminal para confirmar: use --yes")
		}
		fmt.Fprint(os.Stderr, "digite 'apagar' para confirmar: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "apagar" {
			return fmt.Errorf("cancelado")
		}
	}
	// cleanup(): termina mesmo depois de um Ctrl+C. Containers antes, senão o volume está em uso.
	for _, c := range cs {
		cleanup("rm", "-f", "-v", c)
	}
	var left []string
	for _, v := range vs {
		cleanup("volume", "rm", "-f", v)
		if exists("volume", v) {
			left = append(left, v)
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("não consegui remover: %s", strings.Join(left, ", "))
	}
	logf("tudo apagado: %d containers, %d volumes", len(cs), len(vs))
	return nil
}
