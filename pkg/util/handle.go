package util

// Wrapper file for the AddHandler function

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	klog "k8s.io/klog/v2"
)

// AddHandler is a helper function to wrap the handle function with JSON [de]serialization and check
// that the HTTP method is correct
//
// The provided logPrefix is prepended to every log line emitted by the wrapped handler function, to
// offer distinction where that's useful.
func AddHandler[T any, R any](
	logPrefix string,
	mux *http.ServeMux,
	endpoint string,
	method string,
	reqTypeName string,
	handle func(*T) (_ *R, statusCode int, _ error),
) {
	errBadMethod := []byte("request method must be " + method)

	mux.HandleFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write(errBadMethod)
			return
		}

		defer r.Body.Close()
		var req T
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			klog.Errorf("%sError reading request body as JSON %s: %s", logPrefix, reqTypeName, err)
			w.WriteHeader(400)
			_, _ = w.Write([]byte("bad JSON"))
			return
		}

		klog.Infof("%sReceived request on %s (client = %s) %+v", logPrefix, endpoint, r.RemoteAddr, req)

		resp, status, err := handle(&req)

		if err == nil && status != http.StatusOK {
			err = errors.New("HTTP handler error: status != 200 OK, but no error message")
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
				err = fmt.Errorf(
					"%sHTTP handler error: invalid status %d for error response: %w",
					logPrefix, status, err,
				)
				logFunc = klog.Errorf
			}
			respBodyFormatted = err.Error()
			respBody = []byte(respBodyFormatted)
		} else {
			if status == 0 {
				klog.Warningf("%sHTTP handler: non-error response with status = 0", logPrefix)
			}

			respBody, err = json.Marshal(resp)
			if err != nil {
				klog.Errorf("%sError encoding JSON response: %s", logPrefix, err)
				w.WriteHeader(500)
				_, _ = w.Write([]byte("Error encoding JSON response"))
				return
			}
			respBodyFormatted = string(respBody)
			logFunc = klog.Infof
		}

		logFunc(
			"%sResponding to request on %s with status code %d: %s",
			logPrefix, endpoint, status, respBodyFormatted,
		)

		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	})
}
