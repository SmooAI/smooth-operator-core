// Package e2e is the module root for the Go implementation of smooth-operator.
package e2e

// Version is the shared, lockstep release version for all smooth-operator
// language artifacts. The real Go "publish" is a git tag (go/v<Version>); this
// constant is the anchor that scripts/sync-versions.mjs keeps in sync with the
// canonical npm version on every changeset release.
const Version = "1.14.0"
