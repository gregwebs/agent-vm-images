#!/usr/bin/env node
console.log("BIN:" + require("./package.json").name + ":" + require("./package.json").version);
if (process.env.PNPM_TEST_NESTED) console.log("NESTED:" + require("child_process").execFileSync("pnpm", ["config", "get", "ignore-scripts"], {encoding: "utf8"}).trim());
console.log(JSON.stringify({
  argv: process.argv.slice(2),
  home: process.env.HOME,
  policy: Object.fromEntries(Object.entries(process.env).filter(([k]) => k.startsWith("PNPM_CONFIG_"))),
  xdg: process.env.XDG_CONFIG_HOME,
  ignoreScripts: process.env.PNPM_CONFIG_IGNORE_SCRIPTS,
}));
