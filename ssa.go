// Copyright 2013 The llgo Authors.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package llgo

import (
	"fmt"

	"code.google.com/p/go.tools/go/types"
	"code.google.com/p/go.tools/ssa"
	"github.com/axw/gollvm/llvm"
)

type translator struct {
	*compiler
	pkg       *ssa.Package
	functions map[*ssa.Function]*LLVMValue
	globals   map[ssa.Value]*LLVMValue
}

func (c *compiler) translateSSA(pkg *ssa.Package) {
	t := &translator{
		compiler:  c,
		pkg:       pkg,
		functions: make(map[*ssa.Function]*LLVMValue),
		globals:   make(map[ssa.Value]*LLVMValue),
	}

	// Initialize global storage.
	for _, m := range pkg.Members {
		switch v := m.(type) {
		case *ssa.Global:
			llvmType := c.types.ToLLVM(v.Type())
			name := fmt.Sprintf("%s.%s", pkg.Object.Path(), v.Name())
			global := llvm.AddGlobal(c.module.Module, llvmType, name)
			global.SetInitializer(llvm.ConstNull(llvmType))
			value := c.NewValue(global, types.NewPointer(v.Type()))
			t.globals[v] = value.makePointee()
		}
	}

	// Translate functions.
	for f, _ := range ssa.AllFunctions(pkg.Prog) {
		t.declareFunction(f)
	}
	for f, _ := range ssa.AllFunctions(pkg.Prog) {
		t.defineFunction(f)
	}
}

// declareFunction adds a function declaration with the given name
// and type to the module.
func (t *translator) declareFunction(f *ssa.Function) {
	name := f.Name()
	if recv := f.Signature.Recv(); recv != nil {
		// receiver name includes package
		name = fmt.Sprintf("%s.%s", recv.Type(), name)
	} else if f.Pkg != nil {
		name = fmt.Sprintf("%s.%s", f.Pkg.Object.Path(), name)
	}
	llvmFunction := t.module.Module.NamedFunction(name)
	if llvmFunction.IsNil() {
		llvmType := t.types.ToLLVM(f.Signature)
		llvmType = llvmType.StructElementTypes()[0].ElementType()
		llvmFunction = llvm.AddFunction(t.module.Module, name, llvmType)
	}
	fn := t.NewValue(llvmFunction, f.Signature)
	t.functions[f] = fn
}

func (t *translator) defineFunction(f *ssa.Function) {
	fr := frame{
		translator: t,
		blocks:     make([]llvm.BasicBlock, len(f.Blocks)),
		locals:     make(map[ssa.Value]*LLVMValue),
		env:        make(map[ssa.Value]*LLVMValue),
	}

	llvmFunction := t.functions[f].LLVMValue()
	fmt.Println("Define function:", llvmFunction.Name())
	for i, param := range f.Params {
		value := t.NewValue(llvmFunction.Param(i), param.Type())
		fr.env[param] = value
	}
	for i, block := range f.Blocks {
		fr.blocks[i] = llvm.AddBasicBlock(llvmFunction, block.Comment)
	}
	for i, block := range f.Blocks {
		t.builder.SetInsertPointAtEnd(fr.blocks[i])
		if i == 0 {
			// Allocate stack space for locals in the entry block.
			for _, local := range f.Locals {
				typ := t.types.ToLLVM(deref(local.Type()))
				alloca := t.builder.CreateAlloca(typ, local.Name())
				t.memsetZero(alloca, llvm.SizeOf(typ))
				value := fr.NewValue(alloca, local.Type())
				fr.locals[local] = value
				fr.env[local] = value
			}
		}
		for _, instr := range block.Instrs {
			fr.instruction(instr)
		}
	}
}

type frame struct {
	*translator
	blocks []llvm.BasicBlock
	locals map[ssa.Value]*LLVMValue
	env    map[ssa.Value]*LLVMValue
}

func (fr *frame) block(b *ssa.BasicBlock) llvm.BasicBlock {
	return fr.blocks[b.Index]
}

func (fr *frame) value(v ssa.Value) *LLVMValue {
	switch v := v.(type) {
	case nil:
		return nil
	case *ssa.Function:
		return fr.functions[v]
	case *ssa.Const:
		return fr.NewConstValue(v.Value, v.Type())
	case *ssa.Global:
		if value, ok := fr.globals[v]; ok {
			return value
		}
	}
	if value, ok := fr.env[v]; ok {
		return value
	}
	if instr, ok := v.(ssa.Instruction); ok {
		fr.instruction(instr)
		if value, ok := fr.env[v]; ok {
			return value
		}
	}
	panic(fmt.Sprintf("no value for %T: %v", v, v.Name()))
}

func (fr *frame) instruction(instr ssa.Instruction) {
	fset := fr.pkg.Prog.Fset
	fmt.Printf("[%T] %v @ %s\n", instr, instr, fset.Position(instr.Pos()))

	switch instr := instr.(type) {
	case *ssa.Alloc:
		typ := fr.types.ToLLVM(deref(instr.Type()))
		var value llvm.Value
		if instr.Heap {
			value = fr.createTypeMalloc(typ)
			fr.env[instr] = fr.NewValue(value, instr.Type())
		} else {
			value = fr.locals[instr].LLVMValue()
		}
		fr.memsetZero(value, llvm.SizeOf(typ))

	case *ssa.BinOp:
		lhs, rhs := fr.value(instr.X), fr.value(instr.Y)
		result := lhs.BinaryOp(instr.Op, rhs).(*LLVMValue)
		fr.env[instr] = result

	case *ssa.Call:
		fn, args, result := fr.prepareCall(&instr.Call)
		if result != nil {
			fr.env[instr] = result
			return
		}
		if instr.Call.IsInvoke() {
			panic("TODO")
		}
		const hasdefers = false // TODO
		result = fr.createCall(fn, args, instr.Call.HasEllipsis, hasdefers)
		fr.env[instr] = result

	//case *ssa.Builtin:
	//case *ssa.ChangeInterface:

	case *ssa.ChangeType:
		// TODO refactor Convert
		fr.env[instr] = fr.value(instr.X).Convert(instr.Type()).(*LLVMValue)

	case *ssa.Convert:
		fr.env[instr] = fr.value(instr.X).Convert(instr.Type()).(*LLVMValue)

	//case *ssa.DebugRef:
	//case *ssa.Defer:

	case *ssa.Extract:
		tuple := fr.value(instr.Tuple).LLVMValue()
		elem := fr.builder.CreateExtractValue(tuple, instr.Index, instr.Name())
		elemtyp := instr.Type()
		fr.env[instr] = fr.NewValue(elem, elemtyp)

	case *ssa.Field:
		value := fr.value(instr.X).LLVMValue()
		field := fr.builder.CreateExtractValue(value, instr.Field, instr.Name())
		fieldtyp := instr.Type()
		fr.env[instr] = fr.NewValue(field, fieldtyp)

	case *ssa.FieldAddr:
		ptr := fr.value(instr.X).LLVMValue()
		fieldptr := fr.builder.CreateStructGEP(ptr, instr.Field, instr.Name())
		fieldptrtyp := instr.Type()
		fr.env[instr] = fr.NewValue(fieldptr, fieldptrtyp)

	//case *ssa.Go:

	case *ssa.If:
		cond := fr.value(instr.Cond).LLVMValue()
		block := instr.Block()
		trueBlock := fr.block(block.Succs[0])
		falseBlock := fr.block(block.Succs[1])
		fr.builder.CreateCondBr(cond, trueBlock, falseBlock)

	case *ssa.Index:
		ptr := fr.value(instr.X).pointer.LLVMValue()
		index := fr.value(instr.Index).LLVMValue()
		zero := llvm.ConstNull(index.Type())
		addr := fr.builder.CreateGEP(ptr, []llvm.Value{zero, index}, "")
		elemtyp := instr.X.Type().(*types.Array).Elem()
		fr.env[instr] = fr.NewValue(addr, types.NewPointer(elemtyp)).makePointee()

	case *ssa.IndexAddr:
		x := fr.value(instr.X).LLVMValue()
		index := fr.value(instr.Index).LLVMValue()
		var addr llvm.Value
		var elemtyp types.Type
		zero := llvm.ConstNull(index.Type())
		switch typ := instr.X.Type().(type) {
		case *types.Slice:
			elemtyp = typ.Elem()
			x = fr.builder.CreateExtractValue(x, 0, "")
			addr = fr.builder.CreateGEP(x, []llvm.Value{index}, "")
		case *types.Pointer:
			// *array
			elemtyp = deref(typ).(*types.Array).Elem()
			addr = fr.builder.CreateGEP(x, []llvm.Value{zero, index}, "")
		}
		fr.env[instr] = fr.NewValue(addr, types.NewPointer(elemtyp))

	case *ssa.Jump:
		succ := instr.Block().Succs[0]
		fr.builder.CreateBr(fr.block(succ))

	//case *ssa.Lookup:
	//case *ssa.MakeChan:
	//case *ssa.MakeClosure:

	case *ssa.MakeInterface:
		iface := instr.Type().Underlying().(*types.Interface)
		receiver := fr.value(instr.X)
		value := llvm.Undef(fr.types.ToLLVM(iface))
		rtype := fr.types.ToRuntime(receiver.Type())
		rtype = fr.builder.CreateBitCast(rtype, llvm.PointerType(llvm.Int8Type(), 0), "")
		value = fr.builder.CreateInsertValue(value, rtype, 0, "")
		value = fr.builder.CreateInsertValue(value, receiver.interfaceValue(), 1, "")
		// TODO methods
		fr.env[instr] = fr.NewValue(value, instr.Type())

	//case *ssa.MakeMap:
	//case *ssa.MakeSlice:
	//case *ssa.MapUpdate:
	//case *ssa.Next:

	case *ssa.Panic:
		// TODO

	case *ssa.Phi:
		typ := instr.Type()
		phi := fr.builder.CreatePHI(fr.types.ToLLVM(typ), instr.Name())
		fr.env[instr] = fr.NewValue(phi, typ)
		values := make([]llvm.Value, len(instr.Edges))
		blocks := make([]llvm.BasicBlock, len(instr.Edges))
		block := instr.Block()
		for i, edge := range instr.Edges {
			values[i] = fr.value(edge).LLVMValue()
			blocks[i] = fr.block(block.Preds[i])
		}
		phi.AddIncoming(values, blocks)

	//case *ssa.Range:

	case *ssa.Return:
		switch n := len(instr.Results); n {
		case 0:
			fr.builder.CreateRetVoid()
		case 1:
			fr.builder.CreateRet(fr.value(instr.Results[0]).LLVMValue())
		default:
			values := make([]llvm.Value, n)
			for i, result := range instr.Results {
				values[i] = fr.value(result).LLVMValue()
			}
			fr.builder.CreateAggregateRet(values)
		}

	//case *ssa.RunDefers:
	//case *ssa.Select:
	//case *ssa.Send:

	case *ssa.Slice:
		x := fr.value(instr.X)
		low := fr.value(instr.Low)
		high := fr.value(instr.High)
		fr.env[instr] = fr.slice(x, low, high)

	case *ssa.Store:
		addr := fr.value(instr.Addr)
		value := fr.value(instr.Val)
		fr.builder.CreateStore(value.LLVMValue(), addr.LLVMValue())

	case *ssa.TypeAssert:
		x := fr.value(instr.X)
		if !instr.CommaOk {
			fr.env[instr] = x.mustConvertI2V(instr.AssertedType)
		} else {
			result, ok := x.convertI2V(instr.AssertedType)
			resultval := result.LLVMValue()
			okval := ok.LLVMValue()
			pairtyp := llvm.StructType([]llvm.Type{resultval.Type(), okval.Type()}, false)
			pair := llvm.Undef(pairtyp)
			pair = fr.builder.CreateInsertValue(pair, resultval, 0, "")
			pair = fr.builder.CreateInsertValue(pair, okval, 1, "")
			fr.env[instr] = fr.NewValue(pair, instr.Type())
		}

	case *ssa.UnOp:
		result := fr.value(instr.X).UnaryOp(instr.Op).(*LLVMValue)
		fr.env[instr] = result

	default:
		panic("unhandled")
	}
}

// prepareCall returns the evaluated function and arguments.
//
// For builtins that may not be used in go/defer, prepareCall
// will emits inline code. In this case, prepareCall returns
// nil for fn and args, and returns a non-nil value for result.
func (fr *frame) prepareCall(call *ssa.CallCommon) (fn *LLVMValue, args []*LLVMValue, result *LLVMValue) {
	if call.IsInvoke() {
		panic("invoke mode unimplemented")
	}

	args = make([]*LLVMValue, len(call.Args))
	for i, arg := range call.Args {
		args[i] = fr.value(arg)
	}

	switch call.Value.(type) {
	case *ssa.Builtin:
		// handled below
	default:
		fn = fr.value(call.Value)
		return fn, args, nil
	}

	// Builtins may only be used in calls (i.e. can't be assigned),
	// and only print[ln], panic and recover may be used in go/defer.
	builtin := call.Value.(*ssa.Builtin)
	switch builtin.Name() {
	case "print", "println":
		// print/println generates a call-site specific anonymous
		// function to print the values. It's not inline because
		// print/println may be deferred.
		params := make([]*types.Var, len(call.Args))
		for i, arg := range call.Args {
			// make sure to use args[i].Type(), not call.Args[i].Type(),
			// as the evaluated expression converts untyped.
			params[i] = types.NewParam(arg.Pos(), nil, arg.Name(), args[i].Type())
		}
		sig := types.NewSignature(nil, nil, types.NewTuple(params...), nil, false)
		fntyp := fr.types.ToLLVM(sig).StructElementTypes()[0].ElementType()
		llvmfn := llvm.AddFunction(fr.module.Module, "", fntyp)
		currBlock := fr.builder.GetInsertBlock()
		entry := llvm.AddBasicBlock(llvmfn, "entry")
		fr.builder.SetInsertPointAtEnd(entry)
		internalArgs := make([]Value, len(args))
		for i, arg := range args {
			internalArgs[i] = fr.NewValue(llvmfn.Param(i), arg.Type())
		}
		fr.printValues(builtin.Name() == "println", internalArgs...)
		fr.builder.CreateRetVoid()
		fr.builder.SetInsertPointAtEnd(currBlock)
		fn = fr.NewValue(llvmfn, sig)
		return fn, args, nil

	case "panic":
		panic("TODO: panic")

	case "recover":
		panic("TODO: recover")

	case "cap":
		return nil, nil, fr.callCap(args[0])

	case "len":
		return nil, nil, fr.callLen(args[0])

	default:
		panic("unimplemented: " + builtin.Name())
	}
}
