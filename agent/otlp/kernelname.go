package otlp

import (
	"strings"

	"github.com/ianlancetaylor/demangle"
)

// maxKernelFrame is the length kernel frames are shortened to when possible.
const maxKernelFrame = 160

// kernelFrame turns a (usually mangled) CUDA kernel name into a readable frame.
//
// Fully demangled template kernels (PyTorch, CUTLASS) can be thousands of
// characters long. The kernel keeps its qualified name; inside the template
// arguments namespaces are dropped, lambdas become {lambda} and function
// parameters are removed. If the result is still longer than maxKernelFrame,
// template arguments are elided as <…> starting from the deepest level. The
// interesting part (e.g. GeluCUDAKernelImpl, DivFunctor) is usually a type
// name in the arguments, so they are kept for as long as they fit.
//
// Names that are not C++ mangled names are returned unchanged.
func kernelFrame(name string) string {
	a, err := demangle.ToAST(name, demangle.NoParams, demangle.NoEnclosingParams)
	if err != nil {
		return name
	}
	var s string
	for depth := 8; depth >= 0; depth-- {
		s = shortName(a, depth)
		if len(s) <= maxKernelFrame {
			break
		}
	}
	return s
}

// shortName prints the kernel name a, eliding template arguments nested
// deeper than maxDepth.
func shortName(a demangle.AST, maxDepth int) string {
	p := namePrinter{maxDepth: maxDepth}
	p.print(a, 0, true)
	return p.b.String()
}

type namePrinter struct {
	b        strings.Builder
	maxDepth int
}

// print writes a. depth is the template nesting level; top is true for the
// kernel's own name, which keeps its namespaces.
func (p *namePrinter) print(a demangle.AST, depth int, top bool) {
	switch n := a.(type) {
	case *demangle.Name:
		p.b.WriteString(n.Name)
	case *demangle.Typed: // a function with its type: print the name only
		p.print(n.Name, depth, top)
	case *demangle.MethodWithQualifiers: // "operator()() const"
		p.print(n.Method, depth, top)
	case *demangle.Closure:
		p.b.WriteString("{lambda}")
	case *demangle.Qualified:
		p.qualified(n, depth, top)
	case *demangle.Template:
		p.print(n.Name, depth, top)
		if depth >= p.maxDepth {
			p.b.WriteString("<…>")
			return
		}
		p.b.WriteByte('<')
		for i, arg := range n.Args {
			if i > 0 {
				p.b.WriteString(", ")
			}
			p.print(arg, depth+1, false)
		}
		p.b.WriteByte('>')
	case *demangle.PointerType:
		p.print(n.Base, depth, top)
		p.b.WriteByte('*')
	default:
		p.b.WriteString(demangle.ASTToString(a, demangle.NoParams, demangle.NoEnclosingParams))
	}
}

func (p *namePrinter) qualified(n *demangle.Qualified, depth int, top bool) {
	if n.LocalName { // something defined inside the function n.Scope
		start := p.b.Len()
		p.print(n.Scope, depth, top)
		if isLambda(n.Name) {
			// f()::{lambda()#1}::operator()() const::{lambda()#2} -> f::{lambda}
			if !strings.HasSuffix(p.b.String()[start:], "::{lambda}") {
				p.b.WriteString("::{lambda}")
			}
			return
		}
		p.b.WriteString("::")
		p.print(n.Name, depth, false)
		return
	}
	if top {
		p.print(n.Scope, depth, top)
		p.b.WriteString("::")
	}
	p.print(n.Name, depth, top)
}

// isLambda reports whether a local name is a lambda or its call operator.
func isLambda(a demangle.AST) bool {
	switch n := a.(type) {
	case *demangle.Closure:
		return true
	case *demangle.Qualified:
		_, ok := n.Scope.(*demangle.Closure)
		return ok
	}
	return false
}
