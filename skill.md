---
name: pgb
description: Postgres descartável para testar, depurar ou ensaiar migrations sem tocar no banco de verdade. Use quando precisar de um banco para rodar testes, reproduzir um bug com dados reais, ver o que um fluxo do app gravou, ou checar se uma migration trava escritas.
---

# pgb

`pgb` cria **branches**: Postgres descartáveis em Docker, clonados de uma **base** (uma cópia
parada e consistente de um banco). O clone é copy-on-write quando o disco permite (btrfs/XFS):
branch pronta em ~1 s, qualquer que seja o tamanho. Em ext4, Docker Desktop e WSL2 vira cópia
cheia, proporcional ao tamanho da base.

Uma branch é sua para estragar: escreva, rode migrations, apague tabelas. `reset` volta ao estado
da base. O banco de origem nunca é tocado depois da cópia.

Flags e detalhes de cada comando: `pgb <comando> --help`. O estado vive só em labels do Docker
(`pgb base list`, `pgb branch list`); não há arquivo de estado.

## Antes de começar

1. `pgb base list`: há uma base pronta? Use-a com `--from <base>` (opcional se só houver uma).
2. Sem base, pergunte ao usuário de onde copiar. Criar base de prod/staging (`--from-url`,
   `--cloudsql`, `source` do `pgb.yaml`) lê um banco remoto e pode levar minutos: é decisão dele.
   A partir de um Postgres local em Docker é barato:
   `pgb base create dev --from-container <container>` (não para a origem).

## Receitas

**Rodar testes num banco limpo**: cria, roda com `DATABASE_URL` (e `PGB_BRANCH`) no ambiente,
apaga no fim mesmo se falhar. O exit code é o do comando.

```sh
pgb branch exec ci-1 --create --rm -- npm test
```

**Branch para trabalhar por um tempo**: o stdout de `create` é só a URL, então dá para capturar.
Sempre com `--ttl`, para não deixar lixo se a sessão acabar; o progresso vai para o stderr.

```sh
export DATABASE_URL=$(pgb branch create pr-1 --ttl 2h)
pgb branch url pr-1          # a URL de novo, depois
pgb branch rm pr-1           # quando acabar
```

**O que esse fluxo gravou?**: aponte o app para a branch, reproduza o fluxo e leia o DML.

```sh
pgb branch changes pr-1 --consume     # INSERT/UPDATE/DELETE por transação; --consume marca como lido
pgb branch changes pr-1 --summary     # contagem por tabela × operação
pgb branch changes pr-1 --table 'order*' --full
```

Só DML aparece (migrations/DDL não). UPDATE mostra a linha nova inteira, sem os valores antigos;
DELETE só a chave. Use `--consume` entre tentativas para ver só o que a próxima gravou.

**Reproduzir um bug várias vezes**: salve o estado antes da ação problemática e volte a ele a
cada tentativa. Porta e URL não mudam; o ponto continua lá depois do restore.

```sh
pgb save create pr-1 antes-do-bug
# ... reproduz, muda o código ...
pgb save restore pr-1 antes-do-bug
```

**Bug que depende de tempo decorrido** (sessão de 24h, janelas de N dias): `--at` desloca todas
as datas da branch para que aquele momento vire agora.

```sh
pgb branch create inc --from prod --at "2026-10-18 14:32"   # ou --at base: a hora da cópia
```

Não mexe em datas dentro de texto/JSON nem no relógio do app.

**Ensaiar uma migration**: roda numa branch temporária e diz se algum lock travou escritas.

```sh
pgb migrate-test --from dev -- npm run db:migrate > schema.diff
```

Exit 2: um lock bloqueou escritas por mais de `--lock-threshold` (1s); o stderr diz qual tabela
e lock. Exit do comando se ele falhar; 0 se passou. O stdout é o diff do schema. Os tempos são
locais: em prod as tabelas são maiores e o lock dura mais, então trate exit 2 como problema real.

**Checar PII numa branch**: `pgb audit <branch>` falha se achar telefone, e-mail ou CPF em
colunas sem máscara.

## Regras

- Nomes: `a-z`, `0-9` e `-`, até 40 caracteres.
- Aponte o app e os testes para a URL da branch (`127.0.0.1:<porta>`), nunca para a origem.
- `pgb branch list` antes de criar: reaproveite a branch da tarefa em vez de abrir outra.
- Ao terminar, apague as branches que você criou (`branch rm`) ou deixe o TTL vencer (`pgb gc`).
- `pgb branch start <nome>` religa uma branch parada (ex.: depois de reiniciar a máquina).
- `pgb branch reset <nome>` descarta tudo e reclona da versão mais nova da base; mantém os pontos
  de salvamento.
- `pgb purge`, `pgb base rm` e `pgb base refresh` são do usuário: apagam ou recriam o que outros
  podem estar usando. Rode só se ele pedir.

## pgb.yaml (cópia lógica)

Só importa para `base create` sem `--from-container`. Diz, por tabela, `all`, `schema-only` ou
`{where: ...}`, as máscaras por coluna, `profiles` (ex.: um tenant só, com `-p tenant --set
tenant=acme`) e `after_copy` (seed rodado na base antes de congelar). `pgb base create <nome>
--dry-run` mostra o plano e o tamanho estimado sem copiar. A cópia não traz senhas; dado de
login de dev vem do `after_copy`.
