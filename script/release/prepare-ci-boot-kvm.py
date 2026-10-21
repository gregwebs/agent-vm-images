#!/usr/bin/env python3
"""Open and close /dev/kvm the way the runtime will.

Runs as the invoking CI user after the udev grant: mode bits alone are not
proof that the device is usable.
"""
from __future__ import annotations

import os

os.close(os.open('/dev/kvm', os.O_RDWR))
