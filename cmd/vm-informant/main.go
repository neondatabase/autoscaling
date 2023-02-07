package main

import (
	"errors"
	"flag"
	"net/http"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/informant"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func main() {
	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

	// Realistically, we want to be able to distinguish between --cgroup="" and absence of the flag,
	// if only to be able to provide better errors on invalid cgroup names.
	//
	// Because cgroup names *could* be any valid path fragment, we include a null byte in the string
	// to distinguish between --cgroup=... and its absence, because null bytes aren't valid in paths.
	noCgroup := "invalid\x00CgroupName"
	var cgroupName string
	flag.StringVar(&cgroupName, "cgroup", noCgroup, "Sets the cgroup to monitor (optional)")

	flag.Parse()

	var stateOpts []informant.NewStateOpts

	if cgroupName != noCgroup {
		cgroupConfig := informant.DefaultCgroupConfig
		klog.Infof("Selected cgroup %q, starting handler with config %+v", cgroupName, cgroupConfig)
		cgroup, err := informant.NewCgroupManager(cgroupName)
		if err != nil {
			klog.Fatalf("Error starting cgroup handler for cgroup name %q: %s", cgroupName, err)
		}

		stateOpts = append(stateOpts, informant.WithCgroup(cgroup, cgroupConfig))
	} else {
		klog.Infof("No cgroup selected")
	}

	agents := informant.NewAgentSet()
	state, err := informant.NewState(agents, stateOpts...)
	if err != nil {
		klog.Fatalf("Error starting informant.NewState: %s", err)
	}

	mux := http.NewServeMux()
	util.AddHandler("", mux, "/register", http.MethodPost, "AgentDesc", state.RegisterAgent)
	// FIXME: /downscale and /upscale should have the AgentID in the request body
	util.AddHandler("", mux, "/downscale", http.MethodPut, "RawResources", state.TryDownscale)
	util.AddHandler("", mux, "/upscale", http.MethodPut, "RawResources", state.NotifyUpscale)
	util.AddHandler("", mux, "/unregister", http.MethodDelete, "AgentDesc", state.UnregisterAgent)

	server := http.Server{Addr: "0.0.0.0:10301", Handler: mux}
	klog.Infof("Starting server at %s", server.Addr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		klog.Infof("Server ended.")
	} else {
		klog.Fatalf("Server failed: %s", err)
	}
}
