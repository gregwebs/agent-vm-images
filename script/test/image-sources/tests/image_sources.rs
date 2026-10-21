//! Standalone image-source integrity gates; no launcher dependencies.

use std::path::{Path, PathBuf};

use serde_json::Value;

fn repo_path(relative: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .join(relative)
}

fn read_text(relative: &str) -> String {
    let path = repo_path(relative);
    std::fs::read_to_string(&path)
        .unwrap_or_else(|error| panic!("read {}: {error}", path.display()))
}

fn read_json(relative: &str) -> Value {
    let text = read_text(relative);
    serde_json::from_str(&text).unwrap_or_else(|error| panic!("{relative} is valid JSON: {error}"))
}

fn pi_pin() -> String {
    read_json("images/tools/pi/package.json")["dependencies"]["@earendil-works/pi-coding-agent"]
        .as_str()
        .expect("the manifest pins pi")
        .to_string()
}

fn bridge_pin() -> String {
    read_json("images/tools/pi/bridge/package.json")["dependencies"]["pi-claude-bridge"]
        .as_str()
        .expect("the bridge manifest pins pi-claude-bridge")
        .to_string()
}

fn dsh_pin() -> String {
    read_json("images/tools/dsh/package.json")["dependencies"]["@deepseek-ai/dsh"]
        .as_str()
        .expect("the dsh manifest pins @deepseek-ai/dsh")
        .to_string()
}

/// One pinned package: the manifest that declares it, the dependency key and
/// the lock's resolved package key. Named fields rather than three adjacent
/// `&str` selectors (`CODING_STANDARDS.md` → *Types*).
struct PinSpec<'a> {
    manifest_relative: &'a str,
    dependency: &'a str,
    package_key: &'a str,
}

/// Every non-root entry in a committed lock carries `integrity`. For a
/// shrinkwrapped Pi (<=1.0.3) a freshly generated lock does NOT: npm inherits
/// Pi's published `npm-shrinkwrap.json`, which omits the hashes for its five
/// `@earendil-works` siblings, so those are refilled by hand. Pi 1.0.4+ ships no
/// shrinkwrap, so every entry arrives hashed. This catches a regenerated lock
/// that dropped them.
fn check_lock_integrity(lock: &Value) -> Result<(), String> {
    let packages = lock["packages"]
        .as_object()
        .ok_or_else(|| "the lock carries no `packages` object".to_string())?;
    let missing: Vec<&str> = packages
        .iter()
        .filter(|(key, value)| !key.is_empty() && value.get("integrity").is_none())
        .map(|(key, _)| key.as_str())
        .collect();
    if missing.is_empty() {
        Ok(())
    } else {
        Err(format!(
            "these committed lock entries carry no `integrity` hash: {missing:?}"
        ))
    }
}

/// The lock's root dependency and the resolved package version both equal the
/// manifest pin.
fn check_pin_agrees(lock: &Value, spec: PinSpec) -> Result<(), String> {
    let manifest = read_json(spec.manifest_relative);
    let pinned = manifest["dependencies"][spec.dependency]
        .as_str()
        .ok_or_else(|| {
            format!(
                "{} does not pin {}",
                spec.manifest_relative, spec.dependency
            )
        })?;
    let root = lock["packages"][""]["dependencies"][spec.dependency]
        .as_str()
        .ok_or_else(|| format!("the lock's root does not record {}", spec.dependency))?;
    if root != pinned {
        return Err(format!(
            "the lock's root {} is {root}, the manifest pins {pinned}",
            spec.dependency
        ));
    }
    let locked = lock["packages"][spec.package_key]["version"]
        .as_str()
        .ok_or_else(|| format!("{} is not locked", spec.package_key))?;
    if locked != pinned {
        return Err(format!(
            "npm ci would install {locked}, but the manifest pins {pinned}"
        ));
    }
    Ok(())
}

/// `install-pi.sh` verifies the `@earendil-works` siblings nested directly under
/// pi-coding-agent's own `node_modules`, at the pi pin. That layout belongs to a
/// shrinkwrapped Pi (<=1.0.3); Pi 1.0.4+ publishes no `npm-shrinkwrap.json`, so
/// the committed lock is authoritative, every entry carries a hash, and the
/// nested set is legitimately empty. Only those two layouts are legal: a
/// different nonempty set means Pi's dependency layout moved and needs review.
/// The selector is deliberately direct-children-only: pi-ai carries deeper
/// nested deps that DO carry npm-checked integrity, so a looser match
/// over-matches.
fn check_pi_siblings(lock: &Value) -> Result<(), String> {
    const SIBLINGS: [&str; 5] = ["chord", "pi-agent-core", "pi-ai", "pi-telemetry", "pi-tui"];
    const PREFIX: &str =
        "node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/";
    let pinned = pi_pin();
    let packages = lock["packages"]
        .as_object()
        .ok_or_else(|| "the lock carries no `packages` object".to_string())?;
    let mut matched: Vec<(String, String)> = Vec::new();
    for (key, value) in packages {
        let Some(name) = key.strip_prefix(PREFIX) else {
            continue;
        };
        if name.contains('/') {
            continue;
        }
        matched.push((
            name.to_string(),
            value["version"].as_str().unwrap_or("<none>").to_string(),
        ));
    }
    matched.sort();
    if matched.is_empty() {
        return Ok(());
    }
    let mut expected: Vec<(String, String)> = SIBLINGS
        .iter()
        .map(|name| (name.to_string(), pinned.clone()))
        .collect();
    expected.sort();
    if matched == expected {
        Ok(())
    } else {
        Err(format!(
            "the nested sibling set drifted: matched {matched:?}, expected [] or {expected:?}"
        ))
    }
}

#[test]
fn the_pinned_pi_version_agrees_across_the_manifest_and_the_lockfile() {
    let lock = read_json("images/tools/pi/package-lock.json");
    check_pin_agrees(
        &lock,
        PinSpec {
            manifest_relative: "images/tools/pi/package.json",
            dependency: "@earendil-works/pi-coding-agent",
            package_key: "node_modules/@earendil-works/pi-coding-agent",
        },
    )
    .expect("the pi pin agrees with its lock");
}

#[test]
fn every_locked_package_carries_integrity() {
    let lock = read_json("images/tools/pi/package-lock.json");
    assert!(
        lock["packages"].as_object().is_some_and(
            |packages| packages.contains_key("node_modules/@earendil-works/pi-coding-agent")
        ),
        "the pi lock must actually carry pi"
    );
    check_lock_integrity(&lock).unwrap_or_else(|problem| {
        panic!(
            "{problem}\nA freshly generated lock for a shrinkwrapped Pi omits integrity for its five \
             @earendil-works siblings. Refill them from the registry (see images/tools/README.md)."
        )
    });
}

#[test]
fn the_build_verified_sibling_set_is_empty_or_exactly_the_five_nested_earendil_packages() {
    let lock = read_json("images/tools/pi/package-lock.json");
    check_pi_siblings(&lock)
        .expect("the build-verified sibling set is empty or exactly the five packages");
}

// ---------------------------------------------------------------------------
// Pinned bridge packages: full integrity and no second Pi
// ---------------------------------------------------------------------------

/// The bridge pin is an exact version (not a range): the tree has no shrinkwrap,
/// so a regenerated lock could otherwise move the installed version without this
/// repo moving the pin.
fn check_bridge_pin_locked(lock: &Value) -> Result<(), String> {
    let pinned = bridge_pin();
    let range_characters = ['^', '~', '>', '<', '=', '*', '|', ' ', 'x', 'X'];
    if pinned.split('.').count() < 3 || pinned.chars().any(|c| range_characters.contains(&c)) {
        return Err(format!(
            "the pi-claude-bridge pin must be an exact version, not a range: {pinned:?}"
        ));
    }
    let root = lock["packages"][""]["dependencies"]["pi-claude-bridge"]
        .as_str()
        .ok_or_else(|| "the lock's root does not record pi-claude-bridge".to_string())?;
    if root != pinned {
        return Err(format!(
            "the lock's root dependency {root} != the manifest pin {pinned}"
        ));
    }
    let locked = lock["packages"]["node_modules/pi-claude-bridge"]["version"]
        .as_str()
        .ok_or_else(|| "pi-claude-bridge is not locked".to_string())?;
    if locked != pinned {
        return Err(format!(
            "npm ci would install {locked}, but the manifest pins {pinned}"
        ));
    }
    Ok(())
}

/// No bridge-lock entry names a package Pi's extension loader aliases to its own
/// copies. The bridge was locked with `--legacy-peer-deps` precisely to avoid
/// dragging in a second, version-skewed `@earendil-works/pi-coding-agent`.
fn check_no_loader_aliased_pi_package(lock: &Value) -> Result<(), String> {
    const ALIASED: [&str; 5] = [
        "@earendil-works",
        "typebox",
        "pi-agent-core",
        "pi-tui",
        "pi-ai",
    ];
    let packages = lock["packages"]
        .as_object()
        .ok_or_else(|| "the lock carries no `packages` object".to_string())?;
    let offending: Vec<&str> = packages
        .keys()
        .map(String::as_str)
        .filter(|key| ALIASED.iter().any(|aliased| key.contains(aliased)))
        .collect();
    if offending.is_empty() {
        Ok(())
    } else {
        Err(format!(
            "the bridge lock installs a package Pi's loader aliases: {offending:?}"
        ))
    }
}

/// `--omit=optional` and `--legacy-peer-deps` are mandatory on the layer's
/// active `npm ci`: the lock carries the Claude Agent SDK's eight optional
/// native binaries, so dropping `--omit=optional` silently adds ~197 MiB, and
/// dropping `--legacy-peer-deps` lets npm drag in a second, version-skewed Pi.
///
/// Only **active** command text counts: a commented-out `npm ci` must neither
/// satisfy the requirement nor mask a later active invocation missing a flag.
/// A missing active invocation fails rather than letting an empty scan pass, and
/// **every** active `npm ci` invocation - bare or flagged - must carry all three
/// flags. Trailing shell comments are stripped first, so a mandatory flag that
/// exists only after a `#` is never accepted as present.
fn check_bridge_install_flags(script: &str) -> Result<(), String> {
    const MANDATORY: [&str; 3] = ["--ignore-scripts", "--omit=optional", "--legacy-peer-deps"];
    let installers: Vec<&str> = script
        .lines()
        .filter(|line| !line.trim_start().starts_with('#'))
        .map(strip_shell_comment)
        .filter(|command| is_npm_ci(command))
        .collect();
    if installers.is_empty() {
        return Err("no active `npm ci` invocation in install-pi-packages.sh".to_string());
    }
    for line in &installers {
        for flag in MANDATORY {
            if !line.contains(flag) {
                return Err(format!("an active npm ci line dropped {flag}: {line}"));
            }
        }
    }
    Ok(())
}

/// The part of a shell line before an unquoted `#` comment, with the comment and
/// its `#` removed. A `#` opens a comment only at the start of a word (line start
/// or after whitespace), matching `/bin/sh`, so a `#` inside a word is literal.
/// This keeps a flag that lives only in a trailing comment out of the flag check.
fn strip_shell_comment(line: &str) -> &str {
    let mut in_single = false;
    let mut in_double = false;
    let mut word_start = true;
    for (index, ch) in line.char_indices() {
        match ch {
            '\'' if !in_double => in_single = !in_single,
            '"' if !in_single => in_double = !in_double,
            '#' if !in_single && !in_double && word_start => return &line[..index],
            _ => {}
        }
        word_start = ch.is_whitespace();
    }
    line
}

/// Whether the command text invokes `npm ci` (bare or with flags), as two
/// adjacent **unquoted** whitespace-separated tokens. A bare `npm ci` counts, so
/// a second unflagged install cannot hide behind an earlier valid one, while
/// `npm ci` inside a quoted string (an error message) is not an invocation.
fn is_npm_ci(command: &str) -> bool {
    let mut words: Vec<(String, bool)> = Vec::new();
    let mut current = String::new();
    let mut current_quoted = false;
    let mut in_single = false;
    let mut in_double = false;
    for ch in command.chars() {
        match ch {
            '\'' if !in_double => {
                in_single = !in_single;
                current_quoted = true;
            }
            '"' if !in_single => {
                in_double = !in_double;
                current_quoted = true;
            }
            c if c.is_whitespace() && !in_single && !in_double => {
                if !current.is_empty() {
                    words.push((std::mem::take(&mut current), current_quoted));
                    current_quoted = false;
                }
            }
            c => current.push(c),
        }
    }
    if !current.is_empty() {
        words.push((current, current_quoted));
    }
    words
        .windows(2)
        .any(|pair| !pair[0].1 && !pair[1].1 && pair[0].0 == "npm" && pair[1].0 == "ci")
}

#[test]
fn every_bridge_locked_package_carries_integrity() {
    let lock = read_json("images/tools/pi/bridge/package-lock.json");
    assert!(
        lock["packages"]
            .as_object()
            .is_some_and(|packages| packages.contains_key("node_modules/pi-claude-bridge")),
        "the bridge lock must actually carry pi-claude-bridge"
    );
    check_lock_integrity(&lock).expect("every bridge lock entry carries integrity");
}

#[test]
fn the_bridge_pin_is_exact_and_the_lock_agrees() {
    let lock = read_json("images/tools/pi/bridge/package-lock.json");
    check_bridge_pin_locked(&lock).expect("the bridge pin is exact and the lock agrees");
}

#[test]
fn the_bridge_lock_installs_no_loader_aliased_pi_package() {
    let lock = read_json("images/tools/pi/bridge/package-lock.json");
    check_no_loader_aliased_pi_package(&lock)
        .expect("no loader-aliased package in the bridge lock");
}

#[test]
fn the_bridge_install_passes_npm_ci_the_mandatory_flags() {
    let script = read_text("images/tools/pi/install-pi-packages.sh");
    check_bridge_install_flags(&script).expect("install-pi-packages.sh passes the mandatory flags");
}

// ---------------------------------------------------------------------------
// Pinned dsh layer: one reviewed pnpm project read through the shared CLI
// ---------------------------------------------------------------------------

/// The four reviewed dsh inputs. A named field rather than a bare `PathBuf`
/// argument, so a pi/bridge prefix cannot be passed to a dsh gate by mistake.
struct DshProjectFiles {
    prefix: PathBuf,
}

impl DshProjectFiles {
    fn committed() -> Self {
        Self {
            prefix: repo_path("images/tools/dsh"),
        }
    }

    fn carries_npm_lock(&self) -> bool {
        self.prefix.join("package-lock.json").exists()
    }
}

/// One resolved archive as the graph module reported it.
struct DshArchive {
    name: String,
    version: String,
    integrity: String,
    tarball: String,
}

/// One resolved snapshot: the full peer-context identity key.
struct DshSnapshot {
    key: String,
}

/// One importer/root dependency edge.
struct DshEdge {
    name: String,
    target: String,
}

/// A typed read of `pnpm-lock.mjs graph dsh`. A syntax adapter, never a second
/// parser: the shared module owns YAML, pins and the effective-peer rules
/// (`docs/adr/0001-build-only-pnpm-graph-parser.md`), so Rust only types what it
/// printed and asserts independently authored expectations over it.
struct DshGraph {
    version: String,
    packages: Vec<DshArchive>,
    snapshots: Vec<DshSnapshot>,
    importer: Vec<DshEdge>,
}

fn graph_report_field(value: &Value, path: &[&str]) -> Result<String, String> {
    let mut current = value;
    for key in path {
        current = current
            .get(*key)
            .ok_or_else(|| format!("graph report has no {}", path.join(".")))?;
    }
    current
        .as_str()
        .map(str::to_owned)
        .ok_or_else(|| format!("graph report field {} is not a string", path.join(".")))
}

impl DshGraph {
    fn from_value(value: &Value) -> Result<Self, String> {
        match graph_report_field(value, &["selection", "kind"])?.as_str() {
            "dsh" => {}
            other => return Err(format!("graph report is for {other}, not dsh")),
        }
        let mut packages = Vec::new();
        for entry in value
            .get("packages")
            .and_then(Value::as_array)
            .ok_or_else(|| "graph report has no packages array".to_string())?
        {
            packages.push(DshArchive {
                name: graph_report_field(entry, &["id", "name"])?,
                version: graph_report_field(entry, &["id", "version"])?,
                integrity: graph_report_field(entry, &["integrity"])?,
                tarball: graph_report_field(entry, &["tarball"])?,
            });
        }
        let mut snapshots = Vec::new();
        for entry in value
            .get("snapshots")
            .and_then(Value::as_array)
            .ok_or_else(|| "graph report has no snapshots array".to_string())?
        {
            snapshots.push(DshSnapshot {
                key: graph_report_field(entry, &["id", "key"])?,
            });
        }
        let mut importer = Vec::new();
        for entry in value
            .get("importer")
            .and_then(Value::as_array)
            .ok_or_else(|| "graph report has no importer array".to_string())?
        {
            importer.push(DshEdge {
                name: graph_report_field(entry, &["name"])?,
                target: graph_report_field(entry, &["target"])?,
            });
        }
        Ok(Self {
            version: graph_report_field(value, &["selection", "version"])?,
            packages,
            snapshots,
            importer,
        })
    }

    fn packages_named<'a>(&'a self, name: &'a str) -> impl Iterator<Item = &'a DshArchive> {
        self.packages.iter().filter(move |entry| entry.name == name)
    }

    fn package(&self, name: &str, version: &str) -> Option<&DshArchive> {
        self.packages
            .iter()
            .find(|entry| entry.name == name && entry.version == version)
    }

    fn carries(&self, name: &str) -> bool {
        self.packages.iter().any(|entry| entry.name == name)
    }
}

fn run_graph_cli(verb: &str, prefix: &Path) -> Result<String, String> {
    let module = repo_path("images/recipe-contract/pnpm-lock.mjs");
    let output = std::process::Command::new("node")
        .arg(&module)
        .arg(verb)
        .arg("dsh")
        .arg(prefix)
        .output()
        .map_err(|error| format!("cannot run {}: {error}", module.display()))?;
    if !output.status.success() {
        return Err(format!(
            "pnpm-lock.mjs {verb} rejected {}: {}",
            prefix.display(),
            String::from_utf8_lossy(&output.stderr).trim()
        ));
    }
    String::from_utf8(output.stdout)
        .map_err(|_| "pnpm-lock.mjs emitted non-UTF-8 output".to_string())
}

/// The `check` verb re-verifies the reviewed input digests and every effective
/// peer, so its exact report is the selection version.
fn check_dsh_inputs(project: &DshProjectFiles) -> Result<String, String> {
    run_graph_cli("check", &project.prefix)
}

fn read_dsh_graph(project: &DshProjectFiles) -> Result<DshGraph, String> {
    let text = run_graph_cli("graph", &project.prefix)?;
    let value: Value = serde_json::from_str(&text)
        .map_err(|error| format!("graph report is not JSON: {error}"))?;
    DshGraph::from_value(&value)
}

/// A disposable copy of the four reviewed inputs. Mutation controls need the
/// intended structural gate to be the one that rejects, so the fixture can
/// recompute the pin file as test data after an edit.
struct DshFixture {
    directory: PathBuf,
    project: DshProjectFiles,
}

impl DshFixture {
    const INPUTS: [&'static str; 4] = [
        "package.json",
        "pnpm-lock.yaml",
        "pnpm-workspace.yaml",
        "pnpm-inputs.sha256",
    ];

    fn copy(label: &str) -> Self {
        let directory =
            std::env::temp_dir().join(format!("image-sources-dsh-{}-{label}", std::process::id()));
        let _ = std::fs::remove_dir_all(&directory);
        std::fs::create_dir_all(&directory).expect("fixture directory");
        for file in Self::INPUTS {
            std::fs::copy(
                repo_path(&format!("images/tools/dsh/{file}")),
                directory.join(file),
            )
            .expect("copy reviewed input");
        }
        Self {
            directory: directory.clone(),
            project: DshProjectFiles { prefix: directory },
        }
    }

    fn rewrite(&self, file: &str, edit: impl FnOnce(&str) -> String) {
        let path = self.directory.join(file);
        let before = std::fs::read_to_string(&path).expect("read fixture input");
        let after = edit(&before);
        assert_ne!(
            after, before,
            "{file}: the mutation must change the fixture"
        );
        std::fs::write(&path, after).expect("write fixture input");
    }

    fn repin(&self) {
        const SCRIPT: &str = "const fs=require('node:fs'),crypto=require('node:crypto'),path=require('node:path');const prefix=process.argv[1];fs.writeFileSync(path.join(prefix,'pnpm-inputs.sha256'),['package.json','pnpm-lock.yaml','pnpm-workspace.yaml'].map(file=>crypto.createHash('sha256').update(fs.readFileSync(path.join(prefix,file))).digest('hex')+'  '+file+'\\n').join(''));";
        let output = std::process::Command::new("node")
            .arg("-e")
            .arg(SCRIPT)
            .arg(&self.directory)
            .output()
            .expect("run node to re-pin the fixture");
        assert!(
            output.status.success(),
            "fixture re-pin failed: {}",
            String::from_utf8_lossy(&output.stderr)
        );
    }
}

impl Drop for DshFixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.directory);
    }
}

/// Every case recomputes the fixture pins, so a stale digest can never be the
/// reason a mutation is rejected; the expected diagnostic proves the intended
/// rule fired.
fn rejected_mutation(label: &str, mutate: impl FnOnce(&DshFixture), expected: &str) {
    let fixture = DshFixture::copy(label);
    mutate(&fixture);
    fixture.repin();
    let error = match read_dsh_graph(&fixture.project) {
        Ok(_) => panic!("{label}: the mutated dsh project must not validate"),
        Err(error) => error,
    };
    assert!(
        error.contains(expected),
        "{label}: expected {expected:?} in the rejection, got {error:?}"
    );
}

fn is_full_sha512(integrity: &str) -> bool {
    let Some(body) = integrity.strip_prefix("sha512-").map(str::as_bytes) else {
        return false;
    };
    body.len() == 88
        && body.ends_with(b"==")
        && body[..86]
            .iter()
            .all(|byte| byte.is_ascii_alphanumeric() || *byte == b'+' || *byte == b'/')
}

/// Rewrite one lock mapping block selected by its header line. Selecting by
/// header keeps a mutation from silently hitting a different package that
/// happens to share the same inner lines.
fn edit_yaml_block(text: &str, header: &str, edit: impl FnOnce(&str) -> String) -> String {
    let start = text
        .find(header)
        .unwrap_or_else(|| panic!("missing block {header:?}"));
    let end = start + text[start..].find("\n\n").expect("block end");
    let block = &text[start..end];
    let mutated = edit(block);
    assert_ne!(mutated, block, "the block mutation must change {header:?}");
    format!("{}{}{}", &text[..start], mutated, &text[end..])
}

/// Effective peers are per snapshot: an exact declaration must match the
/// resolved edge, and a correct version existing elsewhere is not evidence.
/// Every case re-pins the mutated fixture, so a stale digest can never be the
/// reason a mutation is rejected.
#[test]
fn dsh_graph_and_effective_peer_mutations_are_rejected() {
    let graph = read_dsh_graph(&DshProjectFiles::committed())
        .expect("the reviewed dsh inputs form a valid graph");
    let schemastery: Vec<&str> = graph
        .packages_named("@deepseek-ai/schemastery")
        .map(|entry| entry.version.as_str())
        .collect();
    assert_eq!(
        schemastery,
        ["3.18.2", "3.18.4"],
        "the peer mutation needs the correct version to exist globally"
    );

    rejected_mutation(
        "package-manager",
        |fixture| {
            fixture.rewrite("package.json", |text| {
                text.replacen("pnpm@11.11.0", "pnpm@11.10.0", 1)
            });
        },
        "pnpm@11.11.0",
    );

    rejected_mutation(
        "missing-integrity",
        |fixture| {
            fixture.rewrite("pnpm-lock.yaml", |text| {
                edit_yaml_block(text, "  '@deepseek-ai/dsh@0.1.5-rc.2':\n", |block| {
                    let mut lines: Vec<String> = block.lines().map(str::to_string).collect();
                    let index = lines
                        .iter()
                        .position(|line| line.trim_start().starts_with("resolution:"))
                        .expect("the archive declares a resolution");
                    lines[index] = "    resolution: {}".to_string();
                    lines.join("\n")
                })
            });
        },
        "missing key: integrity",
    );

    rejected_mutation(
        "bad-integrity",
        |fixture| {
            fixture.rewrite("pnpm-lock.yaml", |text| {
                edit_yaml_block(
                    text,
                    "  '@deepseek-ai/dsh-sandbox-local@0.1.5-rc.3':\n",
                    |block| block.replacen("sha512-ma3IB1EervDxQvaZRi5VIozCb+qbWF89i6p8uRdBLpnJHzLcuI/AKI7S3Hkcvuy2JQEUmWTaiaq4Eil8GY9JNQ==", "sha512-short", 1),
                )
            });
        },
        "full sha512 required",
    );

    rejected_mutation(
        "wrong-effective-peer",
        |fixture| {
            fixture.rewrite("pnpm-lock.yaml", |text| {
                edit_yaml_block(
                    text,
                    "  '@deepseek-ai/dsh-settings@0.1.5-rc.3':\n",
                    |block| {
                        block.replacen(
                            "'@deepseek-ai/schemastery': 3.18.2",
                            "'@deepseek-ai/schemastery': 3.18.4",
                            1,
                        )
                    },
                )
            });
        },
        "peer @deepseek-ai/schemastery wanted 3.18.4, got @deepseek-ai/schemastery@3.18.2",
    );

    rejected_mutation(
        "orphan",
        |fixture| {
            fixture.rewrite("pnpm-lock.yaml", |text| {
                let archive = format!(
                    "packages:\n\n  'orphan-fixture@1.0.0':\n    resolution: {{integrity: sha512-{}}}\n\n",
                    "A".repeat(86) + "=="
                );
                let with_archive = text.replacen("packages:\n\n", &archive, 1);
                with_archive.replacen(
                    "snapshots:\n\n",
                    "snapshots:\n\n  'orphan-fixture@1.0.0': {}\n\n",
                    1,
                )
            });
        },
        "orphan",
    );

    rejected_mutation(
        "absent-sandbox",
        |fixture| {
            fixture.rewrite("pnpm-lock.yaml", |text| {
                edit_yaml_block(
                    text,
                    "  '@deepseek-ai/dsh-sandbox-local@0.1.5-rc.3':\n",
                    |_| String::new(),
                )
            });
        },
        "dsh-sandbox-local",
    );
}

#[test]
fn the_pinned_dsh_version_agrees_across_the_manifest_and_the_lockfile() {
    let project = DshProjectFiles::committed();
    let manifest = read_json("images/tools/dsh/package.json");
    assert_eq!(manifest["name"], "agent-vm-dsh-layer");
    assert_eq!(manifest["private"], true);
    let dependencies = manifest["dependencies"]
        .as_object()
        .expect("the dsh manifest declares dependencies");
    let roots: Vec<&str> = dependencies.keys().map(String::as_str).collect();
    assert_eq!(roots, ["@deepseek-ai/dsh"], "the dsh project has one root");
    assert!(
        manifest.get("devDependencies").is_none(),
        "the dsh project declares no development root"
    );
    let base = read_json("images/pnpm/pin.json")["version"]
        .as_str()
        .expect("the base pin carries a version")
        .to_string();
    assert_eq!(
        manifest["packageManager"],
        format!("pnpm@{base}"),
        "packageManager asserts the base pin instead of selecting a second manager"
    );

    let graph = read_dsh_graph(&project).expect("the reviewed dsh inputs form a valid graph");
    assert_eq!(
        graph.version,
        dsh_pin(),
        "the manifest, the image label and the graph select one version"
    );
    assert_eq!(
        check_dsh_inputs(&project).expect("the CLI verifies every reviewed dsh input"),
        format!("{}\n", dsh_pin())
    );
    assert_eq!(graph.importer.len(), 1, "one imported root");
    let root = &graph.importer[0];
    assert_eq!(root.name, "@deepseek-ai/dsh");
    assert!(
        root.target
            .starts_with(&format!("@deepseek-ai/dsh@{}", dsh_pin())),
        "the imported root resolves to the pinned version: {}",
        root.target
    );
    assert!(
        graph.snapshots.iter().any(|entry| entry.key == root.target),
        "the imported root is a graph snapshot"
    );
    assert!(
        !project.carries_npm_lock(),
        "the dsh project owns no npm lock after the migration"
    );
}

#[test]
fn every_dsh_locked_package_carries_integrity() {
    let graph = read_dsh_graph(&DshProjectFiles::committed())
        .expect("the reviewed dsh inputs form a valid graph");
    assert!(!graph.packages.is_empty(), "the report carries the graph");
    let dsh = graph
        .package("@deepseek-ai/dsh", &dsh_pin())
        .expect("the graph carries the selected dsh root");
    // Independently authored from the release metadata in the ticket, not from
    // this graph's own output.
    assert_eq!(
        dsh.integrity,
        "sha512-8Xc8hCQHcIWRmTCVU/xZdp6/qMsWMeAd2ObChKDEsfhUPJFXx6H0lgeb1DxUMD86HZrrVN+1bCvn1ppjZ/fOxw=="
    );
    assert_eq!(
        dsh.tarball, "https://registry.npmjs.org/@deepseek-ai/dsh/-/dsh-0.1.5-rc.2.tgz",
        "only the canonical npmjs resolver is allowed"
    );
    assert!(
        graph.carries("@deepseek-ai/dsh-sandbox-local"),
        "the private hoist's sandbox owner is part of the payload"
    );
    let missing: Vec<&str> = graph
        .packages
        .iter()
        .filter(|entry| !is_full_sha512(&entry.integrity))
        .map(|entry| entry.name.as_str())
        .collect();
    assert!(
        missing.is_empty(),
        "these graph packages carry no full sha512 integrity: {missing:?}"
    );
}

// ---------------------------------------------------------------------------
// Exact shipped version slots: ARGs, labels, recipe sources
// ---------------------------------------------------------------------------

const RECIPES: [&str; 6] = ["dsh", "pi", "codex", "opencode", "claude", "copilot"];

/// Eight standard-owned selections; pnpm is inherited from base.
const VERSION_SLOTS: [(&str, &str); 8] = [
    ("codex", "AGENT_VERSION_CODEX"),
    ("opencode", "AGENT_VERSION_OPENCODE"),
    ("claude", "AGENT_VERSION_CLAUDE"),
    ("copilot", "AGENT_VERSION_COPILOT"),
    ("dsh", "AGENT_VERSION_DSH"),
    ("pi", "AGENT_VERSION_PI"),
    ("pi-claude-bridge", "AGENT_VERSION_PI_CLAUDE_BRIDGE"),
    ("opencode-claude", "AGENT_VERSION_OPENCODE_CLAUDE"),
];

fn standard_dockerfile() -> String {
    read_text("images/standard/Dockerfile")
}

/// The one `ARG NAME=<value>` line's value (panics unless exactly one).
fn dockerfile_arg(text: &str, name: &str) -> String {
    let prefix = format!("ARG {name}=");
    let matches: Vec<&str> = text
        .lines()
        .map(str::trim)
        .filter(|line| line.starts_with(&prefix))
        .collect();
    assert_eq!(
        matches.len(),
        1,
        "expected exactly one `ARG {name}=` line, found {}: {matches:?}",
        matches.len()
    );
    matches[0][prefix.len()..].to_string()
}

/// The active `LABEL` key=value pairs of a Dockerfile, parsed the way a build
/// would see them. Comment lines are dropped before instruction joining, and a
/// trailing `\` joins the next line, so a commented-out or continued label is
/// resolved exactly as the built image resolves it.
fn active_label_values(text: &str) -> Vec<(String, String)> {
    let stripped: Vec<&str> = text
        .lines()
        .filter(|line| !line.trim_start().starts_with('#'))
        .collect();
    let mut logical: Vec<String> = Vec::new();
    let mut current = String::new();
    let mut continuing = false;
    for line in stripped {
        let line = line.trim();
        if continuing {
            current.push(' ');
        } else {
            current.clear();
        }
        current.push_str(line);
        if current.ends_with('\\') {
            current.pop();
            continuing = true;
        } else {
            logical.push(std::mem::take(&mut current));
            continuing = false;
        }
    }
    if !current.is_empty() {
        logical.push(current);
    }
    let mut pairs = Vec::new();
    for instruction in logical {
        let (verb, rest) = match instruction.split_once(char::is_whitespace) {
            Some(parts) => parts,
            None => continue,
        };
        if !verb.eq_ignore_ascii_case("LABEL") {
            continue;
        }
        pairs.extend(label_pairs(rest));
    }
    pairs
}

/// Split a `LABEL` body into `key=value` pairs; a quoted value keeps its bytes.
fn label_pairs(body: &str) -> Vec<(String, String)> {
    let mut pairs = Vec::new();
    let mut chars = body.chars().peekable();
    while chars.peek().is_some() {
        while matches!(chars.peek(), Some(c) if c.is_whitespace()) {
            chars.next();
        }
        let mut key = String::new();
        while let Some(&c) = chars.peek() {
            if c == '=' || c.is_whitespace() {
                break;
            }
            key.push(c);
            chars.next();
        }
        if key.is_empty() {
            break;
        }
        if chars.peek() != Some(&'=') {
            continue;
        }
        chars.next();
        let mut value = String::new();
        if chars.peek() == Some(&'"') {
            chars.next();
            while let Some(c) = chars.next() {
                if c == '"' {
                    break;
                }
                if c == '\\' {
                    if let Some(escaped) = chars.next() {
                        value.push(escaped);
                    }
                } else {
                    value.push(c);
                }
            }
        } else {
            while let Some(&c) = chars.peek() {
                if c.is_whitespace() {
                    break;
                }
                value.push(c);
                chars.next();
            }
        }
        pairs.push((key, value));
    }
    pairs
}

/// The value of the single ACTIVE `LABEL KEY="..."`, or `None` when absent. A
/// duplicate active label is a failure (there is no defensible selection).
fn find_label_value(text: &str, key: &str) -> Option<String> {
    let values: Vec<String> = active_label_values(text)
        .into_iter()
        .filter(|(candidate, _)| candidate == key)
        .map(|(_, value)| value)
        .collect();
    match values.len() {
        0 => None,
        1 => Some(values.into_iter().next().expect("one value")),
        n => panic!("the {key} label must appear exactly once, found {n}"),
    }
}

fn label_value(text: &str, key: &str) -> String {
    find_label_value(text, key).unwrap_or_else(|| panic!("the {key} label is present"))
}

/// The active `org.agent-vm.version.*` keys of a Dockerfile.
fn active_version_labels(text: &str) -> Vec<String> {
    active_label_values(text)
        .into_iter()
        .map(|(key, _)| key)
        .filter(|key| key.starts_with("org.agent-vm.version."))
        .collect()
}

/// Canonical semver 2.0.0 parsing of the exact slot the owning hook accepts:
/// each core identifier is numeric with no leading zero; a pre-release
/// identifier is numeric (no leading zero) or alphanumeric; a build identifier
/// is digits (leading zeros allowed) or alphanumeric.
fn is_exact_semver(body: &str) -> bool {
    fn numeric(s: &str) -> bool {
        !s.is_empty() && s.chars().all(|c| c.is_ascii_digit()) && (s == "0" || !s.starts_with('0'))
    }
    fn digits(s: &str) -> bool {
        !s.is_empty() && s.chars().all(|c| c.is_ascii_digit())
    }
    fn alphanumeric(s: &str) -> bool {
        !s.is_empty()
            && s.chars().all(|c| c.is_ascii_alphanumeric() || c == '-')
            && s.chars().any(|c| !c.is_ascii_digit())
    }
    let (core_and_pre, build) = match body.split_once('+') {
        Some((head, build)) => (head, Some(build)),
        None => (body, None),
    };
    let (core, pre) = match core_and_pre.split_once('-') {
        Some((core, pre)) => (core, Some(pre)),
        None => (core_and_pre, None),
    };
    let core_ids: Vec<&str> = core.split('.').collect();
    if core_ids.len() != 3 || !core_ids.iter().all(|id| numeric(id)) {
        return false;
    }
    if pre.is_some_and(|p| p.is_empty() || !p.split('.').all(|id| numeric(id) || alphanumeric(id)))
    {
        return false;
    }
    if build.is_some_and(|b| b.is_empty() || !b.split('.').all(|id| digits(id) || alphanumeric(id)))
    {
        return false;
    }
    true
}

fn assert_exact_slot(value: &str, prefix: &str) {
    assert!(!value.is_empty(), "a selected slot must not be empty");
    let body = value
        .strip_prefix(prefix)
        .unwrap_or_else(|| panic!("the slot must start with {prefix:?}: {value:?}"));
    assert!(
        is_exact_semver(body),
        "the slot must be an exact semver version: {value:?}"
    );
    for forbidden in ['^', '~', '>', '<', '=', '*', '|', ' ', 'x', 'X', '@', '/'] {
        assert!(
            !value.contains(forbidden),
            "the slot must be exact, not a range/tag/URL: {value:?}"
        );
    }
}

#[test]
fn single_slot_recipes_pin_an_exact_default_and_reference_it_in_the_label() {
    let text = standard_dockerfile();
    for (label, arg) in &VERSION_SLOTS[..4] {
        let value = dockerfile_arg(&text, arg);
        match *label {
            "codex" => assert_exact_slot(&value, "rust-v"),
            "opencode" => {
                assert_exact_slot(&value, "v");
                assert!(value.starts_with("v2."), "OpenCode major 2 only");
            }
            _ => assert_exact_slot(&value, ""),
        }
        assert_eq!(
            label_value(&text, &format!("org.agent-vm.version.{label}")),
            format!("${{{arg}}}"),
            "standard: the version label must reference {arg}"
        );
    }
}

#[test]
fn multi_slot_args_are_empty_and_label_fallbacks_mirror_the_manifests() {
    let dsh = standard_dockerfile();
    assert_eq!(dockerfile_arg(&dsh, "AGENT_VERSION_DSH"), "");
    assert_eq!(dockerfile_arg(&dsh, "AGENT_VERSION_PNPM"), "");
    let base_pnpm = read_json("images/pnpm/pin.json")["version"]
        .as_str()
        .expect("the base pin carries a version")
        .to_string();
    assert_eq!(
        label_value(&dsh, "org.agent-vm.version.dsh"),
        format!("${{AGENT_VERSION_DSH:-{}}}", dsh_pin()),
        "the dsh label fallback must mirror images/tools/dsh/package.json"
    );
    assert_eq!(
        label_value(&read_text("images/Dockerfile"), "org.agent-vm.version.pnpm"),
        base_pnpm,
        "the inherited base pnpm label must mirror the base pin"
    );

    let pi = standard_dockerfile();
    assert_eq!(dockerfile_arg(&pi, "AGENT_VERSION_PI"), "");
    assert_eq!(dockerfile_arg(&pi, "AGENT_VERSION_PI_CLAUDE_BRIDGE"), "");
    assert_eq!(
        label_value(&pi, "org.agent-vm.version.pi"),
        format!("${{AGENT_VERSION_PI:-{}}}", pi_pin()),
        "the pi label fallback must mirror images/tools/pi/package.json"
    );
    assert_eq!(
        label_value(&pi, "org.agent-vm.version.pi-claude-bridge"),
        format!("${{AGENT_VERSION_PI_CLAUDE_BRIDGE:-{}}}", bridge_pin()),
        "the bridge label fallback must mirror images/tools/pi/bridge/package.json"
    );
}

#[test]
fn every_shipped_version_label_is_present_exactly_once() {
    let mut seen = active_version_labels(&standard_dockerfile());
    let mut expected: Vec<String> = VERSION_SLOTS
        .iter()
        .map(|(label, _)| format!("org.agent-vm.version.{label}"))
        .collect();
    seen.sort();
    expected.sort();
    assert_eq!(
        seen, expected,
        "the standard-owned version labels drifted from the eight-slot contract"
    );
}

#[test]
fn commented_out_version_labels_fail_the_guards() {
    assert_eq!(
        active_label_values("FROM scratch\nLABEL a=1 b=\"2\"\n"),
        vec![
            ("a".to_string(), "1".to_string()),
            ("b".to_string(), "2".to_string())
        ],
        "active LABEL pairs must parse"
    );
    assert_eq!(
        label_value(
            "FROM scratch\nLABEL org.agent-vm.version.copilot=\"${AGENT_VERSION_COPILOT}\"\n",
            "org.agent-vm.version.copilot"
        ),
        "${AGENT_VERSION_COPILOT}",
        "the active copilot label value must parse"
    );

    let commented =
        "FROM scratch\n# LABEL org.agent-vm.version.copilot=\"${AGENT_VERSION_COPILOT}\"\n";
    assert!(
        active_label_values(commented).is_empty(),
        "a commented LABEL must not be read: {commented:?}"
    );
    assert_eq!(
        find_label_value(commented, "org.agent-vm.version.copilot"),
        None,
        "a commented LABEL must fail the guard"
    );

    let copilot = standard_dockerfile();
    let mutated = copilot.replace(
        "LABEL org.agent-vm.version.copilot=",
        "# LABEL org.agent-vm.version.copilot=",
    );
    assert_ne!(mutated, copilot, "the mutation must change the text");
    assert!(
        !active_version_labels(&mutated).contains(&"org.agent-vm.version.copilot".to_string()),
        "the shipped-label set must notice the commented label"
    );
}

#[test]
fn continuations_join_and_the_exact_slot_grammar_rejects_bad_defaults() {
    let continued = "FROM scratch\nLABEL org.agent-vm.version.pi=\"x\" \\\n      org.agent-vm.version.pi-claude-bridge=\"y\"\n";
    assert_eq!(
        active_label_values(continued),
        vec![
            ("org.agent-vm.version.pi".to_string(), "x".to_string()),
            (
                "org.agent-vm.version.pi-claude-bridge".to_string(),
                "y".to_string()
            )
        ],
        "a continued LABEL must be joined"
    );

    for good in ["1.2.3", "0.159.3-alpha.1.2", "1.2.3-rc.1+build.01"] {
        assert!(is_exact_semver(good), "{good:?} is exact semver");
    }
    for bad in ["a.b.c", "01.2.3", "1.2.3-01", "1.2.3-a.", "1.2", "latest"] {
        assert!(!is_exact_semver(bad), "{bad:?} is NOT exact semver");
    }
}

// ---------------------------------------------------------------------------
// Recipe-source walkers: no base install helper, no upstream latest
// ---------------------------------------------------------------------------

/// Collect every committed file under `images/tools` as `(relative path, bytes)`.
fn image_source_files() -> Vec<(PathBuf, Vec<u8>)> {
    let root = repo_path("images");
    let mut stack = vec![
        root.join("tools"),
        root.join("standard"),
        root.join("recipe-contract"),
        root.join("pnpm"),
    ];
    let mut out = Vec::new();
    while let Some(dir) = stack.pop() {
        for entry in std::fs::read_dir(&dir)
            .unwrap_or_else(|error| panic!("read {}: {error}", dir.display()))
        {
            let entry = entry.expect("dir entry");
            let path = entry.path();
            if entry.file_type().expect("file type").is_dir() {
                stack.push(path);
            } else {
                let relative = path.strip_prefix(&root).expect("under root").to_path_buf();
                let contents = std::fs::read(&path)
                    .unwrap_or_else(|error| panic!("read {}: {error}", path.display()));
                out.push((relative, contents));
            }
        }
    }
    for name in ["Dockerfile", "install-pnpm.sh"] {
        out.push((
            PathBuf::from(name),
            std::fs::read(root.join(name)).expect("base source"),
        ));
    }
    out.sort_by(|a, b| a.0.cmp(&b.0));
    out
}

/// A recipe Dockerfile/hook/contract/JSON source (not docs, not the
/// `install.upstream.sh` provenance snapshots).
fn is_recipe_source(path: &Path) -> bool {
    if is_provenance_snapshot(path)
        || path
            .file_name()
            .unwrap()
            .to_string_lossy()
            .starts_with("upgrade-")
    {
        return false;
    }
    matches!(
        path.file_name().and_then(|name| name.to_str()),
        Some("Dockerfile")
    ) || matches!(
        path.extension().and_then(|ext| ext.to_str()),
        Some("sh" | "js" | "py")
    )
}

fn is_provenance_snapshot(path: &Path) -> bool {
    path.to_string_lossy().ends_with("install.upstream.sh")
}

#[test]
fn no_shipped_recipe_calls_the_base_agent_vm_install_helper() {
    let mut offenders = Vec::new();
    for (path, contents) in image_source_files() {
        if !is_recipe_source(&path) {
            continue;
        }
        if String::from_utf8_lossy(&contents).contains("agent-vm-install") {
            offenders.push(path);
        }
    }
    assert!(
        offenders.is_empty(),
        "these shipped recipes still call the base agent-vm-install helper: {offenders:?}"
    );
}

/// A recipe `ENV PATH=<new>:${PATH}` line must keep the base's entries. ADR-0035
/// retires the runtime layer contract, but `images/tools/README.md` still
/// requires maintainers to prepend additively so a recipe can never drop a
/// directory the base put there; this source-only guard keeps that rule honest
/// now that the embed-time composer is gone (Standards S8).
fn check_additive_path(text: &str, recipe: &str) -> Result<(), String> {
    for line in text.lines() {
        let line = line.trim();
        if line.starts_with('#') {
            continue;
        }
        let Some(rest) = line.strip_prefix("ENV ") else {
            continue;
        };
        // Parse each `KEY=value` assignment and require the PATH one itself to
        // carry `${PATH}`. Inspecting the whole line would accept a decoy that
        // inherits PATH in a sibling field (`OTHER=${PATH}`) while overwriting it.
        for assignment in rest.split_whitespace() {
            let Some((key, value)) = assignment.split_once('=') else {
                return Err(format!(
                    "{recipe}/Dockerfile uses an ENV form this guard cannot check: {line}"
                ));
            };
            if key == "PATH" && !value.contains("${PATH}") {
                return Err(format!(
                    "{recipe}/Dockerfile sets a non-additive ENV PATH: {line}"
                ));
            }
        }
    }
    Ok(())
}

#[test]
fn shipped_recipes_extend_path_additively() {
    check_additive_path(&standard_dockerfile(), "standard")
        .unwrap_or_else(|problem| panic!("{problem}"));
}

#[test]
fn no_shipped_recipe_resolves_upstream_latest() {
    const FORBIDDEN: [&str; 7] = [
        "/update/api/latest",
        "releases/latest",
        "/latest/download",
        "dist-tags.latest",
        "@latest",
        ":-latest",
        "claude-code-releases/latest",
    ];
    let mut offenders = Vec::new();
    for (path, contents) in image_source_files() {
        if !is_recipe_source(&path) || is_provenance_snapshot(&path) {
            continue;
        }
        let text = String::from_utf8_lossy(&contents);
        for (index, line) in text.lines().enumerate() {
            if line.trim_start().starts_with('#') {
                continue;
            }
            for needle in FORBIDDEN {
                if line.contains(needle) {
                    offenders.push(format!("{}:{}: {needle}", path.display(), index + 1));
                }
            }
        }
    }
    assert!(
        offenders.is_empty(),
        "these shipped recipes still resolve upstream latest: {offenders:?}"
    );
}

/// The exact-install hooks and vendored runnable installers are shipped.
#[test]
fn exact_image_install_hooks_exist() {
    let expected = [
        "codex/install-codex.sh",
        "codex/verify-codex.sh",
        "codex/vendor/install.sh",
        "opencode/install-opencode.sh",
        "opencode/verify-opencode.sh",
        "opencode/vendor/install.sh",
        "claude/install-claude.sh",
        "claude/verify-claude.sh",
        "claude/vendor/install.sh",
        "copilot/install-copilot.sh",
        "copilot/verify-copilot.sh",
        "dsh/install-dsh.sh",
        "dsh/verify-dsh.sh",
        "dsh/prepare-lock.sh",
        "dsh/check-lock-update.js",
        "dsh/check-runtime.mjs",
        "dsh/probe-dsh.py",
        "dsh/project.py",
        "dsh/derive-cordis-overrides.mjs",
        "pi/install-pi.sh",
        "pi/install-pi-packages.sh",
        "pi/verify-pi.sh",
        "pi/prepare-lock.sh",
        "pi/bridge/prepare-lock.sh",
    ];
    for relative in expected {
        let path = repo_path(&format!("images/tools/{relative}"));
        assert!(
            path.is_file(),
            "{} must exist and be a regular file",
            path.display()
        );
    }
}

// ---------------------------------------------------------------------------
// Self-controls: on in-memory copies, each production gate must reject a
// deliberately broken input (the checked-in files must pass).
// ---------------------------------------------------------------------------

#[test]
fn image_source_guards_reject_mutations() {
    // A dropped bridge npm-ci flag must fail the mandatory-flags gate.
    let script = read_text("images/tools/pi/install-pi-packages.sh");
    assert!(check_bridge_install_flags(&script).is_ok());
    let without_omit = script.replace(" --omit=optional", "");
    assert_ne!(without_omit, script, "the mutation must change the script");
    assert!(
        check_bridge_install_flags(&without_omit).is_err(),
        "dropping --omit=optional must fail the gate"
    );

    // A commented-out command carrying every flag must not satisfy the gate:
    // there is no active `npm ci` to check.
    let commented_only = "# npm ci --ignore-scripts --omit=optional --legacy-peer-deps\n";
    assert!(
        check_bridge_install_flags(commented_only).is_err(),
        "a commented-only npm ci must not satisfy the gate"
    );

    // A commented valid command must not mask a later active command missing a
    // flag: every active invocation is inspected.
    let masked = "# npm ci --ignore-scripts --omit=optional --legacy-peer-deps\n\
                  npm ci --ignore-scripts --omit=optional\n";
    assert!(
        check_bridge_install_flags(masked).is_err(),
        "a commented valid npm ci must not mask an active invalid one"
    );

    // A second, bare `npm ci` with no flags must fail even when a valid
    // invocation precedes it: every active invocation is inspected, flagged or
    // not (permanent control for the previous `contains("npm ci --")` bypass).
    let bare_second = "npm ci --ignore-scripts --omit=optional --legacy-peer-deps\nnpm ci\n";
    assert!(
        check_bridge_install_flags(bare_second).is_err(),
        "an active bare npm ci must not hide behind a valid one"
    );

    // A mandatory flag that exists only in a trailing shell comment is never
    // passed to npm and must not satisfy the gate (permanent control for the
    // previous whole-line `contains(flag)` bypass).
    let flags_in_comment = "npm ci --ignore-scripts # --omit=optional --legacy-peer-deps\n";
    assert!(
        check_bridge_install_flags(flags_in_comment).is_err(),
        "a mandatory flag only in a trailing comment must not satisfy the gate"
    );

    // A pi lock entry with its integrity removed must fail the integrity gate.
    let mut pi_lock = read_json("images/tools/pi/package-lock.json");
    if let Some(packages) = pi_lock["packages"].as_object_mut() {
        for (key, value) in packages.iter_mut() {
            if key.is_empty() {
                continue;
            }
            if let Some(entry) = value.as_object_mut() {
                entry.remove("integrity");
                break;
            }
        }
    }
    assert!(
        check_lock_integrity(&pi_lock).is_err(),
        "a lock entry without integrity must fail the gate"
    );

    // A pi lock whose nested sibling layout is neither empty nor the reviewed
    // shrinkwrap set must fail the sibling-set gate, and so must a reviewed
    // sibling at the wrong version. Build the fixtures rather than mutating the
    // committed lock: Pi 1.0.4+ legitimately has no nested siblings at all.
    const SIBLING_PREFIX: &str =
        "node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/";
    let nested_lock = |versions: &[(&str, &str)]| {
        let mut packages = serde_json::Map::new();
        for (name, version) in versions {
            packages.insert(
                format!("{SIBLING_PREFIX}{name}"),
                serde_json::json!({ "version": version }),
            );
        }
        serde_json::json!({ "packages": Value::Object(packages) })
    };
    let pin = pi_pin();
    let full: Vec<(&str, &str)> = ["chord", "pi-agent-core", "pi-ai", "pi-telemetry", "pi-tui"]
        .iter()
        .map(|name| (*name, pin.as_str()))
        .collect();
    // The empty layout (Pi 1.0.4+) and the complete reviewed shrinkwrap layout
    // both pass; a wrong version or a different nonempty set does not.
    assert!(
        check_pi_siblings(&nested_lock(&[])).is_ok(),
        "an empty nested sibling set is the Pi 1.0.4+ layout and must pass"
    );
    assert!(
        check_pi_siblings(&nested_lock(&full)).is_ok(),
        "the complete reviewed shrinkwrap layout must pass"
    );
    let mut wrong_version = full.clone();
    wrong_version[4].1 = "0.0.0";
    assert!(
        check_pi_siblings(&nested_lock(&wrong_version)).is_err(),
        "a nested sibling at the wrong version must fail the gate"
    );
    assert!(
        check_pi_siblings(&nested_lock(&full[..4])).is_err(),
        "a different nonempty nested sibling set must fail the gate"
    );

    // An aliased package inserted into the bridge lock must fail the gate.
    let mut bridge_lock = read_json("images/tools/pi/bridge/package-lock.json");
    if let Some(packages) = bridge_lock["packages"].as_object_mut() {
        packages.insert(
            "node_modules/@earendil-works/pi-ai".to_string(),
            serde_json::json!({ "version": "0.0.0" }),
        );
    }
    assert!(
        check_no_loader_aliased_pi_package(&bridge_lock).is_err(),
        "an aliased bridge package must fail the gate"
    );

    // A dsh input whose bytes no longer match the reviewed pins must fail the
    // graph gate before any structural claim is made about it.
    let stale = DshFixture::copy("stale-pin");
    stale.rewrite("package.json", |text| {
        text.replacen("\"0.1.5-rc.2\"", "\"0.1.5-rc.1\"", 1)
    });
    let error = match read_dsh_graph(&stale.project) {
        Ok(_) => panic!("a stale dsh input pin must fail the graph gate"),
        Err(error) => error,
    };
    assert!(
        error.contains("package.json: bytes differ from reviewed pin"),
        "the digest gate, not a downstream rule, must reject a stale input: {error:?}"
    );

    // A recipe `ENV PATH` that drops `${PATH}` must fail the additive-path gate.
    assert!(check_additive_path("ENV PATH=/opt/go/bin:${PATH}\n", "fixture").is_ok());
    assert!(
        check_additive_path("ENV PATH=/opt/go/bin\n", "fixture").is_err(),
        "a non-additive ENV PATH must fail the gate"
    );
    // A commented non-additive line is inert and must not trip the gate.
    assert!(
        check_additive_path("# ENV PATH=/opt/go/bin\n", "fixture").is_ok(),
        "a commented ENV PATH must not be inspected"
    );
    // A decoy that inherits the real PATH in an unrelated field must not satisfy
    // the guard while PATH itself is overwritten (permanent control for the
    // previous whole-line `${PATH}` substring bypass).
    assert!(
        check_additive_path("ENV PATH=/opt/custom OTHER=${PATH}\n", "fixture").is_err(),
        "a sibling field carrying ${{PATH}} must not make PATH additive"
    );
    assert!(
        check_additive_path("ENV OTHER=${PATH} PATH=/opt/custom\n", "fixture").is_err(),
        "a non-additive PATH after a sibling assignment must fail"
    );
    // An `ENV` form this guard cannot parse is rejected rather than skipped.
    assert!(
        check_additive_path("ENV PATH /opt/custom\n", "fixture").is_err(),
        "an unparseable ENV form must be rejected"
    );

    // Commenting a version label must drop it from the active set.
    let copilot = standard_dockerfile();
    let commented = copilot.replace(
        "LABEL org.agent-vm.version.copilot=",
        "# LABEL org.agent-vm.version.copilot=",
    );
    assert!(
        !active_version_labels(&commented).contains(&"org.agent-vm.version.copilot".to_string()),
        "a commented label must fail the label guard"
    );
}

/// The `sh <script>` command each recipe phase must run. dsh is the migrated
/// pnpm consumer, so both of its files come from the bound source directory.
fn dsh_or_npm_invocations(tool: &str) -> [(&'static str, String); 2] {
    if tool == "dsh" {
        [
            ("install", "sh /tmp/dsh-source/install-dsh.sh".to_string()),
            ("verify", "sh /tmp/dsh-source/verify-dsh.sh".to_string()),
        ]
    } else {
        [
            ("install", format!("sh /tmp/install-{tool}.sh")),
            ("verify", format!("sh /tmp/verify-{tool}.sh")),
        ]
    }
}

/// The dsh block binds its own helper/validator/pnpm sources because the layer
/// is a reviewed pnpm project, not a single copied installer.
fn check_dsh_source_block(active: &str) -> Result<(), String> {
    let copy = "COPY tools/dsh/package.json tools/dsh/pnpm-lock.yaml tools/dsh/pnpm-workspace.yaml tools/dsh/pnpm-inputs.sha256 /opt/agent-vm/dsh/";
    if active.matches(copy).count() != 1 {
        return Err("dsh needs exactly one four-input COPY".into());
    }
    for (phase, script) in [
        ("install", "sh /tmp/dsh-source/install-dsh.sh"),
        ("verify", "sh /tmp/dsh-source/verify-dsh.sh"),
    ] {
        let instruction = active
            .lines()
            .find(|line| line.contains(script))
            .ok_or_else(|| format!("missing {phase}-dsh invocation"))?;
        for token in [
            "--mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro",
            "--mount=type=bind,source=tools/dsh,target=/tmp/dsh-source,ro",
            "--mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro",
            "AGENT_INSTALL_SOFT_FAIL=",
            "AGENT_VM_DSH_SOURCE_DIR=/tmp/dsh-source",
            "PNPM_SOURCE_DIR=/tmp/pnpm-source",
        ] {
            if !instruction.split_whitespace().any(|word| word == token) {
                return Err(format!("dsh {phase} RUN misses {token}"));
            }
        }
    }
    let link =
        "ln -s /opt/agent-vm/dsh/node_modules/@deepseek-ai/dsh/lib/bin.js /usr/local/bin/dsh";
    let link_run = active
        .lines()
        .find(|line| line.contains(link))
        .ok_or("missing the dsh direct bin link")?;
    if active.matches(link).count() != 1 {
        return Err("dsh needs exactly one direct bin link".into());
    }
    for guard in [
        "test ! -e /usr/local/bin/dsh",
        "test ! -L /usr/local/bin/dsh",
    ] {
        if !link_run.contains(guard) {
            return Err(format!("dsh link RUN misses {guard}"));
        }
    }
    if active.contains(".bin/dsh") {
        return Err(
            "dsh must link the installed lib entrypoint, not a generated .bin wrapper".into(),
        );
    }
    Ok(())
}

// Narrow presence checks over active continued instructions, not a layer parser.
fn check_standard_blocks(text: &str) -> Result<(), String> {
    let active = text
        .lines()
        .filter(|l| !l.trim_start().starts_with('#'))
        .collect::<Vec<_>>()
        .join("\n")
        .replace("\\\n", " ");
    if active
        .lines()
        .filter(|l| l.trim_start().starts_with("FROM "))
        .count()
        != 1
    {
        return Err("standard must have one FROM".into());
    }
    for tool in RECIPES {
        for (phase, script) in dsh_or_npm_invocations(tool) {
            if !active.contains(&script) {
                return Err(format!("missing {phase}-{tool} invocation"));
            }
        }
    }
    check_dsh_source_block(&active)?;
    if !active.contains("sh /tmp/verify-standard.sh") {
        return Err("missing final gate".into());
    }
    if active.contains("ARG AGENT_INSTALL_SOFT_FAIL")
        || active.contains("SKIP_AGENT")
        || active.contains("skip-agent")
    {
        return Err("degraded interface".into());
    }
    if active.contains("source=contract") || !active.contains("source=recipe-contract") {
        return Err("noncanonical helpers".into());
    }
    for instruction in active
        .lines()
        .filter(|l| l.trim_start().starts_with("RUN "))
    {
        if (instruction.contains("sh /tmp/recipe-contract/run-install.sh")
            || instruction.contains("sh /tmp/verify-"))
            && !instruction.contains("source=recipe-contract,target=/tmp/recipe-contract,ro")
        {
            return Err("installer/audit RUN must bind canonical helpers read-only".into());
        }
    }
    Ok(())
}
#[test]
fn standard_blocks_and_mutations() {
    let text = standard_dockerfile();
    check_standard_blocks(&text).unwrap();
    for tool in RECIPES {
        assert!(!repo_path(&format!("images/tools/{tool}/Dockerfile")).exists());
        assert!(!repo_path(&format!("images/tools/{tool}/contract")).exists());
        for (phase, script) in dsh_or_npm_invocations(tool) {
            assert!(text.contains(&script), "{phase}-{tool} must be invoked");
            assert!(
                check_standard_blocks(&text.replacen(&script, "true", 1)).is_err(),
                "{phase}-{tool}"
            );
        }
    }
    assert!(check_standard_blocks(&text.replace("sh /tmp/verify-standard.sh", "true")).is_err());
    assert!(
        check_standard_blocks(&text.replacen("source=recipe-contract", "source=missing", 1))
            .is_err()
    );
    // Every dsh RUN must bind its reviewed sources; the runtime verifier cannot
    // borrow another RUN's mount (or the final gate's) and stay certified.
    for mount in [
        "--mount=type=bind,source=tools/dsh,target=/tmp/dsh-source,ro",
        "--mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro",
    ] {
        let mutated = text.replace(mount, "--mount=type=bind,source=missing,ro");
        assert_ne!(mutated, text, "the mutation must change the mount");
        assert!(check_standard_blocks(&mutated).is_err(), "missing {mount}");
    }
    let without_soft_unset = text.replacen(
        "AGENT_INSTALL_SOFT_FAIL= \\\n",
        "AGENT_INSTALL_SOFT_FAIL=1 \\\n",
        1,
    );
    assert_ne!(without_soft_unset, text);
    assert!(
        check_standard_blocks(&without_soft_unset).is_err(),
        "a soft-fail input must be rejected"
    );
    let link = "RUN test ! -e /usr/local/bin/dsh && test ! -L /usr/local/bin/dsh && \\\n    ln -s /opt/agent-vm/dsh/node_modules/@deepseek-ai/dsh/lib/bin.js /usr/local/bin/dsh\n";
    assert!(text.contains(link), "the direct link RUN is discoverable");
    assert!(
        check_standard_blocks(&text.replacen(link, "RUN true\n", 1)).is_err(),
        "a missing direct bin link must be rejected"
    );
    assert!(
        check_standard_blocks(&text.replacen(
            "ln -s /opt/agent-vm/dsh/node_modules/@deepseek-ai/dsh/lib/bin.js /usr/local/bin/dsh",
            "ln -s /opt/agent-vm/dsh/node_modules/.bin/dsh /usr/local/bin/dsh",
            1,
        ))
        .is_err(),
        "a generated .bin wrapper must be rejected"
    );
}

#[test]
fn base_pnpm_pin_and_launcher_ownership() {
    let pin = read_json("images/pnpm/pin.json");
    assert_eq!(pin["version"], "11.11.0");
    assert_eq!(
        pin["url"],
        "https://registry.npmjs.org/pnpm/-/pnpm-11.11.0.tgz"
    );
    assert_eq!(
        pin["sha512"],
        "4463f65fd80ed80d69bc1d4bf163ee94f605c7380fc318bb5b2ebe15f8cd12d49c51a4d59e951b401e764d3b6ca751cbf51bc50ed7001a6bcb4935e684c34882"
    );
    assert_eq!(
        pin["bundleSha256"],
        "e8abc02d5fbffb8f9d8817d44d6eee84c379ea8d5006dd5aa8694bb5e4428d8a"
    );
    assert_eq!(
        read_json("images/tools/dsh/package.json")["packageManager"],
        format!(
            "pnpm@{}",
            pin["version"]
                .as_str()
                .expect("the base pin carries a version")
        ),
        "the dsh manifest asserts the base pin instead of selecting a manager"
    );
    let manifest = read_json("images/tools/dsh/package.json");
    assert!(
        manifest["dependencies"].get("pnpm").is_none(),
        "the dsh project must not depend on pnpm"
    );
    let graph = read_dsh_graph(&DshProjectFiles::committed())
        .expect("the reviewed dsh inputs form a valid graph");
    assert!(
        !graph.carries("pnpm"),
        "the dsh graph must not carry a private pnpm"
    );
    assert!(
        !graph
            .snapshots
            .iter()
            .any(|entry| entry.key.starts_with("pnpm@")),
        "no dsh snapshot may be a pnpm identity"
    );
    assert!(!standard_dockerfile().contains("org.agent-vm.version.pnpm"));
    assert!(!standard_dockerfile().contains(".bin/pnpm /usr/local/bin/pnpm"));
    let launcher = read_text("images/pnpm/launcher.sh");
    assert!(launcher.starts_with("#!/bin/sh\nset -eu\n"));
    for (key, value) in [
        ("MINIMUM_RELEASE_AGE", "1440"),
        ("MINIMUM_RELEASE_AGE_STRICT", "true"),
        ("MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME", "false"),
        ("TRUST_POLICY", "no-downgrade"),
        ("IGNORE_SCRIPTS", "true"),
    ] {
        assert!(launcher.contains(&format!(": \"${{PNPM_CONFIG_{key}:={value}}}\"")));
        assert!(
            launcher
                .lines()
                .filter(|line| line.starts_with("export "))
                .any(|line| line
                    .split_whitespace()
                    .any(|word| word == format!("PNPM_CONFIG_{key}")))
        );
    }
    assert_eq!(launcher.matches(":=").count(), 5);
    assert!(launcher.contains("pnpm) exec /usr/bin/node /opt/agent-vm/pnpm/bin/pnpm.mjs \"$@\""));
    assert!(
        launcher.contains("pnx|pnpx) exec /usr/bin/node /opt/agent-vm/pnpm/bin/pnpx.mjs \"$@\"")
    );
    let bootstrap = read_text("images/pnpm/bootstrap.py");
    assert!(!bootstrap.contains("patch"));
    assert!(!bootstrap.contains("write_text"));
}

fn check_base_pnpm_sources(
    base: &str,
    standard: &str,
    launcher: &str,
    pin: &Value,
) -> Result<(), String> {
    let version = pin["version"].as_str().ok_or("missing base version")?;
    if base.matches("org.agent-vm.version.pnpm=").count() != 1
        || !base.contains(&format!("LABEL org.agent-vm.version.pnpm=\"{version}\""))
        || standard.contains("org.agent-vm.version.pnpm")
        || standard.contains(".bin/pnpm /usr/local/bin/pnpm")
    {
        return Err("pnpm selection/link ownership changed".into());
    }
    for recipe in [base, standard] {
        for command in [
            "/usr/bin/npm",
            "/usr/bin/npx",
            "/usr/local/bin/npm",
            "/usr/local/bin/npx",
        ] {
            if recipe.contains(command) {
                return Err("native npm/npx replacement".into());
            }
        }
    }
    for (key, value) in [
        ("MINIMUM_RELEASE_AGE", "1440"),
        ("MINIMUM_RELEASE_AGE_STRICT", "true"),
        ("MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME", "false"),
        ("TRUST_POLICY", "no-downgrade"),
        ("IGNORE_SCRIPTS", "true"),
    ] {
        if !launcher.contains(&format!(": \"${{PNPM_CONFIG_{key}:={value}}}\""))
            || !launcher
                .lines()
                .filter(|line| line.starts_with("export "))
                .any(|line| {
                    line.split_whitespace()
                        .any(|word| word == format!("PNPM_CONFIG_{key}"))
                })
        {
            return Err(format!("missing caller-preserving default/export: {key}"));
        }
    }
    if !launcher.contains("pnx|pnpx) exec /usr/bin/node /opt/agent-vm/pnpm/bin/pnpx.mjs \"$@\"") {
        return Err("alias dispatch changed".into());
    }
    if pin["sha512"]
        != "4463f65fd80ed80d69bc1d4bf163ee94f605c7380fc318bb5b2ebe15f8cd12d49c51a4d59e951b401e764d3b6ca751cbf51bc50ed7001a6bcb4935e684c34882"
        || pin["bundleSha256"] != "e8abc02d5fbffb8f9d8817d44d6eee84c379ea8d5006dd5aa8694bb5e4428d8a"
    {
        return Err("reviewed digests changed".into());
    }
    Ok(())
}

#[test]
fn base_pnpm_source_mutations_are_rejected() {
    let base = read_text("images/Dockerfile");
    let standard = standard_dockerfile();
    let launcher = read_text("images/pnpm/launcher.sh");
    let pin = read_json("images/pnpm/pin.json");
    assert!(check_base_pnpm_sources(&base, &standard, &launcher, &pin).is_ok());
    for mutated in [
        launcher.replace("export PNPM_CONFIG_IGNORE_SCRIPTS", "# omitted export"),
        launcher.replace(
            ": \"${PNPM_CONFIG_IGNORE_SCRIPTS:=true}\"",
            "PNPM_CONFIG_IGNORE_SCRIPTS=true",
        ),
        launcher.replace(
            ": \"${PNPM_CONFIG_MINIMUM_RELEASE_AGE:=1440}\"",
            "PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440",
        ),
        launcher.replace(
            "pnx|pnpx) exec /usr/bin/node /opt/agent-vm/pnpm/bin/pnpx.mjs",
            "pnx|pnpx) exec /usr/bin/node /opt/agent-vm/pnpm/bin/pnpm.mjs",
        ),
    ] {
        assert!(check_base_pnpm_sources(&base, &standard, &mutated, &pin).is_err());
    }
    for addition in [
        "\nLABEL org.agent-vm.version.pnpm=\"11.11.0\"",
        "\nRUN ln -s /opt/agent-vm/dsh/node_modules/.bin/pnpm /usr/local/bin/pnpm",
        "\nRUN ln -s /usr/local/bin/pnpm /usr/bin/npm",
        "\nRUN ln -s /usr/local/bin/pnx /usr/bin/npx",
    ] {
        assert!(
            check_base_pnpm_sources(&base, &(standard.clone() + addition), &launcher, &pin)
                .is_err()
        );
    }
    assert!(
        check_base_pnpm_sources(
            &base.replace("pnpm=\"11.11.0\"", "pnpm=\"11.12.0\""),
            &standard,
            &launcher,
            &pin
        )
        .is_err()
    );
    for key in ["version", "sha512", "bundleSha256"] {
        let mut mutated = pin.clone();
        mutated[key] = Value::String("mutated".into());
        assert!(check_base_pnpm_sources(&base, &standard, &launcher, &mutated).is_err());
    }
}

#[test]
fn opencode_pnpm_inputs_are_pinned_bytes() {
    use serde_json::json;
    let manifest = read_json("images/tools/opencode/plugins/package.json");
    let sdk = "@anthropic-ai/claude-agent-sdk";
    let plugin = "@openchamber/opencode-claude";
    let version = manifest["dependencies"][plugin].as_str().unwrap();
    assert!(is_exact_semver(version));
    assert_eq!(
        manifest,
        json!({"name":"agent-vm-guest-opencode-plugins", "version":"0.0.0", "private":true,
            "type":"module", "packageManager":"pnpm@11.11.0", "dependencies":{sdk:"0.3.281",plugin:version}})
    );
    // OpenCode keeps three reviewed byte inputs and never adopts the shared
    // graph parser. Its digests are checked by explicit Node/Python suites, so
    // Rust owns only its inventory/path/source boundaries; the dsh graph guards
    // are the ones that run the shared CLI as a syntax adapter.
    let mut names = Vec::new();
    for line in read_text("images/tools/opencode/plugins/pnpm-inputs.sha256").lines() {
        let (digest, name) = line
            .split_once("  ")
            .unwrap_or_else(|| panic!("pin line is not '<digest>  <name>': {line:?}"));
        assert_eq!(line.split("  ").count(), 2, "{line:?}");
        assert_eq!(digest.len(), 64, "{line:?}");
        assert!(
            digest
                .chars()
                .all(|c| c.is_ascii_digit() || ('a'..='f').contains(&c)),
            "{line:?}"
        );
        names.push(name.to_string());
    }
    names.sort();
    assert_eq!(
        names,
        ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"]
    );
    for file in ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"] {
        assert!(
            repo_path(&format!("images/tools/opencode/plugins/{file}")).is_file(),
            "missing {file}"
        );
    }
    check_pnpm_primitive_inventory();
    for file in [
        "images/tools/opencode/plugins/check-lock.mjs",
        "images/tools/opencode/plugins/pin-pnpm-inputs.mjs",
        "images/tools/opencode/plugins/prepare-lock.sh",
        "images/tools/opencode/plugins/upgrade-plugins.sh",
        "images/tools/opencode/install-opencode-plugins.sh",
    ] {
        assert!(
            check_opencode_parser_boundary(&read_text(file)).is_ok(),
            "{file}"
        );
    }
    let dockerfile = standard_dockerfile();
    assert_eq!(
        dockerfile_arg(&dockerfile, "AGENT_VERSION_OPENCODE_CLAUDE"),
        ""
    );
    assert_eq!(
        label_value(&dockerfile, "org.agent-vm.version.opencode-claude"),
        format!("${{AGENT_VERSION_OPENCODE_CLAUDE:-{version}}}")
    );
    assert!(dockerfile.contains(
        "COPY tools/opencode/plugins/pnpm-inputs.sha256 /opt/agent-vm/opencode-plugins/pnpm-inputs.sha256"
    ));
    assert!(
        !dockerfile.contains("pin-pnpm-inputs.mjs"),
        "the developer-only re-pin tool must not be copied into an image"
    );
    let pnpm = read_json("images/pnpm/pin.json");
    assert_eq!(
        manifest["packageManager"],
        format!("pnpm@{}", pnpm["version"].as_str().unwrap())
    );
    let install = read_text("images/tools/opencode/install-opencode-plugins.sh");
    for required in [
        "/usr/local/bin/pnpm",
        "--frozen-lockfile --prod --no-optional",
        "--ignore-scripts --ignore-pnpmfile --pm-on-fail=error",
        "--package-import-method=copy",
        "run-pnpm.sh",
        "check-pnpm-environment.mjs",
    ] {
        assert!(install.contains(required), "missing {required}");
    }
    for forbidden in [
        "npm ci",
        "npm install",
        "pnx",
        "corepack",
        "trust-lockfile",
        "trust-policy=off",
    ] {
        assert!(!install.contains(forbidden), "forbidden {forbidden}");
    }
    for file in [
        "images/tools/opencode/opencode.json",
        "images/tools/opencode/probe-opencode.py",
        "images/tools/opencode/check-claude-runtime.mjs",
        "images/tools/opencode/verify-opencode-plugins.sh",
        "images/recipe-contract/run-pnpm.sh",
        "images/tools/opencode/plugins/pnpm-inputs.sha256",
        "images/tools/opencode/plugins/pin-pnpm-inputs.mjs",
    ] {
        assert!(repo_path(file).is_file(), "missing {file}");
    }
}

fn check_opencode_parser_boundary(source: &str) -> Result<(), String> {
    for forbidden in ["pnpm-lock.mjs", "vendor/yaml"] {
        if source.contains(forbidden) {
            return Err(format!("OpenCode must not adopt {forbidden}"));
        }
    }
    Ok(())
}

fn check_pnpm_primitive_inventory() {
    use std::collections::BTreeSet;
    let vendor = repo_path("images/recipe-contract/vendor/yaml");
    let mut expected = BTreeSet::new();
    for line in read_text("images/recipe-contract/vendor/yaml/upstream.sha256").lines() {
        let (_, relative) = line.split_once("  ").expect("retained-file manifest line");
        assert!(
            relative == "LICENSE"
                || relative == "package.json"
                || (relative.starts_with("dist/") && relative.ends_with(".js"))
        );
        assert!(!Path::new(relative).is_absolute());
        assert!(
            Path::new(relative)
                .components()
                .all(|part| matches!(part, std::path::Component::Normal(_)))
        );
        assert!(
            expected.insert(relative.to_string()),
            "duplicate retained path"
        );
    }
    expected.extend(["README.md".to_string(), "upstream.sha256".to_string()]);
    fn inventory(root: &Path, directory: &Path, paths: &mut BTreeSet<String>) {
        for entry in std::fs::read_dir(directory).unwrap() {
            let entry = entry.unwrap();
            let file_type = entry.file_type().unwrap();
            assert!(!file_type.is_symlink(), "vendor symlink substitution");
            if file_type.is_dir() {
                inventory(root, &entry.path(), paths);
            } else {
                assert!(file_type.is_file());
                paths.insert(
                    entry
                        .path()
                        .strip_prefix(root)
                        .unwrap()
                        .to_str()
                        .unwrap()
                        .to_string(),
                );
            }
        }
    }
    let mut actual = BTreeSet::new();
    inventory(&vendor, &vendor, &mut actual);
    assert_eq!(actual, expected);
    assert!(
        read_text("images/recipe-contract/pnpm-lock.mjs").contains("./vendor/yaml/dist/index.js")
    );
    for file in [
        "images/recipe-contract/pnpm-lock.mjs",
        "script/test/pnpm-vendor.mjs",
        "script/test/pnpm-lock-inputs.mjs",
    ] {
        assert!(repo_path(file).is_file(), "missing primitive {file}");
    }
    for recipe in ["images/Dockerfile", "images/standard/Dockerfile"] {
        for line in read_text(recipe).lines() {
            if line.trim_start().starts_with("COPY ") || line.trim_start().starts_with("ADD ") {
                assert!(
                    !line.contains("recipe-contract"),
                    "build helpers must not become runtime payloads"
                );
                assert!(!line.contains("pnpm-lock.mjs") && !line.contains("vendor/yaml"));
            }
        }
    }
}

#[test]
fn opencode_rejects_parser_adoption_mutations() {
    let checker = read_text("images/tools/opencode/plugins/check-lock.mjs");
    assert!(check_opencode_parser_boundary(&checker).is_ok());
    for import in [
        "import './pnpm-lock.mjs';",
        "import YAML from '../../../recipe-contract/vendor/yaml/dist/index.js';",
    ] {
        assert!(check_opencode_parser_boundary(&format!("{checker}\n{import}")).is_err());
    }
}
