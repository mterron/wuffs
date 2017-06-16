// Use of this source code is governed by a BSD-style license that can be found
// in the LICENSE file.

//go:generate go run gen.go

// puffs-gen-c transpiles a Puffs program to a C program.
//
// The command line arguments list the source Puffs files. If no arguments are
// given, it reads from stdin.
//
// The generated program is written to stdout.
package main

import (
	"bytes"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"strings"

	"github.com/google/puffs/lang/generate"

	a "github.com/google/puffs/lang/ast"
	t "github.com/google/puffs/lang/token"
)

var (
	zero = big.NewInt(0)
	one  = big.NewInt(1)
)

// Prefixes are prepended to names to form a namespace and to avoid e.g.
// "double" being a valid Puffs variable name but not a valid C one.
const (
	aPrefix = "a_" // Function argument.
	fPrefix = "f_" // Struct field.
	tPrefix = "t_" // Temporary local variable.
	vPrefix = "v_" // Local variable.
)

func main() {
	generate.Main(func(pkgName string, tm *t.Map, files []*a.File) ([]byte, error) {
		g := &gen{
			pkgName: pkgName,
			tm:      tm,
			files:   files,
		}
		if err := g.generate(); err != nil {
			return nil, err
		}
		stdout := &bytes.Buffer{}
		cmd := exec.Command("clang-format", "-style=Chromium")
		cmd.Stdin = &g.buffer
		cmd.Stdout = stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, err
		}
		return stdout.Bytes(), nil
	})
}

const userDefinedStatusBase = 128

var builtInStatuses = [...]string{
	// For API/ABI forwards and backwards compatibility, the very first two
	// statuses must be "status ok" (with generated value 0) and "error bad
	// version" (with generated value -2 + 1). This lets caller code check the
	// constructor return value for "error bad version" even if the caller and
	// callee were built with different versions.
	"status ok",
	"error bad version",
	// The order of the remaining statuses is less important, but should remain
	// stable for API/ABI backwards compatibility, where additional built in
	// status codes don't affect existing ones.
	"error bad receiver",
	"error bad argument",
	"error constructor not called",
	"error unexpected EOF", // Used if reading when closed == true.
	"status short read",    // Used if reading when closed == false.
	"status short write",
	"error closed for writes",
}

func init() {
	if len(builtInStatuses) > userDefinedStatusBase {
		panic("too many builtInStatuses")
	}
}

type replacementPolicy bool

const (
	replaceNothing          = replacementPolicy(false)
	replaceCallSuspendibles = replacementPolicy(true)
)

// parenthesesPolicy controls whether to print the outer parentheses in an
// expression like "(x + y)". An "if" or "while" will print their own
// parentheses for "if (expr)" because they need to be able to say "if (x)".
// But a double-parenthesized expression like "if ((x == y))" is a clang
// warning (-Wparentheses-equality) and we like to compile with -Wall -Werror.
type parenthesesPolicy bool

const (
	parenthesesMandatory = parenthesesPolicy(false)
	parenthesesOptional  = parenthesesPolicy(true)
)

type visibility uint32

const (
	bothPubPri = visibility(iota)
	pubOnly
	priOnly
)

const maxTemp = 10000

type status struct {
	name    string
	msg     string
	isError bool
}

type perFunc struct {
	funk        *a.Func
	jumpTargets map[*a.While]uint32
	tempW       uint32
	tempR       uint32
	public      bool
	suspendible bool
}

type gen struct {
	buffer     bytes.Buffer
	pkgName    string
	tm         *t.Map
	files      []*a.File
	statusList []status
	statusMap  map[t.ID]status
	structList []*a.Struct
	structMap  map[t.ID]*a.Struct
	perFunc    perFunc
}

func (g *gen) printf(format string, args ...interface{}) { fmt.Fprintf(&g.buffer, format, args...) }
func (g *gen) writeb(b byte)                             { g.buffer.WriteByte(b) }
func (g *gen) writes(s string)                           { g.buffer.WriteString(s) }

func (g *gen) jumpTarget(n *a.While) (uint32, error) {
	if g.perFunc.jumpTargets == nil {
		g.perFunc.jumpTargets = map[*a.While]uint32{}
	}
	if jt, ok := g.perFunc.jumpTargets[n]; ok {
		return jt, nil
	}
	jt := uint32(len(g.perFunc.jumpTargets))
	if jt == 1000000 {
		return 0, fmt.Errorf("too many jump targets")
	}
	g.perFunc.jumpTargets[n] = jt
	return jt, nil
}

func (g *gen) generate() error {
	g.statusMap = map[t.ID]status{}
	if err := g.forEachStatus(bothPubPri, (*gen).gatherStatuses); err != nil {
		return err
	}

	// Make a topologically sorted list of structs.
	unsortedStructs := []*a.Struct(nil)
	for _, file := range g.files {
		for _, n := range file.TopLevelDecls() {
			if n.Kind() == a.KStruct {
				unsortedStructs = append(unsortedStructs, n.Struct())
			}
		}
	}
	var ok bool
	g.structList, ok = a.TopologicalSortStructs(unsortedStructs)
	if !ok {
		return fmt.Errorf("cyclical struct definitions")
	}
	g.structMap = map[t.ID]*a.Struct{}
	for _, n := range g.structList {
		g.structMap[n.Name()] = n
	}

	if err := g.genHeader(); err != nil {
		return err
	}
	g.writes("// C HEADER ENDS HERE.\n\n")
	return g.genImpl()
}

func (g *gen) genHeader() error {
	includeGuard := "PUFFS_" + strings.ToUpper(g.pkgName) + "_H"
	g.printf("#ifndef %s\n#define %s\n\n", includeGuard, includeGuard)

	g.printf("// Code generated by puffs-gen-c. DO NOT EDIT.\n\n")
	g.writes(baseHeader)
	g.writes("\n#ifdef __cplusplus\nextern \"C\" {\n#endif\n\n")

	g.writes("// ---------------- Status Codes\n\n")
	g.writes("// Status codes are non-positive integers.\n")
	g.writes("//\n")
	g.writes("// The least significant bit indicates a non-recoverable status code: an error.\n")
	g.writes("typedef enum {\n")
	for i, s := range builtInStatuses {
		nudge := ""
		if strings.HasPrefix(s, "error ") {
			nudge = "+1"
		}
		g.printf("%s = %d%s,\n", g.cName(s), -2*i, nudge)
	}
	for i, s := range g.statusList {
		nudge := ""
		if s.isError {
			nudge = "+1"
		}
		g.printf("%s = %d%s,\n", s.name, -2*(userDefinedStatusBase+i), nudge)
	}
	g.printf("} puffs_%s_status;\n\n", g.pkgName)
	g.printf("bool puffs_%s_status_is_error(puffs_%s_status s);\n\n", g.pkgName, g.pkgName)
	g.printf("const char* puffs_%s_status_string(puffs_%s_status s);\n\n", g.pkgName, g.pkgName)

	g.writes("// ---------------- Structs\n\n")
	for _, n := range g.structList {
		if err := g.writeStruct(n); err != nil {
			return err
		}
	}

	g.writes("// ---------------- Public Constructor and Destructor Prototypes\n\n")
	for _, n := range g.structList {
		if n.Public() {
			if err := g.writeCtorPrototype(n); err != nil {
				return err
			}
		}
	}

	g.writes("// ---------------- Public Function Prototypes\n\n")
	if err := g.forEachFunc(pubOnly, (*gen).writeFuncPrototype); err != nil {
		return err
	}

	g.writes("\n#ifdef __cplusplus\n}  // extern \"C\"\n#endif\n\n")
	g.printf("#endif  // %s\n\n", includeGuard)
	return nil
}

func (g *gen) genImpl() error {
	g.writes(baseImpl)
	g.writes("\n")

	g.writes("// ---------------- Status Codes Implementations\n\n")
	g.printf("bool puffs_%s_status_is_error(puffs_%s_status s) {"+
		"return s & 1; }\n\n", g.pkgName, g.pkgName)
	g.printf("const char* puffs_%s_status_strings[%d] = {\n", g.pkgName, len(builtInStatuses)+len(g.statusList))
	for _, s := range builtInStatuses {
		if strings.HasPrefix(s, "status ") {
			s = s[len("status "):]
		} else if strings.HasPrefix(s, "error ") {
			s = s[len("error "):]
		}
		s = g.pkgName + ": " + s
		g.printf("%q,", s)
	}
	for _, s := range g.statusList {
		g.printf("%q,", g.pkgName+": "+s.msg)
	}
	g.writes("};\n\n")

	g.printf("const char* puffs_%s_status_string(puffs_%s_status s) {\n", g.pkgName, g.pkgName)
	g.writes("s = -(s >> 1); if (0 <= s) {\n")
	g.printf("if (s < %d) { return puffs_%s_status_strings[s]; }\n",
		len(builtInStatuses), g.pkgName)
	g.printf("s -= %d;\n", userDefinedStatusBase-len(builtInStatuses))
	g.printf("if ((%d <= s) && (s < %d)) { return puffs_%s_status_strings[s]; }\n",
		len(builtInStatuses), len(builtInStatuses)+len(g.statusList), g.pkgName)
	g.printf("}\nreturn \"%s: unknown status\";\n", g.pkgName)
	g.writes("}\n\n")

	g.writes("// ---------------- Private Constructor and Destructor Prototypes\n\n")
	for _, n := range g.structList {
		if !n.Public() {
			if err := g.writeCtorPrototype(n); err != nil {
				return err
			}
		}
	}

	g.writes("// ---------------- Private Function Prototypes\n\n")
	if err := g.forEachFunc(priOnly, (*gen).writeFuncPrototype); err != nil {
		return err
	}

	g.writes("// ---------------- Constructor and Destructor Implementations\n\n")
	g.writes("// PUFFS_MAGIC is a magic number to check that constructors are called. It's\n")
	g.writes("// not foolproof, given C doesn't automatically zero memory before use, but it\n")
	g.writes("// should catch 99.99% of cases.\n")
	g.writes("//\n")
	g.writes("// Its (non-zero) value is arbitrary, based on md5sum(\"puffs\").\n")
	g.writes("#define PUFFS_MAGIC (0xCB3699CCU)\n\n")
	g.writes("// PUFFS_ALREADY_ZEROED is passed from a container struct's constructor to a\n")
	g.writes("// containee struct's constructor when the container has already zeroed the\n")
	g.writes("// containee's memory.\n")
	g.writes("//\n")
	g.writes("// Its (non-zero) value is arbitrary, based on md5sum(\"zeroed\").\n")
	g.writes("#define PUFFS_ALREADY_ZEROED (0x68602EF1U)\n\n")
	for _, n := range g.structList {
		if err := g.writeCtorImpl(n); err != nil {
			return err
		}
	}

	g.writes("// ---------------- Function Implementations\n\n")
	if err := g.forEachFunc(bothPubPri, (*gen).writeFuncImpl); err != nil {
		return err
	}

	return nil
}

func (g *gen) forEachFunc(v visibility, f func(*gen, *a.Func) error) error {
	for _, file := range g.files {
		for _, n := range file.TopLevelDecls() {
			if n.Kind() != a.KFunc ||
				(v == pubOnly && n.Raw().Flags()&a.FlagsPublic == 0) ||
				(v == priOnly && n.Raw().Flags()&a.FlagsPublic != 0) {
				continue
			}
			if err := f(g, n.Func()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gen) forEachStatus(v visibility, f func(*gen, *a.Status) error) error {
	for _, file := range g.files {
		for _, n := range file.TopLevelDecls() {
			if n.Kind() != a.KStatus ||
				(v == pubOnly && n.Raw().Flags()&a.FlagsPublic == 0) ||
				(v == priOnly && n.Raw().Flags()&a.FlagsPublic != 0) {
				continue
			}
			if err := f(g, n.Status()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gen) cName(name string) string {
	b := []byte(nil)
	b = append(b, "puffs_"...)
	b = append(b, g.pkgName...)
	b = append(b, '_')
	for _, r := range name {
		if 'A' <= r && r <= 'Z' {
			b = append(b, byte(r+'a'-'A'))
		} else if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') || ('_' == r) {
			b = append(b, byte(r))
		} else if ' ' == r {
			b = append(b, '_')
		}
	}
	return string(b)
}

func (g *gen) gatherStatuses(n *a.Status) error {
	msg := n.Message().String(g.tm)
	if len(msg) < 2 || msg[0] != '"' || msg[len(msg)-1] != '"' {
		return fmt.Errorf("bad status message %q", msg)
	}
	msg = msg[1 : len(msg)-1]
	prefix := "status "
	isError := n.Keyword().Key() == t.KeyError
	if isError {
		prefix = "error "
	}
	s := status{
		name:    g.cName(prefix + msg),
		msg:     msg,
		isError: isError,
	}
	g.statusList = append(g.statusList, s)
	g.statusMap[n.Message()] = s
	return nil
}

func (g *gen) writeStruct(n *a.Struct) error {
	// For API/ABI compatibility, the very first field in the struct's
	// private_impl must be the status code. This lets the constructor callee
	// set "self->private_impl.status = etc_error_bad_version;" regardless of
	// the sizeof(*self) struct reserved by the caller and even if the caller
	// and callee were built with different versions.
	structName := n.Name().String(g.tm)
	g.writes("typedef struct {\n")
	g.writes("// Do not access the private_impl's fields directly. There is no API/ABI\n")
	g.writes("// compatibility or safety guarantee if you do so. Instead, use the\n")
	g.printf("// puffs_%s_%s_etc functions.\n", g.pkgName, structName)
	g.writes("//\n")
	g.writes("// In C++, these fields would be \"private\", but C does not support that.\n")
	g.writes("//\n")
	g.writes("// It is a struct, not a struct*, so that it can be stack allocated.\n")
	g.writes("struct {\n")
	if n.Suspendible() {
		g.printf("puffs_%s_status status;\n", g.pkgName)
		g.printf("uint32_t magic;\n")
	}
	for _, o := range n.Fields() {
		o := o.Field()
		if err := g.writeCTypeName(o.XType(), fPrefix, o.Name().String(g.tm)); err != nil {
			return err
		}
		g.writes(";\n")
	}
	g.printf("} private_impl;\n } puffs_%s_%s;\n\n", g.pkgName, structName)
	return nil
}

func (g *gen) writeCtorSignature(n *a.Struct, public bool, ctor bool) {
	structName := n.Name().String(g.tm)
	ctorName := "destructor"
	if ctor {
		ctorName = "constructor"
		if public {
			g.printf("// puffs_%s_%s_%s is a constructor function.\n", g.pkgName, structName, ctorName)
			g.printf("//\n")
			g.printf("// It should be called before any other puffs_%s_%s_* function.\n",
				g.pkgName, structName)
			g.printf("//\n")
			g.printf("// Pass PUFFS_VERSION and 0 for puffs_version and for_internal_use_only.\n")
		}
	}
	g.printf("void puffs_%s_%s_%s(puffs_%s_%s *self", g.pkgName, structName, ctorName, g.pkgName, structName)
	if ctor {
		g.printf(", uint32_t puffs_version, uint32_t for_internal_use_only")
	}
	g.printf(")")
}

func (g *gen) writeCtorPrototype(n *a.Struct) error {
	if !n.Suspendible() {
		return nil
	}
	for _, ctor := range []bool{true, false} {
		g.writeCtorSignature(n, n.Public(), ctor)
		g.writes(";\n\n")
	}
	return nil
}

func (g *gen) writeCtorImpl(n *a.Struct) error {
	if !n.Suspendible() {
		return nil
	}
	for _, ctor := range []bool{true, false} {
		g.writeCtorSignature(n, false, ctor)
		g.printf("{\n")
		g.printf("if (!self) { return; }\n")

		if ctor {
			g.printf("if (puffs_version != PUFFS_VERSION) {\n")
			g.printf("self->private_impl.status = puffs_%s_error_bad_version;\n", g.pkgName)
			g.printf("return;\n")
			g.printf("}\n")

			g.writes("if (for_internal_use_only != PUFFS_ALREADY_ZEROED) {" +
				"memset(self, 0, sizeof(*self)); }\n")
			g.writes("self->private_impl.magic = PUFFS_MAGIC;\n")

			for _, f := range n.Fields() {
				f := f.Field()
				if dv := f.DefaultValue(); dv != nil {
					// TODO: set default values for array types.
					g.printf("self->private_impl.%s%s = %d;\n", fPrefix, f.Name().String(g.tm), dv.ConstValue())
				}
			}
		}

		// Call any ctor/dtors on sub-structs.
		for _, f := range n.Fields() {
			f := f.Field()
			x := f.XType()
			if x != x.Innermost() {
				// TODO: arrays of sub-structs.
				continue
			}
			if g.structMap[x.Name()] == nil {
				continue
			}
			if ctor {
				g.printf("puffs_%s_%s_constructor(&self->private_impl.%s%s,"+
					"PUFFS_VERSION, PUFFS_ALREADY_ZEROED);\n",
					g.pkgName, x.Name().String(g.tm), fPrefix, f.Name().String(g.tm))
			} else {
				g.printf("puffs_%s_%s_destructor(&self->private_impl.%s%s);\n",
					g.pkgName, x.Name().String(g.tm), fPrefix, f.Name().String(g.tm))
			}
		}

		g.writes("}\n\n")
	}
	return nil
}

func (g *gen) writeFuncSignature(n *a.Func) error {
	// TODO: write n's return values.
	if n.Suspendible() {
		g.printf("puffs_%s_status", g.pkgName)
	} else {
		g.printf("void")
	}
	g.printf(" puffs_%s", g.pkgName)
	if r := n.Receiver(); r != 0 {
		g.printf("_%s", r.String(g.tm))
	}
	g.printf("_%s(", n.Name().String(g.tm))

	comma := false
	if r := n.Receiver(); r != 0 {
		g.printf("puffs_%s_%s *self", g.pkgName, r.String(g.tm))
		comma = true
	}
	for _, o := range n.In().Fields() {
		if comma {
			g.writeb(',')
		}
		comma = true
		o := o.Field()
		if err := g.writeCTypeName(o.XType(), aPrefix, o.Name().String(g.tm)); err != nil {
			return err
		}
	}

	g.printf(")")
	return nil
}

func (g *gen) writeFuncPrototype(n *a.Func) error {
	if err := g.writeFuncSignature(n); err != nil {
		return err
	}
	g.writes(";\n\n")
	return nil
}

func (g *gen) writeFuncImpl(n *a.Func) error {
	g.perFunc = perFunc{funk: n}
	if err := g.writeFuncSignature(n); err != nil {
		return err
	}
	g.writes("{\n")

	// Check the previous status and the "self" arg.
	if n.Public() {
		g.perFunc.public = true
		if n.Receiver() != 0 {
			g.writes("if (!self) {\n")
			if n.Suspendible() {
				g.printf("return puffs_%s_error_bad_receiver;", g.pkgName)
			} else {
				g.printf("return;")
			}
			g.writes("}\n")
		}
	}

	if n.Suspendible() {
		g.perFunc.suspendible = true
		g.printf("puffs_%s_status status = ", g.pkgName)
		if n.Receiver() != 0 {
			g.writes("self->private_impl.status;\n")
			if n.Public() {
				g.writes("if (status & 1) { return status; }")
			}
		} else {
			g.printf("puffs_%s_status_ok;\n", g.pkgName)
		}
		if n.Public() {
			g.printf("if (self->private_impl.magic != PUFFS_MAGIC) {"+
				"status = puffs_%s_error_constructor_not_called; goto cleanup0; }\n", g.pkgName)
		}
	} else if n.Receiver() != 0 && n.Public() {
		g.writes("if (self->private_impl.status & 1) { return; }")
		g.printf("if (self->private_impl.magic != PUFFS_MAGIC) {"+
			"self->private_impl.status = puffs_%s_error_constructor_not_called; return; }\n", g.pkgName)
	}

	// For public functions, check (at runtime) the other args for bounds and
	// null-ness. For private functions, those checks are done at compile time.
	if n.Public() {
		if err := g.writeFuncImplArgChecks(n); err != nil {
			return err
		}
	}
	g.writes("\n")

	// Generate the local variables.
	if err := g.writeVars(n.Body(), 0); err != nil {
		return err
	}
	g.writes("\n")

	// Generate the function body.
	for _, o := range n.Body() {
		if err := g.writeStatement(o, 0); err != nil {
			return err
		}
	}
	g.writes("\n")

	if g.perFunc.suspendible {
		if g.perFunc.public {
			g.printf("cleanup0: self->private_impl.status = status;\n")
		}
		g.printf("return status;\n")
	}

	g.writes("}\n\n")
	if g.perFunc.tempW != g.perFunc.tempR {
		return fmt.Errorf("internal error: temporary variable count out of sync")
	}
	return nil
}

func (g *gen) writeFuncImplArgChecks(n *a.Func) error {
	checks := []string(nil)

	for _, o := range n.In().Fields() {
		o := o.Field()
		oTyp := o.XType()
		if oTyp.Decorator().Key() != t.KeyPtr && !oTyp.IsRefined() {
			// TODO: Also check elements, for array-typed arguments.
			continue
		}

		switch {
		case oTyp.Decorator().Key() == t.KeyPtr:
			checks = append(checks, fmt.Sprintf("!%s%s", aPrefix, o.Name().String(g.tm)))

		case oTyp.IsRefined():
			bounds := [2]*big.Int{}
			for i, b := range oTyp.Bounds() {
				if b != nil {
					if cv := b.ConstValue(); cv != nil {
						bounds[i] = cv
					}
				}
			}
			if key := oTyp.Name().Key(); key < t.Key(len(numTypeBounds)) {
				ntb := numTypeBounds[key]
				for i := 0; i < 2; i++ {
					if bounds[i] != nil && ntb[i] != nil && bounds[i].Cmp(ntb[i]) == 0 {
						bounds[i] = nil
						continue
					}
				}
			}
			for i, b := range bounds {
				if b != nil {
					op := '<'
					if i != 0 {
						op = '>'
					}
					checks = append(checks, fmt.Sprintf("%s%s %c %s", aPrefix, o.Name().String(g.tm), op, b))
				}
			}
		}
	}

	if len(checks) == 0 {
		return nil
	}

	g.writes("if (")
	for i, c := range checks {
		if i != 0 {
			g.writes(" || ")
		}
		g.writes(c)
	}
	g.writes(") {")
	if n.Suspendible() {
		g.printf("status = puffs_%s_error_bad_argument; goto cleanup0;", g.pkgName)
	} else if n.Receiver() != 0 {
		g.printf("self->private_impl.status = puffs_%s_error_bad_argument; return;", g.pkgName)
	} else {
		g.printf("return;")
	}
	g.writes("}\n")
	return nil
}

func (g *gen) writeVars(block []*a.Node, depth uint32) error {
	if depth > a.MaxBodyDepth {
		return fmt.Errorf("body recursion depth too large")
	}
	depth++

	for _, o := range block {
		switch o.Kind() {
		case a.KIf:
			for o := o.If(); o != nil; o = o.ElseIf() {
				if err := g.writeVars(o.BodyIfTrue(), depth); err != nil {
					return err
				}
				if err := g.writeVars(o.BodyIfFalse(), depth); err != nil {
					return err
				}
			}

		case a.KVar:
			o := o.Var()
			if err := g.writeCTypeName(o.XType(), vPrefix, o.Name().String(g.tm)); err != nil {
				return err
			}
			g.writes(";\n")
			continue

		case a.KWhile:
			if err := g.writeVars(o.While().Body(), depth); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gen) writeStatement(n *a.Node, depth uint32) error {
	if depth > a.MaxBodyDepth {
		return fmt.Errorf("body recursion depth too large")
	}
	depth++

	switch n.Kind() {
	case a.KAssert:
		// Assertions only apply at compile-time.
		return nil

	case a.KAssign:
		n := n.Assign()
		if err := g.writeSuspendibles(n.LHS(), depth); err != nil {
			return err
		}
		if err := g.writeSuspendibles(n.RHS(), depth); err != nil {
			return err
		}
		if err := g.writeExpr(n.LHS(), replaceCallSuspendibles, parenthesesMandatory, depth); err != nil {
			return err
		}
		// TODO: does KeyAmpHatEq need special consideration?
		g.writes(cOpNames[0xFF&n.Operator().Key()])
		if err := g.writeExpr(n.RHS(), replaceCallSuspendibles, parenthesesMandatory, depth); err != nil {
			return err
		}
		g.writes(";\n")
		return nil

	case a.KExpr:
		n := n.Expr()
		if err := g.writeSuspendibles(n, depth); err != nil {
			return err
		}
		if n.CallSuspendible() {
			return nil
		}
		return fmt.Errorf("TODO: generate code for foo() when foo is not a ? call-suspendible")

	case a.KIf:
		// TODO: for writeSuspendibles, make sure that we get order of
		// sub-expression evaluation correct.
		n, nCloseCurly := n.If(), 1
		for first := true; ; first = false {
			if n.Condition().Suspendible() {
				if !first {
					g.writeb('{')
					const maxCloseCurly = 1000
					if nCloseCurly == maxCloseCurly {
						return fmt.Errorf("too many nested if's")
					}
					nCloseCurly++
				}
				if err := g.writeSuspendibles(n.Condition(), depth); err != nil {
					return err
				}
			}

			g.writes("if (")
			if err := g.writeExpr(n.Condition(), replaceCallSuspendibles, parenthesesOptional, 0); err != nil {
				return err
			}
			g.writes(") {\n")
			for _, o := range n.BodyIfTrue() {
				if err := g.writeStatement(o, depth); err != nil {
					return err
				}
			}
			if bif := n.BodyIfFalse(); len(bif) > 0 {
				g.writes("} else {")
				for _, o := range bif {
					if err := g.writeStatement(o, depth); err != nil {
						return err
					}
				}
				break
			}
			n = n.ElseIf()
			if n == nil {
				break
			}
			g.writes("} else ")
		}
		for ; nCloseCurly > 0; nCloseCurly-- {
			g.writes("}\n")
		}
		return nil

	case a.KJump:
		n := n.Jump()
		jt, err := g.jumpTarget(n.JumpTarget())
		if err != nil {
			return err
		}
		keyword := "continue"
		if n.Keyword().Key() == t.KeyBreak {
			keyword = "break"
		}
		g.printf("goto label_%d_%s;\n", jt, keyword)
		return nil

	case a.KReturn:
		n := n.Return()
		ret := ""
		if n.Keyword() == 0 {
			ret = fmt.Sprintf("puffs_%s_status_ok", g.pkgName)
		} else {
			ret = g.statusMap[n.Message()].name
		}
		if !g.perFunc.suspendible {
			// TODO: consider the return values, especially if they involve
			// suspendible function calls.
			g.writes("return;\n")
		} else if g.perFunc.public {
			g.printf("status = %s; goto cleanup0;\n", ret)
		} else {
			g.printf("return %s;\n", ret)
		}
		return nil

	case a.KVar:
		n := n.Var()
		if v := n.Value(); v != nil {
			if err := g.writeSuspendibles(v, depth); err != nil {
				return err
			}
		}
		if n.XType().Decorator().Key() == t.KeyOpenBracket {
			if n.Value() != nil {
				return fmt.Errorf("TODO: array initializers for non-zero default values")
			}
			// TODO: arrays of arrays.
			cv := n.XType().ArrayLength().ConstValue()
			// TODO: check that cv is within size_t's range.
			g.printf("for (size_t i = 0; i < %d; i++) { %s%s[i] = 0; }\n", cv, vPrefix, n.Name().String(g.tm))
		} else {
			g.printf("%s%s = ", vPrefix, n.Name().String(g.tm))
			if v := n.Value(); v != nil {
				if err := g.writeExpr(v, replaceCallSuspendibles, parenthesesMandatory, 0); err != nil {
					return err
				}
			} else {
				g.writeb('0')
			}
		}
		g.writes(";\n")
		return nil

	case a.KWhile:
		n := n.While()
		// TODO: consider suspendible calls.

		if n.HasContinue() {
			jt, err := g.jumpTarget(n)
			if err != nil {
				return err
			}
			g.printf("label_%d_continue:;\n", jt)
		}
		g.writes("while (")
		if err := g.writeExpr(n.Condition(), replaceCallSuspendibles, parenthesesOptional, 0); err != nil {
			return err
		}
		g.writes(") {\n")
		for _, o := range n.Body() {
			if err := g.writeStatement(o, depth); err != nil {
				return err
			}
		}
		g.writes("}\n")
		if n.HasBreak() {
			jt, err := g.jumpTarget(n)
			if err != nil {
				return err
			}
			g.printf("label_%d_break:;\n", jt)
		}
		return nil

	}
	return fmt.Errorf("unrecognized ast.Kind (%s) for writeStatement", n.Kind())
}

func (g *gen) writeSuspendibles(n *a.Expr, depth uint32) error {
	if !n.Suspendible() {
		return nil
	}
	return g.writeCallSuspendibles(n, depth)
}

func (g *gen) writeCallSuspendibles(n *a.Expr, depth uint32) error {
	// The evaluation order for suspendible calls (which can have side effects)
	// is important here: LHS, MHS, RHS, Args and finally the node itself.
	if !n.CallSuspendible() {
		if depth > a.MaxExprDepth {
			return fmt.Errorf("expression recursion depth too large")
		}
		depth++

		for _, o := range n.Node().Raw().SubNodes() {
			if o != nil && o.Kind() == a.KExpr {
				if err := g.writeCallSuspendibles(o.Expr(), depth); err != nil {
					return err
				}
			}
		}
		for _, o := range n.Args() {
			if o != nil && o.Kind() == a.KExpr {
				if err := g.writeCallSuspendibles(o.Expr(), depth); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// TODO: delete these hacks that only matches "in.src.read_u8?()" etc.
	if isInSrcReadU8(g.tm, n) {
		if g.perFunc.tempW > maxTemp {
			return fmt.Errorf("too many temporary variables required")
		}
		temp := g.perFunc.tempW
		g.perFunc.tempW++

		// TODO: suspend coroutine state.
		g.printf("if (%ssrc->ri >= %ssrc->wi) { status = "+
			"%ssrc->closed ? puffs_%s_error_unexpected_eof : puffs_%s_status_short_read;",
			aPrefix, aPrefix, aPrefix, g.pkgName, g.pkgName)
		if g.perFunc.public && g.perFunc.suspendible {
			g.writes("goto cleanup0;")
		} else {
			g.writes("return status;")
		}
		g.writes("}\n")
		// TODO: watch for passing an array type to writeCTypeName? In C, an
		// array type can decay into a pointer.
		if err := g.writeCTypeName(n.MType(), tPrefix, fmt.Sprint(temp)); err != nil {
			return err
		}
		g.printf(" = %ssrc->ptr[%ssrc->ri++];\n", aPrefix, aPrefix)

	} else if isInDst(g.tm, n, t.KeyWrite) {
		// TODO: suspend coroutine state.
		//
		// TODO: don't assume that the argument is "this.stack[s:]".
		g.printf("if (%sdst->closed) { status = puffs_%s_error_closed_for_writes;", aPrefix, g.pkgName)
		if g.perFunc.public && g.perFunc.suspendible {
			g.writes("goto cleanup0;")
		} else {
			g.writes("return status;")
		}
		g.writes("}\n")
		g.printf("if ((%sdst->len - %sdst->wi) < (sizeof(self->private_impl.f_stack) - v_s)) {", aPrefix, aPrefix)
		g.printf("status = puffs_%s_status_short_write;", g.pkgName)
		if g.perFunc.public && g.perFunc.suspendible {
			g.writes("goto cleanup0;")
		} else {
			g.writes("return status;")
		}
		g.writes("}\n")
		g.printf("memmove(" +
			"a_dst->ptr + a_dst->wi," +
			"self->private_impl.f_stack + v_s," +
			"sizeof(self->private_impl.f_stack) - v_s);\n")
		g.printf("a_dst->wi += sizeof(self->private_impl.f_stack) - v_s;\n")

	} else if isInDst(g.tm, n, t.KeyWriteU8) {
		// TODO: suspend coroutine state.
		g.printf("if (%sdst->wi >= %sdst->len) { status = puffs_%s_status_short_write;",
			aPrefix, aPrefix, g.pkgName)
		if g.perFunc.public && g.perFunc.suspendible {
			g.writes("goto cleanup0;")
		} else {
			g.writes("return status;")
		}
		g.writes("}\n")
		g.printf("%sdst->ptr[%sdst->wi++] = ", aPrefix, aPrefix)
		x := n.Args()[0].Arg().Value()
		if err := g.writeExpr(x, replaceCallSuspendibles, parenthesesMandatory, depth); err != nil {
			return err
		}
		g.writes(";\n")

	} else if isThisDecodeHeader(g.tm, n) {
		g.printf("status = puffs_%s_%s_decode_header(self, %ssrc);\n",
			g.pkgName, g.perFunc.funk.Receiver().String(g.tm), aPrefix)
		g.writes("if (status) { goto cleanup0; }\n")

	} else {
		// TODO: fix this.
		//
		// This might involve calling g.writeExpr with replaceNothing??
		return fmt.Errorf("cannot convert Puffs call %q to C", n.String(g.tm))
	}
	return nil
}

func (g *gen) writeExpr(n *a.Expr, rp replacementPolicy, pp parenthesesPolicy, depth uint32) error {
	if depth > a.MaxExprDepth {
		return fmt.Errorf("expression recursion depth too large")
	}
	depth++

	if rp == replaceCallSuspendibles && n.CallSuspendible() {
		if g.perFunc.tempR >= g.perFunc.tempW {
			return fmt.Errorf("internal error: temporary variable count out of sync")
		}
		// TODO: check that this works with nested call-suspendibles:
		// "foo?().bar().qux?()(p?(), q?())".
		//
		// Also be aware of evaluation order in the presence of side effects:
		// in "foo(a?(), b!(), c?())", b should be called between a and c.
		g.printf("%s%d", tPrefix, g.perFunc.tempR)
		g.perFunc.tempR++
		return nil
	}

	if cv := n.ConstValue(); cv != nil {
		if !n.MType().IsBool() {
			g.writes(cv.String())
		} else if cv.Cmp(zero) == 0 {
			g.writes("false")
		} else if cv.Cmp(one) == 0 {
			g.writes("true")
		} else {
			return fmt.Errorf("%v has type bool but constant value %v is neither 0 or 1", n.String(g.tm), cv)
		}
		return nil
	}

	switch n.ID0().Flags() & (t.FlagsUnaryOp | t.FlagsBinaryOp | t.FlagsAssociativeOp) {
	case 0:
		if err := g.writeExprOther(n, rp, depth); err != nil {
			return err
		}
	case t.FlagsUnaryOp:
		if err := g.writeExprUnaryOp(n, rp, depth); err != nil {
			return err
		}
	case t.FlagsBinaryOp:
		if err := g.writeExprBinaryOp(n, rp, pp, depth); err != nil {
			return err
		}
	case t.FlagsAssociativeOp:
		if err := g.writeExprAssociativeOp(n, rp, depth); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unrecognized token.Key (0x%X) for writeExpr", n.ID0().Key())
	}

	return nil
}

func (g *gen) writeExprOther(n *a.Expr, rp replacementPolicy, depth uint32) error {
	switch n.ID0().Key() {
	case 0:
		if id1 := n.ID1(); id1.Key() == t.KeyThis {
			g.writes("self->private_impl")
		} else {
			// TODO: don't assume that the vPrefix is necessary.
			g.writes(vPrefix)
			g.writes(id1.String(g.tm))
		}
		return nil

	case t.KeyOpenParen:
		// n is a function call.
		// TODO: delete this hack that only matches "foo.low_bits(etc)".
		if isLowBits(g.tm, n) {
			g.printf("PUFFS_LOW_BITS(")
			if err := g.writeExpr(n.LHS().Expr().LHS().Expr(), rp, parenthesesMandatory, depth); err != nil {
				return err
			}
			g.writes(",")
			if err := g.writeExpr(n.Args()[0].Arg().Value(), rp, parenthesesMandatory, depth); err != nil {
				return err
			}
			g.writes(")")
			return nil
		}
		// TODO.

	case t.KeyOpenBracket:
		// n is an index.
		if err := g.writeExpr(n.LHS().Expr(), rp, parenthesesMandatory, depth); err != nil {
			return err
		}
		g.writeb('[')
		if err := g.writeExpr(n.RHS().Expr(), rp, parenthesesOptional, depth); err != nil {
			return err
		}
		g.writeb(']')
		return nil

	case t.KeyColon:
	// n is a slice.
	// TODO.

	case t.KeyDot:
		if n.LHS().Expr().ID1().Key() == t.KeyIn {
			g.writes(aPrefix)
			g.writes(n.ID1().String(g.tm))
			return nil
		}

		if err := g.writeExpr(n.LHS().Expr(), rp, parenthesesMandatory, depth); err != nil {
			return err
		}
		// TODO: choose between . vs -> operators.
		//
		// TODO: don't assume that the fPrefix is necessary.
		g.writes(".")
		g.writes(fPrefix)
		g.writes(n.ID1().String(g.tm))
		return nil
	}
	return fmt.Errorf("unrecognized token.Key (0x%X) for writeExprOther", n.ID0().Key())
}

func isInSrcReadU8(tm *t.Map, n *a.Expr) bool {
	if n.ID0().Key() != t.KeyOpenParen || !n.CallSuspendible() || len(n.Args()) != 0 {
		return false
	}
	n = n.LHS().Expr()
	if n.ID0().Key() != t.KeyDot || n.ID1().Key() != t.KeyReadU8 {
		return false
	}
	n = n.LHS().Expr()
	if n.ID0().Key() != t.KeyDot || n.ID1() != tm.ByName("src") {
		return false
	}
	n = n.LHS().Expr()
	return n.ID0() == 0 && n.ID1().Key() == t.KeyIn
}

func isInDst(tm *t.Map, n *a.Expr, methodName t.Key) bool {
	// TODO: check that n.Args() is "(x:bar)".
	if n.ID0().Key() != t.KeyOpenParen || !n.CallSuspendible() || len(n.Args()) != 1 {
		return false
	}
	n = n.LHS().Expr()
	if n.ID0().Key() != t.KeyDot || n.ID1().Key() != methodName {
		return false
	}
	n = n.LHS().Expr()
	if n.ID0().Key() != t.KeyDot || n.ID1() != tm.ByName("dst") {
		return false
	}
	n = n.LHS().Expr()
	return n.ID0() == 0 && n.ID1().Key() == t.KeyIn
}

func isThisDecodeHeader(tm *t.Map, n *a.Expr) bool {
	// TODO: check that n.Args() is "(src:in.src)".
	if n.ID0().Key() != t.KeyOpenParen || !n.CallSuspendible() || len(n.Args()) != 1 {
		return false
	}
	n = n.LHS().Expr()
	if n.ID0().Key() != t.KeyDot || n.ID1() != tm.ByName("decode_header") {
		return false
	}
	n = n.LHS().Expr()
	return n.ID0() == 0 && n.ID1().Key() == t.KeyThis
}

func isLowBits(tm *t.Map, n *a.Expr) bool {
	// TODO: check that n.Args() is "(n:bar)".
	if n.ID0().Key() != t.KeyOpenParen || n.CallImpure() || len(n.Args()) != 1 {
		return false
	}
	n = n.LHS().Expr()
	return n.ID0().Key() == t.KeyDot && n.ID1().Key() == t.KeyLowBits
}

func (g *gen) writeExprUnaryOp(n *a.Expr, rp replacementPolicy, depth uint32) error {
	// TODO.
	return nil
}

func (g *gen) writeExprBinaryOp(n *a.Expr, rp replacementPolicy, pp parenthesesPolicy, depth uint32) error {
	op := n.ID0()
	if op.Key() == t.KeyXBinaryAs {
		return g.writeExprAs(n.LHS().Expr(), n.RHS().TypeExpr(), rp, depth)
	}
	if pp == parenthesesMandatory {
		g.writeb('(')
	}
	if err := g.writeExpr(n.LHS().Expr(), rp, parenthesesMandatory, depth); err != nil {
		return err
	}
	// TODO: does KeyXBinaryAmpHat need special consideration?
	g.writes(cOpNames[0xFF&op.Key()])
	if err := g.writeExpr(n.RHS().Expr(), rp, parenthesesMandatory, depth); err != nil {
		return err
	}
	if pp == parenthesesMandatory {
		g.writeb(')')
	}
	return nil
}

func (g *gen) writeExprAs(lhs *a.Expr, rhs *a.TypeExpr, rp replacementPolicy, depth uint32) error {
	g.writes("((")
	// TODO: watch for passing an array type to writeCTypeName? In C, an array
	// type can decay into a pointer.
	if err := g.writeCTypeName(rhs, "", ""); err != nil {
		return err
	}
	g.writes(")(")
	if err := g.writeExpr(lhs, rp, parenthesesMandatory, depth); err != nil {
		return err
	}
	g.writes("))")
	return nil
}

func (g *gen) writeExprAssociativeOp(n *a.Expr, rp replacementPolicy, depth uint32) error {
	opName := cOpNames[0xFF&n.ID0().Key()]
	for i, o := range n.Args() {
		if i != 0 {
			g.writes(opName)
		}
		if err := g.writeExpr(o.Expr(), rp, parenthesesMandatory, depth); err != nil {
			return err
		}
	}
	return nil
}

func (g *gen) writeCTypeName(n *a.TypeExpr, varNamePrefix string, varName string) error {
	// It may help to refer to http://unixwiz.net/techtips/reading-cdecl.html

	// maxNumPointers is an arbitrary implementation restriction.
	const maxNumPointers = 16

	x := n
	for ; x != nil && x.Decorator().Key() == t.KeyOpenBracket; x = x.Inner() {
	}

	numPointers, innermost := 0, x
	for ; innermost != nil && innermost.Inner() != nil; innermost = innermost.Inner() {
		// TODO: "nptr T", not just "ptr T".
		if p := innermost.Decorator().Key(); p == t.KeyPtr {
			if numPointers == maxNumPointers {
				return fmt.Errorf("cannot convert Puffs type %q to C: too many ptr's", n.String(g.tm))
			}
			numPointers++
			continue
		}
		// TODO: fix this.
		return fmt.Errorf("cannot convert Puffs type %q to C", n.String(g.tm))
	}

	fallback := true
	if k := innermost.Name().Key(); k < t.Key(len(cTypeNames)) {
		if s := cTypeNames[k]; s != "" {
			g.writes(s)
			fallback = false
		}
	}
	if fallback {
		g.printf("puffs_%s_%s", g.pkgName, n.Name().String(g.tm))
	}

	for i := 0; i < numPointers; i++ {
		g.writeb('*')
	}

	g.writeb(' ')
	g.writes(varNamePrefix)
	g.writes(varName)

	x = n
	for ; x != nil && x.Decorator().Key() == t.KeyOpenBracket; x = x.Inner() {
		g.writeb('[')
		g.writes(x.ArrayLength().ConstValue().String())
		g.writeb(']')
	}

	return nil
}

var numTypeBounds = [256][2]*big.Int{
	t.KeyI8:    {big.NewInt(-1 << 7), big.NewInt(1<<7 - 1)},
	t.KeyI16:   {big.NewInt(-1 << 15), big.NewInt(1<<15 - 1)},
	t.KeyI32:   {big.NewInt(-1 << 31), big.NewInt(1<<31 - 1)},
	t.KeyI64:   {big.NewInt(-1 << 63), big.NewInt(1<<63 - 1)},
	t.KeyU8:    {zero, big.NewInt(0).SetUint64(1<<8 - 1)},
	t.KeyU16:   {zero, big.NewInt(0).SetUint64(1<<16 - 1)},
	t.KeyU32:   {zero, big.NewInt(0).SetUint64(1<<32 - 1)},
	t.KeyU64:   {zero, big.NewInt(0).SetUint64(1<<64 - 1)},
	t.KeyUsize: {zero, zero},
	t.KeyBool:  {zero, one},
}

var cTypeNames = [...]string{
	t.KeyI8:    "int8_t",
	t.KeyI16:   "int16_t",
	t.KeyI32:   "int32_t",
	t.KeyI64:   "int64_t",
	t.KeyU8:    "uint8_t",
	t.KeyU16:   "uint16_t",
	t.KeyU32:   "uint32_t",
	t.KeyU64:   "uint64_t",
	t.KeyUsize: "size_t",
	t.KeyBool:  "bool",
	t.KeyBuf1:  "puffs_base_buf1",
	t.KeyBuf2:  "puffs_base_buf2",
}

var cOpNames = [256]string{
	t.KeyEq:       " = ",
	t.KeyPlusEq:   " += ",
	t.KeyMinusEq:  " -= ",
	t.KeyStarEq:   " *= ",
	t.KeySlashEq:  " /= ",
	t.KeyShiftLEq: " <<= ",
	t.KeyShiftREq: " >>= ",
	t.KeyAmpEq:    " &= ",
	t.KeyAmpHatEq: " no_such_amp_hat_C_operator ",
	t.KeyPipeEq:   " |= ",
	t.KeyHatEq:    " ^= ",

	t.KeyXUnaryPlus:  "+",
	t.KeyXUnaryMinus: "-",
	t.KeyXUnaryNot:   "!",

	t.KeyXBinaryPlus:        " + ",
	t.KeyXBinaryMinus:       " - ",
	t.KeyXBinaryStar:        " * ",
	t.KeyXBinarySlash:       " / ",
	t.KeyXBinaryShiftL:      " << ",
	t.KeyXBinaryShiftR:      " >> ",
	t.KeyXBinaryAmp:         " & ",
	t.KeyXBinaryAmpHat:      " no_such_amp_hat_C_operator ",
	t.KeyXBinaryPipe:        " | ",
	t.KeyXBinaryHat:         " ^ ",
	t.KeyXBinaryNotEq:       " != ",
	t.KeyXBinaryLessThan:    " < ",
	t.KeyXBinaryLessEq:      " <= ",
	t.KeyXBinaryEqEq:        " == ",
	t.KeyXBinaryGreaterEq:   " >= ",
	t.KeyXBinaryGreaterThan: " > ",
	t.KeyXBinaryAnd:         " && ",
	t.KeyXBinaryOr:          " || ",
	t.KeyXBinaryAs:          " no_such_as_C_operator ",

	t.KeyXAssociativePlus: " + ",
	t.KeyXAssociativeStar: " * ",
	t.KeyXAssociativeAmp:  " & ",
	t.KeyXAssociativePipe: " | ",
	t.KeyXAssociativeHat:  " ^ ",
	t.KeyXAssociativeAnd:  " && ",
	t.KeyXAssociativeOr:   " || ",
}
