#!/usr/bin/env python3
"""Validate Vitest JSON evidence for the OpenCode source-input regression gate.

The workflow `.github/workflows/opencode-source-input-gates.yml` runs the current
OpenCodeQuotaFallbackEditor spec twice:

  * current              - every test in the spec must pass;
  * literal217component  - only OpenCodeQuotaFallbackEditor.vue is rewound to
                           9a710719f; the spec, helpers and stubs stay current.
                           The four source-input tests must fail individually
                           with DOM/assertion evidence.

This validator deliberately rejects suite-level import/transform/collection
errors and compile-ish failure messages: a compile error is never the expected
red. It also requires the four source-input tests by exact title, so a shrunken
or skipped spec cannot satisfy the gate.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

DESCRIBE_TITLE = "OpenCodeQuotaFallbackEditor"

# The four source-input behaviors added by fix/opencode-source-input-218.
# Titles must match frontend/src/components/account/__tests__/OpenCodeQuotaFallbackEditor.spec.ts.
EXPECTED_SOURCE_INPUT_TESTS = (
    "adds a blank editable row without preselecting a source or target",
    "accepts a manually typed normal upstream model ID when the catalog failed",
    "commits the source on Enter and prevents implicit form submission",
    "writes a target chosen while the source is still a draft when the source is committed",
)

COMPILE_ERROR_RE = re.compile(
    r"failed to resolve import|failed to load url|cannot find module|"
    r"does not provide an export named|syntaxerror|unexpected token|"
    r"transform failed|parse failure|failed to parse|pre-transform error|"
    r"\[plugin:vite|error when starting dev server",
    re.IGNORECASE,
)

DOM_OR_ASSERTION_RE = re.compile(
    r"assertionerror|unable to get|cannot call get|does not exist|"
    r"expected\b|tocontain|toequal|domwrapper",
    re.IGNORECASE,
)


def fail(message: str) -> None:
    print(f"::error::SOURCE_INPUT_GATE: {message}", file=sys.stderr)
    raise SystemExit(1)


def assertion_messages(assertion: dict) -> str:
    messages = assertion.get("failureMessages") or []
    if isinstance(messages, str):
        messages = [messages]
    return "\n".join(str(message) for message in messages if message is not None)


def load_report(path: Path) -> tuple[dict, list[dict], list[str]]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        fail(f"VITEST_JSON_UNREADABLE: {path}: {exc}")

    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        fail(f"VITEST_JSON_INVALID: {path}: {exc}")

    if not isinstance(data, dict):
        fail("VITEST_JSON_NOT_OBJECT")

    test_results = data.get("testResults")
    if not isinstance(test_results, list) or not test_results:
        fail("VITEST_JSON_NO_TEST_RESULTS")

    assertions: list[dict] = []
    suite_errors: list[str] = []
    for suite in test_results:
        if not isinstance(suite, dict):
            fail("VITEST_JSON_BAD_TEST_RESULT")
        assertion_results = suite.get("assertionResults")
        if not isinstance(assertion_results, list) or not assertion_results:
            # Vitest emits a suite entry without assertion results when the file
            # fails to import/transform/collect. That is not behavior evidence.
            suite_errors.append(
                str(suite.get("message") or suite.get("name") or "<unnamed suite>")
            )
            continue
        for assertion in assertion_results:
            if not isinstance(assertion, dict):
                fail("VITEST_JSON_BAD_ASSERTION_RESULT")
            assertions.append(assertion)

    if not assertions:
        fail("VITEST_JSON_NO_ASSERTIONS")

    num_total = data.get("numTotalTests")
    if isinstance(num_total, int) and num_total != len(assertions):
        fail(
            "VITEST_TEST_COUNT_MISMATCH: "
            f"numTotalTests={num_total} assertionResults={len(assertions)}"
        )

    return data, assertions, suite_errors


def check_no_compile_errors(assertions: list[dict], suite_errors: list[str]) -> None:
    diagnostic = "\n".join(suite_errors)
    for assertion in assertions:
        diagnostic += "\n" + assertion_messages(assertion)
    match = COMPILE_ERROR_RE.search(diagnostic)
    if match:
        fail(
            "COMPILE_OR_COLLECTION_ERROR_NOT_BEHAVIOR_EVIDENCE: "
            f"matched {match.group(0)!r}"
        )


def expected_assertions(assertions: list[dict]) -> dict[str, dict]:
    found: dict[str, list[dict]] = {}
    for assertion in assertions:
        title = assertion.get("title")
        if title not in EXPECTED_SOURCE_INPUT_TESTS:
            continue
        ancestors = assertion.get("ancestorTitles") or []
        if DESCRIBE_TITLE not in ancestors:
            fail(f"EXPECTED_TEST_OUTSIDE_DESCRIBE: {title!r} ancestors={ancestors!r}")
        found.setdefault(title, []).append(assertion)

    missing = [title for title in EXPECTED_SOURCE_INPUT_TESTS if title not in found]
    if missing:
        fail("EXPECTED_SOURCE_INPUT_TEST_MISSING: " + " | ".join(missing))

    expected: dict[str, dict] = {}
    for title in EXPECTED_SOURCE_INPUT_TESTS:
        matches = found[title]
        if len(matches) != 1:
            fail(f"EXPECTED_SOURCE_INPUT_TEST_NOT_UNIQUE: {title!r} matches={len(matches)}")
        expected[title] = matches[0]
    return expected


def expected_status(report: dict, key: str) -> object:
    return report.get(key)


def validate_current(assertions: list[dict], suite_errors: list[str]) -> None:
    if suite_errors:
        fail("CURRENT_SUITE_LEVEL_ERROR: " + " | ".join(suite_errors))

    expected = expected_assertions(assertions)
    for title, assertion in expected.items():
        status = assertion.get("status")
        if status != "passed":
            fail(f"CURRENT_SOURCE_INPUT_TEST_NOT_GREEN: {title!r} status={status!r}")

    non_passing = [
        str(assertion.get("title"))
        for assertion in assertions
        if assertion.get("status") != "passed"
    ]
    if non_passing:
        fail("CURRENT_NON_PASSING_TESTS: " + ", ".join(non_passing[:10]))

    check_no_compile_errors(assertions, suite_errors)
    print(
        f"current: full editor spec green ({len(assertions)} tests); "
        f"{len(expected)} source-input tests PASS"
    )
    for title in EXPECTED_SOURCE_INPUT_TESTS:
        print(f"  PASS {title}")


def validate_literal217component(
    report: dict, assertions: list[dict], suite_errors: list[str]
) -> None:
    if suite_errors:
        fail(
            "LITERAL217_SUITE_LEVEL_ERROR_NOT_BEHAVIOR_EVIDENCE: "
            + " | ".join(suite_errors)
        )
    if report.get("success") is not False:
        fail(f"LITERAL217_REPORT_NOT_RED: success={report.get('success')!r}")

    expected = expected_assertions(assertions)
    evidence: dict[str, str] = {}
    for title, assertion in expected.items():
        status = assertion.get("status")
        if status != "failed":
            fail(
                "LITERAL217_SOURCE_INPUT_TEST_NOT_RED: "
                f"{title!r} status={status!r}"
            )
        messages = assertion_messages(assertion)
        if not messages.strip():
            fail(f"LITERAL217_MISSING_FAILURE_MESSAGE: {title!r}")
        match = COMPILE_ERROR_RE.search(messages)
        if match:
            fail(
                "LITERAL217_COMPILE_ERROR_IS_NOT_RED: "
                f"{title!r} matched {match.group(0)!r}"
            )
        if not DOM_OR_ASSERTION_RE.search(messages):
            fail(f"LITERAL217_NON_DOM_FAILURE: {title!r}: {messages[:400]}")
        evidence[title] = messages.strip().splitlines()[0]

    failed_titles = {
        str(assertion.get("title"))
        for assertion in assertions
        if assertion.get("status") == "failed"
    }
    num_failed = report.get("numFailedTests")
    if isinstance(num_failed, int) and num_failed != len(failed_titles):
        fail(
            "VITEST_FAILED_COUNT_MISMATCH: "
            f"numFailedTests={num_failed} failedAssertionTitles={len(failed_titles)}"
        )

    check_no_compile_errors(assertions, suite_errors)
    print(
        f"literal217component: {len(expected)}/{len(EXPECTED_SOURCE_INPUT_TESTS)} "
        "source-input tests RED with DOM/assertion evidence"
    )
    for title in EXPECTED_SOURCE_INPUT_TESTS:
        print(f"  FAIL {title}: {evidence[title]}")

    additional = sorted(failed_titles - set(EXPECTED_SOURCE_INPUT_TESTS))
    if additional:
        print("literal217component: additional failing tests (recorded, not required):")
        for title in additional:
            print(f"  FAIL {title}")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Validate Vitest JSON evidence for the source-input gate."
    )
    parser.add_argument(
        "mode",
        choices=("current", "literal217component"),
        help="expected outcome for this editor spec run",
    )
    parser.add_argument("json_path", help="Vitest JSON reporter output file")
    args = parser.parse_args()

    report, assertions, suite_errors = load_report(Path(args.json_path))

    if args.mode == "current":
        if report.get("success") is not True:
            fail(f"CURRENT_REPORT_NOT_GREEN: success={report.get('success')!r}")
        validate_current(assertions, suite_errors)
    else:
        validate_literal217component(report, assertions, suite_errors)


if __name__ == "__main__":
    main()
