"""Exercise downloads and service lifecycle using isolated command fixtures."""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
BASH = shutil.which('bash')
if not BASH and os.name == 'nt' and shutil.which('git'):
    BASH = str(Path(shutil.which('git')).resolve().parents[1] / 'bin' / 'bash.exe')


def bash_path(path):
    value = Path(path).resolve().as_posix()
    if os.name == 'nt':
        return '/' + value[0].lower() + value[2:]
    return value


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.bin = self.base / 'bin'
        self.bin.mkdir()
        self.target = self.base / 'deployment'
        self.calls = self.base / 'calls'
        self.asset = self.base / 'asset'
        self.asset.write_bytes(b'#!/bin/sh\necho easy-proxies\n')
        self.checksum = self.base / 'checksum'
        self.checksum.write_text(hashlib.sha256(self.asset.read_bytes()).hexdigest() + '  binary\n')
        self.env = dict(os.environ, FIXTURES=bash_path(self.base), PROJECT=bash_path(ROOT), TEST_INIT='systemd')
        self.systemd_file = self.base / 'systemd' / 'easy-proxies.service'
        self.openrc_file = self.base / 'init.d' / 'easy-proxies'
        (self.base / 'softlevel').write_text('default\n')
        self.mock('id', 'echo "${TEST_UID:-0}"')
        self.mock('mv', r'''
if [[ "${FAIL_UNIT_RENAME:-0}" == 1 && "${@: -1}" == */easy-proxies.service ]]; then exit 1; fi
exec /usr/bin/mv "$@"
''')
        self.mock('uname', '[[ "$1" == -s ]] && echo Linux || echo "${TEST_ARCH:-x86_64}"')
        self.mock('curl', '''
printf '%s\n' "$*" >> "$FIXTURES/calls"
output=
url=
while (($#)); do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --write-out) shift 2 ;;
    https://*) url=$1; shift ;;
    --retry) shift 2 ;;
    *) shift ;;
  esac
done
case "$url" in
  */releases/latest) printf 'https://github.com/x0rz4/EasyProxiesV2/releases/tag/v2.1.0' ;;
  */config.example.yaml) cp "$PROJECT/config.example.yaml" "$output" ;;
  *.sha256) cp "$FIXTURES/checksum" "$output" ;;
  */easy-proxies-linux-*) cp "$FIXTURES/asset" "$output" ;;
  *) exit 9 ;;
esac
''')
        self.mock('systemctl', r'''
printf 'systemctl %s\n' "$*" >> "$FIXTURES/calls"
case "$1" in
  show-environment) [[ "${TEST_INIT:-systemd}" == systemd ]] ;;
  enable)
    [[ "${FAIL_ENABLE:-0}" != 1 ]] || exit 1
    if [[ "${NO_ENABLE_EFFECT:-0}" != 1 ]]; then touch "$FIXTURES/enabled"; fi ;;
  disable) rm -f "$FIXTURES/enabled" ;;
  is-enabled) [[ -f "$FIXTURES/enabled" ]] ;;
  start|restart) [[ "${FAIL_START:-0}" != 1 ]] && touch "$FIXTURES/running" ;;
  stop) rm -f "$FIXTURES/running" ;;
  is-active|--no-pager) [[ -f "$FIXTURES/running" && "${FAIL_ACTIVE:-0}" != 1 ]] ;;
  daemon-reload) exit 0 ;;
  *) exit 9 ;;
esac
''')
        self.mock('rc-status', '[[ "${TEST_INIT:-systemd}" == openrc ]] && echo default')
        self.mock('rc-service', r'''
printf 'rc-service %s\n' "$*" >> "$FIXTURES/calls"
case "$2" in
  start|restart) [[ "${FAIL_START:-0}" != 1 ]] && touch "$FIXTURES/running" ;;
  stop) rm -f "$FIXTURES/running" ;;
  status) [[ -f "$FIXTURES/running" && "${FAIL_ACTIVE:-0}" != 1 ]] ;;
  *) exit 9 ;;
esac
''')
        self.mock('rc-update', r'''
printf 'rc-update %s\n' "$*" >> "$FIXTURES/calls"
case "$1" in
  add) [[ "${FAIL_ENABLE:-0}" != 1 ]] && touch "$FIXTURES/enabled" ;;
  del) rm -f "$FIXTURES/enabled" ;;
  show) if [[ -f "$FIXTURES/enabled" ]]; then echo ' easy-proxies | default'; fi ;;
  *) exit 9 ;;
esac
''')
        self.mock('supervise-daemon', 'exit 0')
        self.mock('logger', 'exit 0')
        self.mock('docker', '''
printf 'docker %s\n' "$*" >> "$FIXTURES/calls"
case "$1" in
  pull) [[ "${FAIL_PULL:-0}" != 1 ]] ;;
  container) [[ "${EXISTING_CONTAINER:-0}" == 1 ]] ;;
  inspect) echo unrelated-owner ;;
  *) exit 0 ;;
esac
''')

    def mock(self, name, body):
        path = self.bin / name
        path.write_bytes(('#!/usr/bin/env bash\nset -eu\n' + body + '\n').encode())
        path.chmod(0o755)

    def run_install(self, *args, ok=True):
        # Source definitions only, then redirect service files into the fixture.
        # The public CLI still uses these exact main/parse/install functions.
        command = '''export PATH="$FIXTURES/bin:$PATH"
source "$PROJECT/install.sh"
SYSTEMD_DIR="$FIXTURES/systemd"
OPENRC_DIR="$FIXTURES/init.d"
OPENRC_STATE_FILE="$FIXTURES/softlevel"
main "$@"
'''
        result = subprocess.run([BASH or 'bash', '-c', command, '--', '--dir', bash_path(self.target), *args],
                                env=self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        return result

    def test_binary_latest_is_pinned_and_config_preserved_on_update(self):
        self.run_install('--no-start')
        self.assertEqual((self.target / 'installed-version').read_text(), 'v2.1.0\n')
        config = self.target / 'config.yaml'
        self.assertNotIn('password: "123456"', config.read_text())
        if os.name != 'nt':  # NTFS does not expose POSIX mode bits to Python.
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)
        self.assertTrue(os.access(self.target / 'easy-proxies', os.X_OK))
        config.write_text('custom config\n')
        (self.target / 'data' / 'data.db').write_text('existing database')
        self.run_install('--version', 'v2.1.0', '--no-start')
        self.assertEqual(config.read_text(), 'custom config\n')
        self.assertEqual((self.target / 'data' / 'data.db').read_text(), 'existing database')
        self.assertIn('/v2.1.0/easy-proxies-linux-amd64', self.calls.read_text())

    def test_arm64_asset(self):
        self.env['TEST_ARCH'] = 'aarch64'
        self.run_install('--version', '2.1.0', '--no-start')
        self.assertIn('/v2.1.0/easy-proxies-linux-arm64', self.calls.read_text())

    def test_bad_checksum_keeps_existing_binary_and_config(self):
        self.target.mkdir()
        (self.target / 'easy-proxies').write_text('old binary')
        (self.target / 'config.yaml').write_text('old config')
        self.checksum.write_text('0' * 64 + '  binary\n')
        self.run_install('--version', '2.1.0', '--no-start', ok=False)
        self.assertEqual((self.target / 'easy-proxies').read_text(), 'old binary')
        self.assertEqual((self.target / 'config.yaml').read_text(), 'old config')

    def test_docker_uses_release_version_and_persistent_mounts(self):
        self.run_install('--method', 'docker', '--version', 'v2.1.0')
        calls = self.calls.read_text()
        self.assertIn('docker pull ghcr.io/x0rz4/easyproxiesv2:2.1.0', calls)
        self.assertIn('docker run -d', calls)
        self.assertIn('--network host', calls)
        self.assertIn('dst=/etc/easy-proxies/data', calls)
        self.assertNotIn(':latest', calls)

    def test_docker_no_start_does_not_replace_container(self):
        self.run_install('--method', 'docker', '--version', '2.1.0', '--no-start')
        calls = self.calls.read_text()
        self.assertNotIn('docker rm', calls)
        self.assertNotIn('docker run', calls)

    def test_failed_pull_does_not_remove_container(self):
        self.env['FAIL_PULL'] = '1'
        self.run_install('--method', 'docker', '--version', '2.1.0', ok=False)
        self.assertNotIn('docker rm', self.calls.read_text())
        self.assertFalse((self.target / 'installed-version').exists())

    def test_prerelease_is_pinned(self):
        self.run_install('--method', 'docker', '--version', 'v2.1.0-rc.1', '--no-start')
        self.assertIn('docker pull ghcr.io/x0rz4/easyproxiesv2:2.1.0-rc.1', self.calls.read_text())

    def test_unmanaged_container_is_not_removed(self):
        self.env['EXISTING_CONTAINER'] = '1'
        result = self.run_install('--method', 'docker', '--version', '2.1.0', ok=False)
        self.assertIn('not managed by this installer', result.stderr)
        self.assertNotIn('docker rm', self.calls.read_text())

    def test_invalid_version_and_architecture(self):
        self.run_install('--version', '../main', '--no-start', ok=False)
        self.env['TEST_ARCH'] = 'riscv64'
        self.run_install('--version', '2.1.0', '--no-start', ok=False)

    def prepare_local_binary(self):
        self.target.mkdir(exist_ok=True)
        shutil.copyfile(self.asset, self.target / 'easy-proxies')
        (self.target / 'easy-proxies').chmod(0o755)
        (self.target / 'config.yaml').write_text('custom config\n')
        (self.target / 'data').mkdir(exist_ok=True)
        (self.target / 'data' / 'data.db').write_text('existing database')

    def test_install_registers_starts_and_enables_systemd(self):
        result = self.run_install('--version', '2.1.0')
        unit = self.systemd_file.read_text()
        self.assertIn(f'WorkingDirectory={bash_path(self.target)}', unit)
        self.assertIn(f'ExecStart={bash_path(self.target)}/easy-proxies --config {bash_path(self.target)}/config.yaml', unit)
        self.assertIn('TimeoutStopSec=45', unit)
        self.assertIn('Restart=on-failure', unit)
        calls = self.calls.read_text()
        self.assertLess(calls.index('systemctl daemon-reload'), calls.index('systemctl enable'))
        self.assertLess(calls.index('systemctl enable'), calls.index('systemctl restart'))
        self.assertIn('systemctl is-active --quiet easy-proxies', calls)
        self.assertIn('Boot autostart: enabled', result.stdout)

    def test_openrc_registration_and_service_lifecycle(self):
        self.env['TEST_INIT'] = 'openrc'
        self.prepare_local_binary()
        self.run_install('--version', '2.1.0')
        unit = self.openrc_file.read_text()
        self.assertIn('supervisor="supervise-daemon"', unit)
        self.assertIn('retry="TERM/45/KILL/5"', unit)
        self.assertIn('respawn_max=0', unit)
        self.assertIn(f'directory="{bash_path(self.target)}"', unit)
        parsed = subprocess.run([BASH or 'bash', '-n', bash_path(self.openrc_file)], capture_output=True, text=True)
        self.assertEqual(parsed.returncode, 0, parsed.stderr)
        for action in ('stop', 'start', 'restart', 'disable', 'enable', 'status'):
            self.run_install('--action', action)
        calls = self.calls.read_text()
        self.assertIn('rc-update add easy-proxies default', calls)
        self.assertIn('rc-update del easy-proxies default', calls)
        self.assertIn('rc-service easy-proxies restart', calls)
        self.assertNotIn('systemctl enable', calls)
        self.run_install('--action', 'unregister')
        self.assertFalse(self.openrc_file.exists())
        self.assertEqual((self.target / 'data' / 'data.db').read_text(), 'existing database')
        self.assertTrue((self.target / 'config.yaml').exists())

    def test_register_existing_binary_is_offline_and_idempotent(self):
        self.prepare_local_binary()
        self.run_install('--action', 'register', '--no-start')
        first = self.systemd_file.read_text()
        self.run_install('--action', 'register', '--no-start')
        self.assertEqual(first, self.systemd_file.read_text())
        calls = self.calls.read_text()
        self.assertNotIn('https://', calls)
        self.assertNotIn('systemctl restart', calls)
        self.assertIn('systemctl enable easy-proxies', calls)
        self.assertEqual((self.target / 'config.yaml').read_text(), 'custom config\n')
        self.assertEqual((self.target / 'data' / 'data.db').read_text(), 'existing database')

    def test_disable_and_unregister_preserve_installation(self):
        self.prepare_local_binary()
        self.run_install('--action', 'register')
        self.run_install('--action', 'disable')
        self.assertFalse((self.base / 'enabled').exists())
        self.assertTrue((self.base / 'running').exists())
        self.run_install('--action', 'unregister')
        self.run_install('--action', 'unregister')
        self.assertFalse(self.systemd_file.exists())
        self.assertFalse((self.base / 'running').exists())
        self.assertTrue((self.target / 'easy-proxies').exists())
        self.assertEqual((self.target / 'config.yaml').read_text(), 'custom config\n')
        self.assertEqual((self.target / 'data' / 'data.db').read_text(), 'existing database')

    def test_no_autostart_starts_without_enabling_boot(self):
        self.run_install('--version', '2.1.0', '--no-autostart')
        self.assertIn('systemctl disable easy-proxies', self.calls.read_text())
        self.assertTrue((self.base / 'running').exists())
        self.assertFalse((self.base / 'enabled').exists())

    def test_openrc_no_autostart_is_repeatable(self):
        self.env['TEST_INIT'] = 'openrc'
        self.prepare_local_binary()
        for _ in range(2):
            self.run_install('--action', 'register', '--no-start', '--no-autostart')
        self.assertFalse((self.base / 'enabled').exists())
        self.assertNotIn('rc-service easy-proxies restart', self.calls.read_text())

    def test_docker_no_autostart_sets_restart_policy(self):
        self.run_install('--method', 'docker', '--version', '2.1.0', '--no-autostart')
        self.assertIn('--restart no', self.calls.read_text())

    def test_no_start_never_changes_service_registration(self):
        self.env['TEST_INIT'] = 'none'
        self.env['TEST_UID'] = '1000'
        self.run_install('--version', '2.1.0', '--no-start')
        self.assertFalse(self.systemd_file.exists())
        self.assertFalse(self.openrc_file.exists())
        self.assertNotIn('systemctl', self.calls.read_text())

    def test_missing_local_files_do_not_register_service(self):
        self.target.mkdir()
        self.run_install('--action', 'register', ok=False)
        self.assertFalse(self.systemd_file.exists())
        self.prepare_local_binary()
        (self.target / 'config.yaml').write_text('')
        self.run_install('--action', 'register', ok=False)
        self.assertFalse(self.systemd_file.exists())

    def test_unmanaged_service_is_not_overwritten_or_downloaded(self):
        self.systemd_file.parent.mkdir()
        self.systemd_file.write_text('[Service]\nExecStart=/some/other/program\n')
        self.run_install('--version', '2.1.0', ok=False)
        self.assertIn('/some/other/program', self.systemd_file.read_text())
        self.assertNotIn('https://', self.calls.read_text())
        self.assertFalse((self.target / 'easy-proxies').exists())

    def test_registration_does_not_retarget_another_directory(self):
        self.prepare_local_binary()
        self.systemd_file.parent.mkdir()
        self.systemd_file.write_text('# Managed by x0rz4/EasyProxiesV2 installer\n# Install directory: /different/install\n')
        self.run_install('--action', 'register', ok=False)
        self.assertIn('/different/install', self.systemd_file.read_text())

    def test_legacy_unit_is_upgraded_in_place(self):
        self.prepare_local_binary()
        self.systemd_file.parent.mkdir()
        directory = bash_path(self.target)
        self.systemd_file.write_bytes(f'Description=Easy Proxies\nWorkingDirectory={directory}\nExecStart={directory}/easy-proxies --config {directory}/config.yaml\n'.encode())
        self.run_install('--action', 'register')
        self.assertIn('# Managed by x0rz4/EasyProxiesV2 installer', self.systemd_file.read_text())

    def test_nonroot_and_missing_init_fail_before_download(self):
        self.env['TEST_UID'] = '1000'
        self.run_install('--version', '2.1.0', ok=False)
        self.assertFalse(self.target.exists())
        self.env['TEST_UID'] = '0'
        self.env['TEST_INIT'] = 'none'
        self.run_install('--version', '2.1.0', ok=False)
        self.assertFalse(self.target.exists())
        self.assertNotIn('https://', self.calls.read_text())

    def test_start_failure_reports_status_without_success_marker(self):
        self.env['FAIL_START'] = '1'
        result = self.run_install('--version', '2.1.0', ok=False)
        self.assertIn('systemctl --no-pager --full status easy-proxies', self.calls.read_text())
        self.assertIn('journalctl', result.stderr)
        self.assertNotIn('Installed v', result.stdout)
        self.assertFalse((self.target / 'installed-version').exists())

    def test_enable_is_verified_and_failure_does_not_start(self):
        self.env['NO_ENABLE_EFFECT'] = '1'
        self.run_install('--version', '2.1.0', ok=False)
        self.assertNotIn('systemctl restart', self.calls.read_text())
        self.assertFalse((self.target / 'installed-version').exists())

    def test_status_is_offline_and_read_only_for_nonroot(self):
        self.env['TEST_UID'] = '1000'
        self.run_install('--action', 'status', ok=False)
        self.assertFalse(self.target.exists())
        self.assertNotIn('https://', self.calls.read_text())
        self.assertNotIn('systemctl enable', self.calls.read_text())

    def test_invalid_service_options_and_unsafe_paths(self):
        for args in (('--action', 'unknown'), ('--service-manager', 'invalid'),
                     ('--method', 'docker', '--action', 'register'), ('--action', 'stop', '--no-start'),
                     ('--dir', '/tmp/bad%path', '--no-start')):
            self.run_install(*args, ok=False)
        self.assertFalse(self.target.exists())

    def test_public_help_and_syntax(self):
        for args in (['-n', bash_path(ROOT / 'install.sh')], [bash_path(ROOT / 'install.sh'), '--help']):
            result = subprocess.run([BASH or 'bash', *args], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_openrc_package_without_boot_state_is_rejected(self):
        self.env['TEST_INIT'] = 'openrc'
        (self.base / 'softlevel').unlink()
        self.run_install('--version', '2.1.0', ok=False)
        self.assertFalse(self.target.exists())

    def test_systemd_start_stop_restart_and_status(self):
        self.prepare_local_binary()
        self.run_install('--action', 'register', '--no-start')
        self.run_install('--action', 'start')
        self.run_install('--action', 'status')
        self.run_install('--action', 'stop')
        self.run_install('--action', 'status', ok=False)
        self.run_install('--action', 'restart')
        self.assertTrue((self.base / 'running').exists())
        self.assertNotIn('https://', self.calls.read_text())

    def test_service_rename_failure_keeps_old_unit_and_cleans_staging(self):
        self.prepare_local_binary()
        self.run_install('--action', 'register', '--no-start')
        before = self.systemd_file.read_bytes()
        self.env['FAIL_UNIT_RENAME'] = '1'
        self.run_install('--action', 'register', ok=False)
        self.assertEqual(before, self.systemd_file.read_bytes())
        self.assertEqual(list(self.systemd_file.parent.glob('.easy-proxies.*')), [])

    @unittest.skipUnless(os.name != 'nt' and shutil.which('systemd-analyze'),
                         'systemd-analyze is only available on Linux test hosts')
    def test_generated_unit_with_systemd_analyze(self):
        self.run_install('--version', '2.1.0')
        result = subprocess.run(['systemd-analyze', 'verify', str(self.systemd_file)],
                                text=True, capture_output=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
