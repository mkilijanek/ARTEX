package signalsidecar

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The sidecar talks to PostgreSQL as its own role, which can read one view and
// call three functions and nothing else. Every value goes through the extended
// protocol as a bound parameter (the pgx default), never into statement text.

var (
	// ErrInsecureDSN is returned for a TCP connection string that does not
	// verify the server certificate and name.
	ErrInsecureDSN = errors.New("the database address must be a Unix socket or use sslmode=verify-full")
	// ErrPasswordNotChecked is returned when the database accepted a login with a
	// password that is certainly wrong: it does not check passwords.
	ErrPasswordNotChecked = errors.New("the database accepted a wrong password")
	// ErrProbeInconclusive is returned when the wrong-password probe could not
	// reach a verdict, for instance because the database is not reachable yet.
	ErrProbeInconclusive = errors.New("the password check could not be completed")
	// ErrDBUnavailable is returned while there is no connection to the database.
	ErrDBUnavailable = errors.New("database unavailable")
)

const (
	storeOpTimeout = 5 * time.Second
	dbBackoffMin   = time.Second
	dbBackoffMax   = 15 * time.Second
)

// DBError is a failed database call. Its text carries the SQLSTATE and, for the
// three functions' own errors, their fixed message; never an argument value and
// never text of the driver that could quote one.
type DBError struct {
	Op       string
	SQLState string
	Text     string
	err      error
}

func (e *DBError) Error() string {
	if e.SQLState != "" {
		return fmt.Sprintf("database %s failed (SQLSTATE %s): %s", e.Op, e.SQLState, e.Text)
	}
	return fmt.Sprintf("database %s failed: %s", e.Op, e.Text)
}

func (e *DBError) Unwrap() error { return e.err }

// dbError reduces a driver error to text that is safe to log and to store.
func dbError(op string, err error) *DBError {
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe):
		text := ""
		switch pe.Code {
		case "22023", "55000", "P0002": // raised by the functions with a fixed text
			text = pe.Message
		}
		return &DBError{Op: op, SQLState: pe.Code, Text: text, err: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &DBError{Op: op, Text: "timed out", err: err}
	case errors.Is(err, context.Canceled):
		return &DBError{Op: op, Text: "canceled", err: err}
	case errors.Is(err, ErrDBUnavailable):
		return &DBError{Op: op, Text: ErrDBUnavailable.Error(), err: err}
	}
	return &DBError{Op: op, Text: Scrub(err.Error()), err: err}
}

// ParseStoreConfig parses the connection string of the sidecar role and
// refuses a TCP connection that does not verify the server.
func ParseStoreConfig(dsn string) (*pgx.ConnConfig, error) {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The driver's text can quote the string; say only that it is unusable.
		return nil, errors.New("the database address cannot be parsed")
	}
	if err := CheckConnSecurity(cc); err != nil {
		return nil, err
	}
	return cc, nil
}

// CheckConnSecurity accepts a connection string whose every host is a Unix
// socket or is reached with a TLS configuration that verifies certificate and
// name (sslmode=verify-full).
func CheckConnSecurity(cc *pgx.ConnConfig) error {
	check := func(host string, c *tls.Config) error {
		if strings.HasPrefix(host, "/") {
			return nil
		}
		if c == nil || c.InsecureSkipVerify {
			return ErrInsecureDSN
		}
		return nil
	}
	if err := check(cc.Host, cc.TLSConfig); err != nil {
		return err
	}
	for _, fb := range cc.Fallbacks {
		if err := check(fb.Host, fb.TLSConfig); err != nil {
			return err
		}
	}
	return nil
}

// ProbeWrongPassword connects as the configured role with a random password.
// It returns nil when the database refuses it (password authentication fails),
// ErrPasswordNotChecked when the login succeeds, and an error wrapping
// ErrProbeInconclusive for anything else.
func ProbeWrongPassword(ctx context.Context, cc *pgx.ConnConfig) error {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("random password: %w", err)
	}
	probe := cc.Copy()
	probe.Password = hex.EncodeToString(raw)
	conn, err := pgx.ConnectConfig(ctx, probe)
	if err == nil {
		_ = conn.Close(ctx)
		return ErrPasswordNotChecked
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "28P01" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrProbeInconclusive, dbError("probe", err).Error())
}

// ConfigView is the row of signal_sidecar_config_v.
type ConfigView struct {
	Desired          string
	Gen              int64
	DeviceName       string
	LinkTTLSeconds   int
	ConfirmedAccount string
	GroupsRefreshGen int64
	DBAuthOK         bool
	HandledGen       int64
	GroupsGen        int64
}

// Heartbeat is one call of signal_sidecar_heartbeat.
type Heartbeat struct {
	State          string
	HandledGen     int64
	Number         string
	ACI            string
	Error          string
	RemoteUnlink   string
	SidecarVersion string
	CLIVersion     string
}

// GroupEntry is one group as stored: the id in ARTEX's form.
type GroupEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Store is the sidecar's one database connection. Calls are serialised. A
// broken connection is dropped and re-opened with a backoff of up to 15 s; the
// store never gives up.
type Store struct {
	cc   *pgx.ConnConfig
	logf func(format string, args ...any)

	mu      sync.Mutex
	conn    *pgx.Conn
	nextTry time.Time
	backoff time.Duration
}

// NewStore returns a store that connects on first use.
func NewStore(cc *pgx.ConnConfig, logf func(format string, args ...any)) *Store {
	return &Store{cc: cc, logf: logf}
}

// Close closes the connection.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked()
}

func (s *Store) dropLocked() {
	if s.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.conn.Close(ctx)
		cancel()
		s.conn = nil
	}
}

func (s *Store) do(ctx context.Context, op string, fn func(context.Context, *pgx.Conn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		if time.Now().Before(s.nextTry) {
			return dbError(op, ErrDBUnavailable)
		}
		cctx, cancel := context.WithTimeout(ctx, storeOpTimeout)
		conn, err := pgx.ConnectConfig(cctx, s.cc)
		cancel()
		if err != nil {
			s.backoff = min(max(s.backoff*2, dbBackoffMin), dbBackoffMax)
			s.nextTry = time.Now().Add(s.backoff)
			return dbError(op, err)
		}
		s.conn, s.backoff, s.nextTry = conn, 0, time.Time{}
	}
	octx, cancel := context.WithTimeout(ctx, storeOpTimeout)
	defer cancel()
	err := fn(octx, s.conn)
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || s.conn.IsClosed() {
		s.dropLocked() // the connection state is unknown: reconnect on the next call
	}
	return dbError(op, err)
}

// Config reads the view.
func (s *Store) Config(ctx context.Context) (v ConfigView, err error) {
	err = s.do(ctx, "read the configuration", func(ctx context.Context, c *pgx.Conn) error {
		var ttl int32
		if e := c.QueryRow(ctx, `SELECT desired, gen, device_name, link_ttl_seconds, confirmed_account,
		                                groups_refresh_gen, db_auth_ok, handled_gen, groups_gen
		                           FROM public.signal_sidecar_config_v`).
			Scan(&v.Desired, &v.Gen, &v.DeviceName, &ttl, &v.ConfirmedAccount,
				&v.GroupsRefreshGen, &v.DBAuthOK, &v.HandledGen, &v.GroupsGen); e != nil {
			return e
		}
		v.LinkTTLSeconds = int(ttl)
		return nil
	})
	return v, err
}

// Heartbeat reports the state.
func (s *Store) Heartbeat(ctx context.Context, h Heartbeat) error {
	return s.do(ctx, "heartbeat", func(ctx context.Context, c *pgx.Conn) error {
		_, e := c.Exec(ctx, `SELECT public.signal_sidecar_heartbeat($1, $2, $3, $4, $5, $6, $7, $8)`,
			h.State, h.HandledGen, h.Number, h.ACI, h.Error, h.RemoteUnlink, h.SidecarVersion, h.CLIVersion)
		return e
	})
}

// SetLink stores the link address of generation gen. It returns false when the
// database no longer wants that link; nothing is stored then.
func (s *Store) SetLink(ctx context.Context, gen int64, uri string) (ok bool, err error) {
	err = s.do(ctx, "store the link", func(ctx context.Context, c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT public.signal_sidecar_set_link($1, $2)`, gen, uri).Scan(&ok)
	})
	return ok, err
}

// SetGroups stores the group list for the refresh generation. It returns false
// when the database does not take it (not linked, or a stale generation).
func (s *Store) SetGroups(ctx context.Context, refreshGen int64, groups []GroupEntry) (ok bool, err error) {
	if groups == nil {
		groups = []GroupEntry{}
	}
	raw, err := json.Marshal(groups)
	if err != nil {
		return false, fmt.Errorf("encode groups: %w", err)
	}
	err = s.do(ctx, "store the groups", func(ctx context.Context, c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT public.signal_sidecar_set_groups($1, $2::jsonb)`, refreshGen, string(raw)).Scan(&ok)
	})
	return ok, err
}
