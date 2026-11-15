package dbclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func server(sqlstate string, message string) ConnectEvidence {
	return ConnectEvidence{
		Sqlstate: &sqlstate,
		Message:  message,
	}
}

func io(kind ErrorKind, message string) ConnectEvidence {
	return ConnectEvidence{
		Io:      &kind,
		Message: message,
	}
}

func expectClass(t *testing.T, evidence ConnectEvidence, want ConnectFailure) {
	t.Helper()
	if got := ClassifyConnect(evidence); got != want {
		t.Fatalf("classify(%q) = %v, want %v", evidence.Message, got, want)
	}
}

func TestAWrongPasswordAndAnUnknownRoleAreARefusedSignIn(t *testing.T) {
	wrong := server("28P01", "password authentication failed for user \"app\"")
	expectClass(t, wrong, ConnectFailureAuth)
	hba := server("28000", "no pg_hba.conf entry for host \"10.0.0.4\"")
	expectClass(t, hba, ConnectFailureAuth)
}

func TestAMissingDatabaseIsNamed(t *testing.T) {
	missing := server("3D000", "database \"shop\" does not exist")
	expectClass(t, missing, ConnectFailureNoDatabase)
}

func TestAPoolerThatRefusesTheStartupOptionIsNotAProtocolError(t *testing.T) {
	pooler := server("08P01", "unsupported startup parameter: options")
	expectClass(t, pooler, ConnectFailureStartupOption)
	// The same state with other words is whatever the server says.
	protocol := server("08P01", "invalid startup packet layout")
	expectClass(t, protocol, ConnectFailureOther)
}

func TestTheSocketSaysRefusedUnreachableAndTimedOut(t *testing.T) {
	expectClass(t, io(ErrorKindConnectionRefused, "Connection refused"), ConnectFailureRefused)
	expectClass(t, io(ErrorKindHostUnreachable, "No route to host"), ConnectFailureUnreachable)
	expectClass(t, io(ErrorKindNetworkUnreachable, "Network is unreachable"), ConnectFailureUnreachable)
	expectClass(t, io(ErrorKindTimedOut, "Operation timed out"), ConnectFailureTimeout)
}

func TestANameThatDoesNotResolveIsReadOffItsText(t *testing.T) {
	mac := io(
		ErrorKindOther,
		"failed to lookup address information: nodename nor servname provided, or not known",
	)
	expectClass(t, mac, ConnectFailureDns)
	linux := io(ErrorKindOther, "Name or service not known")
	expectClass(t, linux, ConnectFailureDns)
	// With no kind at all, the text is still enough.
	bare := ConnectEvidence{
		Message: "Temporary failure in name resolution",
	}
	expectClass(t, bare, ConnectFailureDns)
}

func TestTheDriversOwnDeadlineIsATimeout(t *testing.T) {
	late := ConnectEvidence{
		TimedOut: true,
		Message:  "pool timed out while waiting for an open connection",
	}
	expectClass(t, late, ConnectFailureTimeout)
}

func TestATlsFailureIsTlsWhateverTheSocketSaid(t *testing.T) {
	invalidData := ErrorKindInvalidData
	tls := ConnectEvidence{
		Tls:     true,
		Io:      &invalidData,
		Message: "invalid peer certificate: UnknownIssuer",
	}
	expectClass(t, tls, ConnectFailureTls)
}

func TestWhatTheServerSaidOutranksEverythingElse(t *testing.T) {
	state := "28P01"
	both := ConnectEvidence{
		Sqlstate: &state,
		TimedOut: true,
		Tls:      true,
		Message:  "password authentication failed",
	}
	expectClass(t, both, ConnectFailureAuth)
}

func TestNothingKnownIsOtherAndKeepsTheWords(t *testing.T) {
	odd := ConnectEvidence{
		Message: "encountered unexpected or invalid data",
	}
	err := ConnectErrorFromEvidence(odd)
	if err.Kind != ConnectFailureOther {
		t.Fatalf("kind = %v", err.Kind)
	}
	if err.Detail != "encountered unexpected or invalid data" {
		t.Fatalf("detail = %q", err.Detail)
	}
}

func TestAnErrorSurvivesAnyhowAndAStrangerBecomesOther(t *testing.T) {
	known := fmt.Errorf("while opening the session: %w", NewConnectError(ConnectFailureRefused, "refused"))
	if got := ConnectErrorOf(known).Kind; got != ConnectFailureRefused {
		t.Fatalf("kind = %v", got)
	}

	stranger := fmt.Errorf("reading the password: %w", errors.New("no keychain"))
	read := ConnectErrorOf(stranger)
	if read.Kind != ConnectFailureOther {
		t.Fatalf("kind = %v", read.Kind)
	}
	if read.Detail != "reading the password: no keychain" {
		t.Fatalf("detail = %q", read.Detail)
	}
}

func TestTheOneLineFormLeadsWithTheHeadline(t *testing.T) {
	err := NewConnectError(ConnectFailureAuth, "password authentication failed")
	if got := err.Error(); got != "The server refused the sign-in · password authentication failed" {
		t.Fatalf("one line = %q", got)
	}
}

func TestEveryClassHasAHeadlineAndAHint(t *testing.T) {
	for _, kind := range []ConnectFailure{
		ConnectFailureUnreachable,
		ConnectFailureRefused,
		ConnectFailureTimeout,
		ConnectFailureDns,
		ConnectFailureAuth,
		ConnectFailureNoDatabase,
		ConnectFailureTls,
		ConnectFailureStartupOption,
		ConnectFailureOther,
	} {
		if kind.Headline() == "" {
			t.Fatalf("%v has no headline", kind)
		}
		if !strings.HasSuffix(kind.Hint(), ".") {
			t.Fatalf("%v hint %q does not end in a period", kind, kind.Hint())
		}
	}
}
