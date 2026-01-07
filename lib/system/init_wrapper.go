package system

import _ "embed"

//go:embed init/init.sh
var InitWrapper []byte
