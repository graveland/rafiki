package main

// executorLostSteer is the steer a child gets when an executor it was using
// disconnects for good.
//
// It deliberately does NOT say "stop what you are doing": losing a machine
// never fails a child. Calls to that machine fail until it reconnects, and when
// it does the workspace there is rebuilt from the child's cwd — so anything the
// child never committed on that machine can be gone, which is what it has to be
// told before it reports work as done.
//
// name is DisplayName's: the machine label where the row is known, else a short
// id. It is the only variable part, so the text stays one reviewable sentence
// set rather than a format string assembled at each call site.
func executorLostSteer(name string) string {
	return "EXECUTOR LOST: executor " + name + " disconnected. Calls to it fail until it reconnects; when it does, your workspace there is rebuilt and anything not committed on that machine may be gone. Your other tools, and any other executors, still work."
}
