//go:build linux

package main

/*
#include <signal.h>

// patchSAOnstack reads the current handler for signum and re-installs it with
// SA_ONSTACK added. Go 1.25 added adjustSignalStack2 which panics when a
// signal fires on a stack that is not a Go-managed stack and the handler was
// not registered with SA_ONSTACK. WebKit/JSC installs SIGSEGV/SIGBUS/SIGILL
// handlers without SA_ONSTACK; this function makes them compliant.
static void patchSAOnstack(int signum) {
    struct sigaction sa;
    if (sigaction(signum, NULL, &sa) != 0) return;
    if (sa.sa_flags & SA_ONSTACK) return;
    sa.sa_flags |= SA_ONSTACK;
    sigaction(signum, &sa, NULL);
}

static void fixWebKitSignalHandlers(void) {
    patchSAOnstack(SIGSEGV);
    patchSAOnstack(SIGBUS);
    patchSAOnstack(SIGILL);
}
*/
import "C"

func fixWebKitSignalHandlers() {
	C.fixWebKitSignalHandlers()
}
