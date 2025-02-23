package wasm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

// FailIfClosed returns a sys.ExitError if CloseWithExitCode was called.
func (m *ModuleInstance) FailIfClosed() (err error) {
	if closed := atomic.LoadUint64(&m.Closed); closed != 0 {
		return sys.NewExitError(uint32(closed >> 32)) // Unpack the high order bits as the exit code.
	}
	return nil
}

// CloseModuleOnCanceledOrTimeout take a context `ctx`, which might be a Cancel or Timeout context,
// and spawns the Goroutine to check the context is canceled ot deadline exceeded. If it reaches
// one of the conditions, it sets the appropriate exit code.
//
// Callers of this function must invoke the returned context.CancelFunc to release the spawned Goroutine.
func (m *ModuleInstance) CloseModuleOnCanceledOrTimeout(ctx context.Context) context.CancelFunc {
	// Creating an empty channel in this case is a bit more efficient than
	// creating a context.Context and canceling it with the same effect. We
	// really just need to be notified when to stop listening to the users
	// context. Closing the channel will unblock the select in the goroutine
	// causing it to return an stop listening to ctx.Done().
	cancelChan := make(chan struct{})
	go m.closeModuleOnCanceledOrTimeout(ctx, cancelChan)
	return func() { close(cancelChan) }
}

// closeModuleOnCanceledOrTimeout is extracted from CloseModuleOnCanceledOrTimeout for testing.
func (m *ModuleInstance) closeModuleOnCanceledOrTimeout(ctx context.Context, cancelChan <-chan struct{}) {
	select {
	case <-ctx.Done():
		select {
		case <-cancelChan:
			// In some cases by the time this goroutine is scheduled, the caller
			// has already closed both the context and the cancelChan. In this
			// case go will randomize which branch of the outer select to enter
			// and we don't want to close the module.
		default:
			m.CloseWithCtxErr(ctx)
		}
	case <-cancelChan:
	}
}

// CloseWithExitCode closes the module with an exit code based on the type of
// error reported by the context.
//
// If the context's error is unknown or nil, the module does not close.
func (m *ModuleInstance) CloseWithCtxErr(ctx context.Context) {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// TODO: figure out how to report error here.
		_ = m.CloseWithExitCode(ctx, sys.ExitCodeContextCanceled)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// TODO: figure out how to report error here.
		_ = m.CloseWithExitCode(ctx, sys.ExitCodeDeadlineExceeded)
	}
}

// Name implements the same method as documented on api.Module
func (m *ModuleInstance) Name() string {
	return m.ModuleName
}

// String implements the same method as documented on api.Module
func (m *ModuleInstance) String() string {
	return fmt.Sprintf("Module[%s]", m.Name())
}

// Close implements the same method as documented on api.Module.
func (m *ModuleInstance) Close(ctx context.Context) (err error) {
	return m.CloseWithExitCode(ctx, 0)
}

// CloseWithExitCode implements the same method as documented on api.Module.
func (m *ModuleInstance) CloseWithExitCode(ctx context.Context, exitCode uint32) (err error) {
	if !m.setExitCode(exitCode) {
		return nil // not an error to have already closed
	}
	_ = m.s.deleteModule(m.moduleListNode)
	return m.ensureResourcesClosed(ctx)
}

// closeWithExitCode is the same as CloseWithExitCode besides this doesn't delete it from Store.moduleList.
func (m *ModuleInstance) closeWithExitCode(ctx context.Context, exitCode uint32) (err error) {
	if !m.setExitCode(exitCode) {
		return nil // not an error to have already closed
	}
	return m.ensureResourcesClosed(ctx)
}

func (m *ModuleInstance) setExitCode(exitCode uint32) bool {
	closed := uint64(1) + uint64(exitCode)<<32 // Store exitCode as high-order bits.
	return atomic.CompareAndSwapUint64(&m.Closed, 0, closed)
}

// ensureResourcesClosed ensures that resources assigned to CallContext is released.
// Multiple calls to this function is safe.
func (m *ModuleInstance) ensureResourcesClosed(ctx context.Context) (err error) {
	if sysCtx := m.Sys; sysCtx != nil { // nil if from HostModuleBuilder
		if err = sysCtx.FS().Close(ctx); err != nil {
			return err
		}
		m.Sys = nil
	}

	if m.CodeCloser == nil {
		return
	}
	if e := m.CodeCloser.Close(ctx); e != nil && err == nil {
		err = e
	}
	m.CodeCloser = nil
	return
}

// Memory implements the same method as documented on api.Module.
func (m *ModuleInstance) Memory() api.Memory {
	return m.MemoryInstance
}

// ExportedMemory implements the same method as documented on api.Module.
func (m *ModuleInstance) ExportedMemory(name string) api.Memory {
	_, err := m.getExport(name, ExternTypeMemory)
	if err != nil {
		return nil
	}
	// We Assume that we have at most one memory.
	return m.MemoryInstance
}

// ExportedMemoryDefinitions implements the same method as documented on
// api.Module.
func (m *ModuleInstance) ExportedMemoryDefinitions() map[string]api.MemoryDefinition {
	// Special case as we currently only support one memory.
	if mem := m.MemoryInstance; mem != nil {
		// Now, find out if it is exported
		for name, exp := range m.Exports {
			if exp.Type == ExternTypeMemory {
				return map[string]api.MemoryDefinition{name: mem.definition}
			}
		}
	}
	return map[string]api.MemoryDefinition{}
}

// ExportedFunction implements the same method as documented on api.Module.
func (m *ModuleInstance) ExportedFunction(name string) api.Function {
	exp, err := m.getExport(name, ExternTypeFunc)
	if err != nil {
		return nil
	}

	return m.function(exp.Index)
}

// ExportedFunctionDefinitions implements the same method as documented on
// api.Module.
func (m *ModuleInstance) ExportedFunctionDefinitions() map[string]api.FunctionDefinition {
	result := map[string]api.FunctionDefinition{}
	for name, exp := range m.Exports {
		if exp.Type == ExternTypeFunc {
			result[name] = m.Functions[exp.Index].Definition
		}
	}
	return result
}

func (m *ModuleInstance) Function(funcIdx Index) api.Function {
	if uint32(len(m.Functions)) < funcIdx {
		return nil
	}
	return m.function(funcIdx)
}

func (m *ModuleInstance) function(index Index) api.Function {
	ce, err := m.Engine.NewCallEngine(index)
	if err != nil {
		return nil
	}
	return &function{index: index, ce: ce, m: m}
}

// function implements api.Function. This couples FunctionInstance with CallEngine so that
// it can be used to make function calls originating from the FunctionInstance.
type function struct {
	index Index
	m     *ModuleInstance
	ce    CallEngine
}

// Definition implements the same method as documented on api.FunctionDefinition.
func (f *function) Definition() api.FunctionDefinition {
	return &f.m.definitions[f.index]
}

// Call implements the same method as documented on api.Function.
func (f *function) Call(ctx context.Context, params ...uint64) (ret []uint64, err error) {
	return f.ce.Call(ctx, f.m, params)
}

// GlobalVal is an internal hack to get the lower 64 bits of a global.
func (m *ModuleInstance) GlobalVal(idx Index) uint64 {
	return m.Globals[idx].Val
}

// ExportedGlobal implements the same method as documented on api.Module.
func (m *ModuleInstance) ExportedGlobal(name string) api.Global {
	exp, err := m.getExport(name, ExternTypeGlobal)
	if err != nil {
		return nil
	}
	g := m.Globals[exp.Index]
	if g.Type.Mutable {
		return &mutableGlobal{g}
	}
	valType := g.Type.ValType
	switch valType {
	case ValueTypeI32:
		return globalI32(g.Val)
	case ValueTypeI64:
		return globalI64(g.Val)
	case ValueTypeF32:
		return globalF32(g.Val)
	case ValueTypeF64:
		return globalF64(g.Val)
	default:
		panic(fmt.Errorf("BUG: unknown value type %X", valType))
	}
}
