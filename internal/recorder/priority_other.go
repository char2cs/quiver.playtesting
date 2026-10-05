//go:build !(linux || darwin || freebsd)

package recorder

func lowerPriority(int) {}
