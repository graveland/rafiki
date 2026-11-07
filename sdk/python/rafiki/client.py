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
import datetime
import json
import os
import shutil
import subprocess
import time
from typing import Iterable, Literal

from . import _gen
from .connect import ConnectClient
from .errors import (
    CODE_DEADLINE_EXCEEDED,
    CODE_NOT_FOUND,
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
# durOrDefault(shutdownTimeout, defaultKillShutdown) and
# durOrDefault(killTimeout, defaultKillTimeout); pinned by
# TestKillTimeoutDefaults in cmd/rafikid). stop() mirrors them so its per-call
# read timeout covers exactly the work the daemon may do before answering.
DEFAULT_SHUTDOWN = datetime.timedelta(seconds=2)
DEFAULT_KILL = datetime.timedelta(seconds=30)

# Slack on top of the two windows above: the daemon answers only after the
# ladder completes AND the exit is persisted (waitForChildRemoval), so the
# read timeout is the two windows plus headroom, not the windows exactly.
STOP_TIMEOUT_SLACK = datetime.timedelta(seconds=30)

# The states that mean "settled": a fundi child sits idle between turns
# (agent_settled is pi's true-idle event; the daemon maps it to idle), and
# exited is terminal. Everything else (spawning, streaming, running,
# tool_running, compacting, blocked_ui, shutting_down) is still working --
# "running" is a script child's steady state between spawn and exit.
SETTLED_STATES = ("idle", "exited")

# CLOSED_STATE is the settle a CLOSE reports, and it is deliberately not a
# protocol.Status: the child has left the daemon's store, so it has no status
# left to report and can never be resumed. Reporting it as "exited" would be a
# lie in the other direction -- Settle.exit_code documents None as "signalled",
# and a closed child's exit code is simply gone.
CLOSED_STATE = "closed"

# The durable event types a settle arrives as. A fundi or claude child
# publishes agent_status (idle is the settle; exited the terminal form); a
# SCRIPT child has no turns — its whole life is one run — so its settle is
# the child_exited event alone. Watching both is the kind-agnostic way.
# child_closed is the third: a child closed (finalized) by anyone — another
# client, the CLI, the daemon's grace reaper — publishes no further event, so
# without it a wait() on that child would block until its timeout.
SETTLE_EVENT_TYPES = ["agent_status", "child_exited", "child_closed"]


def _latest_ordinal(summary) -> int:
    """A child's latest event-log ordinal as a replay cursor: -1 when it has
    no events. 0 is a real ordinal, so an unset latest_ordinal must never
    read as 0 — a cursor of 0 would skip the child's first event."""
    return -1 if summary.latest_ordinal is None else summary.latest_ordinal


def _mode_wire(mode: str) -> str:
    """Accept "prompt"/"steer"/"abort" (any case, with - or _) and return the
    proto enum name."""
    name = mode.strip().replace("-", "_").upper()
    if not name.startswith("SEND_MODE_"):
        name = "SEND_MODE_" + name
    return name


# ListChildren's scope names, mapped to the wire enum. "owner" widens a CHILD
# credential's list from its own subtree to every agent its owner owns; a user,
# admin or unix-socket caller has no position in the tree, so both values are
# no-ops there (their root is already everything they may see). None leaves the
# field unset (LIST_SCOPE_UNSPECIFIED), which resolves to the caller's default.
_LIST_SCOPE_WIRE = {
    "subtree": _gen.control_pb.ListScope.LIST_SCOPE_SUBTREE,
    "owner": _gen.control_pb.ListScope.LIST_SCOPE_OWNER,
}


def _list_scope_wire(scope: "Literal['subtree', 'owner'] | None") -> str:
    """Map "subtree"/"owner"/None to the ListScope enum name; anything else is
    a ValueError rather than a silently ignored argument."""
    if scope is None:
        return _gen.control_pb.ListScope.LIST_SCOPE_UNSPECIFIED
    try:
        return _LIST_SCOPE_WIRE[scope]
    except (KeyError, TypeError):
        raise ValueError('scope %r is not "subtree" or "owner"' % (scope,)) from None


def _tag_scope_wire(scope) -> str:
    """Accept a TagScope number (0-3) or name ("lineage", "TAG_SCOPE_TREE")
    and return the proto enum name. 0 is TAG_SCOPE_UNSPECIFIED, the wire's
    unset — lineage at create time — which the daemon never stores."""
    if isinstance(scope, int) and not isinstance(scope, bool):
        names = {
            0: "TAG_SCOPE_UNSPECIFIED",
            1: "TAG_SCOPE_LINEAGE",
            2: "TAG_SCOPE_TREE",
            3: "TAG_SCOPE_OWNER",
        }
        if scope not in names:
            raise ValueError("scope %r is not a TagScope (0-3)" % (scope,))
        return names[scope]
    name = str(scope).strip().replace("-", "_").upper()
    if not name.startswith("TAG_SCOPE_"):
        name = "TAG_SCOPE_" + name
    return name


def _labels_wire(labels: dict | None) -> dict:
    if labels is None:
        return {}
    return {str(k): str(v) for k, v in labels.items()}


def _as_timedelta(value: "datetime.timedelta | float | int | None") -> "datetime.timedelta | None":
    """Coerce a ``timedelta`` or a number of seconds to a ``timedelta``; None
    stays None (the wire's unset, which the daemon reads as its own default)."""
    if value is None:
        return None
    if isinstance(value, datetime.timedelta):
        return value
    return datetime.timedelta(seconds=float(value))


class Settle:
    """One child's settle: the state it landed in (see SETTLED_STATES), or
    ``"closed"`` when the child was closed (finalized) during the watch — it
    left the daemon's store, so it can never be resumed and no further event
    will ever arrive for it. ``exit_code`` is set only when the settle arrived
    as child_exited (None when the child was signalled, and always None for a
    close, whose exit code is gone with the row)."""

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
        fork_from: "dict | _gen.control_pb.SpawnRequest.ForkFrom | None" = None,
        from_tag: "str | None" = None,
        notify: "Literal['all', 'result'] | None" = None,
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

        ``fork_from`` starts the child from a point in another child's
        history — a dict ``{"child_id": ..., "cursor": ...}`` or a generated
        ``SpawnRequest.ForkFrom``; ``cursor`` is ``"m:<ordinal>"`` and empty
        means the source's last completed turn. ``from_tag`` names a tag
        instead, resolved once here, nearest scope first. The two are
        mutually exclusive, and only ``kind="fundi"`` can be forked to. A
        child may fork only what it could already read, and a foreign or
        unknown tag answers the same permission_denied as one it cannot see.

        ``notify`` is the child's wake policy for its spawner, fixed at spawn
        and not inherited. ``None`` leaves it unset, which the daemon reads as
        ``"all"``: every settle and heartbeat wakes the spawner. ``"result"``
        wakes it only for a recorded result (``agent_result``), a pending
        question, a turn error, the child's own budget cap, or the child
        exiting or failing; plain turn ends and heartbeats are dropped. It
        matters to anything with a parent link or an MCP session, since that
        is who is woken. Any other value raises ``ValueError``.
        """
        if notify is not None and notify not in ("all", "result"):
            raise ValueError('notify must be "all" or "result"')
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
        if fork_from is not None:
            req.fork_from = (
                fork_from
                if isinstance(fork_from, _gen.control_pb.SpawnRequest.ForkFrom)
                else _gen.control_pb.SpawnRequest.ForkFrom(**fork_from)
            )
        if from_tag:
            req.from_tag = from_tag
        if notify is not None:
            req.notify = notify
        resp = self._call("Spawn", req, _gen.control_pb.SpawnResponse)
        child_id = resp.child_id
        if prompt:
            self.send(child_id, prompt)
        return child_id

    def get(self, child_id: str) -> _gen.control_pb.ChildSummary:
        """One child's summary: status, cost, labels, latest_ordinal, and —
        for a script child that called result() — its stored result JSON.
        ``result`` is a script-child field only: it is empty for fundi and
        claude children, whose answer is the last assistant turn of
        :meth:`export`."""
        return self._call("GetChild", _gen.control_pb.GetChildRequest(child_id=child_id), _gen.control_pb.GetChildResponse).child

    def list(
        self,
        *,
        statuses: "Iterable[str] | None" = None,
        labels: "dict | None" = None,
        scope: "Literal['subtree', 'owner'] | None" = None,
    ):
        """The caller's children — a user's whole tree, a child's subtree —
        optionally narrowed to statuses and, client-side, to children whose
        labels carry every given pair (ListChildrenRequest filters by status
        only; labels are filtered here, on the summaries it returns).

        ``scope`` changes only a CHILD credential's list: ``"subtree"`` is its
        own subtree (the default when unset), ``"owner"`` every agent its
        owner owns — which may include agents outside that subtree. For a
        user, admin or unix-socket caller both values are no-ops: they have no
        position in the tree, so their root is everything they may see. The
        summaries themselves carry ``in_subtree`` (set on a child credential's
        ``"owner"`` list, unset everywhere else) unchanged."""
        req = _gen.control_pb.ListChildrenRequest(
            statuses=list(statuses) if statuses else [],
            scope=_list_scope_wire(scope),
        )
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
        floor = _latest_ordinal(self.get(child_id)) if mode == "prompt" else None
        message_id = self._call("Send", req, _gen.control_pb.SendResponse).message_id
        if floor is not None:
            self._prompt_floor[child_id] = floor
        return message_id

    def get_history(
        self,
        child_id: str,
        *,
        limit: "int | None" = None,
        before: "datetime.datetime | None" = None,
        after_ordinal: "int | None" = None,
    ) -> "list":
        """One page of a child's transcript as durable events, oldest first,
        each carrying ``ts`` (its row's created_at). Page backwards with
        ``limit`` for the newest N, then again with ``before`` set to the
        oldest ``ts`` held; an empty page is the start. A page never splits
        events that share a ``ts``, so it can hold more than ``limit``.
        ``before`` is an aware ``datetime``. Ordinals here are
        conversation_message ordinals, never the event feed's."""
        req = _gen.control_pb.GetHistoryRequest(
            child_id=child_id, limit=limit, before=before, after_ordinal=after_ordinal
        )
        return list(self._call("GetHistory", req, _gen.control_pb.GetHistoryResponse).events)

    def stop(self, child_id: str, *, shutdown_timeout: "datetime.timedelta | float | None" = None, kill_timeout: "datetime.timedelta | float | None" = None):
        """Stop a child: the graceful window first, then the SIGTERM/SIGKILL
        rungs of the kill ladder at the caller's timeouts. Returns the
        KillResponse (exit_code is None for a signalled child). The child's
        own daemon-managed descendants are NOT swept.

        ``shutdown_timeout`` and ``kill_timeout`` are each a ``timedelta`` or a
        number of seconds; None (or zero) leaves the daemon's own default —
        2 s graceful, then 30 s to kill. The call carries a per-call read
        timeout of ``(shutdown_timeout or DEFAULT_SHUTDOWN) +
        (kill_timeout or DEFAULT_KILL) + STOP_TIMEOUT_SLACK`` — the daemon may
        legitimately spend the whole graceful window on a child that cannot
        answer the shutdown (a script that never reads its Receive stream),
        then the kill rung, then the reap, before answering; the default 30 s
        read timeout would fire while the stop was still working.
        """
        shutdown = _as_timedelta(shutdown_timeout)
        kill = _as_timedelta(kill_timeout)
        req = _gen.control_pb.KillRequest(
            child_id=child_id,
            shutdown_timeout=shutdown or None,
            kill_timeout=kill or None,
        )
        timeout_s = (
            (shutdown or DEFAULT_SHUTDOWN)
            + (kill or DEFAULT_KILL)
            + STOP_TIMEOUT_SLACK
        ).total_seconds()
        return self._call("Kill", req, _gen.control_pb.KillResponse, timeout_s)

    def export(self, child_id: str) -> _gen.control_pb.ConversationExportResponse:
        """One child's decomposed transcript, by child id. Works for every
        child that has a captured conversation, live or exited — fundi and
        claude children alike (the daemon resolves the child id to its
        conversation; a claude child's session id is claude's own and names no
        conversation, so it is never used here). Only a child's main thread is
        returned. A child that has not yet had a model request captured
        answers not_found with a message saying to retry. Script children make
        no model requests and have no conversation."""
        return self.export_conversation(child_id)

    def export_conversation(self, conversation_id: str) -> _gen.control_pb.ConversationExportResponse:
        """One conversation's decomposed transcript, by conversation id (the
        ``id`` of a ``conversation_search`` row, or a recall hit's
        ``conversation_id``) or by child id (see :meth:`export`). A user
        credential reads any of its owner's conversations; a child credential
        reads its owner's whole corpus — its own, its subtree's and other
        agents' alike — or only its subtree when its lineage has no owner. A
        conversation outside that scope is not_found."""
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
        since: "datetime.datetime | None" = None,
        until: "datetime.datetime | None" = None,
        limit: int = 0,
    ) -> "list":
        """Captured conversations matching the filters, newest first, as
        ``ConversationSummary`` rows (id, name, source, model, status, turns,
        token and cost totals, ``first_message``, ``created_at`` and
        ``closed_at`` as tz-aware UTC ``datetime``s — ``closed_at`` is ``None``
        while the conversation is open). 0 for ``limit`` is the daemon default
        (the server clamps to a maximum). ``since``/``until`` bound turn
        activity (``None`` is unbounded). A user credential searches every
        conversation it may read; a child credential reads its owner's whole
        corpus — its own, its subtree's and other agents' alike — or only its
        subtree when its lineage has no owner."""
        req = _gen.control_pb.ConversationSearchRequest(
            since=since,
            until=until,
            owner=owner,
            persona=persona,
            source=source,
            model=model,
            status=status,
            path=path,
            min_tokens=min_tokens,
            text=text,
            limit=limit,
        )
        return self._call("ConversationSearch", req, _gen.control_pb.ConversationSearchResponse).rows

    def status(self) -> _gen.control_pb.StatusResponse:
        """The daemon's status: ``version``, ``started_at`` (a tz-aware UTC
        ``datetime``), child counts. A user credential only: a child credential
        is refused permission_denied (callers recording a build tolerate that)."""
        return self._call("Status", _gen.control_pb.StatusRequest(), _gen.control_pb.StatusResponse)

    # ── tags and forks ───────────────────────────────────────────────────────

    def tag(
        self,
        name: str = "",
        *,
        child_id: str = "",
        cursor: str = "",
        scope=0,
        note: str = "",
        tag_id: str = "",
        expect_node_id: str = "",
        replace: bool = False,
    ) -> _gen.control_pb.TagRow:
        """Create a tag on a node, or move an existing one, and return the
        stored ``TagRow``. A tag is a named, scoped handle to a node: naming
        it is the grant to fork from it, and ``scope`` says who holds it —
        ``"lineage"`` (the default) for the source's subtree and its
        ancestors, ``"tree"`` for everything under the tagger's top-level
        ancestor, ``"owner"`` for every agent of the owner. Anyone who can
        read a node may tag it; only the tag's creator, that creator's
        ancestors or a user credential may move or untag it.

        With ``tag_id`` empty this CREATES: ``name`` (1-64 of
        ``[a-z0-9._-]``, starting with a letter or digit) names the node
        ``child_id`` + ``cursor`` points at (``"m:<ordinal>"``, or
        ``"m:<ordinal>.<n>"``; empty means the last completed turn). A name
        already live in that scope raises already_exists; ``replace=True``
        tombstones the holder first, and only a user credential may do that
        (a child credential is permission_denied).

        With ``tag_id`` set this MOVES it: ``expect_node_id`` is required and
        must be the node the tag currently points at (compare-and-swap),
        while ``name``, ``scope`` and ``replace`` must be unset. A stale
        expectation raises failed_precondition — re-read the tag and retry.
        Every move appends a row, so a tag's history is never overwritten.

        Tags are the connection owner's alone: another owner's tag id answers
        the same not_found an unknown one does, so no caller can probe which
        tags exist."""
        req = _gen.control_pb.TagRequest(
            name=name,
            scope=_tag_scope_wire(scope),
            child_id=child_id,
            cursor=cursor,
            note=note,
            tag_id=tag_id,
            expect_node_id=expect_node_id,
            replace=replace,
        )
        return self._call("Tag", req, _gen.control_pb.TagResponse).tag

    def untag(self, tag_id: str) -> None:
        """Tombstone a tag's live row — the tag is revoked; nothing it
        pointed at is deleted. Only the tag's creator, that creator's
        ancestors or a user credential may untag it (permission_denied).
        An unknown tag id and another owner's look the same: not_found."""
        req = _gen.control_pb.UntagRequest(tag_id=tag_id)
        self._call("Untag", req, _gen.control_pb.UntagResponse)

    def list_tags(self, name_prefix: str = "", limit: int = 0, tag_id: str = "") -> "list":
        """The tags visible to the caller, nearest scope first (lineage, then
        tree, then owner), optionally narrowed to a name prefix; ``limit`` 0
        means the daemon's default of 50 (anything above 500 is clamped).
        With ``tag_id`` set the answer is that one tag's whole history, oldest
        first. Only the connection owner's tags are ever answered, so an
        unknown tag id and a foreign (another owner's) one look the same:
        not_found."""
        req = _gen.control_pb.ListTagsRequest(name_prefix=name_prefix, limit=limit, tag_id=tag_id)
        return self._call("ListTags", req, _gen.control_pb.ListTagsResponse).rows

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
        since: "datetime.datetime | None" = None,
        until: "datetime.datetime | None" = None,
        limit: int = 0,
    ) -> "list":
        """Hybrid search over the caller's memories and its owner's captured
        conversations. ``sources`` narrows to some of ``"memory"``,
        ``"summary"``, ``"window"``; ``since``/``until`` bound activity by an
        aware ``datetime`` (``None`` = unbounded) and 0 for ``limit`` is the
        daemon default. A child credential searches its OWNER's own rows, never
        other users'. Each hit's ``id`` is the key ``recall_context`` expands."""
        req = _gen.control_pb.RecallRequest(
            query=query,
            sources=list(sources or []),
            under=under,
            repo=repo,
            since=since,
            until=until,
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

    # ── model catalog ────────────────────────────────────────────────────────

    def list_models(self, provider: str = "", kind: str = "") -> "list":
        """The daemon's model catalog as ``ModelRow``s (``id`` is the
        ``provider/model`` string ``spawn`` takes). ``context_window`` and
        ``max_completion_tokens`` are optional fields: test with
        ``HasField("context_window")`` — absent means the daemon does not
        know, not zero. ``provider`` narrows to one provider; ``kind`` scopes
        which models that child kind can actually use ("" is the fundi
        default; a "claude" child resolves only Anthropic ids)."""
        req = _gen.control_pb.ListModelsRequest(provider=provider, kind=kind)
        return self._call("ListModels", req, _gen.control_pb.ListModelsResponse).models

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

    # ── model aliases ────────────────────────────────────────────────────────

    def list_model_aliases(self) -> "list":
        """The calling connection owner's model aliases as ``ModelAliasRow``s,
        ordered by name. An alias is a slash-free model name the owner (and
        the owner's children) resolves through: ``ds`` means whatever
        ``put_model_alias`` stored for it. Strictly per owner — no daemon-wide
        or fallback layer — and readable by any caller for its own owner."""
        req = _gen.control_pb.ListModelAliasesRequest()
        return self._call("ListModelAliases", req, _gen.control_pb.ListModelAliasesResponse).rows

    def put_model_alias(self, name: str, target: str) -> _gen.control_pb.ModelAliasRow:
        """Create or replace one alias for the calling connection's owner, and
        return the stored row (``created_at`` survives a replace). ``name`` is
        ``^[a-z0-9][a-z0-9._-]*$`` — no ``/``, no ``[``; ``target`` is a model
        string whose base names a provider (``openrouter/~deepseek/...``), so
        an alias never chains to another alias. Neither is resolved against
        the catalog at put; the daemon refuses a bad name or target with
        invalid_argument. Writing needs a user credential or the local socket:
        a child credential is permission_denied."""
        req = _gen.control_pb.PutModelAliasRequest(name=name, target=target)
        return self._call("PutModelAlias", req, _gen.control_pb.PutModelAliasResponse).row

    def delete_model_alias(self, name: str) -> None:
        """Remove one of the calling owner's aliases. not_found when the owner
        has no alias by that name; needs a user credential or the local
        socket, like ``put_model_alias``."""
        req = _gen.control_pb.DeleteModelAliasRequest(name=name)
        self._call("DeleteModelAlias", req, _gen.control_pb.DeleteModelAliasResponse)

    # ── provider login (per-user provider credentials) ───────────────────────

    def begin_provider_login(self, provider: str) -> _gen.control_pb.BeginProviderLoginResponse:
        """Start an OAuth login for one provider. ``provider`` is the NAME of a
        providers.toml entry whose KIND authenticates by login (e.g.
        ``openai-responses-chatgpt``). Returns the ``login_id`` to complete
        with, the ``authorize_url`` to open in a browser, and where the
        provider sends the browser back: listen on ``redirect_port`` at
        ``callback_path`` on every host in ``redirect_hosts`` (the redirect URI
        is fixed by the provider's registration) or accept a pasted redirect
        URL. The credential is stored against the calling user and is never
        returned: the daemon holds the tokens. Needs a user credential:
        ``failed_precondition`` when the profile has none, ``permission_denied``
        for a child's credential. A provider that is unknown, or whose entry
        does not login-authenticate, is ``invalid_argument``."""
        req = _gen.control_pb.BeginProviderLoginRequest(provider=provider)
        return self._call("BeginProviderLogin", req, _gen.control_pb.BeginProviderLoginResponse)

    def complete_provider_login(
        self, login_id: str, callback_query: str
    ) -> _gen.control_pb.CompleteProviderLoginResponse:
        """Finish the login ``begin_provider_login`` started, handing back the
        raw query string the provider redirected to loopback with
        (``callback_query``, leading ``?`` optional). Returns the ``provider``
        and the authenticated ``account_id``; the tokens stay in the daemon.
        ``not_found`` for a login id that is unknown, expired, or was begun by
        another user: one identical answer for all three, so no caller can
        probe which logins exist."""
        req = _gen.control_pb.CompleteProviderLoginRequest(login_id=login_id, callback_query=callback_query)
        return self._call("CompleteProviderLogin", req, _gen.control_pb.CompleteProviderLoginResponse)

    def delete_provider_credential(self, provider: str) -> None:
        """Delete the calling user's stored credential for one provider, so
        that provider's ``login``-authenticated kind stops resolving for this
        user's children, whose next turn then fails with the re-login error.
        ``not_found`` when the user has no credential stored for that provider
        name, whether or not the provider is configured: the delete never
        resolves the provider entry, so a login stored under a name removed
        from providers.toml can still be cleared."""
        req = _gen.control_pb.DeleteProviderCredentialRequest(provider=provider)
        self._call("DeleteProviderCredential", req, _gen.control_pb.DeleteProviderCredentialResponse)

    # ── MCP OAuth (per-user credentials for remote MCP servers) ──────────────

    def begin_mcp_login(
        self,
        config_path: str,
        server: str,
        scopes: "Iterable[str] | None" = None,
    ) -> _gen.control_pb.BeginMcpLoginResponse:
        """Start an OAuth login for one direct-url entry of an MCP config file.
        ``config_path`` is an ABSOLUTE path ON THE DAEMON HOST — the daemon
        reads that exact file and that exact ``mcpServers`` entry, so a client
        on another machine cannot upload its project config here. ``scopes``
        overrides what discovery would request; empty (the default) asks for
        the challenged scopes, else the resource's advertised ones.

        Returns the ``login_id`` to complete with, the ``authorize_url`` to open
        in a browser, and where the server sends the browser back: listen on
        ``redirect_port`` at ``callback_path`` on every host in
        ``redirect_hosts``, or accept a pasted redirect URL. The grant is stored
        against the CALLING USER and is never returned: no token, no client
        secret and no callback code crosses this seam.

        Needs a user credential: ``failed_precondition`` when the profile has
        none, ``permission_denied`` for a child's credential. A path the daemon
        cannot read, an entry it does not declare, and an entry that cannot log
        in here (a stdio server, a via_executor server, an entry with its own
        Authorization header) are all ``invalid_argument``. This call performs
        no network work until discovery starts, and it never prompts during a
        spawn — an explicit login is the only way a grant exists."""
        req = _gen.control_pb.BeginMcpLoginRequest(
            config_path=config_path, server=server, scopes=list(scopes) if scopes else []
        )
        return self._call("BeginMcpLogin", req, _gen.control_pb.BeginMcpLoginResponse)

    def complete_mcp_login(
        self, login_id: str, callback_query: str
    ) -> _gen.control_pb.CompleteMcpLoginResponse:
        """Finish one consent round of the login :meth:`begin_mcp_login`
        started, handing back the raw query string the server redirected to
        loopback with (``callback_query``, WITHOUT the leading ``?``: the daemon
        parses it as a query string, so a ``"?"`` would become part of the first
        key and the callback would be refused). A callback code is single use:
        replaying one is ``invalid_argument``.

        EXACTLY ONE OUTCOME. When ``continuation`` is set the login is not
        finished: nothing is stored yet, the same ``login_id`` carries on, and
        the continuation is another Begin-shaped descriptor (authorize URL, port,
        path, hosts) to drive another round. Only a response with no
        continuation is a success, and then ``server``, ``resource``,
        ``account_id`` (empty unless a validated source names one) and the sorted
        tool NAMES it serves are set.

        ``not_found`` for a login id that is unknown, expired, or was begun by
        another user: one identical answer for all three, so no caller can probe
        which logins exist. A refused consent round, a replayed callback and a
        callback that does not match the login's state/issuer are all
        ``invalid_argument``: the login is over and must be started again."""
        req = _gen.control_pb.CompleteMcpLoginRequest(login_id=login_id, callback_query=callback_query)
        return self._call("CompleteMcpLogin", req, _gen.control_pb.CompleteMcpLoginResponse)

    def delete_mcp_credential(
        self, config_path: str = "", server: str = "", resource: str = ""
    ) -> None:
        """Delete one of the calling user's stored MCP grants. Two exclusive
        forms: ``config_path`` + ``server`` resolves the configured direct-url
        entry and invalidates its endpoint (which also fences a first login
        still in flight), or ``resource`` names the canonical resource URL
        directly and touches no config at all — that is how a grant is removed
        once its project file is gone. Passing both, or neither, is
        ``invalid_argument``.

        A grant the caller does not have is ``not_found``. The local
        invalidation is what logout means: an upstream revocation that fails
        does not restore the grant, and the next authenticated MCP request from
        the user's children fails with the re-login hint."""
        req = _gen.control_pb.DeleteMcpCredentialRequest(
            config_path=config_path, server=server, resource=resource
        )
        self._call("DeleteMcpCredential", req, _gen.control_pb.DeleteMcpCredentialResponse)

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

        A child CLOSED during the watch settles as ``"closed"``: closing
        finalizes a child (another client's close, ``rafiki forget``, the
        daemon's grace reaper), it leaves the daemon's store, and nothing more
        is ever published for it — so this is the terminal outcome a wait must
        not sit past. A child that had ALREADY left the store when the watch
        began is not settled but raised (``not_found``), like any other read of
        a child that no longer exists.

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
        final state (``"idle"``, ``"exited"``, or ``"closed"`` for a child
        closed during the wait). ``timeout`` bounds the WHOLE wait (seconds)."""
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

        def poll_after_a_stream() -> "object | None":
            """The child's summary, or None when it has left the store.

            Only used AFTER the first poll succeeded, so NotFound here can only
            mean a close landed during the watch: that is a terminal settle, not
            an error. The first poll still raises, because a child this client
            never had (a typo, someone else's id) is not a close."""
            try:
                return self.get(child_id)
            except ConnectError as exc:
                if exc.code == CODE_NOT_FOUND:
                    return None
                raise

        summary = self.get(child_id)
        if settles(summary.status):
            yield done(summary.status)
            return
        last_ordinal = floor if floor is not None else _latest_ordinal(summary)
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

        def request() -> dict:
            # Built per attempt from the last ordinal seen, so a re-open after
            # a mid-stream unavailability (a daemon restarting) replays only
            # what this watch has not had.
            return _gen.control_pb.StreamEventsRequest(
                subject=_gen.control_pb.EventSubject(child=child_id),
                tier="EVENT_TIER_DURABLE",
                types=SETTLE_EVENT_TYPES,
                cursor=_gen.control_pb.EventCursor(ordinals={child_id: seen}),
            ).to_dict()

        while True:
            try:
                for event in self._conn.stream(
                    "StreamEvents", request, read_timeout=remaining(), on_resume=lambda: None
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
                    elif ev.child_closed is not None:
                        # Closed, not exited: the child left the daemon's
                        # store, so nothing more will ever be published for it.
                        # This is terminal and unrecoverable -- the child can
                        # never be resumed -- and without it the watch would sit
                        # here until its timeout.
                        yield done(CLOSED_STATE)
                        return
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
                # the terminal status -- or a close -- raced the stream closed.
                summary = poll_after_a_stream()
                if summary is None:
                    yield done(CLOSED_STATE)
                    return
                if settles(summary.status):
                    yield done(summary.status)
                    return
                continue
            except StreamEnded:
                # Truncated without an end-of-stream envelope: not
                # caller-visible (StreamEnded's docstring). Treat it as the
                # clean end it can only mean here: poll once for a settle
                # that raced the stream closed, else keep watching.
                summary = poll_after_a_stream()
                if summary is None:
                    yield done(CLOSED_STATE)
                    return
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
