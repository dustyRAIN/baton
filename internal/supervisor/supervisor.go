// Package supervisor carries the shell script that runs inside the container.
//
// The script is embedded in the binary so `baton init` can install it without
// anyone having to know where baton was checked out.
package supervisor

import _ "embed"

// Script is the supervisor installed at <code-root>/.baton/supervisor.sh.
//
//go:embed supervisor.sh
var Script []byte
