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

## Como funciona

- **Base**: volume Docker com um data dir *parado*. `--from-container` usa `pg_basebackup` (tar pelo
  stdout, com o WAL), sobe a base uma vez para aplicar o WAL e um `--mask arquivo.sql` opcional, e
  desliga limpo. Como ninguém escreve na base, todo clone dela é consistente.
- **Branch**: volume clonado da base por um container auxiliar com `cp --reflink`. Em btrfs/XFS o
  clone é copy-on-write (instantâneo e só ocupa o que mudar); nos volumes do Docker Desktop/WSL2
  (ext4 dentro da VM) cai para cópia cheia. Cada branch é um container na porta `127.0.0.1:<livre>`.
- **Estado**: só labels `pgb.*` nos volumes e containers do Docker; não há arquivo de estado.

## Próximo

- `base create --from-url` / `--cloudsql`: cópia lógica de um Postgres remoto (Cloud SQL), com o
  mascaramento feito na query de origem (`COPY (SELECT ...) TO STDOUT`), para PII não sair de prod.
