// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"fmt"
	"os"
)

// Version is set by the linker flags in the Makefile.
var Version string

func PrintVersionAndExit() {
	fmt.Printf("%s\n", Version)
	os.Exit(0)
}
