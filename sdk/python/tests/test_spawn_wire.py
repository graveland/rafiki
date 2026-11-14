# SPDX-License-Identifier: Apache-2.0
"""Unit tests for the Spawn wire's ``home_executor`` and ``warnings`` — no
daemon involved.

``Client.spawn`` builds a ``SpawnRequest`` and hands it to ``_call``; these
tests capture that request and assert the wire value it carries, then drive a
``SpawnResponse`` carrying warnings and assert each one is emitted as a
``RafikiSpawnWarning`` — the category a caller filters on, not the message.
"""

import warnings

import pytest

from rafiki import Client, RafikiSpawnWarning
from rafiki._gen import control_pb


def _capture(response=None, **kwargs):
    client = Client("unix:///nonexistent/rafiki.sock")
    seen = {}

    def fake_call(method, request, response_cls, timeout=None):
        seen["method"] = method
        seen["request"] = request
        if response is not None:
            return response
        return control_pb.SpawnResponse(child_id="c_1")

    client._call = fake_call
    seen["child_id"] = client.spawn(cwd="/work", **kwargs)
    return seen


def test_spawn_home_executor_maps_to_request():
    seen = _capture(home_executor="greyshift")
    assert seen["method"] == "Spawn"
    assert seen["request"].home_executor == "greyshift"
    assert seen["request"].to_dict()["homeExecutor"] == "greyshift"


def test_spawn_home_executor_default_leaves_field_unset():
    seen = _capture()
    assert seen["request"].home_executor == ""
    assert "homeExecutor" not in seen["request"].to_dict()


def test_spawn_emits_one_rafiki_spawn_warning_per_warning():
    response = control_pb.SpawnResponse(child_id="c_1", warnings=["w1", "w2"])
    with pytest.warns(RafikiSpawnWarning) as record:
        seen = _capture(response)

    assert seen["child_id"] == "c_1"
    assert len(record) == 2
    assert all(issubclass(w.category, RafikiSpawnWarning) for w in record)
    assert [str(w.message) for w in record] == ["w1", "w2"]


def test_spawn_without_warnings_emits_none():
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        _capture()

    assert [w for w in caught if issubclass(w.category, RafikiSpawnWarning)] == []
