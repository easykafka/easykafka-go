package helpers

// RecoverPanic runs call and returns the value it panicked with, or nil if it
// did not panic.
func RecoverPanic(call func()) (recovered any) {
	defer func() { recovered = recover() }()
	call()
	return nil
}
