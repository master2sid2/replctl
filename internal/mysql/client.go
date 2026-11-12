package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Config holds connection parameters.
type Config struct {
	Host        string
	Port        int
	User        string
	Password    string
	Timeout     time.Duration // TCP connect timeout
	ReadTimeout time.Duration // SQL read timeout; 0 = same as Timeout; negative = no timeout
}

// Client wraps *sql.DB with convenience methods.
type Client struct {
	db  *sql.DB
	cfg Config
}

// NewClient opens a MySQL connection and verifies connectivity.
func NewClient(cfg Config) (*Client, error) {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	var dsn string
	if cfg.ReadTimeout < 0 {
		// No read timeout (e.g. long-running CLONE statement).
		dsn = fmt.Sprintf(
			"%s:%s@tcp(%s:%d)/?timeout=%s&parseTime=true&interpolateParams=true",
			cfg.User, cfg.Password, cfg.Host, cfg.Port, timeout.String(),
		)
	} else {
		readTimeout := cfg.ReadTimeout
		if readTimeout == 0 {
			readTimeout = timeout
		}
		dsn = fmt.Sprintf(
			"%s:%s@tcp(%s:%d)/?timeout=%s&readTimeout=%s&parseTime=true&interpolateParams=true",
			cfg.User, cfg.Password, cfg.Host, cfg.Port,
			timeout.String(), readTimeout.String(),
		)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s:%d: %w", cfg.Host, cfg.Port, err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &Client{db: db, cfg: cfg}, nil
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
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, rows.Err()
	}

	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(cols))
	for i, col := range cols {
		m[col] = string(vals[i])
	}
	return m, rows.Err()
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
	for rows.Next() {
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]string, len(cols))
		for i, col := range cols {
			m[col] = string(vals[i])
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// Host returns the configured host.
func (c *Client) Host() string { return c.cfg.Host }

// Close releases the connection pool.
func (c *Client) Close() error { return c.db.Close() }
