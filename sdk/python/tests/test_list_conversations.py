# SPDX-License-Identifier: Apache-2.0
"""Unit tests for the conversation-listing wrappers, no daemon involved.

``Client.list_conversations`` builds a ``ListConversationsRequest`` and hands
it to ``_call``; these tests capture that request and assert the wire values
it carries, ``iter_conversations``' paging, and the both-or-neither
``ValueError`` a ``get_history`` raises when it names neither or both of a
child id and a conversation id.
"""

import pytest

from rafiki import Client
from rafiki._gen import control_pb


def _capture(**kwargs):
    """Run Client.list_conversations against a captured _call; return the
    method, the request and whatever the fake answered."""
    response = kwargs.pop("response", None)
    client = Client("unix:///nonexistent/rafiki.sock")
    seen = {}

    def fake_call(method, request, response_cls, timeout=None):
        seen["method"] = method
        seen["request"] = request
        seen["result"] = response if response is not None else response_cls()
        return seen["result"]

    client._call = fake_call
    seen["result"] = client.list_conversations(**kwargs)
    return seen


def test_list_conversations_defaults_leave_every_filter_unset():
    seen = _capture()
    assert seen["method"] == "ListConversations"
    req = seen["request"]
    assert req.limit == 0
    assert req.cursor == ""
    assert req.sort == control_pb.ConversationSort.CONVERSATION_SORT_UNSPECIFIED
    assert req.parent_conversation_id == ""
    wire = req.to_dict()
    # UNSPECIFIED is omitted from the wire body: the daemon reads it as RECENT.
    assert "sort" not in wire
    assert "states" not in wire
    assert "labels" not in wire


def test_list_conversations_maps_the_filters_to_the_wire():
    seen = _capture(
        limit=50,
        sort="relevance",
        query="flaky test",
        parent_conversation_id="conv-1",
        states=["open", "no_agent"],
        kinds=["fundi"],
        name_contains="review",
        cwd_contains="/work",
        labels={"team": "infra"},
        response=control_pb.ListConversationsResponse(
            rows=[control_pb.ConversationListing(id="conv-1", child_count=2)],
            next_cursor="cur-1",
        ),
    )
    req = seen["request"]
    assert req.sort == control_pb.ConversationSort.CONVERSATION_SORT_RELEVANCE
    assert req.states == [
        control_pb.ConversationState.CONVERSATION_STATE_OPEN,
        control_pb.ConversationState.CONVERSATION_STATE_NO_AGENT,
    ]
    assert req.to_dict()["states"] == ["CONVERSATION_STATE_OPEN", "CONVERSATION_STATE_NO_AGENT"]
    assert req.to_dict()["labels"] == {"team": "infra"}
    assert req.to_dict()["kinds"] == ["fundi"]
    # The response comes back unwrapped: rows plus the cursor to continue with.
    assert seen["result"].rows[0].id == "conv-1"
    assert seen["result"].next_cursor == "cur-1"


def test_list_conversations_accepts_the_full_enum_names():
    seen = _capture(sort="CONVERSATION_SORT_CREATED", states=["CONVERSATION_STATE_CLOSED"])
    assert seen["request"].sort == control_pb.ConversationSort.CONVERSATION_SORT_CREATED
    assert seen["request"].states == [control_pb.ConversationState.CONVERSATION_STATE_CLOSED]


@pytest.mark.parametrize("bad", ["everything", "RELEVANCY", "", 2])
def test_list_conversations_rejects_an_unknown_sort(bad):
    with pytest.raises(ValueError, match='sort .* is not "recent"'):
        _capture(sort=bad)


@pytest.mark.parametrize("bad", ["everything", "unspecified", "", 1])
def test_list_conversations_rejects_an_unknown_state(bad):
    with pytest.raises(ValueError, match='state .* is not "open"'):
        _capture(states=["open", bad])


def test_iter_conversations_follows_next_cursor_with_the_same_filters():
    client = Client("unix:///nonexistent/rafiki.sock")
    pages = [
        control_pb.ListConversationsResponse(
            rows=[control_pb.ConversationListing(id="a")], next_cursor="cur-1"
        ),
        control_pb.ListConversationsResponse(
            rows=[control_pb.ConversationListing(id="b")], next_cursor="cur-2"
        ),
        control_pb.ListConversationsResponse(),
    ]
    seen = []

    def fake_call(method, request, response_cls, timeout=None):
        seen.append(request)
        return pages.pop(0)

    client._call = fake_call
    rows = list(client.iter_conversations(parent_conversation_id="p1", states=["open"]))
    assert [r.id for r in rows] == ["a", "b"]
    # Each page continues from the previous next_cursor, and the filters ride
    # every request: the daemon refuses a cursor whose sort or parent changed.
    assert [req.cursor for req in seen] == ["", "cur-1", "cur-2"]
    assert all(req.parent_conversation_id == "p1" for req in seen)
    assert all(
        req.states == [control_pb.ConversationState.CONVERSATION_STATE_OPEN] for req in seen
    )


def test_iter_conversations_starts_from_a_given_cursor():
    client = Client("unix:///nonexistent/rafiki.sock")
    pages = [control_pb.ListConversationsResponse()]
    seen = []

    def fake_call(method, request, response_cls, timeout=None):
        seen.append(request)
        return pages.pop(0)

    client._call = fake_call
    assert list(client.iter_conversations(cursor="cur-9")) == []
    assert seen[0].cursor == "cur-9"


def _history_client():
    client = Client("unix:///nonexistent/rafiki.sock")
    seen = {}

    def fake_call(method, request, response_cls, timeout=None):
        seen["method"] = method
        seen["request"] = request
        return control_pb.GetHistoryResponse()

    client._call = fake_call
    return client, seen


@pytest.mark.parametrize(
    "kwargs",
    [
        {},
        {"child_id": None},
        {"conversation_id": None},
        {"child_id": "", "conversation_id": ""},
        {"child_id": "c_1", "conversation_id": "conv-1"},
    ],
)
def test_get_history_requires_exactly_one_target(kwargs):
    client, seen = _history_client()

    def refuse(*args, **kw):
        raise AssertionError("the daemon must not be called")

    client._call = refuse
    with pytest.raises(ValueError, match="exactly one of child_id and conversation_id"):
        client.get_history(**kwargs)
    assert "method" not in seen


def test_get_history_names_the_conversation_directly():
    client, seen = _history_client()
    assert client.get_history(conversation_id="conv-1", limit=10) == []
    assert seen["method"] == "GetHistory"
    assert seen["request"].conversation_id == "conv-1"
    assert seen["request"].child_id == ""
    assert seen["request"].limit == 10


def test_get_history_keeps_the_positional_child_form():
    client, seen = _history_client()
    client.get_history("c_1")
    assert seen["request"].child_id == "c_1"
    assert seen["request"].conversation_id == ""


def test_list_conversations_takes_a_bare_string_for_states_and_kinds():
    """A string is ONE value, not a sequence of its characters.

    Before the wrapper, ``kinds="fundi"`` reached the wire as
    ``["f", "u", "n", "d", "i"]`` and silently filtered to nothing; a bare
    ``states="open"`` at least raised.
    """
    seen = _capture(states="open", kinds="fundi")
    req = seen["request"]
    assert req.states == [control_pb.ConversationState.CONVERSATION_STATE_OPEN]
    assert req.to_dict()["kinds"] == ["fundi"]


def test_iter_conversations_takes_a_bare_string_too():
    client = Client("unix:///nonexistent/rafiki.sock")
    seen = []

    def fake_call(method, request, response_cls, timeout=None):
        seen.append(request)
        return control_pb.ListConversationsResponse()

    client._call = fake_call
    assert list(client.iter_conversations(kinds="fundi", states="open")) == []
    assert seen[0].to_dict()["kinds"] == ["fundi"]
    assert seen[0].states == [control_pb.ConversationState.CONVERSATION_STATE_OPEN]
