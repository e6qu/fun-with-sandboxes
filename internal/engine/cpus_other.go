//go:build !darwin

package engine

import goruntime "runtime"

// cpuTopology counts performance and efficiency cores, which only macOS
// tells apart here.
var cpuTopology = func() (perf, eff int) { return goruntime.NumCPU(), 0 }
