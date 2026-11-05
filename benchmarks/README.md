# Бенчмарки и нагрузочные тесты

## 1. Go-бенчмарки (основное)

Живут в `*_test.go` — компилируются только `go test`, в бинарь `wasmgate` не
попадают. Разбиты по областям: микро (цена одной функции) и e2e (реальный
`Server` за loopback TCP).

| Бенчмарк | Где | Что проверяет | На что смотреть |
|---|---|---|---|
| `BenchmarkThroughput/{direct,zero_copy,idle_watchdog}/{4KiB,64KiB,1MiB}` | `internal/proxy` (`copy_test.go`) | пропускную способность: базовая линия без прокси (`direct`) против zero-copy и buffered watchdog | `MB/s`, `allocs/op` |
| `BenchmarkThroughputScaling/conns={1,4,16,64}` | `internal/proxy` (`copy_test.go`) | масштабирование по числу одновременных соединений | рост `MB/s` |
| `BenchmarkRelayLatency/{direct,through_proxy}` | `internal/proxy` (`copy_test.go`) | задержку одного запроса (RTT, 1 байт туда-обратно) на тёплом соединении, с базовой линией без прокси | `ns/op` |
| `BenchmarkConnectionSetup/{direct,through_proxy}` | `internal/proxy` (`server_test.go`) | скорость установления соединений (conn/s), с базовой линией без прокси | `ns/op` |
| `BenchmarkBalancer/{round_robin,least_conn}/servers={1,8,64}` | `internal/proxy` (`upstream_test.go`) | цену выбора бэкенда и её рост с размером пула (`least_conn` сканирует весь пул) | `ns/op`, `allocs/op` |
| `BenchmarkBalancer/baseline_inline/servers=64` | `internal/proxy` (`upstream_test.go`) | эталон без интерфейса `Balancer`: та же работа с `atomic.Uint64` и индексом | `ns/op` |
| `BenchmarkBalancerParallel/{round_robin,least_conn}` | `internal/proxy` (`upstream_test.go`) | выбор бэкенда, когда все P бьют в один пул (общие счётчики) | `ns/op` |
| `BenchmarkConnLimiter/{unlimited,below_limit,at_limit}` | `internal/proxy` (`state_test.go`) | цену `max_conns` на трёх путях: без лимита, с запасом, на пределе (флуд) | `ns/op` |
| `BenchmarkConnLimiterParallel/{unlimited,below_limit,at_limit}` | `internal/proxy` (`state_test.go`) | то же под конкуренцией: оба счётчика на одной кэш-линии | `ns/op` |
| `BenchmarkConnHotPath` | `internal/proxy` (`proxy_test.go`) | всю CPU-работу на соединение (snapshot + listener + limiter + балансировщик) | `ns/op` |
| `BenchmarkConnHotPathParallel` | `internal/proxy` (`proxy_test.go`) | то же, когда все P делят один `Runtime` (snapshot, лимитер, курсор пула) | `ns/op` |
| `BenchmarkRuntimeLoad` | `internal/proxy` (`snapshot_test.go`) | чтение snapshot на соединение | `ns/op` |
| `BenchmarkReload` | `internal/proxy` (`snapshot_test.go`) | холодный путь reload: сборка нового snapshot + публикация | `ns/op` |
| `BenchmarkLoad`, `BenchmarkValidate` | `internal/config` (`config_test.go`) | стоимость hot reload (SIGHUP) | `ns/op` |
| `BenchmarkLoadInvalid/{unknown_field,duplicate_addr,bad_listener_addr,error_in_last_listener}` | `internal/config` (`config_test.go`) | путь **отказа** reload: битый YAML, битый адрес, ошибка в последнем листенере большого файла | `ns/op` |
| `BenchmarkLimiterAllow`, `BenchmarkLimiterAllowManyKeys` | `internal/observe` (`limiter_test.go`) | лог-лимитер на пути ошибок | `ns/op`, `allocs/op` |
| `BenchmarkLimiterAllowParallel/{shared_key,distinct_keys}` | `internal/observe` (`limiter_test.go`) | лог-лимитер, когда его дёргают все P сразу (один ключ против своих у каждого) | `ns/op` |
| `BenchmarkMetricsConnection`, `BenchmarkMetricsConnectionParallel` | `internal/observe` (`metrics_test.go`) | всю учётную работу метрик на соединение (Accept + AddBytes ×2 + Close) | `ns/op`, `allocs/op` |
| `BenchmarkAllow`, `BenchmarkAllowParallel` | `internal/ratelimit` (`window_test.go`) | проверку скользящего окна по IP/подсети (один ключ в один поток против всех P по разным IP) | `ns/op`, `allocs/op` |
| `BenchmarkOnAccept`, `BenchmarkOnAcceptParallel` | `internal/plugin` (`sandbox_test.go`) | вызов WASM-хука `on_accept` через пул инстансов `wazero` и `CallWithStack` | `ns/op`, `allocs/op` |

> Подпапок `bench/` нет. Тесты и бенчи лежат вместе, в `X_test.go` того исходника,
> который покрывают (`copy.go` → `copy_test.go`), тесты сверху, бенчи ниже — так
> делает и стандартная библиотека Go (`sync/mutex_test.go`,
> `net/http/serve_test.go`).
>
> Бенч, который меряет неэкспортируемое (`connLimiter`, `*upstream`,
> `resolvedListener`, `Snapshot.listeners`), в отдельный пакет не вынести: подпапка
> — это отдельный пакет, и он видит только экспортированные символы.
> `export_test.go` тоже не помогает: файлы `_test.go` пакета попадают лишь в его
> собственный тестовый бинарник. Цена выноса — расширение публичного API ради
> бенчей.

### Что считается нормой

- fast path (балансировщик, лимитер, snapshot, лог-лимитер) — **`0 allocs/op`** и
  единицы–десятки `ns/op` в steady state;
- пропускная способность должна **расти** с числом соединений и с размером чанка
  (на 4C/8T лэптопе точка `conns=64` шумит — там уже переподписка; смотри тренд,
  а не одну точку, и профиль, а не догадки про мьютекс);
- `RelayLatency` — задержка на тёплом соединении без dial/close, «чистый» RTT прокси;
- `ConnectionSetup` даёт потолок по conn/s для сравнения с будущим rate-limit'ом;
- `Reload` должен оставаться в микросекундах (холодный путь, но он на SIGHUP);
- `least_conn` растёт с размером пула, `round_robin` — плоский; если линейный скан
  начнёт мешать, пора заводить heap;
- `ConnLimiter/at_limit` (флуд) должен быть **дешевле** `below_limit`: отказ — это
  один load, без CAS-цикла.
- Парные бенчи (`*Parallel`) дороже одиночных — это норма: общая кэш-линия стоит
  передачи между ядрами. Смотреть на **характер** роста: read-only-путь
  (`at_limit`, `least_conn`) обязан масштабироваться, а если цена не зависит от
  ключей (лог-лимитер: 116 ns на одном ключе и 124 ns при своём у каждого) — узкое
  место один общий мьютекс, и это надо называть явно, а не прятать за именем бенча.
- `LoadInvalid` не должен быть дороже `Load`: отказ конфига тоже обязан быть дешёвым.

### Почему цифры такие

Короткие пояснения к тому, что иначе выглядит контринтуитивно (числа — Ryzen 5
3550H, 8 логических CPU, Windows; Linux-прогон см. §«Платформа»):

| Наблюдение | Причина |
|---|---|
| `ConnLimiter/at_limit` (1.2 ns) **в 12 раз дешевле** `below_limit` (15 ns) | приём — это `Load` + `CAS` в `acquire` и ещё `CAS` в `release`; отказ — два обычных `Load` и сравнение. Все `b.N` итераций идут в reject-ветку (бенч падает, если `acquire` хоть раз вернёт `true`) |
| `at_limit`/`unlimited` в параллельном бенче **дешевле**, чем в одиночном | обе ветки только читают счётчики: линия не инвалидируется, у каждого ядра своя копия, а `ns/op` делится на число горутин. `below_limit` там же дорожает 15 → 87 ns: `CAS` на общей линии — это её передача между ядрами |
| `Balancer/round_robin` (8.4 ns) и `baseline_inline` (8.3 ns) — одно число | вся цена — общий `atomic.Uint64` (locked RMW), а не интерфейс `Balancer`, не `%` и не индексация. Оптимизировать в диспетчере нечего |
| `least_conn` 73 ns на 64 серверах, но в параллельном бенче **17 ns** (против 3 ns на 1 сервере) | линейный скан O(n), но только **чтение** `active`: он масштабируется, пока пул небольшой. На 1000 серверах это ~1 µs на соединение — известное ограничение (heap в future work) |
| `LimiterAllowParallel` 116 ns против 36 ns в один поток, и **не зависит от ключей** (distinct_keys 124 ns) | вся таблица под одним `sync.Mutex`: ключ не важен, важно, что лок один. Приемлемо: под флудом это на два порядка дешевле записи в лог, которую он предотвращает |
| `ConnectionSetup/through_proxy` = 38.5 KB и 80 allocs против 34.3 KB и 27 у `direct` | остаток — не буферы копирования, а сокеты: второй `netFD`, `sockaddr`, `accept`, `context.WithDeadline` на dial. Был бы там голый `io.Copy` (как на старом коде) — добавилось бы ~64 KiB на соединение (2×32 KiB на направления); plain-путь на не-Linux передаёт `io.CopyBuffer` буфер из пула. В профиле аллокаций ~80 % даёт **эхо-бэкенд теста** (`proxy.echo`), не прокси |
| `Load` = 223 µs и 270 allocs | это YAML-декодер с `KnownFields`, и вызывается он на SIGHUP, а не на запрос. Если control plane будет дёргать `Load` на каждый запрос конфига — это станет проблемой, сейчас — нет |
| `ThroughputScaling/conns=64` гуляет между прогонами: то −20 % от пика, то сам пик | в CPU-профиле на 64 соединениях **нет** ни `sync.Mutex`, ни `semacquire`/futex: 94 % — `runtime.cgocall` (каждый сетевой syscall на Windows идёт через него) и `io.copyBuffer`/`WSASend`. Провисание после 16 соединений — переподписка (2×64 горутин релея плюс 64 горутины эха на 8 логических CPU) и syscall-стоимость, а не блокировка: лимитер в этом бенче вообще не вызывается |
| `LoadInvalid/error_in_last_listener` = 5 ms на 500 листенерах | валидация возвращает **первую** ошибку, поэтому цена ~линейна позиции ошибки в документе. Сам отказ (unknown field / duplicate addr / битый адрес) стоит столько же, сколько успешная загрузка: 210–226 µs против 223 µs |

### Раскладка по файлам

Правило одно для тестов и бенчей: **имя файла = имя исходника, который он
покрывает** (`server.go` → `server_test.go`). У каждого `X.go` ровно один
`X_test.go`, и внутри сначала идут тесты, а ниже — бенчи этого исходника. Файл
растёт вширь не строками, а новыми файлами: когда исходник разбивают, вместе с ним
разбивают и его `_test.go`.

```
internal/proxy/
  *.go                     логика (doc, server, proxy, pool, upstream, state, copy, snapshot)
  helpers_test.go          общие фикстуры: echo, прокси, dial, конфиг, sinks
  server_test.go           тесты server.go + BenchmarkConnectionSetup
  proxy_test.go            тесты proxy.go + BenchmarkConnHotPath
  copy_test.go             тесты copy.go + Throughput / ThroughputScaling / RelayLatency
  snapshot_test.go         тесты snapshot.go + BenchmarkRuntimeLoad / Reload
  state_test.go            тесты state.go + BenchmarkConnLimiter
  upstream_test.go         тесты upstream.go + BenchmarkBalancer
  pool_test.go             тесты pool.go
internal/config/
  config_test.go           тесты config.go + BenchmarkLoad / Validate
  listener_config_test.go  тесты listener_config.go
  pool_config_test.go      тесты pool_config.go
internal/plugin/
  sandbox_test.go          тесты sandbox.go + BenchmarkOnAccept / OnAcceptParallel
internal/ratelimit/
  window_test.go           тесты window.go + BenchmarkAllow / AllowParallel
internal/observe/
  limiter_test.go          тесты limiter.go + BenchmarkLimiterAllow / AllowManyKeys
  metrics_test.go          тесты metrics.go + BenchmarkMetricsConnection
  admin_test.go            тесты admin.go
  log_test.go              тесты log.go
```

### Чего пока нет (зоны наблюдаемости)

Покрыто: CPU-горячий путь, пропускная способность, латентность, масштабирование,
reload (включая отказ), лимитер, лог-лимитер, конкуренция. Отдельно — то, что
мерить как бенч бессмысленно, но проверяется тестами: `idle_timeout`, который
реально **срабатывает** (`TestIdleTimeoutClosesSilentConnection`) и не рвёт
односторонний поток (`TestIdleTimeoutKeepsOneWayStreamAlive`), и отсутствие утечек
горутин на соединение (`TestNoGoroutineLeakPerConnection`: 25 соединений на обоих
copy-путях → счётчик горутин вернулся к базе).

**Не** покрыто (осознанные пробелы):

- reload **под нагрузкой** (`BenchmarkReload` мерит только холодную сборку
  snapshot, без живого трафика);
- поведение под насыщением: что происходит, когда `max_conns` достигнут, а
  клиенты продолжают ломиться (нагрузочный сценарий, а не бенч);
- внешние нагрузочные сценарии S0–S5 (см. §2).

### Запуск

```powershell
make bench                                              # всё
go test -run '^$' -bench . -benchmem ./...              # то же вручную
go test -run '^$' -bench BenchmarkThroughput -benchmem ./internal/proxy
go test -run '^$' -bench . -benchmem -benchtime=3s ./internal/proxy
```

### Варианты и сравнение

E2E-бенчи разбиты на подбенчи — по одному на вариант конфигурации:

```
BenchmarkThroughput/direct/64KiB
BenchmarkThroughput/zero_copy/64KiB
BenchmarkThroughput/idle_watchdog/64KiB
BenchmarkThroughputScaling/conns=16
BenchmarkRelayLatency/through_proxy
```

Сравнение «прокси против базы» — это `idle_watchdog` (или `zero_copy`) минус
`direct`: именно столько добавляет сам релей, а не loopback и эхо-бэкенд.

Сравнение прогонов — через `benchstat` (по несколько итераций, `-count`):

```powershell
go install golang.org/x/perf/cmd/benchstat@latest    # один раз

go test -run '^$' -bench . -benchmem -count=10 ./... > new.txt
benchstat old.txt new.txt
```

`benchstat` показывает дельту и её значимость — именно то, что нужно для
«аналитики вариантов» в дипломе (например, zero-copy против buffered).

### Платформа

`splice(2)` (zero-copy) есть только на Linux. Это не деталь оформления, а разница
в измеряемом: на Windows/macOS `io.Copy(dst, src)` уходит в
`TCPConn.ReadFrom` → `genericReadFrom` → `io.copyBuffer`, то есть в копирование
через user-space с **новым 32 KiB буфером на каждое направление и на каждое
соединение**. Поэтому:

- plain-путь (idle_timeout = 0) на Linux — нетронутый `io.Copy`, доходящий до
  `splice(2)`: 0 аллокаций, и прятать его `TCPConn.ReadFrom` обёрткой нельзя —
  это и есть путь к splice. На остальных платформах `copyPlain` (copy.go)
  передаёт той же стандартной петле `io.CopyBuffer` один буфер из `bufferPool`
  вместо новых 32 KiB на каждое направление; платформа проверяется одной строкой
  `runtime.GOOS` при установке соединения — без build-тегов и без своего цикла
  копирования. Цена — два интерфейсных wrapper'а (~64 B) на соединение;
- варианты `zero_copy` и `idle_watchdog` на Windows почти неразличимы (обе копии в
  user-space, у watchdog'а добавляется по `SetReadDeadline` на чанк), и цифры
  zero-copy на Windows **ничего не говорят** о zero-copy.

Рассмотренные альтернативы и почему отклонены (это единственное осознанное
решение про релей, код — в `copyPlain`):

| Альтернатива | Отклонение |
|---|---|
| голый `io.Copy` везде | ~64 KiB мусора на соединение на не-Linux (102.9 KB/conn в `ConnectionSetup` против 38.5 с пулом) — платформа не целевая, но платит каждый, кто запускает на ноутбуке |
| build-теги (`copy_linux.go` / `copy_other.go`) | два файла ради одной ветки на каждое: выбор платформы уезжает из глаз читателя при нулевом профите — вырезанный из бинарика код — это сотни инструкций ветки, выполняющейся раз на соединение (в stdlib тому же `runtime.GOOS` сотни применений вне build-файлов) |
| свой цикл копирования (`copyBuffered`) вместо `io.CopyBuffer` | дубликат семантики stdlib (EOF, `ErrShortWrite`, порядок read/write) ради ~100 B на соединение (0.3 % от 38.5 KB) — корректность важнее; петля одна и та же, что делает оба пути однородными уже без своей петли |
| пул только на idle-пути | без причины: plain-путь вне Linux не может дотянуться до splice в любом случае, значит буфер ему нужен всегда — так почему бы не тот же самый пул |

Build-теги становятся правильным выбором, если платформенная реализация вырастет
в настоящую (свой splice поверх сырых fd, платформенные фоллбеки) — тогда их
введение не переписывает историю решений, а применение проверяется тем же
критерием «существенная реализация на платформу».

Замер на Linux (WSL2, ядро 6.6.87, 4 vCPU; `-benchtime=1s -benchmem`), против
того же CPU на Windows:

| Бенчмарк | Linux | Windows |
|---|---|---|
| `Throughput/zero_copy/64KiB` | **557–725 MB/s**, 0 allocs | 222–253 MB/s |
| `Throughput/idle_watchdog/64KiB` | 257–286 MB/s | 208–314 MB/s |
| `Throughput/zero_copy/1MiB` | **862–977 MB/s** | 262–336 MB/s |
| `Throughput/idle_watchdog/1MiB` | 248–265 MB/s | 282–310 MB/s |
| `Throughput/direct/1MiB` (baseline) | 902–1092 MB/s | 620–627 MB/s |
| `ConnectionSetup/through_proxy` | 36.0 KB/op, 64 allocs | 37.8–38.5 KB/op, 80 allocs |
| `RelayLatency/through_proxy` | 56–60 µs | 109–120 µs |
| `ConnLimiterParallel/below_limit` | 54–57 ns | 84–87 ns |
| `BalancerParallel/round_robin` | 18–24 ns | 14 ns |
| `LimiterAllowParallel/shared_key` | 122–146 ns | 115–116 ns |

На Linux zero-copy быстрее буферизованного пути в **2.1–2.7×** (64 KiB) и
**3.3–3.7×** (1 MiB) — это и есть сигнатура работающего `splice`; на Windows
варианты сходятся (оба копируют в user space, у watchdog'а добавляется по
`SetReadDeadline` на чанк) настолько, что их разброс перекрывается. Аллокации
`zero_copy` на Linux — единицы байт на соединение (плюмбинг сокетов), буферов
копирования нет вообще.

> Числа в таблице — из нескольких прогонов (диапазон = разброс между ними).
> На мелких чанках WSL шумит сильнее (десятки тысяч итераций, 4 виртуальных
> ядра), но отношение zero-copy к buffered устойчиво: даже в самом медленном
> прогоне нулевая копия сохранила своё 3.4× на 1 MiB (594 против 176 MB/s),
> хотя абсолюты обеих сторон провалились вдвое.

> WSL2 — это настоящее ядро Linux в виртуальной машине (не трансляция syscall'ов),
> поэтому `splice` в нём настоящий и относительные числа (zero_copy против
> idle_watchdog, allocs/conn) валидны. Абсолютные MB/s с железом не сравнимы:
> 4 vCPU, виртуализованный NIC и ФС через 9p. Для цифр в диплом — прогон на bare
> metal, VM или CI (`ubuntu-latest`), а не в WSL.

## 2. Внешние нагрузочные тесты (план)

Заглушка под сценарии из `docs/03-mvp.md`, §8:

- `wrk2/` — HTTP-over-L4 пропускная способность + задержки (`p50`, `p99`);
- `k6/` или `vegeta/` — RPS / скорость установления соединений;
- `iperf3/` — «сырая» пропускная способность L4 (Gbps);
- `run.sh` — прогоняет сценарии S0–S5 и скармливает результат `benchstat`.
