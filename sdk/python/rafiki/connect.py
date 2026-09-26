# SPDX-License-Identifier: Apache-2.0
"""The hand-written Connect JSON client: unary calls and enveloped
server-streams over httpx, on a unix socket or TCP (http/https).

Wire facts this module owns (docs/reference/control-protocol.md §2.3):

- unary: POST ``{base}/{service}/{Method}`` with
  ``Content-Type: application/json``; a 200 body is the response message's
  JSON, anything else is a Connect error body (``{"code", "message"}``).
- server-streaming: same path, ``Content-Type: application/connect+json``,
  request body ONE envelope (flags byte, 4-byte big-endian length, payload),
  response a sequence of envelopes; the flags byte's low bit marks the
  end-of-stream envelope, whose payload is ``{"error": {...}}`` (error null
  on a clean close).
- a server stream's headers arrive with its FIRST message, not before: a
  stream with nothing yet to say sends no bytes at all. A client therefore
  budgets an idle-read timeout (``read_timeout``) — a quiet window is the
  normal case, and the SDK surfaces it as code "deadline_exceeded" so callers
  can re-open rather than mistake it for a dead daemon.

Retry rule: UNAVAILABLE only. The proxy's HTTP 503 with a Connect body naming
code "unavailable" is the signal a retrying client waits on (a daemon
restarting behind the per-child socket); a failed dial means the same thing
and retries on the same budget. PermissionDenied, InvalidArgument,
deadline_exceeded and every other code raise through untouched — retrying an
authority refusal would be a busy-wait against a wall.
"""

from __future__ import annotations

import json
import struct
import time
from typing import Callable, Iterator

import httpx

from .errors import (
    CODE_DEADLINE_EXCEEDED,
    CODE_UNAVAILABLE,
    ConnectError,
    StreamEnded,
    error_from_http,
)
from ._gen import control_pb as _control_pb

# The protocol version header every Connect request carries.
PROTOCOL_HEADER = {"Connect-Protocol-Version": "1"}

# Reserved invalid host for unix-socket endpoints, matching the Go client's
# connectUDSBaseURL: a misconfiguration that bypasses the dialer fails loudly
# instead of reaching a real host.
UDS_BASE_URL = "http://connect.rafiki.invalid"

# Content types: the unary codec and the enveloped streaming codec.
CONTENT_UNARY = "application/json"
CONTENT_STREAM = "application/connect+json"

# An envelope's flags byte: bit 0 = compressed (unsupported here), bit 1 =
# end-of-stream.
_FLAG_COMPRESSED = 0x01
_FLAG_END = 0x02


def normalize_url(url: str) -> "tuple[str, str | None]":
    """Split a Client URL into (base URL, unix socket path or None).

    Accepted spellings:

    - ``/path/to/connect.sock`` — a bare filesystem path is a unix socket
    - ``unix:///path/to/connect.sock`` — explicit scheme
    - ``http+unix:///path/to/connect.sock`` — explicit scheme
    - ``https://host[:port]`` / ``http://host[:port]`` — TCP, TLS or not
    """
    if url.startswith("unix://"):
        return UDS_BASE_URL, url[len("unix://") :]
    if url.startswith("http+unix://"):
        return UDS_BASE_URL, url[len("http+unix://") :]
    if "://" in url:
        return url, None
    if url.startswith("/"):
        return UDS_BASE_URL, url
    raise ValueError("url %r: not a unix socket path, unix:/// or http(s):// URL" % url)


class Backoff:
    """Bounded exponential backoff: base * 2**attempt, capped."""

    def __init__(self, base: float = 0.1, max_delay: float = 2.0):
        self.base = base
        self.max_delay = max_delay

    def delay(self, attempt: int) -> float:
        return min(self.max_delay, self.base * (2**attempt))


def _timeout_of(exc: httpx.HTTPError) -> "str | None":
    """Classify an httpx transport error: "unavailable" for everything that
    means the daemon was not reachable (dial refused, dial timed out,
    connection reset), "deadline_exceeded" for an idle-read timeout (the
    stream was up and simply quiet)."""
    if isinstance(exc, (httpx.ConnectError, httpx.ConnectTimeout)):
        return CODE_UNAVAILABLE
    if isinstance(exc, httpx.ReadTimeout):
        return CODE_DEADLINE_EXCEEDED
    if isinstance(exc, httpx.TimeoutException):
        return CODE_DEADLINE_EXCEEDED
    return CODE_UNAVAILABLE


class ConnectClient:
    """The transport the Client methods ride.

    ``max_attempts`` bounds the UNAVAILABLE retry loop (the initial attempt
    plus ``max_attempts - 1`` retries); 1 disables retrying entirely.
    ``stream_idle`` is the default idle-read budget for server streams
    (seconds): a quiet stream raises deadline_exceeded after this long, and
    the caller decides whether that means "resume" or "give up".
    """

    def __init__(
        self,
        url: str,
        token: "str | None" = None,
        *,
        send_auth: bool = True,
        timeout: float = 30.0,
        stream_idle: float = 60.0,
        max_attempts: int = 6,
        backoff: "Backoff | None" = None,
    ):
        base, uds_path = normalize_url(url)
        self._base = base.rstrip("/")
        self._uds = uds_path
        self._token = token
        self._send_auth = send_auth
        self._timeout = timeout
        self._stream_idle = stream_idle
        self._max_attempts = max(1, max_attempts)
        self._backoff = backoff or Backoff()
        if uds_path is not None:
            self._http = httpx.Client(
                transport=httpx.HTTPTransport(uds=uds_path),
                timeout=httpx.Timeout(timeout),
            )
        else:
            self._http = httpx.Client(timeout=httpx.Timeout(timeout))

    def close(self) -> None:
        self._http.close()

    # ── unary ────────────────────────────────────────────────────────────────

    def call(self, method: str, payload: dict, response_factory: "Callable[[dict], object]") -> object:
        """One unary call, retried on Unavailable with bounded backoff.

        ``response_factory`` decodes the response body's dict into the
        generated class; pass a class with ``from_dict`` (or ``dict``) to get
        the shape the caller wants.
        """
        attempt = 0
        while True:
            try:
                body = self._unary_once(method, payload)
                return response_factory(body)
            except ConnectError as exc:
                if not exc.unavailable or attempt + 1 >= self._max_attempts:
                    raise
                time.sleep(self._backoff.delay(attempt))
                attempt += 1

    def _url(self, method: str) -> str:
        return "%s/%s/%s" % (self._base, _control_pb.SERVICE_CONTROL, method)

    def _headers(self, content_type: str) -> dict:
        headers = dict(PROTOCOL_HEADER)
        headers["Content-Type"] = content_type
        # A child-credential socket is the credential: the proxy strips any
        # inbound Authorization and injects the child's own secret, so
        # inside() clients are built with send_auth=False and nothing is
        # ever sent here.
        if self._token and self._send_auth:
            headers["Authorization"] = "Bearer " + self._token
        return headers

    def _unary_once(self, method: str, payload: dict) -> dict:
        try:
            resp = self._http.post(
                self._url(method), content=json.dumps(payload), headers=self._headers(CONTENT_UNARY)
            )
        except httpx.HTTPError as exc:
            # A failed dial means the daemon is unreachable — the same state
            # a 503 names, and retried on the same budget by call(). An idle
            # timeout on a unary call is the daemon being wedged, not down:
            # deadline_exceeded, never retried.
            code = _timeout_of(exc)
            raise ConnectError(code, "%s against %s: %s" % (code, self._describe(), exc)) from exc
        if resp.status_code == 200:
            if not resp.content:
                return {}
            try:
                return resp.json()
            except ValueError as exc:
                raise ConnectError("internal", "malformed response body: %s" % exc, 200) from exc
        raise error_from_http(resp.status_code, resp.content)

    def _describe(self) -> str:
        return self._uds if self._uds else self._base

    @property
    def is_unix(self) -> bool:
        """True when the transport rides a unix socket (death is EOF, so an
        unbounded read can only ever end, never hang); False for a remote
        (TCP) endpoint, where a dropped connection can look like silence."""
        return self._uds is not None

    # ── server-streaming ─────────────────────────────────────────────────────

    def stream(
        self,
        method: str,
        payload: dict,
        *,
        read_timeout: "float | None" = None,
        on_resume: "Callable[[], None] | None" = None,
    ) -> "Iterator[dict]":
        """Yield the JSON payload of every envelope a server-stream sends.

        ``read_timeout`` is the idle budget per attempt (default: the
        client's ``stream_idle``); a quiet window surfaces as a
        ``ConnectError`` with code ``deadline_exceeded`` — this generator
        does NOT retry it (a quiet stream is normal, and only the caller
        knows whether quiet means resume or give up).

        Retried on Unavailable with bounded backoff while nothing has been
        delivered yet (the daemon-restart gap). Once messages HAVE flowed, a
        mid-stream unavailability ends iteration by raising — unless
        ``on_resume`` was given, in which case the hook runs (settled()
        advances its cursor there; payloads already yielded are not
        re-yielded) and the stream re-opens on the same bounded budget.
        Receive() passes no hook and documents its at-most-once contract
        instead.
        """
        attempt = 0
        while True:
            delivered = [False]

            def mark() -> None:
                delivered[0] = True

            try:
                yield from self._stream_once(method, payload, mark, read_timeout)
                return
            except ConnectError as exc:
                # Idle quiet is not an error condition: hand it to the caller.
                if exc.code == CODE_DEADLINE_EXCEEDED:
                    raise
                if not exc.unavailable or attempt + 1 >= self._max_attempts:
                    raise
                if delivered[0] and on_resume is None:
                    raise
                if on_resume is not None:
                    on_resume()
                time.sleep(self._backoff.delay(attempt))
                attempt += 1

    def _stream_once(
        self,
        method: str,
        payload: dict,
        mark_delivered: "Callable[[], None]",
        read_timeout: "float | None",
    ) -> "Iterator[dict]":
        idle = self._stream_idle if read_timeout is None else read_timeout
        req = self._http.build_request(
            "POST",
            self._url(method),
            content=_envelop(json.dumps(payload).encode()),
            headers=self._headers(CONTENT_STREAM),
            # The dial keeps the client's dial timeout; the READ carries the
            # idle budget — this is what bounds a stream the daemon has
            # nothing to say on yet (its headers arrive with the first
            # message, so the budget also covers the open).
            timeout=httpx.Timeout(self._timeout, read=idle if idle and idle > 0 else None),
        )
        try:
            resp = self._http.send(req, stream=True)
        except httpx.HTTPError as exc:
            raise ConnectError(_timeout_of(exc), "%s opening stream: %s" % (_timeout_of(exc), exc)) from exc
        if resp.status_code != 200:
            content = resp.read()
            resp.close()
            raise error_from_http(resp.status_code, content)
        try:
            for flags, chunk in _envelopes(resp.iter_bytes()):
                if flags & _FLAG_COMPRESSED:
                    raise ConnectError("unimplemented", "compressed envelopes are not supported", 200)
                if flags & _FLAG_END:
                    # The end-of-stream envelope: {"error": {...}} on failure,
                    # {"error": null} (or nothing) on a clean close.
                    try:
                        trailer = json.loads(chunk) if chunk else {}
                    except ValueError:
                        trailer = {}
                    err = trailer.get("error") if isinstance(trailer, dict) else None
                    if err:
                        raise ConnectError(str(err.get("code", "unknown")), str(err.get("message", "")), 200)
                    return
                mark_delivered()
                if not chunk:
                    continue
                try:
                    yield json.loads(chunk)
                except ValueError as exc:
                    raise ConnectError("internal", "malformed stream payload: %s" % exc, 200) from exc
        except httpx.HTTPError as exc:
            # A read that outlived the idle budget, or a connection that died
            # mid-envelope: classify and surface (the retry policy lives in
            # stream()).
            raise ConnectError(_timeout_of(exc), "%s reading stream: %s" % (_timeout_of(exc), exc)) from exc
        finally:
            resp.close()


def _envelop(payload: bytes) -> bytes:
    """One request envelope: flags byte, 4-byte big-endian length, payload."""
    return b"\x00" + struct.pack(">I", len(payload)) + payload


def _envelopes(chunks: "Iterator[bytes]") -> "Iterator[tuple[int, bytes]]":
    """Yield (flags, payload) per envelope from a raw byte-chunk iterator.

    Chunk boundaries land anywhere — an httpx chunk may hold half a header,
    two payloads, or the tail of one envelope and the head of the next — so
    the reader buffers and slices exactly.
    """
    buf = b""
    while True:
        while len(buf) < 5:
            try:
                buf += next(chunks)
            except StopIteration:
                raise StreamEnded("stream closed mid-envelope") from None
        flags = buf[0]
        length = struct.unpack(">I", buf[1:5])[0]
        buf = buf[5:]
        while len(buf) < length:
            try:
                buf += next(chunks)
            except StopIteration:
                raise StreamEnded("stream closed mid-envelope") from None
        payload, buf = buf[:length], buf[length:]
        yield flags, payload
