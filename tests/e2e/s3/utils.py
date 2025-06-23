#!/usr/bin/env python

import subprocess
import json
import os
import sys
import boto3
import time
import datetime

import openapi_schema_validator
import yaml

def do_assert(condition, message="assert failed"):
    if not condition:
        raise Exception(message)


def update_agent_biling_config(billing_config: dict):
    """
    Updates the billing configuration for the autoscaling agent.

    Args:
        billing_config (dict): The new billing configuration to be applied.

    Raises:
        CalledProcessError: If the subprocess command returns a non-zero exit code.

    This function updates the billing configuration for the autoscaling agent by retrieving the current
    configuration from a Kubernetes ConfigMap named 'autoscaler-agent-config' in the 'kube-system' namespace.
    The new billing configuration is then applied by executing a subprocess command using the 'kubectl' tool
    to get the ConfigMap in JSON format and store it in the 'config' variable.
    """
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

    # Extract existing config JSON
    agent_config_json = config["data"]["config.json"]
    agent_config = json.loads(agent_config_json)
    # Set the billing config and write back into the ConfigMap
    agent_config["billing"] = billing_config
    agent_config_json = json.dumps(agent_config)
    config["data"]["config.json"] = agent_config_json

    output = subprocess.check_output(
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
    print(output)


def agent_add_s3():
    namespace = os.environ.get("NAMESPACE")
    update_agent_biling_config(
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
    update_agent_biling_config(
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


def s3_check_file(local_endpoint: str):
    """
    Waits for a file to be available in an S3 bucket and performs checks on the file, 
    such as the format of the events and the time intervals.

    Args:
        local_endpoint (str): The endpoint URL of the local S3 service.

    Raises:
        Exception: If the file is not found in the S3 bucket.

    Returns:
        None
    """
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

    do_assert(len(response["Contents"]) > 0)
    key = response["Contents"][0]["Key"]

    # Example key:
    # autoscaler-agent/year=2024/month=04/day=12/07:19:27Z_DGFMM7kfyLFdJASCnJfEYT.ndjson.gz
    do_assert(key.startswith("autoscaler-agent"))
    do_assert(key.endswith(".ndjson.gz"))

    response = s3.get_object(Bucket="metrics", Key=key)
    result = subprocess.check_output(
        ["gzip", "-d", "-c"], input=response["Body"].read()
    )

    events = []
    for line in result.splitlines():
        if line.strip():
            item = json.loads(line)
            events.append(item)

    do_assert(len(events) > 0, "No events found")

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
    do_assert(set(events[0].keys()) == set(
        [
            "idempotency_key",
            "metric",
            "type",
            "endpoint_id",
            "start_time",
            "stop_time",
            "value",
        ]
    ), "actual keys: %s" % events[0].keys())

    # Check time intervals
    start_time = datetime.datetime.strptime(
        events[0]["start_time"].split(".")[0], "%Y-%m-%dT%H:%M:%S"
    )
    stop_time = datetime.datetime.strptime(
        events[0]["stop_time"].split(".")[0], "%Y-%m-%dT%H:%M:%S"
    )

    # We have a 5 second interval, 10 seconds is a reasonable upper bound
    do_assert(stop_time - start_time < datetime.timedelta(seconds=10)) 
    
    now = datetime.datetime.utcnow()
    do_assert(now - stop_time < datetime.timedelta(seconds=10))


class KubctlForward:
    def __init__(self):
        self.namespace = os.environ.get("NAMESPACE")
        self.port = 56632  # arbitrary port

    def start(self):
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

        # Check if the process is still running after 1 second.  
        # If the port is already in use, it would have exited.
        time.sleep(1)
        if self.process.poll() is not None:
            raise Exception("Failed to start port-forward")


    def stop(self):
        print("Killing forward")
        self.process.terminate()
        ret = self.process.wait()
        # -SIGTERM
        do_assert(ret == -15, "Wrong exit code: %s" % ret)

    def endpoint(self):
        return "http://localhost:%d" % self.port


def step2_init():
    forward = KubctlForward()
    
    try:
        forward.start()

        s3_create_bucket(forward.endpoint())
        agent_add_s3()
        agent_restart()
    finally:
        forward.stop()


def step2_assert():
    forward = KubctlForward()
    try:
        forward.start()
        s3_check_file(forward.endpoint())
    finally:
        forward.stop()

def main():
    forward = KubctlForward()

    action = sys.argv[1]
    if action == "step2_init":
        step2_init()
    elif action == "step2_assert":
        step2_assert()
    elif action == "restore":
        agent_remove_s3()
        agent_restart()
    else:
        raise Exception("Unknown action: %s" % action)

if __name__ == "__main__":
    main()
