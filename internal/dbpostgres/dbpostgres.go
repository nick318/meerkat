// Package dbpostgres is the PostgreSQL driver, backed by pgx.
//
// Editing is not built yet: [PostgresConnection.Apply] returns an error,
// exactly as the SQLite driver does. A *typed* statement, though, goes to
// the server verbatim, so a session the user marked read-only asks the
// server for a read-only one — see `connectWith`. Introspection reads
// `pg_catalog` rather than
// `information_schema` for tables and columns, because `pg_catalog` also
// carries the `reltuples` row estimate and `format_type` renders the type
// names the way `psql` shows them. Primary keys come from
// `information_schema`, which already reports the key column order.
// Indexes, constraints, triggers, sequences, routines and types come
// from `pg_catalog` too, each rendered by the server's own `pg_get_*`
// function so a definition reads the way `\d` prints it.
//
// Go port of `crates/db_postgres`, which was backed by sqlx. The source is
// split by topic: this file is the connection and the session,
// `catalog.go` the introspection, `decode.go` the value decoding, and
// `sqlxcompat.go` what sqlx did for the Rust build that pgx does not.
package dbpostgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/introspect"
)

// Two pools, and the split is the point.
//
// `APP_CONNECTIONS` serves what the app does for itself: introspection,
// table pages, and — the one that matters — `pg_cancel_backend`. Stopping
// a run must never wait for a free connection, because the connection it
// would be waiting for is the very statement the user asked to stop.
//
// `db_client::MAX_SESSIONS` is the ceiling on live tabs, one pinned
// connection each, and the app knows the same number — see it there.
// Keeping them in a pool of their own is what stops a wall of open tabs
// from starving the app's own work; a reserve written as a comment over
// one shared pool would be a reserve until the day it was not.
const appConnections int32 = 3
const connectTimeout = 10 * time.Second

// reach knocks on the server's port before the pool does.
//
// **sqlx's pool hides the two failures a person most needs named.** Its
// connect loop reads "connection refused" as a server that is still
// starting up, and retries with a backoff until `acquire_timeout` runs
// out — so a wrong port, which the system reports in a millisecond, came
// back ten seconds later as "pool timed out while waiting for an open
// connection", in the same words as a host that never answered. One bare
// TCP connect tells them apart, and says "refused", "no route" or "no
// such host" at once.
//
// It costs one TCP handshake — one round trip, with no TLS and no
// sign-in behind it — and the deadline is the pool's own, so a firewall
// that drops packets is given up on at `CONNECT_TIMEOUT` here rather
// than after it. A Unix socket is left to the pool: there is no network
// to ask about.
//
// Go port: pgx's pool does not retry a refused connect, but the knock is
// kept — it is the same one round trip, and it is what words the failure
// as `host:port · …` before any pool is built.
func reach(ctx context.Context, options *connectOptions) error {
	host := options.host
	if options.socket != nil || strings.HasPrefix(host, "/") {
		return nil
	}
	port := options.port
	knockCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(knockCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err == nil {
		_ = conn.Close()
		return nil
	}
	if knockCtx.Err() != nil {
		return dbclient.NewConnectError(dbclient.ConnectFailureTimeout, noAnswer(host, port))
	}
	message := fmt.Sprintf("%s:%d · %s", host, port, err)
	// Go port: Go's resolver words a failed lookup as "no such host",
	// which none of `classify_connect`'s needles (getaddrinfo's wording)
	// match, so the lookup is recognised by its type here.
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dbclient.NewConnectError(dbclient.ConnectFailureDns, message)
	}
	evidence := dbclient.ConnectEvidence{Message: message}
	if kind, ok := dbclient.IoErrorKind(err); ok {
		evidence.Io = &kind
	}
	return dbclient.ConnectErrorFromEvidence(evidence)
}

// noAnswer is what a deadline that ran out is worded as: the driver's own
// sentence names a pool, which is nothing the person looking at it asked
// about.
func noAnswer(host string, port uint16) string {
	return fmt.Sprintf("%s:%d did not answer within %d s", host, port, int(connectTimeout.Seconds()))
}

// errPoolTimedOut is sqlx's `Error::PoolTimedOut`: the pool's acquire
// deadline, `CONNECT_TIMEOUT`, ran out. Go port: pgx's pool waits on the
// context it is given, so `acquire` sets that deadline and reports it as
// this.
var errPoolTimedOut = errors.New("pool timed out while waiting for an open connection")

// connectError turns what pgx said about a failed connect into a class
// and a detail. The class is `db_client`'s to decide; this only reads the
// evidence out of the library's error.
//
// Go port: sqlx's error was one enum and its variants were disjoint; a pgx
// error is a chain, and a TLS failure or the pool's deadline can carry a
// socket error inside it. So the readings are tried from the most
// specific outwards — the server's refusal, the pool's deadline, the TLS
// layer, the socket — and the rest are `other`, as in the Rust match.
func connectError(err error, label *Label) dbclient.ConnectError {
	var refused *pgconn.PgError
	if errors.As(err, &refused) {
		code := refused.Code
		return dbclient.ConnectErrorFromEvidence(dbclient.ConnectEvidence{
			Sqlstate: &code,
			Message:  refused.Message,
		})
	}
	// The port answered the knock, so this is the server taking too
	// long over the handshake — or one the pool kept retrying because
	// it said it was starting up or out of connections.
	if errors.Is(err, errPoolTimedOut) {
		return dbclient.NewConnectError(
			dbclient.ConnectFailureTimeout,
			fmt.Sprintf(
				"%s · the port is open, and the server did not finish the sign-in",
				noAnswer(label.Host, label.Port),
			),
		)
	}
	if isTLSError(err) {
		return dbclient.ConnectErrorFromEvidence(dbclient.ConnectEvidence{
			Tls:     true,
			Message: err.Error(),
		})
	}
	if kind, ok := dbclient.IoErrorKind(err); ok {
		message := err.Error()
		var op *net.OpError
		if errors.As(err, &op) {
			message = op.Error()
		}
		return dbclient.ConnectErrorFromEvidence(dbclient.ConnectEvidence{
			Io: &kind,
			// rustls reports a handshake it gave up on as an I/O
			// error of invalid data, with its own words inside.
			Tls:     kind == dbclient.ErrorKindInvalidData && namesTLS(message),
			Message: message,
		})
	}
	return dbclient.ConnectErrorFromEvidence(dbclient.ConnectEvidence{
		Message: err.Error(),
	})
}

// isTLSError is sqlx's `Error::Tls`, as Go's TLS stack and pgconn report
// it: a certificate the handshake would not accept, an alert or a record
// the peer sent, or a server that refused TLS outright.
func isTLSError(err error) bool {
	var verification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &verification) || errors.As(err, &unknownAuthority) ||
		errors.As(err, &invalid) || errors.As(err, &hostname) ||
		errors.As(err, &record) || errors.As(err, &alert) {
		return true
	}
	// pgconn's own words when the server answers the TLS request with "no".
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e.Error() == "server refused TLS connection" {
			return true
		}
	}
	return false
}

func namesTLS(message string) bool {
	lowered := asciiLowercase(message)
	for _, word := range []string{"certificate", "handshake", "tls"} {
		if strings.Contains(lowered, word) {
			return true
		}
	}
	return false
}

// queryCanceled is the SQLSTATE for `query_canceled`. The server answers a
// statement `pg_cancel_backend` reached with it — and a statement its own
// `statement_timeout` ran out on, which is the same event from the
// server's side: it gave the statement up.
const queryCanceled = "57014"

// syntaxError is the SQLSTATE for `syntax_error`. The scanner and the
// grammar both refuse with it — an unterminated string, a missing bracket,
// a keyword where a name belongs — and nothing that needs the catalog
// does. That is what makes it true of the statement rather than of the
// connection it was asked on: see [dbclient.Refusal].
const syntaxError = "42601"

// nameErrors are the SQLSTATEs for a name the server could not resolve.
// `42P01` covers a missing schema as well as a missing table — the server
// reports `nosuchschema.t` as a relation that does not exist rather than
// as a bad schema name — so four codes are the whole of it.
//
// `42P18` (indeterminate datatype) is deliberately **not** here. A
// statement holding a bare `$1` raises it, and a placeholder is a thing
// the user typed on purpose, not a name they got wrong.
var nameErrors = []string{
	// undefined_table, and a schema that does not exist with it.
	"42P01", // undefined_column.
	"42703", // undefined_function.
	"42883", // undefined_object, which is what a type that does not exist is.
	"42704",
}

// relationOn asks the server whether it has a relation of this name.
//
// `to_regclass` is the whole of it, and it is the right call rather than
// a query over `pg_class`: it resolves the name the way the *parser*
// would — through `search_path` when it is unqualified, folding an
// unquoted part to lower case and keeping a quoted one — and it answers
// NULL rather than raising when there is nothing there, including for a
// name that is not a name at all.
//
// It is asked on whichever connection carried it, so a session sees its
// own temp tables and its own `search_path`.
func relationOn(ctx context.Context, conn *pgx.Conn, name string) (exists bool, known bool, err error) {
	var found bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&found); err != nil {
		return false, false, fmt.Errorf("failed to ask the server about a relation: %w", err)
	}
	return found, true, nil
}

// checkStatement asks the server to prepare one statement, and throws the
// result away.
//
// **Nothing runs.** `prepare_with` rather than `describe`, and the
// difference is a round trip: `describe` follows the parse with a catalog
// query for the nullability of each column, which nothing here reads.
// sqlx keeps the prepared statement in the connection's own LRU cache and
// sends a `Close` when it falls out of it, so a session of typing leaves a
// bounded number of them on the server rather than one per keystroke.
//
// Go port: the statement is prepared as the *unnamed* statement, which
// the next unnamed prepare replaces, so nothing is left on the server to
// close at all.
//
// Both callers share this, and the connection is the whole difference
// between them: names resolve against whatever carried the question. See
// [dbclient.CheckError].
func checkStatement(ctx context.Context, conn *pgx.Conn, sql string) *dbclient.CheckError {
	_, err := conn.PgConn().Prepare(ctx, "", sql, nil)
	if err == nil {
		return nil
	}
	var refused *pgconn.PgError
	if !errors.As(err, &refused) {
		// A dropped connection is not the statement's fault, and the
		// caller has nowhere to put it: a check nobody asked for must not
		// raise an error over a query that has not been run.
		return nil
	}
	var refusal dbclient.Refusal
	switch code := refused.Code; {
	case code == syntaxError:
		refusal = dbclient.RefusalSyntax
	case slices.Contains(nameErrors, code):
		refusal = dbclient.RefusalName
	// Everything else the parser can raise is about neither the text
	// nor a name — a transaction that has gone wrong, a permission,
	// an `$1` with no type to infer — and none of it is worth a mark
	// over a statement nobody has run.
	default:
		return nil
	}
	// `Internal` names a position inside a statement the *server*
	// generated — the body of a PL/pgSQL function, say — which is not an
	// offset into anything the editor holds.
	var offset *int
	if refused.Position > 0 {
		if at, ok := byteOffset(sql, int(refused.Position)); ok {
			offset = &at
		}
	}
	return &dbclient.CheckError{
		Refusal: refusal,
		Message: refused.Message,
		Offset:  offset,
	}
}

// statementTimeout is how long a run may take before the server gives it
// up, when nothing else has an opinion. A viewer must not leave a
// statement on a shared server for ever because a window is open
// somewhere.
const statementTimeout = "30s"

// PostgresConnection is one database: two pools onto it, and what the app
// says about it.
type PostgresConnection struct {
	// The app's own work: introspection, table pages, and every cancel.
	pool *pgxpool.Pool
	// One pinned connection per open session. See `APP_CONNECTIONS`.
	sessions *pgxpool.Pool
	label    Label
	limits   dbclient.Limits
	// What a run's `statement_timeout` is set to **when nothing else has
	// set one**, asked for beside the backend id: once per session, or
	// once per run on the pooled path, where every run is a new backend.
	//
	// Postgres says where a setting came from: `pg_settings.source` reads
	// `default` only when nothing anywhere named a value. A role
	// (`ALTER ROLE ... SET`) reads `user`, a database `database`, the
	// config file `configuration file`, a startup option in the connection
	// URL `client`, and a `SET` the user typed `session`. So the app can
	// tell its own silence apart from somebody's choice, and it defers to
	// every one of those — including a deliberate `0`, which `default`
	// would never report.
	//
	// This is why the timeout is **not** a startup option, though
	// `default_transaction_read_only` is. A startup option outranks
	// `ALTER ROLE`, so asking for one would quietly overrule the DBA — and
	// it is fixed before the connect, so it cannot be conditional on what
	// the server turns out to say. The read-only flag has the opposite
	// need: it is a promise, so it must survive `RESET ALL`, and there is
	// nothing on the server to defer to.
	//
	// The trade is that a `RESET ALL` washes the guard off that pooled
	// connection. The next run puts it back, because every run asks — a
	// timeout is a guard, not a promise.
	statementTimeout string
	// Go port: sqlx's per-connection `cache_type_info`. See `typeCache`.
	types *typeCache
}

var _ dbclient.Connection = (*PostgresConnection)(nil)

// Label is what the header and the sidebar card say about this
// connection. The pool does not hand these back once it is built, so keep
// them.
type Label struct {
	Database string
	Host     string
	Port     uint16
}

// Connect connects from a `postgres://user:password@host:port/database`
// URL. A URL carries no saved settings, so the session is read-only: the
// app must not be a way to write to a database by accident.
func Connect(ctx context.Context, url string) (*PostgresConnection, error) {
	return ConnectURL(ctx, url, true)
}

// ConnectURL is the same, saying outright whether the session may write.
// Only the driver's own tests need a writable one from a URL — the app
// opens a writable session from a profile, where the user chose it.
func ConnectURL(ctx context.Context, url string, readOnly bool) (*PostgresConnection, error) {
	options, err := parseConnectURL(url)
	if err != nil {
		// The URL is left out of the message: it holds the password, and
		// this text is painted on screen.
		return nil, fmt.Errorf("not a valid PostgreSQL URL: %w", err)
	}
	return connectWith(ctx, options, readOnly)
}

// ConnectProfile connects from a saved profile. The password comes from
// the OS keychain, keyed by the profile id; a profile without a stored
// password connects without one (trust, peer, or `.pgpass`).
func ConnectProfile(ctx context.Context, profile *dbclient.Profile) (*PostgresConnection, error) {
	return ConnectProbe(ctx, profile, PasswordKeychain{})
}

// ConnectProbe connects from a profile, saying where the password comes
// from. It is what the connection form's "test" needs: the credentials it
// tests have not been saved, so there may be nothing in the keychain under
// that id to read, and "no password" is a different answer from "look
// one up".
func ConnectProbe(ctx context.Context, profile *dbclient.Profile, password Password) (*PostgresConnection, error) {
	options := newConnectOptions()
	database := profile.Database
	options.database = &database
	if profile.Host != nil {
		options.host = *profile.Host
	}
	if profile.Port != nil {
		options.port = *profile.Port
	}
	if profile.User != nil {
		options.username = *profile.User
	}
	switch password := password.(type) {
	case PasswordKeychain:
		if KeychainPassword == nil {
			return nil, errors.New("no keychain is wired into the PostgreSQL driver")
		}
		stored, ok, err := KeychainPassword(profile.Id)
		if err != nil {
			return nil, err
		}
		if ok {
			options.password = &stored
		}
	case PasswordGiven:
		if password.Password != nil {
			given := *password.Password
			options.password = &given
		}
	}
	return connectWith(ctx, options, profile.ReadOnly)
}

// KeychainPassword reads a saved profile's password out of the OS
// keychain: the password and true, or false when nothing is stored under
// that id.
//
// Go port: the Rust driver called `secrets::get_password` directly. The
// `secrets` package is ported beside this one, so the app wires it in at
// boot (`dbpostgres.KeychainPassword = secrets.GetPassword`) rather than
// this package importing it.
var KeychainPassword func(profileID string) (string, bool, error)

func connectWith(ctx context.Context, options connectOptions, readOnly bool) (*PostgresConnection, error) {
	label := Label{
		Database: deref(options.database),
		Host:     options.host,
		Port:     options.port,
	}
	// A read-only session is the *server's* promise, not the app's: the
	// startup packet asks for `default_transaction_read_only`, and
	// `INSERT`, `UPDATE`, `DELETE` and DDL then come back as errors
	// from Postgres itself. Nothing here reads the SQL, so there is no
	// pattern to slip past.
	//
	// It goes in the startup options rather than a `SET` after connect
	// because `RESET ALL` — which `DISCARD ALL` runs, and a pool may —
	// restores a parameter to the value the session *started* with. A
	// `SET` would be washed away by exactly the kind of reset a pool
	// does between one tab and the next; a startup option survives it.
	//
	// A connection pooler that refuses the `options` startup parameter
	// fails the connect. That is the right way round: a read-only
	// session that cannot be asked for must not open at all.
	if readOnly {
		options.addOption("default_transaction_read_only", "on")
	}
	if err := reach(ctx, &options); err != nil {
		return nil, err
	}
	appConfig, err := options.poolConfig(appConnections)
	if err != nil {
		return nil, connectError(err, &label)
	}
	pool, err := pgxpool.NewWithConfig(ctx, appConfig)
	if err != nil {
		return nil, connectError(err, &label)
	}
	// sqlx's `connect_with` opens one connection before it hands the pool
	// back, so a connect that cannot sign in fails here and not on the
	// first statement.
	first, err := acquire(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, connectError(err, &label)
	}
	first.Release()
	// The same options, so a session's startup packet is the app's:
	// one read-only flag, one set of parameters, no second story about
	// what this connection may do. Lazily connected — a session pool
	// that opened eight sockets to serve nobody would be worse than
	// the pooling it replaces.
	sessionConfig, err := options.poolConfig(int32(dbclient.MaxSessions))
	if err != nil {
		pool.Close()
		return nil, connectError(err, &label)
	}
	sessions, err := pgxpool.NewWithConfig(ctx, sessionConfig)
	if err != nil {
		pool.Close()
		return nil, connectError(err, &label)
	}
	return &PostgresConnection{
		pool:             pool,
		sessions:         sessions,
		label:            label,
		limits:           dbclient.DefaultLimits(),
		statementTimeout: statementTimeout,
		types:            newTypeCache(),
	}, nil
}

// WithLimits holds a smaller result than the app's own budget. Only the
// tests need this: a cap of a few kilobytes is reached in a query that
// takes no time, where the real one would want gigabytes of fixture.
func (c *PostgresConnection) WithLimits(limits dbclient.Limits) *PostgresConnection {
	c.limits = limits
	return c
}

// WithStatementTimeout gives up a run sooner than [statementTimeout]
// would. For the tests, which cannot wait out thirty seconds to watch one
// land.
func (c *PostgresConnection) WithStatementTimeout(timeout string) *PostgresConnection {
	c.statementTimeout = timeout
	return c
}

func (c *PostgresConnection) Label() *Label {
	return &c.label
}

// Close hands the server's connections back now. Dropping the pool closes
// them eventually; a connections screen that probes several databases in
// a row should not wait for that.
//
// Go port: nothing drops a Go pool, so this also closes the session pool —
// in the background, because pgx waits there for every session still
// holding a connection, and a session nobody closed gives its connection
// back only when the collector finds it (see `PostgresSession`).
func (c *PostgresConnection) Close(ctx context.Context) {
	c.pool.Close()
	go c.sessions.Close()
}

// Password is where a connect gets its password: [PasswordKeychain] or
// [PasswordGiven].
//
// The keychain is the ordinary answer — a saved profile keeps its password
// there, keyed by its id — but the connection form tests credentials that
// have not been saved, and for those there is nothing under that id to
// read. `Given(None)` is then "connect without a password" (trust, peer or
// `.pgpass`), which is a different request from "look one up".
type Password interface {
	isPassword()
}

// PasswordKeychain looks the password up in the keychain.
type PasswordKeychain struct{}

// PasswordGiven connects with this password, or with none when it is nil.
type PasswordGiven struct {
	Password *string
}

func (PasswordKeychain) isPassword() {}
func (PasswordGiven) isPassword()    {}

// ProfileFromURL splits a `postgres://user:password@host:port/database`
// URL into a saved profile and the password. The password is handed back
// separately because it belongs in the OS keychain, never in the profiles
// file.
//
// The URL is parsed the way the driver parses it at connect time, so
// anything the driver would accept is accepted here, and a typo is caught
// while the user is still looking at the form.
func ProfileFromURL(id, name, url string) (dbclient.Profile, *string, error) {
	options, err := parseConnectURL(url)
	if err != nil {
		// The URL is left out of the message: it holds the password, and
		// this text is painted on screen.
		return dbclient.Profile{}, nil, fmt.Errorf("not a valid PostgreSQL URL: %w", err)
	}
	if options.database == nil || *options.database == "" {
		return dbclient.Profile{}, nil, errors.New("the URL names no database")
	}
	database := *options.database
	username := options.username

	profileName := strings.TrimSpace(name)
	// An unnamed connection goes by its database, as the design's
	// rows do: `meerkat_prod` over the URL it came from.
	if profileName == "" {
		profileName = database
	}
	host := options.host
	port := options.port
	var userPtr *string
	if username != "" {
		userPtr = &username
	}
	profile := dbclient.Profile{
		Id:       id,
		Name:     profileName,
		Engine:   dbclient.EnginePostgres,
		Host:     &host,
		Port:     &port,
		Database: database,
		User:     userPtr,
		// A URL says nothing about how careful to be, so the caller sets
		// the flag from the form. Read-only is the value a new connection
		// starts on.
		ReadOnly: true,
	}
	return profile, passwordIn(url), nil
}

// passwordIn is the password inside a URL's credentials, if it carries
// one.
func passwordIn(url string) *string {
	_, rest, ok := strings.Cut(url, "://")
	if !ok {
		return nil
	}
	credentials, _, ok := strings.Cut(rest, "@")
	if !ok {
		return nil
	}
	_, password, ok := strings.Cut(credentials, ":")
	if !ok || password == "" {
		return nil
	}
	return &password
}

// shortVersion: `16.2 (Debian 16.2-1.pgdg120+2)` is more than a connection
// row needs.
func shortVersion(reported string) string {
	if fields := strings.Fields(reported); len(fields) > 0 {
		return fields[0]
	}
	return reported
}

// byteOffset is where the server's cursor lands in the statement, in
// bytes.
//
// Postgres counts the position in **characters** and counts from one; the
// app counts bytes from zero, as everything above the driver does. A
// position one past the last character — which is what "syntax error at
// end of input" reports — is the length of the statement, and is a
// position like any other: the caller has a rule for marking the end.
func byteOffset(sql string, position int) (int, bool) {
	if position < 1 {
		return 0, false
	}
	index := position - 1
	n := 0
	for offset := range sql {
		if n == index {
			return offset, true
		}
		n++
	}
	if n == index {
		return len(sql), true
	}
	return 0, false
}

func (c *PostgresConnection) Introspect(ctx context.Context) (introspect.Catalog, error) {
	conn, err := acquire(ctx, c.pool)
	if err != nil {
		return introspect.Catalog{}, fmt.Errorf("no connection to read the catalog: %w", err)
	}
	defer conn.Release()
	return readCatalog(ctx, conn.Conn())
}

func (c *PostgresConnection) ServerVersion(ctx context.Context) (string, error) {
	conn, err := acquire(ctx, c.pool)
	if err != nil {
		return "", fmt.Errorf("failed to read the server version: %w", err)
	}
	defer conn.Release()
	var version string
	if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		return "", fmt.Errorf("failed to read the server version: %w", err)
	}
	return fmt.Sprintf("PG %s", shortVersion(version)), nil
}

// Execute runs the app's own statements — table pages — on a pooled
// connection, which is a different backend every time. So the id is asked
// for per run here, where a session asks once: the cap has to be able to
// cancel the statement it stopped reading, even when nothing is watching
// the run.
func (c *PostgresConnection) Execute(ctx context.Context, sql string) (dbclient.QueryResult, error) {
	conn, err := acquire(ctx, c.pool)
	if err != nil {
		return dbclient.QueryResult{}, fmt.Errorf("no connection to run the statement: %w", err)
	}
	backend, link, err := prepareRun(ctx, conn.Conn(), c.statementTimeout)
	if err != nil {
		conn.Release()
		return dbclient.QueryResult{}, err
	}
	result, err := collectCapped(ctx, conn.Conn(), c.pool, backend, sql, c.limits, c.types)
	// The connection goes back to the pool here.
	conn.Release()
	if err == nil {
		result.Wire.LinkMs = &link
	}
	return result, err
}

// OpenSession takes one pinned connection out of the session pool, reads
// its backend once, and asks for its timeout once. Everything a tab runs
// after this goes down that connection, which is what makes a `SET`, a
// `BEGIN` and a temp table mean anything from one run to the next.
func (c *PostgresConnection) OpenSession(ctx context.Context) (dbclient.Session, error) {
	conn, err := acquire(ctx, c.sessions)
	if err != nil {
		return nil, fmt.Errorf("no connection left for another session — close a tab: %w", err)
	}
	backend, link, err := prepareRun(ctx, conn.Conn(), c.statementTimeout)
	if err != nil {
		conn.Release()
		return nil, err
	}
	held := &heldConn{conn: conn}
	session := &PostgresSession{
		app:     c.pool,
		held:    held,
		backend: backend,
		linkMs:  link,
		counted: make(map[int64]counted),
		limits:  c.limits,
		types:   c.types,
	}
	// The Rust `impl Drop`, as near as Go has one: see `PostgresSession`.
	runtime.AddCleanup(session, func(held *heldConn) { held.detach() }, held)
	return session, nil
}

// Check is the pooled check, for a tab that has never run anything.
//
// It goes out on the **app** pool, like every other question the app
// asks for itself, and the cost of that is exactly what
// [dbclient.RefusalName] warns about: this connection is nobody's session,
// so a temp table and a `search_path` are invisible to it. A tab with
// a session should ask [dbclient.Session.Check] instead.
func (c *PostgresConnection) Check(ctx context.Context, sql string) (*dbclient.CheckError, error) {
	conn, err := acquire(ctx, c.pool)
	if err != nil {
		return nil, fmt.Errorf("no connection to check the statement: %w", err)
	}
	defer conn.Release()
	return checkStatement(ctx, conn.Conn(), sql), nil
}

func (c *PostgresConnection) RelationExists(ctx context.Context, name string) (bool, bool, error) {
	conn, err := acquire(ctx, c.pool)
	if err != nil {
		return false, false, fmt.Errorf("no connection to ask about a name: %w", err)
	}
	defer conn.Release()
	return relationOn(ctx, conn.Conn(), name)
}

// Stop is always on the **app** pool, never a session's: the connection
// being stopped is busy, and waiting for it would be waiting for the thing
// the user just asked to stop. That reserve is why the two pools are
// separate.
func (c *PostgresConnection) Stop(ctx context.Context, run dbclient.RunId, how dbclient.Stop) (bool, error) {
	return cancel(ctx, c.pool, run, how)
}

func (c *PostgresConnection) Apply(ctx context.Context, changes []dbclient.RowChange) (uint64, error) {
	return 0, errors.New("in-place editing is not implemented yet (Phase 2)")
}

// PostgresSession is one tab's pinned connection.
//
// `execute` takes `&self` because the tab shares the session behind an
// `Arc`, and sqlx wants `&mut PgConnection`, so the connection sits behind
// an async mutex. It is never contended in practice: a tab runs one
// statement at a time. The `Option` is what `close` empties.
//
// A session may reach its last handle on **any** thread, and in this app
// that thread is usually the UI one: leaving a workspace drops the shell,
// every tab, and every session, inside a mouse-up. The Rust build had an
// `impl Drop` that detached the connection from sqlx's pool there,
// because sqlx gave a dropped connection back by spawning onto tokio, and
// spawning off the runtime aborted the process.
//
// Go port: Go has no destructor and pgx needs no runtime to give a
// connection back, so nothing can abort. What is left of the `Drop` is
// its other half — a session nobody closed must not hand its connection,
// with whatever transaction it holds, back to the pool. A cleanup
// registered with the collector does that: when the session is
// unreachable and still holds a connection, it takes the connection out
// of the pool for good and closes it, and closing is itself a rollback.
// It is the *fallback*, not the ordinary path — `Close` gives the
// connection back properly and leaves nothing for the cleanup to do.
type PostgresSession struct {
	// The *app's* pool, not the session's. A cancel must go out on a
	// connection that is not the one being cancelled.
	app  *pgxpool.Pool
	held *heldConn
	// backend is the session's backend id.
	backend dbclient.RunId
	// One round trip to this server, measured at open on the trip that
	// reads the backend id. A session is one connection to one host, so
	// the number holds for every run on it; it is remeasured only by
	// opening another session.
	linkMs uint64
	// The running totals `pg_stat_statements` reported for each statement
	// this session has run, as of the last time it was asked.
	//
	// **This is what makes one execution's own time knowable.** The view
	// counts rather than logs — one row of totals per statement, shared by
	// every backend in the cluster — so a single reading can only give a
	// mean. Two readings with one execution between them give that
	// execution, and `calls` is what says how many landed in the window.
	countedMu sync.Mutex
	counted   map[int64]counted
	// Set once the server has answered that it cannot report these
	// numbers: no `pg_stat_statements`, or a server too old to carry
	// `pg_stat_activity.query_id`. Asking again every run would be one
	// wasted round trip per run, for ever, on every such server.
	noStatementStats atomic.Bool
	limits           dbclient.Limits
	types            *typeCache
}

var _ dbclient.Session = (*PostgresSession)(nil)

// heldConn is the Rust `Mutex<Option<PoolConnection>>`: the session's
// connection, nil once `close` has emptied it. It is apart from the
// session so the collector's cleanup can reach it once the session itself
// is gone.
type heldConn struct {
	mu   sync.Mutex
	conn *pgxpool.Conn
}

// lock takes the session's connection, or says the session is closed.
// The caller unlocks `mu` when it is done with the connection.
func (h *heldConn) lock() (*pgx.Conn, error) {
	h.mu.Lock()
	if h.conn == nil {
		h.mu.Unlock()
		return nil, errors.New("this tab's session is closed")
	}
	return h.conn.Conn(), nil
}

// detach takes a connection nobody closed out of the pool's hands and
// closes it. Closing is itself a rollback, so no state is left stuck.
func (h *heldConn) detach() {
	h.mu.Lock()
	conn := h.conn
	h.conn = nil
	h.mu.Unlock()
	if conn == nil {
		return
	}
	raw := conn.Hijack()
	go func() { _ = raw.Close(context.Background()) }()
}

// boundary sends a transaction boundary down this session's own
// connection.
//
// It has to be this connection and no other: a transaction belongs to
// the connection that opened it, so a `COMMIT` on a pooled one would
// commit nothing and say it worked.
func (s *PostgresSession) boundary(ctx context.Context, sql string) error {
	conn, err := s.held.lock()
	if err != nil {
		return err
	}
	defer s.held.mu.Unlock()
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("the server refused %s: %w", sql, wrapDB(err))
	}
	return nil
}

// counted is one reading of a statement's running totals.
type counted struct {
	calls  int64
	execMs float64
	planMs float64
}

func (s *PostgresSession) Backend() (dbclient.RunId, bool) {
	return s.backend, true
}

func (s *PostgresSession) Execute(ctx context.Context, sql string) (dbclient.QueryResult, error) {
	conn, err := s.held.lock()
	if err != nil {
		return dbclient.QueryResult{}, err
	}
	defer s.held.mu.Unlock()
	result, err := collectCapped(ctx, conn, s.app, s.backend, sql, s.limits, s.types)
	if err == nil {
		link := s.linkMs
		result.Wire.LinkMs = &link
	}
	return result, err
}

// Check is the same question, down the tab's own connection — so a temp
// table it made and a `search_path` it set are part of the answer.
//
// It takes the session's lock, which is why the caller must not send
// one while a run is out.
func (s *PostgresSession) Check(ctx context.Context, sql string) (*dbclient.CheckError, error) {
	conn, err := s.held.lock()
	if err != nil {
		return nil, err
	}
	defer s.held.mu.Unlock()
	return checkStatement(ctx, conn, sql), nil
}

func (s *PostgresSession) RelationExists(ctx context.Context, name string) (bool, bool, error) {
	conn, err := s.held.lock()
	if err != nil {
		return false, false, err
	}
	defer s.held.mu.Unlock()
	return relationOn(ctx, conn, name)
}

// ServerTiming reads what the server counted for the statement this
// session ran last, and turns two readings into one execution's time.
//
// One round trip, and it goes out on the **app** pool for the reason
// `in_transaction` does: this session's connection may still be
// finishing with the very statement being asked about, and a
// transaction that has gone wrong refuses every statement until it is
// rolled back. It is also asked *after* the result is on screen, so
// the round trip is never in front of the user.
//
// `pg_stat_activity.query_id` is what names the statement. It needs
// `compute_query_id`, which `pg_stat_statements` turns on by itself,
// and it is retained on an idle backend — the column is that backend's
// *most recent* query, not only a running one. So the two views join
// on it and the driver never has to match SQL text, which would not
// match anyway: the view normalizes constants out.
func (s *PostgresSession) ServerTiming(ctx context.Context) (*dbclient.ServerTiming, error) {
	if s.noStatementStats.Load() {
		return nil, nil
	}
	var (
		queryid, calls  int64
		execMs, planMs  float64
		found, answered = false, true
	)
	conn, err := acquire(ctx, s.app)
	if err == nil {
		err = conn.QueryRow(ctx, statementStats, int32(s.backend)).Scan(&queryid, &calls, &execMs, &planMs)
		conn.Release()
	}
	switch {
	case err == nil:
		found = true
	case errors.Is(err, pgx.ErrNoRows):
		found = false
	default:
		answered = false
	}
	// The server cannot answer this question and never will on this
	// connection: the extension is not installed, or the column is
	// not there to join on. That is not a failure worth reporting —
	// the timing the client measured itself still stands.
	if !answered {
		s.noStatementStats.Store(true)
		return nil, nil
	}
	if !found {
		return nil, nil
	}
	now := counted{
		calls:  calls,
		execMs: execMs,
		planMs: planMs,
	}
	s.countedMu.Lock()
	before, had := s.counted[queryid]
	s.counted[queryid] = now
	s.countedMu.Unlock()
	var previous *counted
	if had {
		previous = &before
	}
	return between(previous, now), nil
}

// Introspect reads the catalog down this tab's own connection, which is
// the one place a table it created inside an open transaction exists yet.
func (s *PostgresSession) Introspect(ctx context.Context) (*introspect.Catalog, error) {
	conn, err := s.held.lock()
	if err != nil {
		return nil, err
	}
	defer s.held.mu.Unlock()
	catalog, err := readCatalog(ctx, conn)
	if err != nil {
		return nil, err
	}
	return &catalog, nil
}

// InTransaction is asked of `pg_stat_activity` from the **app** pool,
// never of the session itself. Two reasons: the session's connection may
// be busy with a statement, and asking it would have to queue behind
// exactly the run whose state is in question; and a transaction that has
// gone wrong refuses every statement until it is rolled back, so a session
// in the state most worth reporting is the one that could not answer.
//
// `idle in transaction` and `idle in transaction (aborted)` both
// count — a rollback is what leaves either one.
func (s *PostgresSession) InTransaction(ctx context.Context) (bool, error) {
	conn, err := acquire(ctx, s.app)
	if err != nil {
		return false, fmt.Errorf("failed to read what the session is doing: %w", err)
	}
	defer conn.Release()
	var state *string
	err = conn.QueryRow(ctx, "SELECT state FROM pg_stat_activity WHERE pid = $1", int32(s.backend)).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to read what the session is doing: %w", err)
	}
	return state != nil && strings.HasPrefix(*state, "idle in transaction"), nil
}

// Begin is one statement, one round trip, and nothing else: no cap, no
// timing, no `describe` to ask what columns `BEGIN` returns. See
// [dbclient.Session.Begin] for why a boundary is not a run.
func (s *PostgresSession) Begin(ctx context.Context) error {
	return s.boundary(ctx, "BEGIN")
}

func (s *PostgresSession) EndTransaction(ctx context.Context, how dbclient.TxEnd) error {
	return s.boundary(ctx, how.SQL())
}

func (s *PostgresSession) Close(ctx context.Context) {
	s.held.mu.Lock()
	conn := s.held.conn
	s.held.conn = nil
	s.held.mu.Unlock()
	if conn == nil {
		return
	}
	// Sent whether or not a transaction is open, because it costs one
	// round trip to send and one to ask. Outside a transaction Postgres
	// answers "there is no transaction in progress" and carries on, so
	// the session never has to know which it is in.
	if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
		// Not clean, or not answering. Take it out of the pool for
		// good rather than hand the next tab a connection whose state
		// nobody knows; closing the detached connection closes it,
		// and closing is itself a rollback.
		raw := conn.Hijack()
		_ = raw.Close(ctx)
		return
	}
	// Clean, so the pool may have it back.
	conn.Release()
}

// prepareRun asks the server for the backend id, and for the timeout in
// the same round trip. `set_config(.., false)` is `SET`, and the `WHERE` is
// what makes it conditional — see `statement_timeout` on the connection
// for why it is asked for that way rather than as a startup option.
//
// A session pays this once, at open. The pooled path pays it once per
// run, because a pooled connection is a different backend each time.
//
// It also **times the link, for free**. This statement reads two catalog
// values and sets a string: the server's share of it rounds to nothing, so
// what the clock around it measures is one round trip to this host and
// back. A viewer over a VPN needs that number to make sense of any other —
// and it must not cost a round trip of its own to get, or measuring the
// lag would add to it.
func prepareRun(ctx context.Context, conn *pgx.Conn, timeout string) (dbclient.RunId, uint64, error) {
	started := time.Now()
	var (
		pid     int32
		applied *string
	)
	err := conn.QueryRow(ctx,
		"SELECT pg_backend_pid(), "+
			"(SELECT set_config('statement_timeout', $1, false) "+
			"WHERE (SELECT source FROM pg_settings "+
			"WHERE name = 'statement_timeout') = 'default')",
		timeout,
	).Scan(&pid, &applied)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to read the backend id: %w", wrapDB(err))
	}
	return dbclient.RunId(pid), uint64(time.Since(started).Milliseconds()), nil
}

// statementStats is the running totals for the statement a backend ran
// last.
//
// `userid` and `dbid` are part of the join because `queryid` is not a key
// on its own: the same normalized statement run by two roles, or against
// two databases, is two rows, and summing them would report a stranger's
// work as this session's.
const statementStats = "" +
	"SELECT s.queryid, s.calls, s.total_exec_time, s.total_plan_time " +
	"  FROM pg_stat_activity a " +
	"  JOIN pg_stat_statements s " +
	"    ON s.queryid = a.query_id " +
	"   AND s.userid = a.usesysid " +
	"   AND s.dbid = (SELECT oid FROM pg_database WHERE datname = a.datname) " +
	" WHERE a.pid = $1"

// between turns two readings of a statement's running totals into what
// one execution cost. A plain function over numbers, so the three cases
// can be argued with in a test rather than against a server.
//
//   - **One execution between the readings** is the answer asked for, and
//     it is exact.
//   - **Several** means somebody else ran the same statement in the same
//     window — the view is cluster-wide — so the honest answer is the mean
//     over them, marked inexact.
//   - **No earlier reading** leaves nothing to subtract, so the answer is
//     the mean over every execution the server has ever counted. That is
//     the first run of a statement in a tab: worth showing, never worth
//     calling this run's time. The reading just taken is what makes the
//     *next* run exact.
//
// A `calls` that went *down* is the view being reset or the entry evicted
// under `pg_stat_statements.max`. There is no delta to take across that,
// so it answers nothing and lets the reading just taken be the baseline.
func between(before *counted, now counted) *dbclient.ServerTiming {
	var (
		exec, plan, calls float64
		exact             bool
	)
	switch {
	case before != nil && now.calls > before.calls:
		exec = now.execMs - before.execMs
		plan = now.planMs - before.planMs
		calls = float64(now.calls - before.calls)
		exact = now.calls-before.calls == 1
	case before != nil && now.calls == before.calls:
		return nil
	// Either the first reading, or one taken across a reset.
	case now.calls > 0:
		exec, plan, calls, exact = now.execMs, now.planMs, float64(now.calls), false
	default:
		return nil
	}
	var planMs *float64
	// A zero here says "planning was not tracked" as readily as it says
	// "planning was free", and the two are not the same claim.
	if plan > 0. {
		perCall := plan / calls
		planMs = &perCall
	}
	return &dbclient.ServerTiming{
		ExecMs: exec / calls,
		PlanMs: planMs,
		Exact:  exact,
	}
}

// collectCapped runs one statement and collects it, up to `limits`.
//
// The rows are **streamed** and decoded one at a time, so the process
// holds one row plus whatever the sink has kept — never the whole result.
// `fetch_all` held both the raw rows and the decoded ones at once, which
// is how `SELECT *` over a large table took the app down.
//
// It reads the completion tag as well as the rows, because the tag is the
// only place the number of rows an `UPDATE` changed is ever stated. A
// statement that changes rows sends no row at all, so a reader of rows
// alone answered one with nothing whatever: no rows, no columns and no
// count. The tag is where `rows_affected` comes from.
//
// `app` is the pool used to cancel and to describe: both must reach the
// server while `conn` is busy with the statement, so neither may be `conn`.
//
// Go port: sqlx prepared the statement, resolved its column types on the
// same connection, and executed it asking for every column in binary.
// This does the same through pgconn — the unnamed statement, `typeCache`
// for the types, and one binary format code for every result column — so
// `decode` reads the bytes sqlx's reader read.
func collectCapped(
	ctx context.Context,
	conn *pgx.Conn,
	app *pgxpool.Pool,
	backend dbclient.RunId,
	sql string,
	limits dbclient.Limits,
	types *typeCache,
) (dbclient.QueryResult, error) {
	sink := dbclient.NewRowSink(limits)
	// Stamped before the statement goes out and again on the first row off
	// the wire. Postgres emits no row until it has one, so the gap bounds
	// the server's work — see `db_client::Wire` for what that does and does
	// not separate.
	sent := time.Now()
	var firstRow *time.Time
	// The server's own count, off the completion tag.
	var affected uint64

	pg := conn.PgConn()
	described, err := pg.Prepare(ctx, "", sql, nil)
	if err != nil {
		return dbclient.QueryResult{}, wrapDB(err)
	}
	columnTypes, fetched, err := types.fieldTypes(ctx, conn, described.Fields)
	if err != nil {
		return dbclient.QueryResult{}, wrapDB(err)
	}
	if fetched {
		// The type lookups went down this connection and replaced the
		// unnamed statement, so it is prepared again.
		if _, err := pg.Prepare(ctx, "", sql, nil); err != nil {
			return dbclient.QueryResult{}, wrapDB(err)
		}
	}
	reader := pg.ExecPrepared(ctx, "", nil, nil, []int16{formatBinary})
	capped := false
	for reader.NextRow() {
		// Taken before the row is decoded, or this process's own
		// decoding would be counted as the server's time.
		if firstRow == nil {
			at := time.Now()
			firstRow = &at
		}
		fields := reader.FieldDescriptions()
		if !sink.HasColumns() {
			names := make([]string, len(fields))
			for i, field := range fields {
				names[i] = field.Name
			}
			sink.Columns(names)
		}
		raw := reader.Values()
		values := make([]dbclient.Value, 0, len(raw))
		for i := range raw {
			info := columnTypes[i]
			value, err := decode(raw[i], fields[i].Format, info, i)
			if err != nil {
				_, _ = reader.Close()
				return dbclient.QueryResult{}, err
			}
			values = append(values, value)
		}
		if !sink.Push(values) {
			// The budget is spent. Dropping the stream here would bound
			// *this* process and nothing else: the driver must read every
			// remaining row off the wire before the connection can be
			// used again, so the server would go on building and
			// sending the whole result. Ask it to stop instead — the
			// same `pg_cancel_backend` the stop button sends.
			stopped, _ := cancel(ctx, app, backend, dbclient.StopCancel)
			if err := drain(reader, stopped); err != nil {
				return dbclient.QueryResult{}, err
			}
			capped = true
			break
		}
	}
	if !capped {
		// The statement finished. For an `UPDATE` this is the whole of the
		// answer; for a `SELECT` it repeats the row count, which is why
		// the caller reads it only where there are no columns.
		tag, err := reader.Close()
		if err != nil {
			return dbclient.QueryResult{}, wrapDB(err)
		}
		affected += uint64(tag.RowsAffected())
	}

	// A statement that changed rows has none to name columns with and no
	// columns to describe — the describe would answer `NoData`. So the
	// count is what says not to ask: a round trip on the app pool for
	// headers that cannot exist is a round trip on every `UPDATE`.
	//
	// A count of zero is the case that still has to ask, because a
	// `SELECT` matching nothing reads exactly the same way here, and an
	// empty result set has headers worth painting.
	if !sink.HasColumns() && affected == 0 {
		// No rows came back, so the row metadata cannot name the columns.
		// Ask the server to describe the statement instead, so an empty
		// result still renders its headers. DDL and other statements
		// without a result set simply describe to nothing.
		if names, err := describe(ctx, app, sql); err == nil {
			sink.Columns(names)
		}
	}
	result := sink.Finish(affected)
	result.Wire = wireOf(sent, firstRow)
	return result, nil
}

// describe is sqlx's `describe` on the app pool, as far as it is read
// here: the names of the columns a statement would return.
func describe(ctx context.Context, app *pgxpool.Pool, sql string) ([]string, error) {
	conn, err := acquire(ctx, app)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	described, err := conn.Conn().PgConn().Prepare(ctx, "", sql, nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(described.Fields))
	for i, field := range described.Fields {
		names[i] = field.Name
	}
	return names, nil
}

// wireOf is the client's own view of where a run's time went. `link_ms`
// is filled in by the caller, which is the only side that knows which
// connection — and so which measured round trip — the statement went
// down.
func wireOf(sent time.Time, firstRow *time.Time) dbclient.Wire {
	wire := dbclient.Wire{}
	if firstRow != nil {
		first := uint64(firstRow.Sub(sent).Milliseconds())
		wire.FirstRowMs = &first
		// A statement that returned no rows has no fetch to measure. `0`
		// would read as "the rows crossed instantly", which is a different
		// claim from "there were none".
		fetch := uint64(time.Since(*firstRow).Milliseconds())
		wire.FetchMs = &fetch
	}
	return wire
}

// cancel: `pg_cancel_backend` asks the backend to give up its statement,
// which comes back to the runner as SQLSTATE 57014. `pg_terminate_backend`
// closes the backend outright, and the driver then throws that connection
// away — which is why it is the second press.
func cancel(ctx context.Context, pool *pgxpool.Pool, run dbclient.RunId, how dbclient.Stop) (bool, error) {
	var sql string
	switch how {
	case dbclient.StopCancel:
		sql = "SELECT pg_cancel_backend($1)"
	case dbclient.StopTerminate:
		sql = "SELECT pg_terminate_backend($1)"
	}
	conn, err := acquire(ctx, pool)
	if err != nil {
		return false, fmt.Errorf("failed to ask the server to stop the statement: %w", err)
	}
	defer conn.Release()
	var accepted *bool
	if err := conn.QueryRow(ctx, sql, int32(run)).Scan(&accepted); err != nil {
		return false, fmt.Errorf("failed to ask the server to stop the statement: %w", wrapDB(err))
	}
	return accepted != nil && *accepted, nil
}

// drain reads out whatever the server still sends after a cancel, throwing
// it away, until the statement ends. Nothing is decoded and nothing is
// kept, so this costs bandwidth, not memory — and the connection comes
// back to the pool usable rather than owing the server a result.
//
// `stopped` says the cancel was accepted, and only then is `57014` the
// answer we asked for rather than news. Any other error is the server's own
// and is reported.
func drain(rows *pgconn.ResultReader, stopped bool) error {
	for rows.NextRow() {
	}
	_, err := rows.Close()
	switch {
	case err == nil:
		return nil
	case stopped && isCancelled(err):
		return nil
	default:
		return wrapDB(err)
	}
}

// isCancelled says whether an error is the server saying it gave up the
// statement.
func isCancelled(err error) bool {
	var refused *pgconn.PgError
	return errors.As(err, &refused) && refused.Code == queryCanceled
}

// --- Go port only: what sqlx gave the Rust build for free. ---

// dbError is sqlx's `Error::Database` as the app reads it: "error returned
// from database: " and the server's own message. The server's error stays
// inside it for whoever wants the SQLSTATE.
type dbError struct {
	refused *pgconn.PgError
}

func (e *dbError) Error() string {
	return "error returned from database: " + e.refused.Message
}

func (e *dbError) Unwrap() error {
	return e.refused
}

// wrapDB words a server refusal the way sqlx did, and leaves every other
// error as it was.
func wrapDB(err error) error {
	var refused *pgconn.PgError
	if errors.As(err, &refused) {
		return &dbError{refused: refused}
	}
	return err
}

// acquire is sqlx's `PgPool::acquire` with `acquire_timeout` set to
// `CONNECT_TIMEOUT`: pgx's pool waits on its context for ever, so the
// deadline is put on the context, and running out is reported as
// [errPoolTimedOut].
func acquire(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	timed, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := pool.Acquire(timed)
	if err != nil {
		if ctx.Err() == nil && errors.Is(timed.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %w", errPoolTimedOut, err)
		}
		return nil, err
	}
	return conn, nil
}

// connectOptions is sqlx's `PgConnectOptions`, as far as this driver
// reads it: the defaults it took from the environment, what a URL
// overrides, and the startup parameters it sent.
type connectOptions struct {
	host            string
	port            uint16
	socket          *string
	username        string
	password        *string
	database        *string
	sslMode         *string
	sslRootCert     *string
	sslCert         *string
	sslKey          *string
	applicationName *string
	options         *string
}

// newConnectOptions is `PgConnectOptions::new()`: the libpq environment
// variables, and the defaults behind them. The `.pgpass` lookup sqlx made
// here is left to pgx, which makes it when the connect has no password.
func newConnectOptions() connectOptions {
	var port uint16 = 5432
	if value, ok := os.LookupEnv("PGPORT"); ok {
		if parsed, err := strconv.ParseUint(value, 10, 16); err == nil {
			port = uint16(parsed)
		}
	}
	host, ok := os.LookupEnv("PGHOSTADDR")
	if !ok {
		host, ok = os.LookupEnv("PGHOST")
	}
	if !ok {
		host = defaultHost(port)
	}
	username, ok := os.LookupEnv("PGUSER")
	if !ok {
		username = whoami()
	}
	options := connectOptions{host: host, port: port, username: username}
	options.database = lookupEnv("PGDATABASE")
	options.password = lookupEnv("PGPASSWORD")
	options.sslRootCert = lookupEnv("PGSSLROOTCERT")
	options.sslCert = lookupEnv("PGSSLCERT")
	options.sslKey = lookupEnv("PGSSLKEY")
	options.sslMode = lookupEnv("PGSSLMODE")
	options.applicationName = lookupEnv("PGAPPNAME")
	options.options = lookupEnv("PGOPTIONS")
	return options
}

func lookupEnv(name string) *string {
	if value, ok := os.LookupEnv(name); ok {
		return &value
	}
	return nil
}

// whoami is `whoami::username()`.
func whoami() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	return os.Getenv("USER")
}

// defaultHost tries to find a Unix socket to use, and falls back to
// localhost — sqlx's `default_host`.
func defaultHost(port uint16) string {
	socket := fmt.Sprintf(".s.PGSQL.%d", port)
	for _, candidate := range []string{
		"/var/run/postgresql", // Debian
		"/private/tmp",        // OSX (homebrew)
		"/tmp",                // Default
	} {
		if _, err := os.Stat(filepath.Join(candidate, socket)); err == nil {
			return candidate
		}
	}
	return "localhost"
}

// parseConnectURL is `PgConnectOptions::from_str`: the URL over the
// defaults, every parameter sqlx read, and the rest ignored as sqlx
// ignored them.
func parseConnectURL(text string) (connectOptions, error) {
	parsed, err := url.Parse(text)
	if err != nil {
		return connectOptions{}, err
	}
	if parsed.Scheme == "" {
		return connectOptions{}, errors.New("relative URL without a base")
	}
	options := newConnectOptions()

	if host := parsed.Hostname(); host != "" {
		if strings.HasPrefix(host, "/") {
			options.socket = &host
		} else {
			options.host = host
		}
	}
	if portText := parsed.Port(); portText != "" {
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil {
			return connectOptions{}, errors.New("invalid port number")
		}
		options.port = uint16(port)
	}
	if username := parsed.User.Username(); username != "" {
		options.username = username
	}
	if password, ok := parsed.User.Password(); ok {
		options.password = &password
	}
	if path := strings.TrimLeft(parsed.Path, "/"); path != "" {
		options.database = &path
	}
	for _, pair := range strings.Split(parsed.RawQuery, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return connectOptions{}, err
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return connectOptions{}, err
		}
		switch {
		case key == "sslmode" || key == "ssl-mode":
			switch value {
			case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
				options.sslMode = &value
			default:
				return connectOptions{}, fmt.Errorf("unknown value %q for `ssl_mode`", value)
			}
		case key == "sslrootcert" || key == "ssl-root-cert" || key == "ssl-ca":
			options.sslRootCert = &value
		case key == "sslcert" || key == "ssl-cert":
			options.sslCert = &value
		case key == "sslkey" || key == "ssl-key":
			options.sslKey = &value
		case key == "statement-cache-capacity":
			if _, err := strconv.ParseUint(value, 10, 64); err != nil {
				return connectOptions{}, err
			}
		case key == "host":
			if strings.HasPrefix(value, "/") {
				options.socket = &value
			} else {
				options.host = value
			}
		case key == "hostaddr":
			if net.ParseIP(value) == nil {
				return connectOptions{}, errors.New("invalid IP address syntax")
			}
			options.host = value
		case key == "port":
			port, err := strconv.ParseUint(value, 10, 16)
			if err != nil {
				return connectOptions{}, err
			}
			options.port = uint16(port)
		case key == "dbname":
			options.database = &value
		case key == "user":
			options.username = value
		case key == "password":
			options.password = &value
		case key == "application_name":
			options.applicationName = &value
		case key == "options":
			if options.options != nil {
				joined := *options.options + " " + value
				options.options = &joined
			} else {
				options.options = &value
			}
		case strings.HasPrefix(key, "options["):
			if inner, ok := strings.CutSuffix(strings.TrimPrefix(key, "options["), "]"); ok {
				options.addOption(inner, value)
			}
		}
	}
	return options, nil
}

// addOption is `PgConnectOptions::options`: one `-c key=value` more in the
// `options` startup parameter.
func (o *connectOptions) addOption(key, value string) {
	text := ""
	if o.options != nil {
		text = *o.options
	}
	if text != "" {
		text += " "
	}
	text += fmt.Sprintf("-c %s=%s", key, value)
	o.options = &text
}

// poolConfig is the pgx pool configuration for these options: sqlx's
// startup parameters (`DateStyle`, `client_encoding`, `TimeZone`,
// `extra_float_digits`, then `application_name` and `options`), its pool
// timings, and no connection opened up front.
func (o *connectOptions) poolConfig(maxConns int32) (*pgxpool.Config, error) {
	settings := [][2]string{}
	if o.socket != nil {
		settings = append(settings, [2]string{"host", *o.socket})
	} else {
		settings = append(settings, [2]string{"host", o.host})
	}
	settings = append(settings,
		[2]string{"port", strconv.Itoa(int(o.port))},
		[2]string{"user", o.username},
	)
	if o.password != nil {
		settings = append(settings, [2]string{"password", *o.password})
	}
	if o.database != nil {
		settings = append(settings, [2]string{"dbname", *o.database})
	}
	sslMode := "prefer"
	if o.sslMode != nil {
		sslMode = *o.sslMode
	}
	settings = append(settings, [2]string{"sslmode", sslMode})
	if o.sslRootCert != nil {
		settings = append(settings, [2]string{"sslrootcert", *o.sslRootCert})
	}
	if o.sslCert != nil {
		settings = append(settings, [2]string{"sslcert", *o.sslCert})
	}
	if o.sslKey != nil {
		settings = append(settings, [2]string{"sslkey", *o.sslKey})
	}
	settings = append(settings,
		// Sets the display format for date and time values,
		// as well as the rules for interpreting ambiguous date input values.
		[2]string{"DateStyle", "ISO, MDY"},
		// Sets the client-side encoding (character set).
		[2]string{"client_encoding", "UTF8"},
		// Sets the time zone for displaying and interpreting time stamps.
		[2]string{"TimeZone", "UTC"},
		[2]string{"extra_float_digits", "2"},
	)
	if o.applicationName != nil {
		settings = append(settings, [2]string{"application_name", *o.applicationName})
	}
	if o.options != nil {
		settings = append(settings, [2]string{"options", *o.options})
	}
	var dsn strings.Builder
	for k, setting := range settings {
		if k > 0 {
			dsn.WriteByte(' ')
		}
		dsn.WriteString(setting[0])
		dsn.WriteByte('=')
		dsn.WriteString(quoteSetting(setting[1]))
	}
	config, err := pgxpool.ParseConfig(dsn.String())
	if err != nil {
		return nil, err
	}
	config.MaxConns = maxConns
	config.MinConns = 0
	// sqlx's pool defaults: a connection idle for ten minutes is closed,
	// and none lives past thirty.
	config.MaxConnIdleTime = 10 * time.Minute
	config.MaxConnLifetime = 30 * time.Minute
	config.ConnConfig.ConnectTimeout = connectTimeout
	// The app's own statements run unnamed, so nothing it asks leaves a
	// prepared statement on a session's connection for a `DISCARD ALL` the
	// user types to pull out from under it.
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	return config, nil
}

// quoteSetting quotes one value of a libpq keyword/value string.
func quoteSetting(value string) string {
	if value != "" && !strings.ContainsAny(value, " '\\\t\n\r") && utf8.ValidString(value) {
		return value
	}
	escaped := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value)
	return "'" + escaped + "'"
}
