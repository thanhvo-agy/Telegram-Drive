//go:build !linux

package qos

// SetProcessNicePriority is a no-op on non-Linux platforms
func SetProcessNicePriority(priority int) error {
	return nil
}
