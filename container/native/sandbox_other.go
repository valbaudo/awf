//go:build !linux && !darwin

package native

// detectPlatformSandbox returns (nil, "") permanently on unsupported
// platforms. The OS-agnostic detectSandbox falls back to no-op + warn.
func detectPlatformSandbox(_ func(string) (string, error)) (sandboxLauncher, string) {
	return nil, ""
}
