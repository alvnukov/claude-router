//go:build unix

package platform

// renameBusy reports a rename refused because another handle has the target
// open. Unix renames over open files, so it never is.
func renameBusy(error, string) bool { return false }
