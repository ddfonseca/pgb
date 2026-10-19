package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Pontos de salvamento: o estado de uma branch guardado à parte, para voltar a ele quantas vezes
// quiser (ex.: reproduzir um bug de prod, salvar antes da ação problemática, mudar o código,
// restaurar, repetir).
//
//   - volume pgb-save-<branch>_<ponto>: `_` não é válido em nomes, então não colide. Não leva
//     pgb.base, senão branchesOf() o tomaria por uma branch;
//   - copiar um data dir rodando não é consistente: a branch é parada (fast shutdown) durante a
//     cópia e religada no fim se estava rodando. A cópia é a do clone (reflink ou cheia);
//   - container pgb_lock_<branch> (`docker create`, nome único no daemon) é o lock: uma operação
//     de save por branch de cada vez. É um helper da branch, então o `branch rm` limpa um que sobrou.

func saveVolume(branch, point string) string { return "pgb-save-" + branch + "_" + point }
func saveLock(branch string) string          { return "pgb_lock_" + branch }

// restoreScript esvazia o volume da branch e copia o ponto. Ignora INT/TERM/HUP: um PGDATA pela
// metade fica corrompido, então a cópia vai até o fim.
const restoreScript = `trap '' INT TERM HUP; find /dst -mindepth 1 -delete && ` + cloneScript

// lockBranch pega o lock da branch e devolve o container dela (inspecionado já com o lock) e o unlock.
func lockBranch(name string) (containerInfo, func(), error) {
	info, err := inspectContainer(branchContainer(name))
	if err != nil {
		return info, nil, fmt.Errorf("branch %q não existe", name)
	}
	// Sem cancelamento: um create interrompido não diz se o lock ficou criado ou não.
	args := append([]string{"create", "--name", saveLock(name)}, helperArgs("branch:"+name)...)
	if _, err := dockerIO(context.Background(), nil, nil, append(args, info.Config.Image, "true")...); err != nil {
		if isConflict(err) {
			return info, nil, fmt.Errorf("branch %q tem outra operação de save em andamento (se sobrou de um processo morto: docker rm -v %s)",
				name, saveLock(name))
		}
		return info, nil, err
	}
	unlock := func() { cleanup("rm", "-f", "-v", saveLock(name)) }
	if info, err = inspectContainer(branchContainer(name)); err != nil {
		unlock()
		if err != errInterrupted {
			err = fmt.Errorf("branch %q não existe", name)
		}
		return info, nil, err
	}
	return info, unlock, nil
}

// stopBranch para a branch se estiver rodando e devolve a função que a religa (nada, se estava
// parada). Os dois sem cancelamento: um stop interrompido deixaria o daemon parando o container
// depois do nosso start, e a branch tem que voltar mesmo depois de um Ctrl+C.
func stopBranch(info containerInfo) (func() error, error) {
	if !info.State.Running {
		return func() error { return nil }, nil
	}
	resume := func() error {
		if _, err := dockerIO(context.Background(), nil, nil, "start", info.Name); err != nil {
			return fmt.Errorf("religando a branch: %w", err)
		}
		if appCtx.Err() != nil {
			logf("branch religada (sem esperar ficar pronta)")
			return nil
		}
		l := info.Config.Labels
		return waitReady(info.Name, l[lUser], l[lDB], time.Minute)
	}
	// A imagem oficial usa STOPSIGNAL SIGINT (fast shutdown com checkpoint).
	if _, err := dockerIO(context.Background(), nil, nil, "stop", "-t", "60", info.Name); err != nil {
		return resume, err
	}
	if appCtx.Err() != nil {
		return resume, errInterrupted
	}
	return resume, nil
}

// copyVolume copia src para dst num container auxiliar da branch e devolve o que o script imprimiu.
func copyVolume(ctx context.Context, branch, image, src, dst, script string) (string, error) {
	args := append([]string{"run", "--rm"}, helperArgs("branch:"+branch)...)
	args = append(args, "-v", src+":/src:ro", "-v", dst+":/dst", image, "sh", "-c", script)
	out, err := dockerIO(ctx, nil, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// cleanupCopy remove os helpers da branch menos o lock: ele só sai no fim, pelo unlock.
func cleanupCopy(branch string) {
	out, _ := dockerIO(context.Background(), nil, nil, "ps", "-a", "--format", "{{.Names}}",
		"--filter", "label="+lKind+"=helper", "--filter", "label="+lOwner+"=branch:"+branch)
	for _, n := range strings.Fields(string(out)) {
		if n != saveLock(branch) {
			cleanup("rm", "-f", "-v", n)
		}
	}
}

var copyHow = map[string]string{"cow": "copy-on-write", "full": "cópia cheia (fs sem reflink)"}

// saveCreate: se falhar ou for interrompido, apaga o ponto pela metade e religa a branch se ela
// estava rodando. Ordem dos defers: religa, desfaz o ponto, solta o lock.
func saveCreate(branch, point string) (err error) {
	info, unlock, err := lockBranch(branch)
	if err != nil {
		return err
	}
	defer unlock()
	vol := saveVolume(branch, point)
	// Só depois do lock: `docker volume create` de um nome que já existe não dá erro.
	if exists("volume", vol) {
		return fmt.Errorf("ponto %q da branch %q já existe (pgb save rm %s %s)", point, branch, branch, point)
	}
	ok := false
	defer func() {
		if !ok {
			cleanupCopy(branch)
			cleanup("volume", "rm", "-f", vol)
			logf("ponto %q desfeito", point)
		}
	}()
	labels := map[string]string{lKind: "save", lBranch: branch, lName: point,
		lCreated: time.Now().UTC().Format(time.RFC3339)}
	if _, err := docker(append([]string{"volume", "create"}, append(labelArgs(labels), vol)...)...); err != nil {
		return err
	}
	resume, err := stopBranch(info)
	defer func() {
		if rerr := resume(); rerr != nil {
			if err == nil {
				err = rerr
			} else {
				logf("%v", rerr)
			}
		}
	}()
	if err != nil {
		return err
	}
	start := time.Now()
	mode, err := copyVolume(appCtx, branch, info.Config.Image, branchVolume(branch), vol, cloneScript)
	if err != nil {
		return fmt.Errorf("salvando: %w", err)
	}
	ok = true
	logf("ponto %q da branch %q salvo: %s em %s", point, branch, copyHow[mode], time.Since(start).Round(time.Millisecond))
	return nil
}

// saveRestore volta a branch ao ponto (o ponto continua lá). A cópia não é interrompível.
func saveRestore(branch, point string) error {
	info, unlock, err := lockBranch(branch)
	if err != nil {
		return err
	}
	defer unlock()
	vol := saveVolume(branch, point)
	if l, err := volumeLabels(vol); err != nil || l[lKind] != "save" || l[lBranch] != branch {
		return fmt.Errorf("ponto %q da branch %q não existe", point, branch)
	}
	resume, err := stopBranch(info)
	if err != nil {
		if rerr := resume(); rerr != nil {
			logf("%v", rerr)
		}
		return err
	}
	logf("restaurando o ponto %q na branch %q: não interrompa (um data dir pela metade fica corrompido)", point, branch)
	start := time.Now()
	mode, err := copyVolume(context.Background(), branch, info.Config.Image, vol, branchVolume(branch), restoreScript)
	if err != nil {
		cleanupCopy(branch)
		return fmt.Errorf("restaurando: %w; a branch ficou parada e inconsistente: rode o restore de novo ou pgb branch reset %s",
			err, branch)
	}
	logf("ponto %q restaurado: %s em %s", point, copyHow[mode], time.Since(start).Round(time.Millisecond))
	if err := resume(); err != nil {
		return err
	}
	if !info.State.Running {
		logf("a branch estava parada e continua parada (pgb branch start %s)", branch)
		return nil
	}
	if appCtx.Err() != nil {
		return nil
	}
	u, err := branchURL(branch)
	if err != nil {
		return err
	}
	fmt.Println(u)
	return nil
}

// saveVolumes devolve os volumes de ponto de uma branch.
func saveVolumes(branch string) []string {
	out, _ := docker("volume", "ls", "-q", "--filter", "label="+lKind+"=save", "--filter", "label="+lBranch+"="+branch)
	return strings.Fields(out)
}

// removeSaves apaga todos os pontos de uma branch (branch rm). Usa cleanup(): roda depois de um Ctrl+C.
func removeSaves(branch string) error {
	for _, v := range saveVolumes(branch) {
		cleanup("volume", "rm", "-f", v)
		if exists("volume", v) {
			return fmt.Errorf("não consegui remover o volume %s", v)
		}
	}
	return nil
}

func saveRm(branch, point string) error {
	vol := saveVolume(branch, point)
	if l, err := volumeLabels(vol); err != nil || l[lKind] != "save" {
		return fmt.Errorf("ponto %q da branch %q não existe", point, branch)
	}
	// Com a branch viva, pega o lock para não apagar um ponto que está sendo criado ou restaurado.
	if exists("container", branchContainer(branch)) {
		_, unlock, err := lockBranch(branch)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if _, err := docker("volume", "rm", vol); err != nil {
		return err
	}
	logf("ponto %q da branch %q removido", point, branch)
	return nil
}

func saveList(branch string) error {
	args := []string{"volume", "ls", "--filter", "label=" + lKind + "=save",
		"--format", fmt.Sprintf("{{.Label %q}}\t{{.Label %q}}\t{{.Label %q}}", lBranch, lName, lCreated)}
	if branch != "" {
		args = append(args, "--filter", "label="+lBranch+"="+branch)
	}
	out, err := docker(args...)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 {
			rows = append(rows, f)
		}
	}
	// Por branch e, dentro dela, na ordem em que foram criados.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i][0] != rows[j][0] {
			return rows[i][0] < rows[j][0]
		}
		return rows[i][2] < rows[j][2]
	})
	t := newTable("BRANCH", "PONTO", "CRIADO")
	for _, r := range rows {
		t.row(r...)
	}
	t.flush()
	return nil
}

// saveCounts conta os pontos por branch (coluna PONTOS do `branch list`).
func saveCounts() map[string]int {
	out, _ := docker("volume", "ls", "--filter", "label="+lKind+"=save", "--format", fmt.Sprintf("{{.Label %q}}", lBranch))
	n := map[string]int{}
	for _, b := range strings.Fields(out) {
		n[b]++
	}
	return n
}

// branchPointArgs valida <branch> <ponto>: os dois com as regras de nome.
func branchPointArgs(cmd *cobra.Command, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("informe a branch e o ponto")
	}
	for _, a := range args {
		if err := nameArg(cmd, []string{a}); err != nil {
			return err
		}
	}
	return nil
}

func saveCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "save", Short: "Pontos de salvamento de uma branch, para voltar a eles quantas vezes quiser"}
	two := func(use, short string, fn func(string, string) error) *cobra.Command {
		return &cobra.Command{Use: use + " <branch> <ponto>", Short: short, Args: branchPointArgs,
			RunE: func(_ *cobra.Command, args []string) error { return fn(args[0], args[1]) }}
	}
	cmd.AddCommand(
		two("create", "Salva o estado atual da branch (ela para durante a cópia)", saveCreate),
		two("restore", "Volta a branch ao ponto; o ponto continua lá para restaurar de novo", saveRestore),
		&cobra.Command{Use: "list [branch]", Short: "Lista os pontos de salvamento",
			Args: func(cmd *cobra.Command, args []string) error {
				if len(args) == 0 {
					return nil
				}
				return nameArg(cmd, args)
			},
			RunE: func(_ *cobra.Command, args []string) error {
				if len(args) == 0 {
					return saveList("")
				}
				return saveList(args[0])
			}},
		two("rm", "Apaga um ponto de salvamento", saveRm),
	)
	return cmd
}
