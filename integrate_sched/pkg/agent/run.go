package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type Runner struct {
	podName                 api.PodName
	schedulerIP             net.IP
	ipv6Address             string
	cloudHypervisorSockPath string
	metricsURL              *url.URL
	minVCPU                 uint16
	maxVCPU                 uint16
	currentVCPU             uint16
	servePort               uint16
	readinessPort           uint16
	politeExitPort          uint16
}

func NewRunner(args Args, schedulerIP string, cloudHypervisorSockPath string) (*Runner, error) {
	parsedSchedulerIP := net.ParseIP(schedulerIP)
	if parsedSchedulerIP == nil {
		return nil, fmt.Errorf("Invalid scheduler IP %q", schedulerIP)
	}

	if args.InitVCPU == 0 {
		panic("expected init vCPU >= 1")
	}

	return &Runner{
		podName:                 api.PodName{Name: args.K8sPodName, Namespace: args.K8sPodNamespace},
		schedulerIP:             parsedSchedulerIP,
		ipv6Address: args.IPv6Address,
		cloudHypervisorSockPath: cloudHypervisorSockPath,
		metricsURL:              args.MetricsURL,
		minVCPU:                 args.MinVCPU,
		maxVCPU:                 args.MaxVCPU,
		currentVCPU:             args.InitVCPU,
		readinessPort:           args.ReadinessPort,
		politeExitPort:          args.PoliteExitPort,
	}, nil
}

func (r *Runner) MainLoop(config *Config, ctx context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	if r.politeExitPort != 0 {
		go r.cancelOnPoliteExit(cancel)
	} else {
		klog.V(1).Info("Skipping polite exit server because port is set to zero")
		defer cancel() // make sure cancel() still gets called
	}

	var gotMetricsYet atomic.Bool
	if r.readinessPort != 0 {
		go r.signalReadiness(&gotMetricsYet)
	} else {
		klog.V(1).Info("Skipping readiness server because port is set to zero")
	}

	metrics := make(chan api.Metrics)
	go r.getMetricsLoop(config, metrics)

	klog.Infof(
		"Starting main loop. min vCPU = %d, init vCPU = %d, max vCPU = %d",
		r.minVCPU, r.currentVCPU, r.maxVCPU,
	)

	for {
		select {
		// Simply exit if we're done
		case <-ctx.Done():
			return nil
		case m := <-metrics:
			newVCPUCount := r.newGoalCPUCount(m)
			changedVCPU := newVCPUCount != r.currentVCPU

			klog.Infof("New goal vCPU = %d", newVCPUCount)

			// If the goal is less than the current amount, decrease right away.
			if newVCPUCount < r.currentVCPU {
				klog.Infof(
					"Goal vCPU %d < Current vCPU %d, decrease immediately",
					newVCPUCount, r.currentVCPU,
				)
				if err := r.setCpus(ctx, newVCPUCount); err != nil {
					return fmt.Errorf("Error while setting CPU count: %s", err)
				}
				r.currentVCPU = newVCPUCount
			}

			// Request a new amount of CPU
			req := api.AgentRequest{
				Pod: r.podName,
				Resources: api.Resources{VCPU: newVCPUCount},
				Metrics: m,
			}
			resp, err := r.sendRequestToPlugin(config, &req)
			if err != nil {
				klog.Errorf("Error from resource request: %s", err)
				goto badScheduler // assume something's permanently wrong with the scheduler
			} else if resp.Permit.VCPU > req.Resources.VCPU {
				klog.Errorf("Plugin returned permit with vCPU greater than requested")
				goto badScheduler // assume something's permanently wrong with the scheduler
			}

			if !changedVCPU {
				continue;
			}

			if resp.Permit.VCPU != r.currentVCPU {
				if resp.Permit.VCPU < r.currentVCPU {
					// We shoudln't reach this, because req.VCPUs < r.currentVCPU is already handled
					// above, after which r.currentVCPU == req.VCPUs, so that would mean that the
					// permit caused a decrease beyond what we're expecting.
					klog.Errorf("Scheduler gave bad permit less than current vCPU. Discontinuing further contact after updating vCPUs")
				}

				maybeLessThanDesired := ""
				if resp.Permit.VCPU < newVCPUCount {
					maybeLessThanDesired = " (less than desired)"
				}
				klog.Infof("Setting vCPU = %d%s", resp.Permit.VCPU, maybeLessThanDesired)

				if err := r.setCpus(ctx, resp.Permit.VCPU); err != nil {
					return fmt.Errorf("Error while setting vCPU count: %s", err)
				}

				r.currentVCPU = resp.Permit.VCPU

			} else /* permit.VCPUs == r.currentVCPU */ {
				if r.currentVCPU == newVCPUCount {
					// We actually already set the CPU for this (see above), the returned permit
					// just confirmed that
					klog.Infof("Scheduler confirmed decrease to %d vCPU", newVCPUCount)
				} else {
					// We wanted to increase vCPUs, but the scheduler didn't allow it.
					klog.Infof("Scheduler denied increase to %d vCPU, staying at %d", newVCPUCount, r.currentVCPU)
				}
			}
		}
	}

	// Error-state handling: something went wrong, so we won't allow any more CPU changes. Our logic
	// up to now means our current state is fine (even if it's not the minimum)
badScheduler:
	// We know the scheduler (if it is somehow behaving correctly) won't over-commit resources into
	// our current vCPU count.
	maxFutureVCPU := r.currentVCPU

	klog.Warning("Ignoring all future requests from scheduler because of bad request")
	klog.Infof("Future vCPU limit set at current = %d", maxFutureVCPU)

	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-metrics:
			// We aren't allowed to communicate with the scheduling plugin, so we're upper-bounded
			// by maxFutureVCPU, which was determined by our state at the time the scheduler failed.
			newCpuCount := r.newGoalCPUCount(m)
			// Bound by our artificial maximum
			if newCpuCount > maxFutureVCPU {
				klog.Infof(
					"Want to scale to %d vCPUs, but capped at %d vCPUs because scheduler failed",
					newCpuCount, maxFutureVCPU,
				)
				newCpuCount = maxFutureVCPU
			}
			// Because we're not communicating with the scheduler, we can change the CPU
			// immediately. We know this value is ok because it's bounded by maxFutureVCPU.
			if newCpuCount != r.currentVCPU {
				klog.Infof("Setting vCPU = %d", newCpuCount)
				if err := r.setCpus(ctx, newCpuCount); err != nil {
					return fmt.Errorf("Error while setting CPU count: %s", err)
				}
				r.currentVCPU = newCpuCount
			}
		}
	}
}

// Calculates a new target CPU count, bounded ONLY by the minimum and maximum vCPUs
//
// It is the caller's responsibility to make sure that they don't over-commit beyond what the
// scheduler plugin has permitted.
func (r *Runner) newGoalCPUCount(metrics api.Metrics) uint16 {
	goal := r.currentVCPU
	if metrics.LoadAverage1Min > 0.9*float32(r.currentVCPU) {
		goal *= 2
	} else if metrics.LoadAverage1Min < 0.4*float32(r.currentVCPU) {
		goal /= 2
	}

	if goal < r.minVCPU {
		goal = r.minVCPU
	} else if goal > r.maxVCPU {
		goal = r.maxVCPU
	}

	return goal
}

// Helper function to abbreviate http server creation
func listenAndServe(addr, routePattern string, handler func(http.ResponseWriter, *http.Request)) error {
	mux := http.NewServeMux()
	mux.HandleFunc(routePattern, handler)
	server := http.Server{Addr: addr, Handler: mux}
	return server.ListenAndServe()
}

func (r *Runner) cancelOnPoliteExit(cancelFunc func()) {
	klog.Fatalf("Polite exit server failed: %s", listenAndServe(
		fmt.Sprintf("0.0.0.0:%d", r.politeExitPort), "/",
		func(w http.ResponseWriter, r *http.Request) {
			klog.Info("Received polite exit request")
			w.WriteHeader(200)
			w.Write([]byte("ok"))
			cancelFunc() // TODO: will this cause an exit before the response is written?
		},
	))
}

func (r *Runner) signalReadiness(gotMetrics *atomic.Bool) {
	klog.Infof("Starting readiness server (port %d)", r.readinessPort)

	klog.Fatalf("Readiness server failed: %s", listenAndServe(
		fmt.Sprintf("0.0.0.0:%d", r.readinessPort), "/healthz",
		func(w http.ResponseWriter, r *http.Request) {
			if gotMetrics.Load() {
				w.WriteHeader(200)
				w.Write([]byte("ok"))
			} else {
				w.WriteHeader(503)
				w.Write([]byte(fmt.Sprintf("error: haven't received metrics yet")))
			}
		},
	))
}

func (r *Runner) getMetricsLoop(config *Config, metrics chan<- api.Metrics) {
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := fmt.Errorf("Unexpected redirect while getting metrics")
			klog.Warning(err)
			return err
		},
		Timeout: time.Second * time.Duration(config.Metrics.RequestTimeoutSeconds),
	}

	req := http.Request{
		Method: "GET",
		URL:    r.metricsURL,
		// Allow reusing the TCP connection across multiple requests. Already false by default, but
		// worth setting explicitly.
		Close: false,
	}

	klog.Infof("Starting metrics loop. Timeout = %f seconds", client.Timeout.Seconds())

	// Helper to track whether we've gotten any responses yet. If we have, failed requests are
	// treated more seriously
	gotAtLeastOne := false

	time.Sleep(time.Second * time.Duration(config.Metrics.InitialDelaySeconds))

	for {
		// Wrap the loop body in a function so that we can defer inside it
		func() {
			resp, err := client.Do(&req)
			if err != nil {
				err = fmt.Errorf("Error getting metrics: %s", err)
				if !gotAtLeastOne {
					klog.Warning(err)
				} else {
					klog.Error(err)
				}
				return
			}
			defer resp.Body.Close()

			gotAtLeastOne = true
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				klog.Errorf("Error while reading metrics response: %s", err)
				return
			}

			m, err := api.ReadMetrics(body)
			if err != nil {
				klog.Errorf("Error reading metrics from node_exporter output: %s", err);
				return
			}

			klog.Infof("Processed metrics from VM: %+v", m)
			metrics <- m
		}()

		time.Sleep(time.Second * time.Duration(config.Metrics.SecondsBetweenRequests))
	}
}

func (r *Runner) sendRequestToPlugin(config *Config, req *api.AgentRequest) (*api.PluginResponse, error) {
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := fmt.Errorf("Unexpected redirect while sending message to plugin")
			klog.Warning(err)
			return err
		},
		Timeout: time.Second * time.Duration(config.Scheduler.RequestTimeoutSeconds),
	}

	requestBody, err := json.Marshal(req)
	if err != nil {
		klog.Fatalf("Error encoding scheduer request into JSON: %s", err)
	}

	klog.Infof("Sending AgentRequest: %+v", req)

	url := fmt.Sprintf("http://%s:%d/", r.schedulerIP.String(), config.Scheduler.RequestPort)
	resp, err := client.Post(url, "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("Error sending scheduler request: %s", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error reading body for response: %s", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Received response status %d body %q", resp.StatusCode, string(body))
	}

	var respMsg api.PluginResponse
	if err := json.Unmarshal(body, &respMsg); err != nil {
		return nil, fmt.Errorf("Bad JSON response: %s", err)
	}

	klog.Infof("Received PluginResponse: %+v", respMsg)
	return &respMsg, nil
}

func (r *Runner) setCpus(ctx context.Context, newCpuCount uint16) error {
	// Allow a maximum of 1 second to perform the update; otherwise something unexpected happened
	//
	// Because this relies on the OS to kill the process, it's *maybe* possible for weird edge cases
	// to stall it for longer. I'm not sure. Realistically, it should be fine.
	cmdContext, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(
		cmdContext,
		// ch-remote --api-socket <PATH> resize --cpus <CPUs>
		"ch-remote", "--api-socket", r.cloudHypervisorSockPath, "resize", "--cpus", strconv.Itoa(int(newCpuCount)),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		if cmdContext.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("Running ch-remote timed out")
		} else {
			err = fmt.Errorf("Error while running ch-remote: %s", err)
		}

		klog.Error(err)
		klog.Errorf("ch-remote output:\n%s", string(out))
	}

	return err
}
