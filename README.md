# pgb

Branches descartáveis de Postgres em Docker. Funciona em Linux, macOS (Docker Desktop) e WSL.

```sh
go build -o pgb .

pgb base create dev --from-container myapp-postgres-1   # cópia consistente, sem parar a origem
pgb branch create pr-1 --ttl 1h                                  # imprime a DATABASE_URL
pgb branch exec ci-1 --create --rm -- npm test                   # cria, roda com DATABASE_URL, apaga
pgb branch list | url | start | reset | rm
pgb gc                                                           # apaga branches com TTL vencido
```

## Pontos de salvamento

```sh
pgb save create pr-1 antes-do-bug     # guarda o estado da branch
pgb save restore pr-1 antes-do-bug    # volta a ele (o ponto continua lá: restaure quantas vezes quiser)
pgb save list [pr-1] | rm pr-1 antes-do-bug
```

Para reproduzir um bug: salva antes da ação problemática, muda o código, restaura,
repete. O ponto é um volume `pgb-save-<branch>_<ponto>` copiado como o clone (reflink ou cheia),
com a branch parada durante a cópia (consistência) e religada no fim se estava rodando; porta e URL
não mudam. O restore não é interrompível. `branch rm` apaga os pontos; `branch reset` os mantém.

## Cópia de prod (lógica, filtrada e mascarada na origem)

```sh
pgb base create prod --dry-run                       # plano + estimativa de tamanho, sem copiar
pgb base create prod                                 # usa source/tabelas/máscaras do pgb.yaml
pgb base create t1 -p tenant --set tenant=acme    # profile: só um tenant
pgb base create x --from-url postgres://... -c outro.yaml
```

O `pgb.yaml` diz, por tabela, `all`, `schema-only` ou `{where: ...}` (com `{{since}}` e parâmetros
`{{x}}` de `--set`), e as máscaras por coluna (`null`, `hash`, presets ou SQL com `{{col}}`/`{{salt}}`).
Exemplo completo: `examples/myapp.pgb.yaml`.

- Uma transação `REPEATABLE READ READ ONLY` na origem; o `pg_dump` do schema usa o mesmo snapshot.
- Dados por `COPY (SELECT <máscaras> FROM t WHERE <filtro>) TO STDOUT`: o que não foi pedido não sai.
- `hash` usa um sal aleatório por cópia (descartado): consistente entre tabelas, irreversível.
- Tabelas de extensão (ex.: `spatial_ref_sys`) vêm do `CREATE EXTENSION`; partições pela tabela-mãe.
- Antes das FKs, linhas que apontam para pais que não vieram são apagadas em cascata (`--strict` falha).
- Estimativa pelo planner antes de copiar: aborta acima de `max_size` (20GB) ou do espaço livre.
- `--cloudsql projeto:região:instância` (ou `source.cloudsql`): túnel pelo conector Go do Cloud SQL,
  com `source.impersonate` (a service account a impersonar). **Ainda não testado contra o Cloud SQL.**

## Como funciona

- **Base**: volume Docker com um data dir *parado*. `--from-container` usa `pg_basebackup -X stream`
  num container auxiliar na rede da origem, sobe a base uma vez para aplicar o WAL e um `--mask arquivo.sql` opcional, e
  desliga limpo. Como ninguém escreve na base, todo clone dela é consistente.
- **Branch**: volume clonado da base por um container auxiliar com `cp --reflink`. Em btrfs/XFS o
  clone é copy-on-write (instantâneo e só ocupa o que mudar); nos volumes do Docker Desktop/WSL2
  (ext4 dentro da VM) cai para cópia cheia. Cada branch é um container na porta `127.0.0.1:<livre>`.
- **Estado**: só labels `pgb.*` nos volumes e containers do Docker; não há arquivo de estado.
- **Robustez**: `docker create` do container (branch) ou do `pgb_tmp_<base>` é o lock; a base só
  aparece como pronta depois do volume `pgb-ready-<nome>`. Ctrl+C/SIGTERM desfazem o que estava
  sendo criado; `branch exec --rm` repassa o sinal ao comando (SIGKILL após 10 s) e apaga a branch.

