# -*- coding: utf-8 -*-
"""
云端账号一体化插件（纯 Go 版）：自动启动 Go 桥（Windows/Linux 双端）
- 无 Python 依赖：桥是 Go 编译的独立二进制（毫秒启动/零依赖）
- 自动识别环境：Windows 用 cloud_bridge_win.exe，Linux 用 cloud_bridge_linux
- 持续看护：每 20 秒 HTTP 探测 /__bridge_status，掉线/挂起自动拉起
- 日志：logs/cloud_bridge_go.log（桥 stderr）+ logs/cloud_bridge.log（插件事件）
"""
import os
import subprocess
import threading
import time

TOOLS = os.path.dirname(os.path.abspath(__file__))
LOG_FILE = os.path.join(TOOLS, 'logs', 'cloud_bridge.log')
PORT = 59000


def _log(msg):
    try:
        with open(LOG_FILE, 'a', encoding='utf-8') as f:
            f.write(f'[plugin] {time.strftime("%H:%M:%S")} {msg}\n')
    except Exception:
        pass


def _bridge_exe():
    """按环境选 Go 桥二进制"""
    exe = os.path.join(TOOLS, 'bridge', 'cloud_bridge_win.exe' if os.name == 'nt' else 'cloud_bridge_linux')
    if os.name != 'nt' and os.path.exists(exe):
        try:
            os.chmod(exe, 0o755)  # Linux 需要执行权限
        except Exception:
            pass
    return exe


def _is_port_alive():
    """HTTP 探测桥（/__bridge_status 200 才算活）"""
    import socket
    import urllib.request
    try:
        s = socket.socket()
        s.settimeout(1.5)
        if s.connect_ex(('127.0.0.1', PORT)) != 0:
            s.close()
            return False
        s.close()
        urllib.request.urlopen(f'http://127.0.0.1:{PORT}/__bridge_status', timeout=3).read(8)
        return True
    except Exception:
        return False


def _kill_bridge():
    """杀旧桥进程（按进程名，双端）"""
    try:
        if os.name == 'nt':
            subprocess.run('powershell -Command "Get-Process cloud_bridge_win -ErrorAction SilentlyContinue | Stop-Process -Force"',
                           shell=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        else:
            subprocess.run(['pkill', '-f', 'cloud_bridge_linux'], capture_output=True)
    except Exception:
        pass


def _start_bridge():
    if _is_port_alive():
        _log('桥已运行且 HTTP 探测正常，跳过重启')
        return
    _kill_bridge()
    time.sleep(1)
    exe = _bridge_exe()
    if not os.path.exists(exe):
        _log(f'Go 桥二进制缺失: {exe}')
        return
    oauth_dir = os.path.join(TOOLS, 'data')
    oauth_file = ''
    # 优先选【有 app_id 的实例账号】作主 oauth——空实例账号排在前面会导致 base_url 为空、代理全废
    import json as _json
    try:
        for fn in sorted(os.listdir(oauth_dir)):
            if not (fn.startswith('cloud_oauth') and fn.endswith('.json')):
                continue
            fp = os.path.join(oauth_dir, fn)
            try:
                with open(fp, encoding='utf-8') as f:
                    o = _json.load(f)
                if o.get('app_id'):
                    oauth_file = fp
                    break
            except Exception:
                continue
        if not oauth_file:
            for fn in sorted(os.listdir(oauth_dir)):
                if fn.startswith('cloud_oauth') and fn.endswith('.json'):
                    oauth_file = os.path.join(oauth_dir, fn)
                    break
    except Exception:
        pass
    args = [exe, '--listen', f'127.0.0.1:{PORT}', '--oauth-dir', oauth_dir]
    if oauth_file:
        args += ['--oauth', oauth_file]
    try:
        _go_log = open(os.path.join(TOOLS, 'logs', 'cloud_bridge_go.log'), 'a', encoding='utf-8', errors='ignore')
        proc = subprocess.Popen(args, stdout=_go_log, stderr=_go_log,
                                creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
        _log(f'Go 桥已启动 PID={proc.pid} exe={os.path.basename(exe)}（日志: logs/cloud_bridge_go.log）')
    except Exception as e:
        _log(f'Go 桥启动失败: {e}')


class CloudMultiAccountPlugin:
    def register(self, api):
        def _auto_start():
            time.sleep(3)
            try:
                _start_bridge()
                _log('纯 Go 插件自动启动完成')
            except Exception as e:
                _log(f'插件启动异常: {e}')
            # 持续看护
            while True:
                time.sleep(20)
                try:
                    if not _is_port_alive():
                        _log('⚠️ 桥掉线/挂起检测 → 自动拉起')
                        _start_bridge()
                except Exception as e:
                    _log(f'看护检查异常: {e}')

        threading.Thread(target=_auto_start, daemon=True).start()
        _log('纯 Go 插件已注册，3 秒后启动 Go 桥')


plugin = CloudMultiAccountPlugin()
