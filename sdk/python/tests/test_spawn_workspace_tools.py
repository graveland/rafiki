# SPDX-License-Identifier: Apache-2.0
"""Unit tests for the Spawn ``workspace_tools`` wrapper — no daemon involved.

``Client.spawn`` builds a ``SpawnRequest`` and hands it to ``_call``; these
tests capture that request and assert the wire value it carries, plus the
``ValueError`` for a value the daemon would refuse.
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


@pytest.mark.parametrize("mode", ["native", "rafiki"])
def test_spawn_workspace_tools_maps_to_request(mode):
    seen = _capture(workspace_tools=mode)
    assert seen["method"] == "Spawn"
    assert seen["request"].workspace_tools == mode
    assert seen["request"].to_dict()["workspaceTools"] == mode


def test_spawn_workspace_tools_default_leaves_field_unset():
    seen = _capture()
    assert seen["request"].workspace_tools == ""
    assert "workspaceTools" not in seen["request"].to_dict()


@pytest.mark.parametrize("bad", ["none", "Rafiki", "", 1])
def test_spawn_workspace_tools_rejects_unknown_value(bad):
    with pytest.raises(ValueError, match='workspace_tools must be "native" or "rafiki"'):
        _capture(workspace_tools=bad)