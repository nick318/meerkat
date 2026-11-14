//! Why a connect failed, in words a person can act on.
//!
//! A driver's own error says what the library saw — "pool timed out while
//! waiting for an open connection", "error communicating with database" —
//! and the person looking at it has a different question: is it the
//! network, the server, or what I typed? [`ConnectFailure`] is the answer
//! to that question, and [`classify`] is the one place that decides it.
//!
//! The decision is a pure function over [`Evidence`], which is plain data:
//! a SQLSTATE, an `io::ErrorKind`, two flags and the message. A driver
//! fills the evidence in from whatever its library hands back, so the
//! rules live here once, know nothing about sqlx, and are argued with in
//! a test rather than against a server that has to be broken on purpose.

use std::fmt;
use std::io::ErrorKind;

/// The class of a failed connect. It picks the headline and the hint; the
/// server's or the system's own words travel beside it, in
/// [`ConnectError::detail`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConnectFailure {
    /// No route to the host or to its network: the packets had nowhere to
    /// go. Usually a VPN that is down.
    Unreachable,
    /// The host answered and nothing listens on that port.
    Refused,
    /// Nothing answered in time. A firewall that drops packets looks like
    /// this, and so does a host that is switched off.
    Timeout,
    /// The host name did not resolve to an address.
    Dns,
    /// The server refused the user: a wrong password, an unknown role, or
    /// a `pg_hba.conf` that has no line for this client.
    Auth,
    /// The server is there and the database is not.
    NoDatabase,
    /// The TLS handshake failed, or one side insisted on TLS and the other
    /// would not speak it.
    Tls,
    /// Something in front of the server — a connection pooler — refused
    /// the startup packet. A read-only session asks for
    /// `default_transaction_read_only` there, and PgBouncer refuses a
    /// startup parameter it was not told to pass on.
    StartupOption,
    /// None of the above. The detail is all there is.
    Other,
}

impl ConnectFailure {
    /// One short line that says what went wrong.
    pub fn headline(self) -> &'static str {
        match self {
            Self::Unreachable => "The host is unreachable",
            Self::Refused => "The connection was refused",
            Self::Timeout => "The connection timed out",
            Self::Dns => "The host name was not found",
            Self::Auth => "The server refused the sign-in",
            Self::NoDatabase => "The database does not exist",
            Self::Tls => "The secure connection failed",
            Self::StartupOption => "The pooler refused the session's options",
            Self::Other => "Could not connect",
        }
    }

    /// What to look at first. A hint, not a diagnosis: it names the usual
    /// cause of this class and nothing more.
    pub fn hint(self) -> &'static str {
        match self {
            Self::Unreachable => "Check the VPN or the network, then the host.",
            Self::Refused => {
                "Check the host and the port, and that the server is running and accepts TCP connections."
            }
            Self::Timeout => {
                "Check the VPN or a firewall, then the host and the port. A server that is starting up or out of connections also answers late."
            }
            Self::Dns => "Check the spelling of the host, and the VPN if the name is internal.",
            Self::Auth => {
                "Check the user name and the password. The server may also refuse this client's address or ask for TLS."
            }
            Self::NoDatabase => "Check the database name. It is case-sensitive.",
            Self::Tls => {
                "Check whether the server offers TLS and whether its certificate is valid for this host."
            }
            Self::StartupOption => {
                "A read-only session sends a startup option. Connect to the server directly, let the pooler pass `options` on, or turn read-only off for this connection."
            }
            Self::Other => "Check the connection's settings.",
        }
    }
}

/// What a driver knows about a failed connect, as plain data.
#[derive(Debug, Clone, Copy, Default)]
pub struct Evidence<'a> {
    /// The server's SQLSTATE, when the server is what refused.
    pub sqlstate: Option<&'a str>,
    /// The kind of the I/O error, when the socket is what failed.
    pub io: Option<ErrorKind>,
    /// The driver's own deadline ran out.
    pub timed_out: bool,
    /// The failure came out of the TLS layer.
    pub tls: bool,
    /// The error's text. Read only where nothing better says what
    /// happened: a failed name lookup has no error kind of its own.
    pub message: &'a str,
}

/// Decide the class of a failed connect.
///
/// The order is from the most certain evidence to the least: what the
/// server said outright, then what the socket reported, then the driver's
/// own deadline, and the wording of the message last.
pub fn classify(evidence: &Evidence<'_>) -> ConnectFailure {
    let message = evidence.message.to_ascii_lowercase();
    if let Some(state) = evidence.sqlstate {
        // PgBouncer refuses a startup parameter with `08P01`, and so does
        // the server for a genuine protocol violation — so the wording
        // decides which this is.
        if message.contains("unsupported startup parameter") {
            return ConnectFailure::StartupOption;
        }
        return match state {
            // invalid_password, invalid_authorization_specification.
            "28P01" | "28000" => ConnectFailure::Auth,
            // invalid_catalog_name.
            "3D000" => ConnectFailure::NoDatabase,
            _ => ConnectFailure::Other,
        };
    }
    if evidence.tls {
        return ConnectFailure::Tls;
    }
    // A name that will not resolve arrives as an I/O error of no
    // particular kind; only its text says what it was.
    if is_lookup_failure(&message) {
        return ConnectFailure::Dns;
    }
    match evidence.io {
        Some(ErrorKind::ConnectionRefused) => return ConnectFailure::Refused,
        Some(ErrorKind::TimedOut) => return ConnectFailure::Timeout,
        Some(ErrorKind::HostUnreachable | ErrorKind::NetworkUnreachable) => {
            return ConnectFailure::Unreachable;
        }
        _ => {}
    }
    if evidence.timed_out {
        return ConnectFailure::Timeout;
    }
    ConnectFailure::Other
}

/// The resolver's wording, across the platforms it is worded on: the
/// standard library's own prefix, `getaddrinfo`'s messages on macOS and
/// Linux, and Windows's.
fn is_lookup_failure(lowered: &str) -> bool {
    [
        "failed to lookup address",
        "nodename nor servname",
        "name or service not known",
        "temporary failure in name resolution",
        "no such host is known",
        "no address associated with hostname",
    ]
    .iter()
    .any(|needle| lowered.contains(needle))
}

/// A connect that failed: its class, and the words that came with it.
///
/// It is an error type of its own so it survives `anyhow`: the driver
/// returns it, and the app reads it back with `downcast_ref` to paint the
/// headline, the detail and the hint apart.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConnectError {
    pub kind: ConnectFailure,
    /// The server's or the system's own words, unabridged.
    pub detail: String,
}

impl ConnectError {
    pub fn new(kind: ConnectFailure, detail: impl Into<String>) -> Self {
        Self {
            kind,
            detail: detail.into(),
        }
    }

    /// Classify, and keep the message as the detail.
    pub fn from_evidence(evidence: &Evidence<'_>) -> Self {
        Self::new(classify(evidence), evidence.message)
    }

    /// Read a connect failure back out of an `anyhow` error. An error that
    /// is not one — a keychain that would not open, a URL that would not
    /// parse — is `Other`, with the whole chain as its detail.
    pub fn of(error: &anyhow::Error) -> Self {
        match error.downcast_ref::<ConnectError>() {
            Some(known) => known.clone(),
            None => Self::new(ConnectFailure::Other, format!("{error:#}")),
        }
    }
}

impl fmt::Display for ConnectError {
    /// One line: the headline, then the detail. It is what a caller that
    /// knows nothing of the class paints, so it has to stand on its own.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} · {}", self.kind.headline(), self.detail)
    }
}

impl std::error::Error for ConnectError {}

#[cfg(test)]
mod tests {
    use super::*;

    fn server(sqlstate: &'static str, message: &'static str) -> Evidence<'static> {
        Evidence {
            sqlstate: Some(sqlstate),
            message,
            ..Evidence::default()
        }
    }

    fn io(kind: ErrorKind, message: &'static str) -> Evidence<'static> {
        Evidence {
            io: Some(kind),
            message,
            ..Evidence::default()
        }
    }

    #[test]
    fn a_wrong_password_and_an_unknown_role_are_a_refused_sign_in() {
        let wrong = server("28P01", "password authentication failed for user \"app\"");
        assert_eq!(classify(&wrong), ConnectFailure::Auth);
        let hba = server("28000", "no pg_hba.conf entry for host \"10.0.0.4\"");
        assert_eq!(classify(&hba), ConnectFailure::Auth);
    }

    #[test]
    fn a_missing_database_is_named() {
        let missing = server("3D000", "database \"shop\" does not exist");
        assert_eq!(classify(&missing), ConnectFailure::NoDatabase);
    }

    #[test]
    fn a_pooler_that_refuses_the_startup_option_is_not_a_protocol_error() {
        let pooler = server("08P01", "unsupported startup parameter: options");
        assert_eq!(classify(&pooler), ConnectFailure::StartupOption);
        // The same state with other words is whatever the server says.
        let protocol = server("08P01", "invalid startup packet layout");
        assert_eq!(classify(&protocol), ConnectFailure::Other);
    }

    #[test]
    fn the_socket_says_refused_unreachable_and_timed_out() {
        assert_eq!(
            classify(&io(ErrorKind::ConnectionRefused, "Connection refused")),
            ConnectFailure::Refused
        );
        assert_eq!(
            classify(&io(ErrorKind::HostUnreachable, "No route to host")),
            ConnectFailure::Unreachable
        );
        assert_eq!(
            classify(&io(ErrorKind::NetworkUnreachable, "Network is unreachable")),
            ConnectFailure::Unreachable
        );
        assert_eq!(
            classify(&io(ErrorKind::TimedOut, "Operation timed out")),
            ConnectFailure::Timeout
        );
    }

    #[test]
    fn a_name_that_does_not_resolve_is_read_off_its_text() {
        let mac = io(
            ErrorKind::Other,
            "failed to lookup address information: nodename nor servname provided, or not known",
        );
        assert_eq!(classify(&mac), ConnectFailure::Dns);
        let linux = io(ErrorKind::Other, "Name or service not known");
        assert_eq!(classify(&linux), ConnectFailure::Dns);
        // With no kind at all, the text is still enough.
        let bare = Evidence {
            message: "Temporary failure in name resolution",
            ..Evidence::default()
        };
        assert_eq!(classify(&bare), ConnectFailure::Dns);
    }

    #[test]
    fn the_drivers_own_deadline_is_a_timeout() {
        let late = Evidence {
            timed_out: true,
            message: "pool timed out while waiting for an open connection",
            ..Evidence::default()
        };
        assert_eq!(classify(&late), ConnectFailure::Timeout);
    }

    #[test]
    fn a_tls_failure_is_tls_whatever_the_socket_said() {
        let tls = Evidence {
            tls: true,
            io: Some(ErrorKind::InvalidData),
            message: "invalid peer certificate: UnknownIssuer",
            ..Evidence::default()
        };
        assert_eq!(classify(&tls), ConnectFailure::Tls);
    }

    #[test]
    fn what_the_server_said_outranks_everything_else() {
        let both = Evidence {
            sqlstate: Some("28P01"),
            timed_out: true,
            tls: true,
            message: "password authentication failed",
            ..Evidence::default()
        };
        assert_eq!(classify(&both), ConnectFailure::Auth);
    }

    #[test]
    fn nothing_known_is_other_and_keeps_the_words() {
        let odd = Evidence {
            message: "encountered unexpected or invalid data",
            ..Evidence::default()
        };
        let error = ConnectError::from_evidence(&odd);
        assert_eq!(error.kind, ConnectFailure::Other);
        assert_eq!(error.detail, "encountered unexpected or invalid data");
    }

    #[test]
    fn an_error_survives_anyhow_and_a_stranger_becomes_other() {
        let known = anyhow::Error::new(ConnectError::new(ConnectFailure::Refused, "refused"))
            .context("while opening the session");
        assert_eq!(ConnectError::of(&known).kind, ConnectFailure::Refused);

        let stranger = anyhow::anyhow!("no keychain").context("reading the password");
        let read = ConnectError::of(&stranger);
        assert_eq!(read.kind, ConnectFailure::Other);
        assert_eq!(read.detail, "reading the password: no keychain");
    }

    #[test]
    fn the_one_line_form_leads_with_the_headline() {
        let error = ConnectError::new(ConnectFailure::Auth, "password authentication failed");
        assert_eq!(
            error.to_string(),
            "The server refused the sign-in · password authentication failed"
        );
    }

    #[test]
    fn every_class_has_a_headline_and_a_hint() {
        for kind in [
            ConnectFailure::Unreachable,
            ConnectFailure::Refused,
            ConnectFailure::Timeout,
            ConnectFailure::Dns,
            ConnectFailure::Auth,
            ConnectFailure::NoDatabase,
            ConnectFailure::Tls,
            ConnectFailure::StartupOption,
            ConnectFailure::Other,
        ] {
            assert!(!kind.headline().is_empty());
            assert!(kind.hint().ends_with('.'));
        }
    }
}
