"""Tests for release notice omission and failed-publication boundaries."""

import importlib.util
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("licenses", Path(__file__).with_name("licenses.py"))
licenses = importlib.util.module_from_spec(spec)
spec.loader.exec_module(licenses)


class LicenseCollectionTest(unittest.TestCase):
    def test_local_replacement_retains_upstream_identity_and_separate_tree_digest(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary)
            module = {"Path": "example.com/dependency", "Version": "v1.2.3"}
            declared = {"module": module["Path"], "version": module["Version"],
                        "module_sum": "h1:upstream-archive", "commit": "upstream-commit"}
            (source / "UPSTREAM.json").write_text(json.dumps(declared))
            (source / "LICENSE").write_text("upstream notice\n")
            code = source / "patched.go"
            code.write_text("package patched\n")
            sums = {(module["Path"], module["Version"]): "h1:upstream-archive"}
            reference = licenses.upstream_reference(module, source, sums)
            self.assertEqual(reference["sum"], "h1:upstream-archive")
            self.assertEqual(reference["declared_provenance"]["commit"], "upstream-commit")
            before = licenses.source_tree_digest(source)
            code.write_text("package patched\n// local behavior change\n")
            after = licenses.source_tree_digest(source)
            self.assertNotEqual(before["sha256"], after["sha256"])
            self.assertEqual(before["files"], after["files"])
            self.assertEqual(licenses.upstream_reference(module, source, {})["sum"], "h1:upstream-archive")
            with self.assertRaisesRegex(RuntimeError, "disagrees with go.sum"):
                licenses.upstream_reference(module, source, {(module["Path"], module["Version"]): "h1:other-archive"})

    def test_local_replacement_digest_rejects_symlinks(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary)
            (source / "code.go").write_text("package code\n")
            (source / "alias.go").symlink_to(source / "code.go")
            with self.assertRaisesRegex(RuntimeError, "non-regular file"):
                licenses.source_tree_digest(source)

    def test_nested_notices_are_preserved_and_code_is_not_a_notice(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "LICENSE").write_text("root license\n")
            (root / "license.go").write_text("package license\n")
            (root / "thirdparty").mkdir()
            (root / "thirdparty/NOTICE.txt").write_text("third party attribution\n")
            (root / "LICENSES").mkdir()
            (root / "LICENSES/BSD-3-Clause").write_text("additional license\n")
            (root / "dist").mkdir()
            (root / "dist/LICENSE").write_text("stale output\n")
            self.assertEqual(
                [path.as_posix() for path in licenses.notice_files(root)],
                ["LICENSE", "LICENSES/BSD-3-Clause", "thirdparty/NOTICE.txt"],
            )

    def test_notice_symlink_cannot_escape_module_source(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            source.mkdir()
            (root / "outside").write_text("not part of the source archive")
            (source / "LICENSE").symlink_to(root / "outside")
            with self.assertRaisesRegex(RuntimeError, "leaves its source directory"):
                licenses.notice_files(source)

    def test_nested_dependency_license_does_not_hide_missing_module_license(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            project = root / "project"
            project.mkdir()
            (project / "go.mod").write_text("module example.com/project\n")
            (project / "LICENSE").write_text("project license\n")
            dependency = root / "dependency"
            (dependency / "thirdparty").mkdir(parents=True)
            (dependency / "thirdparty/LICENSE").write_text("not the module's license\n")
            modules = [
                {"Path": "example.com/project", "Main": True, "Dir": str(project), "GoMod": str(project / "go.mod")},
                {"Path": "example.com/dependency", "Version": "v1.0.0", "Dir": str(dependency)},
            ]

            def fake_go(_go, *arguments, **_kwargs):
                if arguments[:3] == ("list", "-m", "-json"):
                    return "\n".join(json.dumps(item) for item in modules)
                if arguments[:3] == ("mod", "download", "-json"):
                    return ""
                if arguments[:3] == ("list", "-deps", "-json"):
                    return ""
                raise AssertionError(arguments)

            with mock.patch.object(licenses, "run_go", side_effect=fake_go):
                with self.assertRaisesRegex(RuntimeError, "root license notice missing for: example.com/dependency@v1.0.0"):
                    licenses.collect(types.SimpleNamespace(go="go", target=["linux/amd64"]), root / "output")

    def test_failed_collection_preserves_previous_complete_bundle(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "licenses"
            output.mkdir()
            previous = '{"status":"previous complete bundle"}\n'
            (output / "manifest.json").write_text(previous)
            with mock.patch("sys.argv", ["licenses.py", "--output", str(output)]):
                with mock.patch.object(licenses, "collect", side_effect=RuntimeError("missing source notice")):
                    with self.assertRaisesRegex(SystemExit, "missing source notice"):
                        licenses.main()
            self.assertEqual((output / "manifest.json").read_text(), previous)
            self.assertEqual([path.name for path in Path(temporary).iterdir()], ["licenses"])


if __name__ == "__main__":
    unittest.main()
