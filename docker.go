package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Tudo passa pela CLI `docker`: funciona igual no Linux, no Docker Desktop (macOS) e no WSL,
// sem depender do SDK nem de onde o daemon guarda os volumes.

// appCtx é cancelado no primeiro SIGINT/SIGTERM/SIGHUP (ver main). O trabalho normal usa ele;
// a limpeza usa cleanup(), que roda mesmo depois do Ctrl+C.
var appCtx = context.Background()

var errInterrupted = errors.New("interrompido")

func docker(args ...string) (string, error) {
	out, err := dockerIO(appCtx, nil, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// cleanup remove recursos ignorando erros e o cancelamento.
func cleanup(args ...string) { dockerIO(context.Background(), nil, nil, args...) }

// dockerIO roda docker com stdin/env opcionais e devolve o stdout (stderr vai no erro).
// No cancelamento, manda SIGINT para o cliente docker: `docker run` repassa o sinal ao container.
func dockerIO(ctx context.Context, stdin io.Reader, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	if env != nil {
		cmd.Env = env
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.Bytes(), errInterrupted
		}
		return out.Bytes(), fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func exists(kind, name string) bool {
	_, err := dockerIO(context.Background(), nil, nil, kind, "inspect", name)
	return err == nil
}

func isConflict(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "already in use") || strings.Contains(err.Error(), "Conflict"))
}

// Containers auxiliares (clone, pg_basebackup, pg_dump) levam o dono no label, para a limpeza
// achá-los mesmo se o pgb morrer no meio.
func helperArgs(owner string) []string {
	return []string{"--label", lKind + "=helper", "--label", lOwner + "=" + owner}
}

func cleanupHelpers(owner string) {
	out, _ := dockerIO(context.Background(), nil, nil, "ps", "-aq",
		"--filter", "label="+lKind+"=helper", "--filter", "label="+lOwner+"="+owner)
	for _, id := range strings.Fields(string(out)) {
		cleanup("rm", "-f", "-v", id)
	}
}

type containerInfo struct {
	Name   string
	Config struct {
		Image  string
		Env    []string
		Labels map[string]string
	}
	State struct {
		Status  string
		Running bool
	}
	Mounts []struct {
		Type        string
		Name        string
		Destination string
	}
}

func (c containerInfo) env(key, def string) string {
	for _, kv := range c.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return def
}

func inspectContainer(name string) (containerInfo, error) {
	var infos []containerInfo
	out, err := docker("container", "inspect", name)
	if err != nil {
		return containerInfo{}, err
	}
	if err := json.Unmarshal([]byte(out), &infos); err != nil || len(infos) == 0 {
		return containerInfo{}, fmt.Errorf("inspect %s: resposta inesperada", name)
	}
	infos[0].Name = strings.TrimPrefix(infos[0].Name, "/")
	return infos[0], nil
}

func volumeLabels(name string) (map[string]string, error) {
	out, err := docker("volume", "inspect", "-f", "{{json .Labels}}", name)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	return labels, json.Unmarshal([]byte(out), &labels)
}

// listByLabel devolve os nomes de volumes ou containers com um label.
func listByLabel(kind, label string) ([]string, error) {
	var out string
	var err error
	if kind == "volume" {
		out, err = docker("volume", "ls", "-q", "--filter", "label="+label)
	} else {
		out, err = docker("ps", "-a", "--format", "{{.Names}}", "--filter", "label="+label)
	}
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

func labelArgs(labels map[string]string) []string {
	var args []string
	for k, v := range labels {
		args = append(args, "--label", k+"="+v)
	}
	return args
}

// waitReady espera o Postgres do container aceitar conexões (inclui o tempo de recovery do WAL).
func waitReady(container, user, db string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := docker("exec", container, "pg_isready", "-q", "-U", user, "-d", db); err == nil {
			return nil
		} else if err == errInterrupted {
			return err
		}
		if running, _ := docker("inspect", "-f", "{{.State.Running}}", container); running != "true" {
			break // morreu: não adianta esperar o timeout
		}
		time.Sleep(300 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", "--tail", "20", container).CombinedOutput()
	return fmt.Errorf("postgres em %s não ficou pronto:\n%s", container, logs)
}

// hostPort devolve a porta do host publicada para o 5432 do container.
func hostPort(container string) (string, error) {
	out, err := docker("port", container, "5432/tcp")
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(out, "\n")
	i := strings.LastIndex(first, ":")
	if i < 0 {
		return "", fmt.Errorf("porta inesperada: %q", out)
	}
	return first[i+1:], nil
}

// freePort escolhe uma porta livre no loopback. Fica gravada no label, para a URL da branch
// não mudar a cada start/reset.
func freePort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port), nil
}
