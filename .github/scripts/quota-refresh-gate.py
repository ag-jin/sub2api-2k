#!/usr/bin/env python3
"""Validate ``go test -json`` evidence for the OpenCode Go quota-refresh gate.

The workflow ``.github/workflows/opencode-quota-refresh-gates.yml`` runs the same
real-PostgreSQL regression twice:

  * ``current``              - every subtest must pass;
  * ``literal218repository`` - only
                               ``backend/internal/repository/account_repo_opencode_go_usage.go``
                               is rewound to the v0.1.218 baseline; the regression
                               test, helpers and this validator stay current.
                               Both ``policyAccountsOnly`` subtests must fail
                               independently with ``SQLSTATE=42703`` and
                               ``column "credentials" does not exist``.

Compile/setup/panic/skip evidence is never accepted as behavior red, and a single
42703 anywhere in the log is not enough: the SQLSTATE must be attributed to each
subtest through the ``Test`` field of the ``-json`` events.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

TEST_NAME = "TestListDueOpenCodeGoUsageAccountsPolicyContract"
SUBTESTS = (
    TEST_NAME + "/policyAccountsOnly=false",
    TEST_NAME + "/policyAccountsOnly=true",
)

EXPECTED_SQLSTATE = "SQLSTATE=42703"
EXPECTED_MESSAGE = 'column "credentials" does not exist'

COMPILE_OR_SETUP_RE = re.compile(
    r"build failed|cannot find package|undefined:|import cycle|syntax error|"
    r"panic:|setup failed|TestMain",
    re.IGNORECASE,
)
NON_SQL_RE = re.compile(r"non-sql error", re.IGNORECASE)
TRANSACTION_ABORTED_RE = re.compile(
    r"SQLSTATE=25P02|current transaction is aborted", re.IGNORECASE
)
HARNESS_RE = re.compile(r"integration harness: database=sub2api_test\b")
VERSION_RE = re.compile(r"server_version_num=\d+")


def fail(prefix: str, message: str) -> None:
    print(f"::error::{prefix}: {message}", file=sys.stderr)
    raise SystemExit(1)


def load_events(path: Path) -> list[dict]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        fail("GO_TEST_JSON_UNREADABLE", f"{path}: {exc}")

    events: list[dict] = []
    for lineno, line in enumerate(text.splitlines(), 1):
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError as exc:
            fail("GO_TEST_JSON_INVALID", f"{path}:{lineno}: {exc}")
        if not isinstance(event, dict):
            fail("GO_TEST_JSON_NOT_OBJECT", f"{path}:{lineno}")
        events.append(event)

    if not events:
        fail("GO_TEST_JSON_EMPTY", str(path))
    return events


def events_by_test(events: list[dict]) -> dict[str, list[dict]]:
    grouped: dict[str, list[dict]] = {}
    for event in events:
        name = event.get("Test")
        if not isinstance(name, str) or not name:
            continue
        grouped.setdefault(name, []).append(event)
    return grouped


def actions_for(grouped: dict[str, list[dict]], name: str) -> set[str]:
    return {str(event.get("Action")) for event in grouped.get(name, [])}


def output_for(grouped: dict[str, list[dict]], name: str) -> str:
    return "\n".join(
        str(event.get("Output", ""))
        for event in grouped.get(name, [])
        if event.get("Action") == "output"
    )


def all_output(events: list[dict]) -> str:
    return "\n".join(
        str(event.get("Output", ""))
        for event in events
        if event.get("Action") == "output"
    )


def read_exit_code(path: Path) -> int:
    try:
        raw = path.read_text(encoding="utf-8").strip()
    except OSError as exc:
        fail("GO_TEST_EXIT_UNREADABLE", f"{path}: {exc}")
    try:
        return int(raw)
    except ValueError:
        fail("GO_TEST_EXIT_INVALID", f"{path}: {raw!r}")


def read_stderr(path: Path | None) -> str:
    if path is None:
        return ""
    try:
        return path.read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        fail("GO_TEST_STDERR_UNREADABLE", f"{path}: {exc}")


def reject_compile_setup_panic(prefix: str, text: str) -> None:
    match = COMPILE_OR_SETUP_RE.search(text)
    if match:
        fail(
            f"{prefix}_COMPILE_OR_SETUP_ERROR_NOT_BEHAVIOR_EVIDENCE",
            f"matched {match.group(0)!r}",
        )


def validate_current(
    events: list[dict], grouped: dict[str, list[dict]], stderr_text: str
) -> None:
    parent_actions = actions_for(grouped, TEST_NAME)
    if "run" not in parent_actions:
        fail(
            "CURRENT_TEST_NOT_RUN",
            f"{TEST_NAME} has no run event; TestMain may have exited before running tests",
        )
    if "pass" not in parent_actions:
        fail("CURRENT_TEST_NOT_PASS", f"{TEST_NAME} actions={sorted(parent_actions)}")
    if "fail" in parent_actions or "skip" in parent_actions:
        fail("CURRENT_TEST_NOT_GREEN", f"{TEST_NAME} actions={sorted(parent_actions)}")

    for subtest in SUBTESTS:
        sub_actions = actions_for(grouped, subtest)
        if "run" not in sub_actions:
            fail("CURRENT_SUBTEST_NOT_RUN", f"{subtest} has no run event")
        if "pass" not in sub_actions:
            fail("CURRENT_SUBTEST_NOT_PASS", f"{subtest} actions={sorted(sub_actions)}")
        if "fail" in sub_actions or "skip" in sub_actions:
            fail("CURRENT_SUBTEST_NOT_GREEN", f"{subtest} actions={sorted(sub_actions)}")

    harness_output = output_for(grouped, TEST_NAME)
    for subtest in SUBTESTS:
        harness_output += "\n" + output_for(grouped, subtest)
    if not HARNESS_RE.search(harness_output):
        fail("CURRENT_HARNESS_LOG_MISSING", "missing integration harness database guard log")
    if not VERSION_RE.search(harness_output):
        fail("CURRENT_HARNESS_VERSION_MISSING", "missing server_version_num guard log")

    for name, test_events in grouped.items():
        actions = {str(event.get("Action")) for event in test_events}
        if "fail" in actions:
            fail("CURRENT_UNEXPECTED_FAIL", name)
        if "skip" in actions:
            fail("CURRENT_UNEXPECTED_SKIP", name)

    reject_compile_setup_panic("CURRENT", all_output(events) + "\n" + stderr_text)
    print("current: both real-PostgreSQL subtests PASS")
    for subtest in SUBTESTS:
        print(f"  PASS {subtest}")


def validate_literal(
    events: list[dict], grouped: dict[str, list[dict]], stderr_text: str
) -> None:
    parent_actions = actions_for(grouped, TEST_NAME)
    if "run" not in parent_actions:
        fail(
            "LITERAL218_TEST_NOT_RUN",
            f"{TEST_NAME} has no run event; compile/setup failure is not a regression red",
        )
    if "fail" not in parent_actions:
        fail("LITERAL218_TEST_NOT_RED", f"{TEST_NAME} actions={sorted(parent_actions)}")
    if "skip" in parent_actions:
        fail("LITERAL218_TEST_SKIPPED", f"{TEST_NAME} was skipped")

    for subtest in SUBTESTS:
        sub_actions = actions_for(grouped, subtest)
        if "run" not in sub_actions:
            fail("LITERAL218_SUBTEST_NOT_RUN", f"{subtest} has no run event")
        if "fail" not in sub_actions:
            fail(
                "LITERAL218_SUBTEST_NOT_RED",
                f"{subtest} actions={sorted(sub_actions)}; old SQL must fail in each mode",
            )
        if "skip" in sub_actions or "pass" in sub_actions:
            fail("LITERAL218_SUBTEST_NOT_EXPECTED", f"{subtest} actions={sorted(sub_actions)}")

        sub_output = output_for(grouped, subtest)
        if EXPECTED_SQLSTATE not in sub_output:
            fail(
                "LITERAL218_SQLSTATE_MISSING",
                f"{subtest}: expected {EXPECTED_SQLSTATE}; got {sub_output[-800:]!r}",
            )
        if EXPECTED_MESSAGE not in sub_output:
            fail(
                "LITERAL218_CREDENTIALS_MESSAGE_MISSING",
                f"{subtest}: expected {EXPECTED_MESSAGE!r}; got {sub_output[-800:]!r}",
            )

    evidence = "\n".join(
        [output_for(grouped, TEST_NAME)]
        + [output_for(grouped, subtest) for subtest in SUBTESTS]
        + [stderr_text]
    )
    if NON_SQL_RE.search(evidence):
        fail("LITERAL218_NON_SQL_ERROR_NOT_RED", "non-SQL error is not the 42703 regression")
    if TRANSACTION_ABORTED_RE.search(evidence):
        fail(
            "LITERAL218_25P02_NOT_RED",
            "aborted transaction (25P02) is not an independent 42703 regression red",
        )
    reject_compile_setup_panic("LITERAL218", all_output(events) + "\n" + stderr_text)

    print("literal218repository: both subtests RED with independent SQLSTATE=42703 evidence")
    for subtest in SUBTESTS:
        print(f"  FAIL {subtest}: {EXPECTED_SQLSTATE} + {EXPECTED_MESSAGE}")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Validate go test -json evidence for the quota-refresh SQL gate."
    )
    parser.add_argument(
        "mode",
        choices=("current", "literal218repository"),
        help="expected outcome for this regression run",
    )
    parser.add_argument("json_path", help="go test -json output file")
    parser.add_argument("exit_path", help="file containing the go test exit code")
    parser.add_argument("--stderr", default=None, help="go test stderr capture (optional)")
    args = parser.parse_args()

    events = load_events(Path(args.json_path))
    grouped = events_by_test(events)
    exit_code = read_exit_code(Path(args.exit_path))
    stderr_text = read_stderr(Path(args.stderr) if args.stderr else None)

    if args.mode == "current":
        if exit_code != 0:
            fail("CURRENT_EXIT_CODE", f"go test must exit 0, got {exit_code}")
        validate_current(events, grouped, stderr_text)
        return

    if exit_code == 0:
        fail(
            "LITERAL218_EXIT_CODE",
            "go test unexpectedly exited 0; the 218 regression was not reproduced",
        )
    validate_literal(events, grouped, stderr_text)


if __name__ == "__main__":
    main()
