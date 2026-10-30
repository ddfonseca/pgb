package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Testes de integração: um Postgres em Docker compartilhado, e cada teste no seu próprio banco.
// Sem Docker (ou com -short) eles são pulados; com PGB_TEST_DOCKER=1 (o CI) a falta de Docker
// é erro, para o CI não passar verde sem ter rodado nada.

// PGB_TEST_IMAGE troca a imagem (o CI roda Alpine e Debian: as ferramentas de shell diferem).
var testImage = cmp.Or(os.Getenv("PGB_TEST_IMAGE"), "postgres:17-alpine")

var testPG struct {
	once      sync.Once
	container string
	port      string
	err       error
}

func TestMain(m *testing.M) {
	removeOrphans()
	code := m.Run()
	if testPG.container != "" {
		exec.Command("docker", "rm", "-f", "-v", testPG.container).Run()
	}
	os.Exit(code)
}

// removeOrphans apaga os containers de teste de rodadas que morreram antes do TestMain limpar
// (Ctrl+C, timeout): cada um leva no label o pid do go test que o criou.
func removeOrphans() {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "label=pgbtest.pid",
		"--format", `{{.Names}} {{.Label "pgbtest.pid"}}`).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, pid, _ := strings.Cut(line, " ")
		n, err := strconv.Atoi(pid)
		if err != nil || n == os.Getpid() || syscall.Kill(n, 0) == nil {
			continue
		}
		exec.Command("docker", "rm", "-f", "-v", name).Run()
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integração (Docker): rode sem -short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("PGB_TEST_DOCKER") != "" {
			t.Fatalf("PGB_TEST_DOCKER=1, mas o Docker não responde: %v", err)
		}
		t.Skip("sem Docker")
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// startPostgres sobe um Postgres com a senha de sempre do pgb e devolve a porta no host.
func startPostgres(name string, env ...string) (string, error) {
	args := []string{"run", "-d", "--name", name, "-e", "POSTGRES_PASSWORD=" + localPassword, "-p", "127.0.0.1::5432",
		"--label", "pgbtest.pid=" + strconv.Itoa(os.Getpid())}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	if _, err := docker(append(args, testImage, "-c", "wal_level=logical")...); err != nil {
		return "", err
	}
	conn, err := connectLocal(context.Background(), name, "postgres", "postgres")
	if err != nil {
		return "", err
	}
	conn.Close(context.Background())
	return hostPort(name)
}

// testDB cria um banco novo e devolve a conexão e labels de branch que apontam para ele (o que
// shiftTime e createChangesSlot leem).
func testDB(t *testing.T) (*pgx.Conn, map[string]string) {
	t.Helper()
	requireDocker(t)
	testPG.once.Do(func() {
		testPG.container = "pgbtest-" + randHex(4)
		testPG.port, testPG.err = startPostgres(testPG.container)
	})
	if testPG.err != nil {
		t.Fatalf("subindo o Postgres de teste: %v", testPG.err)
	}
	name := "t_" + randHex(4)
	labels := map[string]string{lUser: "postgres", lPassword: localPassword, lDB: "postgres", lPort: testPG.port}
	admin := connectTo(t, labels)
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatal(err)
	}
	labels[lDB] = name
	return connectTo(t, labels), labels
}

func labelsURL(l map[string]string) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable", l[lUser], l[lPassword], l[lPort], l[lDB])
}

func connectTo(t *testing.T, l map[string]string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), labelsURL(l))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// mustExec roda SQL de preparação; erro aqui é bug do teste.
func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
