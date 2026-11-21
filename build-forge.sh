#!/bin/sh
# Rebuild the forge binary from src/ and re-split it into base85 parts.
# Requires a Go toolchain (go 1.23+) on PATH. Not run by the pod: the pod
# only runs main.py, which decodes the committed parts.
set -e
cd "$(dirname "$0")"
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o forge ./src
python3 - <<'EOF'
import base64, glob, os
data = open("forge", "rb").read()
enc = base64.b85encode(data).decode()
for old in glob.glob("forge.b85.*"):
    os.remove(old)
step = 1_000_000
for i in range(0, len(enc), step):
    with open("forge.b85.%02d" % (i // step), "w") as f:
        f.write(enc[i:i + step])
print("forge:", len(data), "bytes ->", (len(enc) + step - 1) // step, "parts")
EOF
