# lume

Banco de dados chave-valor escrito do zero em Go, no formato de árvore LSM (a mesma família do LevelDB, do RocksDB e do motor de armazenamento do Cassandra). Sem dependências além da biblioteca padrão.

O painel mostra o banco por dentro enquanto ele trabalha: a tabela em memória enchendo, os arquivos descendo de nível, o que a compactação jogou fora e o caminho exato que uma leitura percorreu.

## O que tem

- **Log de escrita antecipada** com checksum por registro: o que foi confirmado sobrevive à morte do processo.
- **Tabela em memória** como lista de saltos (skip list), ordenada.
- **Arquivos ordenados e imutáveis** (SSTables) em blocos de 4 KB com checksum, índice de blocos e filtro de Bloom.
- **Compactação em níveis** em segundo plano, que descarta valores sobrescritos e marcas de apagado.
- **Manifesto** trocado de forma atômica: depois de uma queda, o banco vê o conjunto antigo de arquivos ou o novo, nunca uma mistura.
- **Leitura por chave, varredura em ordem** e **escrita em lote**.
- **Rastro de leitura:** onde o banco procurou e o que cada lugar respondeu.
- **Queda simulada:** um botão mata o banco sem avisar e o reabre, para ver a recuperação.

## Como rodar

Requisitos: [Go 1.26+](https://go.dev/dl/) e [Node 22+](https://nodejs.org/).

```bat
run.bat
```

Abre em <http://127.0.0.1:6767>. Clique em **+50.000 novas** e veja a árvore se formar.

| Comando | O que faz |
| --- | --- |
| `run.bat` | Painel, com arquivos pequenos (96 KB) para a árvore se mexer rápido |
| `run.bat -Big` | Tamanhos de produção (tabelas de 4 MB) |
| `run.bat -Sync` | `fsync` a cada escrita |
| `run.bat -Reset` | Apaga os dados antes de iniciar |
| `run.bat -Dev` | Servidor + Vite com hot reload em <http://localhost:5183> |
| `run.bat -Test` | Testes do Go e build do frontend |
| `run.bat -Bench` | Mede escritas, leituras e varredura |

## Como usar no código

```go
db, err := lume.Open("dados", lume.Options{})
defer db.Close()

db.Put("usuario:42", []byte(`{"nome":"Ana"}`))
valor, err := db.Get("usuario:42")          // lume.ErrNotFound se não existe
db.Delete("usuario:42")

pares, _ := db.Scan("usuario:", "usuario;", 100)   // intervalo [de, até), em ordem

db.Batch([]lume.Op{{Key: "a", Value: []byte("1")}, {Key: "b", Delete: true}})
```

## Como funciona

### O caminho de uma escrita

```
Put("k", v)
   │ 1. acrescenta ao log em disco            (para sobreviver a uma queda)
   │ 2. insere na memtable                    (ordenada, em memória)
   ▼
memtable cheia ──► vira um arquivo do nível 0 ──► o log dela é apagado
                         │
        4 arquivos no nível 0 ──► fundidos com o nível 1
                                        │
              nível 1 acima do limite ──► um arquivo desce para o nível 2 …
```

Nenhum arquivo é alterado depois de escrito. Escrever é sempre acrescentar ao fim de um log e mexer na memória, o que é rápido; o custo é pago depois, em segundo plano, pela compactação.

### O arquivo (SSTable)

```
bloco de dados | bloco de dados | … | índice | filtro de Bloom | rodapé
```

- **Blocos** de cerca de 4 KB com as entradas em ordem de chave, cada um seguido de um CRC32. Um bit trocado no disco é detectado na leitura.
- **Índice:** a última chave de cada bloco. Achar uma chave é uma busca binária na memória e a leitura de **um** bloco.
- **Filtro de Bloom** (10 bits por chave, 7 funções): responde "esta chave com certeza não está aqui" sem ler o disco. Erra para o outro lado em menos de 1% das vezes.

### O caminho de uma leitura

Do dado mais novo para o mais antigo; o primeiro lugar que conhece a chave tem a resposta:

1. a memtable, e a memtable anterior se ela ainda está sendo gravada;
2. os arquivos do nível 0, do mais novo ao mais antigo (eles podem se sobrepor);
3. do nível 1 para baixo, no máximo **um** arquivo por nível (não se sobrepõem, então uma busca binária pelos intervalos acha o candidato).

Em cada arquivo, o filtro de Bloom é consultado antes de qualquer leitura de disco. O painel mostra esse caminho passo a passo.

### Apagar é escrever

`Delete` grava uma **marca de apagado**. Ela precisa existir: é o que esconde os valores antigos da chave que ainda estão em arquivos mais abaixo. A marca só é descartada quando a compactação a leva até o fundo, onde não resta nada mais antigo para esconder.

### Compactação

- **Nível 0 → 1:** os arquivos do nível 0 se sobrepõem, então todos descem juntos, fundidos com os arquivos do nível 1 que cobrem o mesmo intervalo de chaves.
- **Nível N → N+1:** quando o nível passa do limite (cada nível comporta 10 vezes o anterior), um arquivo é escolhido, em rodízio pelo espaço de chaves, e fundido com os arquivos do nível seguinte que ele sobrepõe.

A fusão lê várias fontes ordenadas como uma só. Quando a mesma chave aparece em mais de uma, vale a mais nova e as outras são descartadas. Essa mesma fusão é usada pela varredura.

### Recuperação

Ao abrir:

1. O **manifesto** diz quais arquivos compõem o banco. Arquivos que ele não conhece são restos de uma gravação ou compactação que não terminou, e são apagados.
2. Os **logs** que sobraram são reaplicados. Um log cujo fim foi cortado no meio de um registro (o processo morreu enquanto escrevia) é lido até o último registro íntegro, conferido pelo checksum.
3. O que estava só nos logs vira um arquivo do nível 0, e os logs são apagados.

A ordem das operações garante que nada se perde: o log de uma memtable só é apagado **depois** que o arquivo dela está no disco e no manifesto; os arquivos de entrada de uma compactação só são apagados **depois** que o manifesto aponta para os de saída.

## Números

No meu computador (SSD NVMe, Windows), com `run.bat -Bench`: 1 milhão de chaves de 115 bytes.

| Medida | Resultado |
| --- | --- |
| Escrita, uma por vez, sem `fsync` | ~185 mil/s |
| Escrita em lotes de 1.000 | ~450 mil/s |
| Escrita com `fsync` a cada uma | ~1,9 mil/s |
| Leitura aleatória de chave existente | ~83 mil/s |
| Leitura de chave que não existe | ~1,8 milhão/s |
| Varredura em ordem | ~1,1 milhão de pares/s |
| Amplificação de escrita | 2,8× |
| Reabrir o banco (113 MB em disco) | 18 ms |

A leitura de chave inexistente é rápida porque o filtro de Bloom evita o disco: em 200 mil tentativas dentro do intervalo de chaves, só cerca de 1.600 (0,8%) chegaram a ler um bloco à toa. Os números de leitura contam com o cache de arquivos do sistema operacional aquecido.

## Testes

```bat
run.bat -Test
```

10 testes:

- **contra um modelo:** 18 mil operações aleatórias comparadas com um `map`, atravessando fechamentos normais, quedas sem aviso e reaberturas, com leitura de todas as chaves e varredura completa a cada rodada;
- **log cortado:** o arquivo é truncado no meio do último registro; as 99 operações íntegras voltam, a cortada não;
- **bloco corrompido:** um bit trocado no arquivo é detectado pelo checksum;
- **filtro de Bloom:** menos de 2% de falsos positivos em 5 mil chaves ausentes;
- **compactação:** descarta o que foi sobrescrito e apagado, mantém os níveis sem sobreposição e não deixa arquivos órfãos;
- lista de saltos, ida e volta de um arquivo, fusão em que o mais novo vence, rastro de leitura, leitores e escritores concorrentes, lote e validação.

O CI roda com `go test -race`.

## Estrutura

```
wal.go          log de escrita antecipada
memtable.go     lista de saltos
sstable.go      formato do arquivo, filtro de Bloom, fusão de fontes ordenadas
db.go           abrir e recuperar, escrever, ler, varrer, manifesto
compaction.go   gravação da memtable e compactação em níveis
lume_test.go    testes
internal/server/  API do painel
cmd/lume/       servidor e benchmark
ui/             painel em React
```

Stack: Go 1.26 (só a biblioteca padrão), React 19, TypeScript 7, Vite 8, Tailwind CSS 4.

## Limites

- **Sem `fsync` por padrão.** Uma escrita confirmada sobrevive à morte do processo, mas não à queda de energia da máquina. Com `-Sync` sobrevive, e fica cerca de cem vezes mais lenta. Não há agrupamento de `fsync` entre escritores.
- **Sem cache de blocos.** Cada leitura que chega ao disco lê e confere o bloco de novo; o que salva é o cache de arquivos do sistema operacional.
- **Sem transações nem snapshots.** Um lote é aplicado junto, mas não há isolamento entre leitores e um lote em andamento, nem leitura de um ponto fixo no tempo.
- **Uma compactação por vez,** numa única goroutine. Sob escrita muito intensa, os escritores esperam a memtable anterior terminar de ser gravada.
- **Sem compressão** dos blocos.
- **Chaves comparadas como bytes.** Não há ordenação customizada.
- **As leituras seguram uma trava de leitura durante o acesso ao disco,** o que simplifica a troca de arquivos na compactação, mas faz uma compactação esperar leituras longas terminarem.
- **Não foi testado com injeção de falhas de disco** (escritas parciais no meio de um arquivo de tabela, disco cheio). As quedas simuladas matam o banco entre operações, não no meio de uma chamada de sistema.

## Licença

MIT. Veja [LICENSE](LICENSE).
