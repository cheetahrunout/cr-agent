//go:build debug

package trust

// debugTrust marks a build that is never signed for release: it also trusts the throwaway test key.
const debugTrust = true
