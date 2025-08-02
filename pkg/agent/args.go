package agent

import (
	"fmt"
	"os"
)

// EnvArgs stores the static configuration data assigned to the autoscaler agent by its
// environment
type EnvArgs struct {
	// ConfigPath gives the path to read static configuration from. It is taken from the CONFIG_PATH
	// environment variable.
	ConfigPath string

	// K8sNodeName is the Kubernetes node the autoscaler agent is running on. It is taken from the
	// K8S_NODE_NAME environment variable, which is set equal to the pod's Spec.NodeName.
	//
	// The Kubernetes documentation doesn't say this, but the NodeName is always populated with the
	// final node the pod was placed on by the time the environment variables are set.
	K8sNodeName string

	// K8sPodIP is the IP address of the Kubernetes pod that this autoscaler-agent is running in
	K8sPodIP string
}

func getEnvVar(err *error, require_nonempty bool, varName string) string {
	if *err != nil {
		return ""
	}

	s := os.Getenv(varName)
	if s == "" && require_nonempty {
		*err = fmt.Errorf("missing %s in environment", varName)
	}

	return s
}

func ArgsFromEnv() (EnvArgs, error) {
	var err error

	args := EnvArgs{
		ConfigPath:  getEnvVar(&err, true, "CONFIG_PATH"),
		K8sNodeName: getEnvVar(&err, true, "K8S_NODE_NAME"),
		K8sPodIP:    getEnvVar(&err, true, "K8S_POD_IP"),
	}

	if err != nil {
		return EnvArgs{}, err
	} else {
		return args, err
	}
}
