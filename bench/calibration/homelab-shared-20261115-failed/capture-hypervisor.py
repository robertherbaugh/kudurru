"""Read-only physical host/VM configuration and contention snapshot."""
import datetime
import json
from pathlib import Path
import socket
import subprocess

config = dict(line.split(': ', 1) for line in subprocess.check_output(
    ['qm', 'config', '20027'], text=True).splitlines() if ': ' in line)
keys = ('name', 'affinity', 'cores', 'sockets', 'cpu', 'memory', 'balloon', 'scsi0', 'args')
paths = ('/proc/stat', '/proc/loadavg', '/proc/pressure/cpu', '/proc/pressure/io')
print(json.dumps({'at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'node': socket.gethostname(), 'vm': {key: config[key] for key in keys},
    'counters': {name: Path(name).read_text() for name in paths if Path(name).exists()}}, indent=2))
