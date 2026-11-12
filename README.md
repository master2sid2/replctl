# mysqlctl

CLI tool for MySQL 8.0+ cloning and replication management.

---

## Features

| Feature | Description |
|---------|-------------|
| `clone` | MySQL Clone Plugin: installs plugin, creates temporary users, runs `CLONE INSTANCE FROM` with live per-stage progress and ETA, waits for restart, verifies clone status, cleans up |
| `switch` | GTID replication setup or repointing to another source: pre-flight checks, recreates replication user, runs `CHANGE REPLICATION SOURCE TO ... SOURCE_AUTO_POSITION=1`, confirms both threads run |
| `lag` | Replication status with color output; supports multiple replicas simultaneously |
| `lag --watch` | Continuous status refresh (like `watch`) |
| `--dry-run` | Prints SQL without connecting to servers; passwords are masked as `****` |
| `--demo` | Simulates all operations — no real MySQL connections |
| `--json` | JSON output for scripting and monitoring |
| `--native-auth` | `IDENTIFIED WITH mysql_native_password` for MySQL ≤ 8.0 |
| Config file | Defaults from `~/.mysqlctl.yaml` |
| Passwords | Interactive input via `term.ReadPassword` — not visible in `ps` or shell history |
| Ctrl+C / `--timeout` | A running `CLONE` is killed on the destination, then cleanup runs. A second Ctrl+C exits immediately |

---

## Installation

```bash
git clone <repo>
cd mysqlctl
go build -o mysqlctl .

# Or install to PATH
go install .
```

**Requirements:** Go 1.22+, MySQL 8.0.17+ on both servers for clone, MySQL 8.0+ for replication.

---

## Configuration

Create `~/.mysqlctl.yaml` to avoid repeating common flags:

```yaml
port: 3306
src_port: 0            # overrides port for source only
dst_port: 0            # overrides port for destination only
mysql_user: root       # set here to skip the interactive username prompt
src_user: ""           # overrides user for source only
dst_user: ""           # overrides user for destination only
mysql_password: ""     # set here or leave empty — tool will prompt interactively
src_password: ""       # overrides password for source only
dst_password: ""       # overrides password for destination only
clone_password: ""     # password for temporary clone_user
repl_user: replicator
repl_password: ""
timeout: 12h
```

Value priority: **CLI flag > environment variable > config file > default**.  
A config file that cannot be parsed is an error — the tool does not fall back to defaults.

Environment variables use the `MYSQLCTL_` prefix:
```bash
export MYSQLCTL_MYSQL_PASSWORD=secret
export MYSQLCTL_CLONE_PASSWORD=clonepass
```

---

## Commands

### `clone` — clone a database

Full data copy from source to destination via MySQL Clone Plugin.  
After cloning, the destination automatically restarts — the tool waits with exponential backoff and verifies `clone_status`.

Success is reported only when `clone_status` on the destination holds a record of the clone **started by this run** with `STATE = Completed`. A dropped connection, an empty table, a record left by an earlier clone or an unreadable status all end with an error and a non-zero exit code.

```bash
mysqlctl clone -s db1.example.com -d db2.example.com

# Different ports
mysqlctl clone -s db1 -d db2 --src-port 3307 --dst-port 3308 --clone-password pass

# Different credentials for source and destination
mysqlctl clone -s db1 -d db2 --src-user admin --dst-user dba \
  --src-password p1 --dst-password p2 --clone-password pass

# For MySQL ≤ 8.0
mysqlctl clone -s db1 -d db2 --native-auth --clone-password pass

# Preview SQL without executing
mysqlctl clone -s db1 -d db2 --dry-run --clone-password pass
```

**Steps:**
1. Parallel connectivity check on both servers
2. Refuse if both hosts are the same instance (`server_uuid`); show what the destination holds now (schemas, size, connected replicas) and source properties that affect it afterwards (enabled events, source being a replica)
3. Confirmation
4. Install clone plugin (if not active)
5. Create temporary `clone_user` on both servers (a leftover one is dropped first)
6. `CLONE INSTANCE FROM` with live per-stage progress (ETA, size, elapsed time)
7. Wait for MySQL restart on destination
8. Verify `performance_schema.clone_status`
9. Drop every `clone_user` account and uninstall the plugin on both servers

Cleanup (step 9) also runs when the clone fails, is interrupted or times out, and reports per server whether it succeeded. Exactly one `--dst` is accepted.

**Interrupting.** `CLONE` drops the destination data as its first stage, so after Ctrl+C or `--timeout` the destination is left incomplete and must be cloned again. The tool kills the `CLONE` thread on the destination (closing the client connection alone would leave it running) and says so.

**mysqld without a supervisor.** If mysqld on the destination is not managed by systemd/mysqld_safe it cannot restart itself (error 3707) and shuts down. The tool tells you to start it manually and keeps waiting (up to 5 minutes), then verifies as usual.

```
[1/4] Checking clone plugin...
  ✓ clone plugin active on db1.example.com
  ✓ clone plugin active on db2.example.com
[2/4] Creating clone users...
  ✓ clone_user@<dst_ip> created on db1.example.com (BACKUP_ADMIN)
  ✓ clone_user@<src_ip> created on db2.example.com (CLONE_ADMIN)
[3/4] Cloning...
  ✓ DROP DATA     8s
  ⠏ FILE COPY     4m12s   ETA ~11m30s    28.4 GB / 64.4 GB   44.1%
...
  ✓ DROP DATA     8s
  ✓ FILE COPY     16m5s       64.4 GB
  ✓ PAGE COPY     < 1s        160.0 KB
  ✓ REDO COPY     < 1s        2.5 KB
  ✓ FILE SYNC     < 1s
[4/4] Waiting for destination MySQL to restart...
  MySQL on db2.example.com is shutting down...
  Waiting for restart...
  MySQL is back online ✓
Verifying clone status...
  ✓ STATE = Completed (source 10.0.0.1:3306)
Total time: 16m59s
  ✓ clone_user dropped, clone plugin uninstalled on db1.example.com
  ✓ clone_user dropped, clone plugin uninstalled on db2.example.com

Done. Destination database is a fresh copy of source.
```

---

### `switch` — configure replication

Run **after** `clone`, or to repoint an existing replica to another source (for example after one of the replicas has been promoted). Whatever replication is configured on the destination is removed with `RESET REPLICA ALL` and replaced.

```bash
mysqlctl switch -s db1.example.com -d db2.example.com

# Different credentials
mysqlctl switch -s db1 -d db2 --src-user admin --dst-user dba \
  --src-password p1 --dst-password p2 --repl-password replpass

# Source without TLS
mysqlctl switch -s db1 -d db2 --repl-ssl=false --repl-password replpass
```

**Pre-flight checks** run before the confirmation and before anything is changed:

| Check | Result |
|-------|--------|
| Same instance (`server_uuid`), equal or zero `server_id` | error |
| `gtid_mode` not `ON` on either server, binary log disabled on source | error |
| Source has purged binary logs the destination still needs (`gtid_purged`) | error — re-clone |
| Destination has transactions the source does not have | warning, shown before the confirmation |
| `read_only` is OFF on destination | warning |

The current source of the destination, if any, is shown before the confirmation.

Runs on source:
```sql
DROP USER IF EXISTS 'replicator'@'<dst_ip>';
CREATE USER 'replicator'@'<dst_ip>' IDENTIFIED BY '...';
GRANT REPLICATION SLAVE ON *.* TO 'replicator'@'<dst_ip>';
```

Runs on destination:
```sql
STOP REPLICA;
RESET REPLICA ALL;
CHANGE REPLICATION SOURCE TO
  SOURCE_HOST='<src_ip>',
  SOURCE_USER='replicator',
  SOURCE_AUTO_POSITION=1,
  SOURCE_SSL=1;
START REPLICA;
```

`DROP USER IF EXISTS` ensures the password is always updated on repeated runs.  
`SOURCE_SSL=1` is the default: `caching_sha2_password` (MySQL 8.0 default) needs either TLS or the source's RSA public key. With `--repl-ssl=false` the tool sends `SOURCE_SSL=0, GET_SOURCE_PUBLIC_KEY=1` instead.

After setup the tool waits up to 30 seconds for both replication threads to run and prints the status. If they do not start, the IO/SQL thread error is shown and the exit code is non-zero.

---

### `lag` — replication status

```bash
# Single replica
mysqlctl lag -d db2.example.com

# Multiple replicas — table view
mysqlctl lag -d replica1.example.com -d replica2.example.com -d replica3.example.com
mysqlctl lag -d replica1,replica2,replica3   # or comma-separated

# Continuous refresh every 3 seconds
mysqlctl lag -d replica1,replica2 --watch --interval 3s

# JSON output for scripts/monitoring
mysqlctl lag -d replica2.example.com --json
```

Every replication channel of a host is shown (multi-source replicas get one entry per channel). `Last Error` combines the IO and SQL thread errors. Without `--watch`, the exit code is non-zero when a host could not be queried.

**Single replica output:**
```
  Host:                        replica2.example.com
  Source_Host:                 db1.example.com:3306
  Replica_IO_Running:          Yes
  Replica_SQL_Running:         Yes
  Seconds_Behind_Source:       0
  IO_State:                    Waiting for source to send event
```

**Multi-replica table:**
```
+----------+-----+-----+---------+--------------------+------------+
| HOST     | IO  | SQL | LAG (S) | SOURCE             | LAST ERROR |
+----------+-----+-----+---------+--------------------+------------+
| replica1 | Yes | Yes |       0 | db1.example.com    |            |
| replica2 | Yes | Yes |       4 | db1.example.com    |            |
| replica3 | No  | Yes |    NULL | db1.example.com    | I/O error  |
+----------+-----+-----+---------+--------------------+------------+
```

---

## Global flags

```
  -s, --src string              source host (IP or FQDN)
  -d, --dst stringArray         destination host(s)
      --port int                MySQL port (default 3306)
      --src-port int            source port (overrides --port)
      --dst-port int            destination port (overrides --port)
  -u, --user string             MySQL user (default "root")
      --src-user string         source user (overrides --user)
      --dst-user string         destination user (overrides --user)
  -p, --password string         MySQL password (prompted if not set)
      --src-password string     source password (overrides --password)
      --dst-password string     destination password (overrides --password)
      --clone-password string   clone_user password
      --repl-user string        replication user name (default "replicator")
      --repl-password string    replication user password
      --timeout duration        overall time limit for clone/switch; a running CLONE is killed when it expires (default 12h)
      --native-auth             IDENTIFIED WITH mysql_native_password (MySQL ≤ 8.0)
      --dry-run                 print SQL without executing
      --json                    JSON output
      --demo                    demo mode (simulate without real MySQL)
  -v, --verbose                 debug logging (SQL statements, timestamps)
```

`switch` only:
```
      --repl-ssl                encrypt the replication connection (default true)
```

Passwords are masked as `****` everywhere: dry-run, `-v` logs and error messages.

---

## Dry-run mode

Shows the exact SQL that would be executed — no server connections:

```bash
mysqlctl clone -s db1 -d db2 --clone-password x --dry-run
```

```
[dry-run] -- SRC (192.168.1.10)
  SET sql_log_bin=0;
  DROP USER IF EXISTS 'clone_user'@'192.168.1.11';
  CREATE USER 'clone_user'@'192.168.1.11' IDENTIFIED BY '****';
  GRANT BACKUP_ADMIN ON *.* TO 'clone_user'@'192.168.1.11';
  SET sql_log_bin=1;

[dry-run] -- DST (192.168.1.11)
  SET sql_log_bin=0;
  DROP USER IF EXISTS 'clone_user'@'192.168.1.10';
  CREATE USER 'clone_user'@'192.168.1.10' IDENTIFIED BY '****';
  GRANT CLONE_ADMIN ON *.* TO 'clone_user'@'192.168.1.10';
  SET sql_log_bin=1;

[dry-run] -- DST (← 192.168.1.10:3306)
  SET GLOBAL clone_valid_donor_list = '192.168.1.10:3306';
  CLONE INSTANCE FROM 'clone_user'@'192.168.1.10':3306 IDENTIFIED BY '****';
```

Passwords are always masked in dry-run output. `DST (← src)` indicates the SQL runs on the destination but clones data from the source.

---

## Demo mode

Simulates all operations without real MySQL — useful for presentations and CLI testing.

```bash
# Clone with animated progress (15–30 sec)
mysqlctl clone -s any-host -d any-host --demo

# Replication setup
mysqlctl switch -s source -d replica --demo

# Lag status with random values
mysqlctl lag -d replica1,replica2 --demo

# Watch mode with drifting lag
mysqlctl lag -d replica1,replica2 --demo --watch
```

Demo mode accepts any hosts and passwords.

---

## Typical workflow

```bash
# 1. Clone data from source to new replica
mysqlctl clone \
  -s db1.prod.example.com \
  -d db2.prod.example.com \
  --clone-password $(cat /etc/mysqlctl/clone.pass)

# 2. Start replication
mysqlctl switch \
  -s db1.prod.example.com \
  -d db2.prod.example.com \
  --repl-password $(cat /etc/mysqlctl/repl.pass)

# 3. Monitor lag
mysqlctl lag -d db2.prod.example.com --watch --interval 5s
```

---

## Project structure

```
.
├── main.go
├── cmd/
│   ├── root.go      # cobra root, persistent flags, viper config, credential helpers
│   ├── clone.go     # clone command
│   ├── switch.go    # switch command
│   ├── lag.go       # lag command (--watch, --multi, --json)
│   ├── demo.go      # demo mode simulation
│   └── prompt.go    # interactive user/password prompts
└── internal/
    ├── mysql/
    │   ├── client.go  # DB client, DSN, ReadTimeout, ExecSession
    │   ├── clone.go   # clone plugin, per-stage progress with spinner/ETA, verification, cleanup
    │   ├── inspect.go # server settings and data summary for pre-flight checks
    │   └── repl.go    # replication status, pre-flight checks, CHANGE SOURCE
    └── dns/
        └── resolve.go # hostname → IP resolution
```
