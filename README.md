# replctl

[![CI](https://github.com/master2sid2/replctl/actions/workflows/ci.yml/badge.svg)](https://github.com/master2sid2/replctl/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/master2sid2/replctl)](https://github.com/master2sid2/replctl/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/master2sid2/replctl)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Build and maintain MySQL replicas from the command line:

- **`clone`** — copy a source server onto a destination with the MySQL Clone
  Plugin, with live progress, and verify the result;
- **`switch`** — set up GTID replication on the destination, or repoint an
  existing replica to another source;
- **`lag`** — replication status of one or many replicas, as text, a table or
  JSON.

Every step that changes a server is checked first and confirmed by the operator,
temporary users and plugins are removed afterwards, and success is only
reported once the server confirms it.

Tested against MySQL 8.0 and 8.4.

## Install

```bash
go install github.com/master2sid2/replctl@latest
```

Or download a binary from the [releases](https://github.com/master2sid2/replctl/releases)
page, or build from source (see [Build](#build)).

**Requirements:** MySQL 8.0.17+ on both servers for `clone` (same version
series; before 8.0.37 the exact same version), MySQL 8.0.23+ for `switch`
(`CHANGE REPLICATION SOURCE TO`), GTID mode `ON` for replication.

## Typical workflow

```bash
# 1. Copy the data from the source onto the new replica
replctl clone -s db1.example.com -d db2.example.com

# 2. Start replication
replctl switch -s db1.example.com -d db2.example.com

# 3. Watch it catch up
replctl lag -d db2.example.com --watch --interval 5s
```

The MySQL password of your own user is asked for on the terminal when it is not
given in flags, the environment or the config file. Passwords for the accounts
replctl creates are generated when not set; see [Users and passwords](#users-and-passwords).

## Commands

### `clone` — copy a server

Full data copy from source to destination via the MySQL Clone Plugin. After
cloning, the destination restarts by itself; the tool waits for it and checks
`performance_schema.clone_status`.

Success is reported only when `clone_status` on the destination holds a record
of the clone **started by this run** with `STATE = Completed`. A dropped
connection, an empty table, a record left by an earlier clone or an unreadable
status all end with an error and a non-zero exit code.

```bash
replctl clone -s db1.example.com -d db2.example.com

# Different ports
replctl clone -s db1 -d db2 --src-port 3307 --dst-port 3308

# Different credentials for source and destination
replctl clone -s db1 -d db2 --src-user admin --dst-user dba

# The servers talk over a network this machine does not see
replctl clone -s db1.example.com -d db2.example.com --src-ip 10.0.0.1 --dst-ip 10.0.0.2

# Preview the SQL without connecting
replctl clone -s db1 -d db2 --dry-run
```

**Steps:**

1. Connect to both servers in parallel.
2. Refuse when both hosts are the same instance (`server_uuid`) or their
   versions cannot be cloned. Show what the destination holds now (schemas,
   size, connected replicas) and source properties that affect it afterwards
   (enabled events, source being a replica).
3. Ask for confirmation (`--yes` skips it).
4. Install the clone plugin where it is not active.
5. Create a temporary `clone_user` with `BACKUP_ADMIN` on the source (a
   leftover one is dropped first), see [Users and passwords](#users-and-passwords).
   On the destination the `CLONE` statement runs as your own user.
6. `CLONE INSTANCE FROM` with live per-stage progress (ETA, size, elapsed time).
7. Wait for MySQL on the destination to restart.
8. Verify `performance_schema.clone_status`.
9. Drop every `clone_user` account and uninstall the plugin on both servers.

Cleanup (step 9) also runs when the clone fails, is interrupted or times out,
and reports per server whether it succeeded.

**Interrupting.** `CLONE` drops the destination's data as its first stage.
After Ctrl+C or `--timeout` the tool kills the `CLONE` thread on the
destination (closing the client connection alone would leave it running) and
tells you whether the data was already dropped. If the clone failed before
that stage, for example because the source was unreachable, the destination
keeps its data. A second Ctrl+C exits immediately, without cleanup.

**Restart.** If mysqld on the destination is not run by a service manager it
cannot restart itself (error 3707) and shuts down. The tool says so and keeps
waiting up to 5 minutes for it to be started, then verifies as usual. systemd,
`mysqld_safe` and a Docker restart policy all restart it automatically.

```
[1/4] Checking clone plugin...
  ✓ clone plugin active on db1.example.com
  ✓ clone plugin active on db2.example.com
[2/4] Creating clone user on source...
  ✓ clone_user@10.0.0.2 created on db1.example.com (BACKUP_ADMIN)
[3/4] Cloning...
  ✓ DROP DATA     8s
  ⠏ FILE COPY     4m12s   ETA ~11m30s    28.4 GB / 64.4 GB   44.1%
...
  ✓ FILE COPY     16m5s       64.4 GB
  ✓ PAGE COPY     < 1s        160.0 KB
  ✓ REDO COPY     < 1s        2.5 KB
  ✓ FILE SYNC     < 1s
[4/4] Waiting for destination MySQL to restart...
  MySQL is back online ✓
Verifying clone status...
  ✓ STATE = Completed (source 10.0.0.1:3306)
Total time: 16m59s
  ✓ clone_user dropped, clone plugin uninstalled on db1.example.com
  ✓ clone_user dropped, clone plugin uninstalled on db2.example.com

Done. Destination database is a fresh copy of source.
```

When the output is not a terminal (a log file, CI), the spinner is replaced by
a progress line once a minute.

### `switch` — set up or repoint replication

Run **after** `clone`, or to repoint an existing replica to another source (for
example after one of the replicas has been promoted). Whatever replication is
configured on the destination is removed with `RESET REPLICA ALL` and replaced.

```bash
replctl switch -s db1.example.com -d db2.example.com

# Source without TLS
replctl switch -s db1 -d db2 --repl-ssl=false
```

**Pre-flight checks** run before the confirmation and before anything is
changed:

| Check | Result |
|---|---|
| Same instance (`server_uuid`), equal or zero `server_id` | error |
| `gtid_mode` not `ON` on either server, binary log disabled on source | error |
| Source has purged binary logs the destination still needs (`gtid_purged`) | error — re-clone |
| Destination has transactions the source does not have | warning |
| `read_only` is OFF on destination | warning |

The current source of the destination, if any, is shown before the
confirmation.

Creates the replication account on the source (see
[Users and passwords](#users-and-passwords)), then runs on the destination:
```sql
STOP REPLICA;
RESET REPLICA ALL;
CHANGE REPLICATION SOURCE TO
  SOURCE_HOST='<src_ip>', SOURCE_PORT=3306,
  SOURCE_USER='replicator', SOURCE_PASSWORD='...',
  SOURCE_AUTO_POSITION=1, SOURCE_SSL=1;
START REPLICA;
```

`SOURCE_SSL=1` is the default because `caching_sha2_password` needs either TLS
or the source's RSA public key; with `--repl-ssl=false` the tool sends
`SOURCE_SSL=0, GET_SOURCE_PUBLIC_KEY=1` instead.

After setup the tool waits up to 30 seconds for both replication threads to run
and prints the status. If they do not start, the IO/SQL thread error is shown
and the exit code is non-zero.

### `lag` — replication status

```bash
replctl lag -d db2.example.com
replctl lag -d replica1 -d replica2 -d replica3
replctl lag -d replica1,replica2,replica3      # same thing
replctl lag -d replica1,replica2 --watch --interval 3s
replctl lag -d db2.example.com --json
```

Every replication channel of a host is shown (multi-source replicas get one
entry per channel). `Last Error` combines the IO and SQL thread errors. Without
`--watch`, the exit code is non-zero when a host could not be queried.

```
  Host: replica2.example.com
  Source_Host:                 db1.example.com:3306
  Replica_IO_Running:          Yes
  Replica_SQL_Running:         Yes
  Seconds_Behind_Source:       0
  IO_State:                    Waiting for source to send event
```

```
+----------+-----+-----+---------+-----------------+------------+
|   HOST   | IO  | SQL | LAG (S) |     SOURCE      | LAST ERROR |
+----------+-----+-----+---------+-----------------+------------+
| replica1 | Yes | Yes |       0 | db1.example.com |            |
| replica2 | Yes | Yes |       4 | db1.example.com |            |
| replica3 | No  | Yes | -       | db1.example.com | IO: ...    |
+----------+-----+-----+---------+-----------------+------------+
```

In JSON, `seconds_behind` is `null` whenever the lag is unknown — replication
stopped, not configured or the host unreachable — so monitoring never mistakes
it for zero.

## Flags

```
  -s, --src string              source host (IP or FQDN)
  -d, --dst stringArray         destination host(s)
      --src-ip string           address the destination uses to reach the source (default: --src resolved here)
      --dst-ip string           address the source sees the destination connecting from (default: --dst resolved here)
      --port int                MySQL port (default 3306)
      --src-port int            source port (overrides --port)
      --dst-port int            destination port (overrides --port)
  -u, --user string             MySQL user (default "root")
      --src-user string         source user (overrides --user)
      --dst-user string         destination user (overrides --user)
  -p, --password string         MySQL password (asked for if not set)
      --src-password string     source password (overrides --password)
      --dst-password string     destination password (overrides --password)
      --clone-password string   clone_user password (random if not set)
      --repl-user string        replication user name (default "replicator")
      --repl-password string    replication user password (generated and shown once if not set)
      --timeout duration        overall time limit for clone/switch; a running CLONE is killed when it expires (default 12h)
      --native-auth             create users WITH mysql_native_password (deprecated; disabled by default in 8.4, removed in 9.0)
      --dry-run                 print SQL without executing
  -y, --yes                     do not ask for confirmation (for scripts)
      --json                    JSON output
      --demo                    demo mode (simulate without real MySQL)
      --config string           config file (default ~/.config/replctl/config.yaml)
  -v, --verbose                 debug logging (SQL statements, timestamps)
      --version                 print version
```

`switch` only: `--repl-ssl` (default true). `lag` only: `--watch`, `--interval`.

`--src-ip` and `--dst-ip` matter when the servers reach each other over a
different network than the machine running the tool: the accounts are created
for the address the other server actually connects from.

## Configuration

Defaults can live in `~/.config/replctl/config.yaml` (`$XDG_CONFIG_HOME` is
honoured), or in any file given with `--config`:

```yaml
port: 3306
src_port: 0            # overrides port for source only
dst_port: 0            # overrides port for destination only
mysql_user: root       # set here to skip the interactive username prompt
src_user: ""           # overrides user for source only
dst_user: ""           # overrides user for destination only
mysql_password: ""     # set here or leave empty — the tool will ask
src_password: ""
dst_password: ""
clone_password: ""     # random if empty
repl_user: replicator
repl_password: ""      # generated and shown once if empty
timeout: 12h
```

Keep the file private (`chmod 600`) if it holds passwords.

Value priority: **flag > environment variable > config file > default**. A
config file that cannot be parsed is an error; the tool never silently falls
back to defaults.

Environment variables use the `REPLCTL_` prefix:

```bash
export REPLCTL_MYSQL_PASSWORD=secret
export REPLCTL_CLONE_PASSWORD=clonepass
```

## Users and passwords

### Accounts replctl creates

Both accounts are created **on the source only**, in one session with the
binary log switched off:

**`clone_user`** — temporary, for `clone`:

```sql
SET sql_log_bin=0;
DROP USER IF EXISTS 'clone_user'@'<dst_ip>';
CREATE USER 'clone_user'@'<dst_ip>' IDENTIFIED BY '<clone_password>';
GRANT BACKUP_ADMIN ON *.* TO 'clone_user'@'<dst_ip>';
SET sql_log_bin=1;
```

The destination then runs `CLONE INSTANCE FROM 'clone_user'@'<src_ip>':<port>
IDENTIFIED BY '<clone_password>'` as your own user. Afterwards — on success,
failure, Ctrl+C or timeout — every `clone_user` account and the clone plugin are
removed from both servers, again with the binary log off. The destination needs
this too: the clone copies the source's `mysql.user`, so the account arrives
there with the data.

**Replication account** — permanent, for `switch` (name from `--repl-user`,
`replicator` by default):

```sql
SET sql_log_bin=0;
DROP USER IF EXISTS 'replicator'@'<dst_ip>';
CREATE USER 'replicator'@'<dst_ip>' IDENTIFIED BY '<repl_password>';
GRANT REPLICATION SLAVE ON *.* TO 'replicator'@'<dst_ip>';
SET sql_log_bin=1;
```

- One account per replica: the host part is the replica's address, not `'%'`,
  and the only privilege is `REPLICATION SLAVE`.
- Running `switch` again for the same replica recreates its account, so the
  password always matches what is sent in `SOURCE_PASSWORD`. Accounts of other
  replicas are not touched.
- The account is never removed. After repointing a replica to another source,
  its old account stays on the previous source.
- With `--native-auth` both accounts are created `IDENTIFIED WITH
  mysql_native_password`; otherwise the server's default plugin is used
  (`caching_sha2_password` in 8.x).

`<dst_ip>` and `<src_ip>` are the hosts resolved on the machine running
replctl, or `--dst-ip` / `--src-ip` when given.

### Why the binary log is off

Statements run with `sql_log_bin=0` produce no GTIDs and are not replicated:

- replctl adds no transactions to the source or its existing replicas, so it
  never creates errant transactions by itself;
- the accounts do not spread to other replicas of the source.

The flip side: after a failover the promoted replica has no replication
accounts of its own — run `switch` with it as `-s` and they are created there.
Installing and uninstalling the clone plugin is not written to the binary log
either.

### Passwords

| Password | Taken from | When not set |
|---|---|---|
| Your MySQL user (`-p`, `--src-password`, `--dst-password`) | flag → `REPLCTL_*` variable → config | asked for on the terminal |
| `clone_user` (`--clone-password`) | flag → `REPLCTL_CLONE_PASSWORD` → config | random, never shown |
| Replication (`--repl-password`) | flag → `REPLCTL_REPL_PASSWORD` → config | generated and printed once |

Generated passwords are 32 characters long and contain lower and upper case
letters, digits and one of `-_.,+=:^~`, so they pass the `validate_password`
MEDIUM and STRONG policies and need no quoting. A generated replication
password is printed right after the account is created (to stderr with
`--json`); replctl does not store it anywhere. MySQL itself keeps the
replication password on the replica, in plain text, in
`mysql.slave_master_info` — that is how `CHANGE REPLICATION SOURCE TO` works.

All passwords are masked as `****` in dry-run output, `-v` logs and error
messages. Passwords given as flags are visible in `ps` and shell history;
prefer the config file, `REPLCTL_*` variables or the prompt. Passwords may
contain any characters: replctl turns off `NO_BACKSLASH_ESCAPES` in its own
sessions, so a backslash means the same on both servers whatever their
`sql_mode`.

### Privileges of your own user

replctl runs as the user you give it (`root` in most setups). It needs:

- on the source: `CREATE USER`; `BACKUP_ADMIN` and `REPLICATION SLAVE` `WITH
  GRANT OPTION` (to grant them); `SELECT` on `mysql.user` (to find leftover
  `clone_user` accounts); `SYSTEM_VARIABLES_ADMIN` or
  `SESSION_VARIABLES_ADMIN` (for `sql_log_bin`); `INSERT` and `DELETE` on
  `mysql.plugin` (to install the clone plugin);
- on the destination: `CLONE_ADMIN`, `SYSTEM_VARIABLES_ADMIN`,
  `REPLICATION_SLAVE_ADMIN`, plus the same as on the source for the plugin and
  cleanup.

### What the clone carries over

`CLONE` copies the whole data directory except a few local files, so the
destination receives every account of the source — including replication
accounts made for other replicas. Its own `server_uuid` and `server_id` stay
the same.

## Dry run and demo

`--dry-run` prints the exact SQL that would run, without connecting:

```
[dry-run] -- SRC (192.168.1.10)
  SET sql_log_bin=0;
  DROP USER IF EXISTS 'clone_user'@'192.168.1.11';
  CREATE USER 'clone_user'@'192.168.1.11' IDENTIFIED BY '****';
  GRANT BACKUP_ADMIN ON *.* TO 'clone_user'@'192.168.1.11';
  SET sql_log_bin=1;
[dry-run] -- DST (← 192.168.1.10:3306)
  SET GLOBAL clone_valid_donor_list = '192.168.1.10:3306';
  CLONE INSTANCE FROM 'clone_user'@'192.168.1.10':3306 IDENTIFIED BY '****';

Dry run: nothing was changed.
```

`--demo` simulates every command without any MySQL server, for presentations
and trying out the CLI:

```bash
replctl clone -s any-host -d any-host --demo
replctl switch -s source -d replica --demo
replctl lag -d replica1,replica2 --demo --watch
```

## Build

Requires Go 1.26+.

```bash
make build      # bin/replctl, version taken from the VERSION file
make test       # unit tests
make lint       # go vet and gofmt check
make install    # copy to /usr/local/bin (PREFIX=... to change)
make dist       # release binaries for Linux, macOS and Windows in dist/
```

Releases on GitHub are built by [GoReleaser](https://goreleaser.com) in GitHub
Actions when a `v*` tag is pushed; the tag must match the `VERSION` file.
`make dist` builds the same binaries locally.

## Project layout

```
main.go
cmd/
  root.go        cobra root, flags, config values
  config.go      config file location
  common.go      prompts, confirmation, addresses, connections, version
  clone.go       clone command
  switch.go      switch command
  lag.go         lag command (--watch, --json)
  demo.go        demo mode
internal/
  mysql/
    client.go    connection pool and query helpers
    clone.go     clone plugin, progress, verification, cleanup
    inspect.go   server settings, data summary, version checks
    repl.go      replica status, pre-flight checks, CHANGE REPLICATION SOURCE
  dns/
    resolve.go   hostname → IP
```

## License

[MIT](LICENSE)
