package helpers

// CountCalls returns how many of the recorded calls equal want. It pairs with
// RecordingClient.Calls.
func CountCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}
