// Package latticeconfig provides environment-driven configuration for the
// Lattice binaries, so peer endpoints and listen addresses can be overridden
// at deploy time without editing source.
package latticeconfig

import "os"

// Env returns the value of the environment variable key, or fallback when the
// variable is unset or empty.
func Env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
