// Copyright 2016 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package vm

import (
	"fmt"
	"regexp"
	"time"

	"github.com/golang/glog"
	"github.com/google/mtail/metrics"
	"github.com/google/mtail/metrics/datum"
	"github.com/pkg/errors"
)

// compiler is data for the code generator.
type codegen struct {
	name string // Name of the program.

	errors ErrorList // Compile errors.
	obj    object    // The object to return

	decos []*decoNode // Decorator stack to unwind
}

// CodeGen is the function that compiles the program to bytecode and data.
func CodeGen(name string, ast astNode) (*object, error) {
	c := &codegen{name: name}
	Walk(c, ast)
	if len(c.errors) > 0 {
		return nil, c.errors
	}
	return &c.obj, nil
}

func (c *codegen) errorf(pos *position, format string, args ...interface{}) {
	e := "Internal compiler error, aborting compilation: " + fmt.Sprintf(format, args...)
	c.errors.Add(pos, e)
}

func (c *codegen) emit(i instr) {
	c.obj.prog = append(c.obj.prog, i)
}

// pc returns the program offset of the last instruction
func (c *codegen) pc() int {
	return len(c.obj.prog) - 1
}

func (c *codegen) VisitBefore(node astNode) Visitor {
	switch n := node.(type) {

	case *declNode:
		var name string
		if n.exportedName != "" {
			name = n.exportedName
		} else {
			name = n.name
		}
		// If the Type is not in the map, then default to metrics.Int.  This is
		// a hack for metrics that no type can be inferred, retaining
		// historical behaviour.
		t := n.Type()
		if IsDimension(t) {
			t = t.(*TypeOperator).Args[len(t.(*TypeOperator).Args)-1]
		}
		var dtyp datum.Type
		switch {
		case Equals(Float, t):
			dtyp = metrics.Float
		default:
			if !IsComplete(t) {
				glog.Infof("Incomplete type %v for %#v", t, n)
			}
			dtyp = metrics.Int
		}
		m := metrics.NewMetric(name, c.name, n.kind, dtyp, n.keys...)
		m.SetSource(n.Pos().String())
		// Scalar counters can be initialized to zero.  Dimensioned counters we
		// don't know the values of the labels yet.  Gauges and Timers we can't
		// assume start at zero.
		if len(n.keys) == 0 && n.kind == metrics.Counter {
			d, err := m.GetDatum()
			if err != nil {
				c.errorf(n.Pos(), "%s", err)
				return nil
			}
			// Initialize to zero at the zero time.
			if dtyp == metrics.Int {
				datum.SetInt(d, 0, time.Unix(0, 0))
			} else {
				datum.SetFloat(d, 0, time.Unix(0, 0))
			}
		}
		m.Hidden = n.hidden
		(*n.sym).Binding = m
		n.sym.Addr = len(c.obj.m)
		c.obj.m = append(c.obj.m, m)
		return nil

	case *condNode:
		if n.cond != nil {
			Walk(c, n.cond)
		}
		// Save PC of previous jump instruction emitted by the n.cond
		// compilation.  (See regexNode and relNode cases, which will emit a
		// jump as the last instr.)  This jump will skip over the truthNode.
		pc := c.pc()
		// Set matched flag false for children.
		c.emit(instr{setmatched, false})
		Walk(c, n.truthNode)
		// Re-set matched flag to true for rest of current block.
		c.emit(instr{setmatched, true})
		// Rewrite n.cond's jump target to jump to instruction after block.
		c.obj.prog[pc].opnd = c.pc() + 1
		// Now also emit the else clause, and a jump.
		if n.elseNode != nil {
			c.emit(instr{op: jmp})
			// Rewrite jump again to avoid this else-skipper just emitted.
			c.obj.prog[pc].opnd = c.pc() + 1
			// Now get the PC of the else-skipper just emitted.
			pc = c.pc()
			Walk(c, n.elseNode)
			// Rewrite else-skipper to the next PC.
			c.obj.prog[pc].opnd = c.pc() + 1
		}
		return nil

	case *regexNode:
		re, err := regexp.Compile(n.pattern)
		if err != nil {
			c.errorf(n.Pos(), "%s", err)
			return nil
		}
		c.obj.re = append(c.obj.re, re)
		// Store the location of this regular expression in the regexNode
		n.addr = len(c.obj.re) - 1
		c.emit(instr{match, n.addr})
		c.emit(instr{op: jnm})

	case *stringConstNode:
		c.obj.str = append(c.obj.str, n.text)
		c.emit(instr{str, len(c.obj.str) - 1})

	case *intConstNode:
		c.emit(instr{push, n.i})

	case *floatConstNode:
		c.emit(instr{push, n.f})

	case *idNode:
		if n.sym == nil || n.sym.Binding == nil {
			c.errorf(n.Pos(), "No metric bound to identifier %q", n.name)
			return nil
		}
		c.emit(instr{mload, n.sym.Addr})
		m := n.sym.Binding.(*metrics.Metric)
		c.emit(instr{dload, len(m.Keys)})

	case *caprefNode:
		if n.sym == nil || n.sym.Binding == nil {
			c.errorf(n.Pos(), "No regular expression bound to capref %q", n.name)
			return nil
		}
		rn := n.sym.Binding.(*regexNode)
		// rn.addr contains the index of the regular expression object,
		// which correlates to storage on the re slice
		c.emit(instr{push, rn.addr})
		// n.sym.addr is the capture group offset
		c.emit(instr{capref, n.sym.Addr})

	case *defNode:
		// Do nothing, defs are inlined.
		return nil

	case *decoNode:
		// Put the current block on the stack
		c.decos = append(c.decos, n)
		if n.def == nil {
			c.errorf(n.Pos(), "No definition found for decorator %q", n.name)
			return nil
		}
		// then iterate over the decorator's nodes
		Walk(c, n.def.block)
		c.decos = c.decos[:len(c.decos)-1]
		return nil

	case *nextNode:
		// Visit the 'next' block on the decorated block stack
		deco := c.decos[len(c.decos)-1]
		Walk(c, deco.block)
		return nil

	case *otherwiseNode:
		c.emit(instr{op: otherwise})
		c.emit(instr{op: jnm})

	case *delNode:
		Walk(c, n.n)
		// overwrite the dload instruction
		pc := c.pc()
		c.obj.prog[pc].op = del

	case *binaryExprNode:
		switch n.op {
		case AND:
			Walk(c, n.lhs)
			// pc is jump from first comparison, triggered if this expression is false
			pc1 := c.pc()
			Walk(c, n.rhs)
			pc2 := c.pc()
			// bounce through the second and leave it there for the condNode containing to overwrite
			c.obj.prog[pc1].opnd = pc2
			return nil

		case OR:
			Walk(c, n.lhs)
			// pc1 is the jump from first comparison, triggered if false, but we want to jump if true to the block
			pc1 := c.pc()
			Walk(c, n.rhs)
			pc2 := c.pc()
			// condNode is going to insert a setmatched instruction next, then the block
			blockPc := pc2 + 2
			c.obj.prog[pc1].opnd = blockPc
			switch c.obj.prog[pc1].op {
			case jnm:
				c.obj.prog[pc1].op = jm
			case jm:
				c.obj.prog[pc1].op = jnm
			}
			return nil

		case ADD_ASSIGN:
			if Equals(n.Type(), Float) {
				// Double-emit the lhs so that it can be assigned to
				Walk(c, n.lhs)
			}

		default:
			// Didn't handle it, let normal walk proceed
			return c
		}

	}

	return c
}

var typedOperators = map[int]map[Type]opcode{
	PLUS: {Int: iadd,
		Float:  fadd,
		String: cat},
	MINUS: {Int: isub,
		Float: fsub},
	MUL: {Int: imul,
		Float: fmul},
	DIV: {Int: idiv,
		Float: fdiv},
	MOD: {Int: imod,
		Float: fmod},
	POW: {Int: ipow,
		Float: fpow},
	ASSIGN: {Int: iset,
		Float: fset},
}

func (c *codegen) VisitAfter(node astNode) {
	switch n := node.(type) {
	case *builtinNode:
		arglen := 0
		if n.args != nil {
			arglen = len(n.args.(*exprlistNode).children)
		}
		switch n.name {
		case "bool":
		// TODO(jaq): Nothing, no support in VM yet.

		case "int", "float", "string":
			// len args should be 1
			if arglen > 1 {
				c.errorf(n.Pos(), "internal error, too many arguments to builtin %q: %#v", n.name, n)
				return
			}
			if err := c.emitConversion(n.args.(*exprlistNode).children[0].Type(), n.Type()); err != nil {
				c.errorf(n.Pos(), "internal error: %s on node %v", err.Error(), n)
				return
			}

		default:
			c.emit(instr{builtin[n.name], arglen})
		}
	case *unaryExprNode:
		switch n.op {
		case INC:
			c.emit(instr{op: inc})
		case NOT:
			c.emit(instr{op: not})
		}
	case *binaryExprNode:
		switch n.op {
		case LT:
			c.emit(instr{cmp, -1})
			c.emit(instr{op: jnm})
		case GT:
			c.emit(instr{cmp, 1})
			c.emit(instr{op: jnm})
		case LE:
			c.emit(instr{cmp, 1})
			c.emit(instr{op: jm})
		case GE:
			c.emit(instr{cmp, -1})
			c.emit(instr{op: jm})
		case EQ:
			c.emit(instr{cmp, 0})
			c.emit(instr{op: jnm})
		case NE:
			c.emit(instr{cmp, 0})
			c.emit(instr{op: jm})
		case ADD_ASSIGN:
			// When operand is not nil, inc pops the delta from the stack.
			// TODO(jaq): string concatenation, once datums can hold strings.
			switch {
			case Equals(n.Type(), Int):
				c.emit(instr{inc, 0})
			case Equals(n.Type(), Float):
				// Already walked the lhs and rhs of this expression
				c.emit(instr{fadd, nil})
				// And a second lhs
				c.emit(instr{fset, nil})
			default:
				c.errorf(n.Pos(), "Internal error: invalid type for add-assignment: %v", n.op)
				return
			}
		case PLUS, MINUS, MUL, DIV, MOD, POW, ASSIGN:
			opmap, ok := typedOperators[n.op]
			if !ok {
				c.errorf(n.Pos(), "Internal error: no typed operator for binary expression %v", n.op)
				return
			}
			emitflag := false
			for t, opcode := range opmap {
				if Equals(n.Type(), t) {
					c.emit(instr{op: opcode})
					emitflag = true
					break
				}
			}
			if !emitflag {
				c.errorf(n.Pos(), "Invalid type for binary expression: %v", n.Type())
				return
			}
		case BITAND:
			c.emit(instr{op: and})
		case BITOR:
			c.emit(instr{op: or})
		case XOR:
			c.emit(instr{op: xor})
		case SHL:
			c.emit(instr{op: shl})
		case SHR:
			c.emit(instr{op: shr})
		}

	case *convNode:
		if err := c.emitConversion(n.n.Type(), n.Type()); err != nil {
			c.errorf(n.Pos(), "internal error: %s on node %v", err.Error(), n)
			return
		}
	}
}

func (c *codegen) emitConversion(inType, outType Type) error {
	glog.Infof("Conversion: %q to %q", inType, outType)
	if Equals(Int, inType) && Equals(Float, outType) {
		c.emit(instr{op: i2f})
	} else if Equals(String, inType) && Equals(Float, outType) {
		c.emit(instr{op: s2f})
	} else if Equals(String, inType) && Equals(Int, outType) {
		c.emit(instr{op: s2i})
	} else if Equals(Float, inType) && Equals(String, outType) {
		c.emit(instr{op: f2s})
	} else if Equals(Int, inType) && Equals(String, outType) {
		c.emit(instr{op: i2s})
	} else {
		return errors.Errorf("can't convert %q to %q", inType, outType)
	}
	return nil
}
