package main

import (
	"encoding/json"
	"fmt"
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
	if err == http.ErrServerClosed {
		klog.Infof("Server ended.")
	} else {
		klog.Errorf("Server failed: %s", err)
	}
}

// helper function to abbreviate bridging the HTTP <-> methods on informant.State
func addHandler[T any, R any](
	mux *http.ServeMux,
	endpoint string,
	method string,
	reqTypeName string,
	handle func(*T) (*R, int, error),
) {
	errBadMethod := []byte("request method must be " + method)

	mux.HandleFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write(errBadMethod)
			return
		}

		defer r.Body.Close()
		var req T
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			klog.Errorf("Error reading request body as JSON %s: %s", reqTypeName, err)
			w.WriteHeader(400)
			w.Write([]byte("bad JSON"))
			return
		}

		klog.Infof("Received request on %s (client = %s) %+v", endpoint, r.RemoteAddr, req)

		resp, status, err := handle(&req)

		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("HTTP handler error: status != 200 OK, but no error message")
			status = 500
		}

		var respBody []byte
		var respBodyFormatted string
		var logFunc func(string, ...any)

		if err != nil {
			if 500 <= status && status < 600 {
				logFunc = klog.Errorf
			} else if 400 <= status && status < 500 {
				logFunc = klog.Warningf
			} else /* unexpected status */ {
				err = fmt.Errorf("HTTP handler error: invalid status %d for error response: %s", status, err)
				logFunc = klog.Errorf
			}
			respBodyFormatted = err.Error()
			respBody = []byte(respBodyFormatted)
		} else {
			if status == 0 {
				klog.Warning("HTTP handler: non-error response with status = 0")
			}

			respBody, err = json.Marshal(resp)
			if err != nil {
				klog.Errorf("Error encoding JSON response: %s", err)
				w.WriteHeader(500)
				w.Write([]byte("Error encoding JSON response"))
				return
			}
			respBodyFormatted = string(respBody)
			logFunc = klog.Infof
		}

		logFunc("Responding to request on %s with status code %d: %s", endpoint, status, respBodyFormatted)

		w.WriteHeader(status)
		w.Write(respBody)
	})
}
