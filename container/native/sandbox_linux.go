//go:build linux

package native

// detectPlatformSandbox returns the best available sandbox launcher for the
// Linux platform. Task 3 fills this with the bwrap → landlock-trampoline
// chain; for now it is a stub returning (nil, "") so the OS-agnostic
// detectSandbox falls back to the no-op + warn path.
func detectPlatformSandbox(_ func(string) (string, error)) (sandboxLauncher, string) {
	return nil, ""
}
