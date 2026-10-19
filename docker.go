package main

import (
	"bytes"
	"encoding/json"
	"io"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Tudo passa pela CLI `docker`: funciona igual no Linux, no Docker Desktop (macOS) e no WSL,
// sem depender do SDK nem de onde o daemon guarda os volumes.

func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// dockerPipe roda `docker src... | docker dst...`.
func dockerPipe(src, dst []string) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	a := exec.Command("docker", src...)
	b := exec.Command("docker", dst...)
	var aerr, berr bytes.Buffer
	a.Stdout, a.Stderr = w, &aerr
	b.Stdin, b.Stderr = r, &berr
	if err := a.Start(); err != nil {
		return err
	}
	if err := b.Start(); err != nil {
		a.Process.Kill()
		return err
	}
	// O pai fecha as pontas dele; senão o leitor nunca vê EOF.
	w.Close()
	r.Close()
	errA, errB := a.Wait(), b.Wait()
	if errA != nil {
		return fmt.Errorf("origem: %v: %s", errA, strings.TrimSpace(aerr.String()))
	}
	if errB != nil {
		return fmt.Errorf("destino: %v: %s", errB, strings.TrimSpace(berr.String()))
	}
	return nil
}

func exists(kind, name string) bool {
	_, err := docker(kind, "inspect", name)
	return err == nil
}

type containerInfo struct {
	Name   string
	Config struct {
		Image  string
		Env    []string
		Labels map[string]string
	}
	State struct {
		Status string
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

// listByLabel devolve os nomes de volumes ou containers com um label pgb.kind.
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
		}
		time.Sleep(300 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", "--tail", "20", container).CombinedOutput()
	return fmt.Errorf("postgres em %s não ficou pronto em %s:\n%s", container, timeout, logs)
}

// hostPort devolve a porta do host que o Docker escolheu para o 5432 do container.
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

// dockerIO roda docker com stdin/env opcionais e devolve o stdout (stderr vai no erro).
func dockerIO(stdin io.Reader, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	if env != nil {
		cmd.Env = env
	}
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
