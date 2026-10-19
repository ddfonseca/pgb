# pgb

Branches descartáveis de Postgres em Docker. Funciona em Linux, macOS (Docker Desktop) e WSL.

```sh
go build -o pgb .

pgb base create dev --from-container myapp-postgres-1   # cópia consistente, sem parar a origem
pgb branch create pr-1 --ttl 1h                                  # imprime a DATABASE_URL
pgb branch exec ci-1 --create --rm -- npm test                   # cria, roda com DATABASE_URL, apaga
pgb branch list | url | start | reset | rm
pgb branch changes pr-1                                          # o que o app escreveu (DML), por transação
pgb gc                                                           # apaga branches com TTL vencido
```

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

## O que o app escreveu na branch

```sh
pgb branch changes pr-1                  # todo INSERT/UPDATE/DELETE desde a criação, por transação
pgb branch changes pr-1 --consume        # idem, e a próxima chamada mostra só o que vier depois
pgb branch changes pr-1 --summary        # contagem por tabela × operação
pgb branch changes pr-1 --table 'user*'  # glob no nome (fora do public: schema.tabela); --full não trunca
```

Para responder "o que esse fluxo gravou?": suba o app apontando para a branch,
reproduza o fluxo e rode `changes`. Saída:

```
── xid 40466  2026-10-19 03:50:25.138  (2)
  INSERT orders  id=1042 customer_id=42 status='pending' ...
  UPDATE orders  id=1042 customer_id=42 status='paid' ...
```

- Toda branch nasce com `wal_level = logical` e o slot `pgb_changes` (plugin `test_decoding`, que vem
  com o Postgres). Branches criadas antes disso não têm: recrie, ou `pgb branch reset` (que recria).
- Só DML: DDL (migrations, `create table`) não aparece. `TRUNCATE` aparece.
- UPDATE mostra a linha nova inteira (o Postgres não manda os valores antigos); `[antes: ...]` só
  quando a chave primária mudou. DELETE mostra só a chave (ou nada, em tabela sem PK).
- Valor grande (TOAST) que o UPDATE não mexeu aparece como `unchanged-toast-datum`.
- O slot segura o WAL até alguém consumir, com teto de 2GB (`max_slot_wal_keep_size`): passou disso,
  o Postgres invalida o slot para proteger o disco (a branch continua normal). `--consume` libera.

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

