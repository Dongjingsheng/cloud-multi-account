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
import io

TOOLS = os.path.dirname(os.path.abspath(__file__))
LOGS_DIR = os.path.join(TOOLS, 'logs')
DATA_DIR = os.path.join(TOOLS, 'data')
LOG_FILE = os.path.join(LOGS_DIR, 'cloud_bridge.log')
PORT = 59000
# 桥监听地址：0.0.0.0 = 本机 + 远程都可访问（云端实例经域名访问面板时需要）；可用环境变量覆盖
LISTEN = os.environ.get('CLOUD_BRIDGE_LISTEN', f'0.0.0.0:{PORT}')


def _ensure_dirs():
    """确保 logs/ 和 data/ 目录存在（插件目录被重建/重装后可能缺失，否则启动桥会静默失败）"""
    try:
        os.makedirs(LOGS_DIR, exist_ok=True)
        os.makedirs(DATA_DIR, exist_ok=True)
    except Exception:
        pass


_ensure_dirs()


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
    args = [exe, '--listen', LISTEN, '--oauth-dir', oauth_dir]
    if oauth_file:
        args += ['--oauth', oauth_file]
    try:
        _go_log = open(os.path.join(TOOLS, 'logs', 'cloud_bridge_go.log'), 'a', encoding='utf-8', errors='ignore')
        proc = subprocess.Popen(args, stdout=_go_log, stderr=_go_log,
                                creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
        _log(f'Go 桥已启动 PID={proc.pid} exe={os.path.basename(exe)}（日志: logs/cloud_bridge_go.log）')
    except Exception as e:
        _log(f'Go 桥启动失败: {e}')


# 记忆实时同步守护（跟随 QwenPaw 生命周期：与桥同规则——启动拉起/掉线自愈/退出跟随）
# 注意：用 python.exe + CREATE_NO_WINDOW（pythonw 无 stdout 会导致守护异常退出）
MEMORY_LIVE_PY = r'C:\Users\74-D\.qwenpaw\workspaces\default\tools\memory_live.py'
MEMORY_LIVE_PYEXE = r'D:\soft\Python310\python.exe'
ML_PID_FILE = os.path.join(DATA_DIR, '_memory_live.pid')


def _memory_live_running():
    """检查 PID 文件记录的 memory_live 守护进程是否存活（精确，避免误判反复杀启）"""
    try:
        if not os.path.exists(ML_PID_FILE):
            return False
        pid = int(io.open(ML_PID_FILE, encoding='utf-8').read().strip())
        if os.name == 'nt':
            r = subprocess.run(f'tasklist /FI "PID eq {pid}"', shell=True, capture_output=True, timeout=10,
                               creationflags=subprocess.CREATE_NO_WINDOW)
            txt = r.stdout.decode('gbk', errors='ignore')
            return f'{pid}' in txt and 'python' in txt
        try:
            os.kill(pid, 0)
            return True
        except Exception:
            return False
    except Exception:
        return False


def _start_memory_live():
    """启动记忆同步守护（python 无窗口）；已存活则不重复启动；启动后写 PID 文件"""
    try:
        if _memory_live_running():
            return
        p = subprocess.Popen([MEMORY_LIVE_PYEXE, MEMORY_LIVE_PY, 'start'],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             creationflags=subprocess.CREATE_NO_WINDOW)
        time.sleep(1)
        if p.pid and p.poll() is None:
            try:
                io.open(ML_PID_FILE, 'w', encoding='utf-8').write(str(p.pid))
            except Exception:
                pass
            _log(f'memory_live 已启动（PID {p.pid}，python 无窗口）')
        else:
            _log('memory_live 启动后立即退出（异常）')
    except Exception as e:
        _log(f'memory_live 启动失败: {e}')


def _kill_memory_live():
    """停止记忆同步守护（按 PID 文件 + 命令行兜底）"""
    try:
        if os.path.exists(ML_PID_FILE):
            try:
                pid = int(io.open(ML_PID_FILE, encoding='utf-8').read().strip())
                subprocess.run(f'taskkill /F /PID {pid}', shell=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            except Exception:
                pass
        if os.name == 'nt':
            ps = ("Get-CimInstance Win32_Process -Filter \"Name='python.exe' or Name='pythonw.exe'\" | "
                  "Where-Object { $_.CommandLine -like '*memory_live.py start*' } | "
                  "ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }")
            subprocess.run(['powershell', '-Command', ps], shell=False,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
        else:
            subprocess.run(['pkill', '-f', 'memory_live.py'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            os.remove(ML_PID_FILE)
        except Exception:
            pass
    except Exception:
        pass


def _qwenpaw_running():
    """检测 QwenPaw Desktop 是否还在运行（Windows/Linux）。看护线程据此跟随 QwenPaw 退出。"""
    try:
        if os.name == 'nt':
            r = subprocess.run('tasklist', capture_output=True, timeout=10,
                               creationflags=subprocess.CREATE_NO_WINDOW)
            return (b'qwenpaw-desktop.exe' in r.stdout) or (b'qwenpaw-backend.exe' in r.stdout)
        r = subprocess.run(['pgrep', '-f', 'qwenpaw'], capture_output=True, timeout=10)
        return r.returncode == 0
    except Exception:
        return True  # 无法判断时保守不杀


class CloudMultiAccountPlugin:
    def register(self, api):
        def _auto_start():
            time.sleep(3)
            try:
                _start_bridge()
                _start_memory_live()
                _log('纯 Go 插件自动启动完成（桥 + 记忆同步）')
            except Exception as e:
                _log(f'插件启动异常: {e}')
            # 持续看护：掉线自动拉起 + QwenPaw 退出时跟随停止
            while True:
                time.sleep(20)
                try:
                    if not _qwenpaw_running():
                        _log('检测到 QwenPaw 已退出 → 停止桥和记忆同步（跟随关闭）')
                        _kill_bridge()
                        _kill_memory_live()
                        return
                    if not _is_port_alive():
                        _log('⚠️ 桥掉线/挂起检测 → 自动拉起')
                        _start_bridge()
                    if not _memory_live_running():
                        _log('⚠️ 记忆同步守护掉线 → 自动拉起')
                        _start_memory_live()
                except Exception as e:
                    _log(f'看护检查异常: {e}')

        threading.Thread(target=_auto_start, daemon=True).start()
        # 跟随 QwenPaw 关闭：注册关闭钩子（正确签名 register_shutdown_hook(hook_name, callback, priority)）
        try:
            if hasattr(api, 'register_shutdown_hook'):
                def _stop_all():
                    _kill_bridge()
                    _kill_memory_live()
                api.register_shutdown_hook('cloud_bridge_stop', _stop_all)
                _log('已注册关闭钩子：QwenPaw 退出时自动停止桥和记忆同步')
            else:
                _log('PluginApi 无 register_shutdown_hook，跳过关闭钩子（看护线程会兜底跟随退出）')
        except Exception as e:
            _log(f'注册关闭钩子失败（忽略，看护线程兜底）: {e}')
        _log('纯 Go 插件已注册，3 秒后启动 Go 桥 + 记忆同步')


plugin = CloudMultiAccountPlugin()
