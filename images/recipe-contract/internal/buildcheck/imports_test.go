package buildcheck

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func yamlImporter(source []byte) (bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "import.go", source, parser.ImportsOnly)
	if err != nil {
		return false, err
	}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return false, err
		}
		if path == "go.yaml.in/yaml/v3" {
			return true, nil
		}
	}
	return false, nil
}

func TestYAMLImportSpellings(t *testing.T) {
	for _, spec := range []string{`"go.yaml.in/yaml/v3"`, "`go.yaml.in/yaml/v3`", `yaml "go.yaml.in/yaml/v3"`, `. "go.yaml.in/yaml/v3"`, `_ "go.yaml.in/yaml/v3"`, `yaml "go.yaml.in/yaml/\x763"`} {
		t.Run(spec, func(t *testing.T) {
			imports, err := yamlImporter([]byte("package mutant\nimport (\n" + spec + "\n)\n"))
			if err != nil || !imports {
				t.Fatalf("import boundary missed %s: %v", spec, err)
			}
		})
	}
	for _, source := range []string{"package control\n// import \"go.yaml.in/yaml/v3\"\n", "package control\nconst text = `go.yaml.in/yaml/v3`\n"} {
		imports, err := yamlImporter([]byte(source))
		if err != nil || imports {
			t.Fatalf("non-import matched: %v", err)
		}
	}
}

func checkYAMLImportBoundary(root string) error {
	importers := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// WalkDir does not follow links, but the Go importer can. Fail closed
		// before skipping the one dependency root owned by go mod vendor.
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in module source tree: %s", path)
		}
		if entry.IsDir() {
			if path == filepath.Join(root, "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		imports, err := yamlImporter(source)
		if err != nil {
			return err
		}
		if imports {
			importers++
			if filepath.Dir(path) != filepath.Join(root, "internal", "document") {
				return fmt.Errorf("only internal/document may import go.yaml.in/yaml/v3: %s", path)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if importers == 0 {
		return fmt.Errorf("no package imports go.yaml.in/yaml/v3")
	}
	return nil
}

func TestYAMLImportBoundary(t *testing.T) {
	if err := checkYAMLImportBoundary(filepath.Join("..", "..")); err != nil {
		t.Fatal(err)
	}
}

func TestYAMLImportBoundarySourceTrees(t *testing.T) {
	for _, mutant := range []string{"external-directory-symlink", "nested-vendor-symlink", "nested-vendor-import", "file-symlink", "top-vendor-symlink", "real-vendor-control"} {
		t.Run(mutant, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, source string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(source), 0644); err != nil {
					t.Fatal(err)
				}
			}
			link := func(target, path string) {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			source := "package mutant\nimport _ \"go.yaml.in/yaml/v3\"\n"
			write(filepath.Join(root, "internal", "document", "yaml.go"), source)
			switch mutant {
			case "external-directory-symlink":
				external := t.TempDir()
				write(filepath.Join(external, "yaml.go"), source)
				link(external, filepath.Join(root, "internal", "smuggled"))
			case "nested-vendor-symlink":
				write(filepath.Join(root, "hidden", "vendor", "yaml.go"), source)
				link(filepath.Join(root, "hidden", "vendor"), filepath.Join(root, "internal", "smuggled"))
			case "nested-vendor-import":
				write(filepath.Join(root, "hidden", "vendor", "yaml.go"), source)
			case "file-symlink":
				external := filepath.Join(t.TempDir(), "ignored.txt")
				write(external, "not Go")
				link(external, filepath.Join(root, "ignored.txt"))
			case "top-vendor-symlink":
				link(t.TempDir(), filepath.Join(root, "vendor"))
			case "real-vendor-control":
				write(filepath.Join(root, "vendor", "dependency", "yaml.go"), source)
				link(t.TempDir(), filepath.Join(root, "vendor", "ignored"))
			}
			err := checkYAMLImportBoundary(root)
			if mutant == "real-vendor-control" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("source-tree mutant escaped import boundary")
			}
		})
	}
}
