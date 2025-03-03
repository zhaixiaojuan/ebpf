package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/build/constraint"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const helpText = `Usage: %[1]s [options] <ident> <source file> [-- <C flags>]

ident is used as the stem of all generated Go types and functions, and
must be a valid Go identifier.

source is a single C file that is compiled using the specified compiler
(usually some version of clang).

You can pass options to the compiler by appending them after a '--' argument
or by supplying -cflags. Flags passed as arguments take precedence
over flags passed via -cflags. Additionally, the program expands quotation
marks in -cflags. This means that -cflags 'foo "bar baz"' is passed to the
compiler as two arguments "foo" and "bar baz".

The program expects GOPACKAGE to be set in the environment, and should be invoked
via go generate. The generated files are written to the current directory.

Options:

`

// Targets understood by bpf2go.
//
// Targets without a Linux string can't be used directly and are only included
// for the generic bpf, bpfel, bpfeb targets.
var targetByGoArch = map[string]target{
	"386":         {"bpfel", "x86"},
	"amd64":       {"bpfel", "x86"},
	"amd64p32":    {"bpfel", ""},
	"arm":         {"bpfel", "arm"},
	"arm64":       {"bpfel", "arm64"},
	"loong64":     {"bpfel", ""},
	"mipsle":      {"bpfel", ""},
	"mips64le":    {"bpfel", ""},
	"mips64p32le": {"bpfel", ""},
	"ppc64le":     {"bpfel", "powerpc"},
	"riscv64":     {"bpfel", ""},
	"armbe":       {"bpfeb", "arm"},
	"arm64be":     {"bpfeb", "arm64"},
	"mips":        {"bpfeb", ""},
	"mips64":      {"bpfeb", ""},
	"mips64p32":   {"bpfeb", ""},
	"ppc64":       {"bpfeb", "powerpc"},
	"s390":        {"bpfeb", "s390"},
	"s390x":       {"bpfeb", "s390"},
	"sparc":       {"bpfeb", "sparc"},
	"sparc64":     {"bpfeb", "sparc"},
}

func run(stdout io.Writer, pkg, outputDir string, args []string) (err error) {
	b2g := bpf2go{
		stdout:    stdout,
		pkg:       pkg,
		outputDir: outputDir,
	}

	fs := flag.NewFlagSet("bpf2go", flag.ContinueOnError)
	fs.StringVar(&b2g.cc, "cc", "clang", "`binary` used to compile C to BPF")
	fs.StringVar(&b2g.strip, "strip", "", "`binary` used to strip DWARF from compiled BPF (default \"llvm-strip\")")
	fs.BoolVar(&b2g.disableStripping, "no-strip", false, "disable stripping of DWARF")
	flagCFlags := fs.String("cflags", "", "flags passed to the compiler, may contain quoted arguments")
	fs.Var(&b2g.tags, "tags", "Comma-separated list of Go build tags to include in generated files")
	flagTarget := fs.String("target", "bpfel,bpfeb", "clang target(s) to compile for (comma separated)")
	fs.StringVar(&b2g.makeBase, "makebase", "", "write make compatible depinfo files relative to `directory`")
	fs.Var(&b2g.cTypes, "type", "`Name` of a type to generate a Go declaration for, may be repeated")
	fs.BoolVar(&b2g.skipGlobalTypes, "no-global-types", false, "Skip generating types for map keys and values, etc.")
	fs.StringVar(&b2g.outputStem, "output-stem", "", "alternative stem for names of generated files (defaults to ident)")

	fs.SetOutput(stdout)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), helpText, fs.Name())
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output())
		printTargets(fs.Output())
	}
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}

	if b2g.pkg == "" {
		return errors.New("missing package, are you running via go generate?")
	}

	if b2g.cc == "" {
		return errors.New("no compiler specified")
	}

	args, cFlags := splitCFlagsFromArgs(fs.Args())

	if *flagCFlags != "" {
		splitCFlags, err := splitArguments(*flagCFlags)
		if err != nil {
			return err
		}

		// Command line arguments take precedence over C flags
		// from the flag.
		cFlags = append(splitCFlags, cFlags...)
	}

	for _, cFlag := range cFlags {
		if strings.HasPrefix(cFlag, "-M") {
			return fmt.Errorf("use -makebase instead of %q", cFlag)
		}
	}

	b2g.cFlags = cFlags[:len(cFlags):len(cFlags)]

	if len(args) < 2 {
		return errors.New("expected at least two arguments")
	}

	b2g.ident = args[0]
	if !token.IsIdentifier(b2g.ident) {
		return fmt.Errorf("%q is not a valid identifier", b2g.ident)
	}

	input := args[1]
	if _, err := os.Stat(input); os.IsNotExist(err) {
		return fmt.Errorf("file %s doesn't exist", input)
	} else if err != nil {
		return fmt.Errorf("state %s: %s", input, err)
	}

	b2g.sourceFile, err = filepath.Abs(input)
	if err != nil {
		return err
	}

	if b2g.makeBase != "" {
		b2g.makeBase, err = filepath.Abs(b2g.makeBase)
		if err != nil {
			return err
		}
	}

	if b2g.outputStem != "" && strings.ContainsRune(b2g.outputStem, filepath.Separator) {
		return fmt.Errorf("-output-stem %q must not contain path separation characters", b2g.outputStem)
	}

	targetArches := strings.Split(*flagTarget, ",")
	if len(targetArches) == 0 {
		return fmt.Errorf("no targets specified")
	}

	targets, err := collectTargets(targetArches)
	if errors.Is(err, errInvalidTarget) {
		printTargets(stdout)
		fmt.Fprintln(stdout)
		return err
	}
	if err != nil {
		return err
	}

	if !b2g.disableStripping {
		// Try to find a suitable llvm-strip, possibly with a version suffix derived
		// from the clang binary.
		if b2g.strip == "" {
			b2g.strip = "llvm-strip"
			if strings.HasPrefix(b2g.cc, "clang") {
				b2g.strip += strings.TrimPrefix(b2g.cc, "clang")
			}
		}

		b2g.strip, err = exec.LookPath(b2g.strip)
		if err != nil {
			return err
		}
	}

	for target, arches := range targets {
		if err := b2g.convert(target, arches); err != nil {
			return err
		}
	}

	return nil
}

// cTypes collects the C type names a user wants to generate Go types for.
//
// Names are guaranteed to be unique, and only a subset of names is accepted so
// that we may extend the flag syntax in the future.
type cTypes []string

var _ flag.Value = (*cTypes)(nil)

func (ct *cTypes) String() string {
	if ct == nil {
		return "[]"
	}
	return fmt.Sprint(*ct)
}

const validCTypeChars = `[a-z0-9_]`

var reValidCType = regexp.MustCompile(`(?i)^` + validCTypeChars + `+$`)

func (ct *cTypes) Set(value string) error {
	if !reValidCType.MatchString(value) {
		return fmt.Errorf("%q contains characters outside of %s", value, validCTypeChars)
	}

	i := sort.SearchStrings(*ct, value)
	if i >= len(*ct) {
		*ct = append(*ct, value)
		return nil
	}

	if (*ct)[i] == value {
		return fmt.Errorf("duplicate type %q", value)
	}

	*ct = append((*ct)[:i], append([]string{value}, (*ct)[i:]...)...)
	return nil
}

type bpf2go struct {
	stdout io.Writer
	// Absolute path to a .c file.
	sourceFile string
	// Absolute path to a directory where .go are written
	outputDir string
	// Alternative output stem. If empty, ident is used.
	outputStem string
	// Valid go package name.
	pkg string
	// Valid go identifier.
	ident string
	// C compiler.
	cc string
	// Command used to strip DWARF.
	strip            string
	disableStripping bool
	// C flags passed to the compiler.
	cFlags          []string
	skipGlobalTypes bool
	// C types to include in the generatd output.
	cTypes cTypes
	// Build tags to be included in the output.
	tags buildTags
	// Base directory of the Makefile. Enables outputting make-style dependencies
	// in .d files.
	makeBase string
}

func (b2g *bpf2go) convert(tgt target, arches []string) (err error) {
	removeOnError := func(f *os.File) {
		if err != nil {
			os.Remove(f.Name())
		}
		f.Close()
	}

	outputStem := b2g.outputStem
	if outputStem == "" {
		outputStem = strings.ToLower(b2g.ident)
	}
	stem := fmt.Sprintf("%s_%s", outputStem, tgt.clang)
	if tgt.linux != "" {
		stem = fmt.Sprintf("%s_%s_%s", outputStem, tgt.clang, tgt.linux)
	}

	objFileName := filepath.Join(b2g.outputDir, stem+".o")

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	var archConstraint constraint.Expr
	for _, arch := range arches {
		tag := &constraint.TagExpr{Tag: arch}
		archConstraint = orConstraints(archConstraint, tag)
	}

	constraints := andConstraints(archConstraint, b2g.tags.Expr)

	cFlags := make([]string, len(b2g.cFlags))
	copy(cFlags, b2g.cFlags)
	if tgt.linux != "" {
		cFlags = append(cFlags, "-D__TARGET_ARCH_"+tgt.linux)
	}

	var dep bytes.Buffer
	err = compile(compileArgs{
		cc:     b2g.cc,
		cFlags: cFlags,
		target: tgt.clang,
		dir:    cwd,
		source: b2g.sourceFile,
		dest:   objFileName,
		dep:    &dep,
	})
	if err != nil {
		return err
	}

	fmt.Fprintln(b2g.stdout, "Compiled", objFileName)

	if !b2g.disableStripping {
		if err := strip(b2g.strip, objFileName); err != nil {
			return err
		}
		fmt.Fprintln(b2g.stdout, "Stripped", objFileName)
	}

	// Write out generated go
	goFileName := filepath.Join(b2g.outputDir, stem+".go")
	goFile, err := os.Create(goFileName)
	if err != nil {
		return err
	}
	defer removeOnError(goFile)

	err = output(outputArgs{
		pkg:             b2g.pkg,
		ident:           b2g.ident,
		cTypes:          b2g.cTypes,
		skipGlobalTypes: b2g.skipGlobalTypes,
		constraints:     constraints,
		obj:             objFileName,
		out:             goFile,
	})
	if err != nil {
		return fmt.Errorf("can't write %s: %s", goFileName, err)
	}

	fmt.Fprintln(b2g.stdout, "Wrote", goFileName)

	if b2g.makeBase == "" {
		return
	}

	deps, err := parseDependencies(cwd, &dep)
	if err != nil {
		return fmt.Errorf("can't read dependency information: %s", err)
	}

	// There is always at least a dependency for the main file.
	deps[0].file = goFileName
	depFile, err := adjustDependencies(b2g.makeBase, deps)
	if err != nil {
		return fmt.Errorf("can't adjust dependency information: %s", err)
	}

	depFileName := goFileName + ".d"
	if err := os.WriteFile(depFileName, depFile, 0666); err != nil {
		return fmt.Errorf("can't write dependency file: %s", err)
	}

	fmt.Fprintln(b2g.stdout, "Wrote", depFileName)
	return nil
}

type target struct {
	clang string
	linux string
}

func printTargets(w io.Writer) {
	var arches []string
	for arch, archTarget := range targetByGoArch {
		if archTarget.linux == "" {
			continue
		}
		arches = append(arches, arch)
	}
	sort.Strings(arches)

	fmt.Fprint(w, "Supported targets:\n")
	fmt.Fprint(w, "\tbpf\n\tbpfel\n\tbpfeb\n")
	for _, arch := range arches {
		fmt.Fprintf(w, "\t%s\n", arch)
	}
}

var errInvalidTarget = errors.New("unsupported target")

func collectTargets(targets []string) (map[target][]string, error) {
	result := make(map[target][]string)
	for _, tgt := range targets {
		switch tgt {
		case "bpf", "bpfel", "bpfeb":
			var goarches []string
			for arch, archTarget := range targetByGoArch {
				if archTarget.clang == tgt {
					// Include tags for all goarches that have the same endianness.
					goarches = append(goarches, arch)
				}
			}
			sort.Strings(goarches)
			result[target{tgt, ""}] = goarches

		case "native":
			tgt = runtime.GOARCH
			fallthrough

		default:
			archTarget, ok := targetByGoArch[tgt]
			if !ok || archTarget.linux == "" {
				return nil, fmt.Errorf("%q: %w", tgt, errInvalidTarget)
			}

			var goarches []string
			for goarch, lt := range targetByGoArch {
				if lt == archTarget {
					// Include tags for all goarches that have the same
					// target.
					goarches = append(goarches, goarch)
				}
			}

			sort.Strings(goarches)
			result[archTarget] = goarches
		}
	}

	return result, nil
}

func main() {
	outputDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	if err := run(os.Stdout, os.Getenv("GOPACKAGE"), outputDir, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
