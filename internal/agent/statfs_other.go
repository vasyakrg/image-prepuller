//go:build !unix

package agent

// osStatFS is a no-op on platforms without statfs; the agent targets Linux
// nodes, this only keeps cross-compilation of the package honest.
func osStatFS(string) (capacity, available int64) { return 0, 0 }
