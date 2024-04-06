#!/usr/bin/env python

import subprocess
import json
import os
import sys
import boto3
import time


def update_agent_biling_clients(billing):
    config = subprocess.check_output(
        [
            "kubectl",
            "get",
            "configmap",
            "autoscaler-agent-config",
            "-n",
            "kube-system",
            "-o",
            "json",
        ]
    )
    config = json.loads(config)
    print(config)
    agent_config_json = config["data"]["config.json"]
    agent_config = json.loads(agent_config_json)
    agent_config["billing"] = billing
    agent_config_json = json.dumps(agent_config)
    config["data"]["config.json"] = agent_config_json

    result = subprocess.check_call(
        [
            "kubectl",
            "patch",
            "configmap",
            "autoscaler-agent-config",
            "-n",
            "kube-system",
            "--type",
            "merge",
            "-p",
            json.dumps(config),
        ]
    )
    assert result == 0


def agent_add_s3():
    namespace = os.environ.get("NAMESPACE")
    update_agent_biling_clients(
        {
            "cpuMetricName": "effective_compute_seconds",
            "activeTimeMetricName": "active_time_seconds",
            "collectEverySeconds": 5,
            "accumulateEverySeconds": 5,
            "clients": {
                "s3": {
                    "bucket": "metrics",
                    "region": "us-east-1",
                    "prefixInBucket": "autoscaler-agent",
                    "endpoint": "http://minio-service.%s:9000" % namespace,
                    "pushEverySeconds": 10,
                    "pushRequestTimeoutSeconds": 20,
                    "maxBatchSize": 200,
                }
            },
        }
    )


def agent_remove_s3():
    update_agent_biling_clients(
        {
            "cpuMetricName": "effective_compute_seconds",
            "activeTimeMetricName": "active_time_seconds",
            "collectEverySeconds": 5,
            "accumulateEverySeconds": 5,
            "clients": {},
        }
    )


def agent_restart():
    os.system("kubectl rollout restart daemonset autoscaler-agent -n kube-system")


def s3_create_bucket(local_endpoint):
    s3 = boto3.client("s3", endpoint_url=local_endpoint)
    s3.create_bucket(Bucket="metrics")


def s3_check_file(local_endpoint):
    s3 = boto3.client("s3", endpoint_url=local_endpoint)
    found = False
    for i in range(100):
        response = s3.list_objects_v2(Bucket="metrics")
        if "Contents" in response:
            found = True
            break
        time.sleep(1)
        print("Waiting for the file in s3")

    if not found:
        raise Exception("File not found in s3")

    assert len(response["Contents"]) > 0
    key = response["Contents"][0]["Key"]

    # Example key:
    # autoscaler-agent/year=2024/month=04/day=12/07:19:27Z_DGFMM7kfyLFdJASCnJfEYT.ndjson.gz
    assert key.startswith("autoscaler-agent")
    assert key.endswith(".ndjson.gz")

    response = s3.get_object(Bucket="metrics", Key=key)
    result = subprocess.check_output(
        ["gzip", "-d", "-c"], input=response["Body"].read()
    )
    result = json.loads(result)
    print(result)

    assert "events" in result

    events = result["events"]
    assert len(events) > 0

    # Example events[0]:
    #
    # {
    #   "idempotency_key": "2024-04-12T12:10:00.845237Z-autoscaler-agent-nwcnd-1/4",
    #   "metric": "effective_compute_seconds",
    #   "type": "incremental",
    #   "endpoint_id": "test",
    #   "start_time": "2024-04-12T12:09:55.844007537Z",
    #   "stop_time": "2024-04-12T12:10:00.845237402Z",
    #   "value": 1
    # }
    assert set(events[0].keys()) == set(
        [
            "idempotency_key",
            "metric",
            "type",
            "endpoint_id",
            "start_time",
            "stop_time",
            "value",
        ]
    ), events[0].keys()


class KubctlForward:
    def __init__(self):
        self.namespace = os.environ.get("NAMESPACE")
        self.port = 12345  # arbitrary starting port

    def start(self):
        while True:
            self.process = subprocess.Popen(
                [
                    "kubectl",
                    "port-forward",
                    "service/minio-service",
                    "%s:9000" % self.port,
                    "-n",
                    self.namespace,
                ]
            )

            time.sleep(1)

            if self.process.poll() is None:
                break

            self.port += 1

    def stop(self):
        print("Killing forward")
        self.process.terminate()
        ret = self.process.wait()
        # -SIGTERM
        assert ret == -15, ret

    def endpoint(self):
        return "http://localhost:%d" % self.port


def step2_init():
    forward = KubctlForward()
    forward.start()

    try:
        s3_create_bucket(forward.endpoint())
        agent_add_s3()
        agent_restart()
    finally:
        forward.stop()


def step2_assert():
    forward = KubctlForward()
    forward.start()

    try:
        s3_check_file(forward.endpoint())
    finally:
        forward.stop()


if __name__ == "__main__":
    forward = KubctlForward()

    action = sys.argv[1]
    if action == "step2_init":
        step2_init()
    elif action == "step2_assert":
        step2_assert()
    elif action == "restore":
        agent_remove_s3()
        agent_restart()
