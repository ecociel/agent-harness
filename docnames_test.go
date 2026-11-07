package harness_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness/hooks/guard"
)

// docPackages are the package names the docs qualify identifiers with, and where
// each package lives.
var docPackages = map[string]string{
	"harness":     ".",
	"model":       "model",
	"tool":        "tool",
	"eval":        "eval",
	"multi":       "multi",
	"hooktest":    "hooktest",
	"journaltest": "journaltest",
	"approval":    "hooks/approval",
	"budget":      "hooks/budget",
	"compact":     "hooks/compact",
	"guard":       "hooks/guard",
	"logging":     "hooks/logging",
	"metrics":     "hooks/metrics",
	"offload":     "hooks/offload",
	"profile":     "hooks/profile",
	"retry":       "hooks/retry",
	"trace":       "hooks/trace",
	"trajectory":  "hooks/trajectory",
	"postgres":    "journal/postgres",
	"anthropic":   "provider/anthropic",
	"openai":      "provider/openai",
	"fake":        "provider/fake",
}

// apiOf is what one package exports: its top-level names, and the fields and
// methods of each of its types.
type apiOf struct {
	names   map[string]bool
	members map[string]map[string]bool
}

func exported(t *testing.T, dir string) apiOf {
	api := apiOf{names: map[string]bool{}, members: map[string]map[string]bool{}}
	member := func(typ, name string) {
		if api.members[typ] == nil {
			api.members[typ] = map[string]bool{}
		}
		api.members[typ][name] = true
	}
	pkgs, err := parser.ParseDir(token.NewFileSet(), dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil {
						api.names[d.Name.Name] = true
						continue
					}
					recv := d.Recv.List[0].Type
					if s, ok := recv.(*ast.StarExpr); ok {
						recv = s.X
					}
					if id, ok := recv.(*ast.Ident); ok {
						member(id.Name, d.Name.Name)
					}
				case *ast.GenDecl:
					for _, s := range d.Specs {
						switch s := s.(type) {
						case *ast.ValueSpec:
							for _, n := range s.Names {
								api.names[n.Name] = true
							}
						case *ast.TypeSpec:
							api.names[s.Name.Name] = true
							member(s.Name.Name, "")
							var fields *ast.FieldList
							switch ty := s.Type.(type) {
							case *ast.StructType:
								fields = ty.Fields
							case *ast.InterfaceType:
								fields = ty.Methods
							}
							if fields == nil {
								continue
							}
							for _, fl := range fields.List {
								for _, n := range fl.Names {
									member(s.Name.Name, n.Name)
								}
								if len(fl.Names) == 0 {
									ty := fl.Type
									if st, ok := ty.(*ast.StarExpr); ok {
										ty = st.X
									}
									switch e := ty.(type) {
									case *ast.Ident:
										member(s.Name.Name, e.Name)
									case *ast.SelectorExpr:
										member(s.Name.Name, e.Sel.Name)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return api
}

var (
	codeSpan   = regexp.MustCompile("`([^`\n]+)`")
	qualified  = regexp.MustCompile(`\b([a-z]+)\.([A-Z]\w*)(?:\.([A-Z]\w*))?`)
	selector   = regexp.MustCompile(`^([A-Z]\w*)\.([A-Z]\w*)\b`)
	literal    = regexp.MustCompile(`^(?:([a-z]+)\.)?([A-Z]\w*)\{([^}]*)\}`)
	literalKey = regexp.MustCompile(`^\s*([A-Z]\w*)\s*(?::|$)`)
)

// The docs name the API by its real identifiers, so a rename or a removal that
// leaves a doc behind fails here. A name the docs qualify with a package must be
// exported by it, and Type.Member or Type{Field} must name a field or method of a
// type of that name in some package. A name qualified with a package this map
// does not know is skipped.
func TestTheDocsNameRealIdentifiers(t *testing.T) {
	apis := map[string]apiOf{}
	for name, dir := range docPackages {
		apis[name] = exported(t, dir)
	}
	hasMember := func(typ, m string) (known, ok bool) {
		for _, a := range apis {
			ms, has := a.members[typ]
			if !has {
				continue
			}
			known = true
			if ms[m] {
				return true, true
			}
		}
		return known, false
	}
	docs := []string{"README.md", "docs/hooks.md", "docs/state-machine.md",
		"docs/design/durable-core.md", "examples/hooks/README.md"}
	patterns, _ := filepath.Glob("docs/patterns/*.md")
	for _, path := range append(docs, patterns...) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range codeSpan.FindAllStringSubmatch(string(raw), -1) {
			span := m[1]
			for _, q := range qualified.FindAllStringSubmatch(span, -1) {
				a, ok := apis[q[1]]
				if !ok {
					continue
				}
				if !a.names[q[2]] {
					t.Errorf("%s: `%s` names %s.%s, which does not exist", path, span, q[1], q[2])
					continue
				}
				if q[3] != "" {
					if ms, isType := a.members[q[2]]; isType && !ms[q[3]] {
						t.Errorf("%s: `%s` names %s.%s.%s, which does not exist", path, span, q[1], q[2], q[3])
					}
				}
			}
			if s := selector.FindStringSubmatch(span); s != nil {
				if known, ok := hasMember(s[1], s[2]); known && !ok {
					t.Errorf("%s: `%s` names %s.%s, which no type has", path, span, s[1], s[2])
				}
			}
			if l := literal.FindStringSubmatch(span); l != nil {
				if _, ours := apis[l[1]]; l[1] != "" && !ours {
					continue
				}
				for _, part := range strings.Split(l[3], ",") {
					k := literalKey.FindStringSubmatch(part)
					if k == nil {
						continue
					}
					if known, ok := hasMember(l[2], k[1]); known && !ok {
						t.Errorf("%s: `%s` names field %s of %s, which no type has", path, span, k[1], l[2])
					}
				}
			}
		}
	}
}

var windowClaim = regexp.MustCompile("`(Secrets|PII)\\(\\)` holds back (\\d+) bytes")

// The docs state how many bytes guard.Stream holds back for the shipped rules with
// a window of 0. The figure is the longest match, so a window one byte smaller must
// be refused. docs/hooks.md, where Stream is described, must state both.
func TestTheDocsStateTheStreamWindow(t *testing.T) {
	rules := map[string][]guard.Rule{"Secrets": guard.Secrets(), "PII": guard.PII()}
	for _, path := range []string{"docs/hooks.md", "README.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, c := range windowClaim.FindAllStringSubmatch(string(raw), -1) {
			n, _ := strconv.Atoi(c[2])
			seen[c[1]] = true
			if _, err := guard.Stream(rules[c[1]], n); err != nil {
				t.Errorf("%s: %s() does not fit in %d bytes: %v", path, c[1], n, err)
			}
			if _, err := guard.Stream(rules[c[1]], n-1); err == nil {
				t.Errorf("%s: %s() fits in %d bytes, so the docs overstate its window", path, c[1], n-1)
			}
		}
		for name := range rules {
			if path == "docs/hooks.md" && !seen[name] {
				t.Errorf("%s does not say how many bytes %s() holds back", path, name)
			}
		}
	}
}
