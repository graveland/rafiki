# SPDX-License-Identifier: Apache-2.0
"""Errors raised by rafiki's Python SDK.

ConnectError is the one error the wire produces: every non-2xx Connect
response and every in-stream error envelope carries a code from Connect's
fixed vocabulary ("unavailable", "permission_denied", "invalid_argument", ...).
The SDK's retry rule keys on exactly one of them — see rafiki.connect.
"""

from __future__ import annotations

import json

# Connect's code vocabulary (the strings a Connect server writes into error
# bodies). A handful is spelled here for readability; the wire may carry any
# of the full set, which ConnectError accepts verbatim.
CODE_CANCELED = "canceled"
CODE_UNKNOWN = "unknown"
CODE_INVALID_ARGUMENT = "invalid_argument"
CODE_DEADLINE_EXCEEDED = "deadline_exceeded"
CODE_NOT_FOUND = "not_found"
CODE_ALREADY_EXISTS = "already_exists"
CODE_PERMISSION_DENIED = "permission_denied"
CODE_RESOURCE_EXHAUSTED = "resource_exhausted"
CODE_FAILED_PRECONDITION = "failed_precondition"
CODE_ABORTED = "aborted"
CODE_OUT_OF_RANGE = "out_of_range"
CODE_UNIMPLEMENTED = "unimplemented"
CODE_INTERNAL = "internal"
CODE_UNAVAILABLE = "unavailable"
CODE_DATA_LOSS = "data_loss"
CODE_UNAUTHENTICATED = "unauthenticated"

# HTTP status → Connect code, for error responses that arrive without a
# Connect body (a proxy answering a bare 404, a TLS listener refusing a
# plaintext dial). This mirrors connect-go's mapping; the 503-with-body case
# never reaches it because the body's own code wins.
_HTTP_TO_CODE = {
    400: CODE_INVALID_ARGUMENT,
    401: CODE_UNAUTHENTICATED,
    403: CODE_PERMISSION_DENIED,
    404: CODE_UNIMPLEMENTED,
    405: CODE_UNIMPLEMENTED,
    408: CODE_DEADLINE_EXCEEDED,
    429: CODE_UNAVAILABLE,
    502: CODE_UNAVAILABLE,
    503: CODE_UNAVAILABLE,
    504: CODE_UNAVAILABLE,
}


class ConnectError(Exception):
    """One Connect error: an HTTP status, a Connect code, a message.

    Never subclassed per code: the code is DATA on this protocol, and a
    daemon can emit any code at any time. Check ``error.code`` (or the
    :attr:`unavailable` convenience) instead of catching subclasses.
    """

    def __init__(self, code: str, message: str = "", status: int | None = None):
        self.code = code
        self.message = message
        self.status = status
        super().__init__("%s%s: %s" % (code, " (http %d)" % status if status else "", message))

    @property
    def unavailable(self) -> bool:
        """True when this error is the one signal the SDK retries on."""
        return self.code == CODE_UNAVAILABLE


def error_from_http(status: int, body: bytes | None) -> ConnectError:
    """Build the ConnectError a response describes.

    A Connect error body is ``{"code": "...", "message": "..."}`` — the
    proxy's 503-with-body is the canonical case. A body that does not parse,
    or carries no code, falls back to the HTTP status's Connect mapping, so a
    bodiless 404 still reads "unimplemented" instead of "unknown".
    """
    if body:
        try:
            parsed = json.loads(body)
            if isinstance(parsed, dict) and parsed.get("code"):
                return ConnectError(str(parsed["code"]), str(parsed.get("message", "")), status)
        except (ValueError, TypeError):
            pass
    return ConnectError(_HTTP_TO_CODE.get(status, CODE_UNKNOWN), (body or b"").decode(errors="replace")[:200], status)


class ClientSetupError(Exception):
    """The client could not be constructed: no RAFIKI_CHILD_CONNECT inside a
    child, no rafiki binary on PATH for from_profile, a profile with no
    endpoint, a remote profile with no token. Nothing was sent anywhere."""


class StreamEnded(Exception):
    """Internal: a server stream closed without an error envelope. Not
    raised to callers — the client's watch loops translate it into the clean
    end (or the follow-up poll) a truncated stream can only mean."""
