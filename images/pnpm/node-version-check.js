#!/usr/bin/env node
// Reject a host Node older than 22.13.0; bootstrap runs this as a build gate.
const [a, b] = process.versions.node.split('.').map(Number);
if (a < 22 || (a === 22 && b < 13)) process.exit(1);
