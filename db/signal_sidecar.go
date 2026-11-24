package db

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The Signal sidecar and the server talk through two single-row tables (migration 5004): the configuration
// row, which only the server writes, and the status row, which only the sidecar writes (through three
// functions, never directly). This file is the server's side: a typed read of both rows and the commands.
//
// Every command is one statement that carries its precondition in its WHERE clause and reports whether it
// applied; a statement that finds no row at all is an error. Every transaction on these tables sets a lock
// timeout, so a session that holds a row lock (a compromised sidecar, a stuck psql) makes the statement fail
// with ErrSignalBusy instead of waiting. Lock order: the configuration row first, then the status row, never
// the reverse; the sidecar's functions lock the status row only.
//
// Protocol: gen counts the commands that need the sidecar to act once (link, unlink); handled_gen is the newest
// generation the sidecar has finished. desired says what is asked: 'link', 'unlink' or 'idle'.

// Values of the state column of the status row, and of the desired column of the configuration row.
const (
	SignalStateUnknown  = "unknown"
	SignalStateUnlinked = "unlinked"
	SignalStateLinking  = "linking"
	SignalStateLinked   = "linked"
	SignalStateError    = "error"

	SignalDesiredIdle   = "idle"
	SignalDesiredLink   = "link"
	SignalDesiredUnlink = "unlink"
)

// SignalHeartbeatFresh is how old the heartbeat of the sidecar may be (by the database clock) for the sidecar
// to count as alive and to accept a command that needs it.
const SignalHeartbeatFresh = 30 * time.Second

// signalLockTimeout bounds every wait for a row lock of these tables.
const signalLockTimeout = "3s"

// signalLinkGrace is added to the link lifetime before the server gives up a link that a dead sidecar never
// finished.
const signalLinkGrace = 60 * time.Second

// ErrSignalBusy is returned when a row of the Signal tables stayed locked by another session for longer than
// the lock timeout.
var ErrSignalBusy = errors.New("signal sidecar rows are locked by another session")

// ErrSignalRowMissing is returned when a command finds no configuration or status row (a damaged database).
var ErrSignalRowMissing = errors.New("signal sidecar row missing")

// ErrSignalDeviceName is returned for a device name that the configuration row would refuse.
var ErrSignalDeviceName = errors.New("signal device name must be 1 to 50 characters of letters, digits, space, dot, underscore or hyphen")

var signalDeviceNameRE = regexp.MustCompile(`^[A-Za-z0-9 ._-]{1,50}$`)

// SignalSidecarState is both rows, read in one statement. It carries no link address and no group list.
type SignalSidecarState struct {
	// Configuration row.
	Desired           string
	Gen               int64
	DeviceName        string
	LinkTTLSeconds    int
	ConfirmedAccount  string // the number the operator confirmed, or ""
	GroupsRefreshGen  int64
	DBAuthState       string // unchecked | ok | trust | asserted
	PublicTempRevoked bool
	RequestedBy       string
	RequestedAt       time.Time // zero when no command was ever written

	// Status row.
	State          string
	HandledGen     int64
	AccountNumber  string // full number: for the server's own comparison, never for a response
	AccountACI     string
	LinkExpiresAt  time.Time // zero unless a link is pending
	GroupsCount    int
	GroupsAt       time.Time // zero when no list was stored
	GroupsGen      int64
	RemoteUnlink   string // "" | removed | local_only
	LastError      string
	LastErrorAt    time.Time
	HeartbeatAt    time.Time // zero when the sidecar never reported
	SidecarVersion string
	CLIVersion     string

	// Derived on the database clock.
	Now              time.Time
	HeartbeatAge     time.Duration // meaningful only when HeartbeatAt is not zero
	HeartbeatFresh   bool          // a heartbeat within SignalHeartbeatFresh
	LinkSecondsLeft  float64       // seconds until LinkExpiresAt; negative once expired
	LinkRequestStale bool          // desired is 'link', not handled, and older than the link lifetime plus a grace
}

// SignalLinkInfo is the pending link: the address to show as a QR code.
type SignalLinkInfo struct {
	Gen       int64
	URI       string
	ExpiresAt time.Time
}

// SignalGroup is one entry of the stored group list.
type SignalGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SignalConfirmResult is the outcome of ConfirmSignalAccount.
type SignalConfirmResult int

const (
	// SignalConfirmed: the account is now confirmed.
	SignalConfirmed SignalConfirmResult = iota
	// SignalConfirmStale: the generation is not the current one, or no account is linked.
	SignalConfirmStale
	// SignalConfirmMismatch: the digits are not the end of the linked number.
	SignalConfirmMismatch
)

func isNullTime(t sql.NullTime) time.Time {
	if t.Valid {
		return t.Time
	}
	return time.Time{}
}

// signalTx runs fn in a transaction with a lock timeout. A lock timeout is reported as ErrSignalBusy.
func (d *DB) signalTx(ctx context.Context, what string, fn func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("signal sidecar %s: begin: %w", what, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit; the error that matters is returned below
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout', $1, true)`, signalLockTimeout); err != nil {
		return fmt.Errorf("signal sidecar %s: set lock timeout: %w", what, err)
	}
	if err := fn(tx); err != nil {
		return signalWrap(what, err)
	}
	if err := tx.Commit(); err != nil {
		return signalWrap(what, err)
	}
	return nil
}

func signalWrap(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return fmt.Errorf("signal sidecar %s: %w", what, ErrSignalBusy)
	}
	return fmt.Errorf("signal sidecar %s: %w", what, err)
}

// signalRowsExist reports whether both rows exist; it is asked only after a statement changed nothing.
func signalRowsExist(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM signal_sidecar_config WHERE id)
	                                        + (SELECT count(*) FROM signal_sidecar_status WHERE id)`).Scan(&n); err != nil {
		return err
	}
	if n != 2 {
		return ErrSignalRowMissing
	}
	return nil
}

// ReadSignalSidecar reads the configuration and the status row.
func (d *DB) ReadSignalSidecar(ctx context.Context) (SignalSidecarState, error) {
	var s SignalSidecarState
	var requestedAt, linkExp, groupsAt, errAt, hb sql.NullTime
	var ageSecs, leftSecs sql.NullFloat64
	var stale bool
	err := d.QueryRowContext(ctx, `
SELECT c.desired, c.gen, c.device_name, c.link_ttl_seconds, c.confirmed_account, c.groups_refresh_gen,
       c.db_auth_state, c.public_temp_revoked, c.requested_by, c.requested_at,
       s.state, s.handled_gen, s.account_number, s.account_aci, s.link_expires_at,
       jsonb_array_length(s.groups), s.groups_at, s.groups_gen, s.remote_unlink, s.last_error, s.last_error_at,
       s.heartbeat_at, s.sidecar_version, s.signal_cli_version,
       now(),
       extract(epoch FROM now() - s.heartbeat_at)::float8,
       extract(epoch FROM s.link_expires_at - now())::float8,
       (c.desired = 'link' AND s.handled_gen < c.gen AND c.requested_at IS NOT NULL
         AND c.requested_at < now() - make_interval(secs => c.link_ttl_seconds + $1))
FROM signal_sidecar_config c CROSS JOIN signal_sidecar_status s
WHERE c.id AND s.id`, int(signalLinkGrace.Seconds())).Scan(
		&s.Desired, &s.Gen, &s.DeviceName, &s.LinkTTLSeconds, &s.ConfirmedAccount, &s.GroupsRefreshGen,
		&s.DBAuthState, &s.PublicTempRevoked, &s.RequestedBy, &requestedAt,
		&s.State, &s.HandledGen, &s.AccountNumber, &s.AccountACI, &linkExp,
		&s.GroupsCount, &groupsAt, &s.GroupsGen, &s.RemoteUnlink, &s.LastError, &errAt,
		&hb, &s.SidecarVersion, &s.CLIVersion,
		&s.Now, &ageSecs, &leftSecs, &stale)
	if errors.Is(err, sql.ErrNoRows) {
		return SignalSidecarState{}, fmt.Errorf("read signal sidecar: %w", ErrSignalRowMissing)
	}
	if err != nil {
		return SignalSidecarState{}, fmt.Errorf("read signal sidecar: %w", err)
	}
	s.RequestedAt, s.LinkExpiresAt, s.GroupsAt = isNullTime(requestedAt), isNullTime(linkExp), isNullTime(groupsAt)
	s.LastErrorAt, s.HeartbeatAt = isNullTime(errAt), isNullTime(hb)
	if ageSecs.Valid {
		s.HeartbeatAge = time.Duration(ageSecs.Float64 * float64(time.Second))
		s.HeartbeatFresh = s.HeartbeatAge >= 0 && s.HeartbeatAge <= SignalHeartbeatFresh
	}
	if leftSecs.Valid {
		s.LinkSecondsLeft = leftSecs.Float64
	}
	s.LinkRequestStale = stale
	return s, nil
}

// ReadSignalLink returns the pending link address. It reports false unless a link is requested, the sidecar
// stored the address and the expiry (by the database clock) has not passed.
func (d *DB) ReadSignalLink(ctx context.Context) (SignalLinkInfo, bool, error) {
	var info SignalLinkInfo
	err := d.QueryRowContext(ctx, `
SELECT c.gen, s.link_uri, s.link_expires_at
FROM signal_sidecar_config c CROSS JOIN signal_sidecar_status s
WHERE c.id AND s.id AND c.desired = 'link' AND s.state = 'linking'
  AND s.link_uri IS NOT NULL AND s.link_expires_at > now()`).Scan(&info.Gen, &info.URI, &info.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SignalLinkInfo{}, false, nil
	}
	if err != nil {
		return SignalLinkInfo{}, false, fmt.Errorf("read signal link: %w", err)
	}
	return info, true, nil
}

// ReadSignalGroups returns the stored group list and the time it was stored (zero when never).
func (d *DB) ReadSignalGroups(ctx context.Context) ([]SignalGroup, time.Time, error) {
	var raw []byte
	var at sql.NullTime
	if err := d.QueryRowContext(ctx, `SELECT groups, groups_at FROM signal_sidecar_status WHERE id`).Scan(&raw, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, time.Time{}, fmt.Errorf("read signal groups: %w", ErrSignalRowMissing)
		}
		return nil, time.Time{}, fmt.Errorf("read signal groups: %w", err)
	}
	var groups []SignalGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, time.Time{}, fmt.Errorf("read signal groups: stored list is not a list of groups")
	}
	return groups, isNullTime(at), nil
}

// reconcileSignal resets desired to 'idle' for a command the sidecar has finished, and gives up a link that a
// dead sidecar never finished (the request is older than the link lifetime plus a grace). It runs inside the
// transaction of the command that needs the answer, and on its own from ReconcileSignalSidecar.
func reconcileSignal(ctx context.Context, tx *sql.Tx) (bool, error) {
	res, err := tx.ExecContext(ctx, `
UPDATE signal_sidecar_config c SET desired = 'idle', updated_at = now()
FROM signal_sidecar_status s
WHERE c.id AND s.id AND c.desired <> 'idle'
  AND (s.handled_gen = c.gen
       OR (c.desired = 'link' AND s.handled_gen < c.gen AND c.requested_at IS NOT NULL
           AND c.requested_at < now() - make_interval(secs => c.link_ttl_seconds + $1)))`,
		int(signalLinkGrace.Seconds()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReconcileSignalSidecar is the server's reconciliation, run on every status read: when the sidecar has
// finished the current generation, or when a link request has outlived its lifetime plus a grace, desired
// returns to 'idle'. It reports whether it changed the row.
func (d *DB) ReconcileSignalSidecar(ctx context.Context) (bool, error) {
	var changed bool
	err := d.signalTx(ctx, "reconcile", func(tx *sql.Tx) error {
		var err error
		changed, err = reconcileSignal(ctx, tx)
		return err
	})
	return changed, err
}

// RequestSignalLink asks the sidecar for a link. It applies only when nothing is pending, the state is
// 'unlinked' and the sidecar's heartbeat is fresh; the confirmation is cleared in the same statement. gen is the
// new generation when applied.
func (d *DB) RequestSignalLink(ctx context.Context, deviceName, requestedBy string) (gen int64, applied bool, err error) {
	if !signalDeviceNameRE.MatchString(deviceName) {
		return 0, false, ErrSignalDeviceName
	}
	err = d.signalTx(ctx, "link", func(tx *sql.Tx) error {
		if _, err := reconcileSignal(ctx, tx); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `
UPDATE signal_sidecar_config c SET desired = 'link', gen = c.gen + 1, confirmed_account = '',
       device_name = $1, requested_by = $2, requested_at = now(), updated_at = now()
FROM signal_sidecar_status s
WHERE c.id AND s.id AND c.desired = 'idle' AND s.state = 'unlinked'
  AND s.heartbeat_at > now() - make_interval(secs => $3)
RETURNING c.gen`, deviceName, requestedBy, SignalHeartbeatFresh.Seconds())
		switch err := row.Scan(&gen); {
		case err == nil:
			applied = true
			return nil
		case errors.Is(err, sql.ErrNoRows):
			return signalRowsExist(ctx, tx)
		default:
			return err
		}
	})
	if err != nil {
		return 0, false, err
	}
	return gen, applied, nil
}

// CancelSignalLink withdraws a pending link request. It applies only while desired is 'link'; the generation
// is unchanged.
func (d *DB) CancelSignalLink(ctx context.Context) (bool, error) {
	return d.signalConfigUpdate(ctx, "cancel", `
UPDATE signal_sidecar_config SET desired = 'idle', updated_at = now()
WHERE id AND desired = 'link'`)
}

// RequestSignalUnlink asks the sidecar to forget the account. It applies only when nothing is pending, the
// state is 'linked' or 'error' and the sidecar's heartbeat is fresh; the confirmation is cleared in the same
// statement.
func (d *DB) RequestSignalUnlink(ctx context.Context, requestedBy string) (gen int64, applied bool, err error) {
	err = d.signalTx(ctx, "unlink", func(tx *sql.Tx) error {
		if _, err := reconcileSignal(ctx, tx); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `
UPDATE signal_sidecar_config c SET desired = 'unlink', gen = c.gen + 1, confirmed_account = '',
       requested_by = $1, requested_at = now(), updated_at = now()
FROM signal_sidecar_status s
WHERE c.id AND s.id AND c.desired = 'idle' AND s.state IN ('linked','error')
  AND s.heartbeat_at > now() - make_interval(secs => $2)
RETURNING c.gen`, requestedBy, SignalHeartbeatFresh.Seconds())
		switch err := row.Scan(&gen); {
		case err == nil:
			applied = true
			return nil
		case errors.Is(err, sql.ErrNoRows):
			return signalRowsExist(ctx, tx)
		default:
			return err
		}
	})
	if err != nil {
		return 0, false, err
	}
	return gen, applied, nil
}

// RequestSignalGroupsRefresh asks the sidecar to list the groups again. It applies only in state 'linked'.
func (d *DB) RequestSignalGroupsRefresh(ctx context.Context) (bool, error) {
	var applied bool
	err := d.signalTx(ctx, "groups refresh", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE signal_sidecar_config c SET groups_refresh_gen = c.groups_refresh_gen + 1, updated_at = now()
FROM signal_sidecar_status s
WHERE c.id AND s.id AND s.state = 'linked'`)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			applied = true
			return nil
		}
		return signalRowsExist(ctx, tx)
	})
	return applied, err
}

// ConfirmSignalAccount stores the linked number as the confirmed account when gen is the current generation,
// an account is linked and lastDigits equals the last six digits of its number. The two rows are locked in the
// documented order, so the answer and the write belong to the same state.
func (d *DB) ConfirmSignalAccount(ctx context.Context, gen int64, lastDigits string) (SignalConfirmResult, error) {
	result := SignalConfirmStale
	err := d.signalTx(ctx, "confirm", func(tx *sql.Tx) error {
		var curGen int64
		if err := tx.QueryRowContext(ctx, `SELECT gen FROM signal_sidecar_config WHERE id FOR UPDATE`).Scan(&curGen); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSignalRowMissing
			}
			return err
		}
		var state, number string
		if err := tx.QueryRowContext(ctx, `SELECT state, account_number FROM signal_sidecar_status WHERE id FOR SHARE`).Scan(&state, &number); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSignalRowMissing
			}
			return err
		}
		if curGen != gen || state != SignalStateLinked || len(number) < 6 {
			result = SignalConfirmStale
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(number[len(number)-6:]), []byte(lastDigits)) != 1 {
			result = SignalConfirmMismatch
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE signal_sidecar_config SET confirmed_account = $1, updated_at = now() WHERE id`, number); err != nil {
			return err
		}
		result = SignalConfirmed
		return nil
	})
	if err != nil {
		return SignalConfirmStale, err
	}
	return result, nil
}

func (d *DB) signalConfigUpdate(ctx context.Context, what, stmt string) (bool, error) {
	var applied bool
	err := d.signalTx(ctx, what, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, stmt)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			applied = true
			return nil
		}
		return signalRowsExist(ctx, tx)
	})
	return applied, err
}
