package impact

import (
	"go/ast"
	"go/format"
	"go/parser"
	gotoken "go/token"
	"path"
	"strconv"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

func goModule(repo snapshot.Snapshot) string {
	file, ok := repo.Find("go.mod")
	if !ok {
		return ""
	}
	for line := range strings.SplitSeq(file.Content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		name := fields[1]
		if value, err := strconv.Unquote(name); err == nil {
			name = value
		}
		return strings.TrimSuffix(name, "/")
	}
	return ""
}

func indexGoContracts(c *ContractCatalog, file snapshot.File, module string) {
	set := gotoken.NewFileSet()
	tree, err := parser.ParseFile(set, file.Path, file.Content, 0)
	if err != nil {
		c.uncertain("", "Go syntax could not be parsed", ContractSite{Path: file.Path, SHA256: file.SHA256})
		return
	}
	location := func(item ast.Node) ContractSite {
		p := set.PositionFor(item.Pos(), false)
		return ContractSite{Path: file.Path, Line: p.Line, First: p.Offset, Last: set.PositionFor(item.End(), false).Offset, SHA256: file.SHA256}
	}
	pkg := path.Join(module, path.Dir(file.Path))
	if module == "" {
		pkg = "local/" + path.Join(path.Dir(file.Path), tree.Name.Name)
	}
	imports := make(map[string]string, len(tree.Imports))
	for _, spec := range tree.Imports {
		name, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		alias := path.Base(name)
		if spec.Name != nil {
			alias = spec.Name.Name
		} else if versionNumber(alias) {
			alias = path.Base(path.Dir(name))
		}
		imports[alias] = name
	}
	for _, item := range tree.Decls {
		decl, ok := item.(*ast.FuncDecl)
		if !ok || !decl.Name.IsExported() {
			continue
		}
		name := decl.Name.Name
		if decl.Recv != nil {
			if len(decl.Recv.List) != 1 {
				continue
			}
			receiver := goReceiver(decl.Recv.List[0].Type)
			if !ast.IsExported(receiver) {
				continue
			}
			name = receiver + "." + name
		}
		var signature strings.Builder
		if err := format.Node(&signature, set, decl.Type); err == nil {
			c.declare("go:"+pkg+"."+name, signature.String(), location(decl.Name))
		}
	}
	ast.Inspect(tree, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := call.Fun
		switch generic := callee.(type) {
		case *ast.IndexListExpr:
			callee = generic.X
		case *ast.IndexExpr:
			callee = generic.X
		}
		switch fn := callee.(type) {
		case *ast.SelectorExpr:
			qualifier, simple := fn.X.(*ast.Ident)
			if simple && qualifier.Obj == nil && imports[qualifier.Name] != "" && fn.Sel.IsExported() {
				c.call("go:"+imports[qualifier.Name]+"."+fn.Sel.Name, location(call.Fun))
			} else {
				c.uncertain(fn.Sel.Name, "method receiver needs type information", location(call.Fun))
			}
		case *ast.Ident:
			if fn.Obj != nil && (fn.Obj.Kind == ast.Var || fn.Obj.Kind == ast.Con) || fn.Obj == nil && imports["."] != "" {
				c.uncertain(fn.Name, "function value or dot import", location(call.Fun))
			}
		}
		return true
	})
}

func versionNumber(part string) bool {
	if len(part) < 2 || part[0] != 'v' {
		return false
	}
	_, err := strconv.Atoi(part[1:])
	return err == nil
}

func goReceiver(expr ast.Expr) string {
	switch recv := expr.(type) {
	case *ast.Ident:
		return recv.Name
	case *ast.StarExpr:
		return goReceiver(recv.X)
	case *ast.IndexExpr:
		return goReceiver(recv.X)
	case *ast.IndexListExpr:
		return goReceiver(recv.X)
	default:
		return ""
	}
}
