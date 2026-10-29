#!/usr/bin/env python3
"""Recording stand-in for the owning preparation driver.

The public `prepare-lock.sh` interfaces are only argument grammar and status
translation, so their suite needs a driver that records exactly the argv it was
handed and answers with a chosen status. It lives here, as a real Python file,
because the recipe-contract sources forbid embedded Python in shell.
"""
import os
import sys

log = os.environ['FAKE_DRIVER_LOG']
with open(log, 'a', encoding='utf-8') as handle:
    handle.write(' '.join(sys.argv[1:]) + '\n')
sys.exit(int(os.environ.get('FAKE_DRIVER_STATUS', '0')))
