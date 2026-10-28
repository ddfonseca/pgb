# pgb

Branches descartáveis de Postgres em Docker. Funciona em Linux, macOS (Docker Desktop) e WSL.

**Uma branch nova fica pronta em cerca de um segundo**, qualquer que seja o tamanho do banco: o
clone é copy-on-write (`cp --reflink`), então nada é copiado até alguém escrever. Medido num
Linux com btrfs: clone em ~0,45 s e Postgres aceitando conexões em ~0,75 s; o comando inteiro
leva ~1,2 s. Em XFS é igual; em ext4 e nos volumes do Docker Desktop/WSL2 não há reflink e o
clone vira cópia cheia, proporcional ao tamanho da base.

## Instalação

Binários em [Releases](https://github.com/ddfonseca/pgb/releases) para Linux e macOS (amd64 e
arm64). No WSL, use o de Linux. Precisa do Docker rodando.

```sh
# troque linux_amd64 por linux_arm64, darwin_amd64 ou darwin_arm64
curl -L https://github.com/ddfonseca/pgb/releases/download/v0.1.0/pgb_0.1.0_linux_amd64.tar.gz | tar xz
sudo mv pgb_0.1.0_linux_amd64/pgb /usr/local/bin/
```

No macOS o binário não é assinado: na primeira vez, `xattr -d com.apple.quarantine /usr/local/bin/pgb`.
Ou compile (Go 1.27+): `git clone https://github.com/ddfonseca/pgb && cd pgb && go build -o pgb .`

## Uso

```sh
pgb base create dev --from-container myapp-postgres-1            # cópia consistente, sem parar a origem
pgb branch create pr-1 --ttl 1h                                  # imprime a DATABASE_URL
pgb branch exec ci-1 --create --rm -- npm test                   # cria, roda com DATABASE_URL, apaga
pgb branch list | url | start | reset | rm
pgb branch changes pr-1                                          # o que o app escreveu (DML), por transação
pgb gc                                                           # apaga branches com TTL vencido
pgb purge                                                        # apaga tudo do pgb (pede confirmação)
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

## Voltar no tempo (`--at`)

```sh
pgb branch create pr-1 --from prod --at base                 # a hora da cópia vira agora
pgb branch create inc --from prod --at "2026-10-18 14:32"    # a hora do incidente vira agora
```

Soma `agora - momento` em toda coluna `timestamp`/`timestamptz` (e `date`, em dias inteiros) da
branch, antes do slot de mudanças (não aparece no `changes`) e sem triggers. Regras de tempo
decorrido (sessão de 24h, nudge, janelas de N dias) agem como naquele momento; o que veio depois
dele fica no futuro. Não mexe em datas dentro de texto/JSON nem no relógio do app (horário do dia,
virada do dia). `time_shift.skip` no `pgb.yaml` (gravado na base) deixa tabelas de fora. O reset
refaz o deslocamento a partir da hora do reset; um ponto de salvamento traz o de quando foi salvo.

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
- Auditoria de PII antes do post-data: WhatsApp, telefone, e-mail ou CPF numa coluna de texto sem
  mask desfaz a base (dado de negócio vai em `audit.ignore`). `pgb audit <branch>` faz o mesmo sob
  demanda; numa base `--from-container` varre todas as colunas, porque o mask não foi aplicado.
- `after_copy`: comandos (`sh -c`, no diretório do `pgb.yaml`) rodados com `DATABASE_URL` na base em
  construção, depois dos índices e antes de congelar; o que gravarem vem em toda branch (ex.: o
  seed de um usuário de dev, já que a cópia não traz senhas). Um que falhe desfaz a base.
- Estimativa pelo planner antes de copiar: aborta acima de `max_size` (20GB) ou do espaço livre.
- `--cloudsql projeto:região:instância` (ou `source.cloudsql`): túnel pelo conector Go do Cloud SQL,
  com `source.impersonate` (a service account a impersonar). Testado com `--dry-run` no Cloud SQL de prod.

## Atualizar uma base (versões)

```sh
pgb base refresh prod             # refaz com a mesma receita como prod.20261020153000
pgb base refresh prod --keep 3    # mantém as 3 versões mais novas (padrão 2)
pgb branch reset pr-1             # a branch passa para a versão nova
```

A receita (origem, `pgb.yaml`, profile, `--set`, `--since`...) fica no label `pgb.recipe` da base,
sem a senha: com `--from-url`, o refresh pede `PGPASSWORD`. `--from prod` e o reset usam a versão
pronta mais nova; as branches abertas continuam na versão delas até o reset, e versões com
branches não são apagadas. Bases criadas antes do refresh existir precisam ser recriadas.

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

## Ensaio de migration

```sh
pgb migrate-test --from dev -- npm run db:migrate              # exit 2 se um lock travar escrita > 1s
pgb migrate-test --lock-threshold 500ms --no-diff -- ./migrar.sh
pgb migrate-test -- npm run db:migrate > schema.diff            # diff do schema no stdout
```

Roda o comando numa branch `mig-<hex>` (TTL 1h, apagada no fim, `--keep` mantém) e responde: a
migration pega locks que travam as escritas do app, quanto tempo leva e o que mudou no schema.
Uma conexão `pgb-monitor` olha o `pg_locks` a cada 100 ms e reporta os locks `Share`,
`ShareRowExclusive`, `Exclusive` e `AccessExclusive` em tabelas que já existiam (os das tabelas
que a migration cria não importam). O diff é `pg_dump -s` antes × depois, pelo `diff -u`.
Exit: o do comando se ele falhar; 2 se algum lock passou de `--lock-threshold`; senão 0. Os
tempos são locais: em prod as tabelas são maiores e o lock dura mais; locks < 100 ms podem escapar.

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

## Licença

[MIT](LICENSE)
