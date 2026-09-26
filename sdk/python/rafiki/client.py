# SPDX-License-Identifier: Apache-2.0
"""rafiki's Python SDK client.

Three ways to build one, per the script-children design:

- ``Client.inside()`` — inside a script child: connects through the
  per-child unix socket in ``RAFIKI_CHILD_CONNECT``. The socket IS the
  credential (the daemon's proxy injects the child's secret per request and
  strips anything a caller sends), so no token is ever set and no
  Authorization header is ever sent. Identity = the child.
- ``Client.from_profile(name=None)`` — standalone (tools, CI, drivers):
  shells out to ``rafiki profile show [name] -o json`` and builds the client
  from the resolved record — pkg/profile stays the only resolver, and the
  SDK parses no profile files. Identity = the profile's user.
- ``Client(url, token=...)`` — explicit: a unix socket path, ``unix://``,
  ``http+unix://``, or an ``http(s)://`` URL.

Every control-plane method maps to one Connect RPC of ``rafiki.v1.Control``
(see docs/reference/control-protocol.md); arguments and returns are the
generated dataclasses in ``rafiki._gen``.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import time
from typing import Iterable

from . import _gen
from .connect import ConnectClient
from .errors import (
    CODE_DEADLINE_EXCEEDED,
    ClientSetupError,
    ConnectError,
    StreamEnded,
)

# Ceiling on an untimed settled() watch's idle read over a remote (TCP)
# endpoint: 0.0 would mean an infinite read timeout, and a silently-dropped
# TCP connection looks exactly like a quiet stream — the watch would hang
# forever. Unix sockets keep the unbounded wait (death is EOF). See
# _settle_one.remaining.
REMOTE_IDLE_CEILING = 60.0

# The states that mean "settled": a fundi child sits idle between turns
# (agent_settled is pi's true-idle event; the daemon maps it to idle), and
# exited is terminal. Everything else (spawning, streaming, tool_running,
# compacting, blocked_ui, shutting_down) is still working.
SETTLED_STATES = ("idle", "exited")

# The durable event types a settle arrives as. A fundi or claude child
# publishes agent_status (idle is the settle; exited the terminal form); a
# SCRIPT child has no turns — its whole life is one run — so its settle is
# the child_exited event alone. Watching both is the kind-agnostic way.
SETTLE_EVENT_TYPES = ["agent_status", "child_exited"]


def _mode_wire(mode: str) -> str:
    """Accept "prompt"/"steer"/"abort" (any case, with - or _) and return the
    proto enum name."""
    name = mode.strip().replace("-", "_").upper()
    if not name.startswith("SEND_MODE_"):
        name = "SEND_MODE_" + name
    return name


def _labels_wire(labels: dict | None) -> dict:
    if labels is None:
        return {}
    return {str(k): str(v) for k, v in labels.items()}


class Settle:
    """One child's settle: the state it landed in (see SETTLED_STATES) and,
    when it arrived as child_exited, the exit code (None when the child was
    signalled)."""

    def __init__(self, child_id: str, state: str, exit_code: "int | None" = None):
        self.child_id = child_id
        self.state = state
        self.exit_code = exit_code

    def __repr__(self) -> str:
        return "Settle(child_id=%r, state=%r, exit_code=%r)" % (self.child_id, self.state, self.exit_code)


class Client:
    """A Connect client for rafiki's control plane. See the module docstring
    for the three constructors."""

    def __init__(
        self,
        url: str | None = None,
        token: str | None = None,
        *,
        child: bool = False,
        max_attempts: int = 6,
    ):
        if url is None:
            raise ClientSetupError("Client(url, token=...) needs a URL: a unix socket path, unix:///path, http+unix:///path, or an http(s):// URL")
        self._conn = ConnectClient(url, token, send_auth=not child, max_attempts=max_attempts)
        # inside() clients never carry a token (the socket is the credential);
        # remember why for the error messages that matter.
        self._child = child

    # ── constructors ─────────────────────────────────────────────────────────

    @classmethod
    def inside(cls) -> "Client":
        """Build the client a script child uses: the per-child unix socket
        from ``RAFIKI_CHILD_CONNECT``, no token ever sent."""
        sock = os.environ.get("RAFIKI_CHILD_CONNECT")
        if not sock:
            raise ClientSetupError(
                "RAFIKI_CHILD_CONNECT is not set: Client.inside() only works inside a "
                "script child, which the daemon launches with exactly that one "
                "control variable. Standalone callers want Client.from_profile()."
            )
        return cls("unix://" + sock, child=True)

    @classmethod
    def from_profile(cls, name: "str | None" = None) -> "Client":
        """Build the standalone client from a rafiki profile, resolved the
        only way profiles resolve: ``rafiki profile show [name] -o json``.

        The record's ``connect_socket`` (a local profile) or ``url`` (a
        remote one) names the endpoint; ``token`` carries the resolved
        credential — empty on a local profile with none, in which case the
        socket admits anonymously (per-user reads resolve to nobody).
        """
        rafiki = shutil.which("rafiki")
        if rafiki is None:
            raise ClientSetupError(
                "rafiki is not on PATH: Client.from_profile() shells out to "
                "`rafiki profile show -o json` and lets pkg/profile do the resolving"
            )
        argv = [rafiki, "profile", "show"]
        if name:
            argv.append(name)
        argv += ["-o", "json"]
        proc = subprocess.run(argv, capture_output=True, text=True)
        if proc.returncode != 0:
            raise ClientSetupError(
                "rafiki profile show %s failed (exit %d): %s"
                % (name or "", proc.returncode, (proc.stderr or proc.stdout).strip())
            )
        try:
            rec = json.loads(proc.stdout)
        except ValueError as exc:
            head = proc.stdout.strip()[:80]
            raise ClientSetupError(
                "rafiki profile show printed %sJSON (%s): the rafiki on PATH likely "
                "predates -o json on profile show — rebuild and reinstall rafiki "
                "from this tree" % ("no " if not head else "malformed ", head or "empty output")
            ) from exc
        if not isinstance(rec, dict):
            raise ClientSetupError("rafiki profile show printed %r, expected one JSON record" % rec)
        token = rec.get("token") or None
        connect_socket = rec.get("connect_socket")
        url = rec.get("url")
        if connect_socket:
            return cls("unix://" + connect_socket, token)
        if url:
            if not token:
                raise ClientSetupError(
                    "profile %r names a remote daemon (%s) with no token: standalone SDK "
                    "calls would 401. Write the token file or re-add the profile with one."
                    % (rec.get("name"), url)
                )
            return cls(url, token)
        raise ClientSetupError("profile %r names no endpoint (neither socket nor url)" % rec.get("name"))

    # ── low-level ────────────────────────────────────────────────────────────

    def _call(self, method: str, request, response_cls):
        return self._conn.call(method, request.to_dict(), lambda d: response_cls.from_dict(d))

    # ── lifecycle ────────────────────────────────────────────────────────────

    def spawn(
        self,
        prompt: "str | None" = None,
        *,
        preset: "str | None" = None,
        kind: "str | None" = None,
        name: "str | None" = None,
        model: "str | None" = None,
        cwd: "str | None" = None,
        labels: "dict | None" = None,
        parent_child_id: "str | None" = None,
        executor: "str | None" = None,
        executor_ref: "str | None" = None,
        script: "dict | _gen.control_pb.SpawnRequest.ScriptSpec | None" = None,
        prefill: "Iterable | None" = None,
        max_cost: "float | None" = None,
        max_depth: "int | None" = None,
        max_children: "int | None" = None,
    ) -> str:
        """Spawn a child; returns its id. ``prompt``, when given, is sent as
        the child's first message (a spawn alone starts no work — fundi
        children idle until prompted, script children run their module).

        ``script`` is the ScriptSpec for kind="script": a dict
        ``{"repo": ..., "script": ..., "modules": [...], "args": [...]}`` or
        a generated ``SpawnRequest.ScriptSpec``.

        From a child credential, ``parent_child_id`` is forced to the
        caller's own id by the daemon; from a user credential it names the
        parent under which to spawn (empty = top level). ``cwd`` defaults
        to the calling process's working directory — the daemon refuses an
        empty one.
        """
        req = _gen.control_pb.SpawnRequest(
            cwd=cwd if cwd else os.getcwd(),
            name=name or "",
            model=model or "",
            kind=kind or "",
            labels=_labels_wire(labels),
            parent_child_id=parent_child_id or "",
            executor_selector=executor or "",
            executor_ref=executor_ref or "",
            preset=preset or "",
            max_depth=max_depth,
            max_cost=max_cost,
            max_children=max_children,
        )
        if script is not None:
            req.script = _script_spec(script)
        if prefill is not None:
            req.prefill = [
                p if isinstance(p, _gen.control_pb.PrefillRead) else _gen.control_pb.PrefillRead(**p)
                for p in prefill
            ]
        resp = self._call("Spawn", req, _gen.control_pb.SpawnResponse)
        child_id = resp.child_id
        if prompt:
            self.send(child_id, prompt)
        return child_id

    def get(self, child_id: str) -> _gen.control_pb.ChildSummary:
        """One child's summary: status, cost, labels, latest_ordinal, and —
        for a script child that called result() — its stored result JSON."""
        return self._call("GetChild", _gen.control_pb.GetChildRequest(child_id=child_id), _gen.control_pb.GetChildResponse).child

    def list(self, *, statuses: "Iterable[str] | None" = None, labels: "dict | None" = None):
        """The caller's children — a user's whole tree, a child's subtree —
        optionally narrowed to statuses and, client-side, to children whose
        labels carry every given pair (ListChildrenRequest filters by status
        only; labels are filtered here, on the summaries it returns)."""
        req = _gen.control_pb.ListChildrenRequest(statuses=list(statuses) if statuses else [])
        children = self._call("ListChildren", req, _gen.control_pb.ListChildrenResponse).children
        if labels:
            want = {str(k): str(v) for k, v in labels.items()}
            children = [c for c in children if all(c.labels.get(k) == v for k, v in want.items())]
        return children

    def send(self, child_id: str, msg: str, *, mode: str = "prompt", attachments: "Iterable | None" = None) -> str:
        """Submit work to a child via its inbox: prompt (queues), steer
        (injects into a running turn) or abort (cancels it — blocks ignored).
        Returns the durable inbox row id. ``attachments`` are extra blocks
        beside the text, each ``{"image": {"mediaType": ..., "data": bytes}}``
        or a generated ContentBlock."""
        blocks = [_gen.event_pb.ContentBlock(index=0, text=_gen.event_pb.TextBlock(text=msg))]
        for a in attachments or []:
            blocks.append(a if isinstance(a, _gen.event_pb.ContentBlock) else _block_from_dict(a))
        req = _gen.control_pb.SendRequest(child_id=child_id, mode=_mode_wire(mode), blocks=blocks)
        return self._call("Send", req, _gen.control_pb.SendResponse).message_id

    def stop(self, child_id: str, *, shutdown_timeout_ms: "int | None" = None, kill_timeout_ms: "int | None" = None):
        """Stop a child: the graceful window first, then the SIGTERM/SIGKILL
        rungs of the kill ladder at the caller's timeouts. Returns the
        KillResponse (exit_code is None for a signalled child). The child's
        own daemon-managed descendants are NOT swept."""
        req = _gen.control_pb.KillRequest(
            child_id=child_id,
            shutdown_timeout_ms=shutdown_timeout_ms or 0,
            kill_timeout_ms=kill_timeout_ms or 0,
        )
        return self._call("Kill", req, _gen.control_pb.KillResponse)

    def export(self, child_id: str) -> _gen.control_pb.ConversationExportResponse:
        """One child's decomposed transcript. ConversationExport speaks
        conversation UUIDs, so the child's session id is resolved first —
        which is exactly what claude children and script children lack (no
        DB conversation), and the error says so rather than 404-ing."""
        child = self.get(child_id)
        session = child.session_id
        if not session:
            raise ConnectError(
                "invalid_argument",
                "child %s has no conversation to export (its session id is empty — "
                "claude and script children have none)" % child_id,
            )
        req = _gen.control_pb.ConversationExportRequest(conversation_id=session)
        return self._call("ConversationExport", req, _gen.control_pb.ConversationExportResponse)

    # ── script-child verbs (Report / Receive / SetResult) ────────────────────

    def report(self, kind: str, data) -> None:
        """Publish one progress report from a script child to its parent's
        event buffer (a top-level script appends to its own event log). The
        data — any JSON value — is serialized compactly; the daemon parses
        it after trimming whitespace, stores the trimmed value, and refuses
        anything over 4 KiB or malformed with invalid_argument (never
        truncated)."""
        if not isinstance(kind, str) or not kind:
            raise ValueError("report(kind, data): kind is required")
        req = _gen.control_pb.ReportRequest(kind=kind, data_json=json.dumps(data, separators=(",", ":")))
        self._call("Report", req, _gen.control_pb.ReportResponse)

    def result(self, obj) -> None:
        """Store the calling script child's structured final result — verbatim
        JSON, ≤4 KiB, last write wins; the value present when the child
        settles rides the settle fragment and GetChild().result. Self-only:
        a user credential has no result to set."""
        req = _gen.control_pb.SetResultRequest(result_json=json.dumps(obj, separators=(",", ":")))
        self._call("SetResult", req, _gen.control_pb.SetResultResponse)

    def receive(self, child_id: "str | None" = None):
        """Yield the calling script child's inbox as it is delivered:
        ``ScriptMessage.Text`` rows (text, prompt/steer mode, the durable
        row id on ``message_ids``, attachments) and ``ScriptMessage.Stop``
        when the daemon stops the child — an inbox abort, the child entering
        shutting_down, or its exit. Each stop ENDS the stream, and the text
        rows ahead of an abort row in the same batch are delivered before it
        (work first, stop last; rows behind it are discarded) — so this
        generator ends right after a Stop and a caller reads it as the
        shutdown signal.

        Delivery is at-most-once: rows are consumed at the pull, so a stream
        that dies between pull and wire loses what it had not yet yielded.
        Unavailable gaps (a daemon restarting) are retried with bounded
        backoff per that contract.
        """
        req = _gen.control_pb.ReceiveRequest(child_id=child_id or "")
        while True:
            try:
                for payload in self._conn.stream(
                    "Receive", req.to_dict(), on_resume=lambda: None
                ):
                    msg = _gen.control_pb.ScriptMessage.from_dict(payload)
                    yield msg
                    if msg.stop is not None:
                        return
                return  # clean stream end without a stop: the run is over
            except StreamEnded:
                # A stream truncated without its end-of-stream envelope is
                # not caller-visible (see StreamEnded's docstring): treat it
                # as the clean end it can only mean here — the run is over.
                return
            except ConnectError as exc:
                if exc.code != CODE_DEADLINE_EXCEEDED:
                    raise
                # A quiet window (the idle-read budget) — not an error: the
                # stream re-opens and pulls again. Unavailable gaps restart
                # inside stream() on the bounded budget.

    # ── settling ─────────────────────────────────────────────────────────────

    def settled(self, children: "Iterable[str] | None" = None):
        """Yield a Settle per child as it settles (reaches ``idle`` or
        ``exited``), watching each with a server-stream of its durable
        settle events — agent_status for a fundi or claude child,
        child_exited for a script child (whose whole life is one run, so its
        exit is its settle).

        Each child is polled first (GetChild): one that has already settled
        is reported at once; the rest are streamed from the child's current
        latest_ordinal, so a settle that lands between the poll and the
        stream's open is replayed by the cursor rather than missed. Children
        are watched in the order given — ``settled()`` yields in watch
        order, not settle order; ``wait()`` is the all-at-once form.
        """
        ids = list(children) if children is not None else []
        if not ids:
            ids = [c.child_id for c in self.list()]
        for child_id in ids:
            yield from self._settle_one(child_id)

    def wait(self, children: "Iterable[str]", *, timeout: "float | None" = None) -> "dict[str, str]":
        """Block until every named child has settled; returns child_id →
        final state. ``timeout`` bounds the WHOLE wait (seconds)."""
        ids = list(children)
        out: dict = {}
        deadline = None if timeout is None else time.monotonic() + timeout
        for child_id in ids:
            budget = None if deadline is None else max(0.0, deadline - time.monotonic())
            for settle in self._settle_one(child_id, timeout=budget):
                out[settle.child_id] = settle.state
        return out

    def _settle_one(self, child_id: str, *, timeout: "float | None" = None):
        """Watch one child to its settle, cursor-replayed so a settle that
        lands during a reconnect is seen rather than waited past forever."""
        summary = self.get(child_id)
        if summary.status in SETTLED_STATES:
            yield Settle(child_id, summary.status)
            return
        last_ordinal = summary.latest_ordinal or 0
        deadline = None if timeout is None else time.monotonic() + timeout

        seen = last_ordinal

        def remaining() -> float:
            # The idle-read budget for one attempt: the WAIT budget's
            # remainder when there is one (a quiet child raises
            # deadline_exceeded when the budget is spent). Untimed, the
            # budget is 0.0 — which _stream_once turns into read=None, an
            # INFINITE read timeout, NOT the transport's stream_idle
            # default. That unbounded wait is deliberate on a unix socket
            # (death is EOF, so the read always ends), but on a remote TCP
            # endpoint a silently-dropped connection would hang it forever,
            # so untimed watches there get REMOTE_IDLE_CEILING instead.
            if deadline is None:
                return 0.0 if self._conn.is_unix else REMOTE_IDLE_CEILING
            return max(0.05, deadline - time.monotonic())

        while True:
            req = _gen.control_pb.StreamEventsRequest(
                subject=_gen.control_pb.EventSubject(child=child_id),
                tier="EVENT_TIER_DURABLE",
                types=SETTLE_EVENT_TYPES,
                cursor=_gen.control_pb.EventCursor(ordinals={child_id: seen}),
            )
            try:
                for event in self._conn.stream(
                    "StreamEvents", req.to_dict(), read_timeout=remaining()
                ):
                    ev = _gen.event_pb.Event.from_dict(event)
                    state = None
                    exit_code = None
                    if ev.agent_status is not None:
                        state = ev.agent_status.state
                    elif ev.child_exited is not None:
                        # A script child's settle: its exit IS its result.
                        state = "exited"
                        exit_code = ev.child_exited.exit_code
                    if state is None:
                        continue
                    if ev.ordinal is not None:
                        seen = max(seen, ev.ordinal)
                    if state in SETTLED_STATES:
                        yield Settle(child_id, state, exit_code)
                        return
                # Clean stream end without a settle: poll once more, in case
                # the terminal status raced the stream closed.
                summary = self.get(child_id)
                if summary.status in SETTLED_STATES:
                    yield Settle(child_id, summary.status)
                    return
                continue
            except StreamEnded:
                # Truncated without an end-of-stream envelope: not
                # caller-visible (StreamEnded's docstring). Treat it as the
                # clean end it can only mean here: poll once for a settle
                # that raced the stream closed, else keep watching.
                summary = self.get(child_id)
                if summary.status in SETTLED_STATES:
                    yield Settle(child_id, summary.status)
                    return
                continue
            except ConnectError as exc:
                if exc.code != CODE_DEADLINE_EXCEEDED:
                    raise
                if deadline is not None and time.monotonic() >= deadline:
                    raise TimeoutError(
                        "child %s did not settle within the wait timeout" % child_id
                    ) from exc
                # A quiet window inside an untimed wait: re-open and keep
                # watching from the last ordinal seen.


def _script_spec(spec) -> _gen.control_pb.SpawnRequest.ScriptSpec:
    if isinstance(spec, _gen.control_pb.SpawnRequest.ScriptSpec):
        return spec
    if isinstance(spec, dict):
        return _gen.control_pb.SpawnRequest.ScriptSpec(
            repo=str(spec.get("repo", "")),
            script=str(spec.get("script", "")),
            modules=[str(m) for m in spec.get("modules") or []],
            args=[str(a) for a in spec.get("args") or []],
        )
    raise ValueError("script spec: a dict {repo, script, modules, args} or a SpawnRequest.ScriptSpec")


def _block_from_dict(d: dict) -> _gen.event_pb.ContentBlock:
    """One content block from a dict shaped like the wire: {"text": {...}} /
    {"image": {...}} / a full ContentBlock mapping."""
    if "text" in d and isinstance(d["text"], dict):
        return _gen.event_pb.ContentBlock(index=d.get("index", 0), text=_gen.event_pb.TextBlock(**d["text"]))
    if "image" in d and isinstance(d["image"], dict):
        img = dict(d["image"])
        return _gen.event_pb.ContentBlock(index=d.get("index", 0), image=_gen.event_pb.ImageBlock(**img))
    return _gen.event_pb.ContentBlock.from_dict(d)
