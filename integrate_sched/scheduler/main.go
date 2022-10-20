package main

import (
	"fmt"
	"os"

	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	plugin "github.com/neondatabase/autoscaling/scheduler/src"
)

// all of the juicy bits are defined in ./src

func main() {
	command := app.NewSchedulerCommand(app.WithPlugin(plugin.Name, plugin.NewAutoscaleEnforcerPlugin))
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
