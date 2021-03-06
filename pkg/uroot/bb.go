// Copyright 2015-2017 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package uroot

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/imports"

	"github.com/u-root/u-root/pkg/cpio"
	"github.com/u-root/u-root/pkg/golang"
)

// Commands to skip building in bb mode.
var skip = map[string]struct{}{
	"bb": {},
}

var BBBuilder = Builder{
	Build:            BBBuild,
	DefaultBinaryDir: "bbin",
}

// BuildBusybox builds a busybox of the given Go packages.
//
// pkgs is a list of Go import paths. If nil is returned, binaryPath will hold
// the busybox-style binary.
func BuildBusybox(env golang.Environ, pkgs []string, binaryPath string) error {
	urootPkg, err := env.Package("github.com/u-root/u-root")
	if err != nil {
		return err
	}

	bbDir := filepath.Join(urootPkg.Dir, "bb")
	// Blow bb away before trying to re-create it.
	if err := os.RemoveAll(bbDir); err != nil {
		return err
	}
	if err := os.MkdirAll(bbDir, 0755); err != nil {
		return err
	}

	var bbPackages []string
	// Move and rewrite package files.
	importer := importer.For("source", nil)
	for _, pkg := range pkgs {
		if _, ok := skip[path.Base(pkg)]; ok {
			continue
		}

		pkgDir := filepath.Join(bbDir, "cmds", path.Base(pkg))
		// TODO: use bbDir to derive import path below or vice versa.
		if err := RewritePackage(env, pkg, pkgDir, "github.com/u-root/u-root/pkg/bb", importer); err != nil {
			return err
		}

		bbPackages = append(bbPackages, fmt.Sprintf("github.com/u-root/u-root/bb/cmds/%s", path.Base(pkg)))
	}

	bb, err := NewPackageFromEnv(env, "github.com/u-root/u-root/pkg/bb/cmd", importer)
	if err != nil {
		return err
	}
	if bb == nil {
		return fmt.Errorf("bb cmd template missing")
	}
	if len(bb.ast.Files) != 1 {
		return fmt.Errorf("bb cmd template is supposed to only have one file")
	}
	// Create bb main.go.
	if err := CreateBBMainSource(bb.fset, bb.ast, bbPackages, bbDir); err != nil {
		return err
	}

	// Compile bb.
	return env.Build("github.com/u-root/u-root/bb", binaryPath, golang.BuildOpts{})
}

// CreateBBMainSource creates a bb Go command that imports all given pkgs.
//
// p must be the bb template.
//
// - For each pkg in pkgs, add
//     import _ "pkg"
//   to astp's first file.
// - Write source file out to destDir.
func CreateBBMainSource(fset *token.FileSet, astp *ast.Package, pkgs []string, destDir string) error {
	for _, pkg := range pkgs {
		for _, sourceFile := range astp.Files {
			// Add side-effect import to bb binary so init registers itself.
			//
			// import _ "pkg"
			astutil.AddNamedImport(fset, sourceFile, "_", pkg)
			break
		}
	}

	// Write bb main binary out.
	for filePath, sourceFile := range astp.Files {
		path := filepath.Join(destDir, filepath.Base(filePath))
		if err := writeFile(path, fset, sourceFile); err != nil {
			return err
		}
		break
	}
	return nil
}

// BBBuild is an implementation of Build for the busybox-like u-root initramfs.
//
// BBBuild rewrites the source files of the packages given to create one
// busybox-like binary containing all commands in `opts.Packages`.
func BBBuild(af ArchiveFiles, opts BuildOpts) error {
	// Build the busybox binary.
	bbPath := filepath.Join(opts.TempDir, "bb")
	if err := BuildBusybox(opts.Env, opts.Packages, bbPath); err != nil {
		return err
	}

	if len(opts.BinaryDir) == 0 {
		return fmt.Errorf("must specify binary directory")
	}

	// Build initramfs.
	if err := af.AddFile(bbPath, path.Join(opts.BinaryDir, "bb")); err != nil {
		return err
	}

	// Add symlinks for included commands to initramfs.
	for _, pkg := range opts.Packages {
		if _, ok := skip[path.Base(pkg)]; ok {
			continue
		}

		// Add a symlink /bbin/{cmd} -> /bbin/bb to our initramfs.
		if err := af.AddRecord(cpio.Symlink(filepath.Join(opts.BinaryDir, path.Base(pkg)), "bb")); err != nil {
			return err
		}
	}
	return nil
}

// Package is a Go package.
//
// It holds AST, type, file, and Go package information about a Go package.
type Package struct {
	name string

	fset        *token.FileSet
	ast         *ast.Package
	typeInfo    types.Info
	types       *types.Package
	sortedFiles []*ast.File

	// initCount keeps track of what the next init's index should be.
	initCount uint

	// init is the cmd.Init function that calls all other InitXs in the
	// right order.
	init *ast.FuncDecl

	// initAssigns is a map of assignment expression -> InitN function call
	// statement.
	//
	// That InitN should contain the assignment statement for the
	// appropriate assignment expression.
	//
	// types.Info.InitOrder keeps track of Initializations by Lhs name and
	// Rhs ast.Expr.  We reparent the Rhs in assignment statements in InitN
	// functions, so we use the Rhs as an easy key here.
	// types.Info.InitOrder + initAssigns can then easily be used to derive
	// the order of Stmts in the "real" init.
	//
	// The key Expr must also be the AssignStmt.Rhs[0].
	initAssigns map[ast.Expr]ast.Stmt
}

// NewPackageFromEnv finds the package identified by importPath, and gathers
// AST, type, and token information.
func NewPackageFromEnv(env golang.Environ, importPath string, importer types.Importer) (*Package, error) {
	p, err := env.Package(importPath)
	if err != nil {
		return nil, err
	}
	return NewPackage(filepath.Base(p.Dir), p, importer)
}

// ParseAST parses p's package files into an AST.
func ParseAST(p *build.Package) (*token.FileSet, *ast.Package, error) {
	name := filepath.Base(p.Dir)
	if !p.IsCommand() {
		return nil, nil, fmt.Errorf("package %q is not a command and cannot be included in bb", name)
	}

	fset := token.NewFileSet()
	pars, err := parser.ParseDir(fset, p.Dir, func(fi os.FileInfo) bool {
		// Only parse Go files that match build tags of this package.
		for _, name := range p.GoFiles {
			if name == fi.Name() {
				return true
			}
		}
		return false
	}, parser.ParseComments)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse AST for pkg %q: %v", name, err)
	}

	astp, ok := pars[p.Name]
	if !ok {
		return nil, nil, fmt.Errorf("parsed files, but could not find package %q in ast: %v", p.Name, pars)
	}
	return fset, astp, nil
}

// NewPackage gathers AST, type, and token information about package p, using
// the given importer to resolve dependencies.
func NewPackage(name string, p *build.Package, importer types.Importer) (*Package, error) {
	fset, astp, err := ParseAST(p)
	if err != nil {
		return nil, err
	}

	pp := &Package{
		name: name,
		fset: fset,
		ast:  astp,
		typeInfo: types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
		},
		initAssigns: make(map[ast.Expr]ast.Stmt),
	}

	// This Init will hold calls to all other InitXs.
	pp.init = &ast.FuncDecl{
		Name: ast.NewIdent("Init"),
		Type: &ast.FuncType{
			Params:  &ast.FieldList{},
			Results: nil,
		},
		Body: &ast.BlockStmt{},
	}

	// The order of types.Info.InitOrder depends on this list of files
	// always being passed to conf.Check in the same order.
	filenames := make([]string, 0, len(pp.ast.Files))
	for name := range pp.ast.Files {
		filenames = append(filenames, name)
	}
	sort.Strings(filenames)

	pp.sortedFiles = make([]*ast.File, 0, len(pp.ast.Files))
	for _, name := range filenames {
		pp.sortedFiles = append(pp.sortedFiles, pp.ast.Files[name])
	}
	// Type-check the package before we continue. We need types to rewrite
	// some statements.
	conf := types.Config{
		Importer: importer,

		// We only need global declarations' types.
		IgnoreFuncBodies: true,
	}
	tpkg, err := conf.Check(p.ImportPath, pp.fset, pp.sortedFiles, &pp.typeInfo)
	if err != nil {
		return nil, fmt.Errorf("type checking failed: %#v: %v", importer, err)
	}
	pp.types = tpkg
	return pp, nil
}

func (p *Package) nextInit(addToCallList bool) *ast.Ident {
	i := ast.NewIdent(fmt.Sprintf("Init%d", p.initCount))
	if addToCallList {
		p.init.Body.List = append(p.init.Body.List, &ast.ExprStmt{X: &ast.CallExpr{Fun: i}})
	}
	p.initCount++
	return i
}

// TODO:
// - write an init name generator, in case InitN is already taken.
// - also rewrite all non-Go-stdlib dependencies.
func (p *Package) rewriteFile(f *ast.File) bool {
	hasMain := false

	// Change the package name declaration from main to the command's name.
	f.Name = ast.NewIdent(p.name)

	// Map of fully qualified package name -> imported alias in the file.
	importAliases := make(map[string]string)
	for _, impt := range f.Imports {
		if impt.Name != nil {
			importPath, err := strconv.Unquote(impt.Path.Value)
			if err != nil {
				panic(err)
			}
			importAliases[importPath] = impt.Name.Name
		}
	}

	// When the types.TypeString function translates package names, it uses
	// this function to map fully qualified package paths to a local alias,
	// if it exists.
	qualifier := func(pkg *types.Package) string {
		name, ok := importAliases[pkg.Path()]
		if ok {
			return name
		}
		// When referring to self, don't use any package name.
		if pkg == p.types {
			return ""
		}
		return pkg.Name()
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			// We only care about vars.
			if d.Tok != token.VAR {
				break
			}
			for _, spec := range d.Specs {
				s := spec.(*ast.ValueSpec)
				if s.Values == nil {
					continue
				}

				// For each assignment, create a new init
				// function, and place it in the same file.
				for i, name := range s.Names {
					varInit := &ast.FuncDecl{
						Name: p.nextInit(false),
						Type: &ast.FuncType{
							Params:  &ast.FieldList{},
							Results: nil,
						},
						Body: &ast.BlockStmt{
							List: []ast.Stmt{
								&ast.AssignStmt{
									Lhs: []ast.Expr{name},
									Tok: token.ASSIGN,
									Rhs: []ast.Expr{s.Values[i]},
								},
							},
						},
					}
					// Add a call to the new init func to
					// this map, so they can be added to
					// Init0() in the correct init order
					// later.
					p.initAssigns[s.Values[i]] = &ast.ExprStmt{X: &ast.CallExpr{Fun: varInit.Name}}
					f.Decls = append(f.Decls, varInit)
				}

				// Add the type of the expression to the global
				// declaration instead.
				if s.Type == nil {
					typ := p.typeInfo.Types[s.Values[0]]
					s.Type = ast.NewIdent(types.TypeString(typ.Type, qualifier))
				}
				s.Values = nil
			}

		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == "main" {
				d.Name.Name = "Main"
				hasMain = true
			}
			if d.Recv == nil && d.Name.Name == "init" {
				d.Name = p.nextInit(true)
			}
		}
	}

	// Now we change any import names attached to package declarations. We
	// just upcase it for now; it makes it easy to look in bbsh for things
	// we changed, e.g. grep -r bbsh Import is useful.
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "// import") {
				c.Text = "// Import" + c.Text[9:]
			}
		}
	}
	return hasMain
}

// RewritePackage rewrites pkgPath to be bb-mode compatible, where destDir is
// the file system destination of the written files and bbImportPath is the Go
// import path of the bb package to register with.
func RewritePackage(env golang.Environ, pkgPath, destDir, bbImportPath string, importer types.Importer) error {
	p, err := NewPackageFromEnv(env, pkgPath, importer)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	return p.Rewrite(destDir, bbImportPath)
}

// Rewrite rewrites p into destDir as a bb package using bbImportPath for the
// bb implementation.
func (p *Package) Rewrite(destDir, bbImportPath string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	// This init holds all variable initializations.
	//
	// func Init0() {}
	varInit := &ast.FuncDecl{
		Name: p.nextInit(true),
		Type: &ast.FuncType{
			Params:  &ast.FieldList{},
			Results: nil,
		},
		Body: &ast.BlockStmt{},
	}

	var mainFile *ast.File
	for _, sourceFile := range p.sortedFiles {
		if hasMainFile := p.rewriteFile(sourceFile); hasMainFile {
			mainFile = sourceFile
		}
	}
	if mainFile == nil {
		return os.RemoveAll(destDir)
	}

	// Add variable initializations to Init0 in the right order.
	for _, initStmt := range p.typeInfo.InitOrder {
		a, ok := p.initAssigns[initStmt.Rhs]
		if !ok {
			return fmt.Errorf("couldn't find init assignment %s", initStmt)
		}
		varInit.Body.List = append(varInit.Body.List, a)
	}

	// import bb "bbImportPath"
	astutil.AddNamedImport(p.fset, mainFile, "bb", bbImportPath)

	// func init() {
	//   bb.Register(p.name, Init, Main)
	// }
	bbRegisterInit := &ast.FuncDecl{
		Name: ast.NewIdent("init"),
		Type: &ast.FuncType{},
		Body: &ast.BlockStmt{
			List: []ast.Stmt{
				&ast.ExprStmt{X: &ast.CallExpr{
					Fun: ast.NewIdent("bb.Register"),
					Args: []ast.Expr{
						// name=
						&ast.BasicLit{
							Kind:  token.STRING,
							Value: strconv.Quote(p.name),
						},
						// init=
						ast.NewIdent("Init"),
						// main=
						ast.NewIdent("Main"),
					},
				}},
			},
		},
	}

	// We could add these statements to any of the package files. We choose
	// the one that contains Main() to guarantee reproducibility of the
	// same bbsh binary.
	mainFile.Decls = append(mainFile.Decls, varInit, p.init, bbRegisterInit)

	// Write all files out.
	for filePath, sourceFile := range p.ast.Files {
		path := filepath.Join(destDir, filepath.Base(filePath))
		if err := writeFile(path, p.fset, sourceFile); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path string, fset *token.FileSet, f *ast.File) error {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return fmt.Errorf("error formatting Go file %q: %v", path, err)
	}
	return writeGoFile(path, buf.Bytes())
}

func writeGoFile(path string, code []byte) error {
	// Format the file. Do not fix up imports, as we only moved code around
	// within files.
	opts := imports.Options{
		Comments:   true,
		TabIndent:  true,
		TabWidth:   8,
		FormatOnly: true,
	}
	code, err := imports.Process("commandline", code, &opts)
	if err != nil {
		return fmt.Errorf("bad parse while processing imports %q: %v", path, err)
	}

	if err := ioutil.WriteFile(path, code, 0644); err != nil {
		return fmt.Errorf("error writing Go file to %q: %v", path, err)
	}
	return nil
}
