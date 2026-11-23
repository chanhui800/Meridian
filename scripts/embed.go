package scripts

import _ "embed"

// AgentInstaller is the single canonical POSIX installer served by Meridian.
// Keep the standalone script and the embedded response in the same directory.
//go:embed agent-install.sh
var AgentInstaller string
