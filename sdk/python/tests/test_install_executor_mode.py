# SPDX-License-Identifier: Apache-2.0
"""Unit tests for ``install_executor``'s ``mode`` argument — no daemon involved.

``Client.install_executor`` builds an ``InstallExecutorRequest`` and hands it to
``_call``; these tests capture that request and assert the wire value it
carries. The daemon owns the mode vocabulary and the refusal, so the SDK sends
the string unchanged — ``""`` included, which the daemon reads as ``auto``.
"""

from rafiki import Client
from rafiki._gen import control_pb


def _capture(**kwargs):
    """Run Client.install_executor against a captured _call; return what it saw."""
    response = kwargs.pop("response", None)
    client = Client("unix:///nonexistent/rafiki.sock")
    seen = {}

    def fake_call(method, request, response_cls, timeout=None):
        seen["method"] = method
        seen["request"] = request
        seen["result"] = response if response is not None else response_cls()
        return seen["result"]

    client._call = fake_call
    seen["row"] = client.install_executor("brent@ublk-s2", **kwargs)
    return seen


def test_install_executor_sends_the_mode_unchanged():
    for mode in ["auto", "direct", "tunnel"]:
        seen = _capture(mode=mode)
        assert seen["method"] == "InstallExecutor"
        assert seen["request"].mode == mode
        assert seen["request"].to_dict()["mode"] == mode


def test_install_executor_sends_an_unknown_mode_rather_than_validating():
    # One rule, in the daemon: a value the daemon will refuse travels so the
    # caller reads the daemon's own message instead of an invented one.
    seen = _capture(mode="sideways")
    assert seen["request"].mode == "sideways"


def test_install_executor_mode_defaults_to_empty():
    seen = _capture()
    assert seen["request"].mode == ""
    # Empty is omitted from the wire body, which the daemon reads as auto.
    assert "mode" not in seen["request"].to_dict()


def test_install_executor_keeps_the_other_arguments():
    seen = _capture(via="greyshift", name="ublk-s2", root="/src",
                    labels={"rack": "r1"}, admits="owner=brent", mode="direct")
    request = seen["request"]
    assert request.target == "brent@ublk-s2"
    assert request.via == "greyshift"
    assert request.name == "ublk-s2"
    assert request.root == "/src"
    assert dict(request.labels) == {"rack": "r1"}
    assert request.admits == "owner=brent"
    assert request.mode == "direct"


def test_install_executor_row_carries_install_mode_and_last_launch():
    # The returned dict is the generated row's own to_dict (camelCase, as
    # protojson spells it): the two install facts the daemon reports ride
    # through without an SDK-side wrapper.
    response = control_pb.InstallExecutorResponse(
        executor=control_pb.ExecutorRow(
            id="exec-1",
            machine="ublk-s2",
            installed_via="greyshift",
            installed_target="brent@ublk-s2",
            install_mode="direct",
            last_launch="ok: direct launch started in 1.2s",
        )
    )
    row = _capture(response=response)["row"]
    assert row["id"] == "exec-1"
    assert row["installMode"] == "direct"
    assert row["lastLaunch"] == "ok: direct launch started in 1.2s"
