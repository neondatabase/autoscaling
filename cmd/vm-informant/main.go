package main

import (
	"errors"
	"net/http"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/informant"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func main() {
	buildInfo := util.GetBuildInfo()
	klog.Infof("buildInfo.GitInfo:   %s", buildInfo.GitInfo)
	klog.Infof("buildInfo.GoVersion: %s", buildInfo.GoVersion)

	agents := informant.NewAgentSet()
	state := informant.NewState(agents)

	mux := http.NewServeMux()
	util.AddHandler("", mux, "/register", http.MethodPost, "AgentDesc", state.RegisterAgent)
	// FIXME: /downscale and /upscale should have the AgentID in the request body
	util.AddHandler("", mux, "/downscale", http.MethodPut, "RawResources", state.TryDownscale)
	util.AddHandler("", mux, "/upscale", http.MethodPut, "RawResources", state.NotifyUpscale)
	util.AddHandler("", mux, "/unregister", http.MethodDelete, "AgentDesc", state.UnregisterAgent)

	server := http.Server{Addr: "0.0.0.0:10301", Handler: mux}
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		klog.Infof("Server ended.")
	} else {
		klog.Fatalf("Server failed: %s", err)
	}
}
