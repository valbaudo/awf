//go:build darwin

package native

// detectPlatformSandbox returns the best available sandbox launcher for
// macOS. Task 4 fills this with sandbox-exec; for now it is a stub
// returning (nil, "") so the OS-agnostic detectSandbox falls back to the
// no-op + warn path.
func detectPlatformSandbox(_ func(string) (string, error)) (sandboxLauncher, string) {
	return nil, ""
}
