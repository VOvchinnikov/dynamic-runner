package main

import (
	"errors"
	"fmt"
	"github.com/remerge/goloader"
	"os"
	"path/filepath"
	"unsafe"
)

type Interface interface {
	ProcessStuff(string) (string, error)
}

type ImplementationWrapper struct {
	module         *goloader.CodeModule
	implementation Interface
}

func NewImplementationWrapper(dir string) (*ImplementationWrapper, error) {
	// the method can also be expanded to load statically linked implementations as well
	pathPattern := filepath.Join(dir, "/*.o")

	objects, err := filepath.Glob(pathPattern)
	if err != nil {
		return nil, fmt.Errorf("failed to find object files: %w", err)
	}

	symPtr := map[string]uintptr{}
	if err = goloader.RegSymbol(symPtr); err != nil {
		return nil, fmt.Errorf("go object loader could not register symbol table: %w", err)
	}

	linker, err := goloader.ReadObjs(objects, make([]string, len(objects)))
	if err != nil {
		return nil, fmt.Errorf("failed to read objects: %w", err)
	}

	module, err := goloader.Load(linker, symPtr)
	if err != nil {
		return nil, fmt.Errorf("failed to dynamically link objects: %w", err)
	}

	// we expect the constructor function to be present in the main package
	ctorPtr, err := getFncPtr(module, "main.NewImplementation")
	if err != nil {
		return nil, fmt.Errorf("failed to get implementation constructor fn: %w", err)
	}

	// unsafe-land!
	// this case relies on the signature of the constructor to be the same
	// for all the implementations
	ctor := *(*func() Interface)(ctorPtr)

	return &ImplementationWrapper{
		module:         module,
		implementation: ctor(),
	}, nil
}

func (iw *ImplementationWrapper) ProcessStuff(stuff string) (string, error) {
	if iw.module == nil {
		return "", errors.New("no implementation loaded")
	}
	return iw.implementation.ProcessStuff(stuff)
}

func (iw *ImplementationWrapper) UnloadImplementation() {
	if iw.module != nil {
		iw.implementation = nil
		iw.module.Unload()
		iw.module = nil
	}
}

func getFncPtr(module *goloader.CodeModule, fncName string) (unsafe.Pointer, error) {
	fPtr, found := module.Syms[fncName]
	if !found || fPtr == 0 {
		return nil, fmt.Errorf("symbol not found: %s", fncName)
	}
	ptrTofPtr := (uintptr)(unsafe.Pointer(&fPtr))
	return unsafe.Pointer(&ptrTofPtr), nil
}

func main() {
	if len(os.Args) < 2 {
		panic(errors.New("no path to the objects dir was provided"))
	}

	implementationWrapper, err := NewImplementationWrapper(os.Args[1])
	if err != nil {
		panic(err)
	}

	stuff := "default test stuff"
	if len(os.Args) > 2 {
		stuff = os.Args[2]
	}

	processedStuff, err := implementationWrapper.ProcessStuff(stuff)
	if err != nil {
		panic(err)
	}

	fmt.Println(processedStuff)
}
