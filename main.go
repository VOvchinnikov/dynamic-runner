package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"unsafe"

	"github.com/pkujhd/goloader"
)

func main() {
	// this is here to link the runtime, without which everything will fail to link
	_ = runtime.Version()

	if len(os.Args) < 2 {
		fmt.Println(errors.New("no path to the objects dir was provided"))
		return
	}

	implementationWrapper, err := NewImplementationWrapper(os.Args[1])
	if err != nil {
		fmt.Printf("failed to load implementation: %s\n", err)
		return
	}

	stuff := "default test stuff"
	if len(os.Args) > 2 {
		stuff = os.Args[2]
	}

	processedStuff, err := implementationWrapper.ProcessStuff(stuff)
	if err != nil {
		fmt.Printf("failed to process stuff: %s\n", err)
	}

	fmt.Println(processedStuff)
}

type Interface interface {
	ProcessStuff(string) (string, error)
}

type ImplementationWrapper struct {
	module         *goloader.CodeModule
	implementation Interface
}

func NewImplementationWrapper(dir string) (*ImplementationWrapper, error) {
	archivesPattern := filepath.Join(dir, "/*.a")
	globbedArchives, err := filepath.Glob(archivesPattern)
	if err != nil {
		return nil, fmt.Errorf("failed to glob archive files: %w", err)
	}

	objectsPattern := filepath.Join(dir, "/*.o")
	globbedObjects, err := filepath.Glob(objectsPattern)
	if err != nil {
		return nil, fmt.Errorf("failed to glob object files: %w", err)
	}

	allObjects := append(globbedArchives, globbedObjects...)

	err = checkDependencies(allObjects)
	if err != nil {
		return nil, fmt.Errorf("failed to check dependencies: %w", err)
	}

	symPtr := map[string]uintptr{}
	if err = goloader.RegSymbol(symPtr); err != nil {
		return nil, fmt.Errorf("go object loader could not register symbol table: %w", err)
	}

	linker, err := goloader.ReadObjs(allObjects, make([]string, len(allObjects)))
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

	result := &ImplementationWrapper{
		module:         module,
		implementation: ctor(),
	}

	return result, nil
}

func (iw *ImplementationWrapper) ProcessStuff(stuff string) (string, error) {
	// implement the Interface to use the wrapper where we want it
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

func checkDependencies(objects []string) error {
	// read own build info
	buildInfo, err := getBuildInfo()
	if err != nil {
		return err
	}

	// we read the files twice since we couldn't find a way to easily access the modinfo
	// as it is mapped to runtime.modinfo which is resolved by the linker
	var implementationBuildInfo *debug.BuildInfo
	for _, obj := range objects {
		if implementationBuildInfo = getModInfoFromFile(obj); implementationBuildInfo != nil {
			break
		}
	}

	if implementationBuildInfo != nil {
		// We need to make sure that what we load has the same deps as we are running with.
		// If a dependency is missing the loader will already fail during dynamic linking,
		// so what is left to us is to check the versions.
		// The question is how strict we want to be? So for now we don't return an error
		// from validateDependencies but only log some warnings
		if err = validateDependencies(buildInfo, implementationBuildInfo); err != nil {
			return err
		}
	}

	return nil
}

func getBuildInfo() (*debug.BuildInfo, error) {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, fmt.Errorf("failed to read debug build info")
	}
	return buildInfo, nil
}

func getModInfoFromFile(path string) *debug.BuildInfo {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil
	}
	// see src/cmd/go/internal/modload/build.go
	infoStart, _ := hex.DecodeString("3077af0c9274080241e1c107e6d618e6")
	infoEnd, _ := hex.DecodeString("f932433186182072008242104116d8f2")
	start := bytes.Index(data, infoStart)
	end := bytes.Index(data, infoEnd)
	if start <= 0 || end <= 0 && end <= start {
		return nil
	}
	bi, ok := readBuildInfo(string(data[start:end]))
	if !ok {
		return nil
	}
	return bi
}

// keep in sync with src/runtime/debug/mod.go:readBuildInfo
func readBuildInfo(data string) (*debug.BuildInfo, bool) {
	if len(data) < 32 {
		return nil, false
	}
	data = data[16 : len(data)-16]

	const (
		pathLine = "path\t"
		modLine  = "mod\t"
		depLine  = "dep\t"
		repLine  = "=>\t"
	)

	readEntryFirstLine := func(elem []string) (debug.Module, bool) {
		if len(elem) != 2 && len(elem) != 3 {
			return debug.Module{}, false
		}
		sum := ""
		if len(elem) == 3 {
			sum = elem[2]
		}
		return debug.Module{
			Path:    elem[0],
			Version: elem[1],
			Sum:     sum,
		}, true
	}

	var (
		info = &debug.BuildInfo{}
		last *debug.Module
		line string
		ok   bool
	)
	// Reverse of cmd/go/internal/modload.PackageBuildInfo
	for len(data) > 0 {
		i := strings.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line, data = data[:i], data[i+1:]
		switch {
		case strings.HasPrefix(line, pathLine):
			elem := line[len(pathLine):]
			info.Path = elem
		case strings.HasPrefix(line, modLine):
			elem := strings.Split(line[len(modLine):], "\t")
			last = &info.Main
			*last, ok = readEntryFirstLine(elem)
			if !ok {
				return nil, false
			}
		case strings.HasPrefix(line, depLine):
			elem := strings.Split(line[len(depLine):], "\t")
			last = new(debug.Module)
			info.Deps = append(info.Deps, last)
			*last, ok = readEntryFirstLine(elem)
			if !ok {
				return nil, false
			}
		case strings.HasPrefix(line, repLine):
			elem := strings.Split(line[len(repLine):], "\t")
			if len(elem) != 3 {
				return nil, false
			}
			if last == nil {
				return nil, false
			}
			last.Replace = &debug.Module{
				Path:    elem[0],
				Version: elem[1],
				Sum:     elem[2],
			}
			last = nil
		}
	}
	return info, true
}

func validateDependencies(buildInfo, needed *debug.BuildInfo) error {
	having := make(map[string]*debug.Module)

	for _, dep := range buildInfo.Deps {
		having[dep.Path] = dep
		// is a single level enough?
		if dep.Replace != nil {
			having[dep.Path] = dep.Replace
		}
	}

	for _, dep := range needed.Deps {
		have, found := having[dep.Path]
		if !found {
			// we should never reach this due to the dynamic linking
			return fmt.Errorf("missing dependency: %s", dep.Path)
		}
		if have.Version != dep.Version {
			// for now, we just print a warning
			fmt.Printf(
				"version mismatch while validating dynamically loaded obj %s@%s: dep %s mismatch have=%s want=%s\n",
				needed.Main.Path, needed.Main.Version,
				dep.Path, have.Version, dep.Version,
			)
		}
	}
	return nil
}
