package main

// Currently our plugin doesn't do anything. We have a plugin implemented that can be referred to as
// NeonTest (I think), but running any code requires implementing one of the framework.*Plugin
// interfaces, as shown in the README

import (
	"fmt"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"os"
)

func main() {
	command := app.NewSchedulerCommand(app.WithPlugin("neon-test-sched-plugin", NewNeonTestPlugin))
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

type NeonTestPlugin struct{}

// NewNeonTestPlugin produces the initial NewNeonTestPlugin to be used by the scheduler
func NewNeonTestPlugin(obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &NeonTestPlugin{}, nil
}

// required for framework.Plugin
func (p *NeonTestPlugin) Name() string {
	return "NeonTest"
}
