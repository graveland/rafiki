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

import dataclasses
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

# The daemon's Kill-handler defaults (cmd/rafikid/controller.go: Kill passes
# durOrDefault(shutdownTimeoutMs, 180*time.Second) and
# durOrDefault(killTimeoutMs, 30*time.Second); pinned by
# TestKillTimeoutDefaults in cmd/rafikid). stop() mirrors them so its per-call
# read timeout covers exactly the work the daemon may do before answering.
DEFAULT_SHUTDOWN_MS = 180_000
DEFAULT_KILL_MS = 30_000

# Slack on top of the two windows above: the daemon answers only after the
# ladder completes AND the exit is persisted (waitForChildRemoval), so the
# read timeout is the two windows plus headroom, not the windows exactly.
STOP_TIMEOUT_SLACK_MS = 30_000

# The states that mean "settled": a fundi child sits idle between turns
# (agent_settled is pi's true-idle event; the daemon maps it to idle), and
# exited is terminal. Everything else (spawning, streaming, running,
# tool_running, compacting, blocked_ui, shutting_down) is still working --
# "running" is a script child's steady state between spawn and exit.
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
        # child_id -> the child's event-log ordinal just before this client's
        # latest prompt Send. While set, a settle must be a busy -> idle
        # transition AFTER that ordinal: a fundi child reports idle between
        # the Send and its turn starting, and settling on that idle reads a
        # transcript the prompt has not reached. See _settle_one.
        self._prompt_floor: dict = {}

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

    def _call(self, method: str, request, response_cls, timeout: "float | None" = None):
        return self._conn.call(method, request.to_dict(), lambda d: response_cls.from_dict(d), timeout)

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
        skip_derived_index: bool = False,
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

        ``skip_derived_index``: True means the recall indexer will not embed
        or summarise this child's conversations or its descendants'; keyword
        search still works; inherited by the subtree.
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
            skip_derived_index=skip_derived_index,
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
        # A prompt starts new work, so a later settled()/wait() on this child
        # must see that work's turn end, not the idle the child sits in until
        # the prompt is picked up. The floor is read BEFORE the Send so a turn
        # that starts and ends immediately still lands above it.
        floor = (self.get(child_id).latest_ordinal or 0) if mode == "prompt" else None
        message_id = self._call("Send", req, _gen.control_pb.SendResponse).message_id
        if floor is not None:
            self._prompt_floor[child_id] = floor
        return message_id

    def stop(self, child_id: str, *, shutdown_timeout_ms: "int | None" = None, kill_timeout_ms: "int | None" = None):
        """Stop a child: the graceful window first, then the SIGTERM/SIGKILL
        rungs of the kill ladder at the caller's timeouts. Returns the
        KillResponse (exit_code is None for a signalled child). The child's
        own daemon-managed descendants are NOT swept.

        The call carries a per-call read timeout of
        ``(shutdown_timeout_ms or DEFAULT_SHUTDOWN_MS) +
        (kill_timeout_ms or DEFAULT_KILL_MS) + STOP_TIMEOUT_SLACK_MS`` — the
        daemon may legitimately spend the whole graceful window on a child
        that cannot answer the shutdown (a script that never reads its
        Receive stream), then the kill rung, then the reap, before answering;
        the default 30 s read timeout would fire while the stop was still
        working.
        """
        req = _gen.control_pb.KillRequest(
            child_id=child_id,
            shutdown_timeout_ms=shutdown_timeout_ms or 0,
            kill_timeout_ms=kill_timeout_ms or 0,
        )
        timeout_ms = (
            (shutdown_timeout_ms or DEFAULT_SHUTDOWN_MS)
            + (kill_timeout_ms or DEFAULT_KILL_MS)
            + STOP_TIMEOUT_SLACK_MS
        )
        return self._call("Kill", req, _gen.control_pb.KillResponse, timeout_ms / 1000.0)

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
        return self.export_conversation(session)

    def export_conversation(self, conversation_id: str) -> _gen.control_pb.ConversationExportResponse:
        """One conversation's decomposed transcript, by conversation id (the
        ``id`` of a ``conversation_search`` row, or a recall hit's
        ``conversation_id``). A user credential reads any of its owner's
        conversations; a child credential only its own subtree's, and a
        conversation outside it is not_found."""
        req = _gen.control_pb.ConversationExportRequest(conversation_id=conversation_id)
        return self._call("ConversationExport", req, _gen.control_pb.ConversationExportResponse)

    def conversation_search(
        self,
        *,
        text: str = "",
        owner: str = "",
        persona: str = "",
        source: str = "",
        model: str = "",
        status: str = "",
        path: str = "",
        min_tokens: int = 0,
        since_unix: "int | None" = None,
        until_unix: "int | None" = None,
        limit: int = 0,
        closed: str = "",
    ) -> "list":
        """Captured conversations matching the filters, newest first, as
        ``ConversationSummary`` rows (id, name, source, model, status, turns,
        token and cost totals, ``first_message``, and ``closed_at`` when the
        conversation has finished). 0 for ``limit`` is the daemon default (the
        server clamps to a maximum). ``closed`` is ``""`` (any), ``"open"``
        (only running conversations) or ``"closed"`` (only finished ones); any
        other value raises ``invalid_argument``. A user credential searches
        every conversation it may read; a child credential answers from its own
        subtree only."""
        req = _gen.control_pb.ConversationSearchRequest(
            since_unix=since_unix,
            until_unix=until_unix,
            owner=owner,
            persona=persona,
            source=source,
            model=model,
            status=status,
            path=path,
            min_tokens=min_tokens,
            text=text,
            limit=limit,
            closed=closed,
        )
        return self._call("ConversationSearch", req, _gen.control_pb.ConversationSearchResponse).rows

    def status(self) -> _gen.control_pb.StatusResponse:
        """The daemon's status: ``version``, ``started_at`` (unix ms), child
        counts. A user credential only: a child credential is refused
        permission_denied (callers recording a build tolerate that)."""
        return self._call("Status", _gen.control_pb.StatusRequest(), _gen.control_pb.StatusResponse)

    # ── presets ──────────────────────────────────────────────────────────────

    def list_presets(self, prefix: "str | None" = None) -> "list":
        """The caller's live presets (latest row per name), optionally
        narrowed to a name prefix such as ``"review:"``. Readable by a child
        credential; presets resolve against the connection's owner."""
        req = _gen.control_pb.ListPresetsRequest(prefix=prefix or "")
        return self._call("ListPresets", req, _gen.control_pb.ListPresetsResponse).rows

    def get_preset(self, name: str) -> _gen.control_pb.PresetRow:
        """One preset's latest live row; an unknown name raises ConnectError
        not_found."""
        req = _gen.control_pb.GetPresetRequest(name=name)
        return self._call("GetPreset", req, _gen.control_pb.GetPresetResponse).rows[0]

    def put_preset(self, preset) -> _gen.control_pb.PresetRow:
        """Save a new version of a preset (a user credential only). ``preset``
        is a PresetRow or a dict in ``rafiki preset get -j``'s shape: snake_case
        fields, and tools/skills/mcp_servers as plain lists — absent means the
        kind's default (all), ``[]`` means none. Returns the stored row."""
        row = preset if isinstance(preset, _gen.control_pb.PresetRow) else _preset_from_dict(preset)
        req = _gen.control_pb.PutPresetRequest(preset=row)
        return self._call("PutPreset", req, _gen.control_pb.PutPresetResponse).preset

    # ── recall and memories ──────────────────────────────────────────────────

    def recall(
        self,
        query: str,
        *,
        sources: "Iterable[str] | None" = None,
        under: str = "",
        repo: str = "",
        since_unix: int = 0,
        until_unix: int = 0,
        limit: int = 0,
    ) -> "list":
        """Hybrid search over the caller's memories and its owner's captured
        conversations. ``sources`` narrows to some of ``"memory"``,
        ``"summary"``, ``"window"``; 0 for ``since_unix``/``until_unix`` is
        unbounded and 0 for ``limit`` is the daemon default. A child
        credential searches its OWNER's own rows, never other users'. Each
        hit's ``id`` is the key ``recall_context`` expands."""
        req = _gen.control_pb.RecallRequest(
            query=query,
            sources=list(sources or []),
            under=under,
            repo=repo,
            since_unix=since_unix,
            until_unix=until_unix,
            limit=limit,
        )
        return self._call("Recall", req, _gen.control_pb.RecallResponse).hits

    def recall_context(self, hit_id: str, *, before: int = 0, after: int = 0, max_chars: int = 0) -> str:
        """Expand one ``recall`` hit id (``m:``/``s:``/``w:`` prefixed) to its
        text. 0 for ``max_chars`` is uncapped."""
        req = _gen.control_pb.RecallContextRequest(id=hit_id, before=before, after=after, max_chars=max_chars)
        return self._call("RecallContext", req, _gen.control_pb.RecallContextResponse).text

    def memory_get(self, path: str, name: str) -> _gen.control_pb.MemoryRow:
        """One of the caller's saved memories; an unknown path/name raises
        ConnectError not_found."""
        req = _gen.control_pb.GetMemoryRequest(path=path, name=name)
        return self._call("GetMemory", req, _gen.control_pb.GetMemoryResponse).memory

    def memory_tree(self, path: str = "", *, depth: int = 0) -> "list":
        """The caller's memories under ``path`` (dot-separated labels), to
        ``depth`` levels; 0 is unlimited."""
        req = _gen.control_pb.MemoryTreeRequest(path=path, depth=depth)
        return self._call("MemoryTree", req, _gen.control_pb.MemoryTreeResponse).memories

    def memory_put(self, path: str, name: str, body: str, meta=None) -> _gen.control_pb.MemoryRow:
        """Save or replace a memory at ``(path, name)`` under the caller's
        owner. ``meta`` is any JSON-serialisable object (default ``{}``).
        Replacing tombstones the old row; nothing is deleted."""
        req = _gen.control_pb.PutMemoryRequest(
            path=path, name=name, body=body, meta_json="" if meta is None else json.dumps(meta)
        )
        return self._call("PutMemory", req, _gen.control_pb.PutMemoryResponse).memory

    def memory_delete(self, path: str, name: str) -> None:
        """Tombstone one of the caller's memories."""
        req = _gen.control_pb.DeleteMemoryRequest(path=path, name=name)
        self._call("DeleteMemory", req, _gen.control_pb.DeleteMemoryResponse)

    # ── routing policy rows ──────────────────────────────────────────────────

    def list_routes(self) -> "list":
        """The live routing-policy rows, one per model line, ordered by line.
        Readable by any caller — routing defaults name providers, not users.
        A row's line matches model ids equal to it or extending it with "-"
        ("z-ai/glm-5.3" also governs "z-ai/glm-5.3-flash")."""
        req = _gen.control_pb.ListRoutesRequest()
        return self._call("ListRoutes", req, _gen.control_pb.ListRoutesResponse).rows

    def set_route(self, model_line: str, spec: str) -> _gen.control_pb.RouteRow:
        """Write (or replace) the routing spec a model line resolves to, and
        return the stored row. ``spec`` is the bracket grammar of a model
        string, e.g. ``"sort=price,quant=fp8+"`` — the daemon validates it
        (the empty string is the zero spec) and refuses what it cannot parse
        with invalid_argument. Writing needs a user credential or the local
        socket."""
        req = _gen.control_pb.SetRouteRequest(model_line=model_line, spec=spec)
        return self._call("SetRoute", req, _gen.control_pb.SetRouteResponse).row

    def delete_route(self, model_line: str) -> None:
        """Remove the routing-policy row for a model line (appended as a
        tombstone — the history is kept). not_found when the line has no
        live row."""
        req = _gen.control_pb.DeleteRouteRequest(model_line=model_line)
        self._call("DeleteRoute", req, _gen.control_pb.DeleteRouteResponse)

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

        A child this client sent a prompt (``send()``, or ``spawn()`` with a
        prompt) settles only on the turn that prompt started: its idle
        counts once a busy status follows the send, so the idle a fresh
        child reports before its prompt is picked up is not a settle.
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
        lands during a reconnect is seen rather than waited past forever.

        After this client sent the child a prompt, idle only counts once a
        busy status has been seen above the prompt's floor ordinal — the same
        working -> idle transition the daemon's own subagent-settled
        notification fires on. Exited always settles."""
        floor = self._prompt_floor.get(child_id)
        busy = False

        def settles(state: str) -> bool:
            return state == "exited" or (state in SETTLED_STATES and (floor is None or busy))

        def done(state: str, exit_code=None) -> Settle:
            self._prompt_floor.pop(child_id, None)
            return Settle(child_id, state, exit_code)

        summary = self.get(child_id)
        if settles(summary.status):
            yield done(summary.status)
            return
        last_ordinal = floor if floor is not None else (summary.latest_ordinal or 0)
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
                    if state not in SETTLED_STATES:
                        busy = True
                    elif settles(state):
                        yield done(state, exit_code)
                        return
                # Clean stream end without a settle: poll once more, in case
                # the terminal status raced the stream closed.
                summary = self.get(child_id)
                if settles(summary.status):
                    yield done(summary.status)
                    return
                continue
            except StreamEnded:
                # Truncated without an end-of-stream envelope: not
                # caller-visible (StreamEnded's docstring). Treat it as the
                # clean end it can only mean here: poll once for a settle
                # that raced the stream closed, else keep watching.
                summary = self.get(child_id)
                if settles(summary.status):
                    yield done(summary.status)
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


# Fields a stored preset row carries that a put never sets: accepted (so a
# `rafiki preset get -j` document round-trips) and dropped.
_PRESET_READ_ONLY = ("version", "written_by_child", "created_at", "deleted_at")
_PRESET_LISTS = ("tools", "skills", "mcp_servers")


def _preset_from_dict(d: dict) -> _gen.control_pb.PresetRow:
    fields = {f.name for f in dataclasses.fields(_gen.control_pb.PresetRow)}
    unknown = sorted(set(d) - fields)
    if unknown:
        raise ValueError("preset: unknown field(s) %s" % ", ".join(unknown))
    kw = {k: v for k, v in d.items() if k not in _PRESET_READ_ONLY}
    for k in _PRESET_LISTS:
        # Tri-state: absent/None stays None (the kind's default, everything);
        # a list — empty included — is an explicit allowlist.
        if kw.get(k) is not None:
            kw[k] = _gen.control_pb.StringList(items=[str(x) for x in kw[k]])
    return _gen.control_pb.PresetRow(**kw)


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
