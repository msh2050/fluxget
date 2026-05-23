//go:build linux

package main

/*
#define _GNU_SOURCE
#include <signal.h>

// fixWebKitSignalHandlers reads each signal's current handler and adds
// SA_ONSTACK if it's missing. JSC installs SIGSEGV/SIGBUS/SIGILL handlers
// without SA_ONSTACK for its GC and JIT exception machinery. Go 1.25 added
// adjustSignalStack2, which panics when a signal fires on a stack not
// recognised as a Go stack and the handler was not registered with SA_ONSTACK.
// This function is called repeatedly from a goroutine to catch JSC's lazy
// handler installation (JSC registers its handlers when JS first executes,
// not at WebView init time).
static void fixWebKitSignalHandlers(void) {
	int sigs[] = {SIGSEGV, SIGBUS, SIGILL};
	for (int i = 0; i < 3; i++) {
		struct sigaction sa;
		if (sigaction(sigs[i], NULL, &sa) != 0) continue;
		if (sa.sa_flags & SA_ONSTACK) continue;
		sa.sa_flags |= SA_ONSTACK;
		sigaction(sigs[i], &sa, NULL);
	}
}
*/
import "C"

func fixWebKitSignalHandlers() {
	C.fixWebKitSignalHandlers()
}
