// gogetdoc gets documentation for Go objects given their locations in the source code
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"

	"golang.org/x/tools/go/buildutil"
	"golang.org/x/tools/go/loader"
)

var (
	cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
	pos        = flag.String("pos", "", "Filename and byte offset of item to document, e.g. foo.go:#123")
	modified   = flag.Bool("modified", false, "read an archive of modified files from standard input")
	linelength = flag.Int("linelength", 80, "maximum length of a line in the output (in Unicode code points)")
)

const modifiedUsage = `
The archive format for the -modified flag consists of the file name, followed
by a newline, the decimal file size, another newline, and the contents of the file.

This allows editors to supply gogetdoc with the contents of their unsaved buffers.
`

const (
	indent    = ""
	preIndent = "    "
)

// Doc holds the resulting documentation for a particular item.
type Doc struct {
	Import string
	Name   string
	Decl   string
	Doc    string
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, modifiedUsage)
	}
	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}
	filename, offset, err := parsePos(*pos)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	ctx := &build.Default
	if *modified {
		overlay, err := buildutil.ParseOverlayArchive(os.Stdin)
		if err != nil {
			log.Fatalln("invalid archive:", err)
		}
		ctx = buildutil.OverlayContext(ctx, overlay)
	}

	d, err := Run(ctx, filename, offset)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	// TODO: output format
	if d.Import != "" {
		fmt.Printf("import \"%s\"\n", d.Import)
		fmt.Println()
	}
	fmt.Println(d.Decl)
	fmt.Println()
	if d.Doc == "" {
		d.Doc = "Undocumented."
	}
	doc.ToText(os.Stdout, d.Doc, indent, preIndent, *linelength)
}

// Run is a wrapper for the gogetdoc command.  It is broken out of main for easier testing.
func Run(ctx *build.Context, filename string, offset int64) (*Doc, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, errors.New("gogetdoc: couldn't get working directory")
	}
	bp, err := buildutil.ContainingPackage(ctx, wd, filename)
	if err != nil {
		return nil, fmt.Errorf("gogetdoc: couldn't get package for %s: %s", filename, err.Error())
	}
	conf := &loader.Config{
		Build:               ctx,
		ParserMode:          parser.ParseComments,
		TypeCheckFuncBodies: func(pkg string) bool { return pkg == bp.ImportPath },
		AllowErrors:         true,
	}

	var parseError error
	conf.TypeChecker.Error = func(err error) {
		if parseError != nil {
			return
		}
		parseError = err
	}
	conf.ImportWithTests(bp.ImportPath)
	lprog, err := conf.Load()
	if err != nil {
		return nil, fmt.Errorf("gogetdoc: error loading program: %s", err.Error())
	}
	doc, err := DocForPos(lprog, filename, offset)
	if err != nil && parseError != nil {
		fmt.Fprintln(os.Stderr, parseError)
	}
	return doc, err
}

// DocForPos attempts to get the documentation for an item given a filename and byte offset.
func DocForPos(lprog *loader.Program, filename string, offset int64) (*Doc, error) {
	tokFile := FileFromProgram(lprog, filename)
	if tokFile == nil {
		return nil, fmt.Errorf("gogetdoc: couldn't find %s in program", filename)
	}
	offPos := tokFile.Pos(int(offset))

	pkgInfo, nodes, _ := lprog.PathEnclosingInterval(offPos, offPos)
	for _, node := range nodes {
		switch i := node.(type) {
		case *ast.ImportSpec:
			return PackageDoc(lprog.Fset, ImportPath(i))
		case *ast.Ident:
			// if we can't find the object denoted by the identifier, keep searching)
			if obj := pkgInfo.ObjectOf(i); obj == nil {
				continue
			}
			return IdentDoc(i, pkgInfo, lprog)
		case *ast.File:
			if i.Doc != nil {
				return &Doc{
					Doc: i.Doc.Text(),
				}, nil
			}
		}
	}
	return nil, errors.New("gogetdoc: no documentation found")
}

// FileFromProgram attempts to locate a token.File from a loaded program.
func FileFromProgram(prog *loader.Program, name string) *token.File {
	for _, info := range prog.AllPackages {
		for _, astFile := range info.Files {
			tokFile := prog.Fset.File(astFile.Pos())
			if tokFile == nil {
				continue
			}
			tokName := tokFile.Name()
			if runtime.GOOS == "windows" {
				tokName = filepath.ToSlash(tokName)
				name = filepath.ToSlash(name)
			}
			if tokName == name {
				return tokFile
			}
			if sameFile(tokName, name) {
				return tokFile
			}
		}
	}
	return nil
}

func parsePos(p string) (filename string, offset int64, err error) {
	// foo.go:#123
	if p == "" {
		err = errors.New("missing required -pos flag")
		return
	}
	sep := strings.LastIndex(p, ":")
	// need at least 2 characters after the ':'
	// (the # sign and the offset)
	if sep == -1 || sep > len(p)-2 || p[sep+1] != '#' {
		err = fmt.Errorf("invalid option: -pos=%s", p)
		return
	}
	filename = p[:sep]
	offset, err = strconv.ParseInt(p[sep+2:], 10, 32)
	return
}

func sameFile(a, b string) bool {
	if filepath.Base(a) != filepath.Base(b) {
		// We only care about symlinks for the GOPATH itself. File
		// names need to match.
		return false
	}
	if ai, err := os.Stat(a); err == nil {
		if bi, err := os.Stat(b); err == nil {
			return os.SameFile(ai, bi)
		}
	}
	return false
}
