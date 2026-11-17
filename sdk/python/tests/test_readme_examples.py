# SPDX-License-Identifier: Apache-2.0
"""Guards on the README's Python example, run without a daemon.

The example is prose documentation rather than an importable module, so these
are the checks that catch a broken snippet: every block must compile as
Python, and a paged generator must be materialised before it is indexed.
``rows = c.iter_conversations(...)`` followed by ``rows[0]`` raised TypeError
before; indexing either the call or a name bound to it fails here.
"""

import ast
from pathlib import Path

import pytest

README = Path(__file__).resolve().parent.parent / "README.md"


def _python_blocks():
    """Every fenced ```python block in the README, in order."""
    blocks, current = [], None
    for line in README.read_text(encoding="utf-8").splitlines():
        if current is None:
            if line.strip() == "```python":
                current = []
            continue
        if line.strip() == "```":
            blocks.append("\n".join(current))
            current = None
            continue
        current.append(line)
    assert current is None, "an unclosed ```python fence in the README"
    return blocks


def _is_iter_conversations(node):
    """True for a call to ``<anything>.iter_conversations(...)``."""
    return (
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Attribute)
        and node.func.attr == "iter_conversations"
    )


def test_readme_python_example_compiles():
    blocks = _python_blocks()
    assert blocks, "the README carries no ```python example"
    for block in blocks:
        compile(block, str(README), "exec")


def test_readme_does_not_index_an_iter_conversations_generator():
    tree = ast.parse("\n".join(_python_blocks()))

    # The names the example binds an iter_conversations() generator to.
    generators = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Assign) and _is_iter_conversations(node.value):
            generators.update(t.id for t in node.targets if isinstance(t, ast.Name))

    for node in ast.walk(tree):
        if not isinstance(node, ast.Subscript):
            continue
        value = node.value
        direct = _is_iter_conversations(value)
        bound = isinstance(value, ast.Name) and value.id in generators
        if direct or bound:
            what = "the iter_conversations() call" if direct else f"`{value.id}`"
            pytest.fail(
                f"README indexes {what} at line {node.lineno}, which is a generator: "
                "materialise it (list(...)) or iterate it before indexing"
            )
