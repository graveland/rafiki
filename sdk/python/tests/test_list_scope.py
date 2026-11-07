# SPDX-License-Identifier: Apache-2.0
"""Unit tests for the ListChildren ``scope`` wrapper — no daemon involved.

``Client.list`` builds a ``ListChildrenRequest`` and hands it to ``_call``;
these tests capture that request and assert the wire value it carries, plus
the ``ValueError`` for a name the enum does not have. ``in_subtree`` is not
wrapped: it rides the generated ``ChildSummary`` the response decodes.
"""

import pytest

from rafiki import Client
from rafiki._gen import control_pb


def _capture(**kwargs):
    """Run Client.list against a captured _call; return the method and request."""
    response = kwargs.pop("response", None)
    client = Client("unix:///nonexistent/rafiki.sock")
    seen = {}

    def fake_call(method, request, response_cls, timeout=None):
        seen["method"] = method
        seen["request"] = request
        seen["result"] = response if response is not None else response_cls()
        return seen["result"]

    client._call = fake_call
    seen["children"] = client.list(**kwargs)
    return seen


def test_list_scope_none_leaves_the_field_unset():
    seen = _capture()
    assert seen["method"] == "ListChildren"
    assert seen["request"].scope == control_pb.ListScope.LIST_SCOPE_UNSPECIFIED
    # UNSPECIFIED is omitted from the wire body: the daemon reads it as the
    # caller's previous behaviour.
    assert "scope" not in seen["request"].to_dict()


def test_list_scope_subtree_maps_to_the_wire_enum():
    seen = _capture(scope="subtree")
    assert seen["request"].scope == control_pb.ListScope.LIST_SCOPE_SUBTREE
    assert seen["request"].to_dict()["scope"] == control_pb.ListScope.LIST_SCOPE_SUBTREE


def test_list_scope_owner_maps_to_the_wire_enum():
    seen = _capture(scope="owner")
    assert seen["request"].scope == control_pb.ListScope.LIST_SCOPE_OWNER
    assert seen["request"].to_dict()["scope"] == control_pb.ListScope.LIST_SCOPE_OWNER


def test_list_scope_keeps_statuses_and_labels():
    seen = _capture(statuses=["idle"], labels={"team": "infra"}, scope="owner")
    assert list(seen["request"].statuses) == ["idle"]
    assert seen["request"].scope == control_pb.ListScope.LIST_SCOPE_OWNER


def test_list_returns_in_subtree_unwrapped():
    response = control_pb.ListChildrenResponse(
        children=[control_pb.ChildSummary(child_id="c_1", in_subtree=False)]
    )
    seen = _capture(scope="owner", response=response)
    # in_subtree rides the generated ChildSummary: a child outside the caller's
    # subtree comes back as False, not wrapped or dropped.
    assert seen["children"][0].in_subtree is False


@pytest.mark.parametrize("bad", ["everything", "OWNER", "", 2, 0])
def test_list_scope_rejects_an_unknown_value(bad):
    with pytest.raises(ValueError):
        _capture(scope=bad)
