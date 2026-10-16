#!/usr/bin/env python3
"""Run as WSL Ubuntu root; installer effects stay in disposable chroots.

Use the real Agent and real install/chown/flock. Stub only systemctl, account
creation and release transport. No mounts, host account changes or real systemctl.
"""

import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import pty
import re
import select
import shutil
import stat
import subprocess
import sys
import sysconfig
import tempfile
import termios
import time
import unittest


PROJECT = Path(__file__).resolve().parents[1]
BINARY = "/usr/local/bin/snell-agent"
CONFIG = "/etc/snell-agent/config.json"
UNIT = "/etc/systemd/system/snell-agent.service"
STATE = "/var/lib/snell-agent"
REPORTS = STATE + "/reports.json"
ACCOUNT_ID = 991
RELEASE_REPO = "Micah123321/mi-node"
RELEASE_TAG = "snell-v1.2.3"
RELEASE_ASSET = "snell-agent-linux-amd64"
TOKEN = 'quote" backslash\\ $(touch /tmp/token-executed) ' + chr(96) + 'touch /tmp/token-backtick' + chr(96) + ' \u4e2d\u6587'
REPORT_BYTES = b'{ "pending": [{"id":"bill-42","bytes":987654321}], "opaque":"\\u4e2d" }\n'

SYSTEMCTL = r'''#!/usr/bin/python3
import hashlib, json, pathlib, sys
p = pathlib.Path("/run/systemctl-state.json")
s = json.loads(p.read_text())
action = sys.argv[1]
files = {}
for key, name in [("binary", "/usr/local/bin/snell-agent"),
                  ("config", "/etc/snell-agent/config.json"),
                  ("unit", "/etc/systemd/system/snell-agent.service")]:
    f = pathlib.Path(name)
    files[key] = hashlib.sha256(f.read_bytes()).hexdigest() if f.is_file() else None
with open("/run/systemctl.log", "a") as log:
    log.write(json.dumps({"args": sys.argv[1:], "files": files,
                          "active": s["active"], "enabled": s["enabled"]}) + "\n")
if action in s.get("fail_once", []):
    s["fail_once"].remove(action)
    if action == "start":
        s["active"] = False
    p.write_text(json.dumps(s))
    sys.exit(1)
if action == "is-active":
    phase = s.get("phase", "active" if s["active"] else "inactive")
    if "--quiet" not in sys.argv:
        print(phase)
    sys.exit(0 if phase in ("active", "reloading") else 3)
elif action == "is-enabled":
    mode = s.get("enabled_mode", "enabled" if s["enabled"] else "disabled")
    if "--quiet" not in sys.argv:
        print(mode)
    sys.exit(0 if s["enabled"] else 1)
elif action == "start":
    s["active"] = not s.pop("exit_after_start", False)
    s.pop("phase", None)
elif action == "stop":
    s["active"] = False
    s.pop("phase", None)
elif action == "enable":
    s["enabled"] = True
    s["enabled_mode"] = "enabled-runtime" if "--runtime" in sys.argv else "enabled"
elif action == "disable":
    s["enabled"] = False
    s["enabled_mode"] = "disabled"
elif action not in ("daemon-reload", "reset-failed"):
    sys.exit("unexpected systemctl action: " + action)
p.write_text(json.dumps(s))
'''

ACCOUNT_TOOL = r'''#!/usr/bin/python3
import json, pathlib, sys
name = sys.argv[-1]
kind = pathlib.Path(sys.argv[0]).name
with open("/run/accounts.log", "a") as log:
    log.write(json.dumps([kind] + sys.argv[1:]) + "\n")
if name != "snell-agent":
    sys.exit("unexpected account")
if kind == "groupadd":
    with open("/etc/group", "a") as f:
        f.write("snell-agent:x:991:\n")
elif kind == "useradd":
    with open("/etc/passwd", "a") as f:
        f.write("snell-agent:x:991:991::/var/lib/snell-agent:/usr/sbin/nologin\n")
else:
    sys.exit("unexpected account tool")
'''

CURL = r'''#!/usr/bin/python3
import json, pathlib, sys
args = sys.argv[1:]
with open("/run/curl.log", "a") as log:
    log.write(json.dumps(args) + "\n")
urls = [arg for arg in args if arg.startswith(("https://", "http://"))]
if len(urls) != 1:
    sys.exit("curl fixture requires exactly one URL")
responses = json.loads(pathlib.Path("/run/curl-responses.json").read_text())
response = responses.get(urls[0])
if response is None:
    sys.exit("unexpected curl URL: " + urls[0])
if response.get("exit"):
    sys.exit(response["exit"])
data = (pathlib.Path(response["file"]).read_bytes() if "file" in response
        else response["body"].encode("utf-8"))
output = "-"
for index, arg in enumerate(args):
    if arg in ("--output", "-o"):
        output = args[index + 1]
    elif arg.startswith("--output="):
        output = arg.split("=", 1)[1]
if output == "-":
    sys.stdout.buffer.write(data)
else:
    pathlib.Path(output).write_bytes(data)
'''


def put(root, name, content, mode=0o644):
    path = root / name.lstrip("/")
    path.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(content, str):
        content = content.encode("utf-8")
    path.write_bytes(content)
    path.chmod(mode)
    return path


def config_bytes(**overrides):
    config = {"panel_url": "https://panel.example.invalid", "node_id": 41,
              "token": TOKEN, "state_dir": STATE, "stale_after": "5m"}
    config.update(overrides)
    return (json.dumps(config, ensure_ascii=False, indent=3) + "  \n").encode("utf-8")


class InstallerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if sys.platform != "linux" or os.geteuid() != 0:
            raise RuntimeError("Run this suite as root in WSL Ubuntu, not on the host")
        if platform.machine() not in ("x86_64", "amd64"):
            raise RuntimeError("This suite requires the real Linux amd64 Agent")
        candidates = [PROJECT / "bin/snell-agent-linux-amd64", PROJECT / "snell-agent-linux-amd64"]
        cls.agent = next((path for path in candidates if path.is_file()), None)
        if cls.agent is None:
            raise RuntimeError("Build bin/snell-agent-linux-amd64 or use the extracted amd64 installation package")
        # WSL may mount /tmp with nodev; /dev/null and controlling TTY need devices.
        cls.scratch = tempfile.TemporaryDirectory(prefix="snell-install-tests-", dir="/var/tmp")
        cls.addClassCleanup(cls.scratch.cleanup)
        cls.base = Path(cls.scratch.name) / "base"
        cls.base.mkdir()
        cls.copied = set()
        cls.chroot = shutil.which("chroot")
        if not cls.chroot:
            raise RuntimeError("chroot is required")
        for command in ("bash", "env", "install", "chown", "chmod", "cp", "mv", "rm",
                        "mktemp", "dirname", "uname", "id", "getent", "flock", "date",
                        "sha256sum", "cat", "sleep", "python3"):
            source = shutil.which(command)
            if not source:
                raise RuntimeError("Missing fixture dependency: " + command)
            cls.copy_executable(Path(source))
        stdlib = Path(sysconfig.get_path("stdlib"))
        shutil.copytree(stdlib, cls.base / str(stdlib).lstrip("/"), dirs_exist_ok=True,
                        ignore=shutil.ignore_patterns("__pycache__", "test", "tests",
                                                     "site-packages", "dist-packages"))
        for extension in (cls.base / str(stdlib).lstrip("/")).rglob("*.so"):
            cls.copy_libraries(Path("/") / extension.relative_to(cls.base))
        # A flat /bin avoids dependence on the distribution's usr-merge links.
        for source, target in (("bash", "/bin/bash"), ("python3", "/usr/bin/python3")):
            cls.copy_executable(Path(shutil.which(source)), target)
        for directory in ("tmp", "dev", "run/systemd/system", "usr/local/bin",
                          "etc/systemd/system", "workspace/bin"):
            (cls.base / directory).mkdir(parents=True, exist_ok=True)
        (cls.base / "tmp").chmod(0o1777)
        put(cls.base, "/etc/passwd", "root:x:0:0:root:/root:/bin/bash\n")
        put(cls.base, "/etc/group", "root:x:0:\n")
        put(cls.base, "/etc/nsswitch.conf", "passwd: files\ngroup: files\n")
        put(cls.base, "/usr/bin/systemctl", SYSTEMCTL, 0o755)
        put(cls.base, "/usr/bin/groupadd", ACCOUNT_TOOL, 0o755)
        put(cls.base, "/usr/bin/useradd", ACCOUNT_TOOL, 0o755)
        put(cls.base, "/usr/bin/curl", CURL, 0o755)
        shutil.copy2(PROJECT / "install.sh", cls.base / "workspace/install.sh")
        shutil.copy2(cls.agent, cls.base / "workspace/bin/snell-agent-linux-amd64")
        cls.agent_bytes = (cls.base / "workspace/bin/snell-agent-linux-amd64").read_bytes()
        cls.agent_hash = hashlib.sha256(cls.agent_bytes).hexdigest()
        cls.unit_bytes = (PROJECT / "deploy/snell-agent.service").read_bytes()

    @classmethod
    def copy_executable(cls, source, target=None):
        target = target or str(source)
        dest = cls.base / str(target).lstrip("/")
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, dest, follow_symlinks=True)
        cls.copy_libraries(source)

    @classmethod
    def copy_libraries(cls, source):
        source = source.resolve()
        if source in cls.copied:
            return
        cls.copied.add(source)
        result = subprocess.run(["ldd", str(source)], capture_output=True, text=True)
        if result.returncode and not any(message in result.stdout + result.stderr
                                        for message in ("not a dynamic executable", "statically linked")):
            raise RuntimeError("ldd failed for %s: %s" % (source, result.stderr))
        for library in re.findall(r"(?:=>\s*)?(/[^\s()]+)", result.stdout):
            path = Path(library)
            dest = cls.base / library.lstrip("/")
            if not dest.exists():
                dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(path, dest, follow_symlinks=True)

    def setUp(self):
        self.case = tempfile.TemporaryDirectory(prefix="case-", dir=self.scratch.name)
        self.addCleanup(self.case.cleanup)
        self.root = Path(self.case.name) / "root"
        # Link immutable runtime assets; /etc and /run need private writable copies.
        shutil.copytree(self.base, self.root,
                        copy_function=lambda src, dst: os.link(src, dst)
                        if Path(src).relative_to(self.base).parts[0] in ("usr", "bin", "lib", "lib64", "workspace")
                        else shutil.copy2(src, dst))
        for name, major, minor in (("null", 1, 3), ("tty", 5, 0)):
            os.mknod(self.root / "dev" / name, stat.S_IFCHR | 0o666, os.makedev(major, minor))
        self.control(active=False, enabled=False)
        self.imported = config_bytes()
        put(self.root, "/input/config.json", self.imported, 0o600)
        put(self.root, "/input/token", TOKEN + "\n", 0o600)
        self.responses = {}
        self.respond("https://downloads.example.invalid/agent",
                     file="/workspace/bin/snell-agent-linux-amd64")

    def path(self, name):
        return self.root / name.lstrip("/")

    def control(self, **values):
        path = self.path("/run/systemctl-state.json")
        state = json.loads(path.read_text()) if path.exists() else {}
        state.update(values)
        put(self.root, "/run/systemctl-state.json", json.dumps(state))
        return state

    def calls(self):
        path = self.path("/run/systemctl.log")
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def respond(self, url, **response):
        self.responses[url] = response
        put(self.root, "/run/curl-responses.json", json.dumps(self.responses), 0o600)

    def curl_calls(self):
        path = self.path("/run/curl.log")
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def curl_urls(self):
        return [arg for call in self.curl_calls() for arg in call
                if arg.startswith(("https://", "http://"))]

    def api_url(self, page=1, repo=RELEASE_REPO):
        return "https://api.github.com/repos/%s/releases?per_page=100&page=%d" % (repo, page)

    def release(self, tag=RELEASE_TAG, assets=None, draft=False, repo=RELEASE_REPO):
        if assets is None:
            assets = [RELEASE_ASSET, "SHA256SUMS"]
        return {"tag_name": tag, "draft": draft, "prerelease": False,
                "assets": [{"name": name, "browser_download_url":
                            "https://github.com/%s/releases/download/%s/%s" % (repo, tag, name)}
                           for name in assets]}

    def release_fixture(self, pages=None, tag=RELEASE_TAG, repo=RELEASE_REPO, manifest=None):
        if pages is None:
            pages = [[self.release(tag, repo=repo)]]
        for page in range(1, 6):
            self.respond(self.api_url(page, repo),
                         body=json.dumps(pages[page - 1] if page <= len(pages) else []))
        prefix = "https://github.com/%s/releases/download/%s/" % (repo, tag)
        binary_url, manifest_url = prefix + RELEASE_ASSET, prefix + "SHA256SUMS"
        self.respond(binary_url, file="/workspace/bin/snell-agent-linux-amd64")
        self.respond(manifest_url, body=manifest if manifest is not None
                     else self.agent_hash + "  " + RELEASE_ASSET + "\n")
        return binary_url, manifest_url

    def assert_no_installation(self):
        for name in (BINARY, CONFIG, UNIT, STATE, "/etc/snell-agent", "/var/backups/snell-agent"):
            self.assertFalse(self.path(name).exists(), name)
        self.assertFalse(self.path("/run/accounts.log").exists())
        self.assertEqual(self.calls(), [])
        self.assertFalse(list(self.path("/tmp").glob("snell-agent-install.*")))

    def run_chroot(self, *args, input=None):
        return subprocess.run([self.chroot, str(self.root), *args], cwd="/",
                              stdin=subprocess.DEVNULL if input is None else None, input=input,
                              capture_output=True, text=True, timeout=30,
                              env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C", "PYTHONUTF8": "1",
                                   "PYTHONDONTWRITEBYTECODE": "1"})

    def install(self, *args, success=True, stream=False):
        injected_failures = self.control().get("fail_once", [])
        if stream:
            result = self.run_chroot("/bin/bash", "-s", "--", *args,
                                     input=(self.base / "workspace/install.sh").read_text())
        else:
            result = self.run_chroot("/bin/bash", "/workspace/install.sh", *args)
        output = result.stdout + result.stderr
        self.assertNotIn(TOKEN, output, "installer leaked the token")
        self.assertFalse(self.path("/tmp/token-executed").exists(), "token shell substitution executed")
        self.assertFalse(self.path("/tmp/token-backtick").exists(), "token backticks executed")
        if success:
            self.assertEqual(result.returncode, 0, output)
        else:
            self.assertNotEqual(result.returncode, 0, output)
            if injected_failures:
                self.assertEqual(self.control().get("fail_once"), [], output)
                self.assertIn("restoring", output)
        return output

    def seed_old(self, active=True, enabled=True):
        self.old = {BINARY: self.agent_bytes + b"\nprevious-agent-version\n",
                    CONFIG: self.imported, UNIT: b"[Unit]\nDescription=previous-unit\n"}
        for name, data in self.old.items():
            put(self.root, name, data, {BINARY: 0o755, CONFIG: 0o600, UNIT: 0o644}[name])
        put(self.root, REPORTS, REPORT_BYTES, 0o600)
        self.control(active=active, enabled=enabled)

    def assert_old(self, active=True, enabled=True):
        for name, data in self.old.items():
            self.assertEqual(self.path(name).read_bytes(), data, name)
        self.assertEqual(self.path(REPORTS).read_bytes(), REPORT_BYTES)
        state = self.control()
        self.assertEqual((state["active"], state["enabled"]), (active, enabled))

    def assert_preflight_failure(self, *args, expected):
        output = self.install(*args, success=False)
        self.assertIn(expected, output)
        self.assert_old()
        self.assertEqual(self.calls(), [], "preflight failure touched the old service")
        self.assertFalse(self.path("/var/backups/snell-agent").exists())
        return output

    def test_first_install_escapes_token_and_sets_permissions(self):
        output = self.install("--panel-url", "https://panel.example.invalid", "--node-id", "41",
                              "--token-file", "/input/token")
        config = json.loads(self.path(CONFIG).read_text())
        self.assertEqual(config["token"], TOKEN)
        self.assertEqual(config["node_id"], 41)
        self.assertEqual(config["panel_url"], "https://panel.example.invalid")
        self.assertEqual(config["state_dir"], STATE)
        self.assertNotIn("token-executed", output)
        for name, mode, owner in ((BINARY, 0o755, 0), (UNIT, 0o644, 0),
                                  (CONFIG, 0o600, ACCOUNT_ID), (STATE, 0o700, ACCOUNT_ID),
                                  ("/etc/snell-agent", 0o750, 0)):
            info = self.path(name).stat()
            self.assertEqual(stat.S_IMODE(info.st_mode), mode, name)
            self.assertEqual(info.st_uid, owner, name)
            self.assertEqual(info.st_gid, ACCOUNT_ID if name in (CONFIG, STATE, "/etc/snell-agent") else 0, name)
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        self.assertEqual(self.path(UNIT).read_bytes(), self.unit_bytes)
        self.assertTrue(self.control()["active"])
        self.assertTrue(self.control()["enabled"])
        accounts = [json.loads(line) for line in self.path("/run/accounts.log").read_text().splitlines()]
        self.assertEqual([call[0] for call in accounts], ["groupadd", "useradd"])
        self.assertIn("--system", accounts[0])
        self.assertIn("--no-create-home", accounts[1])
        self.assertIn("/usr/sbin/nologin", accounts[1])

    def test_imported_config_is_preserved_byte_for_byte(self):
        self.install("--binary", "/workspace/bin/snell-agent-linux-amd64", "--config", "/input/config.json")
        self.assertEqual(self.path(CONFIG).read_bytes(), self.imported)

    def test_repeat_install_preserves_config_reports_and_makes_backups(self):
        self.install("--config", "/input/config.json", "--no-start")
        put(self.root, REPORTS, REPORT_BYTES, 0o600)
        config_inode = self.path(CONFIG).stat().st_ino
        self.install()
        self.assertEqual(self.path(CONFIG).read_bytes(), self.imported)
        self.assertEqual(self.path(CONFIG).stat().st_ino, config_inode)
        self.assertEqual(self.path(REPORTS).read_bytes(), REPORT_BYTES)
        backups = list(self.path("/var/backups/snell-agent").glob("*/config"))
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].read_bytes(), self.imported)
        self.assertEqual((backups[0].parent / "binary").read_bytes(), self.agent_bytes)
        self.assertEqual((backups[0].parent / "service").read_bytes(), self.unit_bytes)

    def test_upgrade_atomically_replaces_program_and_unit(self):
        self.seed_old()
        with self.path(BINARY).open("rb") as binary, self.path(UNIT).open("rb") as unit:
            old_inodes = (os.fstat(binary.fileno()).st_ino, os.fstat(unit.fileno()).st_ino)
            self.install()
            self.assertEqual(binary.read(), self.old[BINARY])
            self.assertEqual(unit.read(), self.old[UNIT])
        self.assertNotEqual(self.path(BINARY).stat().st_ino, old_inodes[0])
        self.assertNotEqual(self.path(UNIT).stat().st_ino, old_inodes[1])
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        self.assertEqual(self.path(UNIT).read_bytes(), self.unit_bytes)
        self.assertEqual(self.path(REPORTS).read_bytes(), REPORT_BYTES)
        self.assertFalse(list(self.root.rglob("*.install-*")))
        self.assertFalse(list(self.root.rglob("*.rollback-*")))
        stop = next(call for call in self.calls() if call["args"][0] == "stop")
        self.assertEqual(stop["files"]["binary"], hashlib.sha256(self.old[BINARY]).hexdigest())

    def test_existing_config_rejects_replacement(self):
        self.seed_old()
        for args in (("--config", "/input/config.json"),
                     ("--panel-url", "https://other.example.invalid", "--node-id", "42", "--token-file", "/input/token")):
            with self.subTest(args=args):
                self.assert_preflight_failure(*args, expected="Configuration already exists")

    def test_sha256_mismatch_fails_before_stop(self):
        self.seed_old()
        self.assert_preflight_failure("--sha256", "0" * 64, expected="Binary SHA256 mismatch")

    def test_wrong_architecture_fails_before_stop(self):
        self.seed_old()
        data = bytearray(self.agent_bytes)
        self.assertEqual(data[:6], b"\x7fELF\x02\x01")
        data[18:20] = (183).to_bytes(2, "little")
        put(self.root, "/input/arm64-agent", data, 0o755)
        self.assert_preflight_failure("--binary", "/input/arm64-agent", expected="not a Linux 64-bit ELF")

    def test_invalid_existing_config_fails_before_stop(self):
        self.seed_old()
        self.old[CONFIG] = config_bytes(node_id=0)
        put(self.root, CONFIG, self.old[CONFIG], 0o600)
        self.assert_preflight_failure(expected="configuration failed")

    def test_invalid_first_config_rejected_by_real_agent(self):
        for label, config in (("invalid node", config_bytes(node_id=0)),
                              ("malformed JSON", b"{invalid"),
                              ("unknown field", config_bytes(unknown_field=True))):
            with self.subTest(case=label):
                put(self.root, "/input/invalid.json", config, 0o600)
                output = self.install("--config", "/input/invalid.json", success=False)
                self.assertIn("configuration failed", output)
                self.assertFalse(self.path(BINARY).exists())
                self.assertFalse(self.path(CONFIG).exists())
                self.assertFalse(self.path(STATE).exists())
                self.assertEqual(self.calls(), [])

    def test_nonstandard_state_directory_fails_before_stop(self):
        self.seed_old()
        self.old[CONFIG] = config_bytes(state_dir="/tmp/alternate-state")
        put(self.root, CONFIG, self.old[CONFIG], 0o600)
        self.assert_preflight_failure(expected="requires state_dir=/var/lib/snell-agent")
        self.assertFalse(self.path("/tmp/alternate-state").exists())

    def test_no_start_first_install_enables_but_leaves_stopped(self):
        self.install("--config", "/input/config.json", "--no-start")
        state = self.control()
        self.assertEqual((state["active"], state["enabled"]), (False, True))
        self.assertNotIn("start", [call["args"][0] for call in self.calls()])

    def test_no_start_upgrade_stops_previously_active_service(self):
        self.seed_old()
        self.install("--no-start")
        self.assertEqual((self.control()["active"], self.control()["enabled"]), (False, True))
        self.assertEqual(self.path(CONFIG).read_bytes(), self.old[CONFIG])
        self.assertEqual(self.path(REPORTS).read_bytes(), REPORT_BYTES)
        actions = [call["args"][0] for call in self.calls()]
        self.assertIn("stop", actions)
        self.assertNotIn("start", actions)

    def test_no_start_stops_service_waiting_for_automatic_restart(self):
        self.seed_old(active=False)
        self.control(phase="activating")
        self.install("--no-start")
        self.assertFalse(self.control()["active"])
        self.assertNotIn("phase", self.control())
        actions = [call["args"][0] for call in self.calls()]
        self.assertIn("stop", actions)
        self.assertNotIn("start", actions)

    def test_failed_upgrade_restores_runtime_only_enablement(self):
        self.seed_old()
        self.control(enabled_mode="enabled-runtime", fail_once=["start"])
        self.install(success=False)
        self.assert_old()
        self.assertEqual(self.control()["enabled_mode"], "enabled-runtime")
        self.assertIn(["enable", "--runtime", "snell-agent.service"],
                      [call["args"] for call in self.calls()])

    def test_private_lock_leaves_global_lock_directory_unchanged(self):
        self.path("/run/lock").mkdir()
        self.path("/run/lock").chmod(0o1777)
        self.install("--config", "/input/config.json", "--no-start")
        self.assertEqual(stat.S_IMODE(self.path("/run/lock").stat().st_mode), 0o1777)
        private = self.path("/run/snell-agent-installer").stat()
        self.assertEqual(stat.S_IMODE(private.st_mode), 0o700)
        self.assertEqual(private.st_uid, 0)

    def test_lock_symlink_never_truncates_existing_config(self):
        self.seed_old()
        lock = self.path("/run/snell-agent-installer/install.lock")
        lock.parent.mkdir(mode=0o700)
        lock.symlink_to(CONFIG)
        self.assert_preflight_failure(expected="Invalid installer lock file")

    def test_concurrent_install_exits_before_service_changes(self):
        self.seed_old()
        lock = self.path("/run/snell-agent-installer/install.lock")
        lock.parent.mkdir(mode=0o700)
        with lock.open("a") as handle:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.assert_preflight_failure(expected="Another Snell Agent installation")

    def test_invalid_utf8_token_is_not_echoed_in_error(self):
        put(self.root, "/input/token", b"secret-prefix-\xff-private", 0o600)
        output = self.install("--panel-url", "https://panel.example.invalid", "--node-id", "41",
                              "--token-file", "/input/token", success=False)
        self.assertIn("invalid configuration input", output)
        self.assertNotIn("secret-prefix", output)
        self.assertFalse(self.path(CONFIG).exists())
        self.assertEqual(self.calls(), [])

    def test_start_failure_restores_old_files_and_active_enabled_state(self):
        self.seed_old()
        self.control(fail_once=["start"])
        output = self.install(success=False)
        self.assertIn("restoring", output)
        self.assert_old()
        starts = [call for call in self.calls() if call["args"][0] == "start"]
        self.assertEqual(len(starts), 2)
        self.assertEqual(starts[0]["files"]["binary"], self.agent_hash)
        self.assertEqual(starts[1]["files"]["binary"], hashlib.sha256(self.old[BINARY]).hexdigest())
        for key, name in (("config", CONFIG), ("unit", UNIT)):
            self.assertEqual(starts[1]["files"][key], hashlib.sha256(self.old[name]).hexdigest())

    def test_start_failure_restores_previously_inactive_disabled_service(self):
        self.seed_old(active=False, enabled=False)
        self.control(fail_once=["start"])
        self.install(success=False)
        self.assert_old(active=False, enabled=False)
        self.assertEqual([call["args"][0] for call in self.calls()].count("start"), 1)

    def test_first_install_failure_removes_new_files_but_keeps_report_queue(self):
        put(self.root, REPORTS, REPORT_BYTES, 0o600)
        self.control(fail_once=["start"])
        self.install("--config", "/input/config.json", success=False)
        for name in (BINARY, CONFIG, UNIT):
            self.assertFalse(self.path(name).exists(), name)
        self.assertTrue(self.path(STATE).is_dir())
        self.assertEqual(self.path(REPORTS).read_bytes(), REPORT_BYTES)
        self.assertEqual((self.control()["active"], self.control()["enabled"]), (False, False))
        self.assertFalse(list(self.root.rglob("*.install-*")))

    def test_daemon_reload_failure_rolls_back(self):
        self.seed_old()
        self.control(fail_once=["daemon-reload"])
        self.install(success=False)
        self.assert_old()

    def test_enable_failure_rolls_back(self):
        self.seed_old()
        self.control(fail_once=["enable"])
        self.install(success=False)
        self.assert_old()

    def test_service_exits_after_start_rolls_back(self):
        self.seed_old()
        self.control(exit_after_start=True)
        output = self.install(success=False)
        self.assertIn("Service exited after startup", output)
        self.assert_old()

    def test_unknown_and_missing_arguments_fail_before_stop(self):
        self.seed_old()
        for args, expected in ((("--unknown",), "Unknown option"),
                               (("--token",), "Missing value"),
                               (("-t",), "Missing value"),
                               (("-a",), "Missing value"),
                               (("-n",), "Missing value"),
                               (("--binary",), "Missing value"),
                               (("--sha256", "not-a-checksum"), "SHA256 must contain"),
                               (("--binary", "/workspace/bin/snell-agent-linux-amd64", "--build"), "Select only one")):
            with self.subTest(option=args[0]):
                self.assert_preflight_failure(*args, expected=expected)

    def test_https_download_uses_checksum_and_restricts_redirect_protocol(self):
        self.install("--binary-url", "https://downloads.example.invalid/agent", "--sha256", self.agent_hash,
                     "--config", "/input/config.json", "--no-start")
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        self.assertEqual(len(self.curl_calls()), 1)
        args = self.curl_calls()[0]
        for flag in ("--proto", "--proto-redir"):
            self.assertEqual(args[args.index(flag) + 1], "=https")

    def test_download_bad_checksum_fails_before_stop(self):
        self.seed_old()
        self.assert_preflight_failure("--binary-url", "https://downloads.example.invalid/agent",
                                      "--sha256", "0" * 64, expected="Binary SHA256 mismatch")
        self.assertTrue(self.path("/run/curl.log").exists())

    def test_download_requires_https_and_sha256_before_stop(self):
        self.seed_old()
        for args in (("--binary-url", "https://downloads.example.invalid/agent"),
                     ("--binary-url", "http://downloads.example.invalid/agent", "--sha256", self.agent_hash)):
            with self.subTest(url=args[1]):
                self.assert_preflight_failure(*args, expected="Downloads require an HTTPS URL and --sha256")
        self.assertFalse(self.path("/run/curl.log").exists())

    def test_build_missing_source_fails_before_stop(self):
        self.seed_old()
        self.assert_preflight_failure("--build", expected="Source module is missing")

    def test_short_parameters_accept_http_and_special_token(self):
        panel = "http://panel.example.invalid:8080"
        self.install("-a", panel, "-t", TOKEN, "-n", "41", "--skip-start")
        config = json.loads(self.path(CONFIG).read_text())
        self.assertEqual((config["panel_url"], config["token"], config["node_id"]), (panel, TOKEN, 41))
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        self.assertEqual((self.control()["active"], self.control()["enabled"]), (False, True))
        self.assertNotIn("start", [call["args"][0] for call in self.calls()])
        self.assertEqual(self.curl_calls(), [], "local prebuilt installs should keep working offline")

    def test_long_panel_aliases_and_token_are_accepted(self):
        for option in ("--api", "--panel", "--panel-url"):
            with self.subTest(option=option):
                self.install(option, "https://panel.example.invalid", "--token", TOKEN,
                             "--node-id", "41", "--skip-start")
                config = json.loads(self.path(CONFIG).read_text())
                self.assertEqual((config["panel_url"], config["token"], config["node_id"]),
                                 ("https://panel.example.invalid", TOKEN, 41))
        self.assertEqual(self.curl_calls(), [])

    def test_repeat_identity_parameters_preserve_config_bytes_and_inode(self):
        self.seed_old()
        inode = self.path(CONFIG).stat().st_ino
        for _ in range(2):
            self.install("-a", "https://panel.example.invalid", "-t", TOKEN, "-n", "41", "--skip-start")
            self.assertEqual(self.path(CONFIG).read_bytes(), self.imported)
            self.assertEqual(self.path(CONFIG).stat().st_ino, inode)
            self.assertEqual(self.path(REPORTS).read_bytes(), REPORT_BYTES)
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)

    def test_matching_individual_identity_parameters_preserve_config(self):
        self.seed_old()
        inode = self.path(CONFIG).stat().st_ino
        for args in (("-a", "https://panel.example.invalid"), ("-t", TOKEN), ("-n", "41")):
            with self.subTest(option=args[0]):
                self.install(*args, "--skip-start")
                self.assertEqual(self.path(CONFIG).read_bytes(), self.imported)
                self.assertEqual(self.path(CONFIG).stat().st_ino, inode)
                self.assertEqual(self.path(REPORTS).read_bytes(), REPORT_BYTES)

    def test_mismatching_individual_identity_parameters_preserve_old_install(self):
        self.seed_old()
        for args in (("-a", "http://different.example.invalid"),
                     ("-t", TOKEN + "-different"), ("-n", "42")):
            with self.subTest(option=args[0]):
                self.assert_preflight_failure(*args, expected="Configuration already exists")

    def test_token_file_conflicts_with_inline_token_before_changes(self):
        self.seed_old()
        for option in ("-t", "--token"):
            with self.subTest(option=option):
                output = self.assert_preflight_failure(option, TOKEN, "--token-file", "/input/token",
                                                       expected="--token")
                self.assertIn("--token-file", output)
                self.assertFalse(self.path("/run/accounts.log").exists())
        self.assert_preflight_failure("--config", "/input/config.json", "-t", TOKEN, expected="--config")

    def test_certificate_options_explain_snell_without_leaking_values_or_changes(self):
        self.seed_old()
        secret = "certificate-value-private-$(touch /tmp/cert-executed)"
        cases = [(option, secret) for option in
                 ("--cert-file", "--cert-key", "--cert-domain", "--cert-mode", "--cert-anything")]
        cases.append(("--cert-file=" + secret,))
        for args in cases:
            with self.subTest(option=args[0].split("=", 1)[0]):
                output = self.install("-t", TOKEN, *args, success=False)
                self.assertIn("Snell", output)
                self.assertRegex(output.lower(), r"(?:does not|doesn't|not needed|not use|no|without).*cert")
                self.assertNotIn("certificate-value-private", output)
                self.assertFalse(self.path("/tmp/cert-executed").exists())
                self.assert_old()
                self.assertEqual(self.calls(), [])
                self.assertEqual(self.curl_calls(), [])
                self.assertFalse(self.path("/run/accounts.log").exists())
                self.assertFalse(self.path("/run/snell-agent-installer").exists())
                self.assertFalse(self.path("/var/backups/snell-agent").exists())
                self.assertFalse(list(self.path("/tmp").iterdir()))

    def test_stream_discovers_complete_snell_release_instead_of_generic_latest(self):
        releases = [self.release("v99.0.0"), self.release("snell-v9.0.0", draft=True),
                    self.release("snell-v8.0.0", assets=["SHA256SUMS"]),
                    self.release("snell-v7.0.0", assets=[RELEASE_ASSET]),
                    self.release("snell-v6.0.0", assets=["snell-agent-linux-arm64", "SHA256SUMS"]),
                    self.release()]
        binary_url, manifest_url = self.release_fixture([releases])
        self.install("-a", "http://panel.example.invalid", "-t", TOKEN, "-n", "41",
                     "--skip-start", stream=True)
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        config = json.loads(self.path(CONFIG).read_text())
        self.assertEqual((config["panel_url"], config["node_id"], config["token"]),
                         ("http://panel.example.invalid", 41, TOKEN))
        self.assertCountEqual(self.curl_urls(), [self.api_url(), binary_url, manifest_url])
        self.assertEqual(self.curl_urls()[0], self.api_url())
        self.assertNotIn(TOKEN, self.path("/run/curl.log").read_text())
        self.assertEqual((self.control()["active"], self.control()["enabled"]), (False, True))

    def test_stream_finds_complete_release_on_second_page(self):
        first_page = [self.release("v2.0.%d" % index) for index in range(100)]
        binary_url, manifest_url = self.release_fixture([first_page, [self.release()]])
        self.install("-a", "https://panel.example.invalid", "-t", TOKEN, "-n", "41",
                     "--skip-start", stream=True)
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        self.assertEqual(self.curl_urls()[:2], [self.api_url(1), self.api_url(2)])
        self.assertCountEqual(self.curl_urls()[2:], [binary_url, manifest_url])

    def test_explicit_version_uses_pinned_assets_without_release_api(self):
        tag = "snell-v0.8.7"
        binary_url, manifest_url = self.release_fixture(tag=tag)
        self.install("--version", tag, "--config", "/input/config.json", "--skip-start")
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        self.assertEqual(self.path(CONFIG).read_bytes(), self.imported)
        self.assertCountEqual(self.curl_urls(), [binary_url, manifest_url])

    def test_stream_custom_release_repo_uses_its_api_and_assets(self):
        repo = "fixture-owner/snell-releases"
        binary_url, manifest_url = self.release_fixture(repo=repo)
        self.install("--release-repo", repo, "--config", "/input/config.json", "--skip-start", stream=True)
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        self.assertCountEqual(self.curl_urls(), [self.api_url(repo=repo), binary_url, manifest_url])

    def test_fixed_version_and_custom_repo_use_same_tag_for_both_assets(self):
        repo, tag = "fixture-owner/snell-releases", "snell-v0.9.8"
        binary_url, manifest_url = self.release_fixture(repo=repo, tag=tag)
        self.install("--release-repo", repo, "--version", tag,
                     "--config", "/input/config.json", "--skip-start", stream=True)
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)
        self.assertCountEqual(self.curl_urls(), [binary_url, manifest_url])

    def test_release_checksum_mismatch_fails_before_stop(self):
        self.seed_old()
        self.release_fixture(manifest="0" * 64 + "  " + RELEASE_ASSET + "\n")
        output = self.install(stream=True, success=False)
        self.assertIn("Binary SHA256 mismatch", output)
        self.assert_old()
        self.assertEqual(self.calls(), [])
        self.assertFalse(self.path("/var/backups/snell-agent").exists())

    def test_manifest_requires_one_exact_asset_with_64_hex_digest(self):
        self.seed_old()
        valid = self.agent_hash + "  " + RELEASE_ASSET + "\n"
        cases = {"duplicate": valid + valid,
                 "short hash": self.agent_hash[:-1] + "  " + RELEASE_ASSET + "\n",
                 "long hash": self.agent_hash + "0  " + RELEASE_ASSET + "\n",
                 "non hex": "g" * 64 + "  " + RELEASE_ASSET + "\n",
                 "suffix only": self.agent_hash + "  " + RELEASE_ASSET + ".bak\n",
                 "prefix only": self.agent_hash + "  prefix-" + RELEASE_ASSET + "\n",
                 "wrong arch": self.agent_hash + "  snell-agent-linux-arm64\n"}
        for label, manifest in cases.items():
            with self.subTest(manifest=label):
                self.release_fixture(manifest=manifest)
                output = self.install("--version", RELEASE_TAG, stream=True, success=False)
                self.assertRegex(output.lower(), r"sha256|checksum|manifest")
                self.assert_old()
                self.assertEqual(self.calls(), [])
                self.assertFalse(self.path("/var/backups/snell-agent").exists())

    def test_manifest_ignores_other_assets_when_exact_entry_is_valid(self):
        manifest = ("0" * 64 + "  " + RELEASE_ASSET + ".bak\n"
                    + self.agent_hash + "  " + RELEASE_ASSET + "\n"
                    + "0" * 64 + "  snell-agent-linux-arm64\n")
        self.release_fixture(manifest=manifest)
        self.install("--version", RELEASE_TAG, "--config", "/input/config.json",
                     "--skip-start", stream=True)
        self.assertEqual(self.path(BINARY).read_bytes(), self.agent_bytes)

    def test_stream_empty_releases_leave_no_installation(self):
        self.release_fixture(pages=[])
        output = self.install("--config", "/input/config.json", stream=True, success=False)
        self.assertRegex(output.lower(), r"(?:release|assets|resources).*(?:found|missing|complete|available)")
        urls = self.curl_urls()
        self.assertGreaterEqual(len(urls), 1)
        self.assertLessEqual(len(urls), 5)
        self.assertEqual(urls, [self.api_url(page) for page in range(1, len(urls) + 1)])
        self.assert_no_installation()

    def test_stream_incomplete_releases_leave_no_installation(self):
        releases = [self.release("snell-v9.0.0", assets=[RELEASE_ASSET]),
                    self.release("snell-v8.0.0", assets=["SHA256SUMS"])]
        self.release_fixture(pages=[releases])
        output = self.install("--config", "/input/config.json", stream=True, success=False)
        self.assertRegex(output.lower(), r"(?:release|assets|resources).*(?:found|missing|complete|available)")
        self.assertTrue(all(url.startswith("https://api.github.com/") for url in self.curl_urls()))
        self.assert_no_installation()

    def test_stream_release_discovery_stops_after_five_pages(self):
        pages = [[self.release("v%d.0.%d" % (page, index)) for index in range(100)]
                 for page in range(1, 6)]
        self.release_fixture(pages=pages)
        output = self.install("--config", "/input/config.json", stream=True, success=False)
        self.assertRegex(output.lower(), r"(?:release|assets|resources).*(?:found|missing|complete|available)")
        self.assertEqual(self.curl_urls(), [self.api_url(page) for page in range(1, 6)])
        self.assert_no_installation()

    def test_check_config_never_creates_or_mutates_report_state(self):
        for directory in (STATE, "/tmp/nonexistent-state"):
            with self.subTest(directory=directory):
                put(self.root, "/input/check.json", config_bytes(state_dir=directory), 0o600)
                result = self.run_chroot("/workspace/bin/snell-agent-linux-amd64", "-check-config", "-config", "/input/check.json")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertFalse(self.path(directory).exists())
        put(self.root, REPORTS, REPORT_BYTES, 0o600)
        before = self.path(REPORTS).stat()
        result = self.run_chroot("/workspace/bin/snell-agent-linux-amd64", "-check-config", "-config", "/input/config.json")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.path(REPORTS).read_bytes(), REPORT_BYTES)
        after = self.path(REPORTS).stat()
        self.assertEqual((before.st_ino, before.st_mtime_ns, before.st_ctime_ns),
                         (after.st_ino, after.st_mtime_ns, after.st_ctime_ns))

    def test_embedded_unit_matches_deploy_template(self):
        script = (self.base / "workspace/install.sh").read_text()
        match = re.search(r"<<'SERVICE'\n(.*?)\nSERVICE(?:\n|$)", script, re.S)
        self.assertIsNotNone(match, "installer embedded unit is missing")
        self.assertEqual((match.group(1) + "\n").encode(), self.unit_bytes)

    def test_tty_prompts_hide_token_and_create_valid_config(self):
        master, slave = pty.openpty()
        self.addCleanup(os.close, master)
        self.addCleanup(os.close, slave)

        def terminal_session():
            os.setsid()
            fcntl.ioctl(slave, termios.TIOCSCTTY, 0)

        process = subprocess.Popen([self.chroot, str(self.root), "/bin/bash", "/workspace/install.sh", "--no-start"],
                                   stdin=slave, stdout=slave, stderr=slave, preexec_fn=terminal_session,
                                   env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C", "PYTHONUTF8": "1",
                                        "PYTHONDONTWRITEBYTECODE": "1"})
        self.addCleanup(lambda: process.wait() if process.poll() is not None else (process.kill(), process.wait()))
        transcript = bytearray()
        deadline = time.monotonic() + 30
        for prompt, answer in ((b"Panel root URL", b"https://panel.example.invalid"),
                               (b"numeric ID", b"41"), (b"token (hidden)", TOKEN.encode())):
            while prompt not in transcript and time.monotonic() < deadline:
                if select.select([master], [], [], 0.1)[0]:
                    transcript.extend(os.read(master, 65536))
                if process.poll() is not None:
                    break
            self.assertIn(prompt, transcript, transcript.decode(errors="replace"))
            os.write(master, answer + b"\n")
        while process.poll() is None and time.monotonic() < deadline:
            if select.select([master], [], [], 0.1)[0]:
                transcript.extend(os.read(master, 65536))
        self.assertEqual(process.wait(timeout=2), 0, transcript.decode(errors="replace"))
        self.assertNotIn(TOKEN.encode(), transcript)
        self.assertEqual(json.loads(self.path(CONFIG).read_text())["token"], TOKEN)
        self.assertFalse(self.path("/tmp/token-executed").exists())
        self.assertFalse(self.path("/tmp/token-backtick").exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
