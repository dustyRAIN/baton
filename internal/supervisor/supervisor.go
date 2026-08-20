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

// Skill is the Claude Code skill that teaches a session how to use baton. It is
// embedded so `baton install-skill` works from an installed binary, without
// anyone having to know where the repository was checked out.
//
//go:embed skill.md
var Skill []byte
