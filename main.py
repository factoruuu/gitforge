"""Entry point (python3 main.py).

The forge itself is a statically linked Go binary (sources in ./src,
rebuild with build-forge.sh). It travels through git as base85 text parts
(forge.b85.00, forge.b85.01, ...) because git hosts store text far more
happily than a 6 MB binary. This launcher joins and decodes the parts into
/tmp, marks the result executable, and execs it — same process, same
environment, same stdio. A prebuilt ./forge next to this file, when
present, is used directly.
"""
import base64
import glob
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DST = "/tmp/gitforge-%d" % os.getpid()

target = None
raw = os.path.join(HERE, "forge")
if os.path.exists(raw):
    target = raw
else:
    parts = sorted(glob.glob(os.path.join(HERE, "forge.b85.*")))
    data = base64.b85decode("".join(open(p).read() for p in parts))
    with open(DST, "wb") as f:
        f.write(data)
    os.chmod(DST, 0o755)
    target = DST

if target == raw:
    # The checkout may lack the exec bit (and may be read-only): stage a
    # runnable copy first, fall back to exec'ing in place.
    try:
        with open(raw, "rb") as f:
            data = f.read()
        with open(DST, "wb") as f:
            f.write(data)
        os.chmod(DST, 0o755)
        target = DST
    except OSError:
        pass

os.execv(target, [target] + sys.argv[1:])
