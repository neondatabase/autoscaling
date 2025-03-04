package billing

import (
	"fmt"
	"os"
)

var hostname string

func init() {
	var err error
	hostname, err = os.Hostname()
	if err != nil {
		panic(fmt.Errorf("failed to get hostname: %w", err))
	}
}

// GetHostname returns the hostname to be used for enriching billing events (see Enrich())
//
// This function MUST NOT be run before init has finished.
func GetHostname() string {
	return hostname
}
