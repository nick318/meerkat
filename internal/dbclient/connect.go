// Why a connect failed, in words a person can act on.
//
// A driver's own error says what the library saw — "pool timed out while
// waiting for an open connection", "error communicating with database" —
// and the person looking at it has a different question: is it the
// network, the server, or what I typed? [ConnectFailure] is the answer
// to that question, and [ClassifyConnect] is the one place that decides it.
//
// The decision is a pure function over [ConnectEvidence], which is plain
// data: a SQLSTATE, an `io::ErrorKind`, two flags and the message. A driver
// fills the evidence in from whatever its library hands back, so the
// rules live here once, know nothing about sqlx, and are argued with in
// a test rather than against a server that has to be broken on purpose.
//
// Go port: the Rust crate re-exported `Evidence as ConnectEvidence` and
// `classify as classify_connect`; the Go package is flat, so those are the
// names here. Go has no `std::io::ErrorKind`, so [ErrorKind] is the part of
// it this file reads, and [IoErrorKind] is `io::Error::kind()`.

package dbclient

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
)

// ConnectFailure is the class of a failed connect. It picks the headline
// and the hint; the server's or the system's own words travel beside it, in
// [ConnectError.Detail].
type ConnectFailure int

const (
	// No route to the host or to its network: the packets had nowhere to
	// go. Usually a VPN that is down.
	ConnectFailureUnreachable ConnectFailure = iota
	// The host answered and nothing listens on that port.
	ConnectFailureRefused
	// Nothing answered in time. A firewall that drops packets looks like
	// this, and so does a host that is switched off.
	ConnectFailureTimeout
	// The host name did not resolve to an address.
	ConnectFailureDns
	// The server refused the user: a wrong password, an unknown role, or
	// a `pg_hba.conf` that has no line for this client.
	ConnectFailureAuth
	// The server is there and the database is not.
	ConnectFailureNoDatabase
	// The TLS handshake failed, or one side insisted on TLS and the other
	// would not speak it.
	ConnectFailureTls
	// Something in front of the server — a connection pooler — refused
	// the startup packet. A read-only session asks for
	// `default_transaction_read_only` there, and PgBouncer refuses a
	// startup parameter it was not told to pass on.
	ConnectFailureStartupOption
	// None of the above. The detail is all there is.
	ConnectFailureOther
)

var connectFailureTags = []string{
	"Unreachable", "Refused", "Timeout", "Dns", "Auth", "NoDatabase", "Tls", "StartupOption", "Other",
}

// String is the derived `Debug`: the variant's name.
func (k ConnectFailure) String() string { return tagName(connectFailureTags, int(k)) }

// Headline is one short line that says what went wrong.
func (k ConnectFailure) Headline() string {
	switch k {
	case ConnectFailureUnreachable:
		return "The host is unreachable"
	case ConnectFailureRefused:
		return "The connection was refused"
	case ConnectFailureTimeout:
		return "The connection timed out"
	case ConnectFailureDns:
		return "The host name was not found"
	case ConnectFailureAuth:
		return "The server refused the sign-in"
	case ConnectFailureNoDatabase:
		return "The database does not exist"
	case ConnectFailureTls:
		return "The secure connection failed"
	case ConnectFailureStartupOption:
		return "The pooler refused the session's options"
	case ConnectFailureOther:
		return "Could not connect"
	}
	panic(fmt.Sprintf("dbclient: unknown ConnectFailure %d", int(k)))
}

// Hint is what to look at first. A hint, not a diagnosis: it names the
// usual cause of this class and nothing more.
func (k ConnectFailure) Hint() string {
	switch k {
	case ConnectFailureUnreachable:
		return "Check the VPN or the network, then the host."
	case ConnectFailureRefused:
		return "Check the host and the port, and that the server is running and accepts TCP connections."
	case ConnectFailureTimeout:
		return "Check the VPN or a firewall, then the host and the port. A server that is starting up or out of connections also answers late."
	case ConnectFailureDns:
		return "Check the spelling of the host, and the VPN if the name is internal."
	case ConnectFailureAuth:
		return "Check the user name and the password. The server may also refuse this client's address or ask for TLS."
	case ConnectFailureNoDatabase:
		return "Check the database name. It is case-sensitive."
	case ConnectFailureTls:
		return "Check whether the server offers TLS and whether its certificate is valid for this host."
	case ConnectFailureStartupOption:
		return "A read-only session sends a startup option. Connect to the server directly, let the pooler pass `options` on, or turn read-only off for this connection."
	case ConnectFailureOther:
		return "Check the connection's settings."
	}
	panic(fmt.Sprintf("dbclient: unknown ConnectFailure %d", int(k)))
}

// ErrorKind is the part of Rust's `std::io::ErrorKind` that the
// classification reads, and the few a driver may hand in beside them.
type ErrorKind int

const (
	ErrorKindOther ErrorKind = iota
	ErrorKindConnectionRefused
	ErrorKindConnectionReset
	ErrorKindConnectionAborted
	ErrorKindTimedOut
	ErrorKindHostUnreachable
	ErrorKindNetworkUnreachable
	ErrorKindInvalidData
	ErrorKindUnexpectedEof
)

var errorKindTags = []string{
	"Other", "ConnectionRefused", "ConnectionReset", "ConnectionAborted", "TimedOut",
	"HostUnreachable", "NetworkUnreachable", "InvalidData", "UnexpectedEof",
}

// String is the derived `Debug`: the variant's name.
func (k ErrorKind) String() string { return tagName(errorKindTags, int(k)) }

// IoErrorKind is `io::Error::kind()` for a Go error: the kind of the socket
// failure inside it, read off the errno the system reported or the
// deadline that ran out. `false` when the error is not a socket's at all,
// which is `io: None` in the evidence.
func IoErrorKind(err error) (ErrorKind, bool) {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED:
			return ErrorKindConnectionRefused, true
		case syscall.ECONNRESET:
			return ErrorKindConnectionReset, true
		case syscall.ECONNABORTED:
			return ErrorKindConnectionAborted, true
		case syscall.ETIMEDOUT:
			return ErrorKindTimedOut, true
		case syscall.EHOSTUNREACH:
			return ErrorKindHostUnreachable, true
		case syscall.ENETUNREACH:
			return ErrorKindNetworkUnreachable, true
		}
		return ErrorKindOther, true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return ErrorKindTimedOut, true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrorKindTimedOut, true
		}
		return ErrorKindOther, true
	}
	return ErrorKindOther, false
}

// ConnectEvidence is what a driver knows about a failed connect, as plain
// data.
type ConnectEvidence struct {
	// The server's SQLSTATE, when the server is what refused.
	Sqlstate *string
	// The kind of the I/O error, when the socket is what failed.
	Io *ErrorKind
	// The driver's own deadline ran out.
	TimedOut bool
	// The failure came out of the TLS layer.
	Tls bool
	// The error's text. Read only where nothing better says what
	// happened: a failed name lookup has no error kind of its own.
	Message string
}

// ClassifyConnect decides the class of a failed connect.
//
// The order is from the most certain evidence to the least: what the
// server said outright, then what the socket reported, then the driver's
// own deadline, and the wording of the message last.
func ClassifyConnect(evidence ConnectEvidence) ConnectFailure {
	message := asciiLowercase(evidence.Message)
	if evidence.Sqlstate != nil {
		state := *evidence.Sqlstate
		// PgBouncer refuses a startup parameter with `08P01`, and so does
		// the server for a genuine protocol violation — so the wording
		// decides which this is.
		if strings.Contains(message, "unsupported startup parameter") {
			return ConnectFailureStartupOption
		}
		switch state {
		// invalid_password, invalid_authorization_specification.
		case "28P01", "28000":
			return ConnectFailureAuth
		// invalid_catalog_name.
		case "3D000":
			return ConnectFailureNoDatabase
		default:
			return ConnectFailureOther
		}
	}
	if evidence.Tls {
		return ConnectFailureTls
	}
	// A name that will not resolve arrives as an I/O error of no
	// particular kind; only its text says what it was.
	if isLookupFailure(message) {
		return ConnectFailureDns
	}
	if evidence.Io != nil {
		switch *evidence.Io {
		case ErrorKindConnectionRefused:
			return ConnectFailureRefused
		case ErrorKindTimedOut:
			return ConnectFailureTimeout
		case ErrorKindHostUnreachable, ErrorKindNetworkUnreachable:
			return ConnectFailureUnreachable
		}
	}
	if evidence.TimedOut {
		return ConnectFailureTimeout
	}
	return ConnectFailureOther
}

// asciiLowercase is `str::to_ascii_lowercase`: ASCII letters only, every
// other byte as it was.
func asciiLowercase(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// isLookupFailure reads the resolver's wording, across the platforms it is
// worded on: the standard library's own prefix, `getaddrinfo`'s messages on
// macOS and Linux, and Windows's.
func isLookupFailure(lowered string) bool {
	for _, needle := range []string{
		"failed to lookup address",
		"nodename nor servname",
		"name or service not known",
		"temporary failure in name resolution",
		"no such host is known",
		"no address associated with hostname",
	} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// ConnectError is a connect that failed: its class, and the words that
// came with it.
//
// It is an error type of its own so it survives wrapping: the driver
// returns it, and the app reads it back with [ConnectErrorOf] (Rust's
// `downcast_ref`, Go's `errors.As`) to paint the headline, the detail and
// the hint apart.
type ConnectError struct {
	Kind ConnectFailure
	// The server's or the system's own words, unabridged.
	Detail string
}

func NewConnectError(kind ConnectFailure, detail string) ConnectError {
	return ConnectError{
		Kind:   kind,
		Detail: detail,
	}
}

// ConnectErrorFromEvidence classifies, and keeps the message as the detail.
func ConnectErrorFromEvidence(evidence ConnectEvidence) ConnectError {
	return NewConnectError(ClassifyConnect(evidence), evidence.Message)
}

// ConnectErrorOf reads a connect failure back out of an error. An error
// that is not one — a keychain that would not open, a URL that would not
// parse — is `Other`, with the whole chain as its detail.
//
// A driver returns a `ConnectError` by value; one returned by pointer is
// read back as well.
func ConnectErrorOf(err error) ConnectError {
	var known ConnectError
	if errors.As(err, &known) {
		return known
	}
	var pointer *ConnectError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer
	}
	return NewConnectError(ConnectFailureOther, err.Error())
}

// Error is one line: the headline, then the detail. It is what a caller
// that knows nothing of the class paints, so it has to stand on its own.
func (e ConnectError) Error() string {
	return fmt.Sprintf("%s · %s", e.Kind.Headline(), e.Detail)
}
