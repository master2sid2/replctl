package mysql

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"time"

	gmysql "github.com/go-sql-driver/mysql"
)

// Config holds connection parameters.
type Config struct {
	Host        string
	Port        int
	User        string
	Password    string
	Timeout     time.Duration // TCP connect timeout; 0 = 30s
	ReadTimeout time.Duration // SQL read timeout; 0 = same as Timeout; negative = no timeout
}

// driverConfig builds the driver configuration. Going through the driver's
// own type instead of a DSN string keeps IPv6 hosts and passwords with
// special characters intact.
func (cfg Config) driverConfig() *gmysql.Config {
	c := gmysql.NewConfig()
	c.User = cfg.User
	c.Passwd = cfg.Password
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	c.Timeout = cmp.Or(cfg.Timeout, 30*time.Second)
	switch {
	case cfg.ReadTimeout < 0:
		// No read timeout (e.g. long-running CLONE statement).
	case cfg.ReadTimeout == 0:
		c.ReadTimeout = c.Timeout
	default:
		c.ReadTimeout = cfg.ReadTimeout
	}
	c.ParseTime = true
	c.InterpolateParams = true
	return c
}

// Client wraps *sql.DB with convenience methods.
type Client struct {
	db  *sql.DB
	cfg Config
}

// NewClient prepares a connection pool; nothing is dialled until first use,
// so call Ping to check the server is reachable.
func NewClient(cfg Config) *Client {
	conn, err := gmysql.NewConnector(cfg.driverConfig())
	if err != nil {
		// Only possible for an invalid config, which driverConfig never builds.
		panic(fmt.Sprintf("mysql connector: %v", err))
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &Client{db: db, cfg: cfg}
}

// Ping checks server reachability.
func (c *Client) Ping(ctx context.Context) error {
	return c.db.PingContext(ctx)
}

// ExecOne executes a single statement.
func (c *Client) ExecOne(ctx context.Context, query string) error {
	_, err := c.db.ExecContext(ctx, query)
	return err
}

// ExecSession runs all queries on one dedicated connection, preserving session state.
// Useful for SET sql_log_bin + DDL sequences.
func (c *Client) ExecSession(ctx context.Context, queries []string) error {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn on %s: %w", c.cfg.Host, err)
	}
	defer conn.Close()
	for _, q := range queries {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("exec on %s %q: %w", c.cfg.Host, maskSQL(q), err)
		}
	}
	return nil
}

// QueryMap runs a query and returns the first row as a column→value map.
// Returns nil if there are no rows.
func (c *Client) QueryMap(ctx context.Context, query string) (map[string]string, error) {
	rows, err := c.QueryRows(ctx, query)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// QueryRows runs a query and returns all rows as a slice of column→value maps.
func (c *Client) QueryRows(ctx context.Context, query string) ([]map[string]string, error) {
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var result []map[string]string
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]string, len(cols))
		for i, col := range cols {
			m[col] = string(vals[i]) // copies, RawBytes is reused by the next Scan
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// Host returns the configured host.
func (c *Client) Host() string { return c.cfg.Host }

// Close releases the connection pool.
func (c *Client) Close() error { return c.db.Close() }
