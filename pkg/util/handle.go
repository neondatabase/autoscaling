package util

// Wrapper file for the AddHandler function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"go.uber.org/zap"
)

// AddHandler is a helper function to wrap the handle function with JSON [de]serialization and check
// that the HTTP method is correct
//
// The provided logPrefix is prepended to every log line emitted by the wrapped handler function, to
// offer distinction where that's useful.
func AddHandler[T any, R any](
	logger *zap.Logger,
	mux *http.ServeMux,
	endpoint string,
	method string,
	reqTypeName string,
	handle func(context.Context, *zap.Logger, *T) (_ *R, statusCode int, _ error),
) {
	errBadMethod := []byte("request method must be " + method)

	logger = logger.With(zap.String("endpoint", endpoint))
	hlogger := logger.Named("http")

	mux.HandleFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write(errBadMethod)
			return
		}

		defer r.Body.Close()
		var req T
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hlogger.Error("Failed to read request body as JSON", zap.String("type", reqTypeName), zap.Error(err))
			w.WriteHeader(400)
			_, _ = w.Write([]byte("bad JSON"))
			return
		}

		hlogger.Info(
			"Received request",
			zap.String("endpoint", endpoint),
			zap.String("client", r.RemoteAddr),
			zap.Any("request", req),
		)

		resp, status, err := handle(r.Context(), logger.With(zap.Any("request", req)), &req)

		if err == nil && status != http.StatusOK {
			err = errors.New("HTTP handler error: status != 200 OK, but no error message")
			status = 500
		}

		var respBody []byte
		var respBodyFormatted zap.Field
		var logFunc func(string, ...zap.Field)

		if err != nil {
			if 500 <= status && status < 600 {
				logFunc = hlogger.Error
			} else if 400 <= status && status < 500 {
				logFunc = hlogger.Warn
			} else /* unexpected status */ {
				err = fmt.Errorf("HTTP handler error: invalid status %d for error response: %w", status, err)
				logFunc = hlogger.Error
			}
			respBodyFormatted = zap.NamedError("response", err)
			respBody = []byte(err.Error())
		} else {
			if status == 0 {
				hlogger.Warn("non-error response with status = 0")
			}

			respBodyFormatted = zap.Any("response", resp)

			respBody, err = json.Marshal(resp)
			if err != nil {
				hlogger.Error("Failed to encode JSON response", respBodyFormatted)
				w.WriteHeader(500)
				_, _ = w.Write([]byte("Error encoding JSON response"))
				return
			}
			logFunc = hlogger.Info
		}

		logFunc(
			"Responding to request",
			zap.String("endpoint", endpoint), zap.Int("status", status), respBodyFormatted,
		)

		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	})
}
