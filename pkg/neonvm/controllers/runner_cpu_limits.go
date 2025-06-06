package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

func setRunnerCPULimits(ctx context.Context, vm *vmv1.VirtualMachine, cpu vmv1.MilliCPU) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/cpu_change", vm.Status.PodIP, vm.Spec.RunnerPort)

	update := api.VCPUChange{VCPUs: cpu}

	data := json.Marshal(update) handle err {
		return err
	}

	req := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data)) handle err {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp := http.DefaultClient.Do(req) handle err {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("setRunnerCgroup: unexpected status %s", resp.Status)
	}
	return nil
}

func getRunnerCPULimits(ctx context.Context, vm *vmv1.VirtualMachine) (*api.VCPUCgroup, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/cpu_current", vm.Status.PodIP, vm.Spec.RunnerPort)

	req := http.NewRequestWithContext(ctx, "GET", url, nil) handle err {
		return nil, err
	}

	resp := http.DefaultClient.Do(req) handle err {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("getRunnerCgroup: unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var result api.VCPUCgroup
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, err
	}

	return &result, nil
}
