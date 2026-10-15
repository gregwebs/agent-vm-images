#!/usr/bin/env node
// Developer-only re-pin of the reviewed pnpm inputs.
//
// This is deliberately a separate program from check-lock.mjs: the checker can
// never write a pin, so a build cannot re-pin the very bytes it is meant to
// verify. Only the deliberate upgrade transaction (upgrade-plugins.sh) and
// prepare-lock.sh --refresh-lock call this, and it is never copied into an
// image.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { digest, pinFile, pinnedInputs } from "./check-lock.mjs";

export function writePins(prefix) {
  const body = pinnedInputs.map(name => `${digest(path.join(prefix, name))}  ${name}\n`).join("");
  fs.writeFileSync(path.join(prefix, pinFile), body);
  return body;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    if (process.argv.length !== 3) throw new Error("usage: pin-pnpm-inputs.mjs PREFIX");
    writePins(process.argv[2]);
  } catch (error) {
    console.error(`pin-pnpm-inputs: ${error.message}`);
    process.exitCode = 1;
  }
}
