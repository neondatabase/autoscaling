package billing

import (
	"fmt"
	"math/rand"
	"os"
)

var hostname string

func init() {
	var err error
	hostname, err = os.Hostname()
	if err != nil {
		hostname = fmt.Sprintf("unknown-%d", rand.Intn(1000))
	}
}

// GetHostname returns the hostname to be used for enriching billing events (see Enrich())
//
// This function MUST NOT be run before init has finished.
func GetHostname() string {
	return hostname
}
