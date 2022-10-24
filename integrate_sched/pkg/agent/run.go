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
	"strings"
	"sync/atomic"
	"time"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
)

type Runner struct {
	podName                 api.PodName
	schedulerIP             net.IP
	cloudHypervisorSockPath string
	metricsURL              *url.URL
	minVCPU                 uint16
	maxVCPU                 uint16
	currentVCPU             uint16
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
		cloudHypervisorSockPath: cloudHypervisorSockPath,
		metricsURL:              args.MetricsURL,
		minVCPU:                 args.MinVCPU,
		maxVCPU:                 args.MaxVCPU,
		currentVCPU:             args.InitVCPU,
		readinessPort:           args.ReadinessPort,
		politeExitPort:          args.PoliteExitPort,
	}, nil
}

// Metrics is the type summarizing the
type Metrics struct {
	// LoadAverage is the current value of the VM's load average, as reported by node_exporter
	LoadAverage float32
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

	metrics := make(chan Metrics)
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
			newCpuCount := r.newGoalCPUCount(m)
			if newCpuCount == r.currentVCPU {
				continue
			}

			klog.Infof("New goal vCPU = %d", newCpuCount)

			// If the goal is less than the current amount, decrease right away.
			if newCpuCount < r.currentVCPU {
				klog.Infof(
					"Goal vCPU %d < Current vCPU %d, decrease immediately",
					newCpuCount, r.currentVCPU,
				)
				if err := r.setCpus(ctx, newCpuCount); err != nil {
					return fmt.Errorf("Error while setting CPU count: %s", err)
				}
				r.currentVCPU = newCpuCount
			}

			// Request a new amount of CPU
			req := api.ResourceRequest{
				VCPUs: newCpuCount,
				Pod:   r.podName,
			}

			permit, err := r.doResourceRequest(config, &req)
			if err != nil {
				klog.Errorf("Error from resource request: %s", err)
				goto badScheduler // assume something's permanently wrong with the scheduler
			}

			if permit.VCPUs != r.currentVCPU {
				if permit.VCPUs < r.currentVCPU {
					// We shoudln't reach this, because req.VCPUs < r.currentVCPU is already handled
					// above, after which r.currentVCPU == req.VCPUs, so that would mean that the
					// permit caused a decrease beyond what we're expecting.
					klog.Errorf("Scheduler gave bad permit less than current vCPU. Discontinuing further contact after updating vCPUs")
				}

				maybeLessThanDesired := ""
				if permit.VCPUs < req.VCPUs {
					maybeLessThanDesired = " (less than desired)"
				}
				klog.Infof("Setting vCPU = %d%s", permit.VCPUs, maybeLessThanDesired)

				if err := r.setCpus(ctx, permit.VCPUs); err != nil {
					return fmt.Errorf("Error while setting vCPU count: %s", err)
				}

				r.currentVCPU = permit.VCPUs

			} else /* permit.VCPUs == r.currentVCPU */ {
				if r.currentVCPU == req.VCPUs {
					// We actually already set the CPU for this (see above), the returned permit
					// just confirmed that
					klog.Infof("Scheduler confirmed decrease to %d vCPU", req.VCPUs)
				} else {
					// We wanted to increase vCPUs, but the scheduler didn't allow it.
					klog.Infof("Scheduler denied increase to %d vCPU, staying at %d", req.VCPUs, r.currentVCPU)
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
func (r *Runner) newGoalCPUCount(metrics Metrics) uint16 {
	goal := r.currentVCPU
	if metrics.LoadAverage > 0.9*float32(r.currentVCPU) {
		goal *= 2
	} else if metrics.LoadAverage < 0.4*float32(r.currentVCPU) {
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

func (r *Runner) getMetricsLoop(config *Config, metrics chan<- Metrics) {
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

			// TODO: This could be more efficient, performing the split + search while reading the
			// body. But that's annoying to do in Go, so I've left it as an exercise for the reader.
			lines := strings.Split(string(body), "\n")
			var loadLine string
			prefix := "node_load1"
			for _, line := range lines {
				if strings.HasPrefix(line, prefix) {
					loadLine = line
					break
				}
			}
			if loadLine == "" {
				klog.Errorf("No line in metrics output starting with %q", prefix)
				return
			}

			fields := strings.Fields(loadLine)
			if len(fields) < 2 {
				klog.Errorf(
					"Expected >= 2 fields in metrics output for %q. Got %v",
					prefix, len(fields),
				)
				return
			}

			loadAverage, err := strconv.ParseFloat(fields[1], 32)
			if err != nil {
				klog.Errorf("Error parsing %q as LoadAverage float: %s", fields[1], err)
				return
			}

			m := Metrics{LoadAverage: float32(loadAverage)}
			klog.Infof("Processed metrics from VM: %+v", m)
			metrics <- m
		}()

		time.Sleep(time.Second * time.Duration(config.Metrics.SecondsBetweenRequests))
	}
}

func (r *Runner) doResourceRequest(config *Config, req *api.ResourceRequest) (*api.ResourcePermit, error) {
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := fmt.Errorf("Unexpected redirect while sending scheduler request")
			klog.Warning(err)
			return err
		},
		Timeout: time.Second * time.Duration(config.Scheduler.RequestTimeoutSeconds),
	}

	agentMsg := req.Request()
	requestBody, err := json.Marshal(&agentMsg)
	if err != nil {
		klog.Fatalf("Error encoding scheduler request into JSON", err)
	}

	klog.Infof("Sending AgentMessage: %+v", agentMsg)

	url := fmt.Sprintf("http://%s:%d/", r.schedulerIP.String(), config.Scheduler.RequestPort)
	resp, err := client.Post(url, "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("Error sending scheduler request: %s", err)
	}

	defer resp.Body.Close()
	var pluginMsg api.PluginMessage
	jsonDecoder := json.NewDecoder(resp.Body)
	if err := jsonDecoder.Decode(&pluginMsg); err != nil {
		return nil, fmt.Errorf("Bad JSON response: %s", err)
	}

	klog.Infof("Received PluginMessage: %+v", pluginMsg)
	if pluginMsg.ID != agentMsg.ID {
		return nil, fmt.Errorf("Response ID from plugin doesn't match request ID: %v != %v", pluginMsg.ID, agentMsg.ID)
	}

	permit, err := pluginMsg.AsResourcePermit()
	if err != nil {
		return nil, fmt.Errorf("Error casting response into ResourcePermit: %s", err)
	} else if permit.VCPUs > req.VCPUs {
		return nil, fmt.Errorf(
			"ResourcePermit doesn't match request: permit vCPUs %d > request vCPUs %d",
			permit.VCPUs, req.VCPUs,
		)
	}

	return permit, nil
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
