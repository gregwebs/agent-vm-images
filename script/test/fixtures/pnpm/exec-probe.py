import json
import os
import sys

print(json.dumps({"argv": sys.argv[1:], "env": dict(os.environ),
                  "stdin": sys.stdin.read(), "pid": os.getpid()}))
print("stderr-token", file=sys.stderr)
sys.exit(int(os.environ.get("TEST_EXIT", "0")))
