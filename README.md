# mysqlctl

CLI-утилита для управления клонированием и репликацией MySQL 8.0+.

Переписанный на Go аналог bash-скрипта — с конкурентными проверками, live-прогрессом клона по стейджам с ETA, конфиг-файлом, graceful shutdown и демо-режимом.

---

## Возможности

| Фича | Описание |
|------|----------|
| `clone` | MySQL Clone Plugin: устанавливает плагин, создаёт временных пользователей, запускает `CLONE INSTANCE FROM` с live-прогрессом по стейджам, ждёт рестарта, верифицирует статус, чистит за собой |
| `switch` | Настройка GTID-репликации: создаёт replication user, выполняет `CHANGE REPLICATION SOURCE TO ... SOURCE_AUTO_POSITION=1` |
| `lag` | Статус репликации с цветным выводом; поддержка нескольких реплик одновременно |
| `lag --watch` | Автообновление статуса в терминале (аналог `watch`) |
| `--dry-run` | Выводит SQL без подключения к серверам |
| `--demo` | Эмуляция всех операций — никаких реальных подключений |
| `--json` | JSON-вывод для интеграции с другими инструментами |
| `--native-auth` | `IDENTIFIED WITH mysql_native_password` для MySQL ≤ 8.0 |
| Конфиг-файл | Параметры по умолчанию из `~/.mysqlctl.yaml` |
| Пароли | Интерактивный ввод через `term.ReadPassword` — пароль не виден в `ps` и истории |
| Ctrl+C | Graceful shutdown: cleanup выполняется даже после отмены |

---

## Установка

```bash
git clone <repo>
cd mysql_clone
go build -o mysqlctl .

# Или сразу в PATH
go install .
```

**Требования:** Go 1.22+, MySQL 8.0.17+ (на обоих серверах для clone), MySQL 8.0+ для репликации.

---

## Конфигурация

Создайте `~/.mysqlctl.yaml` чтобы не вводить повторяющиеся параметры:

```yaml
port: 3306
mysql_user: root
src_user: ""           # переопределяет user только для источника
dst_user: ""           # переопределяет user только для назначения
mysql_password: ""     # оставьте пустым — утилита спросит при запуске
src_password: ""       # переопределяет password только для источника
dst_password: ""       # переопределяет password только для назначения
clone_password: ""     # пароль для временного clone_user
repl_user: replicator
repl_password: ""
timeout: 12h
```

Приоритет значений: **флаг CLI > переменная окружения > конфиг-файл > значение по умолчанию**.

Переменные окружения имеют префикс `MYSQLCTL_`:
```bash
export MYSQLCTL_MYSQL_PASSWORD=secret
export MYSQLCTL_CLONE_PASSWORD=clonepass
```

---

## Команды

### `clone` — клонирование базы

Полная копия данных с источника на назначение через MySQL Clone Plugin.  
После клона назначение автоматически перезапускается — утилита ждёт с экспоненциальным backoff и верифицирует `clone_status.STATE = Completed`.

```bash
mysqlctl clone -s master.example.com -d slave.example.com

# Разные порты
mysqlctl clone -s db1 -d db2 --src-port 3307 --dst-port 3308 --clone-password pass

# Разные учётные данные для src и dst
mysqlctl clone -s db1 -d db2 --src-user admin --dst-user dba \
  --src-password p1 --dst-password p2 --clone-password pass

# Для MySQL ≤ 8.0 (mysql_native_password)
mysqlctl clone -s db1 -d db2 --native-auth --clone-password pass
```

**Порядок шагов:**
1. Параллельная проверка доступности обоих серверов
2. Установка clone plugin (если не активен)
3. Создание временного `clone_user` на обоих серверах
4. `CLONE INSTANCE FROM` с live-прогрессом по стейджам (ETA, объём, время)
5. Ожидание перезапуска MySQL на назначении
6. Верификация `performance_schema.clone_status`
7. Удаление `clone_user` и деинсталляция плагина

```
[1/4] Checking clone plugin...
  ✓ clone plugin active on master.example.com
  ✓ clone plugin active on slave.example.com
[2/4] Creating clone users...
  ✓ clone_user@<slave_ip> created on master.example.com (BACKUP_ADMIN)
  ✓ clone_user@<master_ip> created on slave.example.com (CLONE_ADMIN)
[3/4] Cloning...
  ✓ DROP DATA     8s
  ⠏ FILE COPY     4m12s   ETA ~11m30s    28.4 GB / 64.4 GB   44.1%
[3/4] Cloning...
  ✓ DROP DATA     8s
  ✓ FILE COPY     16m5s       64.4 GB
  ✓ PAGE COPY     < 1s        160.0 KB
  ✓ REDO COPY     < 1s        2.5 KB
  ✓ FILE SYNC     < 1s
[4/4] Waiting for destination MySQL to restart...
  MySQL on slave.example.com is shutting down...
  Waiting for restart...
  MySQL is back online ✓
Verifying clone status...
  ✓ STATE = Completed
Total time: 16m59s
  ✓ clone_user dropped on both servers
  ✓ clone plugin uninstalled

Done. Destination database is a fresh copy of source.
```

---

### `switch` — настройка репликации

Запускать **после** `clone`, когда данные на слейве уже актуальны.

```bash
mysqlctl switch -s master.example.com -d slave.example.com

# Разные учётные данные
mysqlctl switch -s db1 -d db2 --src-user admin --dst-user dba \
  --src-password p1 --dst-password p2 --repl-password replpass
```

Выполняет на мастере:
```sql
CREATE USER 'replicator'@'<slave_ip>' IDENTIFIED BY '...';
GRANT REPLICATION SLAVE ON *.* TO 'replicator'@'<slave_ip>';
```

Выполняет на слейве:
```sql
STOP REPLICA;
RESET REPLICA ALL;
CHANGE REPLICATION SOURCE TO
  SOURCE_HOST='<master_ip>',
  SOURCE_USER='replicator',
  SOURCE_AUTO_POSITION=1;
START REPLICA;
```

После запуска автоматически показывает статус репликации.

---

### `lag` — статус репликации

```bash
# Одна реплика
mysqlctl lag -d slave.example.com

# Несколько реплик — таблица
mysqlctl lag -d slave1.example.com -d slave2.example.com -d slave3.example.com
mysqlctl lag -d slave1,slave2,slave3   # или через запятую

# Автообновление каждые 3 секунды
mysqlctl lag -d slave1,slave2 --watch --interval 3s

# JSON для скриптов/мониторинга
mysqlctl lag -d slave.example.com --json
```

**Пример вывода одной реплики:**
```
  Host:                        slave.example.com
  Source_Host:                 master.example.com:3306
  Replica_IO_Running:          Yes        ← зелёный
  Replica_SQL_Running:         Yes        ← зелёный
  Seconds_Behind_Source:       0          ← зелёный / жёлтый / красный
  IO_State:                    Waiting for source to send event
```

**Таблица нескольких реплик:**
```
+---------+-----+-----+---------+--------------------+------------+
| HOST    | IO  | SQL | LAG (S) | SOURCE             | LAST ERROR |
+---------+-----+-----+---------+--------------------+------------+
| slave1  | Yes | Yes |       0 | master.example.com |            |
| slave2  | Yes | Yes |       4 | master.example.com |            |
| slave3  | No  | Yes |    NULL | master.example.com | I/O error  |
+---------+-----+-----+---------+--------------------+------------+
```

---

## Глобальные флаги

```
  -s, --src string              исходный сервер (IP или FQDN)
  -d, --dst stringArray         целевой сервер(ы)
      --port int                MySQL порт (default 3306)
      --src-port int            порт источника (переопределяет --port)
      --dst-port int            порт назначения (переопределяет --port)
  -u, --user string             MySQL пользователь (default "root")
      --src-user string         пользователь источника (переопределяет --user)
      --dst-user string         пользователь назначения (переопределяет --user)
  -p, --password string         MySQL пароль (если не задан — интерактивный ввод)
      --src-password string     пароль источника (переопределяет --password)
      --dst-password string     пароль назначения (переопределяет --password)
      --clone-password string   пароль для clone_user
      --repl-user string        имя replication user (default "replicator")
      --repl-password string    пароль replication user
      --timeout duration        таймаут операций (default 12h)
      --native-auth             IDENTIFIED WITH mysql_native_password (для MySQL ≤ 8.0)
      --dry-run                 вывести SQL без выполнения
      --json                    JSON-вывод
      --demo                    демо-режим (эмуляция без реального MySQL)
  -v, --verbose                 debug-логирование (SQL, таймстемпы)
```

---

## Dry-run режим

Показывает точный SQL, который будет выполнен — без подключения к серверам:

```bash
mysqlctl clone -s master -d slave --clone-password x --dry-run
```

```
[dry-run] -- SRC (192.168.1.10)
  SET sql_log_bin=0;
  CREATE USER IF NOT EXISTS 'clone_user'@'192.168.1.11' IDENTIFIED BY 'x';
  GRANT BACKUP_ADMIN ON *.* TO 'clone_user'@'192.168.1.11';
  SET sql_log_bin=1;

[dry-run] -- DST (192.168.1.11)
  SET GLOBAL clone_valid_donor_list = '192.168.1.10:3306';
  CLONE INSTANCE FROM 'clone_user'@'192.168.1.10':3306 IDENTIFIED BY 'x';
```

---

## Demo-режим

Эмулирует все операции без реального MySQL — удобно для презентаций и тестирования CLI.

```bash
# Клон с анимированным прогрессом (15–30 сек)
mysqlctl clone -s any-host -d any-host --demo

# Настройка репликации
mysqlctl switch -s master -d slave --demo

# Статус (случайные значения лага)
mysqlctl lag -d replica1,replica2 --demo

# Watch с дрейфующим лагом
mysqlctl lag -d replica1,replica2 --demo --watch
```

В демо-режиме принимаются любые хосты и пароли.  
Прогресс клона воспроизводит реальные стадии: `DROP DATA → FILE COPY → PAGE COPY → REDO COPY → FILE SYNC`.

---

## Типичный рабочий сценарий

```bash
# 1. Клонировать данные с мастера на новый слейв
mysqlctl clone \
  -s master.prod.example.com \
  -d slave.prod.example.com \
  --clone-password $(cat /etc/mysqlctl/clone.pass)

# 2. Запустить репликацию
mysqlctl switch \
  -s master.prod.example.com \
  -d slave.prod.example.com \
  --repl-password $(cat /etc/mysqlctl/repl.pass)

# 3. Мониторинг лага
mysqlctl lag -d slave.prod.example.com --watch --interval 5s
```

---

## Отличия от bash-версии

| | bash.sh | mysqlctl |
|---|---|---|
| Проверка коннекта | последовательная | параллельная (goroutines) |
| Прогресс клона | нет | live стейджи с ETA via `clone_progress` |
| Обрыв соединения при клоне | не обрабатывается | polling `clone_status` до Completed |
| Ожидание рестарта | `sleep 60` | экспоненциальный backoff |
| Пароли | видны в `ps` / истории | интерактивный ввод |
| Раздельные credentials | нет | `--src-user/password`, `--dst-user/password` |
| Конфиг | хардкод в скрипте | `~/.mysqlctl.yaml` + env |
| Отмена операции | kill процесса | Ctrl+C + cleanup через context |
| Несколько реплик | нет | параллельный опрос + таблица |
| Вывод | только текст | текст / JSON |
| Сухой прогон | нет | `--dry-run` |
| Демо без MySQL | нет | `--demo` |

---

## Структура проекта

```
.
├── main.go
├── cmd/
│   ├── root.go      # cobra root, persistent flags, viper config, credential helpers
│   ├── clone.go     # команда clone
│   ├── switch.go    # команда switch
│   ├── lag.go       # команда lag (--watch, --multi, --json)
│   ├── demo.go      # эмуляция всех операций (--demo)
│   └── prompt.go    # интерактивный ввод пользователя и паролей
└── internal/
    ├── mysql/
    │   ├── client.go  # DB-клиент, DSN, ReadTimeout, ExecSession
    │   ├── clone.go   # clone plugin, прогресс со спиннером/ETA, cleanup
    │   └── repl.go    # статус репликации, CHANGE SOURCE
    └── dns/
        └── resolve.go # hostname → IP
```
