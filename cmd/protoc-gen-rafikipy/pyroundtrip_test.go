// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

// pythonDriver runs the generated module and asserts the codec's behaviour
// directly; every function named test_* is one behaviour, and any failure
// exits non-zero so the Go test reports the detail.
const pythonDriver = `
import datetime
import sample_pb


def test_timestamp_round_trip():
    got = sample_pb.Sample.from_dict({"ts": "2026-10-05T01:02:03.123456789Z"})
    assert got.ts == datetime.datetime(2026, 10, 5, 1, 2, 3, 123456, tzinfo=datetime.timezone.utc), got.ts
    assert got.to_dict()["ts"] == "2026-10-05T01:02:03.123456Z", got.to_dict()


def test_timestamp_no_fraction_and_offset():
    got = sample_pb.Sample.from_dict({"ts": "2026-10-05T01:02:03+00:00"})
    assert got.ts == datetime.datetime(2026, 10, 5, 1, 2, 3, tzinfo=datetime.timezone.utc), got.ts
    assert got.to_dict()["ts"] == "2026-10-05T01:02:03Z", got.to_dict()
    naive = sample_pb.Sample(ts=datetime.datetime(2026, 10, 5, 1, 2, 3))
    assert naive.to_dict()["ts"] == "2026-10-05T01:02:03Z", naive.to_dict()


def test_duration_round_trip():
    for text, seconds in [("1.5s", 1.5), ("-0.25s", -0.25), ("3s", 3.0)]:
        got = sample_pb.Sample.from_dict({"d": text})
        assert got.d == datetime.timedelta(seconds=seconds), (text, got.d)
        assert got.to_dict()["d"] == text, (text, got.to_dict())
    assert sample_pb.Sample.from_dict({"d": "0.000000001s"}).d == datetime.timedelta(0)


def test_unset_is_none_and_omitted():
    got = sample_pb.Sample.from_dict({})
    assert got.ts is None and got.d is None, got
    assert "ts" not in got.to_dict() and "d" not in got.to_dict(), got.to_dict()
    explicit = sample_pb.Sample.from_dict({"ts": None, "d": None})
    assert explicit.ts is None and explicit.d is None, explicit


def test_malformed_raises_value_error_without_the_content():
    for key, bad in [("ts", "not-a-timestamp"), ("d", "banana")]:
        try:
            sample_pb.Sample.from_dict({key: bad})
        except ValueError as exc:
            text = str(exc)
            assert key in text, text
            assert bad not in text, text
        else:
            raise AssertionError("expected ValueError for malformed %s" % key)


def _run():
    failures = 0
    for name in sorted(list(globals())):
        if name.startswith("test_"):
            try:
                globals()[name]()
                print("ok", name)
            except Exception as exc:
                failures += 1
                print("FAIL", name, repr(exc))
    return failures


if __name__ == "__main__":
    raise SystemExit(_run())
`

// TestPythonRoundTrip executes the generated module under python3 and runs the
// codec assertions above. It skips where python3 is unavailable rather than
// failing, since the generator's output is otherwise covered in-process.
func TestPythonRoundTrip(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	runPythonDriver(t, python)
}

// TestPythonRoundTripPython310 pins the codec against the oldest Python the SDK
// declares support for (pyproject requires-python >=3.9): 3.10's fromisoformat
// accepts only 3 or 6 fractional digits, so a 9-digit nanosecond Timestamp must
// be normalised by the generated code, not by the interpreter.
func TestPythonRoundTripPython310(t *testing.T) {
	python := ""
	for _, candidate := range []string{"python3.10", "/opt/homebrew/bin/python3.10"} {
		if p, err := exec.LookPath(candidate); err == nil {
			python = p
			break
		}
	}
	if python == "" {
		t.Skip("python3.10 not available")
	}
	runPythonDriver(t, python)
}

// runPythonDriver renders the sample proto, writes the module and the driver
// into a fresh temp dir, and executes the driver under the named interpreter.
func runPythonDriver(t *testing.T, python string) {
	t.Helper()
	assert := assert.NewAborting(t)

	resp, err := generate(sampleRequest())
	assert.NoError(err)
	mod := generatedFile(t, resp, "sample_pb.py")

	dir := t.TempDir()
	assert.NoError(os.WriteFile(filepath.Join(dir, "sample_pb.py"), []byte(mod), 0o644))
	assert.NoError(os.WriteFile(filepath.Join(dir, "driver.py"), []byte(pythonDriver), 0o644))

	cmd := exec.Command(python, "driver.py")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	assert.NoError(err, "%s driver failed:\n%s", python, out)
	t.Logf("%s driver output:\n%s", python, out)
}
