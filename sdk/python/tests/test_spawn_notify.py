# SPDX-License-Identifier: Apache-2.0
"""Unit tests for the Spawn ``notify`` wrapper — no daemon involved.

``Client.spawn`` builds a ``SpawnRequest`` and hands it to ``_call``; these
tests capture that request and assert the wire value it carries, plus the
``ValueError`` for a policy the daemon would refuse.
"""

import pytest

from rafiki import Client
from rafiki._gen import control_pb


def _capture(**kwargs):
    client = Client("unix:///nonexistent/rafiki.sock")
    seen = {}

    def fake_call(method, request, response_cls, timeout=None):
        seen["method"] = method
        seen["request"] = request
        return control_pb.SpawnResponse(child_id="c_1")

    client._call = fake_call
    client.spawn(cwd="/work", **kwargs)
    return seen


@pytest.mark.parametrize("policy", ["all", "result"])
def test_spawn_notify_maps_to_request(policy):
    seen = _capture(notify=policy)
    assert seen["method"] == "Spawn"
    assert seen["request"].notify == policy
    assert seen["request"].to_dict()["notify"] == policy


def test_spawn_notify_default_leaves_field_unset():
    seen = _capture()
    assert seen["request"].notify == ""
    assert "notify" not in seen["request"].to_dict()


@pytest.mark.parametrize("bad", ["none", "RESULT", "", 1])
def test_spawn_notify_rejects_unknown_value(bad):
    with pytest.raises(ValueError, match='notify must be "all" or "result"'):
        _capture(notify=bad)
