package hisepress

import (
	"context"
	"sync"
)

// Func is a piece of code a suite step can call by name:
//
//	{"code": {"name": "MyFunc", "args": {"id": "<var name=id />"}}}
//
// The returned value is converted to generic JSON, so assignments can pull
// values out of it with jsonpath. A function can attach files to the run with
// step.AddArtifact.
type Func func(ctx context.Context, step *Step, args map[string]string) (any, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Func{}
)

// Register makes fn available to every suite under name. Registering a name
// twice replaces the earlier function.
func Register(name string, fn Func) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = fn
}

func lookupFunc(name string, local map[string]Func) (Func, bool) {
	if fn, ok := local[name]; ok {
		return fn, true
	}
	registryMu.RLock()
	defer registryMu.RUnlock()
	fn, ok := registry[name]
	return fn, ok
}
