package closure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/internal/strictjson"
)

// The loader. Its rule table, in the order the rules fire. Every rule names the
// test that fails when it is deleted (run for every row in the #249 PR), so a
// deletion mutation that leaves the suite green is a rule without a witness. The
// Python reader (examples/layers/go-dev/gopls_closure.py at 335fd30) is the
// oracle until #254; the line ranges are that file's, and its own verdicts on the
// same mutants are recorded in script/test/gopls-closure-parity.py.
//
//	rule                                               Python                          test
//	directory used in the caller's spelling           load_closure 1035 (Path)        main: TestCheckUsesTheRequestedSpelling
//	input set: five regular files, no symlink          load_closure 1035-1060          TestInputShapeRulesRefuse, TestSymlinkedDataDirectoryRefuses
//	pin schema and closed key sets                     load_closure 1070-1090, 895-909 TestPinShapeRulesRefuse
//	four content digests, pin excluded                 load_closure 1091-1093          TestContentDigestRulesRefuse
//	wrapper canonical form + tool/version              parse_wrapper_go_mod 519-573    TestWrapperRulesRefuse
//	both sums per selected module                      parse_go_sum 616-649            TestGoSumRulesRefuse
//	root .mod evidence h1                              h1_go_mod 667-672               TestRootEvidenceH1RefusesMismatch
//	root .mod evidence grammar (G3)                    parse_root_go_mod 575-612       TestRootEvidenceGrammarRefuses, TestRootEvidenceReadsTabSeparatedDirectives, TestRootEvidenceAgreesWithTheGoCommand
//	adoption subset at matching root versions          check_adoption_restriction 1212 TestAdoptionRuleRefuses
//	compiler/language relation                         load_closure 1130-1136          TestGoLanguageRelationRefuses
//	module list uniqueness, one version per path (G3)  load_closure 1148-1165          TestModuleUniquenessRefuses
//	notice framing and identity (G3)                   parse_licenses 683-729          TestNoticeFramingRefuses, TestNoticeIdentityRefuses
//	dated scoped review                                _parse_review 957-1033          TestReviewScopeRefuses
//	optional Docker ARG assertions                     load_closure 1195-1200, 1714    TestExpectationAssertionsRefuse
//	status and the printed release                     main 1738-1765                  TestStatusMatrix, TestCheckMakesNoRequest
//
// Rules that the Python reader applies twice are not ported twice: the root
// module's zip h1 and its version are already decided by the per-module sum loop
// and by the chained wrapper/go.mod equalities, so the two extra passes Python
// runs after them are duplicates, not defence in depth (#188 R-c). The signature
// inventory's scope is one rule and lives in the review test.
// Data is a validated closure: the record, the bytes it binds, and the selection.
type Data struct {
	// Directory is where these bytes were read from, in the caller's spelling, when
	// the source can name one. The installer re-reads the committed inputs from it
	// and re-checks their digests before it copies or publishes them, so the copy it
	// builds from cannot be a different tree than the one that was verified here.
	Directory string
	Pin       Pin
	Wrapper   WrapperGoMod
	Sums      GoSum
	Root      RootGoMod
	Notices   []LicenseSection
}

// Selection is the validated module set, in pin order.
func (d *Data) Selection() []ModuleID {
	selection := make([]ModuleID, 0, len(d.Pin.Modules))
	for _, module := range d.Pin.Modules {
		selection = append(selection, module.ID)
	}
	return selection
}

// Request is one `check`: the optional assertions the Dockerfile's ARGs become.
// The data comes from the Source, so the directory is named once.
type Request struct {
	// Pointers, so `--expect-gopls ''` is an assertion that fails rather than
	// "unset". The Python reader treats an empty string as unset (argparse's
	// default is None and it tests truthiness); the parity suite records that
	// difference as a declared divergence.
	ExpectGopls *ReleaseVersion
	ExpectGo    *GoVersion
}

// Load reads and verifies the closure. It runs no Go command and makes no network
// request: `check` is a pure read of the committed bytes, which is why it can run
// under `--network=none` with only the data and the binary mounted.
func Load(ctx context.Context, request Request, source Source) (*Data, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := readInputs(source)
	if err != nil {
		return nil, err
	}

	document, err := decodeJSON(raw[FilePin], string(FilePin))
	if err != nil {
		return nil, err
	}
	pin, err := parsePin(document, raw)
	if err != nil {
		return nil, err
	}

	wrapper, err := parseWrapperGoMod(raw[FileGoMod], string(FileGoMod))
	if err != nil {
		return nil, err
	}
	if err := requireCanonicalWrapper(wrapper, raw[FileGoMod]); err != nil {
		return nil, err
	}
	rootVersion, present := wrapper.Requires[ModulePath(Gopls)]
	if !present {
		return nil, refuse("go.mod: the tool module %s is not required", Gopls)
	}
	if string(rootVersion) != string(pin.Gopls) {
		return nil, refuse("go.mod: %s is %s, not %s", Gopls, rootVersion, pin.Gopls)
	}

	sums, err := parseGoSum(raw[FileGoSum], string(FileGoSum))
	if err != nil {
		return nil, err
	}
	root, err := parseRootGoMod(raw[FileRootGoMod], string(FileRootGoMod))
	if err != nil {
		return nil, err
	}
	rootID := ModuleID{Path: ModulePath(Gopls), Version: ModuleVersion(pin.Gopls)}
	rootSum, present := sums.Mod[rootID]
	if !present {
		return nil, refuse("go.sum lacks go.mod hash for %s", rootID)
	}
	if h1GoMod(raw[FileRootGoMod]) != rootSum {
		return nil, refuse("root go.mod evidence does not match go.sum")
	}

	if err := requireAdoption(wrapper.Requires, root.Requires); err != nil {
		return nil, err
	}
	if wrapper.Go != root.Go {
		return nil, refuse("go directive %s does not equal the root module's %s", wrapper.Go,
			root.Go)
	}
	if !languageAtMost(wrapper.Go, pin.Go) {
		return nil, refuse("go directive %s is newer than the pinned compiler go%s", wrapper.Go,
			pin.Go)
	}
	if err := requireSelectedSums(pin.Modules, wrapper.Requires, sums); err != nil {
		return nil, err
	}

	notices, err := parseLicenses(raw[FileLicenses], string(FileLicenses))
	if err != nil {
		return nil, err
	}
	if err := requireNoticeIdentity(pin, notices, raw[FileLicenses]); err != nil {
		return nil, err
	}
	if err := requireProvenance(pin.Provenance, pin.Gopls); err != nil {
		return nil, err
	}
	if err := requireSignatureScope(pin.Review.Signatures, pin.Modules); err != nil {
		return nil, err
	}
	if err := requireExpectations(request, pin); err != nil {
		return nil, err
	}
	data := &Data{Pin: pin, Wrapper: wrapper, Sums: sums, Root: root, Notices: notices}
	if locator, ok := source.(Locator); ok {
		data.Directory = locator.Directory()
	}
	return data, nil
}

// readInputs enforces the exact input set: every committed name present, nothing
// else in the directory, and each one a regular file that is not a symlink.
func readInputs(source Source) (map[Filename][]byte, error) {
	names, err := source.List()
	if err != nil {
		return nil, err
	}
	present := make(map[string]struct{}, len(names))
	for _, name := range names {
		present[name] = struct{}{}
	}
	for _, name := range DataFiles {
		if _, ok := present[string(name)]; !ok {
			return nil, refuse("missing input: %s", name)
		}
	}
	known := map[string]struct{}{}
	for _, name := range DataFiles {
		known[string(name)] = struct{}{}
	}
	extra := []string{}
	for _, name := range names {
		if _, ok := known[name]; !ok {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, refuse("unreviewed input: %s", extra[0])
	}
	raw := make(map[Filename][]byte, len(DataFiles))
	for _, name := range DataFiles {
		content, err := source.Read(name)
		if err != nil {
			return nil, err
		}
		raw[name] = content
	}
	return raw, nil
}

// parsePin reads the record and binds the four digests it carries.
func parsePin(document any, raw map[Filename][]byte) (Pin, error) {
	members, err := requireKeys(document, pinKeys, nil, string(FilePin))
	if err != nil {
		return Pin{}, err
	}
	schemaValue, err := fieldOf(members, "schema", string(FilePin))
	if err != nil {
		return Pin{}, err
	}
	schema, err := exactInt(schemaValue, "schema")
	if err != nil {
		return Pin{}, err
	}
	if schema != 1 {
		return Pin{}, refuse("pin.json: unsupported schema")
	}
	goplsValue, err := fieldOf(members, "gopls", string(FilePin))
	if err != nil {
		return Pin{}, err
	}
	gopls, err := parseReleaseVersion(goplsValue, "gopls")
	if err != nil {
		return Pin{}, err
	}
	goValue, err := fieldOf(members, "go", string(FilePin))
	if err != nil {
		return Pin{}, err
	}
	compiler, err := parseGoVersion(goValue, "go")
	if err != nil {
		return Pin{}, err
	}
	moduleValue, err := fieldOf(members, "module", string(FilePin))
	if err != nil {
		return Pin{}, err
	}
	module, err := exactString(moduleValue, "module")
	if err != nil {
		return Pin{}, err
	}
	if module != Wrapper {
		return Pin{}, refuse("pin.json: module must be %s", Wrapper)
	}
	filesValue, err := fieldOf(members, "files", string(FilePin))
	if err != nil {
		return Pin{}, err
	}
	fileMembers, err := requireKeys(filesValue, filenames(FileKeys), nil, "files")
	if err != nil {
		return Pin{}, err
	}
	files := map[Filename]SHA256{}
	for _, name := range FileKeys {
		digestValue, err := fieldOf(fileMembers, string(name), "files")
		if err != nil {
			return Pin{}, err
		}
		digest, err := parseSHA256(digestValue, "files."+string(name))
		if err != nil {
			return Pin{}, err
		}
		actual := sha256.Sum256(raw[name])
		if hex.EncodeToString(actual[:]) != string(digest) {
			return Pin{}, refuse("%s: bytes differ from the committed digest", name)
		}
		files[name] = digest
	}
	modules, err := parseModules(members)
	if err != nil {
		return Pin{}, err
	}
	provenanceValue, err := fieldOf(members, "provenance", string(FilePin))
	if err != nil {
		return Pin{}, err
	}
	provenance, err := parseProvenance(provenanceValue)
	if err != nil {
		return Pin{}, err
	}
	authValue, err := fieldOf(members, "authentication", string(FilePin))
	if err != nil {
		return Pin{}, err
	}
	review, err := parseReview(authValue, ModuleID{Path: ModulePath(Gopls),
		Version: ModuleVersion(gopls)}, files[FileGoMod], files[FileGoSum])
	if err != nil {
		return Pin{}, err
	}
	return Pin{Gopls: gopls, Go: compiler, Module: module, Files: files, Modules: modules,
		Provenance: provenance, Review: review}, nil
}

// parseModules reads the selected set and refuses what a path-keyed map would
// otherwise collapse: the same module twice, a module's notice names twice, and
// two versions of one module path (#188 G3). Every one of those used to reach
// the set comparisons as fewer entries than the record claimed.
func parseModules(members members) ([]PinnedModule, error) {
	value, err := fieldOf(members, "modules", string(FilePin))
	if err != nil {
		return nil, err
	}
	items, ok := value.([]strictjson.Value)
	if !ok || len(items) == 0 {
		return nil, refuse("pin.json: modules must be a non-empty list")
	}
	modules := make([]PinnedModule, 0, len(items))
	for _, item := range items {
		node, err := requireKeys(item, moduleKeys, nil, "modules")
		if err != nil {
			return nil, err
		}
		pathValue, err := fieldOf(node, "path", "modules")
		if err != nil {
			return nil, err
		}
		path, err := parseModulePath(pathValue, "modules.path")
		if err != nil {
			return nil, err
		}
		versionValue, err := fieldOf(node, "version", "modules")
		if err != nil {
			return nil, err
		}
		version, err := parseModuleVersion(versionValue, "modules.version")
		if err != nil {
			return nil, err
		}
		licensesValue, err := fieldOf(node, "licenses", "modules")
		if err != nil {
			return nil, err
		}
		licenseItems, ok := licensesValue.([]strictjson.Value)
		if !ok || len(licenseItems) == 0 {
			return nil, refuse("license evidence missing for %s %s", path, version)
		}
		licenses := make([]LicenseFile, 0, len(licenseItems))
		for _, licenseNode := range licenseItems {
			license, err := requireKeys(licenseNode, licenseKeys, nil, "modules.licenses")
			if err != nil {
				return nil, err
			}
			nameValue, err := fieldOf(license, "name", "modules.licenses")
			if err != nil {
				return nil, err
			}
			name, err := parseNoticePath(nameValue, "modules.licenses.name")
			if err != nil {
				return nil, err
			}
			digestValue, err := fieldOf(license, "sha256", "modules.licenses")
			if err != nil {
				return nil, err
			}
			digest, err := parseSHA256(digestValue, "modules.licenses.sha256")
			if err != nil {
				return nil, err
			}
			licenses = append(licenses, LicenseFile{Name: name, SHA256: digest})
		}
		h1Value, err := fieldOf(node, "h1", "modules")
		if err != nil {
			return nil, err
		}
		h1, err := parseH1(h1Value, "modules.h1")
		if err != nil {
			return nil, err
		}
		goModValue, err := fieldOf(node, "go_mod_h1", "modules")
		if err != nil {
			return nil, err
		}
		goModH1, err := parseH1(goModValue, "modules.go_mod_h1")
		if err != nil {
			return nil, err
		}
		modules = append(modules, PinnedModule{ID: ModuleID{Path: path, Version: version},
			H1: h1, GoModH1: goModH1, Licenses: licenses})
	}
	if err := requireModuleUniqueness(modules); err != nil {
		return nil, err
	}
	return modules, nil
}

// requireModuleUniqueness refuses what a path-keyed map collapses: the same
// module twice, two versions of one module path, and a version repeated inside a
// module's own notice list (#188 G3). The Python reader built
// `{module.id.path: module}` and then compared versions against the survivor, so
// a second version could hide behind the first.
func requireModuleUniqueness(modules []PinnedModule) error {
	seen := map[ModuleID]struct{}{}
	versions := map[ModulePath]ModuleVersion{}
	for _, module := range modules {
		if _, duplicate := seen[module.ID]; duplicate {
			return refuse("pin.json: duplicate module")
		}
		seen[module.ID] = struct{}{}
		version, present := versions[module.ID.Path]
		if !present {
			versions[module.ID.Path] = module.ID.Version
			continue
		}
		if version != module.ID.Version {
			return refuse("pin.json: %s is selected at both %s and %s; review required",
				module.ID.Path, version, module.ID.Version)
		}
	}
	paths := make([]string, 0, len(modules))
	for _, module := range modules {
		paths = append(paths, string(module.ID.Path))
	}
	if !sameStrings(paths, sortedCopy(paths)) {
		return refuse("pin.json: modules must be sorted by path")
	}
	return nil
}

// requireCanonicalWrapper is the byte comparison: a wrapper that parses but does
// not re-render is not the reviewed file.
func requireCanonicalWrapper(wrapper WrapperGoMod, raw []byte) error {
	rendered := renderWrapper(wrapper.Go, wrapper.Requires)
	if string(rendered) == string(raw) {
		return nil
	}
	return refuse("go.mod is not in the reviewed canonical form")
}

// requireAdoption is the reviewed adoption restriction of ADR 0008: every module
// the wrapper selects must be named by the root's own go.mod at that version. It
// is a property of this record, not a Go invariant.
func requireAdoption(wrapper, root map[ModulePath]ModuleVersion) error {
	paths := make([]string, 0, len(wrapper))
	for path := range wrapper {
		if path == ModulePath(Gopls) {
			continue
		}
		paths = append(paths, string(path))
	}
	sort.Strings(paths)
	const remediation = "current-pin adoption restriction changed; inspect upstream go.mod " +
		"and both Linux build lists; update the rule and ADR in a reviewed proposal"
	for _, name := range paths {
		path, version := ModulePath(name), wrapper[ModulePath(name)]
		required, present := root[path]
		if !present {
			return refuse("%s is not required by the root module; review required\n%s", path,
				remediation)
		}
		if required != version {
			return refuse("%s %s differs from the root module's requirement %s; review "+
				"required\n%s", path, version, required, remediation)
		}
	}
	return nil
}

// requireSelectedSums binds the pin's selection to both checksum kinds of every
// module.
func requireSelectedSums(modules []PinnedModule, wrapper map[ModulePath]ModuleVersion,
	sums GoSum) error {
	pinned := make(map[ModulePath]PinnedModule, len(modules))
	for _, module := range modules {
		pinned[module.ID.Path] = module
	}
	if len(pinned) != len(wrapper) {
		return refuse("pin modules disagree with go.mod: the reviewed sets must be identical")
	}
	for path, version := range wrapper {
		module, present := pinned[path]
		if !present {
			return refuse("pin modules disagree with go.mod: the reviewed sets must be " +
				"identical")
		}
		if module.ID.Version != version {
			return refuse("%s: pinned %s != go.mod %s", path, module.ID.Version, version)
		}
	}
	for _, module := range modules {
		zip, present := sums.Zip[module.ID]
		if !present {
			return refuse("go.sum lacks h1 for %s", module.ID)
		}
		mod, present := sums.Mod[module.ID]
		if !present {
			return refuse("go.sum lacks go.mod hash for %s", module.ID)
		}
		if zip != module.H1 {
			return refuse("%s h1 differs from go.sum", module.ID)
		}
		if mod != module.GoModH1 {
			return refuse("%s go.mod h1 differs from go.sum", module.ID)
		}
	}
	return nil
}

// requireNoticeIdentity binds the framed notice evidence to the pin's recorded
// names and digests, and refuses a layout outside the reviewed policy. Duplicate
// names are refused before either side is indexed (#188 G3): two rows for one
// notice used to collapse into one, so the set comparison balanced and the extra
// row was accepted.
func requireNoticeIdentity(pin Pin, notices []LicenseSection, raw []byte) error {
	text, err := decodeText(raw, string(FileLicenses))
	if err != nil {
		return err
	}
	headerRelease, err := headerVersion(text, string(FileLicenses))
	if err != nil {
		return err
	}
	if headerRelease != pin.Gopls {
		return refuse("license evidence header disagrees with the pinned release")
	}
	committed := make(map[noticeKey]SHA256, len(notices))
	for _, section := range notices {
		committed[noticeKey{id: section.ID, name: section.Name}] = section.SHA256
	}
	reviewed := map[noticeKey]SHA256{}
	for _, module := range pin.Modules {
		nested := reviewedNestedNotices(module.ID)
		names := make([]string, 0, len(module.Licenses))
		for _, license := range module.Licenses {
			names = append(names, string(license.Name))
		}
		roots, nestedNames := []string{}, []string{}
		for _, name := range names {
			if strings.Contains(name, "/") {
				nestedNames = append(nestedNames, name)
				continue
			}
			roots = append(roots, name)
		}
		if len(roots) == 0 || !sameStrings(sortedCopy(nestedNames), sortedCopy(nested)) {
			return refuse("%s: unsupported license layout; review notices and update the "+
				"ADR/layout policy before staging", module.ID)
		}
		for _, license := range module.Licenses {
			base := string(license.Name)
			if slash := strings.LastIndexByte(base, '/'); slash >= 0 {
				base = base[slash+1:]
			}
			if !noticeNamePattern.MatchString(base) {
				return refuse("%s: unsupported license layout; review notices and update the "+
					"ADR/layout policy before staging", module.ID)
			}
			key := noticeKey{id: module.ID, name: license.Name}
			if _, duplicate := reviewed[key]; duplicate {
				return refuse("pin.json: duplicate notice %s for %s; review required",
					license.Name, module.ID)
			}
			reviewed[key] = license.SHA256
		}
	}
	missing, extra := []string{}, []string{}
	for key := range reviewed {
		if _, present := committed[key]; !present {
			missing = append(missing, key.id.String()+" "+string(key.name))
		}
	}
	for key := range committed {
		if _, present := reviewed[key]; !present {
			extra = append(extra, key.id.String()+" "+string(key.name))
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		candidates := append(sortedCopy(missing), sortedCopy(extra)...)
		return refuse("license evidence does not match pin.json for %s", candidates[0])
	}
	for key, digest := range reviewed {
		if committed[key] != digest {
			return refuse("license evidence differs from the committed digest: %s %s",
				key.id.Path, key.name)
		}
	}
	return nil
}

// noticeKey is the identity of one notice: its module at one version and its
// path inside that module. The version is part of the key, so a notice cannot
// migrate between versions unnoticed.
type noticeKey struct {
	id   ModuleID
	name NoticePath
}

// requireProvenance binds the unauthenticated origin metadata to this release.
func requireProvenance(provenance Provenance, release ReleaseVersion) error {
	if provenance.Source != Proxy+"/"+Gopls+"/@v/"+string(release)+".info" {
		return refuse("provenance.source: not the canonical proxy .info origin for this release")
	}
	if provenance.Vcs != "git" || provenance.Hash == "" || provenance.Ref == "" {
		return refuse("provenance: incomplete unauthenticated origin record")
	}
	return nil
}

// requireSignatureScope is the structural half of the signature inventory: a
// record must name an identity this closure selects, at that version. An unlisted
// module makes no claim either way, and equality with today's source inventory is
// the stager's renewal comparison, not the checker's rule.
func requireSignatureScope(signatures []SignatureEvidence, modules []PinnedModule) error {
	selected := make(map[ModulePath]ModuleVersion, len(modules))
	for _, module := range modules {
		selected[module.ID.Path] = module.ID.Version
	}
	seen := map[ModuleID]struct{}{}
	for _, signature := range signatures {
		if _, duplicate := seen[signature.ID]; duplicate {
			return refuse("authentication.known_vcs_signatures: duplicate %s", signature.ID)
		}
		seen[signature.ID] = struct{}{}
		version, present := selected[signature.ID.Path]
		if !present {
			return refuse("signature inventory names %s, which the closure does not select; "+
				"review required", signature.ID)
		}
		if version != signature.ID.Version {
			return refuse("signature inventory names %s but the closure selects %s@%s; review "+
				"required", signature.ID, signature.ID.Path, version)
		}
	}
	return nil
}

// requireExpectations applies the optional Docker ARG assertions last, so a
// version bump without a regenerated closure reports the closure first.
func requireExpectations(request Request, pin Pin) error {
	if request.ExpectGopls != nil && *request.ExpectGopls != pin.Gopls {
		return refuse("asserted gopls version %s != pinned %s", *request.ExpectGopls, pin.Gopls)
	}
	if request.ExpectGo != nil && *request.ExpectGo != pin.Go {
		return refuse("pin generated with go%s; the layer installs go%s", pin.Go,
			*request.ExpectGo)
	}
	return nil
}

// LanguageAtMost is the compiler/language relation: a `go` directive may not name a
// language newer than the pinned compiler. The writer asks it before it retargets the
// resolved wrapper (#252), so a release that needs a newer Go is refused as a
// release/toolchain mismatch rather than as a `go mod tidy` error.
func LanguageAtMost(language GoLanguage, compiler GoVersion) bool {
	return languageAtMost(language, compiler)
}

// languageAtMost compares a `go` directive against the pinned compiler, treating a
// missing patch number as zero.
func languageAtMost(language GoLanguage, compiler GoVersion) bool {
	languageParts := numbers(string(language))
	compilerParts := numbers(string(compiler))
	for len(languageParts) < 3 {
		languageParts = append(languageParts, 0)
	}
	for index, part := range languageParts {
		if part != compilerParts[index] {
			return part < compilerParts[index]
		}
	}
	return true
}

func numbers(text string) []int {
	parts := []int{}
	current, seen := 0, false
	for _, digit := range text {
		if digit >= '0' && digit <= '9' {
			current = current*10 + int(digit-'0')
			seen = true
			continue
		}
		if seen {
			parts = append(parts, current)
		}
		current, seen = 0, false
	}
	if seen {
		parts = append(parts, current)
	}
	return parts
}

func filenames(names []Filename) []string {
	texts := make([]string, 0, len(names))
	for _, name := range names {
		texts = append(texts, string(name))
	}
	return texts
}

func sortedCopy(names []string) []string {
	copied := append([]string{}, names...)
	sort.Strings(copied)
	return copied
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
