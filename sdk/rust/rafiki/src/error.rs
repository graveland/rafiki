// SPDX-License-Identifier: Apache-2.0
//! Connect errors: the 16 codes, and the wire error bodies that carry them.

use std::fmt;

use base64::alphabet;
use base64::engine::{DecodePaddingMode, GeneralPurpose, GeneralPurposeConfig};
use base64::Engine as _;
use prost::Message as _;
use serde::Deserialize;

/// Standard base64 with padding optional: connect-go and Go's encoders differ
/// on padding, and an `ErrorInfo` detail is unreadable if one side's spelling
/// is refused.
const BASE64_STD: GeneralPurpose = GeneralPurpose::new(
    &alphabet::STANDARD,
    GeneralPurposeConfig::new().with_decode_padding_mode(DecodePaddingMode::Indifferent),
);

/// The `type` of the detail an error's `reason` is read from.
const ERROR_INFO_TYPE: &str = "google.rpc.ErrorInfo";

/// The `ErrorInfo` reason a daemon attaches to the `failed_precondition` it
/// answers a peer whose epoch is missing or wrong (`pkg/rpcreason`).
pub(crate) const PROTOCOL_MISMATCH_REASON: &str = "protocol_mismatch";

/// `google.rpc.ErrorInfo`, decoded locally. Only `reason` is read; the message
/// mirrors the wire so decoding is a plain `prost` job.
#[derive(Clone, PartialEq, prost::Message)]
struct ErrorInfo {
    #[prost(string, tag = "1")]
    reason: String,
    #[prost(string, tag = "2")]
    domain: String,
}

/// A Connect error code, spelled on the wire in snake_case.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Code {
    Canceled,
    Unknown,
    InvalidArgument,
    DeadlineExceeded,
    NotFound,
    AlreadyExists,
    PermissionDenied,
    ResourceExhausted,
    FailedPrecondition,
    Aborted,
    OutOfRange,
    Unimplemented,
    Internal,
    Unavailable,
    DataLoss,
    Unauthenticated,
}

impl Code {
    /// Every code, in the order Connect lists them.
    pub const ALL: [Code; 16] = [
        Code::Canceled,
        Code::Unknown,
        Code::InvalidArgument,
        Code::DeadlineExceeded,
        Code::NotFound,
        Code::AlreadyExists,
        Code::PermissionDenied,
        Code::ResourceExhausted,
        Code::FailedPrecondition,
        Code::Aborted,
        Code::OutOfRange,
        Code::Unimplemented,
        Code::Internal,
        Code::Unavailable,
        Code::DataLoss,
        Code::Unauthenticated,
    ];

    /// The snake_case name the wire uses.
    pub fn as_str(self) -> &'static str {
        match self {
            Code::Canceled => "canceled",
            Code::Unknown => "unknown",
            Code::InvalidArgument => "invalid_argument",
            Code::DeadlineExceeded => "deadline_exceeded",
            Code::NotFound => "not_found",
            Code::AlreadyExists => "already_exists",
            Code::PermissionDenied => "permission_denied",
            Code::ResourceExhausted => "resource_exhausted",
            Code::FailedPrecondition => "failed_precondition",
            Code::Aborted => "aborted",
            Code::OutOfRange => "out_of_range",
            Code::Unimplemented => "unimplemented",
            Code::Internal => "internal",
            Code::Unavailable => "unavailable",
            Code::DataLoss => "data_loss",
            Code::Unauthenticated => "unauthenticated",
        }
    }

    /// Parse a wire code name. `None` for anything unrecognised, so a caller
    /// decides whether that is `Unknown` or a defect.
    pub fn from_wire(name: &str) -> Option<Code> {
        Code::ALL.into_iter().find(|code| code.as_str() == name)
    }
}

impl fmt::Display for Code {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl std::str::FromStr for Code {
    type Err = UnknownCode;

    fn from_str(name: &str) -> Result<Code, UnknownCode> {
        Code::from_wire(name).ok_or_else(|| UnknownCode(name.to_string()))
    }
}

/// The error [`Code`]'s `FromStr` returns for a name it does not know.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("unknown connect code {0:?}")]
pub struct UnknownCode(pub String);

/// A failed call: a Connect [`Code`], the peer's message, and — when the peer
/// said why — the `google.rpc.ErrorInfo` reason it attached.
#[derive(Debug, Clone, thiserror::Error)]
#[error("{code}: {message}")]
pub struct Error {
    pub code: Code,
    pub message: String,
    /// `google.rpc.ErrorInfo.reason`, when the peer attached one.
    pub reason: Option<String>,
    /// The HTTP status the error arrived with, when it arrived over HTTP at
    /// all (a local dial failure has none).
    pub http_status: Option<u16>,
    /// Set only on the error a stream's own idle window raised, so
    /// [`Error::is_idle`] never mistakes a unary timeout or a peer's
    /// `deadline_exceeded` for a quiet stream.
    idle: bool,
}

impl Error {
    pub fn new(code: Code, message: impl Into<String>) -> Error {
        Error {
            code,
            message: message.into(),
            reason: None,
            http_status: None,
            idle: false,
        }
    }

    /// The error a stream's idle window ends it with.
    pub(crate) fn stream_idle() -> Error {
        Error {
            idle: true,
            ..Error::new(Code::DeadlineExceeded, "stream idle")
        }
    }

    pub fn with_reason(mut self, reason: impl Into<String>) -> Error {
        self.reason = Some(reason.into());
        self
    }

    pub fn with_status(mut self, status: u16) -> Error {
        self.http_status = Some(status);
        self
    }

    /// The daemon refused us for speaking another wire epoch. Terminal: a
    /// retry cannot upgrade the daemon.
    pub fn is_protocol_mismatch(&self) -> bool {
        self.code == Code::FailedPrecondition
            && self.reason.as_deref() == Some(PROTOCOL_MISMATCH_REASON)
    }

    /// The daemon was not reachable (or a stream's connection died).
    pub fn is_unavailable(&self) -> bool {
        self.code == Code::Unavailable
    }

    pub fn is_unauthenticated(&self) -> bool {
        self.code == Code::Unauthenticated
    }

    pub fn is_not_found(&self) -> bool {
        self.code == Code::NotFound
    }

    /// The stream's idle window expired: it was quiet for the window the
    /// caller chose. Not a failure — re-open and resume from the caller's own
    /// cursor. A unary timeout is `deadline_exceeded` but NOT idle: the call's
    /// outcome is unknown, and a `send` must not be repeated on it.
    pub fn is_idle(&self) -> bool {
        self.idle
    }
}

/// A Connect error body, as both a unary non-200 and a stream's end-of-stream
/// trailer carry it.
#[derive(Debug, Deserialize)]
pub(crate) struct ConnectBody {
    #[serde(default)]
    code: String,
    #[serde(default)]
    message: String,
    #[serde(default)]
    details: Vec<Detail>,
}

#[derive(Debug, Deserialize)]
struct Detail {
    #[serde(rename = "type")]
    kind: String,
    #[serde(default)]
    value: String,
}

impl ConnectBody {
    fn into_error(self, status: u16) -> Error {
        let reason = self
            .details
            .iter()
            .find(|detail| detail.kind == ERROR_INFO_TYPE)
            .and_then(|detail| decode_reason(&detail.value));
        Error {
            // An unknown code name reads as Unknown, never as a code we would
            // retry or treat as terminal by accident.
            code: Code::from_wire(&self.code).unwrap_or(Code::Unknown),
            message: self.message,
            reason,
            http_status: Some(status),
            idle: false,
        }
    }
}

/// Decode an `ErrorInfo` detail's base64 `value` into its reason.
fn decode_reason(value: &str) -> Option<String> {
    let bytes = BASE64_STD.decode(value).ok()?;
    let info = ErrorInfo::decode(bytes.as_slice()).ok()?;
    (!info.reason.is_empty()).then_some(info.reason)
}

/// The error a non-200 response body carries, and whether the body was a
/// Connect error at all. `false` means something other than a Connect server
/// answered — a proxy's HTML page, an empty body, a truncated write — and the
/// status is all the peer told us.
pub(crate) fn from_body(status: u16, body: &[u8]) -> (Error, bool) {
    match serde_json::from_slice::<ConnectBody>(body) {
        Ok(parsed) if !parsed.code.is_empty() => (parsed.into_error(status), true),
        _ => (
            Error::new(code_for_status(status), format!("HTTP {status}")).with_status(status),
            false,
        ),
    }
}

/// The error a stream's end-of-stream envelope carries, or `None` for a clean
/// close. The payload is `{"error": {...} | null, "metadata"?: …}`.
pub(crate) fn from_trailer(payload: &[u8]) -> Option<Error> {
    #[derive(Deserialize)]
    struct Trailer {
        #[serde(default)]
        error: Option<ConnectBody>,
    }

    if payload.is_empty() {
        return None;
    }
    match serde_json::from_slice::<Trailer>(payload) {
        Ok(trailer) => trailer.error.map(|body| body.into_error(200)),
        // A trailer that is not JSON at all is not a clean close; say so
        // rather than ending a stream on a body nobody could read.
        Err(err) => Some(
            Error::new(
                Code::Internal,
                format!("malformed end-of-stream trailer: {err}"),
            )
            .with_status(200),
        ),
    }
}

/// The code an HTTP status implies when the body names none. The mapping is
/// Connect's own, so a proxy in front of the daemon is classified the same way
/// connect-go classifies it.
pub(crate) fn code_for_status(status: u16) -> Code {
    match status {
        400 => Code::Internal,
        401 => Code::Unauthenticated,
        403 => Code::PermissionDenied,
        404 => Code::Unimplemented,
        408 => Code::DeadlineExceeded,
        409 => Code::Aborted,
        412 => Code::FailedPrecondition,
        413 => Code::ResourceExhausted,
        415 => Code::Internal,
        429 => Code::Unavailable,
        431 => Code::ResourceExhausted,
        502..=504 => Code::Unavailable,
        _ => Code::Unknown,
    }
}

#[cfg(test)]
mod tests {
    use base64::engine::general_purpose::{STANDARD, STANDARD_NO_PAD};

    use super::*;

    /// An `ErrorInfo` detail's `value`, encoded the way its author chose.
    fn error_info_value(reason: &str, unpadded: bool) -> String {
        let info = ErrorInfo {
            reason: reason.to_string(),
            domain: "rafiki".to_string(),
        };
        let bytes = info.encode_to_vec();
        if unpadded {
            STANDARD_NO_PAD.encode(bytes)
        } else {
            STANDARD.encode(bytes)
        }
    }

    #[test]
    fn error_body_carries_reason() {
        for unpadded in [false, true] {
            let value = error_info_value("protocol_mismatch", unpadded);
            let body = serde_json::json!({
                "code": "failed_precondition",
                "message": "the daemon's own words",
                "details": [{"type": ERROR_INFO_TYPE, "value": value}],
            })
            .to_string();
            let (err, connect) = from_body(400, body.as_bytes());
            assert!(connect);
            assert_eq!(err.code, Code::FailedPrecondition);
            assert_eq!(err.message, "the daemon's own words");
            assert_eq!(err.reason.as_deref(), Some("protocol_mismatch"));
            assert_eq!(err.http_status, Some(400));
            assert!(err.is_protocol_mismatch());
        }
    }

    #[test]
    fn error_body_without_error_info_has_no_reason() {
        let body = br#"{"code": "not_found", "message": "no such child"}"#;
        let (err, connect) = from_body(404, body);
        assert!(connect);
        assert_eq!(err.code, Code::NotFound);
        assert_eq!(err.reason, None);
        assert!(err.is_not_found());
    }

    #[test]
    fn non_json_503_is_unavailable() {
        let (err, connect) = from_body(503, b"<html>upstream is restarting</html>");
        assert!(!connect, "a proxy's page is not a Connect body");
        assert_eq!(err.code, Code::Unavailable);
        assert!(err.is_unavailable());
        assert_eq!(err.reason, None);
    }

    /// JSON is not enough to be a Connect error: a body that names no code is
    /// some other server's.
    #[test]
    fn json_without_a_code_is_not_a_connect_body() {
        let (err, connect) = from_body(502, br#"{"error": "bad gateway"}"#);
        assert!(!connect);
        assert!(err.is_unavailable());
    }

    #[test]
    fn only_the_stream_idle_error_is_idle() {
        assert!(Error::stream_idle().is_idle());
        assert_eq!(Error::stream_idle().code, Code::DeadlineExceeded);
        assert!(!Error::new(Code::DeadlineExceeded, "timed out").is_idle());
    }

    #[test]
    fn an_unparseable_value_has_no_reason() {
        let body = serde_json::json!({
            "code": "failed_precondition",
            "message": "nope",
            "details": [{"type": ERROR_INFO_TYPE, "value": "!!! not base64 !!!"}],
        })
        .to_string();
        let (err, _) = from_body(412, body.as_bytes());
        assert_eq!(err.reason, None);
        assert!(!err.is_protocol_mismatch());
    }

    #[test]
    fn trailer_null_error_is_a_clean_close() {
        assert!(from_trailer(br#"{"error": null}"#).is_none());
        assert!(from_trailer(br#"{"metadata": {}}"#).is_none());
        assert!(from_trailer(b"").is_none());
    }

    #[test]
    fn trailer_error_is_that_error() {
        let value = error_info_value("protocol_mismatch", false);
        let payload = serde_json::json!({
            "error": {
                "code": "unauthenticated",
                "message": "invalid auth token",
                "details": [{"type": ERROR_INFO_TYPE, "value": value}],
            }
        })
        .to_string();
        let err = from_trailer(payload.as_bytes()).expect("a trailer error");
        assert_eq!(err.code, Code::Unauthenticated);
        assert!(err.is_unauthenticated());
        assert_eq!(err.reason.as_deref(), Some("protocol_mismatch"));
    }

    #[test]
    fn trailer_that_is_not_json_is_internal() {
        let err = from_trailer(b"not json at all").expect("an error");
        assert_eq!(err.code, Code::Internal);
    }

    #[test]
    fn codes_round_trip_through_their_wire_names() {
        for code in Code::ALL {
            assert_eq!(Code::from_wire(code.as_str()), Some(code));
            assert_eq!(code.to_string(), code.as_str());
            assert_eq!(code.as_str().parse::<Code>(), Ok(code));
        }
        assert_eq!(Code::from_wire("no_such_code"), None);
        assert!("no_such_code".parse::<Code>().is_err());
    }

    #[test]
    fn status_mapping_matches_connect() {
        let cases = [
            (400, Code::Internal),
            (401, Code::Unauthenticated),
            (403, Code::PermissionDenied),
            (404, Code::Unimplemented),
            (408, Code::DeadlineExceeded),
            (409, Code::Aborted),
            (412, Code::FailedPrecondition),
            (413, Code::ResourceExhausted),
            (415, Code::Internal),
            (429, Code::Unavailable),
            (431, Code::ResourceExhausted),
            (500, Code::Unknown),
            (502, Code::Unavailable),
            (503, Code::Unavailable),
            (504, Code::Unavailable),
        ];
        for (status, code) in cases {
            assert_eq!(code_for_status(status), code, "HTTP {status}");
        }
    }
}
