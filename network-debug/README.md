# Kubernetes Network Debugging Toolkit

This toolkit provides a comprehensive solution for debugging network issues in Kubernetes clusters, specifically designed for investigating packet losses in overlay networks.

## Features

- **Automated Interface Detection**: Automatically detects bridge and overlay network interfaces on each node
- **Distributed Packet Capture**: Runs tcpdump on all nodes in the cluster as a DaemonSet
- **Web-based Analysis UI**: Provides a simple web interface to browse, download, and analyze capture files
- **Node Selection**: Easily switch between nodes to view capture files from different nodes
- **Capture Rotation**: Automatically rotates capture files to prevent disk space issues

## Prerequisites

- Kubernetes cluster with kubectl access
- Sufficient permissions to create resources in the kube-system namespace

## Deployment

The toolkit includes a deployment script that makes it easy to deploy and manage:

```bash
# Deploy the toolkit
./deploy.sh deploy

# Check the status
./deploy.sh status

# Access the analyzer UI (if port 8080 is already in use, use a different port)
kubectl -n kube-system port-forward svc/tcpdump-analyzer 8081:80
```

Then access the analyzer UI at http://localhost:8081

## Architecture

The toolkit consists of two main components:

1. **tcpdump DaemonSet**: Runs on all nodes and captures packets on bridge and overlay interfaces
   - Automatically detects relevant interfaces (cilium_host, cilium_net, cilium_vxlan, neon-br0, neon-vxlan0)
   - Stores capture files in a hostPath volume (/var/log/tcpdump-captures)
   - Rotates capture files to prevent disk space issues

2. **Analyzer DaemonSet**: Provides a web UI for browsing and analyzing capture files
   - Runs on all nodes in the cluster
   - Each instance accesses capture files from its node's hostPath volume
   - Provides a web UI to browse, download, and analyze capture files
   - Includes a node selector to easily switch between nodes

## Using the Analyzer UI

The analyzer UI provides a simple interface to browse, download, and analyze capture files:

1. **Node Access**: Each analyzer pod shows capture files from its own node
2. **File Browsing**: View all capture files from the current node
3. **File Analysis**: Click the "Analyze" button to analyze a capture file
4. **File Download**: Click the "Download" button to download a capture file for offline analysis

To view capture files from a different node, you need to port-forward to the analyzer pod on that node:

```bash
# List all analyzer pods with their nodes
kubectl -n kube-system get pods -l app=tcpdump-analyzer -o wide

# Port-forward to a specific analyzer pod
kubectl -n kube-system port-forward pod/tcpdump-analyzer-[pod-id] 8080:8080
```

Alternatively, you can use the `port-forward` command in the deploy script, which will prompt you to select a pod:

```bash
./deploy.sh port-forward
```

## Debugging Packet Loss in Overlay Networks

This toolkit is particularly useful for debugging packet loss in overlay networks. Here's how to use it effectively:

1. **Deploy the toolkit** to start capturing packets on all nodes
2. **Generate traffic** that reproduces the packet loss issue
3. **Access the analyzer UI** to examine the capture files
4. **Compare captures** from different nodes to identify where packets are being dropped
5. **Look for patterns** in the packet loss, such as specific protocols, packet sizes, or source/destination pairs

Common causes of packet loss in overlay networks:

1. **MTU misconfigurations**: Check if packets larger than the MTU are being dropped
2. **Network congestion**: Look for retransmissions and out-of-order packets
3. **Firewall rules**: Check if specific traffic is being blocked
4. **CNI plugin issues**: Compare behavior with different CNI configurations
5. **Physical network problems**: Look for patterns that might indicate hardware issues

## Cleanup

To remove all deployed resources:

```bash
./deploy.sh cleanup
```

## Customization

You can customize the toolkit by modifying the following files:

- **tcpdump-daemonset.yaml**: Modify the DaemonSet configuration
- **capture-analyzer.yaml**: Adjust the analyzer deployment and service

## Troubleshooting

If you encounter issues:

1. Check the logs of the tcpdump pods:
   ```bash
   kubectl -n kube-system logs -l app=tcpdump-bridge
   ```

2. Check the logs of the analyzer pods:
   ```bash
   kubectl -n kube-system logs -l app=tcpdump-analyzer
   ```

3. Ensure the analyzer pods can access the capture files:
   ```bash
   kubectl exec -n kube-system $(kubectl -n kube-system get pods -l app=tcpdump-analyzer -o name | head -n 1) -- find /captures -type f | grep pcap