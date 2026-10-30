"""SKILL.md says what the mock does; its worked example runs, and its front matter and claims are checked."""

from __future__ import annotations

import re
import signal
import subprocess
import sys
import unittest
from pathlib import Path

from mail_mock import manifest

from .support import HOST, WAIT_SECONDS

PACKAGE_ROOT = Path(__file__).resolve().parents[1]
SKILL = (PACKAGE_ROOT / "SKILL.md").read_text(encoding="utf-8")
README = (PACKAGE_ROOT / "README.md").read_text(encoding="utf-8")
EXAMPLE_CONTROL_LINE = 'CONTROL = "http://127.0.0.1:19100"'
#: How long a stopping process is given to exit, in seconds.
EXIT_SECONDS = 10
#: Longest README the order allows, in lines.
README_MAX_LINES = 15


def example_code() -> str:
    """The python block under 'Worked example'."""
    section = SKILL.split("## Worked example", 1)[1]
    return re.search(r"```python\n(.*?)```", section, re.S).group(1)


class SkillTest(unittest.TestCase):
    def test_front_matter_names_the_skill_and_carries_license_and_origin(self) -> None:
        front = SKILL.split("---")[1]
        self.assertRegex(front, r"(?m)^name: mail-mock$")
        self.assertRegex(front, r"(?m)^license: Apache-2\.0$")
        self.assertRegex(front, r"(?m)^description: .{40,}")
        self.assertRegex(front, r"(?m)^metadata:\n(?:  .+\n)*  saphan\.origin: \S+")

    def test_readme_is_at_most_fifteen_lines(self) -> None:
        self.assertLessEqual(len(README.splitlines()), README_MAX_LINES)

    def test_every_setting_the_skill_documents_is_declared_and_every_declared_setting_is_documented(self) -> None:
        section = SKILL.split("## Settings", 1)[1].split("\n## ", 1)[0]
        documented = set(re.findall(r"^\| `([a-z_]+)`(?:, `([a-z_]+)`)? \|", section, re.M))
        names = {n for pair in documented for n in pair if n}
        self.assertEqual(names, {s["name"] for s in manifest.settings()})

    def test_every_safe_field_the_skill_names_is_declared_safe(self) -> None:
        for field in ("from", "to", "subject", "message_id"):
            self.assertIn(field, manifest.SAFE_FIELDS)
            self.assertIn(f"`{field}`", SKILL)

    def test_the_worked_example_runs_against_a_real_mock_process_and_every_assertion_in_it_holds(self) -> None:
        self.assertIn(EXAMPLE_CONTROL_LINE, SKILL)
        proc = subprocess.Popen(
            [sys.executable, "-m", "mail_mock", "serve", "--control", f"{HOST}:0"],
            cwd=PACKAGE_ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        self.addCleanup(lambda: proc.poll() is None and proc.kill())
        url = proc.stdout.readline().strip().rsplit(" ", 1)[1]
        code = example_code().replace(EXAMPLE_CONTROL_LINE, f'CONTROL = "{url}"')
        namespace: dict = {"__name__": "skill_example"}
        exec(compile(code, "SKILL.md worked example", "exec"), namespace)  # noqa: S102 - the file under test is this repository's own
        self.assertIn("run", namespace)
        proc.send_signal(signal.SIGTERM)
        proc.communicate(timeout=EXIT_SECONDS)
        self.assertEqual(proc.returncode, 0)

    def test_the_default_password_in_the_example_is_the_manifest_default(self) -> None:
        self.assertIn(manifest.DEFAULT_PASSWORD, example_code())
        self.assertIn(manifest.DEFAULT_USER, example_code())
