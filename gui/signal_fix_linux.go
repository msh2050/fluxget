//go:build linux

package main

/*
#cgo LDFLAGS: -ldl
#define _GNU_SOURCE
#include <signal.h>
#include <dlfcn.h>

// real_sa is initialised by the constructor below before main() starts.
static int (*real_sa)(int, const struct sigaction *, struct sigaction *);

__attribute__((constructor))
static void init_sigaction_shim(void) {
	real_sa = dlsym(RTLD_NEXT, "sigaction");
}

// sigaction overrides the libc function for the entire process.
//
// Main-binary global symbols have dynamic-linker priority over shared-library
// symbols, so every PLT call to sigaction() — including from WebKit/JSC shared
// libraries — resolves here instead of to libc.
//
// Go's runtime registers its own signal handlers via the rt_sigaction syscall
// directly (not through libc), so it is completely unaffected by this override.
// Go's CGo layer (x_cgo_sigaction) does call libc sigaction, but it already
// includes SA_ONSTACK in its flags, making our OR a no-op for those calls.
//
// WebKit/JavaScriptCore installs SIGSEGV/SIGBUS/SIGILL handlers for its GC and
// exception machinery without SA_ONSTACK. Go 1.25 added adjustSignalStack2,
// which panics when a signal fires on a stack not recognised as a Go stack and
// the handler was not registered with SA_ONSTACK. Adding SA_ONSTACK here makes
// signal delivery switch to the thread's alternate stack (which JSC sets up via
// sigaltstack), so sp lands in a known range and Go's check passes.
int sigaction(int signum, const struct sigaction *act, struct sigaction *oldact) {
	if (act) {
		struct sigaction p = *act;
		p.sa_flags |= SA_ONSTACK;
		return real_sa(signum, &p, oldact);
	}
	return real_sa(signum, act, oldact);
}

// fixWebKitSignalHandlers patches any handlers that were registered before our
// process-wide override became active (e.g. via early-init C++ constructors in
// shared libraries that ran before main-binary constructors, or via direct
// rt_sigaction syscalls from non-Go C++ code that bypasses libc).
static void fixWebKitSignalHandlers(void) {
	int sigs[] = {SIGSEGV, SIGBUS, SIGILL};
	for (int i = 0; i < 3; i++) {
		struct sigaction sa;
		if (real_sa(sigs[i], NULL, &sa) != 0) continue;
		if (sa.sa_flags & SA_ONSTACK) continue;
		sa.sa_flags |= SA_ONSTACK;
		real_sa(sigs[i], &sa, NULL);
	}
}
*/
import "C"

func fixWebKitSignalHandlers() {
	C.fixWebKitSignalHandlers()
}
