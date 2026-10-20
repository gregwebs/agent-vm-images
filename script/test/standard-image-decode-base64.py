#!/usr/bin/env python3
"""Decode the base64 blob on stdin to raw bytes on stdout.

Text is read as text and written as bytes, matching the shell capture path that
fed this blob: a text-normalizing rewrite would corrupt the captured reports.
"""
from __future__ import annotations

import base64
import sys

sys.stdout.buffer.write(base64.b64decode(sys.stdin.read()))
