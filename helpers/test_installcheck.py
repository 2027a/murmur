"""Exercise installcheck's shell control flow with real staged install/uninstall.

The prebuilt binary is a tiny stand-in and build is explicitly skipped.
Run make installcheck separately to check the real Go executable.
"""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
MAKE = shutil.which("make")


@unittest.skipUnless(MAKE, "GNU make required")
class InstallcheckTests(unittest.TestCase):
    def run_check(self, fault=""):
        with tempfile.TemporaryDirectory(prefix="murmur-installcheck-") as temp:
            workspace = Path(temp)
            for name in ("Makefile", "README.md", "LICENSE"):
                shutil.copyfile(ROOT / name, workspace / name)
            for name in ("docs", "examples"):
                shutil.copytree(ROOT / name, workspace / name)
            helper = workspace / "helpers/osd-gtk/murmur-osd.py"
            helper.parent.mkdir(parents=True)
            shutil.copyfile(ROOT / "helpers/osd-gtk/murmur-osd.py", helper)
            binary = workspace / "build/murmur"
            binary.parent.mkdir()
            binary.write_text(
                '#!/bin/sh\n'
                'printf "%s\\n" "$1" >>"$BINARY_LOG"\n'
                '[ "$FAULT" != binary-error ] || exit 23\n'
                '[ "$1" = help ]\n'
            )
            binary.chmod(0o755)
            wrapper = workspace / "recursive-make"
            wrapper.write_text(
                '#!/bin/sh\n'
                'set -eu\n'
                'target= osd= destination=\n'
                'for arg do\n'
                '  case "$arg" in\n'
                '    install|uninstall) target="$arg";;\n'
                '    INSTALL_OSD=*) osd="${arg#*=}";;\n'
                '    DESTDIR=*) destination="${arg#*=}";;\n'
                '  esac\n'
                'done\n'
                'printf "%s:%s\\n" "$target" "$osd" >>"$OPERATIONS_LOG"\n'
                'case "$FAULT:$target" in\n'
                '  install-error:install|uninstall-error:uninstall) exit 17;;\n'
                '  install-no-op:install|uninstall-no-op:uninstall) exit 0;;\n'
                'esac\n'
                '"$REAL_MAKE" --assume-old=build "$@"\n'
                'if [ "$FAULT:$target" = missing-readme:install ]; then\n'
                '  rm "$destination/usr/share/doc/murmur/README.md"\n'
                'fi\n'
            )
            wrapper.chmod(0o755)
            staging = workspace / "staging"
            staging.mkdir()
            operations_log = workspace / "operations.log"
            binary_log = workspace / "binary.log"
            env = os.environ.copy()
            for name in ("MAKEFLAGS", "MFLAGS", "MAKELEVEL"):
                env.pop(name, None)
            env.update(
                TMPDIR=str(staging),
                FAULT=fault,
                REAL_MAKE=MAKE,
                OPERATIONS_LOG=str(operations_log),
                BINARY_LOG=str(binary_log),
            )
            result = subprocess.run(
                [MAKE, "--no-print-directory", "--assume-old=build", "installcheck",
                 "GO=false", f"MAKE={wrapper}"],
                cwd=workspace, env=env, stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT, text=True, timeout=20,
            )
            operations = operations_log.read_text().splitlines() if operations_log.exists() else []
            binary_calls = binary_log.read_text().splitlines() if binary_log.exists() else []
            self.assertEqual(list(staging.iterdir()), [], "installcheck leaked its temporary DESTDIR")
            return result, operations, binary_calls

    def test_nominal_install_execute_uninstall(self):
        result, operations, binary_calls = self.run_check()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(operations, ["install:0", "uninstall:0", "install:1", "uninstall:1"])
        self.assertEqual(binary_calls, ["help"])

    def test_failures_are_not_masked_and_stop_later_operations(self):
        for fault, expected_operations in (
            ("install-error", ["install:0"]),
            ("install-no-op", ["install:0"]),
            ("missing-readme", ["install:0"]),
            ("uninstall-error", ["install:0", "uninstall:0"]),
            ("uninstall-no-op", ["install:0", "uninstall:0"]),
            ("binary-error", ["install:0", "uninstall:0", "install:1"]),
        ):
            with self.subTest(fault=fault):
                result, operations, _ = self.run_check(fault)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertEqual(operations, expected_operations, result.stdout)


if __name__ == "__main__":
    unittest.main()
