# mysqlctl

CLI-утилита для управления клонированием и репликацией MySQL 8.0+.

Переписанный на Go аналог bash-скрипта — с конкурентными проверками, live progress-баром клона, конфиг-файлом, graceful shutdown и демо-режимом.

---

## Возможности

| Фича | Описание |
|------|----------|
| `clone` | MySQL Clone Plugin: устанавливает плагин, создаёт временных пользователей, запускает `CLONE INSTANCE FROM` с live-прогрессом, ждёт рестарта, чистит за собой |
| `switch` | Настройка GTID-репликации: создаёт replication user, выполняет `CHANGE REPLICATION SOURCE TO ... SOURCE_AUTO_POSITION=1` |
| `lag` | Статус репликации с цветным выводом; поддержка нескольких реплик одновременно |
| `lag --watch` | Автообновление статуса в терминале (аналог `watch`) |
| `--dry-run` | Выводит SQL без подключения к серверам |
| `--demo` | Эмуляция всех операций — никаких реальных подключений |
| `--json` | JSON-вывод для интеграции с другими инструментами |
| Конфиг-файл | Параметры по умолчанию из `~/.mysqlctl.yaml` |
| Пароли | Интерактивный ввод через `term.ReadPassword` — пароль не виден в `ps` и истории |
| Ctrl+C | Graceful shutdown через `context.WithCancel` |

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
mysql_password: ""        # оставьте пустым — утилита спросит при запуске
clone_password: ""        # пароль для временного clone_user
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

Полная копия данных с мастера на слейв через MySQL Clone Plugin.  
После клона слейв автоматически перезапускается — утилита ждёт с экспоненциальным backoff.

```bash
mysqlctl clone -s master.example.com -d slave.example.com
```

**Порядок шагов:**
1. Параллельная проверка доступности обоих серверов
2. Установка clone plugin (если не активен)
3. Создание временного `clone_user` на обоих серверах
4. `CLONE INSTANCE FROM` с live progress-баром (опрос `performance_schema.clone_progress`)
5. Ожидание перезапуска MySQL на слейве
6. Удаление `clone_user` и деинсталляция плагина

```
[1/4] Checking clone plugin...
[2/4] Creating clone users...
[3/4] Cloning...
Cloning [FILE COPY] 38% |████████░░░░░░░░░░░░| (18/48 GB, 312 MB/s)
[4/4] Waiting for destination MySQL to restart...
Clone completed in 2m34s
```

---

### `switch` — настройка репликации

Запускать **после** `clone`, когда данные на слейве уже актуальны.

```bash
mysqlctl switch -s master.example.com -d slave.example.com
```

Выполняет на мастере:
```sql
CREATE USER 'replicator'@'<slave_ip>' ...;
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

**JSON-вывод:**
```json
{
  "host": "slave.example.com",
  "source_host": "master.example.com",
  "io_running": true,
  "sql_running": true,
  "seconds_behind": 0,
  "configured": true
}
```

---

## Глобальные флаги

```
  -s, --src string            исходный сервер (IP или FQDN)
  -d, --dst stringArray       целевой сервер(ы)
      --port int              MySQL порт (default 3306)
  -u, --user string           MySQL пользователь (default "root")
  -p, --password string       MySQL пароль (если не задан — интерактивный ввод)
      --clone-password string пароль для clone_user
      --repl-user string      имя replication user (default "replicator")
      --repl-password string  пароль replication user
      --timeout duration      таймаут операций (default 12h)
      --dry-run               вывести SQL без выполнения
      --json                  JSON-вывод
      --demo                  демо-режим (эмуляция без реального MySQL)
  -v, --verbose               debug-логирование
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
  CREATE USER IF NOT EXISTS 'clone_user'@'192.168.1.11' ...;
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
# Клон с анимированным прогресс-баром (15–30 сек)
mysqlctl clone -s any-host -d any-host --demo

# Настройка репликации
mysqlctl switch -s master -d slave --demo

# Статус (случайные значения лага)
mysqlctl lag -d replica1,replica2 --demo

# Watch с дрейфующим лагом
mysqlctl lag -d replica1,replica2 --demo --watch
```

В демо-режиме принимаются любые хосты и пароли.  
Прогресс-бар клона воспроизводит реальные стадии: `DROP DATA → FILE COPY → PAGE COPY → REDO COPY → FILE SYNC`.

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
| Прогресс клона | нет | live bar via `clone_progress` |
| Ожидание рестарта | `sleep 60` | экспоненциальный backoff |
| Пароли | видны в `ps` / истории | интерактивный ввод |
| Конфиг | хардкод в скрипте | `~/.mysqlctl.yaml` + env |
| Отмена операции | kill процесса | Ctrl+C через context |
| Несколько реплик | нет | параллельный опрос + таблица |
| Вывод | только текст | текст / JSON |
| Сухой прогон | нет | `--dry-run` |

---

## Структура проекта

```
.
├── main.go
├── cmd/
│   ├── root.go      # cobra root, persistent flags, viper config
│   ├── clone.go     # команда clone
│   ├── switch.go    # команда switch
│   ├── lag.go       # команда lag (--watch, --multi, --json)
│   ├── demo.go      # эмуляция всех операций (--demo)
│   └── prompt.go    # интерактивный ввод паролей
└── internal/
    ├── mysql/
    │   ├── client.go  # DB-клиент, DSN, ExecSession
    │   ├── clone.go   # clone plugin, прогресс, cleanup
    │   └── repl.go    # статус репликации, CHANGE SOURCE
    ├── dns/
    │   └── resolve.go # hostname → IP
    └── progress/
        └── bar.go     # обёртка над progressbar/v3
```
